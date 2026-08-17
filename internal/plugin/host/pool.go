package host

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// errPoolDraining is the condition both of acquire's rejection paths report:
// the pool is shutting down, so no instance will be handed out. It is one
// value rather than two identical strings so a caller can tell "shutting down"
// from a real failure with errors.Is.
var errPoolDraining = errors.New("pool is draining")

// errDrainAlreadyStarted is what a second drain reports. It is a sentinel so a
// caller can tell "someone else already owns this pool's convergence" from a
// convergence that was attempted and failed, which is a different operational
// situation with a different response.
var errDrainAlreadyStarted = errors.New("pool drain already started")

// pool hands out a bounded set of plugin Instances, one caller at a time per
// instance, and converges its in-flight calls on shutdown.
//
// It exists because Instance requires its caller to serialize every call on it
// (Invoke, Dead and Close all touch unsynchronized state), while a plugin's
// tools are called concurrently. The pool is what provides that
// serialization: an instance acquire returns belongs to that caller alone
// until it is released, so the caller's Invoke, the pool's own Dead() check in
// release and drain's Close never run against the same Instance at the same
// time. Handing the same instance to two callers, or touching one that is
// still checked out, would break Instance's contract — so the pool never does.
//
// Concurrency contract of the pool's own methods:
//
//   - acquire and release are safe to call concurrently from any number of
//     goroutines, which is the whole point.
//   - drain is safe to call concurrently with acquire and release; that is the
//     case it is built for (shutdown racing live traffic). It must be called
//     at most once — a second call reports an error rather than pretending to
//     have waited for a convergence the first call owns.
//   - release must be called exactly once per successful acquire, with the
//     instance that acquire returned. Both halves are invariants, not
//     courtesies: violating them corrupts the pool's slot accounting, so
//     release panics rather than corrupting it quietly. The pool tracks which
//     instances are checked out (see checkedOut), so a nil, foreign or
//     repeated release is detected exactly, whatever the pool's size and
//     whatever else is checked out at the time.
//
// Instances are created lazily. newPool cannot report a factory failure (it
// returns no error, deliberately: a pool is cheap and a guest instantiation is
// not), so the first acquire of each slot is what builds that slot's instance
// and the first caller is who hears about a failure to build it.
type pool struct {
	// size is the number of slots, fixed at construction. It is also the
	// number of entries free always holds when no call is in flight, which is
	// what lets drain collect every slot without blocking.
	size int

	// factory builds one new instance, and must build a fresh one on every
	// call (see newPool). It is called by acquire, never by newPool or drain,
	// and never while any pool lock is held.
	factory func() (*Instance, error)

	// free is the pool's semaphore and its instance store in one: it holds
	// exactly one entry per slot, and an entry is either a pooled instance or
	// nil for a slot whose instance has not been created yet (or was discarded
	// after dying). Taking an entry out is taking the slot; putting one back
	// is releasing it. Because entries are conserved — every path that
	// receives one either returns one or hands the instance to a caller who
	// must release it — a send on free never blocks.
	free chan *Instance

	// closed is closed by drain. Its only job is to be selectable: a caller
	// already queued on free when drain begins is woken by it and rejected,
	// rather than waiting for a slot the pool will not hand out.
	closed chan struct{}

	mu sync.Mutex
	// closing is guarded by mu rather than being an atomic.Bool, because
	// acquire has to test it and register with inflight as one indivisible
	// step. sync.WaitGroup requires that an Add which starts when the counter
	// is zero happens before Wait; with an atomic flag, an acquire could pass
	// the test, drain could then set the flag and see a zero counter, and the
	// acquire's Add would land after drain had already declared convergence.
	closing bool
	// checkedOut is the set of instances currently handed out to a caller, and
	// it is the pool's slot accounting made exact: acquire adds an instance on
	// every path that returns one, release removes it, so membership means
	// "one entry of free is missing because this instance holds it".
	//
	// It exists so that release can tell an honest hand-back from a caller bug
	// — a nil instance, an instance this pool never created, or a second
	// release of one it already took back — by asking the record instead of
	// inferring from how full free happens to be. Inference only works when
	// every other slot is idle; at size > 1 a double release would otherwise
	// fit into a free channel that still has room, put the same instance in
	// two slots, and hand it to two callers at once.
	//
	// Guarded by mu.
	checkedOut map[*Instance]struct{}
	// disposeErrs collects failures from closes that happened where nobody
	// could be told: release has no error return, and the instance it
	// discards still has to be closed. drain joins these onto its own result,
	// so such a failure is reported late rather than dropped.
	disposeErrs []error

	// inflight counts callers between a successful acquire and its release —
	// the calls drain waits to reach zero. Add happens under mu (see closing);
	// Done is called after the slot is back in free, so a woken drain always
	// finds every slot there.
	inflight sync.WaitGroup
}

