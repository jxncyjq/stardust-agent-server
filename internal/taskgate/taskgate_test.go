package taskgate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// taskGateTestWait is the bound every "this must happen" wait in this file
// uses. Hitting it fails the test: no wait here may hang until the package
// timeout.
const taskGateTestWait = 5 * time.Second

// taskGateNegativeWindow is how long the "fn has not run yet" assertions give
// the implementation to misbehave before concluding it did not. It only ever
// shortens a passing test's runtime, never turns a failure into a pass: the
// positive half of every such test still waits on a channel.
const taskGateNegativeWindow = 200 * time.Millisecond

// taskGateRecvWithin receives from ch or fails the test at the bound.
func taskGateRecvWithin[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()

	select {
	case v := <-ch:
		return v
	case <-time.After(taskGateTestWait):
		var zero T
		t.Fatalf("timed out after %s waiting for %s", taskGateTestWait, what)
		return zero
	}
}

// taskGateMustBegin starts a task on the gate and fails the test if the gate
// refused.
func taskGateMustBegin(t *testing.T, g *TaskGate) func() {
	t.Helper()

	end, err := g.Begin()
	if err != nil {
		t.Fatalf("Begin: unexpected error: %v", err)
	}
	if end == nil {
		t.Fatal("Begin returned a nil end func with a nil error")
	}
	return end
}

func TestApplyAtBoundaryRunsFnWhenNothingIsRunning(t *testing.T) {
	t.Parallel()

	g := NewTaskGate()
	var calls atomic.Int64
	err := g.ApplyAtBoundary(context.Background(), taskGateTestWait, func() error {
		calls.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("ApplyAtBoundary: unexpected error: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fn call count = %d, want 1", got)
	}

	// The gate is open again afterwards.
	taskGateMustBegin(t, g)()
}

func TestApplyAtBoundaryWaitsForAnInFlightTask(t *testing.T) {
	t.Parallel()

	g := NewTaskGate()
	end := taskGateMustBegin(t, g)

	// taskEnded is set immediately before end() below, so fn can prove it did
	// not run while the task was in flight rather than only that it ran late.
	var taskEnded atomic.Bool
	fnRan := make(chan struct{})
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- g.ApplyAtBoundary(context.Background(), taskGateTestWait, func() error {
			if !taskEnded.Load() {
				return errors.New("fn ran while a task was still in flight")
			}
			close(fnRan)
			return nil
		})
	}()

	// fn must not run, and the apply must not finish, while the task is in
	// flight. Bounded: the window only has to be long enough for a goroutine
	// to start, and the positive half below is a channel, not a guess.
	select {
	case <-fnRan:
		t.Fatal("fn ran before the in-flight task ended")
	case err := <-applyDone:
		t.Fatalf("ApplyAtBoundary returned while a task was still in flight: %v", err)
	case <-time.After(taskGateNegativeWindow):
	}

	taskEnded.Store(true)
	end()

	// Poll applyDone without blocking before waiting on fnRan: if fn ran early
	// (a regression), it returns its real error into applyDone and never
	// closes fnRan, so waiting on fnRan first would report a generic timeout
	// and bury the actual cause sitting right here.
	select {
	case err := <-applyDone:
		t.Fatalf("ApplyAtBoundary returned before fn confirmed it ran (fn likely ran early): %v", err)
	default:
	}

	taskGateRecvWithin(t, fnRan, "fn to run after the in-flight task ended")
	if err := taskGateRecvWithin(t, applyDone, "ApplyAtBoundary to return"); err != nil {
		t.Fatalf("ApplyAtBoundary: unexpected error: %v", err)
	}
}

func TestBeginIsRefusedWhileAnApplyIsPending(t *testing.T) {
	t.Parallel()

	g := NewTaskGate()
	// Probing from inside fn is deterministic: fn only runs while the apply is
	// pending, so there is no window to guess at.
	var (
		beginErr  error
		beginEnd  func()
		refusedIn time.Duration
	)
	err := g.ApplyAtBoundary(context.Background(), taskGateTestWait, func() error {
		start := time.Now()
		beginEnd, beginErr = g.Begin()
		refusedIn = time.Since(start)
		return nil
	})
	if err != nil {
		t.Fatalf("ApplyAtBoundary: unexpected error: %v", err)
	}
	if beginErr == nil {
		t.Fatal("Begin succeeded while an apply was pending, want an error")
	}
	if beginEnd != nil {
		t.Fatal("Begin returned a non-nil end func together with an error")
	}
	if !errors.Is(beginErr, ErrApplyPending) {
		t.Fatalf("Begin error = %v, want one matching ErrApplyPending", beginErr)
	}
	// It refuses; it does not block until the wait bound expires.
	if refusedIn >= taskGateTestWait {
		t.Fatalf("Begin blocked for %s before refusing, want an immediate refusal", refusedIn)
	}

	// The refusal is temporary: the gate is open again once the apply is done.
	taskGateMustBegin(t, g)()
}

