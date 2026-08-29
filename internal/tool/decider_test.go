package tool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
)

// The decider seam is the first extension point that can REFUSE a call. Every
// test here exists to pin one half of the same sentence: a decider may
// tighten, and a decider may never widen.

// decidingRegistry builds a registry that permits one tool and records
// whether its handler ran.
func decidingRegistry(t *testing.T, policy Policy, ran *bool) *Registry {
	t.Helper()

	registry := NewRegistry(
		policy,
		NewRolePermissionEnforcer(map[string]bool{"developer:demo_tool": true}),
		NoopGuardrails{},
	)
	registry.Register("demo_tool", HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		if ran != nil {
			*ran = true
		}
		return domain.ToolResult{CallID: "c1", Success: true, Output: "the real output"}, nil
	}))
	return registry
}

func denyingDecider(reason string) Decider {
	return DeciderFunc(func(context.Context, domain.ToolCall) Verdict {
		return Verdict{Decision: DecisionDeny, Reason: reason}
	})
}

func allowingDecider() Decider {
	return DeciderFunc(func(context.Context, domain.ToolCall) Verdict {
		return Verdict{Decision: DecisionAllow}
	})
}

func TestADeciderThatAllowsLetsTheCallThrough(t *testing.T) {
	t.Parallel()

	ran := false
	registry := decidingRegistry(t, NewStaticPolicy(DecisionAllow), &ran)
	registry.AddDecider("plugin:demo", allowingDecider())

	got, err := registry.Execute(context.Background(), developer(), demoCall())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !ran || got.Output != "the real output" {
		t.Errorf("handler ran = %t, result = %+v; an allowing decider must change nothing", ran, got)
	}
}

// TestADeciderThatDeniesStopsTheCallBeforeItRuns: the refusal must land
// BEFORE dispatch. A decision point that only reported afterwards would be an
// observer with a misleading name.
func TestADeciderThatDeniesStopsTheCallBeforeItRuns(t *testing.T) {
	t.Parallel()

	ran := false
	registry := decidingRegistry(t, NewStaticPolicy(DecisionAllow), &ran)
	registry.AddDecider("plugin:demo", denyingDecider("writes are frozen during the incident"))

	_, err := registry.Execute(context.Background(), developer(), demoCall())
	if err == nil {
		t.Fatal("Execute with a denying decider = nil error, want a refusal")
	}
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("error = %v, want it to be ErrPermissionDenied: callers select on that", err)
	}
	if !strings.Contains(err.Error(), "plugin:demo") {
		t.Errorf("error = %v, want it to name WHO refused", err)
	}
	if !strings.Contains(err.Error(), "writes are frozen during the incident") {
		t.Errorf("error = %v, want it to carry the reason the decider gave", err)
	}
	if ran {
		t.Error("the handler ran for a call a decider refused")
	}
}

// TestADeciderIsNotConsultedForACallThePolicyAlreadyRefused is the structural
// half of "can only tighten": a plugin is never even shown a call the host
// decided against, so there is no position from which it could widen one.
func TestADeciderIsNotConsultedForACallThePolicyAlreadyRefused(t *testing.T) {
	t.Parallel()

	registry := decidingRegistry(t, NewStaticPolicy(DecisionDeny), nil)
	consulted := false
	registry.AddDecider("plugin:demo", DeciderFunc(func(context.Context, domain.ToolCall) Verdict {
		consulted = true
		return Verdict{Decision: DecisionAllow} // a plugin trying to widen
	}))

	if _, err := registry.Execute(context.Background(), developer(), demoCall()); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("Execute = %v, want the policy's refusal to stand", err)
	}
	if consulted {
		t.Error("a decider was consulted about a call the host policy had already refused")
	}
}

func TestADeciderIsNotConsultedForACallTheEnforcerRefused(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(
		NewStaticPolicy(DecisionAllow),
		NewRolePermissionEnforcer(map[string]bool{}), // nothing permitted
		NoopGuardrails{},
	)
	registry.Register("demo_tool", observedHandler())
	consulted := false
	registry.AddDecider("plugin:demo", DeciderFunc(func(context.Context, domain.ToolCall) Verdict {
		consulted = true
		return Verdict{Decision: DecisionAllow}
	}))

	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err == nil {
		t.Fatal("Execute with no permission = nil error, want a refusal")
	}
	if consulted {
		t.Error("a decider was consulted about a call the enforcer refused")
	}
}

// TestOneDenyAmongManyDecidersRefusesTheCall: the composition rule is
// STRICTEST WINS, and it must not depend on registration order.
func TestOneDenyAmongManyDecidersRefusesTheCall(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		order []Decider
	}{
		{name: "deny first", order: []Decider{denyingDecider("no"), allowingDecider()}},
		{name: "deny last", order: []Decider{allowingDecider(), denyingDecider("no")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			registry := decidingRegistry(t, NewStaticPolicy(DecisionAllow), &ran)
			for _, decider := range tc.order {
				registry.AddDecider("plugin:demo", decider)
			}

			if _, err := registry.Execute(context.Background(), developer(), demoCall()); !errors.Is(err, ErrPermissionDenied) {
				t.Fatalf("Execute = %v, want a refusal whichever order the deciders were added in", err)
			}
			if ran {
				t.Error("the handler ran despite one decider refusing")
			}
		})
	}
}

