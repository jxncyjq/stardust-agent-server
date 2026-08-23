package loader

import (
	"fmt"
	"strings"
)

// depState is which of the two lifecycle positions resolveStates leaves a
// plugin in: depActive (its contributions stand) or depSuspended (its
// contributions are withdrawn because something it Requires cannot be
// resolved). There is no third state here — resolveStates is a pure
// decision function; the ledger, wasm instances, and the actual Suspend and
// Resume calls belong to the Loader (see depNode's doc for the boundary).
type depState int

const (
	// depActive means the plugin's Requires are all satisfied — either by
	// resolvable or by another depNode in the same call whose Provides
	// covers the name, and which is itself depActive.
	depActive depState = iota
	// depSuspended means at least one entry of the plugin's Requires could
	// not be resolved: resolvable rejected it, and no depActive entry
	// provides it either.
	depSuspended
)

// String renders a depState for error messages and test failure output.
func (s depState) String() string {
	switch s {
	case depActive:
		return "depActive"
	case depSuspended:
		return "depSuspended"
	default:
		return fmt.Sprintf("depState(%d)", int(s))
	}
}

// depNode is one plugin's dependency-relevant shape: the tool names it
// contributes (Provides) and the tool names it calls into (Requires). It
// carries nothing else — no ledger handle, no wasm instance, no host
// reference — because resolveStates is a pure function over plain data.
// Task 4 (the Loader) is what turns depNode's source data (a mounted
// plugin's manifest) and resolveStates's answer into actual Suspend/Resume
// calls; this package boundary is what keeps resolveStates exhaustively
// testable without a running host.
type depNode struct {
	// Name identifies the plugin. It is only used to key the returned map
	// and to name plugins in cycle errors; resolveStates does not interpret
	// it otherwise.
	Name string
	// Provides lists the tool names this plugin contributes while active.
	Provides []string
	// Requires lists the tool names this plugin calls into. Every name here
	// must resolve — either via the resolvable callback or via another
	// depNode's Provides — for this plugin to stay depActive.
	Requires []string
}

