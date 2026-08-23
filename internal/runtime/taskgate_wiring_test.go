package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// hookMaas answers every request with "done" after running hook, so a test can
// observe the gate from INSIDE a running task — the only moment at which "a
// task is in flight" is true and testable without sleeping.
type hookMaas struct {
	hook func()
}

func (m hookMaas) Generate(context.Context, port.InferenceRequest) (port.InferenceResponse, error) {
	if m.hook != nil {
		m.hook()
	}
	return port.InferenceResponse{Text: "done"}, nil
}

// gatedRuntime builds a Runtime on gate with a maas that runs hook mid-task.
func gatedRuntime(gate *taskgate.TaskGate, maas port.MaasInferenceClient, events port.EventBus) *Runtime {
	return NewRuntime(Config{
		Maas:   maas,
		Audit:  adapter.NewMemoryAuditLog(),
		Events: events,
		Gate:   gate,
	})
}

func gateTestTask(id string) domain.Task {
	return domain.Task{
		ID:        id,
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Status:    domain.TaskRunning,
		Input:     "say done",
	}
}

func gateTestAgent() domain.Agent {
	return domain.Agent{ID: "agent-1", CompanyID: "company-1", Role: "developer", Status: domain.AgentActive}
}

// TestNewRuntimeRejectsNilGate locks decision 2: a nil Gate is a wiring error,
// not "gate disabled". A permissive default would turn the task-boundary
// contract into a protection that can be switched off by forgetting a field.
func TestNewRuntimeRejectsNilGate(t *testing.T) {
	t.Parallel()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("NewRuntime() with a nil Gate did not panic; a missing gate must fail loud")
		}
		msg, ok := recovered.(string)
		if !ok {
			t.Fatalf("NewRuntime() panicked with %T (%v), want a string naming the field", recovered, recovered)
		}
		if !strings.Contains(msg, "Gate") {
			t.Fatalf("NewRuntime() panic = %q, want it to name Config.Gate", msg)
		}
	}()

	NewRuntime(Config{
		Maas:   adapter.NewRecordingMaas("done"),
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
	})
}

// TestNewAgentRuntimeResolverRejectsNilGate holds the same line for the
// per-agent runtimes: they run tasks through the same RunTask, so a resolver
// without a gate would be a hole in the contract exactly where per-agent tasks
// run.
func TestNewAgentRuntimeResolverRejectsNilGate(t *testing.T) {
	t.Parallel()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("NewAgentRuntimeResolver() with a nil Gate did not panic; a missing gate must fail loud")
		}
		msg, ok := recovered.(string)
		if !ok {
			t.Fatalf("NewAgentRuntimeResolver() panicked with %T (%v), want a string naming the field", recovered, recovered)
		}
		if !strings.Contains(msg, "Gate") {
			t.Fatalf("NewAgentRuntimeResolver() panic = %q, want it to name Config.Gate", msg)
		}
	}()

	NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
	})
}

// TestRunTaskRefusesWhileApplyPending covers the second half of contract 4: a
// task that would start into a half-changed plugin set must not start at all,
// and must say so in a way the caller can act on (errors.Is taskgate.ErrApplyPending).
//
// The check runs INSIDE ApplyAtBoundary's fn, which is exactly the window the
// gate protects and needs no synchronisation of its own: while fn runs, the
// apply is pending by construction.
func TestRunTaskRefusesWhileApplyPending(t *testing.T) {
	t.Parallel()

	gate := taskgate.NewTaskGate()
	maas := adapter.NewRecordingMaas("done")
	events := adapter.NewMemoryEventBus()
	runner := gatedRuntime(gate, maas, events)

	var runErr error
	applyErr := gate.ApplyAtBoundary(context.Background(), 0, func() error {
		_, runErr = runner.RunTask(context.Background(), gateTestAgent(), gateTestTask("task-refused"))
		return nil
	})
	if applyErr != nil {
		t.Fatalf("ApplyAtBoundary() error = %v, want nil (no task was in flight)", applyErr)
	}
	if !errors.Is(runErr, taskgate.ErrApplyPending) {
		t.Fatalf("RunTask() error = %v, want one matching taskgate.ErrApplyPending", runErr)
	}
	if !strings.Contains(runErr.Error(), "task-refused") {
		t.Errorf("RunTask() error = %q, want it to name the task it refused", runErr)
	}

	// The refusal must come before the task does anything: an inference call or a
	// task_started event would mean the task really did start against a catalog
	// that was being replaced.
	if maas.CallCount() != 0 {
		t.Errorf("MaasInferenceClient calls = %d, want 0; the refused task must not run", maas.CallCount())
	}
	for _, event := range mustRuntimeEvents(t, events) {
		if event.Type == "task_started" {
			t.Errorf("published %q for a task the gate refused: %#v", event.Type, event)
		}
	}
}

