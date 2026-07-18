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
	sink  ApprovalEventSink
}

// CoordinatorOption configures an ApprovalCoordinator.
type CoordinatorOption func(*ApprovalCoordinator)

// WithCoordinatorSink attaches an optional ApprovalEventSink so Decide (and the
// timeout sweep that routes through it) emits approval_resolved notifications.
func WithCoordinatorSink(sink ApprovalEventSink) CoordinatorOption {
	return func(a *ApprovalCoordinator) { a.sink = sink }
}

// NewApprovalCoordinator returns an ApprovalCoordinator recording decisions to
// store and resuming tasks through sched, configured by opts.
func NewApprovalCoordinator(store *approval.ToolGateStore, sched SchedulerGate, opts ...CoordinatorOption) *ApprovalCoordinator {
	a := &ApprovalCoordinator{store: store, sched: sched}
	for _, o := range opts {
		o(a)
	}
	return a
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
	rec, err := a.store.Decide(sessionKey, ticketID, status, t.WorkingDir)
	if err != nil {
		return approval.ToolApproval{}, fmt.Errorf("record decision for ticket %s: %w", ticketID, err)
	}
	if a.sink != nil {
		a.sink.ApprovalResolved(ctx, taskID, ticketID, string(status))
	}
	remaining, err := a.store.ListForTask(sessionKey, taskID, t.WorkingDir)
	if err != nil {
		return approval.ToolApproval{}, fmt.Errorf("list tickets for task %s: %w", taskID, err)
	}
	if ticketsAllDecided(remaining) && t.Status == domain.TaskSuspended {
		if err := a.sched.Transition(ctx, taskID, domain.TaskRunning); err != nil {
			// Two concurrent Decide calls on the final tickets of the same task
			// can both observe Suspended+allDecided (the Status read above and
			// this Transition are not atomic together) and both attempt the
			// Suspended->Running flip; the loser gets ErrInvalidTransition for a
			// decision that WAS validly recorded above. Re-check the task's
			// current state: if someone else already flipped it to Running,
			// this decision still succeeded and there is nothing more to do —
			// only an unexpected state propagates the error.
			now, ok, getErr := a.sched.Get(ctx, taskID)
			if getErr != nil {
				return approval.ToolApproval{}, fmt.Errorf("resume task %s after decision: %w", taskID, err)
			}
			if !ok || now.Status != domain.TaskRunning {
				return approval.ToolApproval{}, fmt.Errorf("resume task %s after decision: %w", taskID, err)
			}
			// Concurrent flip already landed the task in Running; treat this
			// decision as successfully recorded, not an error.
		}
	}
	return rec, nil
}

// ticketsAllDecided reports whether none of tickets is still ApprovalPending.
// An empty slice is vacuously "all decided" by this predicate alone — callers
// that must distinguish "no tickets at all" from "every ticket resolved"
// (ReconcileResume) check len(tickets) themselves before calling this.
func ticketsAllDecided(tickets []approval.ToolApproval) bool {
	for _, t := range tickets {
		if t.Status == approval.ApprovalPending {
			return false
		}
	}
	return true
}

// ReconcileResume resumes a Suspended task at process startup if every
// approval ticket recorded for it was already decided before the restart
// (e.g. a human approved the last ticket but the process died before the
// resume dispatch ran). It is the startup counterpart to Decide's
// Suspended->Running flip.
//
// A task that is not currently Suspended is left untouched (nothing to
// reconcile). A Suspended task with NO recorded tickets is also left
// untouched — it is suspended for a reason other than a tool-approval
// decision (e.g. a plain checkpoint), and ReconcileResume must not guess at
// resuming it. A Suspended task with any ticket still ApprovalPending is left
// untouched — it is still waiting on a human. Only when every ticket has
// moved past ApprovalPending does ReconcileResume flip the task to Running so
// the coordinator's resume scan picks it up.
func (a *ApprovalCoordinator) ReconcileResume(ctx context.Context, taskID string) error {
	t, ok, err := a.sched.Get(ctx, taskID)
	if err != nil {
		return fmt.Errorf("lookup task %s for approval reconcile: %w", taskID, err)
	}
	if !ok {
		return fmt.Errorf("reconcile approval resume: task %s not found", taskID)
	}
	if t.Status != domain.TaskSuspended {
		return nil
	}
	sessionKey := sessionKeyForTask(t)
	tickets, err := a.store.ListForTask(sessionKey, taskID, t.WorkingDir)
	if err != nil {
		return fmt.Errorf("list tickets for task %s approval reconcile: %w", taskID, err)
	}
	if len(tickets) == 0 {
		// Suspended for a reason other than a tool-approval ticket; not ours to
		// resume.
		return nil
	}
	if !ticketsAllDecided(tickets) {
		return nil
	}
	if err := a.sched.Transition(ctx, taskID, domain.TaskRunning); err != nil {
		return fmt.Errorf("resume task %s on approval reconcile: %w", taskID, err)
	}
	return nil
}
