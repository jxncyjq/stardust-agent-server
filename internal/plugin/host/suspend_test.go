package host

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/toolauth"
)

// readPoolProbe reads the fixture's guest-side instrumentation (opProbe, see
// testdata/README.md) through the plugin's own pool.
//
// It is the only assertion in this file that can tell "the same guest instance
// is still there" from "a fresh one was built and the tool name merely came
// back": alloc_calls counts every plugin_alloc entry this INSTANCE has seen, so
// it only ever climbs while an instance lives and restarts near zero on a new
// one. Reading it through the pool rather than through an *Instance is
// deliberate — the pool is the thing Suspend must not disturb.
func readPoolProbe(t *testing.T, p *pool) probeReport {
	t.Helper()

	out, err := p.call(context.Background(), opProbe, nil)
	if err != nil {
		t.Fatalf("pool.call(opProbe): %v", err)
	}
	var got probeReport
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode probe report %s: %v", out, err)
	}
	return got
}

// assertOrderedLabels fails unless owner holds exactly want, in that order.
// The order is asserted, not just the set: the ledger disposes in reverse
// filing order, so it is the order that decides whether tools are withdrawn
// before the pool is drained.
func assertOrderedLabels(t *testing.T, ledger *lifecycle.Ledger, owner lifecycle.Owner, want ...string) {
	t.Helper()

	got := ledger.Snapshot()[owner]
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ledger.Snapshot()[%s] = %v, want %v", owner, got, want)
	}
}

// TestToolsOwnerDerivesTheContributionSideOwner pins the spelling both halves
// of a split activation agree on. It is a derivation rather than a stored
// field so that a caller holding only the instance owner — the Loader, the CLI
// drain — can reach the contributions without a directory.
func TestToolsOwnerDerivesTheContributionSideOwner(t *testing.T) {
	if got, want := ToolsOwner(testOwner), lifecycle.Owner("plugin:legion-test-plugin/tools"); got != want {
		t.Errorf("ToolsOwner(%s) = %s, want %s", testOwner, got, want)
	}
	if ToolsOwner(testOwner) == testOwner {
		t.Error("ToolsOwner returned the instance owner itself: the two sides must be separable")
	}
}

// TestActivateSplitsTheOwners covers the filing structure the whole task rests
// on: the wasm resources stay under the instance owner, the contributions move
// to the tools owner, and the instance owner additionally holds the ONE entry
// that reaches across to them — filed after both wasm entries, so reverse
// disposal withdraws the tools first, then the pool, then the runtime.
func TestActivateSplitsTheOwners(t *testing.T) {
	c := newContribution(t)

	assertOrderedLabels(t, c.ledger, testOwner, ledgerLabelRuntime, ledgerLabelPool, ledgerLabelContributions)
	assertOrderedLabels(t, c.ledger, ToolsOwner(testOwner),
		"tool:"+fixtureProvidedTool, gateableLabel(fixtureProvidedTool))
	if c.plugin.Suspended() {
		t.Error("Plugin.Suspended() = true straight after Activate, want false")
	}
}

// TestSuspendWithdrawsOnlyTheContributions is invariant 1: after Suspend the
// model can no longer see or call the plugin's tools and no per-agent config
// can name them, while the instance side is untouched — the runtime and the
// pool are still filed, and the guest still answers, which is what makes a
// later Resume cheap instead of a re-activation.
func TestSuspendWithdrawsOnlyTheContributions(t *testing.T) {
	c := newContribution(t)

	// Prove the tool answered BEFORE the suspension, so "not found" afterwards
	// is a withdrawal and not a contribution that never happened.
	if _, err := c.execute(t, "call-1", map[string]string{"text": "hi"}); err != nil {
		t.Fatalf("Execute before Suspend: %v", err)
	}

	if err := c.plugin.Suspend(context.Background()); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if !c.plugin.Suspended() {
		t.Error("Plugin.Suspended() = false after Suspend, want true")
	}
	if _, err := c.execute(t, "call-2", map[string]string{"text": "hi"}); !errors.Is(err, tool.ErrToolNotFound) {
		t.Errorf("Execute after Suspend error = %v, want %v", err, tool.ErrToolNotFound)
	}
	for _, descriptor := range c.registry.Descriptors() {
		if descriptor.Name == fixtureProvidedTool {
			t.Errorf("Registry.Descriptors() still lists %q after Suspend: the model would keep offering "+
				"a tool that cannot work", fixtureProvidedTool)
		}
	}
	if toolauth.IsGateable(fixtureProvidedTool) {
		t.Errorf("toolauth.IsGateable(%q) = true after Suspend, want false: the gateable entry outlived "+
			"the contribution it describes", fixtureProvidedTool)
	}

	if labels := c.ledger.Snapshot()[ToolsOwner(testOwner)]; len(labels) != 0 {
		t.Errorf("ledger.Snapshot()[%s] = %v after Suspend, want nothing", ToolsOwner(testOwner), labels)
	}
	// The instance side is what a suspension must NOT touch.
	assertOrderedLabels(t, c.ledger, testOwner, ledgerLabelRuntime, ledgerLabelPool, ledgerLabelContributions)
	if _, err := c.plugin.pool.call(context.Background(), opEcho, []byte(`{"name":"legion","n":21}`)); err != nil {
		t.Errorf("the suspended plugin's guest no longer answers: %v; Suspend drained the pool it must keep", err)
	}
}

