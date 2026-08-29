package tool

import (
	"context"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
)

// Observer is notified, read-only, after a tool call has produced a result.
//
// It is the narrowest of the extension seams on purpose: it returns nothing.
// An observer cannot allow, deny, delay or rewrite anything — the result the
// caller receives is decided before any observer runs, and nothing an observer
// does changes it. That is what makes it safe to hand to plugin code, which is
// untrusted.
//
// WHEN it is called is part of the contract, and the exclusions matter as much
// as the inclusion. Observers see a call that RAN AND ANSWERED, including one
// that answered with a failed ToolResult. They do NOT see:
//
//   - a call refused by permissions, policy or guardrails — that call never
//     happened, and telling an observer about it would report an execution
//     that did not occur;
//   - a handler that returned a Go error — that is a fault in the host or the
//     tool implementation rather than an answer, and it is already reported
//     through the audit trail as tool_failed.
//
// Observe must not block for long: it runs inside Execute, on the caller's
// goroutine, and every millisecond it spends is spent by the tool call. An
// implementation that talks to something slow is responsible for its own
// bound (the plugin observer bounds itself with a per-notification timeout).
type Observer interface {
	Observe(ctx context.Context, call domain.ToolCall, result domain.ToolResult)
}

// ObserverFunc adapts a plain function to Observer.
type ObserverFunc func(ctx context.Context, call domain.ToolCall, result domain.ToolResult)

// Observe implements Observer.
func (f ObserverFunc) Observe(ctx context.Context, call domain.ToolCall, result domain.ToolResult) {
	f(ctx, call, result)
}

// registeredObserver pairs an observer with a label, so a diagnostic can name
// who is on the seam rather than printing a function pointer.
type registeredObserver struct {
	label    string
	observer Observer
}

// AddObserver registers o to be notified after each completed tool call,
// returning the function that removes it.
//
// label names the registrant (for the plugin seam, "plugin:<name>"). It is not
// required to be unique: two registrations are two observers, and each is
// removed by its own returned function.
//
// The revoke function is idempotent — calling it twice removes one
// registration, not two — because a caller that has both a ledger entry and a
// direct handle must not be able to unregister somebody else's observer by
// calling stale cleanup twice.
func (r *Registry) AddObserver(label string, o Observer) func() {
	if o == nil {
		panic("tool: AddObserver: observer is nil; a registration that notifies nothing is a wiring mistake")
	}
	entry := &registeredObserver{label: label, observer: o}

	r.mu.Lock()
	r.observers = append(r.observers, entry)
	r.mu.Unlock()

	var once bool
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if once {
			return
		}
		once = true
		for i, candidate := range r.observers {
			if candidate == entry {
				r.observers = append(r.observers[:i], r.observers[i+1:]...)
				return
			}
		}
	}
}

// notifyObservers tells every observer on this registry AND on its parents
// about a completed call.
//
// Parents are included because a scoped view (Subset/Without) executes calls
// whose handlers live on the parent: an observer registered where the plugin
// was mounted must see those, or "this plugin observes tool calls" would
// silently mean "…except the ones made through a per-agent view".
//
// The observer slice is COPIED under the lock and the observers are called
// with the lock released. Holding it across the call would let one slow
// observer block every registration and revocation in the process — and an
// observer that registers or revokes during its own notification (a plugin
// unloading itself, say) would deadlock outright.
func (r *Registry) notifyObservers(ctx context.Context, call domain.ToolCall, result domain.ToolResult) {
	for registry := r; registry != nil; registry = registry.parent {
		registry.mu.RLock()
		snapshot := make([]*registeredObserver, len(registry.observers))
		copy(snapshot, registry.observers)
		registry.mu.RUnlock()

		for _, entry := range snapshot {
			entry.observer.Observe(ctx, call, result)
		}
	}
}

// ObserveOwned registers an observer and files its removal in the ledger under
// owner, so that disposing the owner takes the observer off the seam.
//
// It is to AddObserver what RegisterOwned is to RegisterDescriptor: the same
// registration, with the revocation recorded where a plugin's whole
// contribution is torn down at once.
func ObserveOwned(
	ledger *lifecycle.Ledger,
	owner lifecycle.Owner,
	r *Registry,
	label string,
	o Observer,
) func() error {
	revoke := r.AddObserver(label, o)
	return ledger.Add(owner, "observer:"+label, func() error {
		revoke()
		return nil
	})
}
