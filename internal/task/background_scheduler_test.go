package task

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/memory"
)

func TestBackgroundSchedulerRunOnceRejectsReentry(t *testing.T) {
	t.Parallel()

	scheduler := NewBackgroundScheduler()
	var calls atomic.Int32
	scheduler.AddJob("reentrant", func(ctx context.Context) error {
		calls.Add(1)
		err := scheduler.RunOnce(ctx)
		if !errors.Is(err, ErrBackgroundSchedulerRunning) {
			t.Errorf("RunOnce() nested error = %v, want ErrBackgroundSchedulerRunning", err)
		}
		return nil
	})

	if err := scheduler.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v, want nil", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("job calls = %d, want 1", got)
	}
}

func TestLockStoreReapExpiredRemovesOnlyExpiredLocks(t *testing.T) {
	t.Parallel()

	store := NewLockStore()
	ctx := context.Background()
	if ok, err := store.TryLock(ctx, "expired", "agent-1", -time.Second); err != nil || !ok {
		t.Fatalf("TryLock(%q) = %t, %v; want true, nil", "expired", ok, err)
	}
	if ok, err := store.TryLock(ctx, "fresh", "agent-1", time.Minute); err != nil || !ok {
		t.Fatalf("TryLock(%q) = %t, %v; want true, nil", "fresh", ok, err)
	}

	reaped, err := store.ReapExpired(ctx, time.Now())
	if err != nil {
		t.Fatalf("ReapExpired() error = %v, want nil", err)
	}
	if reaped != 1 {
		t.Errorf("ReapExpired() = %d, want 1", reaped)
	}
	if ok, err := store.TryLock(ctx, "fresh", "agent-2", time.Minute); err != nil || ok {
		t.Errorf("TryLock(%q) after reap = %t, %v; want false, nil", "fresh", ok, err)
	}
}

func TestBackgroundSchedulerStartStops(t *testing.T) {
	t.Parallel()

	scheduler := NewBackgroundScheduler()
	var calls atomic.Int32
	scheduler.AddJob("counter", func(context.Context) error {
		calls.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := scheduler.Start(ctx, time.Millisecond)
	waitForCalls(t, &calls)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start() did not stop after context cancellation")
	}
}

func TestLockReaperJobRemovesExpiredLocks(t *testing.T) {
	t.Parallel()

	store := NewLockStore()
	ctx := context.Background()
	if ok, err := store.TryLock(ctx, "expired", "agent-1", -time.Second); err != nil || !ok {
		t.Fatalf("TryLock(%q) = %t, %v; want true, nil", "expired", ok, err)
	}
	scheduler := NewBackgroundScheduler()
	scheduler.AddJob("reap-locks", NewLockReaperJob(store, func() time.Time {
		return time.Now()
	}))

	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce() error = %v, want nil", err)
	}
	if ok, err := store.TryLock(ctx, "expired", "agent-2", time.Minute); err != nil || !ok {
		t.Errorf("TryLock(%q) after reaper = %t, %v; want true, nil", "expired", ok, err)
	}
}

func TestGepFailureScanJobRunsCycleForFailureLearningEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	audit := adapter.NewHashChainAuditLog()
	store := memory.NewCapabilityMemoryStore()
	cycle := evolution.NewGepCycle(evolution.GepCycleConfig{
		Extractor:       evolution.NewSignalExtractor(),
		CapabilityStore: store,
		EventLog:        evolution.NewEvolutionEventLog(audit),
	})
	if err := events.Publish(ctx, evolution.NewLearningRuntimeEvent(evolution.LearningEvent{
		AgentID:       "agent-1",
		TaskID:        "task-fail",
		Signal:        evolution.SignalFailure,
		Reason:        evolution.FailureReasonInferenceError,
		IsLightweight: true,
		PublishedAt:   time.Now(),
	})); err != nil {
		t.Fatalf("Publish(failure learning event) error = %v, want nil", err)
	}
	scheduler := NewBackgroundScheduler()
	scheduler.AddJob("gep_failure_scan", NewGepFailureScanJob(events, cycle))

	if err := scheduler.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce() error = %v, want nil", err)
	}
	hits, err := store.SearchGenes(ctx, memory.CapabilityQuery{Text: "task-fail", Tags: []string{"failure"}, TopK: 1})
	if err != nil {
		t.Fatalf("SearchGenes() error = %v, want nil", err)
	}
	if len(hits) != 1 {
		t.Fatalf("SearchGenes() len = %d, want 1", len(hits))
	}
	auditEvents, err := audit.Events()
	if err != nil {
		t.Fatalf("audit.Events() error = %v, want nil", err)
	}
	if len(auditEvents) != 6 {
		t.Fatalf("audit.Events() len = %d, want 6", len(auditEvents))
	}
	if err := audit.VerifyChain(ctx); err != nil {
		t.Fatalf("VerifyChain() error = %v, want nil", err)
	}
}

