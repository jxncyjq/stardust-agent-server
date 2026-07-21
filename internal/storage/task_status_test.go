package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/storage"
)

// TestUpdateTaskStatusPreservesTheRestOfTheRow pins the write surface of a task
// state change: only status and agent_id may move. A full-row upsert here would
// let a caller holding a partially populated domain.Task (the scheduler's
// restart-recovery path builds one from a checkpoint) blank out the user's
// original input and the company_id that tenant checks read.
func TestUpdateTaskStatusPreservesTheRestOfTheRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestRepo(t)
	created := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	original := domain.Task{
		ID:            "task-1",
		CompanyID:     "company-1",
		AgentID:       "",
		SessionID:     "session-1",
		Status:        domain.TaskPending,
		Input:         "the user's original prompt",
		MaxIterations: 7,
		CreatedAt:     created,
	}
	if err := repo.SaveTask(ctx, original); err != nil {
		t.Fatalf("SaveTask(task-1) error = %v, want nil", err)
	}

	if err := repo.UpdateTaskStatus(ctx, "task-1", domain.TaskAssigned, "agent-1"); err != nil {
		t.Fatalf("UpdateTaskStatus(task-1) error = %v, want nil", err)
	}

	got, ok, err := repo.GetTask(ctx, "task-1")
	if err != nil || !ok {
		t.Fatalf("GetTask(task-1) = (_, %t, %v), want (_, true, nil)", ok, err)
	}
	if got.Status != domain.TaskAssigned {
		t.Errorf("status = %q, want %q", got.Status, domain.TaskAssigned)
	}
	if got.AgentID != "agent-1" {
		t.Errorf("agent = %q, want %q", got.AgentID, "agent-1")
	}
	if got.Input != original.Input {
		t.Errorf("input = %q, want %q", got.Input, original.Input)
	}
	if got.CompanyID != original.CompanyID {
		t.Errorf("company = %q, want %q", got.CompanyID, original.CompanyID)
	}
	if got.SessionID != original.SessionID {
		t.Errorf("session = %q, want %q", got.SessionID, original.SessionID)
	}
	if got.MaxIterations != original.MaxIterations {
		t.Errorf("max_iterations = %d, want %d", got.MaxIterations, original.MaxIterations)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("created_at = %v, want %v", got.CreatedAt, created)
	}
}

// TestUpdateTaskStatusOnUnpersistedTaskIsNotAnError states the contract for a
// task the table does not hold: the tasks table records only tasks that entered
// through a creation path, so workflow-internal tasks legitimately have no row
// to update. Absence here is contract-declared optional, not a swallowed error.
func TestUpdateTaskStatusOnUnpersistedTaskIsNotAnError(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)

	if err := repo.UpdateTaskStatus(context.Background(), "never-persisted", domain.TaskRunning, "agent-1"); err != nil {
		t.Fatalf("UpdateTaskStatus(never-persisted) error = %v, want nil", err)
	}
}

func openTestRepo(t *testing.T) *storage.SQLiteRepository {
	t.Helper()
	repo, err := storage.OpenSQLite(context.Background(), t.TempDir()+"/agent.db")
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	return repo
}
