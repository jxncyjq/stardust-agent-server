package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrApplyPending is what Begin reports while a plugin change is waiting for a
// task boundary. It is a sentinel so a caller can tell "the gate is closed for
// a moment, try again" from a real failure to start the task with errors.Is —
// the two deserve different responses, and only the caller knows which.
var ErrApplyPending = errors.New("a plugin change is waiting for a task boundary")

// errApplyInProgress is what a second, overlapping ApplyAtBoundary reports.
// The convergence belongs to the first caller: a second one that returned nil
// would claim an apply it never performed, and one that shared the first's
// pending flag would clear it while the first was still inside fn — reopening
// the gate mid-apply, which is exactly what this type exists to prevent.
var errApplyInProgress = errors.New("another plugin change is already being applied")

// TaskGate serializes plugin changes against task boundaries: a task that has
// started keeps the capability catalog it started with, and a new target state
// only affects tasks started afterwards.
//
// # Why this exists
//
// The capability catalog is built per task, but it is built over a live view of
// the process-wide tool registry, which resolves handlers at call time — the
// registry's derived views deliberately never copy handlers. So deregistering a
// tool halfway through a task is visible to that running task no matter how its
// catalog was built: the prompt keeps advertising a tool whose every call now
// fails. Building a deeper snapshot would mean copying handlers, which the
// registry forbids; serializing the change against task boundaries fixes the
// same problem without copying anything.
//
// It matters beyond a stray tool error. A mid-task change to the advertised
// capability set invalidates the model's prompt prefix, and a measured reload
// mid-catalog zeroed one provider's prompt-cache hits and cut another's from
// 1792 to 768 — a mid-task apply throws away a whole task's cache.
//
// # How it behaves
//
// Begin registers an in-flight task and returns the end func that retires it.
// BeginChild registers a continuation of a task that is already in flight (a
// delegated sub-task) and never refuses. ApplyAtBoundary marks a change
// pending, waits (bounded) for the in-flight count to reach zero, runs fn
// there, and clears the flag.
//
// Two choices are deliberate and should not be softened:
//
//   - While an apply is pending, Begin REFUSES with ErrApplyPending rather than
//     blocking. Blocking would turn a plugin reload into an invisible service
//     pause that callers only experience as tasks mysteriously hanging; an
//     error lets the caller decide between retrying and surfacing it.
//   - A wait that times out is a loud failure that changes NOTHING: fn is not
//     called, the pending flag is cleared, and the error names how many tasks
//     are still running. Never "we waited long enough, apply anyway" — that is
//     precisely the mid-task change this gate forbids, and a timeout is the
//     case where it would hurt most (a long task is the one with the most
//     cache to lose).
//
// # What it does NOT guarantee: one catalog per CONTINUOUS execution
//
// The unit this gate protects is a stretch of continuous execution, not a task
// id. A task that suspends for human approval (manual mode: RunTask returns
// ErrSuspended, its end func runs, the gate reopens) and is later resumed
// through a fresh RunTask may therefore observe a DIFFERENT plugin catalog
// across that suspend/resume boundary — including in the conversation it
// restored from its checkpoint, whose prompt prefix is invalidated by the
// change exactly as a mid-task apply would have invalidated it.
//
// This is an accepted narrowing, not an oversight. A suspended task is waiting
// on a human and can sit for hours or days; counting it as in flight would let
// one unanswered approval wedge every plugin reload for as long as it went
// unanswered, and an operator with no way to load a plugin is a worse failure
// than one prompt-cache miss on the resumed leg. What the gate still guarantees
// absolutely is that no apply lands while a task is actually executing.
//
// A TaskGate is safe for concurrent use by any number of goroutines, and its
// zero value is not usable — construct one with NewTaskGate.
type TaskGate struct {
	mu sync.Mutex
	// running counts tasks between a successful Begin and its end. Guarded by
	// mu rather than being an atomic, because Begin has to test pending and
	// register the task as one indivisible step: with separate atomics a Begin
	// could pass the test, an apply could then set pending and observe a zero
	// count, and the task would join a catalog that is already being replaced.
	running int
	// pending reports that an apply owns this gate right now: it is set for the
	// whole of ApplyAtBoundary, from before the wait until after fn returns, so
	// no task can start into a catalog that is mid-change. Guarded by mu.
	pending bool
	// idle is closed once running reaches zero while an apply is pending; it is
	// what lets ApplyAtBoundary wait with a bound (a sync.Cond cannot be
	// selected on alongside a context). It is non-nil exactly while pending is
	// set, and a fresh channel is made per apply so a timed-out apply leaves
	// nothing closed behind for the next one. Guarded by mu.
	idle chan struct{}
}

