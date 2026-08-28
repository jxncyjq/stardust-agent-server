package host

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

// poolEchoRequest is the opEcho body every pool test reuses; opEcho doubles
// "n", so a healthy instance answers with "doubled":42.
var poolEchoRequest = []byte(`{"name":"legion","n":21}`)

// newPoolFixture builds a pool of real fixture instances: one wazero Runtime
// and one CompiledModule shared by every instance the pool creates, which is
// exactly the arrangement Task 6 will use. Real instances are used rather than
// stubs because the invariant under test — a call interrupted by a context
// timeout leaves an instance that must never be pooled again — is wazero's
// actual WithCloseOnContextDone behavior, which no stub reproduces.
//
// The returned function reports how many instances the factory has created,
// which is how the tests distinguish "reused the pooled instance" from
// "created a replacement". The runtime is closed by newTestFixture's cleanup,
// so a test that does not drain its pool still leaks nothing.
func newPoolFixture(t *testing.T, size int) (*pool, func() int64) {
	t.Helper()

	rt, compiled := newTestFixture(t, testMemoryPages)

	var created atomic.Int64
	p := newPool(size, func() (*Instance, error) {
		created.Add(1)
		return NewInstance(context.Background(), rt, compiled)
	})
	return p, created.Load
}

// mustAcquire fails the test if the pool cannot hand out an instance.
func mustAcquire(t *testing.T, p *pool) *Instance {
	t.Helper()

	inst, err := p.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if inst == nil {
		t.Fatal("acquire returned a nil Instance and a nil error")
	}
	return inst
}

// assertEchoWorks proves the instance is usable, not merely non-nil.
func assertEchoWorks(t *testing.T, inst *Instance) {
	t.Helper()

	out, err := inst.Invoke(context.Background(), opEcho, poolEchoRequest)
	if err != nil {
		t.Fatalf("Invoke(opEcho) on an instance the pool handed out: %v", err)
	}
	if !strings.Contains(string(out), `"doubled":42`) {
		t.Fatalf("Invoke(opEcho) = %s, want it to contain \"doubled\":42", out)
	}
}

// killByTimeout runs the fixture's pure-compute infinite loop under a short
// deadline, which is how WithCloseOnContextDone is made to interrupt a call:
// wazero closes the whole module, so the instance is scrap.
func killByTimeout(t *testing.T, inst *Instance) {
	t.Helper()

	cctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	if _, err := inst.Invoke(cctx, opBusyLoop, nil); err == nil {
		t.Fatal("Invoke(opBusyLoop) under a short deadline returned no error; the guest was not interrupted")
	}
	if !inst.Dead() {
		t.Fatal("Instance.Dead() = false after a timeout interrupted its call, want true")
	}
}

// TestPoolReusesAReleasedInstance is the base case the two invariants sit on
// top of: a released, healthy instance is the very same one the next acquire
// hands out, and the factory is not called again.
func TestPoolReusesAReleasedInstance(t *testing.T) {
	p, created := newPoolFixture(t, 1)

	first := mustAcquire(t, p)
	assertEchoWorks(t, first)
	p.release(first)

	second := mustAcquire(t, p)
	if second != first {
		t.Error("acquire created a new instance although a healthy one was released back into the pool")
	}
	assertEchoWorks(t, second)
	p.release(second)

	if got := created(); got != 1 {
		t.Errorf("factory called %d times for one slot, want 1", got)
	}
	if err := p.drain(context.Background()); err != nil {
		t.Errorf("drain: %v", err)
	}
}

