package loader

import (
	"strings"
	"testing"
)

// alwaysFalse is a resolvable func that never treats a tool name as builtin —
// every requirement must come from another entry's Provides.
func alwaysFalse(string) bool { return false }

func TestResolveStates_NoDependencies_AllActive(t *testing.T) {
	entries := []depNode{
		{Name: "a"},
		{Name: "b"},
	}
	states, err := resolveStates(entries, alwaysFalse)
	if err != nil {
		t.Fatalf("resolveStates: unexpected error: %v", err)
	}
	for _, n := range entries {
		if states[n.Name] != depActive {
			t.Errorf("states[%q] = %v, want depActive", n.Name, states[n.Name])
		}
	}
}

func TestResolveStates_ProviderPresent_BothActive(t *testing.T) {
	entries := []depNode{
		{Name: "a", Provides: []string{"tool_t"}},
		{Name: "b", Requires: []string{"tool_t"}},
	}
	states, err := resolveStates(entries, alwaysFalse)
	if err != nil {
		t.Fatalf("resolveStates: unexpected error: %v", err)
	}
	if states["a"] != depActive {
		t.Errorf("states[a] = %v, want depActive", states["a"])
	}
	if states["b"] != depActive {
		t.Errorf("states[b] = %v, want depActive", states["b"])
	}
}

func TestResolveStates_NoProviderNoBuiltin_Suspended(t *testing.T) {
	entries := []depNode{
		{Name: "b", Requires: []string{"tool_t"}},
	}
	states, err := resolveStates(entries, alwaysFalse)
	if err != nil {
		t.Fatalf("resolveStates: unexpected error: %v", err)
	}
	if states["b"] != depSuspended {
		t.Errorf("states[b] = %v, want depSuspended", states["b"])
	}
}

func TestResolveStates_ResolvableBuiltin_ActiveWithoutProvider(t *testing.T) {
	entries := []depNode{
		{Name: "b", Requires: []string{"read_file"}},
	}
	resolvable := func(toolName string) bool { return toolName == "read_file" }
	states, err := resolveStates(entries, resolvable)
	if err != nil {
		t.Fatalf("resolveStates: unexpected error: %v", err)
	}
	if states["b"] != depActive {
		t.Errorf("states[b] = %v, want depActive (builtin tool, no plugin provides it)", states["b"])
	}
}

func TestResolveStates_ThreeLevelCascade_AbsentRoot_SuspendsWholeChain(t *testing.T) {
	// A provides T1; B requires T1 and provides T2; C requires T2. A is absent
	// from entries (e.g. never installed), so B's requirement is unsatisfied
	// and must cascade to C, which depends on B's contribution.
	entries := []depNode{
		{Name: "b", Requires: []string{"tool_t1"}, Provides: []string{"tool_t2"}},
		{Name: "c", Requires: []string{"tool_t2"}},
	}
	states, err := resolveStates(entries, alwaysFalse)
	if err != nil {
		t.Fatalf("resolveStates: unexpected error: %v", err)
	}
	if states["b"] != depSuspended {
		t.Errorf("states[b] = %v, want depSuspended (A absent, tool_t1 unresolved)", states["b"])
	}
	if states["c"] != depSuspended {
		t.Errorf("states[c] = %v, want depSuspended (cascaded from b's suspension)", states["c"])
	}
}

func TestResolveStates_PartialCascade_UnrelatedProviderStaysActive(t *testing.T) {
	// Same graph as the three-level cascade, but this time A is present and B
	// is absent. C must suspend (nobody provides tool_t2 any more), while A —
	// which does not depend on anything — must stay active: the cascade must
	// not over-suspend nodes outside the affected chain.
	entries := []depNode{
		{Name: "a", Provides: []string{"tool_t1"}},
		{Name: "c", Requires: []string{"tool_t2"}},
	}
	states, err := resolveStates(entries, alwaysFalse)
	if err != nil {
		t.Fatalf("resolveStates: unexpected error: %v", err)
	}
	if states["a"] != depActive {
		t.Errorf("states[a] = %v, want depActive (a has no requirements of its own)", states["a"])
	}
	if states["c"] != depSuspended {
		t.Errorf("states[c] = %v, want depSuspended (nobody provides tool_t2)", states["c"])
	}
}

func TestResolveStates_Cycle_ReturnsErrorNamingBothPlugins(t *testing.T) {
	entries := []depNode{
		{Name: "a", Provides: []string{"tool_a"}, Requires: []string{"tool_b"}},
		{Name: "b", Provides: []string{"tool_b"}, Requires: []string{"tool_a"}},
	}
	_, err := resolveStates(entries, alwaysFalse)
	if err == nil {
		t.Fatal("resolveStates: want error for dependency cycle, got nil")
	}
	// Assert the actual rendered cycle, not a loose substring check: with
	// single-letter plugin names, strings.Contains(err.Error(), "b") would
	// stay true even if the real cycle-naming logic broke and "b" only
	// appeared incidentally elsewhere in a reworded message.
	const wantCycle = "a -> b -> a"
	if !strings.Contains(err.Error(), wantCycle) {
		t.Errorf("cycle error %q must contain the rendered cycle %q", err.Error(), wantCycle)
	}
}