// TestRunTaskHoldsGateWhileRunning covers the first half of contract 4 at the
// runtime seam: while a task runs, an apply cannot reach its fn.
//
// It proves ONLY the runtime half of the chain — that a real RunTask registers
// with the gate and retires it — against an apply that is the gate's own
// ApplyAtBoundary. That the Loader routes its convergence through that same
// call, so a real plugin change lands only at a boundary, is the other half and
// is proved by TestApplyLandsOnlyAtTaskBoundary in
// internal/plugin/loader/boundary_test.go. Neither test implies the other:
// delete either one and the survivor still passes with the chain broken, so
// they must be maintained as a pair.
func TestRunTaskHoldsGateWhileRunning(t *testing.T) {
	t.Parallel()

	gate := taskgate.NewTaskGate()
	var (
		midTaskErr     error
		midTaskApplied bool
	)
	runner := gatedRuntime(gate, hookMaas{hook: func() {
		// wait <= 0 means "only if the gate is idle right now", so this returns
		// immediately either way — no sleeping, no chance of hanging.
		midTaskErr = gate.ApplyAtBoundary(context.Background(), 0, func() error {
			midTaskApplied = true
			return nil
		})
	}}, adapter.NewMemoryEventBus())

	if _, err := runner.RunTask(context.Background(), gateTestAgent(), gateTestTask("task-holds-gate")); err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if midTaskErr == nil {
		t.Fatalf("ApplyAtBoundary() during a running task returned nil, want an error; the task was in flight")
	}
	if midTaskApplied {
		t.Fatalf("ApplyAtBoundary() ran fn while a task was in flight; contract 4 forbids it")
	}

	// The task is over, so the boundary has been reached: the same call must now
	// succeed, which is what proves RunTask retires its Begin.
	applied := false
	if err := gate.ApplyAtBoundary(context.Background(), 0, func() error {
		applied = true
		return nil
	}); err != nil {
		t.Fatalf("ApplyAtBoundary() after RunTask returned error = %v, want nil; RunTask left the gate closed", err)
	}
	if !applied {
		t.Fatalf("ApplyAtBoundary() after RunTask did not run fn")
	}
}

// TestRunTaskReleasesGateWhenItFails is the error-path twin of the test above:
// a task that fails still reaches a boundary. A gate that only reopened on
// success would wedge every later apply behind one failed task.
func TestRunTaskReleasesGateWhenItFails(t *testing.T) {
	t.Parallel()

	gate := taskgate.NewTaskGate()
	runner := gatedRuntime(gate, failingMaas{}, adapter.NewMemoryEventBus())

	if _, err := runner.RunTask(context.Background(), gateTestAgent(), gateTestTask("task-fails")); err == nil {
		t.Fatalf("RunTask() error = nil, want the inference failure")
	}

	applied := false
	if err := gate.ApplyAtBoundary(context.Background(), 0, func() error {
		applied = true
		return nil
	}); err != nil {
		t.Fatalf("ApplyAtBoundary() after a failed RunTask error = %v, want nil; the gate stayed closed", err)
	}
	if !applied {
		t.Fatalf("ApplyAtBoundary() after a failed RunTask did not run fn")
	}
}

// TestSubRuntimeInheritsGate locks the delegation seam. A child built by
// newSubRuntime bypasses NewRuntime, so nothing else would catch a child with
// no gate (a nil dereference in RunTask) or — worse — a child with a gate of
// its own, which would let a delegated sub-task start in the middle of an apply
// that the shared gate believed it had held shut.
func TestSubRuntimeInheritsGate(t *testing.T) {
	t.Parallel()

	gate := taskgate.NewTaskGate()
	parent := gatedRuntime(gate, adapter.NewRecordingMaas("done"), adapter.NewMemoryEventBus())
	child, err := parent.newSubRuntime(roleLeaf, nil)
	if err != nil {
		t.Fatalf("newSubRuntime() error = %v, want nil", err)
	}
	if child.gate != gate {
		t.Fatalf("child gate = %p, want the parent's gate %p", child.gate, gate)
	}

	// Behavioural check: the child's own run has to register on that same gate.
	var midTaskApplied bool
	child.maas = hookMaas{hook: func() {
		_ = gate.ApplyAtBoundary(context.Background(), 0, func() error {
			midTaskApplied = true
			return nil
		})
	}}
	if _, err := child.RunTask(context.Background(), gateTestAgent(), gateTestTask("task-child")); err != nil {
		t.Fatalf("child RunTask() error = %v, want nil", err)
	}
	if midTaskApplied {
		t.Fatalf("an apply ran fn while a delegated sub-task was in flight; the child did not hold the shared gate")
	}
}