// TestPoolNeverHandsOutAnInstanceATimeoutKilled is invariant 1: a dead
// instance never goes back into the pool. The instance is killed the way
// production kills one — a context deadline expiring mid-call, which
// WithCloseOnContextDone answers by closing the module — and then released.
// The pool must discard it (and close it) rather than pool it, and the next
// acquire must hand out a fresh, working instance.
//
// This is the test the mandated mutation (deleting release's Dead() check)
// must break: without that check the released corpse is pooled, so the second
// acquire returns the same pointer, creates no replacement, and cannot echo.
func TestPoolNeverHandsOutAnInstanceATimeoutKilled(t *testing.T) {
	p, created := newPoolFixture(t, 1)

	killed := mustAcquire(t, p)
	killByTimeout(t, killed)

	// Count the closes of the corpse: discarding it means closing it, not just
	// dropping the pointer. killed.Dead() is already true here (killByTimeout
	// asserts it), so asserting Dead() after the release would prove nothing —
	// only the disposer being called does. closeInstance is the package's seam
	// for that, and the substitute still performs the real close.
	var killedCloses int
	originalClose := closeInstance
	closeInstance = func(ctx context.Context, i *Instance) error {
		if i == killed {
			killedCloses++
		}
		return originalClose(ctx, i)
	}
	t.Cleanup(func() { closeInstance = originalClose })

	p.release(killed)
	if killedCloses != 1 {
		t.Errorf("release closed the discarded instance %d times, want exactly 1", killedCloses)
	}

	fresh := mustAcquire(t, p)
	if fresh == killed {
		t.Fatal("acquire handed back the instance a timeout killed; release must discard a dead instance, not pool it")
	}
	if fresh.Dead() {
		t.Error("acquire handed out a dead instance")
	}
	assertEchoWorks(t, fresh)
	p.release(fresh)

	if got := created(); got != 2 {
		t.Errorf("factory called %d times, want 2 (the original plus one replacement for the killed instance)", got)
	}

	if err := p.drain(context.Background()); err != nil {
		t.Errorf("drain: %v", err)
	}
}

