package tool

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/quality"
)

func TestToolRegistryPipeline(t *testing.T) {
	t.Parallel()

	var steps []string
	audit := adapter.NewMemoryAuditLog()
	registry := NewRegistry(
		NewExecutionPolicy(ExecutionPolicyConfig{AutoAllowTools: []string{"echo"}}),
		PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error {
			steps = append(steps, "permission")
			return nil
		}),
		GuardrailsFunc{
			BeforeFunc: func(context.Context, domain.ToolCall) error {
				steps = append(steps, "before")
				return nil
			},
			AfterFunc: func(context.Context, domain.ToolCall, domain.ToolResult) error {
				steps = append(steps, "after")
				return nil
			},
		},
	).WithAuditLog(audit).WithOutputSanitizer(quality.NewOutputSanitizer())
	registry.RegisterDescriptor(Descriptor{
		Name: "echo",
		InputSchema: map[string]any{
			"required": []string{"message"},
			"properties": map[string]any{
				"message": map[string]any{"type": "string"},
			},
		},
		RiskLevel: "low",
		Timeout:   time.Second,
	}, HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		steps = append(steps, "execute")
		return domain.ToolResult{CallID: "call-1", Success: true, Output: "ok\n<script>"}, nil
	}))

	result, err := registry.Execute(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.ToolCall{
		ID:        "call-1",
		Name:      "echo",
		Arguments: map[string]string{"message": "hello"},
	})
	if err != nil {
		t.Fatalf("Registry.Execute(echo) error = %v, want nil", err)
	}
	wantSteps := []string{"permission", "before", "execute", "after"}
	if !reflect.DeepEqual(steps, wantSteps) {
		t.Errorf("Registry.Execute(echo) steps = %v, want %v", steps, wantSteps)
	}
	if result.Output == "ok\n<script>" {
		t.Errorf("Registry.Execute(echo) output = %q, want sanitized output", result.Output)
	}
	auditEvents := mustAuditEvents(t, audit)
	if !hasAuditAction(auditEvents, "tool_executed") {
		t.Errorf("Registry.Execute(echo) audit events = %#v, want tool_executed", auditEvents)
	}
}

func TestExecutionPolicyAutoAllow(t *testing.T) {
	t.Parallel()

	policy := NewExecutionPolicy(ExecutionPolicyConfig{AutoAllowTools: []string{"read_file"}})
	got := policy.Decide(domain.Agent{Role: "developer"}, domain.ToolCall{Name: "read_file", RiskLevel: "high"})
	if got != DecisionAllow {
		t.Errorf("ExecutionPolicy.Decide(read_file) = %s, want %s", got, DecisionAllow)
	}

	got = policy.Decide(domain.Agent{Role: "developer"}, domain.ToolCall{Name: "write_file", RiskLevel: "high"})
	if got != DecisionDeny {
		t.Errorf("ExecutionPolicy.Decide(write_file high risk) = %s, want %s", got, DecisionDeny)
	}
}

func TestPermissionEnforcerBatchRoleOverride(t *testing.T) {
	t.Parallel()

	enforcer := NewBatchRolePermissionEnforcer(
		map[string]bool{
			"developer:read_file": true,
			"developer:delete":    false,
		},
		[]RolePermissionOverride{
			{Role: "maintainer", ToolName: "delete", Allow: true},
		},
	)

	errs := enforcer.CheckBatch(domain.Agent{Role: "maintainer"}, []domain.ToolCall{
		{Name: "read_file"},
		{Name: "delete"},
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("BatchRolePermissionEnforcer.CheckBatch()[%d] error = %v, want nil", i, err)
		}
	}

	err := enforcer.Check(domain.Agent{Role: "developer"}, domain.ToolCall{Name: "delete"})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("BatchRolePermissionEnforcer.Check(developer delete) error = %v, want ErrPermissionDenied", err)
	}
}

func TestToolGuardrailsTimeout(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(
		NewExecutionPolicy(ExecutionPolicyConfig{AutoAllowTools: []string{"slow"}}),
		PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		NoopGuardrails{},
	)
	registry.RegisterDescriptor(Descriptor{
		Name:    "slow",
		Timeout: 5 * time.Millisecond,
	}, HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		<-ctx.Done()
		return domain.ToolResult{CallID: call.ID}, ctx.Err()
	}))

	_, err := registry.Execute(context.Background(), domain.Agent{Role: "developer"}, domain.ToolCall{
		ID:   "call-1",
		Name: "slow",
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Registry.Execute(slow) error = %v, want DeadlineExceeded", err)
	}
}
