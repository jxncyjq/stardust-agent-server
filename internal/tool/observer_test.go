package tool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
)

// The observer seam is the narrowest of the plugin extension points: it is
// notified after a call answered, and it can change nothing. These tests pin
// both halves — that it IS notified, and that everything it might want to
// influence is out of its reach.

// observingRegistry builds a registry that allows one tool and records what
// its observers see.
func observingRegistry(t *testing.T, handler Handler) *Registry {
	t.Helper()

	registry := NewRegistry(
		NewStaticPolicy(DecisionAllow),
		NewRolePermissionEnforcer(map[string]bool{"developer:demo_tool": true}),
		NoopGuardrails{},
	)
	registry.Register("demo_tool", handler)
	return registry
}

// observedHandler answers with a distinctive result, so a test can prove the
// caller received exactly what the tool produced. (registry_revoke_test.go
// already has an okHandler; this one exists because these tests assert on the
// output text.)
func observedHandler() Handler {
	return HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: "c1", Success: true, Output: "the real output"}, nil
	})
}

func demoCall() domain.ToolCall {
	return domain.ToolCall{ID: "c1", Name: "demo_tool"}
}

func developer() domain.Agent {
	return domain.Agent{ID: "agent-1", Role: "developer"}
}

func TestObserversSeeACompletedCall(t *testing.T) {
	t.Parallel()

	registry := observingRegistry(t, observedHandler())
	var seen []domain.ToolResult
	registry.AddObserver("test", ObserverFunc(func(_ context.Context, _ domain.ToolCall, result domain.ToolResult) {
		seen = append(seen, result)
	}))

	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(seen) != 1 || seen[0].Output != "the real output" {
		t.Errorf("observer saw %+v, want one result carrying the tool's output", seen)
	}
}

// TestObserversSeeAFailedToolResult: a tool that answers "no" ran and
// answered. Hiding that from observers would make the seam report only the
// happy path, which is the half nobody needs to watch.
func TestObserversSeeAFailedToolResult(t *testing.T) {
	t.Parallel()

	registry := observingRegistry(t, HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: "c1", Success: false, Error: "nope"}, nil
	}))
	var seen int
	registry.AddObserver("test", ObserverFunc(func(_ context.Context, _ domain.ToolCall, result domain.ToolResult) {
		seen++
		if result.Success {
			t.Errorf("observer saw success = true for a failed result")
		}
	}))

	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seen != 1 {
		t.Errorf("observer notified %d times, want 1", seen)
	}
}

// TestObserversDoNotSeeARefusedCall pins the exclusion that matters most: a
// call the enforcer refused never happened, and reporting it would tell an
// observer about an execution that did not occur.
func TestObserversDoNotSeeARefusedCall(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(
		NewStaticPolicy(DecisionAllow),
		NewRolePermissionEnforcer(map[string]bool{}), // nothing permitted
		NoopGuardrails{},
	)
	registry.Register("demo_tool", observedHandler())
	notified := false
	registry.AddObserver("test", ObserverFunc(func(context.Context, domain.ToolCall, domain.ToolResult) {
		notified = true
	}))

	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err == nil {
		t.Fatal("Execute with no permission = nil error, want a refusal")
	}
	if notified {
		t.Error("an observer was told about a call that was refused before it ran")
	}
}

// TestObserversDoNotSeeAHandlerError: a Go error is a fault in the host or the
// tool, not an answer. It is already in the audit trail as tool_failed.
func TestObserversDoNotSeeAHandlerError(t *testing.T) {
	t.Parallel()

	registry := observingRegistry(t, HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, errors.New("boom")
	}))
	notified := false
	registry.AddObserver("test", ObserverFunc(func(context.Context, domain.ToolCall, domain.ToolResult) {
		notified = true
	}))

	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err == nil {
		t.Fatal("Execute with a failing handler = nil error, want the error")
	}
	if notified {
		t.Error("an observer was told about a handler error, which is a fault rather than an answer")
	}
}

