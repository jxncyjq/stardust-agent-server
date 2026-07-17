package gateway

import (
	"sync"
	"time"
)

// trackedTask is the per-task delivery state held by DeliveryTracker: the
// outbound target plus retry bookkeeping (attempts made so far and the
// earliest time the next delivery attempt may run).
type trackedTask struct {
	target   DeliveryTarget
	attempts int
	nextAt   time.Time // earliest time to (re)attempt delivery; zero = ready now
}

// DeliveryTracker maps an in-flight task id to the outbound target that should
// receive its result, plus retry state for bounded delivery retry. It is
// process-local and short-lived: an entry is created when a task is submitted
// and removed when its result is delivered (or delivery permanently fails). A
// restart loses in-flight entries (documented tradeoff).
type DeliveryTracker struct {
	mu    sync.Mutex
	tasks map[string]trackedTask
}

// NewDeliveryTracker returns an empty tracker.
func NewDeliveryTracker() *DeliveryTracker {
	return &DeliveryTracker{tasks: make(map[string]trackedTask)}
}

// Track records the delivery target for a submitted task id, with a fresh
// retry state (zero attempts, ready for immediate delivery).
func (t *DeliveryTracker) Track(taskID string, target DeliveryTarget) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tasks[taskID] = trackedTask{target: target}
}

// Take returns and removes the target for a task id. ok is false when the id is
// not tracked, which is a legitimate case (the completed task was not submitted
// by this gateway) the caller skips rather than treats as an error.
func (t *DeliveryTracker) Take(taskID string) (DeliveryTarget, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	task, ok := t.tasks[taskID]
	if ok {
		delete(t.tasks, taskID)
	}
	return task.target, ok
}

// Pending returns a snapshot of the currently in-flight task ids, so the
// completion poller can iterate them without holding the lock during I/O.
func (t *DeliveryTracker) Pending() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]string, 0, len(t.tasks))
	for id := range t.tasks {
		ids = append(ids, id)
	}
	return ids
}

// Get returns the tracked target and retry state for a task id without
// removing it. ok is false when the id is not tracked.
func (t *DeliveryTracker) Get(taskID string) (target DeliveryTarget, attempts int, nextAt time.Time, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	task, ok := t.tasks[taskID]
	if !ok {
		return DeliveryTarget{}, 0, time.Time{}, false
	}
	return task.target, task.attempts, task.nextAt, true
}

// MarkAttempt records a failed delivery attempt for a task id: increments its
// attempt count and sets the earliest time the next attempt may run. It is a
// no-op if the task id is not tracked (e.g. taken by a concurrent pass).
func (t *DeliveryTracker) MarkAttempt(taskID string, nextAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	task, ok := t.tasks[taskID]
	if !ok {
		return
	}
	task.attempts++
	task.nextAt = nextAt
	t.tasks[taskID] = task
}
