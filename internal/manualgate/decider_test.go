package manualgate

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/task"
)

func TestDecideFlipsTaskToRunningWhenAllDecided(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	sched := task.NewScheduler()
	_ = sched.Add(context.Background(), domain.Task{ID: "t1", SessionID: "s1", Status: domain.TaskRunning})
	_ = sched.Transition(context.Background(), "t1", domain.TaskSuspended)
	// two pending tickets
	_, _ = store.Open(approval.ToolApproval{SessionKey: "s1", TaskID: "t1", ToolCallID: "c1", ToolName: "write_file"})
	_, _ = store.Open(approval.ToolApproval{SessionKey: "s1", TaskID: "t1", ToolCallID: "c2", ToolName: "send_message"})
	ac := NewApprovalCoordinator(store, sched)
	// decide first → still one pending → stays Suspended
	if _, err := ac.Decide(context.Background(), "t1", approval.TicketID("t1", "c1"), approval.ApprovalApproved); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := sched.Get(context.Background(), "t1"); got.Status != domain.TaskSuspended {
		t.Fatalf("after 1/2 decided: status=%s, want suspended", got.Status)
	}
	// decide second → all decided → flips to Running
	if _, err := ac.Decide(context.Background(), "t1", approval.TicketID("t1", "c2"), approval.ApprovalDenied); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := sched.Get(context.Background(), "t1"); got.Status != domain.TaskRunning {
		t.Fatalf("after all decided: status=%s, want running", got.Status)
	}
}

// TestDecideUnknownTaskFailsLoud guards the fail-loud contract: deciding on a
// ticket whose task the scheduler doesn't know about must error, never
// silently no-op.
func TestDecideUnknownTaskFailsLoud(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	sched := task.NewScheduler()
	ac := NewApprovalCoordinator(store, sched)
	if _, err := ac.Decide(context.Background(), "ghost", approval.TicketID("ghost", "c1"), approval.ApprovalApproved); err == nil {
		t.Fatal("Decide on unknown task: err = nil, want error")
	}
}

// TestDecideDoesNotFlipNonSuspendedTask covers the guard `t.Status ==
// domain.TaskSuspended` in Decide: if the task is not Suspended (e.g. still
// Running, or already Done) when the last ticket is decided, Decide must not
// attempt an invalid transition.
func TestDecideDoesNotFlipNonSuspendedTask(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	sched := task.NewScheduler()
	_ = sched.Add(context.Background(), domain.Task{ID: "t1", SessionID: "s1", Status: domain.TaskRunning})
	_, _ = store.Open(approval.ToolApproval{SessionKey: "s1", TaskID: "t1", ToolCallID: "c1", ToolName: "write_file"})
	ac := NewApprovalCoordinator(store, sched)
	if _, err := ac.Decide(context.Background(), "t1", approval.TicketID("t1", "c1"), approval.ApprovalApproved); err != nil {
		t.Fatal(err)
	}
	got, _, _ := sched.Get(context.Background(), "t1")
	if got.Status != domain.TaskRunning {
		t.Fatalf("status = %s, want unchanged running", got.Status)
	}
}
