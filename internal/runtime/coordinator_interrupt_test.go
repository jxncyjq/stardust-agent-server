package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/task"
)

// blockingRunner blocks in RunTask until its context is cancelled, then returns
// the context's error. It stands in for a real tool-loop that is stopped
// mid-flight by a per-task cancel: the runtime's generate/executeToolCalls
// return ctx.Err() on cancellation, so RunTask surfaces context.Canceled the
// same way. started is closed once RunTask is actually executing so a test can
// wait for the task to be genuinely running before interrupting it.
type blockingRunner struct {
	started chan struct{}
	once    bool
}

func (r *blockingRunner) RunTask(ctx context.Context, _ domain.Agent, _ domain.Task) (domain.TaskRun, error) {
	if !r.once {
		r.once = true
		close(r.started)
	}
	<-ctx.Done()
	return domain.TaskRun{}, ctx.Err()
}

// TestCoordinatorInterruptCancelsRunningTask dispatches a task whose runner
// blocks until cancelled, interrupts it, and asserts it lands in Cancelled —
// not Failed. Cancellation must be a first-class terminal outcome, distinct
// from a run that errored out.
func TestCoordinatorInterruptCancelsRunningTask(t *testing.T) {
	sched := task.NewScheduler()
	runner := &blockingRunner{started: make(chan struct{})}
	coord := newTestCoordinatorWithRunner(t, sched, runner)

	ctx := context.Background()
	const id = "t-interrupt"
	if err := sched.Add(ctx, domain.Task{ID: id, AgentID: "default-agent", Status: domain.TaskPending, Input: "x"}); err != nil {
		t.Fatalf("Add(%q) error = %v, want nil", id, err)
	}
	if _, _, err := coord.Heartbeat(ctx); err != nil {
		t.Fatalf("Heartbeat() error = %v, want nil", err)
	}

	// Wait until the runner is actually executing (the task is truly running),
	// so the interrupt targets a live run rather than racing dispatch.
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("runner never started; task was not dispatched")
	}

	if err := coord.Interrupt(id); err != nil {
		t.Fatalf("Interrupt(%q) error = %v, want nil", id, err)
	}
	coord.Wait()

	got, ok, err := sched.Get(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Get(%q) ok=%v err=%v", id, ok, err)
	}
	if got.Status != domain.TaskCancelled {
		t.Fatalf("interrupted task status = %q, want %q", got.Status, domain.TaskCancelled)
	}
}

// TestCoordinatorInterruptResumedTaskCancels drives a task through the RESUME
// dispatch path (a Running+checkpointed task picked up by resumeScan →
// runResume, as if ApprovalCoordinator.Decide had just flipped it
// Suspended→Running) whose runner blocks until cancelled, then interrupts it.
// It asserts the resumed run is interruptible at all — Interrupt must find the
// task in the cancels map and return nil — and that the cancel lands the task
// in Cancelled, NOT Failed. This mirrors TestCoordinatorInterruptCancelsRunningTask
// but for the resume goroutine, which was previously unregistered (Interrupt
// 404'd) and mis-classified a cancel as Failed.
func TestCoordinatorInterruptResumedTaskCancels(t *testing.T) {
	dir := t.TempDir()
	store := sessionstate.NewStore(dir)
	const id = "t-resume-interrupt"
	if err := store.Save(sessionstate.Checkpoint{
		SchemaVersion: sessionstate.CheckpointSchemaVersion,
		TaskID:        id,
		AgentID:       "default-agent",
		SessionKey:    "s1",
		Mode:          domain.ModeManual,
		PendingCalls:  []domain.ToolCall{{ID: "c1", Name: "read_x"}},
	}); err != nil {
		t.Fatalf("Save checkpoint error = %v, want nil", err)
	}

	sched := task.NewScheduler()
	ctx := context.Background()
	// Already Running (post-decision) with a persisted checkpoint: exactly what
	// resumeScan looks for, so Heartbeat dispatches it via runResume.
	if err := sched.Add(ctx, domain.Task{
		ID: id, AgentID: "default-agent", SessionID: "s1", Mode: domain.ModeManual, Status: domain.TaskRunning,
	}); err != nil {
		t.Fatalf("Add(%q) error = %v, want nil", id, err)
	}

	runner := &blockingRunner{started: make(chan struct{})}
	coord := NewCoordinator(CoordinatorConfig{
		Agent:       domain.Agent{ID: "default-agent", Status: domain.AgentActive},
		Scheduler:   sched,
		Locks:       task.NewLockStore(),
		Runtime:     runner,
		Reviewer:    quality.NewAegisReviewer(),
		Evaluator:   quality.NewEvalEngine(3),
		Approvals:   approval.NewService(),
		Audit:       adapter.NewMemoryAuditLog(),
		Events:      adapter.NewMemoryEventBus(),
		LockTTL:     time.Minute,
		Checkpoints: store,
	})

	if _, _, err := coord.Heartbeat(ctx); err != nil {
		t.Fatalf("Heartbeat() error = %v, want nil", err)
	}

	// Wait until the resumed runner is actually executing before interrupting,
	// so the interrupt targets a live resume rather than racing dispatch.
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("runner never started; resumed task was not dispatched")
	}

	if err := coord.Interrupt(id); err != nil {
		t.Fatalf("Interrupt(%q) error = %v, want nil (resumed task must be registered in cancels map)", id, err)
	}
	coord.Wait()

	got, ok, err := sched.Get(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Get(%q) ok=%v err=%v", id, ok, err)
	}
	if got.Status != domain.TaskCancelled {
		t.Fatalf("interrupted resumed task status = %q, want %q", got.Status, domain.TaskCancelled)
	}
}

// TestCoordinatorInterruptUnknownTaskErrors verifies interrupting a task that is
// not currently running returns an error rather than pretending it happened.
func TestCoordinatorInterruptUnknownTaskErrors(t *testing.T) {
	sched := task.NewScheduler()
	coord := newTestCoordinatorWithRunner(t, sched, &recordingTaskRunner{result: "ok"})

	if err := coord.Interrupt("nope"); err == nil {
		t.Fatalf("Interrupt(%q) error = nil, want non-nil", "nope")
	}
}