func TestApplyAtBoundaryTimesOutWithoutRunningFn(t *testing.T) {
	t.Parallel()

	g := NewTaskGate()
	end := taskGateMustBegin(t, g)
	defer end()

	var calls atomic.Int64
	err := g.ApplyAtBoundary(context.Background(), 50*time.Millisecond, func() error {
		calls.Add(1)
		return nil
	})
	// Asserted before the error check on purpose: "fn ran anyway" is the
	// contract violation that matters, so it must be what a regression reports.
	if got := calls.Load(); got != 0 {
		t.Fatalf("fn call count = %d, want 0: a wait timeout must never apply the change anyway", got)
	}
	if err == nil {
		t.Fatal("ApplyAtBoundary returned nil after the wait expired, want an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want one matching context.DeadlineExceeded", err)
	}
	// The error names how many tasks are still running.
	if !strings.Contains(err.Error(), "1 task") {
		t.Fatalf("error = %q, want it to name the 1 task still running", err)
	}

	// The pending flag was cleared: a timed-out apply must not wedge the gate.
	taskGateMustBegin(t, g)()
}

func TestApplyAtBoundaryStopsForACancelledContextWithoutRunningFn(t *testing.T) {
	t.Parallel()

	g := NewTaskGate()
	end := taskGateMustBegin(t, g)
	defer end()

	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int64
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- g.ApplyAtBoundary(ctx, taskGateTestWait, func() error {
			calls.Add(1)
			return nil
		})
	}()
	cancel()

	err := taskGateRecvWithin(t, applyDone, "ApplyAtBoundary to give up on the cancelled context")
	if got := calls.Load(); got != 0 {
		t.Fatalf("fn call count = %d, want 0: a cancelled wait must never apply the change anyway", got)
	}
	if err == nil {
		t.Fatal("ApplyAtBoundary returned nil for a cancelled context, want an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want one matching context.Canceled", err)
	}

	taskGateMustBegin(t, g)()
}

func TestApplyAtBoundarySurfacesFnError(t *testing.T) {
	t.Parallel()

	g := NewTaskGate()
	sentinel := errors.New("converge plugin set")
	err := g.ApplyAtBoundary(context.Background(), taskGateTestWait, func() error {
		return fmt.Errorf("mount plugin echo: %w", sentinel)
	})
	if err == nil {
		t.Fatal("ApplyAtBoundary returned nil for a failing fn, want an error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want one matching the error fn returned", err)
	}

	// A failing fn still clears the pending flag.
	taskGateMustBegin(t, g)()
	if err := g.ApplyAtBoundary(context.Background(), taskGateTestWait, func() error { return nil }); err != nil {
		t.Fatalf("second ApplyAtBoundary after a failing fn: unexpected error: %v", err)
	}
}

func TestApplyAtBoundaryRefusesAConcurrentApply(t *testing.T) {
	t.Parallel()

	g := NewTaskGate()
	var inner error
	err := g.ApplyAtBoundary(context.Background(), taskGateTestWait, func() error {
		inner = g.ApplyAtBoundary(context.Background(), taskGateTestWait, func() error {
			return errors.New("the second apply must not have run fn")
		})
		return nil
	})
	if err != nil {
		t.Fatalf("outer ApplyAtBoundary: unexpected error: %v", err)
	}
	if inner == nil {
		t.Fatal("a second apply started while one was already pending, want an error")
	}

	taskGateMustBegin(t, g)()
}

func TestBeginEndPanicsWhenCalledTwice(t *testing.T) {
	t.Parallel()

	g := NewTaskGate()
	end := taskGateMustBegin(t, g)
	end()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("calling end twice did not panic")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "end") {
			t.Fatalf("panic message = %q, want it to name the end func", msg)
		}
	}()
	end()
}

