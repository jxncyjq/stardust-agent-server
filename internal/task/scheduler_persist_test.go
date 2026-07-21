package task

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// recordingSink captures every task handed to it, standing in for the SQLite
// repository without dragging a database into a unit test.
type recordingSink struct {
	mu    sync.Mutex
	saved []domain.Task
	err   error
}

func (s *recordingSink) SaveTask(ctx context.Context, task domain.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.saved = append(s.saved, task)
	return nil
}

func (s *recordingSink) last(t *testing.T) domain.Task {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.saved) == 0 {
		t.Fatalf("sink recorded no saves, want at least one")
	}
	return s.saved[len(s.saved)-1]
}

func TestSchedulerAddPersistsTask(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	scheduler := NewSchedulerWithSink(sink)
	added := domain.Task{ID: "task-1", CompanyID: "company-1", Status: domain.TaskPending}
	if err := scheduler.Add(context.Background(), added); err != nil {
		t.Fatalf("Add(%q) error = %v, want nil", added.ID, err)
	}

	if got := sink.last(t); got.ID != "task-1" || got.Status != domain.TaskPending {
		t.Errorf("sink last save = {ID:%q Status:%q}, want {ID:%q Status:%q}", got.ID, got.Status, "task-1", domain.TaskPending)
	}
}

func TestSchedulerNextPersistsAssignment(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	scheduler := NewSchedulerWithSink(sink)
	ctx := context.Background()
	if err := scheduler.Add(ctx, domain.Task{ID: "task-1", CompanyID: "company-1", Status: domain.TaskPending}); err != nil {
		t.Fatalf("Add(task-1) error = %v, want nil", err)
	}

	if _, ok, err := scheduler.Next(ctx, "agent-1"); err != nil || !ok {
		t.Fatalf("Next(agent-1) = (_, %v, %v), want (_, true, nil)", ok, err)
	}

	got := sink.last(t)
	if got.Status != domain.TaskAssigned {
		t.Errorf("sink last save status = %q, want %q", got.Status, domain.TaskAssigned)
	}
	if got.AgentID != "agent-1" {
		t.Errorf("sink last save agent = %q, want %q", got.AgentID, "agent-1")
	}
}

func TestSchedulerTransitionPersistsStatus(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	scheduler := NewSchedulerWithSink(sink)
	ctx := context.Background()
	if err := scheduler.Add(ctx, domain.Task{ID: "task-1", CompanyID: "company-1", Status: domain.TaskAssigned}); err != nil {
		t.Fatalf("Add(task-1) error = %v, want nil", err)
	}

	if err := scheduler.Transition(ctx, "task-1", domain.TaskRunning); err != nil {
		t.Fatalf("Transition(task-1, %q) error = %v, want nil", domain.TaskRunning, err)
	}

	if got := sink.last(t); got.Status != domain.TaskRunning {
		t.Errorf("sink last save status = %q, want %q", got.Status, domain.TaskRunning)
	}
}

func TestSchedulerTransitionFailsLoudWhenPersistFails(t *testing.T) {
	t.Parallel()

	persistErr := errors.New("disk on fire")
	sink := &recordingSink{}
	scheduler := NewSchedulerWithSink(sink)
	ctx := context.Background()
	if err := scheduler.Add(ctx, domain.Task{ID: "task-1", CompanyID: "company-1", Status: domain.TaskAssigned}); err != nil {
		t.Fatalf("Add(task-1) error = %v, want nil", err)
	}
	sink.err = persistErr

	err := scheduler.Transition(ctx, "task-1", domain.TaskRunning)
	if !errors.Is(err, persistErr) {
		t.Fatalf("Transition(task-1, %q) error = %v, want it to wrap %v", domain.TaskRunning, err, persistErr)
	}

	// The in-memory status must not advance when the durable write failed:
	// diverging here is exactly the silent state corruption the persistence is
	// meant to prevent.
	stored, ok, getErr := scheduler.Get(ctx, "task-1")
	if getErr != nil || !ok {
		t.Fatalf("Get(task-1) = (_, %v, %v), want (_, true, nil)", ok, getErr)
	}
	if stored.Status != domain.TaskAssigned {
		t.Errorf("after failed persist, in-memory status = %q, want %q", stored.Status, domain.TaskAssigned)
	}
}

func TestSchedulerWithoutSinkStaysInMemory(t *testing.T) {
	t.Parallel()

	scheduler := NewScheduler()
	ctx := context.Background()
	if err := scheduler.Add(ctx, domain.Task{ID: "task-1", CompanyID: "company-1", Status: domain.TaskAssigned}); err != nil {
		t.Fatalf("Add(task-1) error = %v, want nil", err)
	}
	if err := scheduler.Transition(ctx, "task-1", domain.TaskRunning); err != nil {
		t.Fatalf("Transition(task-1, %q) error = %v, want nil", domain.TaskRunning, err)
	}
}
