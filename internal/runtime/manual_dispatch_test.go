package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/manualgate"
	"github.com/stardust/legion-agent/internal/tool"
)

func TestDispatchDeniedSensitiveReturnsRejectResult(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	gate := manualgate.New(store)
	var writeCalled bool
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{})
	reg.RegisterDescriptor(tool.Descriptor{Name: "write_file", Sensitive: true}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		writeCalled = true
		return domain.ToolResult{Success: true}, nil
	}))
	r := NewRuntime(Config{Maas: &oneToolThenTextMaas{}, Tools: reg, Checkpoints: nil, ToolGate: gate})
	task := domain.Task{ID: "t1", SessionID: "s1", Mode: domain.ModeManual}
	call := domain.ToolCall{ID: "c1", Name: "write_file", Arguments: map[string]string{}}
	// open + deny the ticket first
	if _, err := gate.ShouldSuspend(context.Background(), task, []domain.ToolCall{call}, reg); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalDenied); err != nil {
		t.Fatal(err)
	}
	res, err := r.dispatchToolCall(context.Background(), domain.Agent{ID: "a1"}, task, call, reg)
	if err != nil {
		t.Fatalf("dispatchToolCall err = %v, want nil (reject is a result, not a Go error)", err)
	}
	if res.Success || !strings.Contains(res.Error, "denied") {
		t.Fatalf("want unsuccessful denied result, got %+v", res)
	}
	if writeCalled {
		t.Fatal("denied sensitive tool must not execute")
	}
}

// TestDispatchDeniedLazyCallToolReturnsRejectResult mirrors
// TestDispatchDeniedSensitiveReturnsRejectResult but drives the outer lazy
// call_tool meta call through dispatch: the gate must peek the wrapped real
// tool for sensitivity while keying the approval ticket on the outer call_tool
// call ID, and a denied decision must reject before the inner write_file
// handler ever runs.
func TestDispatchDeniedLazyCallToolReturnsRejectResult(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	gate := manualgate.New(store)
	var writeCalled bool
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{})
	reg.RegisterDescriptor(tool.Descriptor{Name: "write_file", Sensitive: true}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		writeCalled = true
		return domain.ToolResult{Success: true}, nil
	}))
	r := NewRuntime(Config{Maas: &oneToolThenTextMaas{}, Tools: reg, Checkpoints: nil, ToolGate: gate, LazyTools: true})
	task := domain.Task{ID: "t1", SessionID: "s1", Mode: domain.ModeManual}
	call := domain.ToolCall{ID: "c1", Name: "call_tool", Arguments: map[string]string{"tool_name": "write_file", "arguments_json": "{}"}}
	// open + deny the ticket, keyed on the outer call_tool call ID
	if _, err := gate.ShouldSuspend(context.Background(), task, []domain.ToolCall{call}, reg); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalDenied); err != nil {
		t.Fatal(err)
	}
	res, err := r.dispatchToolCall(context.Background(), domain.Agent{ID: "a1"}, task, call, reg)
	if err != nil {
		t.Fatalf("dispatchToolCall err = %v, want nil (reject is a result, not a Go error)", err)
	}
	if res.Success || !strings.Contains(res.Error, "denied") {
		t.Fatalf("want unsuccessful denied result, got %+v", res)
	}
	if writeCalled {
		t.Fatal("denied lazy call_tool call must not execute the wrapped real tool")
	}
}