// The bounds every wait added below is held to. Reaching one FAILS the test —
// nothing here waits on an unbounded channel, and every poll loop checks an
// explicit deadline.
const (
	// gatePendingBound is how long a test waits for a backgrounded apply to
	// claim the gate.
	gatePendingBound = 5 * time.Second
	// gateApplyBound is how long a test waits for a backgrounded apply or a
	// background sub-task to finish once it has been let go. It is well under
	// the suite's -timeout so a wedged gate is reported as a failed assertion
	// rather than as a killed test binary.
	gateApplyBound = 30 * time.Second
	// gatePollInterval is the gap between two probes of the gate.
	gatePollInterval = 5 * time.Millisecond
)

// awaitApplyPending blocks until an apply owns gate, and fails the test if none
// does within gatePendingBound. It probes with Begin, so its success condition
// is the gate's own refusal — the apply is demonstrably pending, not merely
// started.
//
// It must be called from the test's own goroutine (it calls t.Fatalf).
func awaitApplyPending(t *testing.T, gate *taskgate.TaskGate) {
	t.Helper()

	deadline := time.Now().Add(gatePendingBound)
	for {
		end, err := gate.Begin()
		if err != nil {
			if !errors.Is(err, taskgate.ErrApplyPending) {
				t.Fatalf("Begin() error = %v, want one matching taskgate.ErrApplyPending", err)
			}
			return
		}
		end()
		if time.Now().After(deadline) {
			t.Fatalf("no apply claimed the gate within %s", gatePendingBound)
		}
		time.Sleep(gatePollInterval)
	}
}

