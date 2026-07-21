package task

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// recordingSink captures every state change handed to it, standing in for the
// SQLite repository without dragging a database into a unit test.
type recordingSink struct {
	mu    sync.Mutex
	saved []statusWrite
	err   error
}

type statusWrite struct {
	taskID  string
	status  domain.TaskStatus
	agentID string
}

func (s *recordingSink) UpdateTaskStatus(ctx context.Context, taskID string, status domain.TaskStatus, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.saved = append(s.saved, statusWrite{taskID: taskID, status: status, agentID: agentID})
	return nil
}

func (s *recordingSink) last(t *testing.T) statusWrite {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.saved) == 0 {
		t.Fatalf("sink recorded no writes, want at least one")
	}
	return s.saved[len(s.saved)-1]
}

// TestSchedulerAddDoesNotWriteThrough pins the boundary that keeps a partially
// populated task from reaching storage: registering a task is not a state
// change, so Add must not touch the sink. RecoverSuspended registers tasks
// rebuilt from checkpoints, which carry no company or input.
func TestSchedulerAddDoesNotWriteThrough(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	scheduler := NewSchedulerWithSink(sink)
	added := domain.Task{ID: "task-1", CompanyID: "company-1", Status: domain.TaskPending}
	if err := scheduler.Add(context.Background(), added); err != nil {
		t.Fatalf("Add(%q) error = %v, want nil", added.ID, err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.saved) != 0 {
		t.Errorf("Add wrote %d state change(s) through to storage, want 0: %+v", len(sink.saved), sink.saved)
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
	if got.status != domain.TaskAssigned {
		t.Errorf("sink last write status = %q, want %q", got.status, domain.TaskAssigned)
	}
	if got.agentID != "agent-1" {
		t.Errorf("sink last write agent = %q, want %q", got.agentID, "agent-1")
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

	if got := sink.last(t); got.status != domain.TaskRunning {
		t.Errorf("sink last write status = %q, want %q", got.status, domain.TaskRunning)
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

func TestSchedulerNextFailsLoudWhenPersistFails(t *testing.T) {
	t.Parallel()

	persistErr := errors.New("disk on fire")
	sink := &recordingSink{}
	scheduler := NewSchedulerWithSink(sink)
	ctx := context.Background()
	if err := scheduler.Add(ctx, domain.Task{ID: "task-1", CompanyID: "company-1", Status: domain.TaskPending}); err != nil {
		t.Fatalf("Add(task-1) error = %v, want nil", err)
	}
	sink.err = persistErr

	assigned, ok, err := scheduler.Next(ctx, "agent-1")
	if !errors.Is(err, persistErr) {
		t.Fatalf("Next(agent-1) error = %v, want it to wrap %v", err, persistErr)
	}
	if ok {
		t.Errorf("Next(agent-1) ok = true, want false: a task whose assignment was not recorded must not be handed out")
	}
	if assigned.ID != "" {
		t.Errorf("Next(agent-1) task = %+v, want the zero task", assigned)
	}

	// The task must still be claimable: leaving it Assigned in memory while
	// storage never heard about the assignment would strand it, since Next only
	// ever scans Pending.
	stored, found, getErr := scheduler.Get(ctx, "task-1")
	if getErr != nil || !found {
		t.Fatalf("Get(task-1) = (_, %v, %v), want (_, true, nil)", found, getErr)
	}
	if stored.Status != domain.TaskPending {
		t.Errorf("after failed persist, in-memory status = %q, want %q", stored.Status, domain.TaskPending)
	}
}

func TestSchedulerWithoutSinkStillChangesState(t *testing.T) {
	t.Parallel()

	scheduler := NewScheduler()
	ctx := context.Background()
	if err := scheduler.Add(ctx, domain.Task{ID: "task-1", CompanyID: "company-1", Status: domain.TaskAssigned}); err != nil {
		t.Fatalf("Add(task-1) error = %v, want nil", err)
	}
	if err := scheduler.Transition(ctx, "task-1", domain.TaskRunning); err != nil {
		t.Fatalf("Transition(task-1, %q) error = %v, want nil", domain.TaskRunning, err)
	}

	stored, ok, err := scheduler.Get(ctx, "task-1")
	if err != nil || !ok {
		t.Fatalf("Get(task-1) = (_, %v, %v), want (_, true, nil)", ok, err)
	}
	if stored.Status != domain.TaskRunning {
		t.Errorf("status = %q, want %q", stored.Status, domain.TaskRunning)
	}
}

func TestSchedulerReleaseReturnsAssignedTaskToPending(t *testing.T) {
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

	if err := scheduler.Release(ctx, "task-1"); err != nil {
		t.Fatalf("Release(task-1) error = %v, want nil", err)
	}

	if got := sink.last(t); got.status != domain.TaskPending {
		t.Errorf("sink last write status = %q, want %q", got.status, domain.TaskPending)
	}
	// Claimable again: Next only ever scans Pending, so a released task that
	// stayed Assigned would never run.
	reclaimed, ok, err := scheduler.Next(ctx, "agent-2")
	if err != nil || !ok {
		t.Fatalf("Next(agent-2) after Release = (_, %v, %v), want (_, true, nil)", ok, err)
	}
	if reclaimed.ID != "task-1" {
		t.Errorf("reclaimed task = %q, want %q", reclaimed.ID, "task-1")
	}
}

func TestSchedulerReleaseRejectsTaskThatIsNotAssigned(t *testing.T) {
	t.Parallel()

	scheduler := NewScheduler()
	ctx := context.Background()
	if err := scheduler.Add(ctx, domain.Task{ID: "task-1", CompanyID: "company-1", Status: domain.TaskAssigned}); err != nil {
		t.Fatalf("Add(task-1) error = %v, want nil", err)
	}
	if err := scheduler.Transition(ctx, "task-1", domain.TaskRunning); err != nil {
		t.Fatalf("Transition(task-1, running) error = %v, want nil", err)
	}

	// Releasing a task that already started running would re-run work whose
	// side effects already happened; only an unstarted claim may be undone.
	err := scheduler.Release(ctx, "task-1")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Release(running task-1) error = %v, want ErrInvalidTransition", err)
	}
}

func TestSchedulerReleaseRejectsUnknownTask(t *testing.T) {
	t.Parallel()

	err := NewScheduler().Release(context.Background(), "nope")
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Release(nope) error = %v, want ErrTaskNotFound", err)
	}
}
