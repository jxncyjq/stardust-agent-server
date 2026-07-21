package runtime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/task"
)

// learningFailingEventBus accepts every event except the learning signal, which
// it rejects.
//
// The discrimination matters: RunTask publishes "runtime started" before it can
// reach any learning-signal path, and a bus that failed everything would abort
// there instead, never exercising the path under test. A backend that is up but
// rejects one write is also the realistic shape of the bug — a constraint
// violation or a transient SQLite error on one row, not the whole store going
// away.
type learningFailingEventBus struct {
	mu     sync.Mutex
	err    error
	events []domain.RuntimeEvent
}

func (b *learningFailingEventBus) Publish(_ context.Context, event domain.RuntimeEvent) error {
	if event.Type == evolution.RuntimeEventLearning {
		return b.err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, event)
	return nil
}

func (b *learningFailingEventBus) Events() ([]domain.RuntimeEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]domain.RuntimeEvent(nil), b.events...), nil
}

// TestRuntimeLogsLearningPublishFailure covers audit item V7: Runtime dropped
// the error from every failure-learning publish with `_ = r.publishLearning(...)`.
//
// The consequence is not cosmetic. Those signals feed the evolution pipeline and
// the trust score; when the bus silently rejects them, a persistently failing
// agent's score never drops, TrustGate keeps letting it take tasks, and nothing
// anywhere records why. Runtime had no logger at all, so there was nowhere for
// the error to go — hence the field this test forces into existence.
func TestRuntimeLogsLearningPublishFailure(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	events := &learningFailingEventBus{err: errors.New("event bus rejected learning signal")}
	// Maas nil takes the shortest path to a failure-learning publish: RunTask
	// reports ErrMaasUnavailable and, on the way out, tries to record the signal.
	rt := NewRuntime(Config{
		Events: events,
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})

	_, err := rt.RunTask(context.Background(),
		domain.Agent{ID: "researcher", CompanyID: "company-1"},
		domain.Task{ID: "task-1", CompanyID: "company-1", Input: "x"})
	if !errors.Is(err, ErrMaasUnavailable) {
		t.Fatalf("RunTask error = %v, want %v", err, ErrMaasUnavailable)
	}

	got := logs.String()
	for _, want := range []string{"WARN", "publish failure learning event", "event bus rejected learning signal", "task-1", "researcher"} {
		if !strings.Contains(got, want) {
			t.Fatalf("logger output = %q, want it to contain %q", got, want)
		}
	}
}

// ctxCancellingRunner cancels the context the coordinator handed it and then
// fails, standing in for a run torn down mid-flight (shutdown, client
// disconnect, deadline).
//
// This is the only deterministic way to make Scheduler.Transition fail from a
// test: Coordinator holds a concrete *task.Scheduler, not an interface, so it
// cannot be replaced with a stub. Transition's failure modes are ctx.Err,
// ErrTaskNotFound and ErrInvalidTransition; the task under test is present and
// legally Running, which leaves the context.
type ctxCancellingRunner struct {
	cancel context.CancelFunc
	err    error
}

func (r *ctxCancellingRunner) RunTask(context.Context, domain.Agent, domain.Task) (domain.TaskRun, error) {
	r.cancel()
	return domain.TaskRun{}, r.err
}

// TestCoordinatorLogsFailedTransitionFailure covers audit item V6: nine
// `_ = c.scheduler.Transition(ctx, id, domain.TaskFailed)` sites dropped the
// error from the very call that was supposed to land the task in a terminal
// state.
//
// When that transition itself fails the task stays Running and, on the
// runAssigned path, keeps its lock until the lease expires — so no worker can
// pick it up and resumeScan retries the same stuck task. The visible symptom is
// a task frozen forever with nothing in the log.
func TestCoordinatorLogsFailedTransitionFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logs bytes.Buffer
	sched := task.NewScheduler()
	c := NewCoordinator(CoordinatorConfig{
		Agent:      domain.Agent{ID: "default-agent", CompanyID: "company-1", Role: "developer", Status: domain.AgentActive},
		Scheduler:  sched,
		Locks:      task.NewLockStore(),
		Runtime:    &ctxCancellingRunner{cancel: cancel, err: errors.New("runner exploded")},
		Reviewer:   quality.NewAegisReviewer(),
		Evaluator:  quality.NewEvalEngine(3),
		Approvals:  approval.NewService(),
		Audit:      adapter.NewMemoryAuditLog(),
		Events:     adapter.NewMemoryEventBus(),
		LockTTL:    time.Minute,
		MaxWorkers: 1,
		Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	})

	if err := sched.Add(ctx, domain.Task{ID: "t-stuck", AgentID: "default-agent", Status: domain.TaskPending, Input: "x"}); err != nil {
		t.Fatalf("Add(t-stuck) error = %v, want nil", err)
	}
	if _, _, err := c.Heartbeat(ctx); err != nil {
		t.Fatalf("Heartbeat() error = %v, want nil", err)
	}
	c.Wait()

	got := logs.String()
	for _, want := range []string{"mark task failed", "t-stuck"} {
		if !strings.Contains(got, want) {
			t.Fatalf("logger output = %q, want it to contain %q", got, want)
		}
	}
}

// TestSubRuntimeInheritsLogger pins a crash, not a cosmetic gap. newSubRuntime
// builds its child as a struct literal, bypassing NewRuntime and the nil-logger
// fallback there, so a child that did not carry the parent's logger over would
// panic the first time one of its failure paths tried to record anything —
// exactly where a delegated sub-task is already going wrong.
func TestSubRuntimeInheritsLogger(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	parent := NewRuntime(Config{Logger: logger, MaxSpawnDepth: 3})

	child, err := parent.newSubRuntime("leaf", nil)
	if err != nil {
		t.Fatalf("newSubRuntime error = %v, want nil", err)
	}
	if child.logger == nil {
		t.Fatal("child runtime has a nil logger; its failure paths would panic")
	}
	if child.logger != parent.logger {
		t.Errorf("child logger = %p, want the parent's %p", child.logger, parent.logger)
	}
}