// newPool creates a pool of size slots, each of which will hold one instance
// built by factory on first use.
//
// factory must return a freshly built Instance on every call. Returning the
// same *Instance twice — memoizing it, or handing back one the pool already
// holds — would put one instance in two slots and so aim it at the pool's
// central guarantee: Instance is not safe for concurrent use, and this is the
// one caller-supplied hook that can break the exclusivity the pool provides.
// The pool does not trust the contract silently — an instance that would be
// checked out to a second caller while the first still holds it panics (see
// markCheckedOut) — but it cannot repair such a factory, so the contract is
// the caller's to keep.
//
// size must be greater than zero and factory must be non-nil: a zero-size pool
// could never serve a call and a pool with no factory could never fill a slot,
// so both are programming errors with no sensible default — newPool panics
// rather than substituting one. Choosing the size (Task 6 uses 1) belongs to
// the caller.
func newPool(size int, factory func() (*Instance, error)) *pool {
	if size <= 0 {
		panic(fmt.Sprintf("host: newPool: size must be > 0, got %d", size))
	}
	if factory == nil {
		panic("host: newPool: factory is nil; a pool with no way to build an instance can never serve a call")
	}

	p := &pool{
		size:       size,
		factory:    factory,
		free:       make(chan *Instance, size),
		closed:     make(chan struct{}),
		checkedOut: make(map[*Instance]struct{}, size),
	}
	// Prime every slot as empty: the slot exists from the start (it is the
	// semaphore), the instance in it does not.
	for i := 0; i < size; i++ {
		p.free <- nil
	}
	return p
}

// acquire checks out one instance for the calling goroutine, creating it if
// its slot is still empty. The caller owns that instance exclusively — no
// other goroutine, and not the pool itself, will touch it — until the caller
// hands it back with release, which it must do exactly once.
//
// acquire blocks while every slot is checked out. It stops blocking, with an
// error, when any of three things happens:
//
//   - ctx is cancelled or expires: the error wraps ctx.Err();
//   - drain begins: a queued caller is woken and rejected rather than parked
//     until its own deadline;
//   - the factory fails to build this slot's instance: its error is wrapped
//     and the slot is handed back untouched, so a failure to instantiate does
//     not shrink the pool.
//
// acquire never returns a nil instance with a nil error. A factory that
// returns neither an instance nor an error has broken its contract badly
// enough that passing the nil on would defer the crash to whichever call site
// dereferenced it, so acquire panics instead.
//
// acquire does not re-check Dead() on what it takes out of the pool: release is
// the only way in, and it is where the check belongs (see its comment). The
// residual case is an instance that dies while idle — which takes something
// closing the pool's wazero Runtime underneath it, or the narrow window where
// wazero closes a module just after a call returned successfully. Such an
// instance is handed out once, its first Invoke fails loudly, and release then
// discards it. That is a reported failure and a self-healing slot, not a
// silent one, and it keeps the invariant enforced at exactly one place.
func (p *pool) acquire(ctx context.Context) (*Instance, error) {
	if err := p.enter(); err != nil {
		return nil, fmt.Errorf("acquire plugin instance: %w", err)
	}

	var slot *Instance
	select {
	case slot = <-p.free:
	case <-p.closed:
		p.inflight.Done()
		return nil, fmt.Errorf("acquire plugin instance: %w", errPoolDraining)
	case <-ctx.Done():
		p.inflight.Done()
		return nil, fmt.Errorf("acquire plugin instance: %w", ctx.Err())
	}
	// Winning free while closed is also ready is harmless: this caller is
	// registered with inflight, so drain waits for its release before closing
	// anything.

	if slot != nil {
		p.markCheckedOut(slot)
		return slot, nil
	}

	inst, err := p.factory()
	if err != nil {
		// Hand the slot back before reporting: the send cannot block (this
		// caller took the only entry it is returning), and a slot dropped here
		// would be lost for the pool's whole lifetime.
		p.free <- nil
		p.inflight.Done()
		return nil, fmt.Errorf("create plugin instance: %w", err)
	}
	if inst == nil {
		panic("host: pool.acquire: factory returned a nil Instance and a nil error")
	}
	p.markCheckedOut(inst)
	return inst, nil
}

