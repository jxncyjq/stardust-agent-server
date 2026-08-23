package loader

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/tool"
)

// This file covers the Loader's dependency convergence: a mounted plugin whose
// "requires" cannot be resolved has its contributions withdrawn (suspended)
// instead of advertising tools whose every call must fail, and gets them back
// the moment the requirement returns.
//
// # Bounds (fork-bomb regime)
//
// Every test here mounts, suspends, resumes and unmounts REAL wasm instances.
// The bounds are the same regime the rest of this package works under:
//
//   - the one test that applies in a LOOP
//     (TestApplyConvergesToTheSameDependencyStateEveryRound) runs a LITERAL 5
//     rounds and asserts the ledger's instance-owner count against a declared
//     ceiling in every single round, so a convergence that leaked an instance
//     per round fails on the first leaked one rather than after "enough" rounds;
//   - every other test applies a fixed, written-out number of times;
//   - nothing here waits on a channel, and no test uses the feature under test
//     as its termination condition.

// The three plugins this file builds a dependency chain out of:
//
//	dep-a-plugin  provides dep_a_tool, requires nothing
//	dep-b-plugin  provides dep_b_tool, requires dep_a_tool
//	dep-c-plugin  provides dep_c_tool, requires dep_b_tool
//
// Three are needed because a two-plugin chain cannot tell "suspend the direct
// dependent" apart from "suspend everything downstream", which is the whole
// point of the cascade.
const (
	depAPluginName = "dep-a-plugin"
	depAToolName   = "dep_a_tool"

	depBPluginName = "dep-b-plugin"
	depBToolName   = "dep_b_tool"

	depCPluginName = "dep-c-plugin"
	depCToolName   = "dep_c_tool"
)

// hostBuiltinToolName is a tool registered straight on the harness registry,
// belonging to no plugin at all. It stands in for what a "requires" entry names
// in almost every real deployment: one of the HOST's own tools, which no
// plugin's suspension can take away and which therefore has to resolve through
// externalToolNames rather than through the dependency graph.
const hostBuiltinToolName = "host_builtin_tool"

// The plugin that requires nothing but hostBuiltinToolName. Its names are kept
// short because patchIdentity has to fit them into a fixed-length literal.
const (
	builtinDepPluginName = "hostdep-plugin"
	builtinDepToolName   = "hostdep_tool"
)

// The three plugins the mid-chain resume-failure test deploys. They are a
// second, SHORTER-named chain rather than the dep-* one because its middle
// plugin has to provide TWO tools — one that the chain depends on and one a
// squatter can take — and patchIdentity's fixed-length budget cannot hold two
// tool names next to a name as long as "dep-b-plugin".
//
//	mid-a  provides ma_tool, requires nothing
//	mid-b  provides mb_tool and mb_aux, requires ma_tool
//	mid-c  provides mc_tool, requires mb_tool
const (
	midAPluginName = "mid-a"
	midAToolName   = "ma_tool"

	midBPluginName = "mid-b"
	midBToolName   = "mb_tool"
	midBAuxName    = "mb_aux"

	midCPluginName = "mid-c"
	midCToolName   = "mc_tool"
)

// echoIdentity is the self-description plugin.wasm answers abi.OpManifest
// with, byte for byte (see internal/plugin/host/testdata/README.md). It is
// spelled here so a rebuilt fixture that changed one character fails
// patchIdentity loudly instead of letting these tests mount a plugin under an
// identity nobody patched.
const echoIdentity = `{"name":"legion-test-plugin","version":"0.1.0","provides":["echo_tool"]}`