func TestRevokingADeciderStopsConsultation(t *testing.T) {
	t.Parallel()

	registry := decidingRegistry(t, NewStaticPolicy(DecisionAllow), nil)
	revoke := registry.AddDecider("plugin:demo", denyingDecider("no"))

	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err == nil {
		t.Fatal("Execute before revocation = nil error, want the refusal")
	}
	revoke()
	revoke() // idempotent: a stale cleanup must not remove somebody else's decider
	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err != nil {
		t.Errorf("Execute after revocation = %v, want nil: the decider is gone", err)
	}
}

// TestDecidersOnAParentSeeCallsThroughAScopedView: a per-agent view executes
// handlers that live on the parent. A decider registered where the plugin was
// mounted must be consulted for those, or "this plugin can refuse tool calls"
// would quietly mean "…except the ones made through a scoped view" — which is
// the path most agent calls actually take.
func TestDecidersOnAParentSeeCallsThroughAScopedView(t *testing.T) {
	t.Parallel()

	parent := decidingRegistry(t, NewStaticPolicy(DecisionAllow), nil)
	parent.AddDecider("plugin:demo", denyingDecider("no"))

	view := parent.Subset("demo_tool")
	if _, err := view.Execute(context.Background(), developer(), demoCall()); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("Execute through a scoped view = %v, want the parent decider's refusal", err)
	}
}

// TestADeciderGetsABoundedShareOfTheCallBudget: the consultation is spent by
// the tool call that is waiting for it. A decider may have at most a quarter
// of the call's own timeout, and never more than deciderMaxTimeout.
func TestADeciderGetsABoundedShareOfTheCallBudget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		toolTimeout time.Duration
		wantAtMost  time.Duration
	}{
		{name: "short tool", toolTimeout: 400 * time.Millisecond, wantAtMost: 100 * time.Millisecond},
		{name: "long tool", toolTimeout: time.Minute, wantAtMost: deciderMaxTimeout},
		{name: "no declared timeout", toolTimeout: 0, wantAtMost: deciderMaxTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewRegistry(
				NewStaticPolicy(DecisionAllow),
				NewRolePermissionEnforcer(map[string]bool{"developer:demo_tool": true}),
				NoopGuardrails{},
			)
			registry.RegisterDescriptor(Descriptor{
				Name: "demo_tool", Description: "d", Group: "g", RiskLevel: "low", Timeout: tc.toolTimeout,
			}, observedHandler())

			var budget time.Duration
			registry.AddDecider("plugin:demo", DeciderFunc(func(ctx context.Context, _ domain.ToolCall) Verdict {
				if deadline, ok := ctx.Deadline(); ok {
					budget = time.Until(deadline)
				}
				return Verdict{Decision: DecisionAllow}
			}))

			if _, err := registry.Execute(context.Background(), developer(), demoCall()); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if budget <= 0 {
				t.Fatal("the decider ran with no deadline; a slow decider would hang the caller indefinitely")
			}
			if budget > tc.wantAtMost {
				t.Errorf("decider budget = %v, want at most %v", budget, tc.wantAtMost)
			}
		})
	}
}