// TestPoolAcquireQueuesWhenExhaustedAndReportsContextCancellation covers the
// semaphore: with every instance checked out, acquire waits instead of
// exceeding the pool size, and a caller whose context is cancelled while
// queued gets that context's error back — not a nil instance with a nil
// error, and not an instance the pool does not have.
//
// The 200ms window before the cancellation is what proves the acquire really
// queued: a pool that ignored its size would have returned immediately.
func TestPoolAcquireQueuesWhenExhaustedAndReportsContextCancellation(t *testing.T) {
	p, created := newPoolFixture(t, 1)

	held := mustAcquire(t, p)

	queueCtx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		inst *Instance
		err  error
	}
	queued := make(chan outcome, 1)
	go func() {
		inst, err := p.acquire(queueCtx)
		queued <- outcome{inst, err}
	}()

	select {
	case got := <-queued:
		t.Fatalf("acquire returned (%v, %v) while the pool's only instance was checked out; it must queue", got.inst, got.err)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	select {
	case got := <-queued:
		if got.err == nil {
			t.Fatal("acquire returned no error after its context was cancelled while queued")
		}
		if !errors.Is(got.err, context.Canceled) {
			t.Errorf("acquire error %q does not carry context.Canceled", got.err)
		}
		if got.inst != nil {
			t.Error("acquire returned an instance together with an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquire did not return after its context was cancelled")
	}

	// The abandoned attempt must not have consumed the slot: releasing the
	// held instance has to make the pool usable again. A leaked slot would
	// hang this acquire (its context has no deadline) rather than fail it.
	p.release(held)
	again := mustAcquire(t, p)
	assertEchoWorks(t, again)
	p.release(again)

	if got := created(); got != 1 {
		t.Errorf("factory called %d times for one slot, want 1", got)
	}
	if err := p.drain(context.Background()); err != nil {
		t.Errorf("drain: %v", err)
	}
}

// TestPoolDrainBlocksUntilTheInFlightCallFinishes is invariant 2: drain
// returns only once in-flight work has reached zero. The instance is checked
// out and has really called into the guest, so what drain waits for is a
// caller that could still be inside Invoke — the case where closing the
// module underneath it would interrupt a live call.
func TestPoolDrainBlocksUntilTheInFlightCallFinishes(t *testing.T) {
	p, _ := newPoolFixture(t, 1)

	inst := mustAcquire(t, p)
	assertEchoWorks(t, inst)

	drained := make(chan error, 1)
	go func() { drained <- p.drain(context.Background()) }()

	select {
	case err := <-drained:
		t.Fatalf("drain returned (%v) while an instance was still checked out", err)
	case <-time.After(200 * time.Millisecond):
	}

	p.release(inst)
	select {
	case err := <-drained:
		if err != nil {
			t.Errorf("drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return after the in-flight call finished")
	}

	if !inst.Dead() {
		t.Error("drain returned without closing the instance it drained")
	}
}

// TestPoolAcquireAfterDrainFailsImmediately is the other half of invariant 2:
// once drain has set the pool closing, acquire reports that rather than
// blocking. The context deliberately carries no deadline — an implementation
// that blocked would hang this test to the package timeout instead of quietly
// passing after a wait.
func TestPoolAcquireAfterDrainFailsImmediately(t *testing.T) {
	p, _ := newPoolFixture(t, 1)

	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	inst, err := p.acquire(context.Background())
	if err == nil {
		t.Fatal("acquire succeeded after drain")
	}
	if inst != nil {
		t.Error("acquire returned an instance together with an error")
	}
	if !strings.Contains(err.Error(), "drain") {
		t.Errorf("acquire error %q does not say the pool is draining", err)
	}
}

// TestPoolWakesAQueuedAcquireWhenDrainBegins pins the case that separates
// "rejects new acquires" from "rejects acquires that have not got a slot yet":
// a caller already queued for a slot when drain begins must be woken with an
// error. Its context has no deadline, so an implementation that left it
// parked would hang rather than pass — and the queued caller cannot be
// satisfied by the pool either, because the only instance stays checked out
// until after the assertion.
func TestPoolWakesAQueuedAcquireWhenDrainBegins(t *testing.T) {
	p, _ := newPoolFixture(t, 1)

	held := mustAcquire(t, p)

	// The hand-off is a channel rather than a sleep: the goroutine signals
	// immediately before it calls acquire, so the test proceeds once that
	// acquire is under way instead of once a sleep guessed long enough. The
	// sleep was worse than slow — being too short would not have failed here,
	// it would have quietly demoted this test to a copy of
	// TestPoolAcquireAfterDrainFailsImmediately.
	//
	// What the signal proves is that the acquire has started; that it has
	// reached the park inside it is not observable from outside the pool
	// without instrumenting the pool itself, which is not worth it for this
	// test. What is observable is asserted below.
	queued := make(chan error, 1)
	acquiring := make(chan struct{})
	go func() {
		close(acquiring)
		inst, err := p.acquire(context.Background())
		if inst != nil {
			p.release(inst)
		}
		queued <- err
	}()
	<-acquiring

	// And the queue is a state the test can see, not an assumption: the only
	// instance is checked out, so free is empty and the caller above cannot be
	// served by the pool at all — the only thing that can end its wait is drain
	// waking it.
	if got := len(p.free); got != 0 {
		t.Fatalf("len(p.free) = %d while the pool's only instance was checked out, want 0: the caller is not queueing for anything", got)
	}

	drained := make(chan error, 1)
	go func() { drained <- p.drain(context.Background()) }()

	select {
	case err := <-queued:
		if err == nil {
			t.Error("a queued acquire succeeded although drain had begun")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a queued acquire was never woken after drain began")
	}

	p.release(held)
	select {
	case err := <-drained:
		if err != nil {
			t.Errorf("drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return after the checked-out instance was released")
	}
}

// TestPoolServesConcurrentCallersWithoutSharingAnInstance is the race test:
// 50 goroutines take several turns each through a pool of two real instances,
// invoking the guest every turn. Instance requires its caller to serialize
// every call on it, so the test asserts what the pool owes its callers — an
// instance handed to one caller is handed to nobody else until it comes back —
// and that the pool never exceeds its size.
//
// It also asserts that callers actually contended (two instances checked out
// at the same moment). A concurrency test that never overlapped would prove
// nothing, so the absence of overlap is a failure rather than a quiet pass.
func TestPoolServesConcurrentCallersWithoutSharingAnInstance(t *testing.T) {
	const (
		size       = 2
		goroutines = 50
		rounds     = 4
	)

	p, created := newPoolFixture(t, size)

	var (
		mu            sync.Mutex
		checkedOut    = make(map[*Instance]int)
		concurrent    int
		maxConcurrent int
	)
	// Two reports per round is the worst case (a shared-instance report does
	// not return, so an invoke report can follow it), and nothing drains this
	// channel until after wg.Wait() — sized for one report per round, a
	// thoroughly broken pool would hang the test instead of failing it.
	failures := make(chan error, goroutines*rounds*2)

	begin := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-begin

			for r := 0; r < rounds; r++ {
				inst, err := p.acquire(context.Background())
				if err != nil {
					failures <- fmt.Errorf("goroutine %d round %d: acquire: %w", g, r, err)
					return
				}

				mu.Lock()
				checkedOut[inst]++
				holders := checkedOut[inst]
				concurrent++
				if concurrent > maxConcurrent {
					maxConcurrent = concurrent
				}
				mu.Unlock()
				if holders != 1 {
					failures <- fmt.Errorf("goroutine %d round %d: acquire handed out an instance %d callers hold at once", g, r, holders)
				}

				out, ierr := inst.Invoke(context.Background(), opEcho, poolEchoRequest)

				mu.Lock()
				checkedOut[inst]--
				concurrent--
				mu.Unlock()
				p.release(inst)

				if ierr != nil {
					failures <- fmt.Errorf("goroutine %d round %d: Invoke(opEcho): %w", g, r, ierr)
					return
				}
				if !strings.Contains(string(out), `"doubled":42`) {
					failures <- fmt.Errorf("goroutine %d round %d: Invoke(opEcho) = %s", g, r, out)
				}
			}
		}(g)
	}
	close(begin)
	wg.Wait()
	close(failures)

	for err := range failures {
		t.Error(err)
	}

	mu.Lock()
	gotMax := maxConcurrent
	mu.Unlock()
	if gotMax < 2 {
		t.Errorf("at most %d caller(s) ever held an instance at the same time: the test never contended, so it proves nothing", gotMax)
	}
	if gotMax > size {
		t.Errorf("%d callers held instances at once, want at most the pool size %d", gotMax, size)
	}
	if got := created(); got != size {
		t.Errorf("factory called %d times for %d slots, want %d", got, size, size)
	}

	if err := p.drain(context.Background()); err != nil {
		t.Errorf("drain: %v", err)
	}
}

// TestPoolAcquireReportsAFactoryFailure covers the fail-loud path: a factory
// that cannot build an instance must surface its error to the caller, wrapped
// and with a nil instance — never a nil error, and never a silently empty
// pool. It must also hand the slot back, which the second acquire proves: its
// context has no deadline, so a lost slot hangs the test.
func TestPoolAcquireReportsAFactoryFailure(t *testing.T) {
	rt, compiled := newTestFixture(t, testMemoryPages)

	factoryErr := errors.New("instantiate refused")
	var broken atomic.Bool
	broken.Store(true)
	p := newPool(1, func() (*Instance, error) {
		if broken.Load() {
			return nil, factoryErr
		}
		return NewInstance(context.Background(), rt, compiled)
	})

	inst, err := p.acquire(context.Background())
	if err == nil {
		t.Fatal("acquire succeeded although the factory failed")
	}
	if !errors.Is(err, factoryErr) {
		t.Errorf("acquire error %q does not carry the factory failure", err)
	}
	if inst != nil {
		t.Error("acquire returned an instance together with an error")
	}

	broken.Store(false)
	recovered := mustAcquire(t, p)
	assertEchoWorks(t, recovered)
	p.release(recovered)

	if err := p.drain(context.Background()); err != nil {
		t.Errorf("drain: %v", err)
	}
}

// TestPoolDrainClosesEveryPooledInstance pins the rest of drain's contract:
// every instance the pool created is closed, and a second drain is reported
// rather than silently doing nothing (its "all in flight finished" answer
// would be a guess, since the first drain owns the wait).
func TestPoolDrainClosesEveryPooledInstance(t *testing.T) {
	p, created := newPoolFixture(t, 2)

	first := mustAcquire(t, p)
	second := mustAcquire(t, p)
	if first == second {
		t.Fatal("a pool of two slots handed the same instance to two callers")
	}
	p.release(first)
	p.release(second)

	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !first.Dead() || !second.Dead() {
		t.Errorf("drain left instances open: first.Dead()=%v second.Dead()=%v", first.Dead(), second.Dead())
	}
	if got := created(); got != 2 {
		t.Errorf("factory called %d times for two slots, want 2", got)
	}

	err := p.drain(context.Background())
	if err == nil {
		t.Fatal("a second drain reported success; it cannot vouch for the first drain's wait")
	}
	// The rejection is a sentinel, so a caller can tell "someone else owns this
	// pool's convergence" from a convergence that was attempted and failed.
	if !errors.Is(err, errDrainAlreadyStarted) {
		t.Errorf("second drain error %q does not carry errDrainAlreadyStarted", err)
	}
}

// TestPoolAcquirePanicsOnAFactoryThatReturnsTheSameInstanceTwice pins the one
// caller-supplied hook that can break the pool's central guarantee: a factory
// that hands back an instance the pool already has would put one Instance in
// two slots, and Instance is not safe for concurrent use. The pool does not
// hand that out and hope — it stops.
func TestPoolAcquirePanicsOnAFactoryThatReturnsTheSameInstanceTwice(t *testing.T) {
	rt, compiled := newTestFixture(t, testMemoryPages)

	shared, err := NewInstance(context.Background(), rt, compiled)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	p := newPool(2, func() (*Instance, error) { return shared, nil })

	first := mustAcquire(t, p)
	if first != shared {
		t.Fatalf("acquire returned %p, want the factory's instance %p", first, shared)
	}

	// The pool is abandoned after the panic (see
	// TestPoolAcquirePanicsOnAFactoryThatReturnsNothing): the second acquire
	// dies holding a slot and an in-flight registration.
	defer func() {
		if recover() == nil {
			t.Error("acquire returned normally for a factory that returned an already checked-out instance, want a panic")
		}
	}()
	_, _ = p.acquire(context.Background())
}

// TestPoolDrainNeverCreatesAnInstance covers the lazy-slot case: a pool that
// was never used has nothing to close, and drain must not instantiate a guest
// just to close it.
func TestPoolDrainNeverCreatesAnInstance(t *testing.T) {
	p, created := newPoolFixture(t, 3)

	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := created(); got != 0 {
		t.Errorf("draining an unused pool called the factory %d times, want 0", got)
	}
}

// TestPoolDrainReportsACloseFailure asserts a dispose failure during drain is
// reported rather than swallowed. closeInstance is the package's existing seam
// for this (forcing wazero's own Close to fail is not practically reachable);
// the substitute still performs the real close, because a disposer that
// reports a failure has not earned the right to leak the module.
func TestPoolDrainReportsACloseFailure(t *testing.T) {
	p, _ := newPoolFixture(t, 1)

	inst := mustAcquire(t, p)
	assertEchoWorks(t, inst)
	p.release(inst)

	closeErr := errors.New("close refused")
	original := closeInstance
	closeInstance = func(ctx context.Context, i *Instance) error {
		_ = original(ctx, i)
		return closeErr
	}
	t.Cleanup(func() { closeInstance = original })

	err := p.drain(context.Background())
	if err == nil {
		t.Fatal("drain reported success although closing a pooled instance failed")
	}
	if !errors.Is(err, closeErr) {
		t.Errorf("drain error %q does not carry the close failure", err)
	}
	if !inst.Dead() {
		t.Error("a failing close must still release the module")
	}
}

// TestPoolReleaseReportsADiscardedInstanceCloseFailureThroughDrain covers the
// one place the pool cannot return an error to the caller who caused it:
// release has no error return, and closing the corpse it discards can fail.
// That failure must still reach somebody, so it is joined onto drain's error.
func TestPoolReleaseReportsADiscardedInstanceCloseFailureThroughDrain(t *testing.T) {
	p, _ := newPoolFixture(t, 1)

	killed := mustAcquire(t, p)
	killByTimeout(t, killed)

	closeErr := errors.New("close refused")
	original := closeInstance
	closeInstance = func(ctx context.Context, i *Instance) error {
		_ = original(ctx, i)
		return closeErr
	}
	t.Cleanup(func() { closeInstance = original })

	p.release(killed)

	err := p.drain(context.Background())
	if err == nil {
		t.Fatal("drain reported success although closing a discarded instance failed")
	}
	if !errors.Is(err, closeErr) {
		t.Errorf("drain error %q does not carry the failure release could not return", err)
	}
}

// TestPoolDrainReportsContextCancellationWhileCallsAreInFlight covers drain's
// own error path: a caller that never gives its instance back must not hang
// shutdown forever, and the abandoned wait must be reported rather than
// mistaken for a completed drain.
func TestPoolDrainReportsContextCancellationWhileCallsAreInFlight(t *testing.T) {
	p, _ := newPoolFixture(t, 1)

	inst := mustAcquire(t, p)

	drainCtx, cancel := context.WithCancel(context.Background())
	drained := make(chan error, 1)
	go func() { drained <- p.drain(drainCtx) }()

	// Let drain reach its wait before cancelling, so the cancellation lands on
	// the wait rather than short-circuiting the whole call.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-drained:
		if err == nil {
			t.Fatal("drain reported success although an instance was never released")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("drain error %q does not carry context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return after its context was cancelled")
	}

	// The pool stays closed: a failed drain does not reopen it.
	if _, err := p.acquire(context.Background()); err == nil {
		t.Error("acquire succeeded after a failed drain")
	}
	p.release(inst)
}

// TestNewPoolPanicsOnAnUnusableConfiguration covers the invariant violations
// that are programming errors, not runtime conditions: there is no sensible
// default pool size and no pool without a factory, so neither is quietly
// substituted.
func TestNewPoolPanicsOnAnUnusableConfiguration(t *testing.T) {
	factory := func() (*Instance, error) { return nil, errors.New("never called") }

	tests := []struct {
		name    string
		size    int
		factory func() (*Instance, error)
	}{
		{name: "zero size", size: 0, factory: factory},
		{name: "negative size", size: -1, factory: factory},
		{name: "nil factory", size: 1, factory: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("newPool returned normally, want a panic")
				}
			}()
			_ = newPool(tc.size, tc.factory)
		})
	}
}