// resolveStates decides, for a fixed snapshot of plugins described by
// entries, which should be depActive and which depSuspended, given the
// caller's answer for whether a tool name resolves outside this graph
// entirely (resolvable — true for e.g. builtin host tools, which are always
// satisfied regardless of what any plugin contributes).
//
// # Algorithm
//
//  1. Every entry starts depActive.
//  2. Repeat in rounds: for each still-depActive entry, if any name in its
//     Requires is satisfied by neither resolvable nor by another entry that
//     is depActive in the round's starting snapshot, the entry transitions
//     to depSuspended. All transitions decided in a round are collected
//     from that one snapshot and applied together at the round's end, so a
//     round's outcome does not depend on entries' slice order.
//  3. A round with zero transitions means convergence: nothing left would
//     change, so the result is returned as-is.
//
// # Why the loop is bounded, and by what
//
// A transition only ever goes depActive -> depSuspended; resolveStates
// never resumes an entry once suspended in the same call. That makes the
// total number of transitions across every round at most len(entries), and
// a round that performs zero transitions is, by definition, convergence.
// So at most len(entries) rounds can ever transition anything, and the
// very next round after that is guaranteed to see zero transitions and
// return. resolveStates therefore runs at most len(entries)+1 rounds: up to
// len(entries) rounds that may each transition entries, plus exactly one
// more round whose only job is to observe that nothing changed. That +1
// round is a hard, literal part of the bound (not a "keep going until
// stable" loop) — it exists to let the very last transition's cascade
// (if any) be confirmed as final before returning.
//
// If that last, (len(entries)+1)th round still finds a transition to make,
// the bound above has been violated by the code's own logic — not by any
// input resolveStates was given — so resolveStates panics naming the
// invariant instead of taking another round. This is what stands between a
// bug in this cascade and the unbounded "loop until nothing changes" shape
// that has previously hung a test to 170GB of virtual memory: the round
// count is capped by len(entries) before a single round runs, full stop.
//
// # Cycles are different from suspension
//
// Before any round runs, resolveStates checks the dependency graph formed
// purely by Requires and Provides (regardless of resolvable, and regardless
// of any entry's eventual depState) for a cycle: a chain of entries where
// each requires a tool the next one provides, looping back on itself. A
// cycle is not something the round loop above would misbehave on — mutual
// providers that are both active satisfy each other's Requires and the
// cascade simply converges with both depActive — but it is still an
// unresolvable activation order for a caller trying to bring the plugins up
// from nothing, so it is reported as an error naming every plugin on the
// cycle. This is operator data (a bad set of manifests), not a programming
// error, so it is returned, never panicked.
//
// A depNode whose Requires names a tool it contributes itself is a
// different case: Task 1 (internal/plugin/manifest's ParsePlugin) rejects
// that at manifest-parse time, so it must never reach resolveStates.
// Reaching this function is therefore a programming invariant violation —
// a caller bug, not operator data — and resolveStates panics naming it
// rather than silently treating the requirement as satisfied or folding it
// into the cycle-detection error meant for genuine multi-plugin cycles.
// This check is deliberately node-local: it tests n's own Provides directly
// against n's own Requires, never routed through the providerOf map below,
// because self-dependency is a property of a single entry and must not
// depend on what any other entry happens to declare (see the duplicate
// Provides section next for why routing it through providerOf would be
// wrong). It is also checked before duplicate Provides below, so an entry
// that is simultaneously self-dependent and part of a duplicate-Provides
// pair still panics rather than being reported as a duplicate-provider
// error — the programming-invariant violation takes priority over the
// operator-data error.
//
// # Duplicate Provides across plugins is refused, not resolved by order
//
// Two entries in the same call must never declare the same tool name in
// Provides. This is operator data, exactly like a cycle: two plugins
// contributing the same tool cannot both be mounted anyway — the host's
// tool registry and the gateable catalog both panic on a duplicate
// registration, and the Loader already pre-checks tool-name conflicts
// before activating a plugin — so a depgraph containing two Provides for
// the same tool describes a manifest-level mistake, not a graph
// resolveStates can meaningfully decide. Silently letting the later entry
// in the slice win (as an ordinary map insert would) is precisely the
// silent, input-order-dependent default the fail-loud rule forbids
// elsewhere in this function, and it has a real consequence: a single
// providerOf entry is all requirementsSatisfied consults, so if the
// winning provider is suspended while the losing (equally real) provider
// is still active, the cascade would suspend a requirement that a correct
// "any active provider satisfies" semantics would not touch. resolveStates
// therefore returns an error naming the tool and both plugins as soon as a
// second Provides for the same name is seen, before any cascade or cycle
// check runs on the resulting (ambiguous) providerOf map.
func resolveStates(entries []depNode, resolvable func(toolName string) bool) (map[string]depState, error) {
	if resolvable == nil {
		panic("depgraph: resolveStates called with a nil resolvable; callers must always supply a callback (one that always returns false is fine), never nil")
	}

	for _, n := range entries {
		for _, r := range n.Requires {
			if containsName(n.Provides, r) {
				panic(fmt.Sprintf(
					"depgraph: plugin %q requires %q, which it provides itself; "+
						"internal/plugin/manifest.ParsePlugin should have rejected this at parse time",
					n.Name, r))
			}
		}
	}

	providerOf := make(map[string]string, len(entries))
	for _, n := range entries {
		for _, tool := range n.Provides {
			if existing, dup := providerOf[tool]; dup {
				return nil, fmt.Errorf(
					"depgraph: tool %q is provided by both plugin %q and plugin %q; "+
						"two plugins cannot both provide the same tool name",
					tool, existing, n.Name)
			}
			providerOf[tool] = n.Name
		}
	}

	if cycle := detectCycle(entries, providerOf); cycle != nil {
		display := append(append([]string(nil), cycle...), cycle[0])
		return nil, fmt.Errorf("depgraph: dependency cycle among plugins: %s", strings.Join(display, " -> "))
	}

	states := make(map[string]depState, len(entries))
	for _, n := range entries {
		states[n.Name] = depActive
	}

	maxRounds := len(entries)
	for round := 0; round <= maxRounds; round++ {
		var toSuspend []string
		for _, n := range entries {
			if states[n.Name] != depActive {
				continue
			}
			if requirementsSatisfied(n, states, providerOf, resolvable) {
				continue
			}
			toSuspend = append(toSuspend, n.Name)
		}

		if len(toSuspend) == 0 {
			return states, nil
		}

		if round == maxRounds {
			panic(fmt.Sprintf(
				"depgraph: cascade still finding transitions after %d rounds (node count + 1 confirmation round); "+
					"this violates the bound that at most len(entries) transitions can ever happen — plugins still "+
					"pending suspension: %s", maxRounds, strings.Join(toSuspend, ", ")))
		}

		for _, name := range toSuspend {
			states[name] = depSuspended
		}
	}

	// Unreachable: the loop above always either returns on convergence or
	// panics on round == maxRounds, and round reaches maxRounds within
	// maxRounds+1 iterations.
	panic("depgraph: resolveStates fell through its bounded loop, which should be unreachable")
}