// TestResumeRestoresTheContributionsOnTheSameInstance is invariant 2. The tool
// coming back is the weak half of it: the strong half is that the guest behind
// it is the one that was there before, asserted from the guest's own
// cross-call counters rather than from the name reappearing.
func TestResumeRestoresTheContributionsOnTheSameInstance(t *testing.T) {
	c := newContribution(t)

	// Several calls first, so the counter is unmistakably past what a fresh
	// instance could report for the single call made after the resume.
	for i, callID := range []string{"warm-1", "warm-2", "warm-3"} {
		if _, err := c.execute(t, callID, map[string]string{"text": "hi"}); err != nil {
			t.Fatalf("Execute #%d before Suspend: %v", i, err)
		}
	}
	before := readPoolProbe(t, c.plugin.pool)
	if !before.Initialized {
		t.Fatal("the fixture reports it was never initialized: this test cannot see the state it claims to pin")
	}
	if before.AllocCalls == 0 {
		t.Fatal("the fixture reports 0 plugin_alloc calls after three tool calls: the counter this test " +
			"reads is not moving, so it could not tell a surviving instance from a fresh one")
	}

	if err := c.plugin.Suspend(context.Background()); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := c.plugin.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if c.plugin.Suspended() {
		t.Error("Plugin.Suspended() = true after Resume, want false")
	}
	if !toolauth.IsGateable(fixtureProvidedTool) {
		t.Errorf("toolauth.IsGateable(%q) = false after Resume, want true: the tool is callable again "+
			"but no per-agent config can disable it — an authorization bypass", fixtureProvidedTool)
	}
	result, err := c.execute(t, "call-after-resume", map[string]string{"text": "again"})
	if err != nil {
		t.Fatalf("Execute after Resume: %v", err)
	}
	// The fixture answers "<tool>:<arguments as JSON>", so this proves the
	// restored handler really reaches the guest rather than merely occupying
	// the name.
	if want := fixtureProvidedTool + `:{"text":"again"}`; result.Output != want {
		t.Errorf("result.Output = %q, want %q", result.Output, want)
	}

	after := readPoolProbe(t, c.plugin.pool)
	if !after.Initialized || after.AllocCalls <= before.AllocCalls {
		t.Errorf("the guest reports initialized=%t alloc_calls=%d after the resume, want initialized and "+
			"strictly more than the %d it had before: a resumed plugin must keep serving from the SAME "+
			"instance (a fresh one starts its counters at zero), not from a rebuilt pool",
			after.Initialized, after.AllocCalls, before.AllocCalls)
	}
}

