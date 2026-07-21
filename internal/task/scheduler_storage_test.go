package task_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/storage"
	"github.com/stardust/legion-agent/internal/task"
)

// TestSchedulerWriteThroughKeepsStoredTaskIntact runs the scheduler against the
// real repository along the restart-recovery shape: serve rebuilds a suspended
// task from its checkpoint, which knows the task's id, agent, session, mode and
// working dir -- but not the company that owns it, the user's input, or when it
// was created. Registering and then advancing such a task must not cost the
// stored row those fields.
//
// company_id in particular is what the HTTP layer's tenant check reads, so a
// blanked value is not merely lost history: it corrupts an authorization input.
func TestSchedulerWriteThroughKeepsStoredTaskIntact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})

	created := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	original := domain.Task{
		ID:            "task-1",
		CompanyID:     "company-1",
		AgentID:       "researcher",
		SessionID:     "session-1",
		Status:        domain.TaskSuspended,
		Input:         "the user's original prompt",
		MaxIterations: 7,
		CreatedAt:     created,
	}
	if err := repo.SaveTask(ctx, original); err != nil {
		t.Fatalf("SaveTask(task-1) error = %v, want nil", err)
	}

	scheduler := task.NewSchedulerWithSink(repo)
	recovered := domain.Task{
		ID:        "task-1",
		AgentID:   "researcher",
		SessionID: "session-1",
		Status:    domain.TaskSuspended,
	}
	if err := scheduler.Add(ctx, recovered); err != nil {
		t.Fatalf("Add(recovered task-1) error = %v, want nil", err)
	}
	if err := scheduler.Transition(ctx, "task-1", domain.TaskRunning); err != nil {
		t.Fatalf("Transition(task-1, %q) error = %v, want nil", domain.TaskRunning, err)
	}

	got, ok, err := repo.GetTask(ctx, "task-1")
	if err != nil || !ok {
		t.Fatalf("GetTask(task-1) = (_, %t, %v), want (_, true, nil)", ok, err)
	}
	if got.Status != domain.TaskRunning {
		t.Errorf("status = %q, want %q", got.Status, domain.TaskRunning)
	}
	if got.CompanyID != original.CompanyID {
		t.Errorf("company = %q, want %q; a blanked company_id corrupts the tenant check", got.CompanyID, original.CompanyID)
	}
	if got.Input != original.Input {
		t.Errorf("input = %q, want %q", got.Input, original.Input)
	}
	if got.MaxIterations != original.MaxIterations {
		t.Errorf("max_iterations = %d, want %d", got.MaxIterations, original.MaxIterations)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("created_at = %v, want %v", got.CreatedAt, created)
	}
}
