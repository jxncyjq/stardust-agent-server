package manualgate

import (
	"context"
	"fmt"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
)

// SchedulerGate is the narrow slice of task.Scheduler that ApprovalCoordinator
// needs: look up a task's current state and transition it. *task.Scheduler
// satisfies it structurally; kept as an interface so decider.go does not
// import internal/task and pull in the whole scheduling package.
type SchedulerGate interface {
	Get(ctx context.Context, taskID string) (domain.Task, bool, error)
	Transition(ctx context.Context, taskID string, next domain.TaskStatus) error
}

// ApprovalCoordinator applies a human's approve/deny decision on a Manual-mode
// tool-approval ticket and, once every ticket for the owning task has been
// decided, resumes the task by flipping it Suspended→Running. It is the
// dispatch-side half of the resume design (plan B): Decide only records the
// decision and flips the scheduler state — the coordinator's Heartbeat resume
// scan is what actually re-runs the task from its checkpoint.
type ApprovalCoordinator struct {
	store *approval.ToolGateStore
	sched SchedulerGate
}

// NewApprovalCoordinator returns an ApprovalCoordinator recording decisions to
// store and resuming tasks through sched.
func NewApprovalCoordinator(store *approval.ToolGateStore, sched SchedulerGate) *ApprovalCoordinator {
	return &ApprovalCoordinator{store: store, sched: sched}
}

// Decide records the decision on disk and, when every ticket for the task is
// decided, transitions the task Suspended→Running so the coordinator's resume
// scan picks it up. Returns the decided ToolApproval.
func (a *ApprovalCoordinator) Decide(ctx context.Context, taskID, ticketID string, status approval.ApprovalStatus) (approval.ToolApproval, error) {
	t, ok, err := a.sched.Get(ctx, taskID)
	if err != nil {
		return approval.ToolApproval{}, fmt.Errorf("lookup task %s for decision: %w", taskID, err)
	}
	if !ok {
		return approval.ToolApproval{}, fmt.Errorf("decide approval: task %s not found", taskID)
	}
	sessionKey := sessionKeyForTask(t)
	rec, err := a.store.Decide(sessionKey, ticketID, status)
	if err != nil {
		return approval.ToolApproval{}, fmt.Errorf("record decision for ticket %s: %w", ticketID, err)
	}
	remaining, err := a.store.ListForTask(sessionKey, taskID)
	if err != nil {
		return approval.ToolApproval{}, fmt.Errorf("list tickets for task %s: %w", taskID, err)
	}
	allDecided := true
	for _, r := range remaining {
		if r.Status == approval.ApprovalPending {
			allDecided = false
			break
		}
	}
	if allDecided && t.Status == domain.TaskSuspended {
		if err := a.sched.Transition(ctx, taskID, domain.TaskRunning); err != nil {
			return approval.ToolApproval{}, fmt.Errorf("resume task %s after decision: %w", taskID, err)
		}
	}
	return rec, nil
}