// TestPoolAcquirePanicsOnAFactoryThatReturnsNothing pins the fail-loud rule
// at its sharpest: a factory that hands back neither an instance nor an error
// has broken its contract, and acquire must never pass that nil on to a
// caller as a usable instance.
func TestPoolAcquirePanicsOnAFactoryThatReturnsNothing(t *testing.T) {
	p := newPool(1, func() (*Instance, error) { return nil, nil })

	// The pool is deliberately abandoned once the panic is recovered: acquire
	// died with the slot taken out of free and its in-flight registration still
	// standing, which is correct for a terminal invariant violation but leaves
	// the pool unusable. Do not extend this test with another acquire or a
	// drain — drain would wait forever for an in-flight call that no longer
	// exists and the package would time out instead of failing.
	defer func() {
		if recover() == nil {
			t.Error("acquire returned normally for a factory that returned (nil, nil), want a panic")
		}
	}()
	_, _ = p.acquire(context.Background())
}

// newForeignInstance builds a live instance that belongs to no pool, for the
// "release something this pool never handed out" cases. Its runtime is closed
// by newTestFixture's cleanup.
func newForeignInstance(t *testing.T) *Instance {
	t.Helper()

	rt, compiled := newTestFixture(t, testMemoryPages)
	inst, err := NewInstance(context.Background(), rt, compiled)
	if err != nil {
		t.Fatalf("NewInstance for a foreign instance: %v", err)
	}
	return inst
}