// call runs one guest operation on an instance nobody else can touch while it
// runs, and is how everything outside this file uses the pool (see guestCaller):
// acquire once, release exactly once on the success path, never on the acquire
// error path. Those are invariants release enforces by panicking, so keeping
// them lives here, in one place, rather than at every call site.
//
// The instance is released whether the call succeeded or not: release is what
// discards a dead instance (a trapped guest, a call wazero interrupted) and
// closes it, so an error path that skipped it would leak the corpse and lose the
// slot for the pool's whole lifetime.
func (p *pool) call(ctx context.Context, op int32, in []byte) ([]byte, error) {
	inst, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer p.release(inst)

	out, err := inst.Invoke(ctx, op, in)
	if err != nil {
		return nil, fmt.Errorf("invoke plugin guest op %d: %w", op, err)
	}
	return out, nil
}

// markCheckedOut records that inst is now the calling goroutine's, so release
// can prove it is being handed back by the caller who took it. An instance
// that is already recorded means the factory handed out an instance the pool
// still holds (see newPool's contract), which would put one Instance in two
// slots — so it is a violated invariant, not a condition to recover from.
func (p *pool) markCheckedOut(inst *Instance) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, dup := p.checkedOut[inst]; dup {
		panic(fmt.Sprintf("host: pool.acquire: instance %p is already checked out; "+
			"the factory must return a fresh Instance on every call", inst))
	}
	p.checkedOut[inst] = struct{}{}
}

// enter registers one caller with inflight unless the pool is draining, in
// which case it reports errPoolDraining for acquire to wrap. The test and the
// registration are a single step under mu — see the closing field's comment for
// why that matters to drain's wait.
func (p *pool) enter() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closing {
		return errPoolDraining
	}
	p.inflight.Add(1)
	return nil
}

// release hands an instance back. inst must be the instance a matching
// acquire returned, and it must be handed back exactly once; the caller must
// not use it afterwards.
//
// That precondition is checked, not assumed: release consults the pool's
// checked-out set (see takeBack), so a nil, foreign or repeated release panics
// exactly, at any pool size, instead of duplicating an entry and letting two
// callers share one Instance.
//
// A dead instance is discarded instead of pooled — the pool's first invariant.
// wazero's WithCloseOnContextDone interrupts a call whose context expires by
// closing the whole module, so a timed-out call leaves scrap behind; pooling
// it would fail every subsequent caller that drew that slot. Discarding means
// closing it as well as dropping it, not merely dropping it: some failures
// (a guest trap, an unreadable result) mark an Instance dead while leaving its
// module open, so a corpse that was never closed would leak wazero resources.
// The slot itself survives — it goes back empty, and the next acquire builds a
// replacement.
//
// Dead is the authority for that decision, not the error the caller saw:
// Instance documents that Invoke on a dead instance is not guaranteed to fail,
// and that a module wazero closed on context completion can look like a
// successful call.
//
// The discard closes with context.Background(): release takes no context by
// design, and the context that killed the call is exactly the wrong one to
// close with — an already-cancelled context makes wazero refuse the close and
// leak the module. Because release cannot return an error either, a failing
// close is recorded on the pool and joined onto drain's result rather than
// swallowed.
func (p *pool) release(inst *Instance) {
	p.takeBack(inst)

	slot := inst
	if inst.Dead() {
		slot = nil
		if err := closeInstance(context.Background(), inst); err != nil {
			p.recordDisposeErr(fmt.Errorf("close discarded plugin instance: %w", err))
		}
	}

	// The slot must be back in free BEFORE inflight drops, because a drain
	// woken by inflight reaching zero goes straight on to collect every slot
	// and would otherwise block on a short channel.
	//
	// The send cannot block: takeBack above proved this instance held one of
	// free's entries, and that entry is the one being returned here. A full
	// channel would therefore mean the pool's entry conservation is broken —
	// entries created or duplicated somewhere — so it panics instead of
	// blocking forever and turning a bug into a hang.
	select {
	case p.free <- slot:
	default:
		panic("host: pool.release: free is full although this instance held a slot; pool entry accounting is corrupt")
	}
	p.inflight.Done()
}

// takeBack removes inst from the checked-out set, and panics if it was not
// there. Absence is the exact test for every way release can be misused: a nil
// instance, an instance this pool never handed out, and a second release of one
// already handed back all fail it, whatever the pool's size and whatever else
// is checked out at the time.
//
// Panicking is the point. Each of those is a caller bug that would otherwise
// duplicate an entry in free, so two later acquires would hand one Instance to
// two goroutines and inflight would reach zero while a call was still running —
// letting drain close an instance a caller is inside. Corrupting the pool
// quietly is strictly worse than stopping here.
func (p *pool) takeBack(inst *Instance) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.checkedOut[inst]; !ok {
		panic(fmt.Sprintf("host: pool.release: instance %p is not checked out of this pool; "+
			"release must be called exactly once per successful acquire, with the instance acquire returned", inst))
	}
	delete(p.checkedOut, inst)
}

