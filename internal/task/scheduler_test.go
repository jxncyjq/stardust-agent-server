package task

import (
	"context"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestSchedulerNextAssignsPendingTask(t *testing.T) {
	t.Parallel()

	scheduler := NewScheduler()
	task := domain.Task{
		ID:        "task-1",
		CompanyID: "company-1",
		Status:    domain.TaskPending,
		Input:     "build the thing",
	}
	if err := scheduler.Add(context.Background(), task); err != nil {
		t.Fatalf("Add(%q) error = %v, want nil", task.ID, err)
	}

	assigned, ok, err := scheduler.Next(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("Next(%q) error = %v, want nil", "agent-1", err)
	}
	if !ok {
		t.Fatalf("Next(%q) ok = false, want true", "agent-1")
	}
	if assigned.Status != domain.TaskAssigned {
		t.Errorf("Next(%q) status = %q, want %q", "agent-1", assigned.Status, domain.TaskAssigned)
	}
	if assigned.AgentID != "agent-1" {
		t.Errorf("Next(%q) agent = %q, want %q", "agent-1", assigned.AgentID, "agent-1")
	}
}

func TestSchedulerNextPreservesExplicitTaskAgentID(t *testing.T) {
	t.Parallel()

	scheduler := NewScheduler()
	task := domain.Task{
		ID:        "task-1",
		CompanyID: "company-1",
		AgentID:   "agent-explicit",
		Status:    domain.TaskPending,
		Input:     "build the thing",
	}
	if err := scheduler.Add(context.Background(), task); err != nil {
		t.Fatalf("Add(%q) error = %v, want nil", task.ID, err)
	}

	assigned, ok, err := scheduler.Next(context.Background(), "agent-fallback")
	if err != nil {
		t.Fatalf("Next(%q) error = %v, want nil", "agent-fallback", err)
	}
	if !ok {
		t.Fatalf("Next(%q) ok = false, want true", "agent-fallback")
	}
	if assigned.AgentID != "agent-explicit" {
		t.Errorf("Next(%q) agent = %q, want %q", "agent-fallback", assigned.AgentID, "agent-explicit")
	}
	if assigned.Status != domain.TaskAssigned {
		t.Errorf("Next(%q) status = %q, want %q", "agent-fallback", assigned.Status, domain.TaskAssigned)
	}
}

func TestSchedulerTransitionRejectsInvalidJump(t *testing.T) {
	t.Parallel()

	scheduler := NewScheduler()
	task := domain.Task{
		ID:        "task-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Status:    domain.TaskAssigned,
	}
	if err := scheduler.Add(context.Background(), task); err != nil {
		t.Fatalf("Add(%q) error = %v, want nil", task.ID, err)
	}

	err := scheduler.Transition(context.Background(), "task-1", domain.TaskDone)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Transition(%q, %q) error = %v, want ErrInvalidTransition", "task-1", domain.TaskDone, err)
	}

	if err := scheduler.Transition(context.Background(), "task-1", domain.TaskRunning); err != nil {
		t.Fatalf("Transition(%q, %q) error = %v, want nil", "task-1", domain.TaskRunning, err)
	}
	if err := scheduler.Transition(context.Background(), "task-1", domain.TaskQualityReview); err != nil {
		t.Fatalf("Transition(%q, %q) error = %v, want nil", "task-1", domain.TaskQualityReview, err)
	}
	if err := scheduler.Transition(context.Background(), "task-1", domain.TaskDone); err != nil {
		t.Fatalf("Transition(%q, %q) error = %v, want nil", "task-1", domain.TaskDone, err)
	}
}

func TestSchedulerListReturnsAddedTasksInOrder(t *testing.T) {
	t.Parallel()

	scheduler := NewScheduler()
	ctx := context.Background()
	for _, id := range []string{"task-a", "task-b", "task-c"} {
		if err := scheduler.Add(ctx, domain.Task{ID: id, CompanyID: "company-1", Status: domain.TaskPending}); err != nil {
			t.Fatalf("Add(%q) error = %v, want nil", id, err)
		}
	}

	tasks, err := scheduler.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("List() len = %d, want 3", len(tasks))
	}
	wantOrder := []string{"task-a", "task-b", "task-c"}
	for i, want := range wantOrder {
		if tasks[i].ID != want {
			t.Errorf("List()[%d].ID = %q, want %q", i, tasks[i].ID, want)
		}
	}
}

func TestSchedulerListEmptyReturnsEmptySlice(t *testing.T) {
	t.Parallel()

	scheduler := NewScheduler()
	tasks, err := scheduler.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if tasks == nil {
		t.Fatalf("List() = nil, want non-nil empty slice")
	}
	if len(tasks) != 0 {
		t.Fatalf("List() len = %d, want 0", len(tasks))
	}
}
