package task

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/stardust/legion-agent/internal/domain"
)

var (
	ErrTaskNotFound       = errors.New("task not found")
	ErrInvalidTransition  = errors.New("invalid task transition")
	ErrTaskAlreadyPresent = errors.New("task already present")
)

// TaskSink is the durable store a Scheduler writes each task state change
// through to. The storage repository satisfies it.
type TaskSink interface {
	SaveTask(ctx context.Context, task domain.Task) error
}

type Scheduler struct {
	mu    sync.Mutex
	order []string
	tasks map[string]domain.Task
	sink  TaskSink
}

func NewScheduler() *Scheduler {
	return &Scheduler{tasks: make(map[string]domain.Task)}
}

// NewSchedulerWithSink returns a Scheduler that writes every task state change
// through to sink before committing it in memory. A nil sink is the contract's
// explicit opt-out: the scheduler stays purely in-memory, as NewScheduler.
func NewSchedulerWithSink(sink TaskSink) *Scheduler {
	return &Scheduler{tasks: make(map[string]domain.Task), sink: sink}
}

func (s *Scheduler) Add(ctx context.Context, task domain.Task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[task.ID]; ok {
		return fmt.Errorf("%w: %s", ErrTaskAlreadyPresent, task.ID)
	}
	if err := s.persist(ctx, task); err != nil {
		return err
	}
	s.tasks[task.ID] = task
	s.order = append(s.order, task.ID)
	return nil
}

// persist writes task through to the durable sink, if one is configured. It is
// always called before the in-memory map is updated: a durable write that fails
// must leave the scheduler where it was rather than let memory and storage
// disagree about a task's state. Callers hold s.mu, which also serializes the
// sink writes.
func (s *Scheduler) persist(ctx context.Context, task domain.Task) error {
	if s.sink == nil {
		return nil
	}
	if err := s.sink.SaveTask(ctx, task); err != nil {
		return fmt.Errorf("persist task %q as %s: %w", task.ID, task.Status, err)
	}
	return nil
}

func (s *Scheduler) Next(ctx context.Context, agentID string) (domain.Task, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Task{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		task := s.tasks[id]
		if task.Status != domain.TaskPending {
			continue
		}
		if task.AgentID == "" {
			task.AgentID = agentID
		}
		task.Status = domain.TaskAssigned
		if err := s.persist(ctx, task); err != nil {
			return domain.Task{}, false, err
		}
		s.tasks[id] = task
		return task, true, nil
	}
	return domain.Task{}, false, nil
}

// List returns a snapshot of every task currently held by the scheduler, in the
// order they were added. The slice is freshly allocated under the lock so the
// caller cannot observe later mutations; an empty scheduler yields an empty
// (non-nil) slice so JSON callers serialize it as [] rather than null.
func (s *Scheduler) List(ctx context.Context) ([]domain.Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := make([]domain.Task, 0, len(s.order))
	for _, id := range s.order {
		tasks = append(tasks, s.tasks[id])
	}
	return tasks, nil
}

func (s *Scheduler) Get(ctx context.Context, taskID string) (domain.Task, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Task{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	return task, ok, nil
}

func (s *Scheduler) Transition(ctx context.Context, taskID string, next domain.TaskStatus) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
	}
	if !canTransition(task.Status, next) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, task.Status, next)
	}
	task.Status = next
	if err := s.persist(ctx, task); err != nil {
		return err
	}
	s.tasks[taskID] = task
	return nil
}

func canTransition(current domain.TaskStatus, next domain.TaskStatus) bool {
	switch current {
	case domain.TaskPending:
		return next == domain.TaskAssigned
	case domain.TaskAssigned:
		return next == domain.TaskRunning
	case domain.TaskRunning:
		return next == domain.TaskQualityReview ||
			next == domain.TaskSuspended ||
			next == domain.TaskFailed
	case domain.TaskSuspended:
		return next == domain.TaskRunning
	case domain.TaskQualityReview:
		return next == domain.TaskDone || next == domain.TaskFailed
	default:
		return false
	}
}