// TestADeciderMayRegisterDuringItsOwnConsultation is the deadlock guard, same
// as the observer seam's: the consultation runs with the registry lock
// released.
func TestADeciderMayRegisterDuringItsOwnConsultation(t *testing.T) {
	t.Parallel()

	registry := decidingRegistry(t, NewStaticPolicy(DecisionAllow), nil)
	var once sync.Once
	registry.AddDecider("reentrant", DeciderFunc(func(context.Context, domain.ToolCall) Verdict {
		once.Do(func() {
			registry.AddDecider("added-from-inside", allowingDecider())()
		})
		return Verdict{Decision: DecisionAllow}
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
		t.Fatal("Execute did not return within 5s: the decider consultation is holding the registry lock")
	}
}

func TestDecideOwnedRevokesWithTheOwner(t *testing.T) {
	t.Parallel()

	registry := decidingRegistry(t, NewStaticPolicy(DecisionAllow), nil)
	ledger := lifecycle.NewLedger()
	owner := lifecycle.Owner("plugin:demo")
	DecideOwned(ledger, owner, registry, "plugin:demo", denyingDecider("no"))

	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err == nil {
		t.Fatal("Execute before disposal = nil error, want the refusal")
	}
	if err := ledger.DisposeOwner(owner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}
	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err != nil {
		t.Errorf("Execute after disposal = %v, want nil: disposing the owner takes it off the seam", err)
	}
}

// The third answer, ask, is the one a decider cannot deliver by itself: the
// suspend happens at the round boundary, in the ToolGate. What the registry
// owes is the fail-closed half — an ask reaching dispatch may only pass on a
// decision somebody already made.

func askingDecider(reason string) Decider {
	return DeciderFunc(func(context.Context, domain.ToolCall) Verdict {
		return Verdict{Decision: DecisionAsk, Reason: reason}
	})
}

// TestAnAskWithNoArbiterRefusesTheCall: a deployment with no approval
// machinery cannot honour "ask a human", and the safe reading of an
// unanswerable question is no. Degrading it to allow would let a plugin's
// most cautious answer become its most permissive one.
func TestAnAskWithNoArbiterRefusesTheCall(t *testing.T) {
	t.Parallel()

	ran := false
	registry := decidingRegistry(t, NewStaticPolicy(DecisionAllow), &ran)
	registry.AddDecider("plugin:demo", askingDecider("a human should look at this"))

	_, err := registry.Execute(context.Background(), developer(), demoCall())
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("Execute = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "a human should look at this") {
		t.Errorf("error = %v, want it to carry the reason the plugin gave", err)
	}
	if !strings.Contains(err.Error(), "审批") && !strings.Contains(err.Error(), "approval") {
		t.Errorf("error = %v, want it to say an approval was required", err)
	}
	if ran {
		t.Error("the handler ran for a call that required approval nobody could grant")
	}
}

func TestAnApprovedAskLetsTheCallThrough(t *testing.T) {
	t.Parallel()

	ran := false
	registry := decidingRegistry(t, NewStaticPolicy(DecisionAllow), &ran)
	registry.AddDecider("plugin:demo", askingDecider("look at this"))
	registry.SetAskArbiter(AskArbiterFunc(func(context.Context, domain.ToolCall) (bool, error) {
		return true, nil
	}))

	if _, err := registry.Execute(context.Background(), developer(), demoCall()); err != nil {
		t.Fatalf("Execute with an approved ask = %v, want nil", err)
	}
	if !ran {
		t.Error("the handler did not run for an approved call")
	}
}

func TestAnUnapprovedOrFailedAskRefusesTheCall(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		approved bool
		err      error
	}{
		{name: "not approved", approved: false},
		{name: "arbiter failed", err: errors.New("ticket store unreadable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			registry := decidingRegistry(t, NewStaticPolicy(DecisionAllow), &ran)
			registry.AddDecider("plugin:demo", askingDecider("look at this"))
			registry.SetAskArbiter(AskArbiterFunc(func(context.Context, domain.ToolCall) (bool, error) {
				return tc.approved, tc.err
			}))

			if _, err := registry.Execute(context.Background(), developer(), demoCall()); !errors.Is(err, ErrPermissionDenied) {
				t.Fatalf("Execute = %v, want a refusal", err)
			}
			if ran {
				t.Error("the handler ran without an approval")
			}
		})
	}
}

// TestDenyBeatsAskWhicheverOrderTheyWereAdded: the composition rule is
// strictest-wins across three values now, and a deny must never be softened
// into "well, ask somebody" by another plugin's milder answer.
func TestDenyBeatsAskWhicheverOrderTheyWereAdded(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		order []Decider
	}{
		{name: "ask first", order: []Decider{askingDecider("look"), denyingDecider("no")}},
		{name: "deny first", order: []Decider{denyingDecider("no"), askingDecider("look")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := decidingRegistry(t, NewStaticPolicy(DecisionAllow), nil)
			for _, decider := range tc.order {
				registry.AddDecider("plugin:demo", decider)
			}
			// An arbiter that approves everything: if the deny were softened
			// into an ask, this call would go through.
			registry.SetAskArbiter(AskArbiterFunc(func(context.Context, domain.ToolCall) (bool, error) {
				return true, nil
			}))

			_, err := registry.Execute(context.Background(), developer(), demoCall())
			if !errors.Is(err, ErrPermissionDenied) {
				t.Fatalf("Execute = %v, want the deny to stand", err)
			}
			if !strings.Contains(err.Error(), "no") {
				t.Errorf("error = %v, want the denying plugin's reason", err)
			}
		})
	}
}

// TestConsultDecidersIsExportedForTheRoundBoundary: the ToolGate asks the same
// deciders one round earlier, to decide whether to suspend. It must reach them
// through the registry rather than keeping its own list, or the two would
// disagree about who is registered.
func TestConsultDecidersIsExportedForTheRoundBoundary(t *testing.T) {
	t.Parallel()

	registry := decidingRegistry(t, NewStaticPolicy(DecisionAllow), nil)
	registry.AddDecider("plugin:demo", askingDecider("look at this"))

	label, verdict := registry.ConsultDeciders(context.Background(), demoCall())
	if verdict.Decision != DecisionAsk || verdict.Reason != "look at this" {
		t.Errorf("verdict = %+v, want the ask and its reason", verdict)
	}
	if label != "plugin:demo" {
		t.Errorf("label = %q, want the registrant that asked", label)
	}
}