// patchIdentity returns plugin.wasm with the identity it answers
// abi.OpManifest with rewritten to name and toolNames, so this package can
// mount SEVERAL distinct plugins from the two guest binaries the repository
// commits.
//
// It is needed because host.Activate cross-checks a Spec against the guest's
// OWN self-description: the plugin name must be the one the guest declares and
// every contributed tool must appear in the guest's "provides". Two committed
// fixtures therefore cap a test at two simultaneous plugins, and a dependency
// CASCADE needs three. Building a third guest would need a Rust toolchain in
// CI, which the fixtures exist to avoid.
//
// The rewrite is a byte substitution of EQUAL length — the replacement is
// padded with the whitespace JSON allows before its closing brace — because
// the guest holds the document as a fixed-length constant: its length is an
// immediate in the compiled code, so a longer or shorter document would make
// the guest hand back a truncated or over-long body. Everything here is
// fail-loud: a fixture that no longer contains the literal exactly once, or an
// identity too long to fit, fails the test rather than producing a module that
// would fail later for an unrelated-looking reason.
//
// This is the same kind of surgical fixture edit appendCustomSection performs;
// like it, it never touches the committed file, only the bytes a test writes
// into its own temp directory.
func patchIdentity(t *testing.T, name string, toolNames ...string) []byte {
	t.Helper()

	if len(toolNames) == 0 {
		t.Fatalf("patchIdentity: plugin %q was given no tool names; a guest that provides nothing "+
			"cannot back any deployment entry this package writes", name)
	}

	wasm := fixtureWasm(t, echoWasmFile)
	old := []byte(echoIdentity)
	if count := bytes.Count(wasm, old); count != 1 {
		t.Fatalf("patchIdentity: %s contains the identity literal %s %d times, want exactly 1; "+
			"the fixture was rebuilt and this helper no longer knows where to patch it",
			echoWasmFile, echoIdentity, count)
	}

	quoted := make([]string, 0, len(toolNames))
	for _, toolName := range toolNames {
		quoted = append(quoted, fmt.Sprintf("%q", toolName))
	}
	body := fmt.Sprintf(`{"name":%q,"version":"0.1.0","provides":[%s]`, name, strings.Join(quoted, ","))
	padding := len(old) - len(body) - len("}")
	if padding < 0 {
		t.Fatalf("patchIdentity: identity for plugin %q / tools %v is %d bytes too long for the %d-byte "+
			"literal it replaces; pick shorter names", name, toolNames, -padding, len(old))
	}
	replacement := make([]byte, 0, len(old))
	replacement = append(replacement, body...)
	replacement = append(replacement, bytes.Repeat([]byte(" "), padding)...)
	replacement = append(replacement, '}')
	if len(replacement) != len(old) {
		t.Fatalf("patchIdentity: replacement is %d bytes, want %d", len(replacement), len(old))
	}
	return bytes.Replace(wasm, old, replacement, 1)
}

// writeDep writes one dependency-chain plugin package under its own directory
// and returns the deployment entry that installs it. dir is the package
// directory's name inside the deployment root, so a test can rewrite the same
// plugin with a different "requires" by calling this again.
func (h *harness) writeDep(dir, name, toolName string, requires ...string) manifest.Entry {
	h.t.Helper()

	return h.writeDepTools(dir, name, []string{toolName}, requires...)
}

// writeDepTools is writeDep for a plugin that provides more than one tool: the
// middle of a chain needs a second, unrequired name for a squatter to take, so
// that its resume can be made to fail WITHOUT the tool its dependents need
// ending up registered by somebody else.
func (h *harness) writeDepTools(dir, name string, tools []string, requires ...string) manifest.Entry {
	h.t.Helper()

	writePackage(h.t, filepath.Join(h.root, dir), pkg{
		wasm:     patchIdentity(h.t, name, tools...),
		name:     name,
		version:  "0.1.0",
		tools:    tools,
		requires: requires,
	})
	return entryFor(name, dir, nil, tools...)
}

// midChain writes the mid-a -> mid-b -> mid-c chain and returns the three
// entries in that order. mid-b provides midBAuxName on top of midBToolName; see
// the constants' comment.
func (h *harness) midChain() (a, b, c manifest.Entry) {
	h.t.Helper()

	a = h.writeDep("mida", midAPluginName, midAToolName)
	b = h.writeDepTools("midb", midBPluginName, []string{midBToolName, midBAuxName}, midAToolName)
	c = h.writeDep("midc", midCPluginName, midCToolName, midBToolName)
	return a, b, c
}

// depChain writes the whole dep-a -> dep-b -> dep-c chain and returns the three
// entries in that order.
func (h *harness) depChain() (a, b, c manifest.Entry) {
	h.t.Helper()

	a = h.writeDep("depa", depAPluginName, depAToolName)
	b = h.writeDep("depb", depBPluginName, depBToolName, depAToolName)
	c = h.writeDep("depc", depCPluginName, depCToolName, depBToolName)
	return a, b, c
}

// statusOf returns the Status row for one plugin, failing the test when there
// is none: an absent row is never the answer a dependency assertion wants, and
// reporting "state was empty" instead would point at the wrong thing.
func (h *harness) statusOf(name string) InstanceStatus {
	h.t.Helper()

	for _, row := range h.loader.Status() {
		if row.Name == name {
			return row
		}
	}
	h.t.Fatalf("Status has no row for plugin %q; rows: %v", name, h.loader.Status())
	return InstanceStatus{}
}

