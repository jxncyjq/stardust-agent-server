package runtime

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/manualgate"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/tool"
)

// TestManualGateDenyThenResume drives a full Manual-mode round trip where the
// human denies the pending sensitive tool call: suspend on round 1, then
// resume completes without ever executing write_file.
func TestManualGateDenyThenResume(t *testing.T) {
	dir := t.TempDir()
	cpStore := sessionstate.NewStore(dir)
	apStore := approval.NewToolGateStore(dir)
	gate := manualgate.New(apStore)
	var writeCalled bool
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }), tool.NoopGuardrails{})
	reg.RegisterDescriptor(tool.Descriptor{Name: "write_file", Sensitive: true}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		writeCalled = true
		return domain.ToolResult{Success: true, Output: "wrote"}, nil
	}))
	maas := &oneToolThenTextMaas{toolName: "write_file"}
	r := NewRuntime(Config{Maas: maas, Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
		Tools: reg, Checkpoints: cpStore, ToolGate: gate})
	task := domain.Task{ID: "t1", SessionID: "s1", AgentID: "a1", Status: domain.TaskRunning, Mode: domain.ModeManual, Input: "go"}
	if _, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task); err != ErrSuspended {
		t.Fatalf("first run err = %v, want ErrSuspended", err)
	}
	if _, err := apStore.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalDenied, ""); err != nil {
		t.Fatal(err)
	}
	run, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task)
	if err != nil {
		t.Fatalf("resume run err = %v", err)
	}
	if writeCalled {
		t.Fatal("denied write_file executed on resume")
	}
	if run.Result == "" {
		t.Fatal("resume produced no final answer")
	}
}

// TestManualGateApproveThenResume mirrors the deny scenario but the human
// approves the ticket: resume must execute write_file and complete.
func TestManualGateApproveThenResume(t *testing.T) {
	dir := t.TempDir()
	cpStore := sessionstate.NewStore(dir)
	apStore := approval.NewToolGateStore(dir)
	gate := manualgate.New(apStore)
	var writeCalled bool
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }), tool.NoopGuardrails{})
	reg.RegisterDescriptor(tool.Descriptor{Name: "write_file", Sensitive: true}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		writeCalled = true
		return domain.ToolResult{Success: true, Output: "wrote"}, nil
	}))
	maas := &oneToolThenTextMaas{toolName: "write_file"}
	r := NewRuntime(Config{Maas: maas, Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
		Tools: reg, Checkpoints: cpStore, ToolGate: gate})
	task := domain.Task{ID: "t1", SessionID: "s1", AgentID: "a1", Status: domain.TaskRunning, Mode: domain.ModeManual, Input: "go"}
	if _, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task); err != ErrSuspended {
		t.Fatalf("first run err = %v, want ErrSuspended", err)
	}
	if _, err := apStore.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalApproved, ""); err != nil {
		t.Fatal(err)
	}
	run, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task)
	if err != nil {
		t.Fatalf("resume run err = %v", err)
	}
	if !writeCalled {
		t.Fatal("approved write_file did not execute on resume")
	}
	if run.Result == "" {
		t.Fatal("resume produced no final answer")
	}
}