func TestGepFailureScanJobIgnoresSuccessAndProcessedEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	runner := &recordingGepRunner{}
	job := NewGepFailureScanJob(events, runner)
	if err := events.Publish(ctx, evolution.NewLearningRuntimeEvent(evolution.LearningEvent{
		AgentID:     "agent-1",
		TaskID:      "task-success",
		Signal:      evolution.SignalSuccess,
		PublishedAt: time.Now(),
	})); err != nil {
		t.Fatalf("Publish(success learning event) error = %v, want nil", err)
	}
	if err := events.Publish(ctx, evolution.NewLearningRuntimeEvent(evolution.LearningEvent{
		AgentID:     "agent-1",
		TaskID:      "task-loop",
		Signal:      evolution.SignalHardLoopFailure,
		Reason:      evolution.FailureReasonHardLoop,
		PublishedAt: time.Now(),
	})); err != nil {
		t.Fatalf("Publish(hardloop learning event) error = %v, want nil", err)
	}

	if err := job(ctx); err != nil {
		t.Fatalf("GepFailureScanJob() first error = %v, want nil", err)
	}
	if err := job(ctx); err != nil {
		t.Fatalf("GepFailureScanJob() second error = %v, want nil", err)
	}
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("GepRunner calls = %d, want 1", got)
	}
	if runner.lastInput.Task.Status != domain.TaskSuspended {
		t.Fatalf("GepRunner last task status = %s, want %s", runner.lastInput.Task.Status, domain.TaskSuspended)
	}
}

// Regression: a single failed run publishes more than one failure learning
// event (inference_error, then task_run_error), typically within the same
// second. Dedup keyed on the event itself let both through, but
// ExtractionInput.Cycle is PublishedAt.Unix(), so both map to the SAME GEP
// cycle id — and the cycle writes an audit entry keyed by that id. The second
// run therefore died on "UNIQUE constraint failed: audit_events.id", surfacing
// as a recurring background-scheduler error in the logs.
func TestGepFailureScanJobRunsOnceForSameCycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	runner := &recordingGepRunner{}
	job := NewGepFailureScanJob(events, runner)

	publishedAt := time.Now()
	for _, reason := range []string{evolution.FailureReasonInferenceError, "task_run_error"} {
		if err := events.Publish(ctx, evolution.NewLearningRuntimeEvent(evolution.LearningEvent{
			AgentID:       "agent-1",
			TaskID:        "task-fail",
			Signal:        evolution.SignalFailure,
			Reason:        reason,
			IsLightweight: true,
			PublishedAt:   publishedAt,
		})); err != nil {
			t.Fatalf("Publish(%s learning event) error = %v, want nil", reason, err)
		}
	}

	if err := job(ctx); err != nil {
		t.Fatalf("GepFailureScanJob() error = %v, want nil", err)
	}
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("GepRunner calls = %d, want 1 (both events belong to the same GEP cycle)", got)
	}
}

// A later failure of the same task is a new cycle (Cycle is a second-resolution
// timestamp) and must still be scanned — the dedup must not swallow it.
func TestGepFailureScanJobRunsAgainForLaterCycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	runner := &recordingGepRunner{}
	job := NewGepFailureScanJob(events, runner)

	first := time.Now()
	for _, at := range []time.Time{first, first.Add(2 * time.Second)} {
		if err := events.Publish(ctx, evolution.NewLearningRuntimeEvent(evolution.LearningEvent{
			AgentID:       "agent-1",
			TaskID:        "task-fail",
			Signal:        evolution.SignalFailure,
			Reason:        evolution.FailureReasonInferenceError,
			IsLightweight: true,
			PublishedAt:   at,
		})); err != nil {
			t.Fatalf("Publish(learning event at %s) error = %v, want nil", at, err)
		}
	}

	if err := job(ctx); err != nil {
		t.Fatalf("GepFailureScanJob() error = %v, want nil", err)
	}
	if got := runner.calls.Load(); got != 2 {
		t.Fatalf("GepRunner calls = %d, want 2 (distinct cycles)", got)
	}
}

type recordingGepRunner struct {
	calls     atomic.Int32
	lastInput evolution.ExtractionInput
}

func (r *recordingGepRunner) Run(_ context.Context, input evolution.ExtractionInput) (evolution.GepResult, error) {
	r.calls.Add(1)
	r.lastInput = input
	return evolution.GepResult{}, nil
}

func waitForCalls(t *testing.T, calls *atomic.Int32) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatalf("job calls = %d, want at least 1", calls.Load())
		case <-ticker.C:
			if calls.Load() > 0 {
				return
			}
		}
	}
}