// wantState asserts one plugin's reported state and the tool names its
// suspension is blamed on.
func (h *harness) wantState(name, state string, suspendedBy ...string) {
	h.t.Helper()

	row := h.statusOf(name)
	if row.State != state {
		h.t.Fatalf("plugin %q: State = %q, want %q (LastError %q)", name, row.State, state, row.LastError)
	}
	got := append([]string(nil), row.SuspendedBy...)
	slices.Sort(got)
	want := append([]string(nil), suspendedBy...)
	slices.Sort(want)
	wantStrings(h.t, fmt.Sprintf("plugin %q SuspendedBy", name), got, want)
}

// messagesOfType returns the Message of every published event of one type, in
// publication order.
func (h *harness) messagesOfType(eventType string) []string {
	h.t.Helper()

	events := h.eventsOfType(eventType)
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.Message)
	}
	return out
}

// TestApplySuspendsAPluginWhoseRequirementIsAbsent is the base case: a plugin
// is mounted, its guest is alive, and its tools are NOT in the registry,
// because the tool it calls into is not there.
func TestApplySuspendsAPluginWhoseRequirementIsAbsent(t *testing.T) {
	h := newHarness(t)
	_, b, _ := h.depChain()

	h.apply(b)

	h.wantState(depBPluginName, StateSuspended, depAToolName)
	// The withdrawal is the point: an advertised tool whose every call must
	// fail is worse than an absent one.
	wantStrings(t, "registry after suspension", h.toolNames(), nil)
	// The plugin itself is still mounted — suspension is not an unload, and the
	// guest keeps whatever it holds.
	wantStrings(t, "ledger instance owners", h.owners(), []string{"plugin:" + depBPluginName + "@0.1.0"})

	messages := h.messagesOfType(RuntimeEventSuspended)
	if len(messages) != 1 {
		t.Fatalf("plugin/suspended events: got %d (%v), want 1", len(messages), messages)
	}
	for _, want := range []string{"plugin=" + depBPluginName, "unresolved=[" + depAToolName + "]", "cascade=no"} {
		if !strings.Contains(messages[0], want) {
			t.Fatalf("plugin/suspended message %q does not contain %q", messages[0], want)
		}
	}
}

// TestApplyResolvesARequirementAgainstAHostBuiltinTool covers the other half of
// resolvability, the one the dependency graph never answers: a "requires" entry
// that names a tool NO mounted plugin provides is satisfied by the host's own
// registry, and only externalToolNames can say so.
//
// Every other test in this file has each required tool provided by another
// plugin under test, which makes externalToolNames' whole result empty — so an
// implementation that returned an empty set unconditionally would pass all of
// them while suspending, in production, every single plugin that calls into a
// builtin. That is what this test is here to catch, from both directions: the
// builtin is present and the plugin runs, then the builtin goes away and the
// plugin is suspended naming it.
func TestApplyResolvesARequirementAgainstAHostBuiltinTool(t *testing.T) {
	h := newHarness(t)

	// Registered straight on the registry, so it belongs to no plugin: this is
	// a host tool, exactly what externalToolNames exists to recognise.
	revokeBuiltin := h.registry.Register(hostBuiltinToolName, tool.HandlerFunc(
		func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{}, nil
		}))
	t.Cleanup(revokeBuiltin)

	entry := h.writeDep("hostdep", builtinDepPluginName, builtinDepToolName, hostBuiltinToolName)

	h.apply(entry)

	h.wantState(builtinDepPluginName, StateLoaded)
	wantStrings(t, "registry while the host builtin is present", h.toolNames(),
		[]string{hostBuiltinToolName, builtinDepToolName})
	if got := len(h.messagesOfType(RuntimeEventSuspended)); got != 0 {
		t.Fatalf("plugin/suspended events while the required builtin is registered: got %d (%v), want 0",
			got, h.messagesOfType(RuntimeEventSuspended))
	}

	// The host tool goes away — an ungated capability, a revoked builtin — and
	// the plugin that calls into it must go down with it.
	revokeBuiltin()

	h.apply(entry)

	h.wantState(builtinDepPluginName, StateSuspended, hostBuiltinToolName)
	wantStrings(t, "registry after the host builtin was revoked", h.toolNames(), nil)
}

