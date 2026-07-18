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

// spyApprovalSink is a test double for ApprovalEventSink that records every
// notification it receives, for both ShouldSuspend's approval_pending
// emission (this file) and Decide's approval_resolved emission
// (decider_test.go).
type spyApprovalSink struct {
	pending  []string // ticketIDs
	resolved []string // ticketID:decision
}

func (s *spyApprovalSink) ApprovalPending(_ context.Context, _, ticketID, _ string, _ map[string]string) {
	s.pending = append(s.pending, ticketID)
}

func (s *spyApprovalSink) ApprovalResolved(_ context.Context, _, ticketID, decision string) {
	s.resolved = append(s.resolved, ticketID+":"+decision)
}

func TestShouldSuspendEmitsApprovalPending(t *testing.T) {
	store := approval.NewToolGateStore(t.TempDir())
	sink := &spyApprovalSink{}
	gate := New(store, WithApprovalSink(sink))
	tools := gateRegistry() // existing helper: registers write_file with Sensitive:true
	task := domain.Task{ID: "task-1", SessionID: "s1", Mode: domain.ModeManual}
	call := domain.ToolCall{ID: "call-1", Name: "write_file", Arguments: map[string]string{"path": "/tmp/x"}}

	suspend, err := gate.ShouldSuspend(context.Background(), task, []domain.ToolCall{call}, tools)
	if err != nil {
		t.Fatalf("ShouldSuspend() error = %v, want nil", err)
	}
	if !suspend {
		t.Fatal("ShouldSuspend() = false, want true for Manual+sensitive")
	}
	wantTicket := approval.TicketID("task-1", "call-1")
	if len(sink.pending) != 1 || sink.pending[0] != wantTicket {
		t.Fatalf("sink.pending = %v, want [%s]", sink.pending, wantTicket)
	}
}

// TestShouldSuspendDoesNotReEmitForExistingPendingTicket covers T3 Minor#2:
// store.Open is idempotent and returns success for an already-open pending
// ticket, so guarding the sink notification on "Open succeeded" (rather than
// on "found" reporting no prior ticket) re-fires approval_pending for a call
// ID that was already pending — the GUI would show the approval prompt
// twice for one ticket. ApprovalEventSink's contract says ApprovalPending
// "fires once per newly opened pending ticket", so two ShouldSuspend passes
// over the same task/call ID must record the notification only once.
func TestShouldSuspendDoesNotReEmitForExistingPendingTicket(t *testing.T) {
	store := approval.NewToolGateStore(t.TempDir())
	sink := &spyApprovalSink{}
	gate := New(store, WithApprovalSink(sink))
	tools := gateRegistry()
	task := domain.Task{ID: "task-1", SessionID: "s1", Mode: domain.ModeManual}
	call := domain.ToolCall{ID: "call-1", Name: "write_file", Arguments: map[string]string{"path": "/tmp/x"}}

	if _, err := gate.ShouldSuspend(context.Background(), task, []domain.ToolCall{call}, tools); err != nil {
		t.Fatalf("ShouldSuspend() #1 error = %v, want nil", err)
	}
	if _, err := gate.ShouldSuspend(context.Background(), task, []domain.ToolCall{call}, tools); err != nil {
		t.Fatalf("ShouldSuspend() #2 error = %v, want nil", err)
	}

	wantTicket := approval.TicketID("task-1", "call-1")
	if len(sink.pending) != 1 || sink.pending[0] != wantTicket {
		t.Fatalf("sink.pending = %v, want exactly one [%s] (no re-emit for existing pending ticket)", sink.pending, wantTicket)
	}
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
	if a, ok, _ := store.Get("s1", tid, ""); !ok || a.Status != approval.ApprovalPending {
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
	if _, err := store.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalDenied, ""); err != nil {
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
	_, _ = store.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalApproved, "")
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

// TestResolveUndecidedSensitiveFailsLoud covers the invariant-3 default branch
// of Resolve: a sensitive call reaching dispatch with no recorded decision
// (no ticket at all, or a ticket still pending) must fail loud — never allow
// — because the round-level gate should already have suspended it.
func TestResolveUndecidedSensitiveFailsLoud(t *testing.T) {
	t.Run("no ticket on disk", func(t *testing.T) {
		g := New(approval.NewToolGateStore(t.TempDir()))
		call := domain.ToolCall{ID: "c1", Name: "write_file"}
		allow, err := g.Resolve(context.Background(), manualTask(), call, gateRegistry())
		if err == nil {
			t.Fatal("want error for sensitive call with no ticket, got nil")
		}
		if allow {
			t.Fatal("want allow=false for sensitive call with no ticket")
		}
	})

	t.Run("ticket pending", func(t *testing.T) {
		store := approval.NewToolGateStore(t.TempDir())
		g := New(store)
		reg := gateRegistry()
		call := domain.ToolCall{ID: "c1", Name: "write_file"}
		// Open a ticket via ShouldSuspend but never decide it.
		if _, err := g.ShouldSuspend(context.Background(), manualTask(), []domain.ToolCall{call}, reg); err != nil {
			t.Fatal(err)
		}
		allow, err := g.Resolve(context.Background(), manualTask(), call, reg)
		if err == nil {
			t.Fatal("want error for sensitive call with pending ticket, got nil")
		}
		if allow {
			t.Fatal("want allow=false for sensitive call with pending ticket")
		}
	})
}

// TestShouldSuspendMultiCallPartialDecision covers a round with two sensitive
// calls where only one has been decided: ShouldSuspend must still report
// suspend=true (the other call is still pending), the decided call's ticket
// must not be reset to pending by the second ShouldSuspend pass (idempotent
// Open), and Resolve must allow the decided call while failing loud on the
// undecided one.
func TestShouldSuspendMultiCallPartialDecision(t *testing.T) {
	store := approval.NewToolGateStore(t.TempDir())
	g := New(store)
	reg := gateRegistry()
	task := manualTask()
	calls := []domain.ToolCall{
		{ID: "c1", Name: "write_file"},
		{ID: "c2", Name: "write_file"},
	}

	suspend, err := g.ShouldSuspend(context.Background(), task, calls, reg)
	if err != nil {
		t.Fatal(err)
	}
	if !suspend {
		t.Fatal("want suspend=true when both calls are pending")
	}

	if _, err := store.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalApproved, ""); err != nil {
		t.Fatal(err)
	}

	suspend, err = g.ShouldSuspend(context.Background(), task, calls, reg)
	if err != nil {
		t.Fatal(err)
	}
	if !suspend {
		t.Fatal("want suspend=true when c2 is still pending")
	}

	// c1's ticket must remain approved — Open must be idempotent and must not
	// reset an already-decided ticket back to pending.
	c1, ok, err := store.Get("s1", approval.TicketID("t1", "c1"), "")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || c1.Status != approval.ApprovalApproved {
		t.Fatalf("c1 ticket = %+v, ok=%v, want status=approved", c1, ok)
	}

	allow, err := g.Resolve(context.Background(), task, calls[0], reg)
	if err != nil || !allow {
		t.Fatalf("Resolve(c1): allow=%v err=%v, want true,nil", allow, err)
	}

	allow, err = g.Resolve(context.Background(), task, calls[1], reg)
	if err == nil {
		t.Fatal("Resolve(c2): want error for undecided ticket, got nil")
	}
	if allow {
		t.Fatal("Resolve(c2): want allow=false for undecided ticket")
	}
}
