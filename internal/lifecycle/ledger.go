// Package lifecycle owns the answer to one question: when something must be
// torn down, who is responsible for revoking it.
//
// A Ledger records revocation actions grouped by owner. It never decides WHAT a
// disposer does — the creator supplies that — only who must run it and in which
// order. Ownership follows the creator, not the place the resource ends up: a
// plugin that registers a handler into someone else's map still owns the
// revocation, so no central component has to maintain a directory of who came,
// who left, and what they left behind.
package lifecycle

import (
	"errors"
	"fmt"
	"sync"
)

// Owner identifies whoever created a resource: a plugin instance id, an agent
// session id, or a static assembly name. Compared by value.
type Owner string

// entry is one revocable resource. done guards against running a disposer
// twice when a handle and DisposeOwner race.
type entry struct {
	label   string
	dispose func() error
	done    bool
}

// Ledger maps owners to the revocation actions they are responsible for.
// The zero value is not usable; call NewLedger.
type Ledger struct {
	mu      sync.Mutex
	entries map[Owner][]*entry
}

// NewLedger returns an empty Ledger.
func NewLedger() *Ledger {
	return &Ledger{entries: make(map[Owner][]*entry)}
}

// Add registers dispose under owner and returns a one-shot handle that runs it
// immediately and removes it from the ledger. label names the resource in
// diagnostics and in wrapped errors, so it should identify the thing being
// revoked ("tool:read_file", "wasm-instance"), not the action.
//
// A nil dispose is a programming error: registering a resource with no way to
// revoke it defeats the ledger's only purpose.
func (l *Ledger) Add(owner Owner, label string, dispose func() error) func() error {
	if dispose == nil {
		panic(fmt.Sprintf("lifecycle: Add(%s, %s) requires a dispose func", owner, label))
	}
	e := &entry{label: label, dispose: dispose}
	l.mu.Lock()
	l.entries[owner] = append(l.entries[owner], e)
	l.mu.Unlock()
	return func() error { return l.revoke(owner, e) }
}

// revoke runs one entry if it has not run yet and detaches it from its owner.
func (l *Ledger) revoke(owner Owner, target *entry) error {
	l.mu.Lock()
	if target.done {
		l.mu.Unlock()
		return nil
	}
	target.done = true
	list := l.entries[owner]
	for i, e := range list {
		if e == target {
			l.entries[owner] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(l.entries[owner]) == 0 {
		delete(l.entries, owner)
	}
	l.mu.Unlock()

	if err := target.dispose(); err != nil {
		return fmt.Errorf("dispose %s/%s: %w", owner, target.label, err)
	}
	return nil
}

// DisposeOwner runs every disposer registered under owner in reverse
// registration order — last created, first closed — and clears the owner.
//
// Every remaining disposer runs even when an earlier one fails: stopping at the
// first failure would leave more ghosts behind than it prevents. Failures are
// joined and returned; callers at a teardown boundary must log the result at
// Error level rather than discarding it.
func (l *Ledger) DisposeOwner(owner Owner) error {
	l.mu.Lock()
	list := l.entries[owner]
	delete(l.entries, owner)
	pending := make([]*entry, 0, len(list))
	for _, e := range list {
		if !e.done {
			e.done = true
			pending = append(pending, e)
		}
	}
	l.mu.Unlock()

	var errs []error
	for i := len(pending) - 1; i >= 0; i-- {
		if err := pending[i].dispose(); err != nil {
			errs = append(errs, fmt.Errorf("dispose %s/%s: %w", owner, pending[i].label, err))
		}
	}
	return errors.Join(errs...)
}

// Snapshot reports the live entry labels per owner, in registration order. The
// returned maps and slices are copies; mutating them does not affect the
// ledger. It exists for diagnostics: "plugin loaded but nothing happened" must
// be answerable without reading code.
func (l *Ledger) Snapshot() map[Owner][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[Owner][]string, len(l.entries))
	for owner, list := range l.entries {
		labels := make([]string, 0, len(list))
		for _, e := range list {
			if !e.done {
				labels = append(labels, e.label)
			}
		}
		if len(labels) > 0 {
			out[owner] = labels
		}
	}
	return out
}