// TestPoolReleaseRejectsWhatItNeverHandedOut covers release's own invariant
// assertions. Every case is a caller bug that would otherwise corrupt the
// pool's accounting silently: it would put a second entry into free, so two
// later acquires would hand one Instance to two goroutines (Instance is not
// safe for concurrent use) and in-flight would reach zero while a call was
// still running, letting drain close an instance a caller is inside.
//
// The cases deliberately include pools that are not fully free — a pool of two
// with one slot still checked out, and a pool of one with its only slot checked
// out. Inferring the violation from how full the free channel happens to be
// catches neither: the erroneous entry simply fits, and the corruption is
// silent. release therefore consults what is actually checked out.
func TestPoolReleaseRejectsWhatItNeverHandedOut(t *testing.T) {
	t.Run("nil instance", func(t *testing.T) {
		p, _ := newPoolFixture(t, 1)

		defer func() {
			if recover() == nil {
				t.Error("release(nil) returned normally, want a panic")
			}
		}()
		p.release(nil)
	})

	t.Run("released twice", func(t *testing.T) {
		p, _ := newPoolFixture(t, 1)

		inst := mustAcquire(t, p)
		p.release(inst)

		defer func() {
			if recover() == nil {
				t.Error("a second release of the same instance returned normally, want a panic")
			}
		}()
		p.release(inst)
	})

	t.Run("released twice while another slot is checked out", func(t *testing.T) {
		p, _ := newPoolFixture(t, 2)

		first := mustAcquire(t, p)
		second := mustAcquire(t, p)
		if first == second {
			t.Fatal("a pool of two slots handed the same instance to two callers")
		}
		p.release(first)

		// free now holds one of two entries, so the erroneous second release
		// would fit into it — and the pool would hold "first" twice.
		defer func() {
			if recover() == nil {
				// The pool is corrupt now (first sits in two slots), so the
				// checks below would report a pool that has already failed.
				t.Fatal("a second release of the same instance returned normally although the pool had a free slot, want a panic")
			}
			if got := len(p.free); got != 1 {
				t.Errorf("len(p.free) = %d after the rejected release, want 1: the pool was corrupted instead of rejecting it", got)
			}
			p.release(second)
			if err := p.drain(context.Background()); err != nil {
				t.Errorf("drain after the rejected release: %v", err)
			}
		}()
		p.release(first)
	})

	t.Run("foreign instance, no slot free", func(t *testing.T) {
		p, _ := newPoolFixture(t, 1)

		held := mustAcquire(t, p)
		foreign := newForeignInstance(t)

		// free is empty, so the send of a foreign instance would succeed and
		// the pool would hand out an instance it never created.
		defer func() {
			if recover() == nil {
				// The pool holds an instance it never created; releasing held
				// on top of that would overflow its single slot.
				t.Fatal("release of a foreign instance returned normally, want a panic")
			}
			p.release(held)
			if err := p.drain(context.Background()); err != nil {
				t.Errorf("drain after the rejected release: %v", err)
			}
		}()
		p.release(foreign)
	})

	t.Run("foreign instance while another slot is checked out", func(t *testing.T) {
		p, _ := newPoolFixture(t, 2)

		held := mustAcquire(t, p)
		foreign := newForeignInstance(t)

		defer func() {
			if recover() == nil {
				t.Fatal("release of a foreign instance returned normally although the pool had a free slot, want a panic")
			}
			p.release(held)
			if err := p.drain(context.Background()); err != nil {
				t.Errorf("drain after the rejected release: %v", err)
			}
		}()
		p.release(foreign)
	})
}