// TestApplyResumesAPluginWhenItsRequirementArrives asserts the state comes back
// on the SAME instance: no plugin/loaded is published for the resumed plugin,
// so its guest — and everything in its linear memory — was never rebuilt.
func TestApplyResumesAPluginWhenItsRequirementArrives(t *testing.T) {
	h := newHarness(t)
	a, b, _ := h.depChain()

	h.apply(b)
	h.wantState(depBPluginName, StateSuspended, depAToolName)

	h.apply(a, b)

	h.wantState(depAPluginName, StateLoaded)
	h.wantState(depBPluginName, StateLoaded)
	wantStrings(t, "registry after resume", h.toolNames(), []string{depAToolName, depBToolName})

	// One mount for b across both applies: a resume must not remount.
	loads := 0
	for _, message := range h.messagesOfType(RuntimeEventLoaded) {
		if strings.Contains(message, "plugin="+depBPluginName+" ") {
			loads++
		}
	}
	if loads != 1 {
		t.Fatalf("plugin/loaded events for %s: got %d, want 1 (a resume must reuse the mounted instance)",
			depBPluginName, loads)
	}
	resumed := h.messagesOfType(RuntimeEventResumed)
	if len(resumed) != 1 || !strings.Contains(resumed[0], "plugin="+depBPluginName) {
		t.Fatalf("plugin/resumed events: got %v, want one naming %s", resumed, depBPluginName)
	}
}

// TestApplySuspendsAgainWhenTheRequirementLeaves closes the loop: the state is
// not one-way, and the second suspension is decided from the CURRENT plugin
// set rather than from what an earlier Apply remembered.
func TestApplySuspendsAgainWhenTheRequirementLeaves(t *testing.T) {
	h := newHarness(t)
	a, b, _ := h.depChain()

	h.apply(a, b)
	h.wantState(depBPluginName, StateLoaded)

	h.apply(b)

	h.wantState(depBPluginName, StateSuspended, depAToolName)
	wantStrings(t, "registry after the requirement left", h.toolNames(), nil)
}

// TestApplySuspendsTheWholeChainWhenTheRootLeaves is the cascade: dep-c does
// not require anything dep-a provides, so a Loader that only suspended DIRECT
// dependents would leave dep_c_tool in the registry, advertising a tool whose
// only implementation calls into a tool that is gone.
//
// This is also the test that catches the "a suspended plugin still counts as a
// provider" mistake: dep-b's contributions are withdrawn in this same Apply, so
// anything that decided dep-c's fate from the registry snapshot taken before
// the suspensions would find dep_b_tool present and keep dep-c active.
func TestApplySuspendsTheWholeChainWhenTheRootLeaves(t *testing.T) {
	h := newHarness(t)
	a, b, c := h.depChain()

	h.apply(a, b, c)
	wantStrings(t, "registry with the whole chain up", h.toolNames(),
		[]string{depAToolName, depBToolName, depCToolName})

	h.apply(b, c)

	h.wantState(depBPluginName, StateSuspended, depAToolName)
	h.wantState(depCPluginName, StateSuspended, depBToolName)
	wantStrings(t, "registry after the root left", h.toolNames(), nil)

	// The cascade is called out in the event, because "the tool you depend on
	// was never there" and "the plugin that provides it was suspended too" are
	// different things to an operator reading the stream.
	var cascaded []string
	for _, message := range h.messagesOfType(RuntimeEventSuspended) {
		if strings.Contains(message, "cascade=yes") {
			cascaded = append(cascaded, message)
		}
	}
	if len(cascaded) != 1 || !strings.Contains(cascaded[0], "plugin="+depCPluginName) {
		t.Fatalf("cascade=yes suspensions: got %v, want exactly one for %s", cascaded, depCPluginName)
	}
}