// TestSuspendTwiceIsAnError and TestResumeWhileActiveIsAnError are invariant 3.
// The state machine is strict on purpose: a Loader that suspended a plugin
// twice, or resumed one that never went down, is reasoning from a stale view of
// the plugin set, and a silent no-op would let it keep doing so.
func TestSuspendTwiceIsAnError(t *testing.T) {
	c := newContribution(t)

	if err := c.plugin.Suspend(context.Background()); err != nil {
		t.Fatalf("first Suspend: %v", err)
	}
	err := c.plugin.Suspend(context.Background())
	if err == nil {
		t.Fatal("the second Suspend succeeded, want an error naming the current state")
	}
	for _, want := range []string{fixtureManifestName, "suspended"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if !c.plugin.Suspended() {
		t.Error("Plugin.Suspended() = false after a refused second Suspend, want the state unchanged")
	}
}

func TestResumeWhileActiveIsAnError(t *testing.T) {
	c := newContribution(t)

	err := c.plugin.Resume(context.Background())
	if err == nil {
		t.Fatal("Resume on an active plugin succeeded, want an error naming the current state")
	}
	for _, want := range []string{fixtureManifestName, "active"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// A refused Resume must not have contributed a second time — that would
	// panic on the duplicate name, and a passing assertion here proves the
	// state check came first.
	assertOrderedLabels(t, c.ledger, ToolsOwner(testOwner),
		"tool:"+fixtureProvidedTool, gateableLabel(fixtureProvidedTool))
}

// TestResumeRefusesToolNamesTakenWhileSuspended is the trap this task exists
// not to fall into. A suspended plugin's names are free, so another
// contributor may legitimately take one; both the registry and the gateable
// catalog answer a duplicate with a PANIC, so Resume must check before it
// contributes and report the conflicting names as an error.
//
// Both halves are covered separately because they fail independently: an
// implementation that only asked the registry would still panic in toolauth.
func TestResumeRefusesToolNamesTakenWhileSuspended(t *testing.T) {
	t.Run("name taken in the registry", func(t *testing.T) {
		c := newContribution(t)
		if err := c.plugin.Suspend(context.Background()); err != nil {
			t.Fatalf("Suspend: %v", err)
		}

		squatter := c.registry.RegisterDescriptor(
			tool.Descriptor{Name: fixtureProvidedTool, Description: "someone else took the name"},
			tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
				return domain.ToolResult{CallID: call.ID, Success: true, Output: "squatter"}, nil
			}))
		t.Cleanup(squatter)

		assertResumeConflict(t, c)
	})

	t.Run("name taken in the gateable catalog", func(t *testing.T) {
		c := newContribution(t)
		if err := c.plugin.Suspend(context.Background()); err != nil {
			t.Fatalf("Suspend: %v", err)
		}

		// A gateable name with no registry entry: the registry would let the
		// registration through and toolauth is what refuses, so this is the
		// half a "the registry is enough" pre-check would hide.
		revoke := toolauth.Contribute(toolauth.GateableTool{
			Name:        fixtureProvidedTool,
			Description: "someone else took the name",
		})
		t.Cleanup(revoke)

		assertResumeConflict(t, c)
	})
}

// assertResumeConflict runs the Resume that must be refused and holds it to the
// whole contract: an error rather than a panic, the conflicting name in the
// message, and a plugin left exactly as suspended as it was, with nothing
// half-contributed.
func assertResumeConflict(t *testing.T, c *contribution) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Resume panicked on a taken tool name (%v), want an error naming it: "+
				"operator-authored data that collides is an error, not an invariant violation", r)
		}
	}()

	err := c.plugin.Resume(context.Background())
	if err == nil {
		t.Fatal("Resume succeeded although another contributor holds the plugin's tool name")
	}
	for _, want := range []string{fixtureManifestName, fixtureProvidedTool} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if !c.plugin.Suspended() {
		t.Error("Plugin.Suspended() = false after a refused Resume, want the plugin to stay suspended")
	}
	if labels := c.ledger.Snapshot()[ToolsOwner(testOwner)]; len(labels) != 0 {
		t.Errorf("a refused Resume filed %v under %s, want nothing: it must contribute all or nothing",
			labels, ToolsOwner(testOwner))
	}
}

// TestDisposeOwnerWhileSuspendedTearsDownCleanly is invariant 4. The Loader,
// the CLI drain and ServeResult.Close all know only the instance owner, so
// disposing it must still take everything down — and must not report a failure
// merely because the contribution side was already empty.
func TestDisposeOwnerWhileSuspendedTearsDownCleanly(t *testing.T) {
	instanceCloses := countInstanceCloses(t)
	c := newContribution(t)

	if err := c.plugin.Suspend(context.Background()); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := c.ledger.DisposeOwner(testOwner); err != nil {
		t.Fatalf("DisposeOwner while suspended: %v", err)
	}

	if snapshot := c.ledger.Snapshot(); len(snapshot) != 0 {
		t.Errorf("ledger.Snapshot() = %v after DisposeOwner, want empty", snapshot)
	}
	if got := instanceCloses.Load(); got != 1 {
		t.Errorf("closeInstance ran %d times, want exactly 1: a suspended plugin still holds its one "+
			"pooled instance, and the drain is what must close it", got)
	}
	if _, err := c.plugin.pool.call(context.Background(), opEcho, nil); err == nil {
		t.Error("the plugin still answers a call after DisposeOwner, want an error: its pool was not drained")
	}
}

