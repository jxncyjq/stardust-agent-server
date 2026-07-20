package tool

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

func TestRegistryExecuteBlocksDeniedPermission(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(NewStaticPolicy(DecisionAllow), NewRolePermissionEnforcer(map[string]bool{
		"reader:write_file": false,
	}), NoopGuardrails{})
	registry.Register("write_file", HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		t.Error("blocked tool handler was called")
		return domain.ToolResult{}, nil
	}))

	_, err := registry.Execute(context.Background(), domain.Agent{Role: "reader"}, domain.ToolCall{Name: "write_file"})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("Registry.Execute denied tool error = %v, want ErrPermissionDenied", err)
	}
}

func TestRegistryExecuteBlocksPathOutsideWorkspace(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(
		NewStaticPolicy(DecisionAllow),
		NewRolePermissionEnforcer(map[string]bool{"developer:read_file": true}),
		NewPathGuardrails(port.NewWorkspacePathGuard(filepath.Clean(`C:\workspace`)), "path"),
	)
	registry.Register("read_file", HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		t.Error("blocked path handler was called")
		return domain.ToolResult{}, nil
	}))

	_, err := registry.Execute(context.Background(), domain.Agent{Role: "developer"}, domain.ToolCall{
		Name:      "read_file",
		Arguments: map[string]string{"path": filepath.Clean(`C:\other\secret.txt`)},
	})
	if !errors.Is(err, port.ErrPathOutsideWorkspace) {
		t.Errorf("Registry.Execute outside path error = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestRegistryExecuteAuditsSuccessfulToolCall(t *testing.T) {
	t.Parallel()

	audit := adapter.NewMemoryAuditLog()
	registry := NewRegistry(
		NewStaticPolicy(DecisionAllow),
		NewRolePermissionEnforcer(map[string]bool{"developer:read_file": true}),
		NoopGuardrails{},
	).WithAuditLog(audit)
	registry.Register("read_file", HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: "call-1", Success: true, Output: "ok"}, nil
	}))

	_, err := registry.Execute(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.ToolCall{
		ID:   "call-1",
		Name: "read_file",
	})
	if err != nil {
		t.Fatalf("Registry.Execute(%q) error = %v, want nil", "read_file", err)
	}
	auditEvents := mustAuditEvents(t, audit)
	if !hasAuditAction(auditEvents, "tool_executed") {
		t.Errorf("audit events missing %q: %#v", "tool_executed", auditEvents)
	}
}

func TestRegistryExecuteAuditsFailedToolCall(t *testing.T) {
	t.Parallel()

	audit := adapter.NewMemoryAuditLog()
	registry := NewRegistry(
		NewStaticPolicy(DecisionAllow),
		NewRolePermissionEnforcer(map[string]bool{"developer:read_file": true}),
		NoopGuardrails{},
	).WithAuditLog(audit)
	registry.Register("read_file", HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, fmt.Errorf("disk unavailable")
	}))

	_, err := registry.Execute(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.ToolCall{
		ID:   "call-1",
		Name: "read_file",
	})
	if err == nil {
		t.Fatalf("Registry.Execute(%q) error = nil, want non-nil", "read_file")
	}
	auditEvents := mustAuditEvents(t, audit)
	if !hasAuditAction(auditEvents, "tool_failed") {
		t.Errorf("audit events missing %q: %#v", "tool_failed", auditEvents)
	}
}

// mustAuditEvents reads the audit log's events, failing the test immediately
// if the read itself errors (fail-loud: never silently substitute an empty
// slice for a read failure).
func mustAuditEvents(t *testing.T, log port.AuditLog) []domain.AuditEvent {
	t.Helper()
	events, err := log.Events()
	if err != nil {
		t.Fatalf("AuditLog.Events() error = %v", err)
	}
	return events
}

func hasAuditAction(events []domain.AuditEvent, action string) bool {
	for _, event := range events {
		if event.Action == action {
			return true
		}
	}
	return false
}
