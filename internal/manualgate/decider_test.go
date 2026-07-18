package manualgate

import (
	"context"
	"sync"
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

// TestDecideEmitsApprovalResolved covers the sink emission added on top of
// Decide's on-disk commit: once store.Decide records the decision, the
// coordinator must notify its ApprovalEventSink with the ticket and decision,
// regardless of any subsequent transition outcome. spyApprovalSink is defined
// in manualgate_test.go (same package) and reused by ShouldSuspend's
// approval_pending test. There is no existing SchedulerGate fake in this
// package — every other Decide test here drives a real *task.Scheduler — so
// this test does the same rather than inventing a fake construction.
func TestDecideEmitsApprovalResolved(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	rec, err := store.Open(approval.ToolApproval{
		SessionKey: "s1", TaskID: "task-1", ToolCallID: "call-1", ToolName: "write_file",
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v, want nil", err)
	}
	sched := task.NewScheduler()
	if err := sched.Add(context.Background(), domain.Task{ID: "task-1", SessionID: "s1", Status: domain.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Transition(context.Background(), "task-1", domain.TaskSuspended); err != nil {
		t.Fatal(err)
	}
	sink := &spyApprovalSink{}
	coord := NewApprovalCoordinator(store, sched, WithCoordinatorSink(sink))

	if _, err := coord.Decide(context.Background(), "task-1", rec.TicketID, approval.ApprovalApproved); err != nil {
		t.Fatalf("Decide() error = %v, want nil", err)
	}
	want := rec.TicketID + ":approved"
	if len(sink.resolved) != 1 || sink.resolved[0] != want {
		t.Fatalf("sink.resolved = %v, want [%s]", sink.resolved, want)
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

// TestDecideConcurrentFinalDecisionsNoSpuriousError guards the fix for the
// race in Decide's Suspended->Running flip: the task's Status is read once
// (via sched.Get, above) before store.Decide/ListForTask run, so two
// concurrent Decide calls on the two tickets of the same task can both
// observe Suspended+allDecided and both attempt the Suspended->Running
// transition. Without tolerance for the "someone else already flipped it"
// case, the loser would get ErrInvalidTransition for a decision that WAS
// validly recorded — a legitimate decision spuriously erroring. Both
// goroutines here must return no error, and the task must end Running.
func TestDecideConcurrentFinalDecisionsNoSpuriousError(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	sched := task.NewScheduler()
	if err := sched.Add(context.Background(), domain.Task{ID: "t1", SessionID: "s1", Status: domain.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Transition(context.Background(), "t1", domain.TaskSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(approval.ToolApproval{SessionKey: "s1", TaskID: "t1", ToolCallID: "c1", ToolName: "write_file"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(approval.ToolApproval{SessionKey: "s1", TaskID: "t1", ToolCallID: "c2", ToolName: "send_message"}); err != nil {
		t.Fatal(err)
	}
	ac := NewApprovalCoordinator(store, sched)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := ac.Decide(context.Background(), "t1", approval.TicketID("t1", "c1"), approval.ApprovalApproved)
		errs[0] = err
	}()
	go func() {
		defer wg.Done()
		_, err := ac.Decide(context.Background(), "t1", approval.TicketID("t1", "c2"), approval.ApprovalDenied)
		errs[1] = err
	}()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Decide goroutine %d: err = %v, want nil", i, err)
		}
	}
	got, ok, err := sched.Get(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.Status != domain.TaskRunning {
		t.Fatalf("t1 status = %v (ok=%v), want running", got.Status, ok)
	}
}

// TestReconcileResumeFlipsWhenAllTicketsAlreadyDecided covers the restart
// path: a task suspended before a crash whose ticket was approved (but the
// resume dispatch never ran) must flip Suspended->Running when
// ReconcileResume runs at the next startup.
func TestReconcileResumeFlipsWhenAllTicketsAlreadyDecided(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	sched := task.NewScheduler()
	ctx := context.Background()
	if err := sched.Add(ctx, domain.Task{ID: "t1", SessionID: "s1", Status: domain.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Transition(ctx, "t1", domain.TaskSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(approval.ToolApproval{SessionKey: "s1", TaskID: "t1", ToolCallID: "c1", ToolName: "write_file"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalApproved); err != nil {
		t.Fatal(err)
	}
	ac := NewApprovalCoordinator(store, sched)
	if err := ac.ReconcileResume(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	got, _, err := sched.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskRunning {
		t.Fatalf("status = %s, want running", got.Status)
	}
}

// TestReconcileResumeNoTicketsStaysSuspended covers the "suspended for a
// different reason" case: a task with no recorded approval tickets at all
// must NOT be resumed by ReconcileResume — it is not this coordinator's
// decision to make.
func TestReconcileResumeNoTicketsStaysSuspended(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	sched := task.NewScheduler()
	ctx := context.Background()
	if err := sched.Add(ctx, domain.Task{ID: "t1", SessionID: "s1", Status: domain.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Transition(ctx, "t1", domain.TaskSuspended); err != nil {
		t.Fatal(err)
	}
	ac := NewApprovalCoordinator(store, sched)
	if err := ac.ReconcileResume(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	got, _, err := sched.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskSuspended {
		t.Fatalf("status = %s, want still suspended (no tickets)", got.Status)
	}
}

// TestReconcileResumePendingTicketStaysSuspended covers the "still waiting on
// a human" case: a task with at least one ApprovalPending ticket must stay
// Suspended.
func TestReconcileResumePendingTicketStaysSuspended(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	sched := task.NewScheduler()
	ctx := context.Background()
	if err := sched.Add(ctx, domain.Task{ID: "t1", SessionID: "s1", Status: domain.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Transition(ctx, "t1", domain.TaskSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(approval.ToolApproval{SessionKey: "s1", TaskID: "t1", ToolCallID: "c1", ToolName: "write_file"}); err != nil {
		t.Fatal(err)
	}
	ac := NewApprovalCoordinator(store, sched)
	if err := ac.ReconcileResume(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	got, _, err := sched.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskSuspended {
		t.Fatalf("status = %s, want still suspended (pending ticket)", got.Status)
	}
}

// TestReconcileResumeUnknownTaskFailsLoud guards the fail-loud contract for
// the reconcile path: an unknown task must error, not silently no-op.
func TestReconcileResumeUnknownTaskFailsLoud(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	sched := task.NewScheduler()
	ac := NewApprovalCoordinator(store, sched)
	if err := ac.ReconcileResume(context.Background(), "ghost"); err == nil {
		t.Fatal("ReconcileResume on unknown task: err = nil, want error")
	}
}

// TestReconcileResumeNonSuspendedTaskIsNoop covers a task that is not
// Suspended at all (e.g. already Running or Done): ReconcileResume must not
// attempt any transition.
func TestReconcileResumeNonSuspendedTaskIsNoop(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	sched := task.NewScheduler()
	ctx := context.Background()
	if err := sched.Add(ctx, domain.Task{ID: "t1", SessionID: "s1", Status: domain.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	ac := NewApprovalCoordinator(store, sched)
	if err := ac.ReconcileResume(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	got, _, err := sched.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskRunning {
		t.Fatalf("status = %s, want unchanged running", got.Status)
	}
}
