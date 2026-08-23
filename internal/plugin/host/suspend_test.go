package host

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// TestResumeAfterDisposalIsRefused closes the gap between "suspended" and
// "gone": both leave the contribution owner empty, but a disposed plugin has
// lost the entry that would revoke anything filed now — so a Resume that went
// ahead would put a tool the drained pool can never serve into the registry,
// and a name into the PROCESS-GLOBAL gateable catalog, with nothing left that
// could ever take either out again.
func TestResumeAfterDisposalIsRefused(t *testing.T) {
	c := newContribution(t)

	if err := c.plugin.Suspend(context.Background()); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := c.ledger.DisposeOwner(testOwner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}

	err := c.plugin.Resume(context.Background())
	if err == nil {
		t.Fatal("Resume on a disposed plugin succeeded, want an error: its contributions could never be revoked")
	}
	for _, want := range []string{fixtureManifestName, "disposed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	for _, descriptor := range c.registry.Descriptors() {
		if descriptor.Name == fixtureProvidedTool {
			t.Errorf("a refused Resume registered %q anyway", fixtureProvidedTool)
		}
	}
	if toolauth.IsGateable(fixtureProvidedTool) {
		t.Errorf("a refused Resume left %q in the gateable catalog, which nothing could take out again",
			fixtureProvidedTool)
	}
	if snapshot := c.ledger.Snapshot(); len(snapshot) != 0 {
		t.Errorf("ledger.Snapshot() = %v after a refused Resume, want empty", snapshot)
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

// TestSuspendAfterDisposalIsRefused is the third state on the OTHER transition.
// lifecycle.Ledger.DisposeOwner answers an unknown owner with nil, so a Suspend
// that only asked the ledger would report a withdrawal it never performed and
// leave Suspended() describing a plugin that no longer exists as merely
// resting.
func TestSuspendAfterDisposalIsRefused(t *testing.T) {
	c := newContribution(t)

	// Disposed while ACTIVE, so "already suspended" cannot be what refuses it.
	if err := c.ledger.DisposeOwner(testOwner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}

	err := c.plugin.Suspend(context.Background())
	if err == nil {
		t.Fatal("Suspend on a disposed plugin succeeded, want an error naming the state: there was nothing " +
			"left to withdraw, so reporting success describes a withdrawal that never happened")
	}
	for _, want := range []string{fixtureManifestName, "disposed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if c.plugin.Suspended() {
		t.Error("Plugin.Suspended() = true after a refused Suspend, want false: a disposed plugin is gone, " +
			"not suspended, and a caller reading true would wait for a Resume that can never come")
	}
}

// TestResumeRefusesItsOwnLeftoverContributions covers the residue a
// contribution that died part-way through leaves: entries of the plugin's OWN
// under its own contribution owner. Reporting those as names another
// contributor holds is misleading and unactionable, and contributing over them
// would panic on the duplicate — so they are named for what they are.
func TestResumeRefusesItsOwnLeftoverContributions(t *testing.T) {
	c := newContribution(t)

	if err := c.plugin.Suspend(context.Background()); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	// Stands in for a contributeTools that panicked between its two halves: the
	// entry is filed and the plugin is still suspended. The ledger is the only
	// place that records it, which is why Resume has to look there.
	c.ledger.Add(ToolsOwner(testOwner), "tool:"+fixtureProvidedTool, func() error { return nil })

	err := c.plugin.Resume(context.Background())
	if err == nil {
		t.Fatal("Resume succeeded with its own half-filed contributions still under its contribution owner, " +
			"want an error: contributing over them panics on the duplicate registration")
	}
	for _, want := range []string{fixtureManifestName, string(ToolsOwner(testOwner)), "tool:" + fixtureProvidedTool} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "another contributor") {
		t.Errorf("error %q blames another contributor for the plugin's own leftovers", err)
	}
	if !c.plugin.Suspended() {
		t.Error("Plugin.Suspended() = false after a refused Resume, want the plugin to stay suspended")
	}
}

// disposalOvertakeWindow is how long TestResumeAndDisposalCannotInterleave
// holds a resume inside its critical section while a DisposeOwner runs against
// it. It is the one deliberately NEGATIVE wait in this package: the property
// under test is that the disposal cannot get past Plugin.markDisposed while the
// resume holds the lock, so this bound is expected to expire. Disposing a
// suspended plugin (an empty contribution owner, one pooled instance, one
// runtime) takes single-digit milliseconds when nothing holds it off, so a
// second is ample slack for a loaded machine and still bounds the test.
const disposalOvertakeWindow = time.Second

// TestResumeAndDisposalCannotInterleave is the concurrency half of the state
// machine. Both halves guard ONE outcome: contributions that no entry can
// revoke. They would sit in the tool registry and in the PROCESS-GLOBAL
// gateable catalog for the life of the process, served by a pool that has been
// drained — the exact leak the cross-owner entry exists to prevent, reached by
// two goroutines instead of by one caller mistake.
func TestResumeAndDisposalCannotInterleave(t *testing.T) {
	t.Run("a disposal already under way refuses the resume", func(t *testing.T) {
		// closeInstance is the seam that stops a disposal midway: the pool
		// drain runs AFTER the cross-owner entry's disposer (reverse filing
		// order), so a disposal paused here has already taken the contribution
		// side down and has not returned.
		draining, release := make(chan struct{}), make(chan struct{})
		var opened sync.Once
		var blockedTooLong atomic.Bool
		original := closeInstance
		closeInstance = func(ctx context.Context, inst *Instance) error {
			opened.Do(func() { close(draining) })
			select {
			case <-release:
			case <-time.After(30 * time.Second):
				blockedTooLong.Store(true)
			}
			return original(ctx, inst)
		}
		t.Cleanup(func() { closeInstance = original })

		c := newContribution(t)
		// A safety net, not part of the assertion: if the refusal below ever
		// stops working, this keeps a leaked gateable name from panicking every
		// later test in the package instead of just failing this one.
		t.Cleanup(func() { _ = c.ledger.DisposeOwner(ToolsOwner(testOwner)) })
		if err := c.plugin.Suspend(context.Background()); err != nil {
			t.Fatalf("Suspend: %v", err)
		}

		disposed := make(chan error, 1)
		go func() { disposed <- c.ledger.DisposeOwner(testOwner) }()
		select {
		case <-draining:
		case <-time.After(30 * time.Second):
			t.Fatal("the disposal never reached the pool drain within 30s")
		}

		err := c.plugin.Resume(context.Background())
		if err == nil {
			t.Error("Resume succeeded against a disposal already under way, want an error: the entry that " +
				"would revoke these contributions has already been disposed")
		} else {
			for _, want := range []string{fixtureManifestName, "disposed"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		}

		close(release)
		select {
		case derr := <-disposed:
			if derr != nil {
				t.Fatalf("DisposeOwner: %v", derr)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("DisposeOwner did not return within 60s of being released")
		}
		if blockedTooLong.Load() {
			t.Fatal("the paused drain hit its own 30s bound instead of being released")
		}
		assertNothingSurvivedTheDisposal(t, c)
	})

	t.Run("a disposal arriving mid-resume waits for it", func(t *testing.T) {
		c := newContribution(t)
		t.Cleanup(func() { _ = c.ledger.DisposeOwner(ToolsOwner(testOwner)) })
		if err := c.plugin.Suspend(context.Background()); err != nil {
			t.Fatalf("Suspend: %v", err)
		}

		var disposeErr error
		finished := make(chan struct{})
		var overtaken atomic.Bool
		original := resumeContributionBarrier
		resumeContributionBarrier = func() {
			go func() {
				disposeErr = c.ledger.DisposeOwner(testOwner)
				close(finished)
			}()
			select {
			case <-finished:
				overtaken.Store(true)
			case <-time.After(disposalOvertakeWindow):
			}
		}
		t.Cleanup(func() { resumeContributionBarrier = original })

		if err := c.plugin.Resume(context.Background()); err != nil {
			t.Fatalf("Resume: %v; it holds the lock, so the disposal has to wait for it rather than refuse it", err)
		}
		if overtaken.Load() {
			t.Error("the disposal ran to completion while the resume sat between its checks and its " +
				"contribution: everything the resume then filed is unrevocable")
		}
		select {
		case <-finished:
		case <-time.After(60 * time.Second):
			t.Fatal("DisposeOwner did not return within 60s of the resume releasing the plugin")
		}
		if disposeErr != nil {
			t.Fatalf("DisposeOwner: %v", disposeErr)
		}
		assertNothingSurvivedTheDisposal(t, c)
	})
}

// assertNothingSurvivedTheDisposal is the outcome both halves of
// TestResumeAndDisposalCannotInterleave exist for, whichever of the two won the
// lock: nothing filed, nothing callable, and — the one that cannot be undone —
// nothing left in the process-global gateable catalog.
func assertNothingSurvivedTheDisposal(t *testing.T, c *contribution) {
	t.Helper()

	if snapshot := c.ledger.Snapshot(); len(snapshot) != 0 {
		t.Errorf("ledger.Snapshot() = %v after the disposal, want empty", snapshot)
	}
	if toolauth.IsGateable(fixtureProvidedTool) {
		t.Errorf("toolauth.IsGateable(%q) = true after the disposal: the name is in the PROCESS-GLOBAL "+
			"catalog with nothing left that could ever take it out", fixtureProvidedTool)
	}
	for _, descriptor := range c.registry.Descriptors() {
		if descriptor.Name == fixtureProvidedTool {
			t.Errorf("Registry.Descriptors() still lists %q after the disposal: it is served by a drained "+
				"pool, so every call to it must fail", fixtureProvidedTool)
		}
	}
}