func TestResolveStates_NilEntries_ReturnsEmptyMapNoError(t *testing.T) {
	states, err := resolveStates(nil, alwaysFalse)
	if err != nil {
		t.Fatalf("resolveStates: unexpected error: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("states = %v, want empty map", states)
	}
}

func TestResolveStates_EmptyEntries_ReturnsEmptyMapNoError(t *testing.T) {
	states, err := resolveStates([]depNode{}, alwaysFalse)
	if err != nil {
		t.Fatalf("resolveStates: unexpected error: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("states = %v, want empty map", states)
	}
}

// TestResolveStates_DuplicateProvides_ReturnsErrorNamingToolAndBothPlugins
// covers the shape the review flagged: two plugins that are otherwise
// unrelated to any self-dependency both declare Provides for the same tool
// name. Letting the later entry win by slice order would be exactly the
// silent, order-dependent default the fail-loud rule forbids, so this must
// be a fail-loud error naming the tool and both plugins.
func TestResolveStates_DuplicateProvides_ReturnsErrorNamingToolAndBothPlugins(t *testing.T) {
	entries := []depNode{
		{Name: "x", Provides: []string{"tool_t"}},
		{Name: "y", Provides: []string{"tool_t"}},
	}
	_, err := resolveStates(entries, alwaysFalse)
	if err == nil {
		t.Fatal("resolveStates: want error for duplicate Provides, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tool_t") {
		t.Errorf("duplicate-provides error %q must name the tool \"tool_t\"", msg)
	}
	if !strings.Contains(msg, "\"x\"") || !strings.Contains(msg, "\"y\"") {
		t.Errorf("duplicate-provides error %q must name both plugins \"x\" and \"y\"", msg)
	}
}

// TestResolveStates_SelfDependencyWithDuplicateProvides_PanicsBeforeDuplicateError
// pins the overlap the review identified: x requires and provides "T"
// itself (a genuine self-dependency), while an unrelated plugin y also
// provides "T" (making the graph a duplicate-Provides graph too). Both
// checks are real for this input; resolveStates must run the node-local
// self-dependency check first and panic, never reaching (or masking the
// panic behind) the duplicate-Provides error — because the self-dependency
// panic is a programming-invariant violation, which takes priority over an
// operator-data error.
func TestResolveStates_SelfDependencyWithDuplicateProvides_PanicsBeforeDuplicateError(t *testing.T) {
	entries := []depNode{
		{Name: "x", Provides: []string{"tool_t"}, Requires: []string{"tool_t"}},
		{Name: "y", Provides: []string{"tool_t"}},
	}

	var (
		err      error
		panicked any
	)
	func() {
		defer func() { panicked = recover() }()
		_, err = resolveStates(entries, alwaysFalse)
	}()

	if panicked == nil {
		t.Fatalf("resolveStates: want panic for x's self-dependency (must fire before the duplicate-Provides error), got err=%v", err)
	}
	msg, ok := panicked.(string)
	if !ok {
		t.Fatalf("resolveStates: panic value = %#v, want string naming plugin \"x\"", panicked)
	}
	if !strings.Contains(msg, "\"x\"") {
		t.Errorf("panic message %q must name plugin \"x\" (the self-dependent entry), not resolve to the duplicate-Provides error", msg)
	}
}

// TestResolveStates_SelfDependency_PanicsRatherThanReachingCascade documents
// that a manifest whose Requires names a tool it contributes itself must
// never reach resolveStates: internal/plugin/manifest rejects it at parse
// time (see manifest.validateRequires). If one ever does arrive here — a
// caller bug, or a future manifest regression — resolveStates must fail
// loudly with a panic naming the violated invariant, not silently treat the
// entry as satisfied or hand back a misleading cycle error.
func TestResolveStates_SelfDependency_PanicsRatherThanReachingCascade(t *testing.T) {
	entries := []depNode{
		{Name: "a", Provides: []string{"tool_a"}, Requires: []string{"tool_a"}},
	}

	var (
		err      error
		panicked any
	)
	func() {
		defer func() { panicked = recover() }()
		_, err = resolveStates(entries, alwaysFalse)
	}()

	if panicked == nil {
		t.Fatalf("resolveStates: want panic for self-dependency (Task 1 should have rejected this at the manifest layer), got err=%v", err)
	}
}