// awaitGateIdle blocks until an apply can reach its fn, and fails the test if
// that has not happened within gateApplyBound. The fn it probes with is a
// no-op — no plugin instance is created by this loop, whatever number of rounds
// it takes.
func awaitGateIdle(t *testing.T, gate *taskgate.TaskGate) {
	t.Helper()

	deadline := time.Now().Add(gateApplyBound)
	for {
		applied := false
		err := gate.ApplyAtBoundary(context.Background(), 0, func() error {
			applied = true
			return nil
		})
		if err == nil {
			if !applied {
				t.Fatalf("ApplyAtBoundary() returned nil without running fn")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the gate never reopened within %s: %v", gateApplyBound, err)
		}
		time.Sleep(gatePollInterval)
	}
}

// TestDelegatedSubTaskRunsWhileApplyPending pins the one in-flight shape the
// gate must NOT turn away: a sub-task delegated by a task that is already
// running, while an apply waits for that very task's boundary.
//
// The parent started before the apply was requested, so contract 4 promises it
// the catalog it started with — including the tools its delegation will use. A
// gate that refused the child would break the task it is holding the apply for,
// and the parent could do nothing about it: the failure would surface as a
// delegate_task error the model has to reason about.
//
// Everything happens inside the parent's inference hook, which is the only
// moment "the parent is in flight" is true and testable without sleeping. The
// only wait is awaitApplyPending's bounded poll.
func TestDelegatedSubTaskRunsWhileApplyPending(t *testing.T) {
	t.Parallel()

	gate := taskgate.NewTaskGate()
	applyDone := make(chan error, 1)

	var (
		parent   *Runtime
		spawned  atomic.Bool
		subRes   SubTaskResult
		subErr   error
		applyErr error
	)
	maas := hookMaas{hook: func() {
		// Only the parent's own inference arranges this; the child runs on the
		// same maas and must find the latch already taken. It has to be a CAS
		// and not a sync.Once: the child's inference is reached from INSIDE this
		// closure, and Once.Do blocks a re-entrant caller forever.
		if !spawned.CompareAndSwap(false, true) {
			return
		}
		go func() {
			applyDone <- gate.ApplyAtBoundary(context.Background(), gateApplyBound, func() error { return nil })
		}()
		// The apply is now waiting for THIS task to end. A child that consulted
		// the pending flag would be refused from here on.
		awaitApplyPending(t, gate)
		subRes, subErr = parent.RunSubTask(context.Background(), SubTaskSpec{
			ParentTaskID: "task-parent",
			Goal:         "do the child half of the parent work",
		})
	}}
	parent = gatedRuntime(gate, maas, adapter.NewMemoryEventBus())

	if _, err := parent.RunTask(context.Background(), gateTestAgent(), gateTestTask("task-parent")); err != nil {
		t.Fatalf("parent RunTask() error = %v, want nil", err)
	}
	if !spawned.Load() {
		t.Fatalf("the parent inference hook never ran; the test asserted nothing")
	}
	if subErr != nil {
		t.Fatalf("RunSubTask() while an apply was pending error = %v, want nil; "+
			"a child of an in-flight task must not be refused", subErr)
	}
	if subRes.Summary != "done" {
		t.Errorf("RunSubTask() summary = %q, want %q", subRes.Summary, "done")
	}

	// The apply was only ever delayed, never lost: both the parent and its child
	// have ended, so it reaches its boundary.
	select {
	case applyErr = <-applyDone:
	case <-time.After(gateApplyBound):
		t.Fatalf("the pending apply did not return within %s of the parent task ending", gateApplyBound)
	}
	if applyErr != nil {
		t.Fatalf("ApplyAtBoundary() error = %v, want nil once the parent and its child were over", applyErr)
	}
}

// blockingEventBus is a bus that holds every "subtask_completed" event until
// release is closed, and reports on entered that it is holding one. That opens
// a controllable window AFTER a background sub-task's own RunTask has ended and
// retired its own gate registration, but BEFORE the background work is over —
// the window only RunSubTaskAsync's own token covers.
//
// Its wait is bounded: if a test fails before releasing it, it gives up and
// reports the failure instead of parking the goroutine forever.
type blockingEventBus struct {
	inner   port.EventBus
	entered chan struct{}
	release chan struct{}
}

func (b *blockingEventBus) Publish(ctx context.Context, event domain.RuntimeEvent) error {
	if event.Type == "subtask_completed" {
		select {
		case b.entered <- struct{}{}:
		default:
		}
		select {
		case <-b.release:
		case <-time.After(gateApplyBound):
			return fmt.Errorf("blocking event bus: %q was never released within %s", event.Type, gateApplyBound)
		}
	}
	return b.inner.Publish(ctx, event)
}

func (b *blockingEventBus) Events() ([]domain.RuntimeEvent, error) { return b.inner.Events() }

// TestBackgroundSubTaskHoldsGateAfterItsParentReturns covers the other half of
// the same decision. RunSubTaskAsync runs on a detached context and can outlive
// the parent task that started it, so exempting children by depth alone would
// leave the background work uncounted the moment the parent retired — and an
// apply would land on top of a task that is still running, which is precisely
// what this gate exists to prevent.
//
// The window is made deterministic rather than raced for: the sub-task's own
// RunTask has finished (its own registration is gone) and the goroutine is held
// inside the completion publish. Whatever still holds the gate at that point
// can only be the token RunSubTaskAsync took at spawn time.
func TestBackgroundSubTaskHoldsGateAfterItsParentReturns(t *testing.T) {
	t.Parallel()

	gate := taskgate.NewTaskGate()
	bus := &blockingEventBus{
		inner:   adapter.NewMemoryEventBus(),
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	// Registered before anything below can fail, so an early t.Fatalf still lets
	// the background goroutine finish instead of leaving it parked.
	releaseOnce := sync.OnceFunc(func() { close(bus.release) })
	t.Cleanup(releaseOnce)

	var (
		parent   *Runtime
		spawned  atomic.Bool
		spawnErr error
	)
	maas := hookMaas{hook: func() {
		// A CAS, not a sync.Once: the background child runs on this same maas
		// from another goroutine, and it must fall straight through rather than
		// block behind the parent's turn.
		if !spawned.CompareAndSwap(false, true) {
			return
		}
		_, spawnErr = parent.RunSubTaskAsync(context.Background(), SubTaskSpec{
			ParentTaskID: "task-parent",
			Goal:         "keep working after the parent has returned",
		})
	}}
	parent = gatedRuntime(gate, maas, bus)

	if _, err := parent.RunTask(context.Background(), gateTestAgent(), gateTestTask("task-parent")); err != nil {
		t.Fatalf("parent RunTask() error = %v, want nil", err)
	}
	if !spawned.Load() {
		t.Fatalf("the parent inference hook never ran; the test asserted nothing")
	}
	if spawnErr != nil {
		t.Fatalf("RunSubTaskAsync() error = %v, want nil", spawnErr)
	}

	select {
	case <-bus.entered:
	case <-time.After(gateApplyBound):
		t.Fatalf("the background sub-task did not reach its completion event within %s", gateApplyBound)
	}

	// The parent is long gone and the child's own RunTask is over. wait <= 0
	// means "only if the gate is idle right now", so this returns immediately
	// either way.
	applied := false
	if err := gate.ApplyAtBoundary(context.Background(), 0, func() error {
		applied = true
		return nil
	}); err == nil {
		t.Fatalf("ApplyAtBoundary() while a background sub-task was still running returned nil, want an error")
	}
	if applied {
		t.Fatalf("an apply ran fn while a background sub-task outlived its parent; " +
			"the token taken at spawn time was not held for the whole of it")
	}

	// Releasing it must reopen the gate — a token that leaked would wedge the
	// gate shut against every later apply, which is the opposite failure.
	releaseOnce()
	awaitGateIdle(t, gate)
}
