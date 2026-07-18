package manualgate

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/tool"
)

func gateRegistry() *tool.Registry {
	reg := tool.NewRegistry(
		tool.NewStaticPolicy(tool.DecisionAllow),
		tool.NewRolePermissionEnforcer(map[string]bool{}),
		tool.NoopGuardrails{},
	)
	reg.RegisterDescriptor(tool.Descriptor{Name: "read_file", Sensitive: false}, tool.HandlerFunc(noHandler))
	reg.RegisterDescriptor(tool.Descriptor{Name: "write_file", Sensitive: true}, tool.HandlerFunc(noHandler))
	return reg
}
func noHandler(context.Context, domain.ToolCall) (domain.ToolResult, error) {
	return domain.ToolResult{Success: true}, nil
}

func manualTask() domain.Task {
	return domain.Task{ID: "t1", SessionID: "s1", AgentID: "a1", Mode: domain.ModeManual}
}

func TestShouldSuspendOnSensitiveDirectCall(t *testing.T) {
	store := approval.NewToolGateStore(t.TempDir())
	g := New(store)
	reg := gateRegistry()
	calls := []domain.ToolCall{{ID: "c1", Name: "write_file", Arguments: map[string]string{"path": "x"}}}
	suspend, err := g.ShouldSuspend(context.Background(), manualTask(), calls, reg)
	if err != nil {
		t.Fatal(err)
	}
	if !suspend {
		t.Fatal("want suspend for sensitive write_file in manual mode")
	}
	// A pending ticket must exist for the call.
	tid := approval.TicketID("t1", "c1")
	if a, ok, _ := store.Get("s1", tid); !ok || a.Status != approval.ApprovalPending {
		t.Fatalf("expected pending ticket, ok=%v a=%+v", ok, a)
	}
}

func TestShouldSuspendSkipsReadOnly(t *testing.T) {
	g := New(approval.NewToolGateStore(t.TempDir()))
	calls := []domain.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": "x"}}}
	suspend, err := g.ShouldSuspend(context.Background(), manualTask(), calls, gateRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if suspend {
		t.Fatal("read-only call must not suspend")
	}
}

func TestShouldSuspendLazyCallToolPeeksRealTool(t *testing.T) {
	g := New(approval.NewToolGateStore(t.TempDir()))
	// lazy meta call wrapping the sensitive write_file
	calls := []domain.ToolCall{{ID: "c1", Name: "call_tool", Arguments: map[string]string{"tool_name": "write_file", "arguments_json": "{}"}}}
	suspend, err := g.ShouldSuspend(context.Background(), manualTask(), calls, gateRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if !suspend {
		t.Fatal("lazy call_tool wrapping sensitive tool must suspend")
	}
}

func TestShouldSuspendAutoModeNeverSuspends(t *testing.T) {
	g := New(approval.NewToolGateStore(t.TempDir()))
	task := domain.Task{ID: "t1", SessionID: "s1", Mode: domain.ModeAuto}
	calls := []domain.ToolCall{{ID: "c1", Name: "write_file"}}
	suspend, err := g.ShouldSuspend(context.Background(), task, calls, gateRegistry())
	if err != nil || suspend {
		t.Fatalf("auto mode: suspend=%v err=%v, want false,nil", suspend, err)
	}
}

func TestResolveDeniedReturnsDisallow(t *testing.T) {
	store := approval.NewToolGateStore(t.TempDir())
	g := New(store)
	reg := gateRegistry()
	call := domain.ToolCall{ID: "c1", Name: "write_file"}
	// open + deny
	if _, err := g.ShouldSuspend(context.Background(), manualTask(), []domain.ToolCall{call}, reg); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalDenied); err != nil {
		t.Fatal(err)
	}
	allow, err := g.Resolve(context.Background(), manualTask(), call, reg)
	if err != nil {
		t.Fatal(err)
	}
	if allow {
		t.Fatal("denied call must resolve to disallow")
	}
}

func TestResolveApprovedAllows(t *testing.T) {
	store := approval.NewToolGateStore(t.TempDir())
	g := New(store)
	reg := gateRegistry()
	call := domain.ToolCall{ID: "c1", Name: "write_file"}
	_, _ = g.ShouldSuspend(context.Background(), manualTask(), []domain.ToolCall{call}, reg)
	_, _ = store.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalApproved)
	allow, err := g.Resolve(context.Background(), manualTask(), call, reg)
	if err != nil || !allow {
		t.Fatalf("approved: allow=%v err=%v, want true,nil", allow, err)
	}
}

func TestResolveReadOnlyAlwaysAllows(t *testing.T) {
	g := New(approval.NewToolGateStore(t.TempDir()))
	call := domain.ToolCall{ID: "c1", Name: "read_file"}
	allow, err := g.Resolve(context.Background(), manualTask(), call, gateRegistry())
	if err != nil || !allow {
		t.Fatalf("read-only: allow=%v err=%v, want true,nil", allow, err)
	}
}
