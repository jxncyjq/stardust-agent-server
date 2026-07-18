package runtime

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/task"
)

// suspendingRunner always returns ErrSuspended, modelling a runtime that
// checkpointed and paused.
type suspendingRunner struct{}

func (suspendingRunner) RunTask(context.Context, domain.Agent, domain.Task) (domain.TaskRun, error) {
	return domain.TaskRun{}, ErrSuspended
}

func TestCoordinatorSuspendedRunLandsSuspendedNotFailed(t *testing.T) {
	sched := task.NewScheduler()
	ctx := context.Background()
	if err := sched.Add(ctx, domain.Task{ID: "t-suspend", AgentID: "default-agent", Status: domain.TaskPending, Input: "x"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	coord := newTestCoordinatorWithRunner(t, sched, suspendingRunner{})

	if _, _, err := coord.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	coord.Wait()

	got, ok, err := sched.Get(ctx, "t-suspend")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("task missing")
	}
	if got.Status != domain.TaskSuspended {
		t.Errorf("status = %s, want %s", got.Status, domain.TaskSuspended)
	}
}