// NewTaskGate returns a gate with no tasks in flight and no apply pending.
func NewTaskGate() *TaskGate {
	return &TaskGate{}
}

// Begin registers one in-flight task and returns the end func that retires it.
//
// It reports an error wrapping ErrApplyPending if a plugin change is currently
// waiting for a task boundary; the task must not start, because it would run
// against a capability catalog that is about to be replaced underneath it. The
// refusal is immediate — Begin never blocks — so the caller is free to retry
// shortly or to surface the condition. It is temporary: the gate reopens as
// soon as the apply finishes, whether the apply succeeded or failed.
//
// When Begin reports an error the returned end func is nil and must not be
// called. Otherwise end must be called exactly once, when the task is over
// (defer it): until it is, no plugin change can be applied. Calling it a second
// time is a programming error that would retire a task twice and let an apply
// run while another task is still live, so end panics rather than corrupting
// the count quietly.
func (g *TaskGate) Begin() (end func(), err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.pending {
		return nil, fmt.Errorf("begin task: %w", ErrApplyPending)
	}
	g.running++

	var ended atomic.Bool
	return func() {
		if !ended.CompareAndSwap(false, true) {
			panic("runtime: TaskGate: end func called more than once; " +
				"each Begin must be retired exactly once")
		}
		g.finish()
	}, nil
}

// BeginChild registers one in-flight task that continues a task already in
// flight — a delegated sub-task — and returns the end func that retires it.
//
// Unlike Begin it never consults the pending flag and therefore never refuses.
// The contract this gate enforces protects work that has ALREADY started, and a
// child of an in-flight task is part of that work rather than a new arrival.
// Refusing it would break the very task the apply is holding: the parent
// started before the apply was requested, is entitled to finish against the
// catalog it started with, and would instead see its delegation fail for a
// reason it can neither retry nor understand. The apply loses nothing by
// admitting the child — it is already waiting for the parent, and the child
// finishes inside the parent's own in-flight window (for the one path where it
// can outlive the parent, RunSubTaskAsync holds a token of its own from spawn
// time until the background work is over, so that window is covered too).
//
// Because there is no refusal there is no error to report, so BeginChild
// returns only end. end carries exactly Begin's contract: call it once, when
// the child is over (defer it); a second call panics rather than retiring a
// task twice.
//
// The caller must be certain the task really is a continuation of one already
// in flight — a top-level task admitted this way would walk straight through a
// pending apply into a catalog that is being replaced. Calling it while an
// apply has already reached its boundary (nothing in flight) is that mistake by
// definition, and panics.
func (g *TaskGate) BeginChild() (end func()) {
	g.mu.Lock()
	if g.pending && g.running == 0 {
		g.mu.Unlock()
		panic("runtime: TaskGate.BeginChild: no task is in flight while an apply holds the gate; " +
			"a child task cannot continue a parent that is not running")
	}
	g.running++
	g.mu.Unlock()

	var ended atomic.Bool
	return func() {
		if !ended.CompareAndSwap(false, true) {
			panic("runtime: TaskGate: end func called more than once; " +
				"each BeginChild must be retired exactly once")
		}
		g.finish()
	}
}

// finish retires one in-flight task and, if that was the last one an apply is
// waiting for, releases the apply.
func (g *TaskGate) finish() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.running <= 0 {
		panic(fmt.Sprintf("runtime: TaskGate: end func retired a task while %d were running; "+
			"in-flight accounting is corrupt", g.running))
	}
	g.running--
	if g.pending && g.running == 0 {
		// Closes exactly once per apply: idle is made open only when running
		// was already above zero, and while pending is set running can only
		// rise from a non-zero count (Begin refuses; BeginChild panics rather
		// than raise it from zero), so this is the single transition to zero.
		close(g.idle)
	}
}