func TestApplyAtBoundaryPanicsOnNilFn(t *testing.T) {
	t.Parallel()

	g := NewTaskGate()
	defer func() {
		if recover() == nil {
			t.Fatal("ApplyAtBoundary(nil fn) did not panic")
		}
	}()
	//nolint:errcheck // the call panics; there is no error to check.
	_ = g.ApplyAtBoundary(context.Background(), taskGateTestWait, nil)
}

func TestTaskGateUnderConcurrentBeginAndApply(t *testing.T) {
	t.Parallel()

	const (
		workers = 50
		rounds  = 100
	)

	g := NewTaskGate()
	var refused atomic.Int64
	var unexpected atomic.Int64
	firstRefusal := make(chan struct{})
	var refusalOnce sync.Once

	// fn stays inside the apply — and so keeps the gate pending — until a
	// Begin has actually been refused. That is what makes the contention this
	// test claims to exercise a fact rather than a hope.
	inFn := make(chan struct{})
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- g.ApplyAtBoundary(context.Background(), taskGateTestWait, func() error {
			close(inFn)
			select {
			case <-firstRefusal:
				return nil
			case <-time.After(taskGateTestWait):
				return errors.New("no Begin was refused while the apply was pending")
			}
		})
	}()

	// Start the workers only once the apply is provably inside fn, so their
	// Begin calls cannot all slip past before the gate ever goes pending.
	taskGateRecvWithin(t, inFn, "the apply to enter fn")

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				end, err := g.Begin()
				if err != nil {
					if !errors.Is(err, ErrApplyPending) {
						// Recorded, not silently dropped: a non-test goroutine
						// cannot call t.Fatalf, but a regression that returns
						// some other error type must still fail this test, not
						// slide by because other calls still hit ErrApplyPending.
						unexpected.Add(1)
						continue
					}
					refused.Add(1)
					refusalOnce.Do(func() { close(firstRefusal) })
					continue
				}
				end()
			}
		}()
	}

	workersDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(workersDone)
	}()
	taskGateRecvWithin(t, workersDone, "the worker goroutines to finish")

	if err := taskGateRecvWithin(t, applyDone, "ApplyAtBoundary to return"); err != nil {
		t.Fatalf("ApplyAtBoundary: unexpected error: %v", err)
	}
	if got := unexpected.Load(); got != 0 {
		t.Fatalf("Begin returned %d error(s) that did not match ErrApplyPending", got)
	}
	if got := refused.Load(); got < 1 {
		t.Fatalf("refused Begin count = %d, want at least 1: the apply window and the "+
			"Begin calls never overlapped, so this test proved nothing", got)
	}
	t.Logf("%d of %d Begin calls were refused while the apply held the gate", refused.Load(), workers*rounds)

	// Every accounting entry balanced out: the gate is idle and usable.
	if err := g.ApplyAtBoundary(context.Background(), taskGateTestWait, func() error { return nil }); err != nil {
		t.Fatalf("ApplyAtBoundary after the concurrent run: unexpected error: %v", err)
	}
}

// TestApplyAtBoundaryStopsForACancelledContextWhenGateIsIdle covers the blind
// spot the review found: TestApplyAtBoundaryStopsForACancelledContextWithoutRunningFn
// above always keeps a task in flight, so it never exercises the case where
// the gate happens to be idle already. An already-cancelled ctx means "don't
// do this" regardless of whether the gate ever needed to wait, and this must
// hold even though beginApply closes idle immediately when running == 0.
func TestApplyAtBoundaryStopsForACancelledContextWhenGateIsIdle(t *testing.T) {
	t.Parallel()

	g := NewTaskGate()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before ApplyAtBoundary is ever called

	var calls atomic.Int64
	err := g.ApplyAtBoundary(ctx, taskGateTestWait, func() error {
		calls.Add(1)
		return nil
	})
	if got := calls.Load(); got != 0 {
		t.Fatalf("fn call count = %d, want 0: an already-cancelled ctx must never run fn, "+
			"even when the gate has no in-flight task", got)
	}
	if err == nil {
		t.Fatal("ApplyAtBoundary returned nil for an already-cancelled ctx on an idle gate, want an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want one matching context.Canceled", err)
	}

	// The gate is left usable: this is a refusal to run fn, not a wedge.
	taskGateMustBegin(t, g)()
	if err := g.ApplyAtBoundary(context.Background(), taskGateTestWait, func() error { return nil }); err != nil {
		t.Fatalf("ApplyAtBoundary after the cancelled-ctx case: unexpected error: %v", err)
	}
}
