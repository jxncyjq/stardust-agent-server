package tool

import (
	"context"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
)

// deciderMaxTimeout is the ceiling on ONE decider's consultation, and
// deciderBudgetShare is the fraction of the tool's own timeout a consultation
// may take when that is smaller.
//
// Both exist because the consultation is spent by the call that is waiting on
// it: a decider runs before dispatch, on the caller's goroutine, and every
// millisecond it takes is a millisecond the tool has not started. A decider
// that needs longer than this is not slow, it is misdesigned — the decision it
// is making cannot depend on anything that takes seconds, or the answer would
// be stale by the time the tool ran.
//
// The share matters as much as the ceiling: a tool declaring a 300ms timeout
// must not spend 200ms of it being asked for permission.
const (
	deciderMaxTimeout  = 200 * time.Millisecond
	deciderBudgetShare = 4
)

// Verdict is one decider's answer about one call.
//
// Decision reuses the registry's own allow/deny vocabulary rather than
// introducing a second one, because these answers are composed with the
// host's: a decider can only make the outcome stricter, never looser, and
// sharing the type keeps that comparison honest. The vocabulary has room to
// grow — an "ask" (require human approval) is the next value, and it slots
// into the ranking below without changing this shape.
//
// Reason reaches the caller in the refusal error, so it must say what was
// wrong in words the person reading a denied tool call can act on. A denial
// with no reason is the one answer nothing downstream can do anything with,
// so an empty Reason is replaced with a placeholder rather than silently
// producing a bare "denied".
type Verdict struct {
	Decision Decision
	Reason   string
}

// Decider is consulted BEFORE a tool call is dispatched and may refuse it.
//
// It can only TIGHTEN. The registry consults deciders only after its own
// enforcer and policy have allowed the call, so a decider is never even shown
// a call the host already refused — there is no position from which one could
// turn a refusal into a permission. Returning DecisionAllow means "I do not
// object", never "I authorize".
//
// Decide must not block for long: see deciderMaxTimeout. The ctx it receives
// already carries that bound, and an implementation that talks to something
// slow is responsible for honouring it. A decider that runs out of time is
// the caller's problem to define — for the plugin seam it is a REFUSAL (see
// the fail-closed argument in host.pluginDecider), because a security control
// that an attacker can switch off by making a plugin hang is not a control.
type Decider interface {
	Decide(ctx context.Context, call domain.ToolCall) Verdict
}

// DeciderFunc adapts a plain function to Decider.
type DeciderFunc func(ctx context.Context, call domain.ToolCall) Verdict

// Decide implements Decider.
func (f DeciderFunc) Decide(ctx context.Context, call domain.ToolCall) Verdict {
	return f(ctx, call)
}

// registeredDecider pairs a decider with the label of whoever registered it,
// so a refusal can name WHO refused. A denial an operator cannot attribute is
// a denial they cannot fix.
type registeredDecider struct {
	label   string
	decider Decider
}

// AddDecider registers d to be consulted before each dispatched tool call,
// returning the function that removes it.
//
// label names the registrant (for the plugin seam, "plugin:<name>") and is
// reported in the refusal. It is not required to be unique.
//
// The revoke function is idempotent, for the same reason AddObserver's is: a
// caller holding both a ledger entry and a direct handle must not be able to
// remove somebody else's registration by running stale cleanup twice.
func (r *Registry) AddDecider(label string, d Decider) func() {
	if d == nil {
		panic("tool: AddDecider: decider is nil; a registration that decides nothing is a wiring mistake")
	}
	entry := &registeredDecider{label: label, decider: d}

	r.mu.Lock()
	r.deciders = append(r.deciders, entry)
	r.mu.Unlock()

	var once bool
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if once {
			return
		}
		once = true
		for i, candidate := range r.deciders {
			if candidate == entry {
				r.deciders = append(r.deciders[:i], r.deciders[i+1:]...)
				return
			}
		}
	}
}

// DecideOwned registers a decider and files its removal in the ledger under
// owner, so that disposing the owner takes it off the seam.
//
// It is to AddDecider what ObserveOwned is to AddObserver: a plugin whose
// contributions were withdrawn but which still refuses tool calls would be a
// plugin the deployment believes it has disabled.
func DecideOwned(
	ledger *lifecycle.Ledger,
	owner lifecycle.Owner,
	r *Registry,
	label string,
	d Decider,
) func() error {
	revoke := r.AddDecider(label, d)
	return ledger.Add(owner, "decider:"+label, func() error {
		revoke()
		return nil
	})
}

// consultDeciders asks every decider on this registry AND on its parents
// about a call, and reports the strictest answer.
//
// Parents are included for the same reason they are in notifyObservers: a
// scoped view (Subset/Without) executes handlers that live on the parent, and
// most agent calls travel through one. A decider that saw only direct calls
// would be a refusal an agent could evade by holding a narrower view.
//
// It SHORT-CIRCUITS on the first refusal. With allow and deny the outcome is
// order-independent either way — deny is strictest — and spending the
// caller's remaining budget asking the rest for an answer that cannot change
// anything would be paid for by the call being refused. The consequence,
// worth stating because it is visible in the error text: the refusal names
// the first decider that objected, not every one that would have.
//
// The decider slice is COPIED under the lock and the deciders are called with
// the lock released, so a decider that registers or revokes during its own
// consultation (a plugin unloading itself) completes instead of deadlocking.
func (r *Registry) consultDeciders(ctx context.Context, descriptor Descriptor, call domain.ToolCall) (string, Verdict) {
	for registry := r; registry != nil; registry = registry.parent {
		registry.mu.RLock()
		snapshot := make([]*registeredDecider, len(registry.deciders))
		copy(snapshot, registry.deciders)
		registry.mu.RUnlock()

		for _, entry := range snapshot {
			verdict := decideWithBudget(ctx, descriptor, entry.decider, call)
			if verdict.Decision == DecisionDeny {
				if verdict.Reason == "" {
					verdict.Reason = "no reason given"
				}
				return entry.label, verdict
			}
		}
	}
	return "", Verdict{Decision: DecisionAllow}
}

// decideWithBudget runs one consultation under its own deadline.
//
// The deadline is derived from the caller's context, so a caller who walked
// away is not waited on; it is the SMALLER of deciderMaxTimeout and a quarter
// of the tool's declared timeout, so a short tool cannot have most of its
// budget spent deciding whether to run it.
func decideWithBudget(ctx context.Context, descriptor Descriptor, decider Decider, call domain.ToolCall) Verdict {
	budget := deciderMaxTimeout
	if share := descriptor.Timeout / deciderBudgetShare; descriptor.Timeout > 0 && share < budget {
		budget = share
	}
	decideCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return decider.Decide(decideCtx, call)
}