// TestApplyResumesAWholeChainInOneApply is the ordering requirement: dep-b has
// to come back before dep-c, or dep-c is brought up against a provider that is
// still suspended. ONE Apply must converge the whole chain — "run it again and
// it settles" is not something a convergence function may ask of an operator.
func TestApplyResumesAWholeChainInOneApply(t *testing.T) {
	h := newHarness(t)
	a, b, c := h.depChain()

	h.apply(b, c)
	h.wantState(depBPluginName, StateSuspended, depAToolName)
	h.wantState(depCPluginName, StateSuspended, depBToolName)

	h.apply(a, b, c)

	h.wantState(depAPluginName, StateLoaded)
	h.wantState(depBPluginName, StateLoaded)
	h.wantState(depCPluginName, StateLoaded)
	wantStrings(t, "registry after the chain resumed", h.toolNames(),
		[]string{depAToolName, depBToolName, depCToolName})

	// Neither b nor c was remounted: each has exactly the ONE plugin/loaded its
	// first mount published, so both came back on the instance they kept.
	for _, name := range []string{depBPluginName, depCPluginName} {
		loads := 0
		for _, message := range h.messagesOfType(RuntimeEventLoaded) {
			if strings.Contains(message, "plugin="+name+" ") {
				loads++
			}
		}
		if loads != 1 {
			t.Fatalf("plugin/loaded events for %s: got %d, want 1 (a resume must reuse the mounted instance)",
				name, loads)
		}
	}
	if got := len(h.messagesOfType(RuntimeEventResumed)); got != 2 {
		t.Fatalf("plugin/resumed events: got %d (%v), want 2",
			got, h.messagesOfType(RuntimeEventResumed))
	}
}

// TestApplyRefusesACyclicManifestWithoutTouchingMountedPlugins pins decision 4:
// a cycle is a manifest mistake, and a manifest mistake must not tear down the
// plugins that are working. The convergence reports it and leaves every mounted
// plugin in the state it was already in.
func TestApplyRefusesACyclicManifestWithoutTouchingMountedPlugins(t *testing.T) {
	h := newHarness(t)
	echo := h.writeEcho("1.0.0")

	h.apply(echo)
	wantStrings(t, "ledger instance owners before the cyclic manifest", h.owners(),
		[]string{"plugin:" + echoPluginName + "@1.0.0"})
	wantStrings(t, "registry before the cyclic manifest", h.toolNames(), []string{echoToolName})

	// dep-a and dep-b require each other: an unresolvable activation order.
	cycleA := h.writeDep("depa", depAPluginName, depAToolName, depBToolName)
	cycleB := h.writeDep("depb", depBPluginName, depBToolName, depAToolName)

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{echo, cycleA, cycleB}}, h.root)
	if err == nil {
		t.Fatalf("Apply must refuse a cyclic manifest, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("Apply error %v does not name the cycle", err)
	}

	// The plugin that was already running is untouched, in the ledger and in
	// the registry alike. Both are compared EXACTLY, not for containment:
	// "changes nothing" is also violated by an Apply that adds something, and
	// the two cyclic plugins are listed here because pass 3 does mount them —
	// the cycle is only discovered afterwards, by a convergence that then
	// declines to move anybody.
	wantStrings(t, "ledger instance owners after the cyclic manifest", h.owners(), []string{
		"plugin:" + depAPluginName + "@0.1.0",
		"plugin:" + depBPluginName + "@0.1.0",
		"plugin:" + echoPluginName + "@1.0.0",
	})
	wantStrings(t, "registry after the cyclic manifest", h.toolNames(),
		[]string{depAToolName, depBToolName, echoToolName})
	h.wantState(echoPluginName, StateLoaded)
	// Nothing was suspended either: the graph never produced an answer to act
	// on, so no state changed.
	for _, name := range []string{depAPluginName, depBPluginName} {
		if row := h.statusOf(name); row.State == StateSuspended {
			t.Fatalf("plugin %q was suspended over a cyclic manifest (%v); a graph that errored decides nothing",
				name, row)
		}
	}
	if got := len(h.messagesOfType(RuntimeEventSuspended)); got != 0 {
		t.Fatalf("plugin/suspended events over a cyclic manifest: got %d, want 0", got)
	}
}

