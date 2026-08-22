package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
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
func gatedRuntime(gate *TaskGate, maas port.MaasInferenceClient, events port.EventBus) *Runtime {
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
// and must say so in a way the caller can act on (errors.Is ErrApplyPending).
//
// The check runs INSIDE ApplyAtBoundary's fn, which is exactly the window the
// gate protects and needs no synchronisation of its own: while fn runs, the
// apply is pending by construction.
func TestRunTaskRefusesWhileApplyPending(t *testing.T) {
	t.Parallel()

	gate := NewTaskGate()
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
	if !errors.Is(runErr, ErrApplyPending) {
		t.Fatalf("RunTask() error = %v, want one matching ErrApplyPending", runErr)
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
func TestRunTaskHoldsGateWhileRunning(t *testing.T) {
	t.Parallel()

	gate := NewTaskGate()
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

	gate := NewTaskGate()
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

	gate := NewTaskGate()
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
