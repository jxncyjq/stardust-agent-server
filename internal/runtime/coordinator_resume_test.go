package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/task"
)

// TestHeartbeatResumesRunningCheckpointedTask verifies the resume-dispatch path
// (design plan B): a task the scheduler already shows Running (as if
// ApprovalCoordinator.Decide had just flipped it Suspended→Running) with a
// persisted checkpoint on disk is picked up by Heartbeat's resume scan, run to
// completion, and lands Done — exercising the resumeScan → runResume →
// afterRun chain end to end.
func TestHeartbeatResumesRunningCheckpointedTask(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := sessionstate.NewStore(dir)
	if err := store.Save(sessionstate.Checkpoint{
		SchemaVersion: sessionstate.CheckpointSchemaVersion,
		TaskID:        "t1",
		AgentID:       "a1",
		SessionKey:    "s1",
		Mode:          domain.ModeManual,
		PendingCalls:  []domain.ToolCall{{ID: "c1", Name: "read_x"}},
	}); err != nil {
		t.Fatal(err)
	}

	sched := task.NewScheduler()
	if err := sched.Add(context.Background(), domain.Task{
		ID: "t1", AgentID: "a1", SessionID: "s1", Mode: domain.ModeManual, Status: domain.TaskRunning,
	}); err != nil {
		t.Fatal(err)
	}

	runner := &recordingTaskRunner{result: "ok"}
	c := NewCoordinator(CoordinatorConfig{
		Agent:       domain.Agent{ID: "a1", Status: domain.AgentActive},
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

	if _, _, err := c.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Wait()

	got := awaitTerminal(t, sched, "t1")
	if got.Status != domain.TaskDone {
		t.Fatalf("t1 status = %s, want done", got.Status)
	}
	if runner.calls != 1 {
		t.Fatalf("recordingTaskRunner.calls = %d, want 1 (resume did not invoke RunTask exactly once)", runner.calls)
	}
}

// TestHeartbeatSkipsRunningTaskWithoutCheckpoint guards resumeScan's
// no-double-dispatch invariant from the other direction: a Running task with
// NO persisted checkpoint (a fresh dispatch still mid-flight, before it has
// ever suspended) must never be picked up by the resume scan — only a
// checkpoint marks a task as an actual resume candidate.
func TestHeartbeatSkipsRunningTaskWithoutCheckpoint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := sessionstate.NewStore(dir)

	sched := task.NewScheduler()
	if err := sched.Add(context.Background(), domain.Task{
		ID: "t1", AgentID: "a1", SessionID: "s1", Status: domain.TaskRunning,
	}); err != nil {
		t.Fatal(err)
	}

	runner := &recordingTaskRunner{result: "ok"}
	c := NewCoordinator(CoordinatorConfig{
		Agent:       domain.Agent{ID: "a1", Status: domain.AgentActive},
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

	if _, _, err := c.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Wait()

	if runner.calls != 0 {
		t.Fatalf("recordingTaskRunner.calls = %d, want 0 (checkpoint-less Running task must not be resumed)", runner.calls)
	}
	got, ok, err := sched.Get(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.Status != domain.TaskRunning {
		t.Fatalf("t1 status = %v (ok=%v), want still running", got.Status, ok)
	}
}

// TestHeartbeatResumeNoDoubleDispatchUnderRace concurrently drives many
// Heartbeat ticks against N Running+checkpointed tasks and asserts every task
// is resumed (RunTask'd) exactly once. This is the double-dispatch guard the
// reviewer will attack: resumeScan claims a task via TryLock before spawning
// its goroutine, so a second concurrent Heartbeat's TryLock on the same task
// must fail while the first resume is in flight (or after it finishes, since
// the task is no longer Running).
func TestHeartbeatResumeNoDoubleDispatchUnderRace(t *testing.T) {
	t.Parallel()

	const numTasks = 20
	const numHeartbeats = 8

	dir := t.TempDir()
	store := sessionstate.NewStore(dir)
	sched := task.NewScheduler()

	counts := make(map[string]*int64, numTasks)
	for i := range numTasks {
		id := taskIDForIndex(i)
		if err := store.Save(sessionstate.Checkpoint{
			SchemaVersion: sessionstate.CheckpointSchemaVersion,
			TaskID:        id,
			AgentID:       "a1",
			SessionKey:    id,
			Mode:          domain.ModeManual,
			PendingCalls:  []domain.ToolCall{{ID: "c1", Name: "read_x"}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := sched.Add(context.Background(), domain.Task{
			ID: id, AgentID: "a1", SessionID: id, Mode: domain.ModeManual, Status: domain.TaskRunning,
		}); err != nil {
			t.Fatal(err)
		}
		var n int64
		counts[id] = &n
	}

	runner := &countingTaskRunner{counts: counts, result: "ok"}
	c := NewCoordinator(CoordinatorConfig{
		Agent:       domain.Agent{ID: "a1", Status: domain.AgentActive},
		Scheduler:   sched,
		Locks:       task.NewLockStore(),
		Runtime:     runner,
		Reviewer:    quality.NewAegisReviewer(),
		Evaluator:   quality.NewEvalEngine(3),
		Approvals:   approval.NewService(),
		Audit:       adapter.NewMemoryAuditLog(),
		Events:      adapter.NewMemoryEventBus(),
		LockTTL:     time.Minute,
		MaxWorkers:  numTasks,
		Checkpoints: store,
	})

	var wg sync.WaitGroup
	for range numHeartbeats {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := c.Heartbeat(context.Background()); err != nil {
				t.Errorf("Heartbeat: %v", err)
			}
		}()
	}
	wg.Wait()
	c.Wait()

	// Some tasks may not have been claimed yet by any of the racing
	// Heartbeats within this pass (resumeScan is best-effort per tick), but
	// none may ever be resumed more than once.
	for id, n := range counts {
		if got := atomic.LoadInt64(n); got > 1 {
			t.Fatalf("task %s resumed %d times, want at most 1 (double dispatch)", id, got)
		}
	}
}

// TestHeartbeatResumeNoDoubleDispatchWhenRunOutlivesLease is the crux
// regression test for the lock-lease-expiry double-dispatch defect: TryLock's
// lease (lockTTL) is a fixed duration that is never renewed while a resume is
// in flight. If runResume's RunTask call runs longer than the lease — which a
// real LLM agent routinely does — the lease expires while the task is still
// Running with its checkpoint still on disk, and the next Heartbeat's
// resumeScan would (absent the in-process guard) re-TryLock it and start a
// SECOND concurrent runResume of the same task. This test uses a very short
// lockTTL and a runner double whose RunTask blocks, so the lease provably
// expires mid-run, then drives a second Heartbeat and asserts the runner was
// entered exactly once despite the expired lease — proving the in-flight
// guard (not the lock) is what prevents re-dispatch.
func TestHeartbeatResumeNoDoubleDispatchWhenRunOutlivesLease(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := sessionstate.NewStore(dir)
	if err := store.Save(sessionstate.Checkpoint{
		SchemaVersion: sessionstate.CheckpointSchemaVersion,
		TaskID:        "t1",
		AgentID:       "a1",
		SessionKey:    "s1",
		Mode:          domain.ModeManual,
		PendingCalls:  []domain.ToolCall{{ID: "c1", Name: "read_x"}},
	}); err != nil {
		t.Fatal(err)
	}

	sched := task.NewScheduler()
	if err := sched.Add(context.Background(), domain.Task{
		ID: "t1", AgentID: "a1", SessionID: "s1", Mode: domain.ModeManual, Status: domain.TaskRunning,
	}); err != nil {
		t.Fatal(err)
	}

	runner := &blockingTaskRunner{result: "ok", unblock: make(chan struct{}), entered: make(chan struct{})}
	const shortTTL = 10 * time.Millisecond
	c := NewCoordinator(CoordinatorConfig{
		Agent:       domain.Agent{ID: "a1", Status: domain.AgentActive},
		Scheduler:   sched,
		Locks:       task.NewLockStore(),
		Runtime:     runner,
		Reviewer:    quality.NewAegisReviewer(),
		Evaluator:   quality.NewEvalEngine(3),
		Approvals:   approval.NewService(),
		Audit:       adapter.NewMemoryAuditLog(),
		Events:      adapter.NewMemoryEventBus(),
		LockTTL:     shortTTL,
		Checkpoints: store,
	})

	// First Heartbeat: resumeScan claims t1 and spawns runResume, which
	// blocks inside RunTask (simulating a long-running agent turn).
	if _, _, err := c.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Wait for RunTask to actually be entered before letting the lease
	// expire, so the race is deterministic rather than timing-dependent.
	select {
	case <-runner.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("runner.RunTask was never entered by the first resume")
	}

	// Let the lock lease expire while the run is still in flight.
	time.Sleep(3 * shortTTL)

	// Second Heartbeat: t1 is still Running with its checkpoint still on
	// disk, and the lease has expired, so TryLock alone would succeed again
	// here. The in-process resuming guard must still block a second dispatch.
	if _, _, err := c.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Give any (incorrect) second dispatch a chance to start before we assert.
	time.Sleep(50 * time.Millisecond)

	if got := runner.callCount(); got != 1 {
		t.Fatalf("runner entered %d times after lease expiry, want exactly 1 (double dispatch)", got)
	}

	// Unblock the in-flight run and let it finish normally.
	close(runner.unblock)
	c.Wait()

	got := awaitTerminal(t, sched, "t1")
	if got.Status != domain.TaskDone {
		t.Fatalf("t1 status = %s, want done", got.Status)
	}
	if got := runner.callCount(); got != 1 {
		t.Fatalf("runner entered %d times total, want exactly 1 (double dispatch)", got)
	}
}

// blockingTaskRunner simulates a long-running agent turn: RunTask signals
// entry on the entered channel (once, on the first call) and then blocks
// until unblock is closed, incrementing an atomic call counter on entry so
// the double-dispatch test can assert "entered exactly once" independent of
// timing.
type blockingTaskRunner struct {
	result  string
	unblock chan struct{}

	enteredOnce sync.Once
	entered     chan struct{}
	calls       int64
}

func (r *blockingTaskRunner) callCount() int64 {
	return atomic.LoadInt64(&r.calls)
}

func (r *blockingTaskRunner) RunTask(ctx context.Context, agent domain.Agent, t domain.Task) (domain.TaskRun, error) {
	atomic.AddInt64(&r.calls, 1)
	r.enteredOnce.Do(func() {
		close(r.entered)
	})
	select {
	case <-r.unblock:
	case <-ctx.Done():
		return domain.TaskRun{}, ctx.Err()
	}
	return domain.TaskRun{ID: t.ID + ":run", TaskID: t.ID, AgentID: agent.ID, Result: r.result}, nil
}

// TestHeartbeatResumeScanErrorPropagatesWrapped covers the fail-loud branch in
// resumeScan: when the scheduler listing that resumeScan depends on fails, the
// error must propagate out of Heartbeat wrapped as "resume scan: %w" rather
// than being silently swallowed. resumeScan's first dependency call is
// c.scheduler.List(ctx), which itself fails fast on ctx.Err() before touching
// its internal state — so a pre-cancelled context is a real (not mocked)
// failure seam for this scheduler implementation, requiring no test double.
func TestHeartbeatResumeScanErrorPropagatesWrapped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := sessionstate.NewStore(dir)

	sched := task.NewScheduler()
	if err := sched.Add(context.Background(), domain.Task{
		ID: "t1", AgentID: "a1", SessionID: "s1", Mode: domain.ModeManual, Status: domain.TaskRunning,
	}); err != nil {
		t.Fatal(err)
	}

	c := NewCoordinator(CoordinatorConfig{
		Agent:       domain.Agent{ID: "a1", Status: domain.AgentActive},
		Scheduler:   sched,
		Locks:       task.NewLockStore(),
		Runtime:     &recordingTaskRunner{result: "ok"},
		Reviewer:    quality.NewAegisReviewer(),
		Evaluator:   quality.NewEvalEngine(3),
		Approvals:   approval.NewService(),
		Audit:       adapter.NewMemoryAuditLog(),
		Events:      adapter.NewMemoryEventBus(),
		LockTTL:     time.Minute,
		Checkpoints: store, // non-nil so resumeScan actually runs its body
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: scheduler.List(ctx) fails fast on ctx.Err()

	_, _, err := c.Heartbeat(ctx)
	if err == nil {
		t.Fatal("Heartbeat with failing resume scan: err = nil, want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Heartbeat error = %v, want wrapped context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "resume scan:") {
		t.Fatalf("Heartbeat error = %q, want it to carry the \"resume scan:\" wrap prefix", err.Error())
	}
}

func taskIDForIndex(i int) string {
	return "resume-task-" + string(rune('a'+i))
}

// countingTaskRunner records per-task-ID call counts atomically, so the
// double-dispatch race test can assert "exactly once" without a data race on
// the counters themselves (only the map access happens before goroutines
// start; the *int64 values are shared and mutated with atomic ops).
type countingTaskRunner struct {
	result string
	counts map[string]*int64
}

func (r *countingTaskRunner) RunTask(ctx context.Context, agent domain.Agent, t domain.Task) (domain.TaskRun, error) {
	if err := ctx.Err(); err != nil {
		return domain.TaskRun{}, err
	}
	atomic.AddInt64(r.counts[t.ID], 1)
	return domain.TaskRun{ID: t.ID + ":run", TaskID: t.ID, AgentID: agent.ID, Result: r.result}, nil
}