// TestApplyReportsAResumeWhoseToolNameWasTaken is the error path of the resume
// side: a suspended plugin's tool names are FREE, so another contributor may
// legitimately take one while it is down. The plugin then cannot come back, and
// that has to travel out as an error rather than as a silent half-resume.
//
// The chain is three levels deep because the interesting case is a failure in
// the MIDDLE of one. The graph, resolved before anything moved, says all three
// may be active; mid-b's resume then fails, so mb_tool never gets registered,
// and mid-c — which the graph cleared to come back — has to notice that against
// the LIVE registry and refuse rather than return to the model advertising a
// tool whose implementation calls into nothing. Both failures travel out of the
// same Apply.
//
// The squatter takes mb_aux, a name mid-b provides and nobody requires, rather
// than mb_tool itself: taking mb_tool would leave the name registered (by the
// squatter) and mid-c would then be entitled to come back, which tests the
// opposite of the cascade this is about.
func TestApplyReportsAResumeWhoseToolNameWasTaken(t *testing.T) {
	h := newHarness(t)
	a, b, c := h.midChain()

	h.apply(b, c)
	h.wantState(midBPluginName, StateSuspended, midAToolName)
	h.wantState(midCPluginName, StateSuspended, midBToolName)

	// Somebody else claims one of mid-b's names while mid-b is suspended.
	revoke := h.registry.Register(midBAuxName, tool.HandlerFunc(
		func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{}, nil
		}))
	t.Cleanup(revoke)

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{a, b, c}}, h.root)
	if err == nil {
		t.Fatalf("Apply must report a resume whose tool name was taken, got nil")
	}
	// Both hops are named: the resume that failed on the taken name, and the one
	// downstream of it that refused because of the first one's failure.
	for _, want := range []string{midBPluginName, midBAuxName, midCPluginName, midBToolName} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Apply error %v does not name %q; a mid-chain resume failure has to report both hops",
				err, want)
		}
	}

	// mid-a came up regardless — one entry's failure never aborts the others —
	// and neither of the two downstream plugins is half-resumed.
	h.wantState(midAPluginName, StateLoaded)
	// mid-b's blocker is NOT a missing dependency any more: ma_tool is back and
	// registered. SuspendedBy has to say so — naming ma_tool here would send an
	// operator after a dependency that is already satisfied — and LastError is
	// what carries the taken name.
	h.wantState(midBPluginName, StateSuspended)
	// mid-c's blocker, by contrast, really is a missing dependency: mb_tool is
	// unregistered because mid-b never came back.
	h.wantState(midCPluginName, StateSuspended, midBToolName)
	for _, name := range []string{midBPluginName, midCPluginName} {
		if row := h.statusOf(name); row.LastError == "" {
			t.Fatalf("plugin %q reports no LastError after a failed resume: %v", name, row)
		}
	}
	// Nothing either of them provides reached the registry: mid-a's tool and the
	// squatter's are all that is there.
	wantStrings(t, "registry after the mid-chain resume failed", h.toolNames(),
		[]string{midAToolName, midBAuxName})
}

// suspendApplyRounds is how many times TestApplyConvergesToTheSameDependencyState
// EveryRound applies. It is a LITERAL bound, not a "until it settles" loop: the
// feature under test is exactly what a stability loop would be waiting for, and
// this repository has already paid once for a test whose only termination
// condition was the thing being tested.
const suspendApplyRounds = 5

// suspendOwnerCeiling is the most ledger instance owners
// TestApplyConvergesToTheSameDependencyStateEveryRound may ever hold: one per
// plugin IT deploys, which is two — dep-b and dep-c; dep-a is written but never
// applied, which is what keeps the chain suspended. It is asserted in EVERY
// round, so a convergence that leaked an instance per round fails on the first
// leaked one rather than after "enough" rounds.
const suspendOwnerCeiling = 2

// TestApplyConvergesToTheSameDependencyStateEveryRound pins idempotence: an
// unchanged manifest reaches the same suspension state every time, without
// suspending an already-suspended plugin (which host.Plugin.Suspend refuses)
// and without leaking an instance per round.
func TestApplyConvergesToTheSameDependencyStateEveryRound(t *testing.T) {
	h := newHarness(t)
	_, b, c := h.depChain()

	for round := 0; round < suspendApplyRounds; round++ {
		h.apply(b, c)

		if got := len(h.owners()); got > suspendOwnerCeiling {
			t.Fatalf("round %d: %d ledger instance owners exceeds the ceiling of %d: %v",
				round, got, suspendOwnerCeiling, h.owners())
		}
		h.wantState(depBPluginName, StateSuspended, depAToolName)
		h.wantState(depCPluginName, StateSuspended, depBToolName)
		wantStrings(t, fmt.Sprintf("round %d registry", round), h.toolNames(), nil)
	}

	// One suspension each, not one per round: an already-suspended plugin is
	// left alone.
	if got := len(h.messagesOfType(RuntimeEventSuspended)); got != 2 {
		t.Fatalf("plugin/suspended events over %d rounds: got %d (%v), want 2",
			suspendApplyRounds, got, h.messagesOfType(RuntimeEventSuspended))
	}
}