// containsName reports whether name is present in names. resolveStates uses
// it for the self-dependency check, which must stay a purely node-local
// property (does this entry's own Provides contain a name from its own
// Requires) rather than being routed through the providerOf map, which can
// only ever name one winning provider per tool.
func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// requirementsSatisfied reports whether every name in n.Requires resolves —
// either via resolvable, or via another entry that both provides it and is
// depActive in the snapshot states holds at the moment of the call. states
// is read only, never written, so the same snapshot can be reused to decide
// every entry's transition within one round before any of them are applied.
// providerOf is guaranteed by resolveStates to hold at most one provider
// name per tool — duplicate Provides across entries is refused as an error
// before requirementsSatisfied is ever called — so the single lookup below
// is never resolving an ambiguity by picking one of several real providers.
func requirementsSatisfied(n depNode, states map[string]depState, providerOf map[string]string, resolvable func(string) bool) bool {
	for _, r := range n.Requires {
		if resolvable(r) {
			continue
		}
		provider, ok := providerOf[r]
		if ok && states[provider] == depActive {
			continue
		}
		return false
	}
	return true
}

// detectCycle looks for a cycle in the dependency graph formed by entries'
// Requires and Provides — an edge runs from a plugin to whichever other
// plugin provides a tool it requires — and returns the plugin names on the
// first cycle found, in cycle order, or nil if the graph is acyclic. It
// does not consult resolvable or any depState: a cycle is a property of the
// declared dependency shape alone, independent of whether the round loop in
// resolveStates would ever actually suspend either plugin.
//
// The traversal is a standard three-colour DFS (white/gray/black) over
// len(entries) nodes, with one outgoing edge per Requires name that
// resolves to another entry's Provides — so a node with several Requires
// contributes several edges, not just one; termination does not depend on
// counting edges at all. It visits each node at most once thanks to the
// colour marks (white -> gray -> black, never revisited once black), so it
// terminates without needing any loop-count cap of its own.
func detectCycle(entries []depNode, providerOf map[string]string) []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)

	byName := make(map[string]depNode, len(entries))
	for _, n := range entries {
		byName[n.Name] = n
	}

	color := make(map[string]int, len(entries))
	var stack []string
	var cycle []string

	var visit func(name string) bool
	visit = func(name string) bool {
		color[name] = gray
		stack = append(stack, name)

		for _, r := range byName[name].Requires {
			next, ok := providerOf[r]
			if !ok {
				continue
			}
			switch color[next] {
			case white:
				if visit(next) {
					return true
				}
			case gray:
				start := indexOfName(stack, next)
				cycle = append([]string(nil), stack[start:]...)
				return true
			case black:
				// Fully explored already; no cycle reachable through it.
			}
		}

		stack = stack[:len(stack)-1]
		color[name] = black
		return false
	}

	for _, n := range entries {
		if color[n.Name] == white {
			if visit(n.Name) {
				return cycle
			}
		}
	}
	return nil
}

// indexOfName returns the index of name in stack. stack is the current DFS
// recursion stack in detectCycle, and name is always present on it at the
// call site (detectCycle only calls this when color[next] == gray, which
// means next was pushed onto stack and not yet popped) — so a missing name
// there would be a bug in detectCycle's own bookkeeping, not bad input.
func indexOfName(stack []string, name string) int {
	for i, s := range stack {
		if s == name {
			return i
		}
	}
	panic(fmt.Sprintf("depgraph: %q marked gray but not found on the DFS stack %v", name, stack))
}