// ApplyAtBoundary runs fn at a task boundary: it marks a plugin change pending
// (so no new task can start), waits up to wait for the tasks already running to
// finish, runs fn with none in flight, and then reopens the gate.
//
// fn therefore observes a quiescent gate, which is the whole point: whatever it
// changes about the shared tool registry becomes visible only to tasks that
// start afterwards. fn must not call Begin or ApplyAtBoundary on this gate —
// both are refused while it holds the gate, by design.
//
// It reports an error, without having run fn, when:
//
//   - the wait expires or ctx is cancelled first — the error wraps ctx.Err()
//     and names how many tasks are still running. Nothing is changed: applying
//     anyway is the mid-task change this gate exists to prevent, and giving up
//     loudly lets the caller retry at a calmer moment;
//   - ctx is already done even though the gate turns out to be idle right
//     now — a cancelled ctx means "don't do this", and that holds whether or
//     not any waiting was ever needed to reach the boundary;
//   - another apply is already pending — that convergence belongs to its own
//     caller, and reporting success here would claim an apply that never ran.
//
// A wait of zero or less means "apply only if the gate is idle right now":
// there is no wait to expire, so a task in flight is reported immediately.
//
// An error from fn is returned wrapped, so errors.Is and errors.As still reach
// it. The pending flag is cleared before returning on every path — including
// the one where fn panics, since it is cleared by a defer — because a failed
// apply that wedged the gate shut would refuse every later task forever.
//
// fn must not be nil — an apply with nothing to apply is a programming error at
// the call site, not a state to tolerate, so ApplyAtBoundary panics.
func (g *TaskGate) ApplyAtBoundary(ctx context.Context, wait time.Duration, fn func() error) error {
	if fn == nil {
		panic("runtime: TaskGate.ApplyAtBoundary: fn is nil; there is nothing to apply at the boundary")
	}

	idle, err := g.beginApply()
	if err != nil {
		return err
	}
	defer g.endApply()

	if err := g.awaitIdle(ctx, wait, idle); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return fmt.Errorf("apply plugin change at task boundary: %w", err)
	}
	return nil
}

// beginApply claims the gate for one apply and returns the channel that reports
// when the tasks already in flight have finished. The channel comes back
// already closed when there were none, so the common case does not wait.
func (g *TaskGate) beginApply() (<-chan struct{}, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.pending {
		return nil, fmt.Errorf("apply plugin change at task boundary: %w", errApplyInProgress)
	}
	g.pending = true
	g.idle = make(chan struct{})
	if g.running == 0 {
		close(g.idle)
	}
	return g.idle, nil
}

// endApply releases the gate, reopening it for new tasks. It runs on every exit
// path of ApplyAtBoundary, including the ones that never reached fn.
func (g *TaskGate) endApply() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.pending {
		panic("runtime: TaskGate: an apply ended while none was pending; " +
			"pending accounting is corrupt")
	}
	g.pending = false
	g.idle = nil
}

// awaitIdle waits for idle, giving up after wait or when ctx ends, whichever
// comes first. Before reporting success it also checks the parent ctx (not
// the derived waitCtx) one last time, so a ctx that was already cancelled
// when the gate happened to be idle right now still stops fn from running —
// see the "ctx is already done" case documented on ApplyAtBoundary.
//
// Giving up is an error naming the tasks that are still running and how long
// the wait actually ran: the caller has to be able to tell "nothing was
// applied, and here is what was in the way" from a successful apply.
func (g *TaskGate) awaitIdle(ctx context.Context, wait time.Duration, idle <-chan struct{}) error {
	start := time.Now()
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	select {
	case <-idle:
	case <-waitCtx.Done():
	}

	// The deadline and the last task's end can land together; select picks
	// between two ready cases at random, so ask once more before failing an
	// apply that in fact reached its boundary.
	select {
	case <-idle:
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("apply plugin change at task boundary: ctx was already done "+
				"once the boundary was reached, nothing was applied: %w", err)
		}
		return nil
	default:
	}

	// Captured right where the recheck just failed, not after any further
	// work, so a last end() landing after this point cannot make the count
	// in the error contradict the "still running" it is reporting.
	running := g.runningCount()
	return fmt.Errorf("apply plugin change at task boundary: waited %s for the running tasks to finish, "+
		"%d task(s) still running, nothing was applied: %w", time.Since(start), running, waitCtx.Err())
}

// runningCount reports how many tasks are in flight right now. It is a snapshot
// for an error message, not a value to branch on.
func (g *TaskGate) runningCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.running
}