// recordDisposeErr stores a close failure that had nowhere to be returned, for
// drain to report.
func (p *pool) recordDisposeErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.disposeErrs = append(p.disposeErrs, err)
}

// drain shuts the pool down and returns only once its in-flight calls have
// reached zero — the pool's second invariant. In order:
//
//  1. the pool is marked closing, so a new acquire is rejected immediately
//     (an error, not a wait), and closed is closed, so a caller already queued
//     for a slot is woken and rejected too;
//  2. drain waits for every caller between acquire and release to finish. It
//     does not interrupt them: a checked-out instance may be inside a guest
//     call, and closing the module underneath it would turn an in-flight tool
//     call into a truncated one;
//  3. every pooled instance is closed. Because in-flight is zero and no new
//     acquire can start, every slot is back in free by now, so collecting all
//     of them cannot block — and a slot that is missing all the same is a
//     broken invariant, which drain reports by panicking rather than by
//     waiting forever for it.
//
// Errors are reported, never swallowed: each failing close is joined, together
// with any failure release could not return (see recordDisposeErr), into
// drain's single error.
//
// If ctx is cancelled or expires while waiting at step 2, drain gives up and
// returns that as an error rather than waiting for a caller that may never
// come back. The pool stays closed — it is not reopened by a failed drain —
// and the instances still checked out are NOT closed by it; releasing the
// wazero Runtime they belong to (which Activate files in the lifecycle ledger)
// is what reclaims those.
//
// drain must be called at most once. A second call returns
// errDrainAlreadyStarted: the convergence belongs to the first call, so
// answering "drained" without having waited would be a guess, and closing an
// already-closed channel would panic. It is a sentinel so a caller can tell
// that case from a convergence that was really attempted and failed.
func (p *pool) drain(ctx context.Context) error {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return fmt.Errorf("drain plugin instance pool: %w; drain must be called at most once, "+
			"and only its first caller waits for in-flight calls", errDrainAlreadyStarted)
	}
	p.closing = true
	close(p.closed)
	p.mu.Unlock()

	// sync.WaitGroup cannot be selected on, so the wait runs in a goroutine.
	// It outlives a ctx-cancelled drain and ends whenever the last straggler
	// releases; it holds nothing but the WaitGroup.
	converged := make(chan struct{})
	go func() {
		p.inflight.Wait()
		close(converged)
	}()

	select {
	case <-converged:
	case <-ctx.Done():
		return p.drainResult(fmt.Errorf("wait for in-flight plugin calls: %w", ctx.Err()))
	}

	// The closes below run with cancellation scrubbed: the wait above is what
	// ctx bounds, and a ctx that dies between the wait and here must not turn
	// a converged drain into leaked wazero modules — wazero refuses work on a
	// dead context, so the close would fail on arrival and release nothing.
	closeCtx := context.WithoutCancel(ctx)

	var errs []error
	for i := 0; i < p.size; i++ {
		// Cannot block: in-flight is zero and closing bars new callers, so all
		// p.size entries are in the channel. A missing entry would mean that
		// premise is broken — an entry lost, or an in-flight caller never
		// counted — and waiting for it would hang shutdown with no diagnostic,
		// which is the worst way for a pool to fail. So it says so and stops,
		// the same stance release takes on the sending side.
		var inst *Instance
		select {
		case inst = <-p.free:
		default:
			panic(fmt.Sprintf("host: pool.drain: only %d of %d slots came back after in-flight reached zero; "+
				"pool entry accounting is corrupt", i, p.size))
		}
		if inst == nil {
			continue
		}
		if err := closeInstance(closeCtx, inst); err != nil {
			errs = append(errs, fmt.Errorf("close pooled plugin instance: %w", err))
		}
	}
	return p.drainResult(errs...)
}

// drainResult builds drain's return value: its own failures joined with the
// ones release could not return, cleared as they are taken so each is
// reported exactly once. A failure recorded by a straggler that releases after
// a ctx-cancelled drain has already returned has nowhere left to go, which is
// one more reason such a drain is a reported failure rather than a shrug.
func (p *pool) drainResult(errs ...error) error {
	p.mu.Lock()
	errs = append(errs, p.disposeErrs...)
	p.disposeErrs = nil
	p.mu.Unlock()

	if joined := errors.Join(errs...); joined != nil {
		return fmt.Errorf("drain plugin instance pool: %w", joined)
	}
	return nil
}