// TestDisposeOwnerAfterResumeRemovesTheRestoredTool is the same invariant for
// the entries RESUME filed: they are not the ones activation filed, and the
// single cross-owner entry under the instance owner is what still reaches them.
func TestDisposeOwnerAfterResumeRemovesTheRestoredTool(t *testing.T) {
	c := newContribution(t)

	if err := c.plugin.Suspend(context.Background()); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := c.plugin.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := c.ledger.DisposeOwner(testOwner); err != nil {
		t.Fatalf("DisposeOwner after Resume: %v", err)
	}

	if _, err := c.execute(t, "call-after-dispose", nil); !errors.Is(err, tool.ErrToolNotFound) {
		t.Errorf("Execute after DisposeOwner error = %v, want %v: the tools a Resume restored "+
			"outlived the plugin", err, tool.ErrToolNotFound)
	}
	if toolauth.IsGateable(fixtureProvidedTool) {
		t.Errorf("toolauth.IsGateable(%q) = true after DisposeOwner, want false", fixtureProvidedTool)
	}
	if snapshot := c.ledger.Snapshot(); len(snapshot) != 0 {
		t.Errorf("ledger.Snapshot() = %v after DisposeOwner, want empty", snapshot)
	}
}

// TestSuspendResumeCyclesKeepOneInstance walks the state machine a fixed,
// small number of times — the count is a literal, and the instance ceiling is
// asserted every round — so a leak that only shows up on repetition is caught
// without the test ever becoming an unbounded "repeat until stable" loop.
func TestSuspendResumeCyclesKeepOneInstance(t *testing.T) {
	instanceCloses := countInstanceCloses(t)
	c := newContribution(t)

	previous := readPoolProbe(t, c.plugin.pool)
	const rounds = 3
	for round := 1; round <= rounds; round++ {
		if err := c.plugin.Suspend(context.Background()); err != nil {
			t.Fatalf("round %d Suspend: %v", round, err)
		}
		if err := c.plugin.Resume(context.Background()); err != nil {
			t.Fatalf("round %d Resume: %v", round, err)
		}
		if _, err := c.execute(t, "cycle", map[string]string{"round": "x"}); err != nil {
			t.Fatalf("round %d Execute: %v", round, err)
		}

		// The ceiling: MaxInstances is 1 and nothing was closed, so the guest
		// this probe reaches can only be the one instance the plugin started
		// with — and its counters prove it never restarted.
		if got := instanceCloses.Load(); got != 0 {
			t.Fatalf("round %d: closeInstance ran %d times, want 0: suspending must not discard the "+
				"plugin's instance", round, got)
		}
		current := readPoolProbe(t, c.plugin.pool)
		if !current.Initialized || current.AllocCalls <= previous.AllocCalls {
			t.Fatalf("round %d: guest reports initialized=%t alloc_calls=%d, want initialized and more "+
				"than the previous round's %d", round, current.Initialized, current.AllocCalls, previous.AllocCalls)
		}
		previous = current
	}
	assertOrderedLabels(t, c.ledger, ToolsOwner(testOwner),
		"tool:"+fixtureProvidedTool, gateableLabel(fixtureProvidedTool))
}

// TestActivateRefusesAnOccupiedToolsOwner extends the owner-exclusivity
// precondition to the side this task created: activation now files into a
// second owner, so entries already sitting there would be torn down by this
// plugin's disposal and would make the snapshot lie about who contributed what.
func TestActivateRefusesAnOccupiedToolsOwner(t *testing.T) {
	ledger := lifecycle.NewLedger()
	ledger.Add(ToolsOwner(testOwner), "squatter", func() error { return nil })

	p, err := Activate(context.Background(), ledger, testOwner, fixtureSpec(t))
	if err == nil {
		t.Fatal("Activate succeeded although the tools owner already holds an entry")
	}
	if p != nil {
		t.Errorf("Activate returned a plugin (%+v) together with an error", p)
	}
	for _, want := range []string{string(ToolsOwner(testOwner)), "squatter"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if labels := ledger.Snapshot()[testOwner]; len(labels) != 0 {
		t.Errorf("the refused activation filed %v under %s, want nothing", labels, testOwner)
	}
}