// TestObserverCannotChangeTheResult is the whole safety claim of this seam,
// stated as a test: the observer receives a COPY, and whatever it does with it
// never reaches the caller.
func TestObserverCannotChangeTheResult(t *testing.T) {
	t.Parallel()

	registry := observingRegistry(t, observedHandler())
	registry.AddObserver("test", ObserverFunc(func(_ context.Context, _ domain.ToolCall, result domain.ToolResult) {
		result.Output = "tampered"
		result.Success = false
	}))

	got, err := registry.Execute(context.Background(), developer(), demoCall())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.Output != "the real output" || !got.Success {
		t.Errorf("Execute returned %+v; an observer changed the caller's result", got)
	}
}

func TestRevokingAnObserverStopsNotifications(t *testing.T) {
	t.Parallel()

	registry := observingRegistry(t, observedHandler())
	count := 0
	revoke := registry.AddObserver("test", ObserverFunc(func(context.Context, domain.ToolCall, domain.ToolResult) {
		count++
	}))

	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	revoke()
	revoke() // idempotent: a stale cleanup must not remove somebody else's observer
	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if count != 1 {
		t.Errorf("observer notified %d times, want 1 (once before revocation)", count)
	}
}

// TestObserversOnAParentSeeCallsThroughAScopedView: a per-agent view executes
// handlers that live on the parent. An observer registered where the plugin
// was mounted must see those, or "this plugin observes tool calls" would
// quietly mean "…except the ones made through a scoped view".
func TestObserversOnAParentSeeCallsThroughAScopedView(t *testing.T) {
	t.Parallel()

	parent := observingRegistry(t, observedHandler())
	notified := 0
	parent.AddObserver("test", ObserverFunc(func(context.Context, domain.ToolCall, domain.ToolResult) {
		notified++
	}))

	view := parent.Subset("demo_tool")
	if _, err := view.Execute(context.Background(), developer(), demoCall()); err != nil {
		t.Fatalf("Execute through the view: %v", err)
	}
	if notified != 1 {
		t.Errorf("parent observer notified %d times for a call through a scoped view, want 1", notified)
	}
}

// TestObserverMayRegisterDuringItsOwnNotification is the deadlock guard: the
// notification runs with the registry lock RELEASED, so an observer that
// registers or revokes while being notified — a plugin unloading itself, say —
// completes instead of hanging.
func TestObserverMayRegisterDuringItsOwnNotification(t *testing.T) {
	t.Parallel()

	registry := observingRegistry(t, observedHandler())
	var once sync.Once
	registry.AddObserver("reentrant", ObserverFunc(func(context.Context, domain.ToolCall, domain.ToolResult) {
		once.Do(func() {
			registry.AddObserver("added-from-inside", ObserverFunc(
				func(context.Context, domain.ToolCall, domain.ToolResult) {}))()
		})
	}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := registry.Execute(context.Background(), developer(), demoCall()); err != nil {
			t.Errorf("Execute: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return within 5s: the observer notification is holding the registry lock, " +
			"so an observer that registers or revokes during its own notification deadlocks")
	}
}

func TestObserveOwnedRevokesWithTheOwner(t *testing.T) {
	t.Parallel()

	registry := observingRegistry(t, observedHandler())
	ledger := lifecycle.NewLedger()
	owner := lifecycle.Owner("plugin:demo")
	count := 0
	ObserveOwned(ledger, owner, registry, "plugin:demo", ObserverFunc(
		func(context.Context, domain.ToolCall, domain.ToolResult) { count++ }))

	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := ledger.DisposeOwner(owner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}
	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err != nil {
		t.Fatalf("Execute after disposal: %v", err)
	}
	if count != 1 {
		t.Errorf("observer notified %d times, want 1: disposing the owner must take it off the seam", count)
	}
}
