package runtime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/task"
)

// flakyStatusSink fails the durable write for one target status and lets every
// other one through, standing in for a transient storage failure that hits a
// single state change.
type flakyStatusSink struct {
	failOn domain.TaskStatus
}

func (s flakyStatusSink) UpdateTaskStatus(ctx context.Context, taskID string, status domain.TaskStatus, agentID string) error {
	if status == s.failOn {
		return errors.New("storage unavailable")
	}
	return nil
}

// TestCoordinatorReturnsTaskToPendingWhenMarkRunningFails covers the window
// between the scheduler handing a task out and the dispatcher marking it
// Running. If the durable write of Running fails, the task is left Assigned --
// and Next only ever scans Pending, while resumeScan only picks up Suspended
// tasks and Running ones with a checkpoint. Without compensation the task is
// unreachable by every path there is, and the client polls a task that will
// never finish. Nothing has run at that point, so undoing the claim is safe.
func TestCoordinatorReturnsTaskToPendingWhenMarkRunningFails(t *testing.T) {
	t.Parallel()

	scheduler := task.NewSchedulerWithSink(flakyStatusSink{failOn: domain.TaskRunning})
	if err := scheduler.Add(context.Background(), domain.Task{
		ID:        "task-1",
		CompanyID: "company-1",
		Status:    domain.TaskPending,
		Input:     "ship it",
	}); err != nil {
		t.Fatalf("Add() error = %v, want nil", err)
	}
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	locks := task.NewLockStore()
	coordinator := NewCoordinator(CoordinatorConfig{
		Agent: domain.Agent{
			ID:        "agent-1",
			CompanyID: "company-1",
			Role:      "developer",
			Status:    domain.AgentActive,
		},
		Scheduler: scheduler,
		Locks:     locks,
		Runtime: NewRuntime(Config{
			Maas:   adapter.NewRecordingMaas("safe result"),
			Audit:  audit,
			Events: events,
		}),
		Reviewer:  quality.NewAegisReviewer(),
		Evaluator: quality.NewEvalEngine(3),
		Approvals: approval.NewService(),
		Audit:     audit,
		Events:    events,
		LockTTL:   time.Minute,
	})

	if _, _, err := coordinator.Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat() error = %v, want nil", err)
	}
	coordinator.Wait()

	stored, ok, err := scheduler.Get(context.Background(), "task-1")
	if err != nil || !ok {
		t.Fatalf("Get(task-1) = (_, %v, %v), want (_, true, nil)", ok, err)
	}
	if stored.Status != domain.TaskPending {
		t.Errorf("task status = %q, want %q; a task whose dispatch failed before it ran must stay claimable", stored.Status, domain.TaskPending)
	}

	// The lock must go back too: a released task that is still locked cannot be
	// picked up again until the lease expires.
	locked, err := locks.TryLock(context.Background(), "task-1", "agent-2", time.Minute)
	if err != nil {
		t.Fatalf("TryLock(task-1, agent-2) error = %v, want nil", err)
	}
	if !locked {
		t.Errorf("TryLock(task-1, agent-2) = false, want true; the failed dispatch kept its lock")
	}
}

// failingRunner fails every task, driving the coordinator onto failTask.
type failingRunner struct{ err error }

func (r failingRunner) RunTask(context.Context, domain.Agent, domain.Task) (domain.TaskRun, error) {
	return domain.TaskRun{}, r.err
}

// TestCoordinatorLogsStuckRunningTaskAtErrorLevel pins the severity of a task
// whose terminal transition could not be recorded. Unlike the abandoned-claim
// case, the run already happened and its side effects are real, so the task
// cannot be re-queued -- it stays Running in memory and in storage while the
// truth is that it failed. That is an unrecoverable divergence for that task,
// which is Error, not Warn, and the log has to say what was left behind or the
// operator sees a task that is simply "still running".
func TestCoordinatorLogsStuckRunningTaskAtErrorLevel(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	scheduler := task.NewSchedulerWithSink(flakyStatusSink{failOn: domain.TaskFailed})
	if err := scheduler.Add(context.Background(), domain.Task{
		ID:        "task-1",
		CompanyID: "company-1",
		Status:    domain.TaskPending,
		Input:     "ship it",
	}); err != nil {
		t.Fatalf("Add() error = %v, want nil", err)
	}
	audit := adapter.NewMemoryAuditLog()
	coordinator := NewCoordinator(CoordinatorConfig{
		Agent:      domain.Agent{ID: "agent-1", CompanyID: "company-1", Role: "developer", Status: domain.AgentActive},
		Scheduler:  scheduler,
		Locks:      task.NewLockStore(),
		Runtime:    failingRunner{err: errors.New("runner exploded")},
		Reviewer:   quality.NewAegisReviewer(),
		Evaluator:  quality.NewEvalEngine(3),
		Approvals:  approval.NewService(),
		Audit:      audit,
		Events:     adapter.NewMemoryEventBus(),
		LockTTL:    time.Minute,
		MaxWorkers: 1,
		Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	})

	if _, _, err := coordinator.Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat() error = %v, want nil", err)
	}
	coordinator.Wait()

	got := logs.String()
	for _, want := range []string{"level=ERROR", "mark task failed", "task-1", "left_status=running"} {
		if !strings.Contains(got, want) {
			t.Errorf("logger output = %q, want it to contain %q", got, want)
		}
	}
}