// TestPoolInflightCountTracksAcquiredInstances pins the readable counter the
// unload-leak report depends on. A WaitGroup cannot be read, so this shadow
// counter is the only way to say HOW MUCH a drain left behind — and a shadow
// that drifts would report a number nobody can act on.
func TestPoolInflightCountTracksAcquiredInstances(t *testing.T) {
	p, _ := newPoolFixture(t, 2)

	if got := p.inflightCount(); got != 0 {
		t.Fatalf("inflightCount on a fresh pool = %d, want 0", got)
	}
	first := mustAcquire(t, p)
	if got := p.inflightCount(); got != 1 {
		t.Errorf("inflightCount after one acquire = %d, want 1", got)
	}
	second := mustAcquire(t, p)
	if got := p.inflightCount(); got != 2 {
		t.Errorf("inflightCount after two acquires = %d, want 2", got)
	}
	p.release(first)
	p.release(second)
	if got := p.inflightCount(); got != 0 {
		t.Errorf("inflightCount after both releases = %d, want 0", got)
	}
}

// TestDrainThatTimesOutIsMarkedIncomplete pins the sentinel the Loader keys on.
// Without it the one failure that means "guest work outlived the plugin" would
// be indistinguishable from any other disposal error.
func TestDrainThatTimesOutIsMarkedIncomplete(t *testing.T) {
	p, _ := newPoolFixture(t, 1)
	held := mustAcquire(t, p) // never released: the drain cannot converge

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := p.drain(ctx)
	if err == nil {
		t.Fatal("drain with an unreleased instance = nil error, want a timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("drain error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if got := p.inflightCount(); got != 1 {
		t.Errorf("inflightCount after the failed drain = %d, want 1: the held call is what was left behind", got)
	}
	p.release(held)
}
