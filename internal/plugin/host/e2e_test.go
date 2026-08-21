package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/toolauth"
)

// e2eAgent is the identity the acceptance tests call as: a model-shaped call
// carries the agent Registry.Execute evaluates policy and audit against.
func e2eAgent() domain.Agent {
	return domain.Agent{ID: "agent-e2e", CompanyID: "co-1", Role: "developer", Status: domain.AgentActive}
}

// registryHasTool reports whether the registry advertises a tool by that name.
// It asks Descriptors() — the list the model is actually offered — rather than
// probing Execute, because "the model is offered it" and "a call would resolve"
// are two different claims, and the lifecycle test makes both separately.
func registryHasTool(registry *tool.Registry, name string) bool {
	for _, descriptor := range registry.Descriptors() {
		if descriptor.Name == name {
			return true
		}
	}
	return false
}

// TestPluginLifecycleFromActivationToDisposal is the acceptance test for one
// .wasm going through the whole lifecycle at the seams a deployment uses:
//
//	Activate → the tool is in Registry.Descriptors() and gateable → a
//	model-shaped Registry.Execute succeeds → DisposeOwner → ErrToolNotFound, no
//	longer gateable, no longer advertised, nothing left in the ledger, the pool
//	drained and the guest module closed.
//
// Every "after" assertion is paired with its "before": the tool is proven
// absent from the registry and from the gateable catalog BEFORE activation, and
// proven working BEFORE disposal, so neither half can pass on a state that was
// already true without this plugin.
func TestPluginLifecycleFromActivationToDisposal(t *testing.T) {
	ctx, guestClosed := watchGuestClose(context.Background())
	// The plugin's only guest instance is the pooled one (MaxInstances is 1 and
	// the manifest read borrows from the pool), so teardown must close exactly
	// one — counted, because guestClosed cannot say WHICH module was closed.
	instanceCloses := countInstanceCloses(t)

	audit := adapter.NewMemoryAuditLog()
	registry := tool.NewRegistry(nil, nil, nil).WithAuditLog(audit)
	ledger := lifecycle.NewLedger()

	spec := fixtureSpec(t)
	spec.Registry = registry

	if registryHasTool(registry, fixtureProvidedTool) {
		t.Fatalf("%q is advertised before activation: the assertions below would pass on a state that has "+
			"nothing to do with this plugin", fixtureProvidedTool)
	}
	if toolauth.IsGateable(fixtureProvidedTool) {
		t.Fatalf("%q is already gateable before activation: an earlier test leaked its contribution, and the "+
			"gateable assertions below would be vacuous", fixtureProvidedTool)
	}

	plugin, err := Activate(ctx, ledger, testOwner, spec)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// Unconditional, because toolauth.Contribute writes process-global state: a
	// test that left its contribution behind would panic the next contributor of
	// the same name. Disposal is asserted below; this is the safety net for a
	// failure between here and there.
	t.Cleanup(func() { _ = ledger.DisposeOwner(testOwner) })

	// 1. The model is offered the tool, with the descriptor the DEPLOYMENT
	// claimed rather than anything the guest chose: a plugin must not be able to
	// describe itself into another group or a lower risk level.
	var advertised tool.Descriptor
	found := false
	for _, descriptor := range registry.Descriptors() {
		if descriptor.Name == fixtureProvidedTool {
			advertised = descriptor
			found = true
		}
	}
	if !found {
		t.Fatalf("Registry.Descriptors() does not advertise %q after activation; got %v",
			fixtureProvidedTool, registry.Descriptors())
	}
	want := fixtureDescriptor()
	if advertised.Description != want.Description || advertised.Group != want.Group ||
		advertised.RiskLevel != want.RiskLevel || advertised.Timeout != want.Timeout {
		t.Errorf("advertised descriptor = %+v, want the deployment's claim %+v", advertised, want)
	}

	// 2. And the per-agent disabled_tools machinery can reach it. A tool that is
	// callable but not gateable is an authorization bypass, not a cosmetic gap.
	if !toolauth.IsGateable(fixtureProvidedTool) {
		t.Errorf("toolauth.IsGateable(%q) = false after activation: no agent config could disable this tool",
			fixtureProvidedTool)
	}

	// 3. A model-shaped call succeeds, and its answer proves the arguments
	// really travelled into the guest and the guest's answer really came back:
	// the fixture echoes "<tool>:<arguments as compact JSON>".
	result, err := registry.Execute(ctx, e2eAgent(), domain.ToolCall{
		ID:        "model-call-1",
		Name:      fixtureProvidedTool,
		Arguments: map[string]string{"text": "hi"},
	})
	if err != nil {
		t.Fatalf("Execute(%q): %v", fixtureProvidedTool, err)
	}
	if !result.Success {
		t.Errorf("result.Success = false (error %q), want true", result.Error)
	}
	if wantOutput := fixtureProvidedTool + `:{"text":"hi"}`; result.Output != wantOutput {
		t.Errorf("result.Output = %q, want %q", result.Output, wantOutput)
	}
	// The fixture always answers with the literal "guest-call-id" (see
	// testdata/README.md), so this pins that the HOST owns the correlation id.
	if result.CallID != "model-call-1" {
		t.Errorf("result.CallID = %q, want %q: the host, not the guest, owns the correlation id",
			result.CallID, "model-call-1")
	}

	// 4. The call is in the audit trail, attributed to whoever made it. This one
	// was model-initiated, so it must NOT read as plugin-initiated.
	events, err := audit.Events()
	if err != nil {
		t.Fatalf("read audit events: %v", err)
	}
	audited := false
	for _, ev := range events {
		if ev.Action != "tool_executed" || ev.RequestID != "model-call-1" {
			continue
		}
		audited = true
		if ev.Origin != tool.OriginAgent {
			t.Errorf("the model's call to the plugin tool was audited with origin %q, want %q",
				ev.Origin, tool.OriginAgent)
		}
	}
	if !audited {
		t.Errorf("no tool_executed audit row for the model's call; got %d events", len(events))
	}

	// 5. One DisposeOwner is the whole teardown.
	if err := ledger.DisposeOwner(testOwner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}

	if _, err := registry.Execute(context.Background(), e2eAgent(), domain.ToolCall{
		ID:   "model-call-2",
		Name: fixtureProvidedTool,
	}); !errors.Is(err, tool.ErrToolNotFound) {
		t.Errorf("Execute after disposal error = %v, want %v", err, tool.ErrToolNotFound)
	}
	if registryHasTool(registry, fixtureProvidedTool) {
		t.Errorf("Registry.Descriptors() still advertises %q after disposal: the model would keep being "+
			"offered a tool nothing can serve", fixtureProvidedTool)
	}
	if toolauth.IsGateable(fixtureProvidedTool) {
		t.Errorf("toolauth.IsGateable(%q) = true after disposal: the gateable entry outlived the plugin "+
			"that contributed it", fixtureProvidedTool)
	}
	if snapshot := ledger.Snapshot(); len(snapshot) != 0 {
		t.Errorf("ledger.Snapshot() = %v after disposal, want empty", snapshot)
	}
	// The pool is drained: a call arriving afterwards is refused by the pool
	// itself rather than reaching a closed wazero module.
	_, callErr := plugin.pool.call(context.Background(), opEcho, nil)
	if callErr == nil {
		t.Error("the plugin still answers a guest call after disposal, want an error: its pool was not drained")
	} else if !errors.Is(callErr, errPoolDraining) {
		t.Errorf("the drained pool refused with %v, want it to wrap %v", callErr, errPoolDraining)
	}
	if !guestClosed.Load() {
		t.Error("no guest module was closed by teardown: the disposers were dropped rather than run")
	}
	if got := instanceCloses.Load(); got != 1 {
		t.Errorf("closeInstance ran %d times across teardown, want exactly 1 (the plugin's one pooled instance)", got)
	}
}

// What testdata/e2e.wasm declares about itself and what it does when its tool
// is called. A rebuild that changes any of these breaks the round-trip test
// loudly instead of letting it pass against a guest nobody looked at; see
// testdata/README.md for the fixture's op table.
const (
	e2eManifestName    = "legion-e2e-plugin"
	e2eManifestVersion = "0.1.0"
	// e2eProxyTool is the tool the guest declares and the host contributes: its
	// whole behaviour is to call the host straight back.
	e2eProxyTool = "e2e_proxy_tool"
	// e2eInnerTool is the tool the guest asks the host to run, with
	// e2eGuestCallID and e2eGuestArgument, all three hard-coded in the guest.
	// It is deliberately NOT e2eProxyTool: a guest that called its own
	// contributed tool back would recurse, and the only thing stopping the chain
	// would be the call_tool depth cap — a bound this test does not own.
	e2eInnerTool     = "e2e_inner_tool"
	e2eGuestCallID   = "guest-inner-call"
	e2eGuestArgument = "from-guest"
)

// e2eOwner is the ledger owner the round-trip test files its plugin under. It
// differs from testOwner because the plugin does: two owners for two plugins is
// the ledger's contract (see Activate's owner-exclusivity precondition).
const e2eOwner lifecycle.Owner = "plugin:legion-e2e-plugin"

// e2eWasm loads the compiled end-to-end fixture guest once per test binary run.
// Unlike testdata/plugin.wasm it imports the call_tool host function, and
// unlike testdata/hostcall.wasm it answers abi.OpManifest, so it is the only
// fixture that can be ACTIVATED and then call back into the host.
var e2eWasm = sync.OnceValues(func() ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", "e2e.wasm"))
})

// dispatchProbeKey keys a value this test puts on the context it dispatches
// with. It is the test's own key type, so nothing but this test can set or read
// it, and the value can only reach the far side by having been carried there.
type dispatchProbeKey struct{}

// dispatchProbe is that value.
const dispatchProbe = "dispatch-context-marker"

// innerObservation is what the innermost tool — the one the GUEST asks the host
// to run — saw on the context it was executed with.
type innerObservation struct {
	callID      string
	argument    string
	origin      string
	probe       string
	hasBudget   bool
	hasDeadline bool
	remaining   time.Duration
}

// TestPluginToolCallCarriesTheDispatchContextThroughTheGuest closes the gap the
// contribution tests cannot: they stand a test double in for the instance pool
// (guestCallerFunc), so nothing yet proved that the context a contributed
// tool's handler builds survives the real pool.call → Instance.Invoke → wazero
// → host-function path.
//
// The chain here is entirely real. A model-shaped Registry.Execute enters the
// handler contributeTools registered; the handler marks the context and hands
// it to the plugin's pool; the pool invokes abi.OpCallTool on a real guest; the
// guest calls the real call_tool host function; and call_tool runs another
// registered tool. What that innermost tool sees is what survived the round
// trip:
//
//   - the test's own context value, which only this test can set: it can only
//     be there if the same context chain travelled through wasm and back;
//   - the shared per-task tool budget, the production mechanism that depends on
//     exactly this propagation — call_tool DENIES a call whose context carries
//     no budget, so a dropped context would refuse the call rather than
//     silently running it uncounted;
//   - the deadline Registry.Execute put on the call from the descriptor's
//     Timeout;
//   - the "plugin:<name>" call origin, which is what makes the plugin's own
//     calls tellable from the model's in one audit trail.
//
// It is bounded by construction: the guest calls one fixed tool that never
// calls back (see e2eInnerTool), so there is exactly one round trip whatever
// the depth cap does.
func TestPluginToolCallCarriesTheDispatchContextThroughTheGuest(t *testing.T) {
	env := newTestEnv(t)

	audit := adapter.NewMemoryAuditLog()
	registry := tool.NewRegistry(nil, nil, nil).WithAuditLog(audit)

	var (
		mu       sync.Mutex
		observed []innerObservation
	)
	registry.Register(e2eInnerTool, tool.HandlerFunc(
		func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			probe, _ := ctx.Value(dispatchProbeKey{}).(string)
			_, hasBudget := tool.LoopBudgetFrom(ctx)
			deadline, hasDeadline := ctx.Deadline()

			observation := innerObservation{
				callID:      call.ID,
				argument:    call.Arguments["probe"],
				origin:      tool.CallOriginFrom(ctx),
				probe:       probe,
				hasBudget:   hasBudget,
				hasDeadline: hasDeadline,
			}
			if hasDeadline {
				observation.remaining = time.Until(deadline)
			}
			mu.Lock()
			observed = append(observed, observation)
			mu.Unlock()

			return domain.ToolResult{CallID: call.ID, Success: true, Output: "inner:" + call.Arguments["probe"]}, nil
		}))

	wasmBytes, err := e2eWasm()
	if err != nil {
		t.Fatalf("read e2e fixture wasm: %v", err)
	}
	descriptor := tool.Descriptor{
		Name:        e2eProxyTool,
		Description: "把调用转交回宿主的端到端夹具工具",
		Group:       "plugins",
		RiskLevel:   "low",
		Timeout:     30 * time.Second,
	}
	// Deps.PluginName is left empty on purpose: Activate fills it from
	// Spec.Name, which is what makes the origin below the plugin's own identity
	// rather than something this test could have spelled independently.
	env.deps.PluginName = ""
	env.deps.Tools = registry

	ledger := lifecycle.NewLedger()
	plugin, err := Activate(context.Background(), ledger, e2eOwner, Spec{
		Name:         e2eManifestName,
		Wasm:         wasmBytes,
		Tools:        []tool.Descriptor{descriptor},
		Registry:     registry,
		MaxInstances: 1,
		MemoryPages:  testMemoryPages,
		// Only the tool capability: the guest imports call_tool and nothing
		// else, so a narrower grant would fail CheckImports and a wider one
		// would hand it functions it never asked for.
		Grant: perm.Grant{Tool: true},
		Deps:  env.deps,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	t.Cleanup(func() { _ = ledger.DisposeOwner(e2eOwner) })

	if plugin.Manifest.Name != e2eManifestName || plugin.Manifest.Version != e2eManifestVersion {
		t.Errorf("Plugin.Manifest = %+v, want name %q version %q",
			plugin.Manifest, e2eManifestName, e2eManifestVersion)
	}

	// The context the runtime's tool loop dispatches with: the shared per-task
	// budget, plus this test's own marker.
	budget := newFakeBudget(30)
	dispatchCtx := tool.WithLoopBudget(
		context.WithValue(context.Background(), dispatchProbeKey{}, dispatchProbe), budget)

	result, err := registry.Execute(dispatchCtx, e2eAgent(), domain.ToolCall{
		ID:   "model-call-1",
		Name: e2eProxyTool,
	})
	if err != nil {
		t.Fatalf("Execute(%q): %v", e2eProxyTool, err)
	}

	// The answer travelled the whole way back: the innermost tool's output came
	// out of the guest, through the handler's strict decode, to the model.
	if !result.Success {
		t.Fatalf("result.Success = false (error %q), want true", result.Error)
	}
	if want := "inner:" + e2eGuestArgument; result.Output != want {
		t.Errorf("result.Output = %q, want %q (the innermost tool's answer, carried back out of the guest)",
			result.Output, want)
	}
	// The innermost result carries the guest's own call id; the handler must
	// overwrite it with the model's, because the host owns the correlation id.
	if result.CallID != "model-call-1" {
		t.Errorf("result.CallID = %q, want %q: the guest answered with %q and the host must overwrite it",
			result.CallID, "model-call-1", e2eGuestCallID)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 1 {
		t.Fatalf("the innermost tool ran %d times, want exactly 1: %+v", len(observed), observed)
	}
	got := observed[0]

	// The propagation proof: a value only this test can set, read at the far
	// side of wasm.
	if got.probe != dispatchProbe {
		t.Errorf("the innermost tool saw context probe %q, want %q: the dispatch context did not survive "+
			"handler → pool.call → Instance.Invoke → wazero → call_tool", got.probe, dispatchProbe)
	}
	// And the production mechanism that rides on it. Both halves matter: the
	// budget must be REACHABLE (call_tool denies a call without one) and it must
	// have been CHARGED for the tool the guest actually reached.
	if !got.hasBudget {
		t.Error("the innermost tool saw no shared per-task budget on its context: call_tool would have " +
			"refused this call as broken wiring")
	}
	if names := budget.names(); len(names) != 1 || names[0] != e2eInnerTool {
		t.Errorf("shared budget recorded %v, want exactly [%s]: a plugin-initiated call must spend the same "+
			"allowance the model's own loop spends", names, e2eInnerTool)
	}
	// The call's own bound travelled too: Registry.Execute derives it from the
	// descriptor's Timeout, so a context replaced anywhere along the way would
	// arrive without one.
	if !got.hasDeadline {
		t.Error("the innermost tool's context carries no deadline: the timeout Registry.Execute put on the " +
			"plugin call did not reach the tool the plugin called")
	} else if got.remaining > descriptor.Timeout {
		t.Errorf("the innermost tool's context has %s left, more than the plugin tool's own timeout of %s: "+
			"this deadline is not the one the plugin call carried", got.remaining, descriptor.Timeout)
	}
	// The guest chose the inner call's id and arguments, so these prove the
	// request really came from inside the guest rather than from this test.
	if got.callID != e2eGuestCallID || got.argument != e2eGuestArgument {
		t.Errorf("the innermost tool was called with id %q and probe %q, want %q and %q (the values the "+
			"guest hard-codes)", got.callID, got.argument, e2eGuestCallID, e2eGuestArgument)
	}
	if want := pluginCallOrigin(e2eManifestName); got.origin != want {
		t.Errorf("the innermost tool saw call origin %q, want %q", got.origin, want)
	}

	// Nothing was refused: a denial here would mean the round trip "succeeded"
	// by being turned away somewhere, which is the failure this test would
	// otherwise read as a pass.
	if denied := deniedEvents(t, env); len(denied) != 0 {
		t.Errorf("the round trip published %d denial events, want 0: %v", len(denied), denied)
	}

	// One audit trail, two distinguishable rows: the model's call to the plugin
	// tool and the plugin's own call back into the host.
	events, err := audit.Events()
	if err != nil {
		t.Fatalf("read audit events: %v", err)
	}
	origins := map[string]string{}
	for _, ev := range events {
		if ev.Action != "tool_executed" {
			continue
		}
		origins[ev.RequestID] = ev.Origin
	}
	if origins["model-call-1"] != tool.OriginAgent {
		t.Errorf("the model's call was audited with origin %q, want %q", origins["model-call-1"], tool.OriginAgent)
	}
	if want := pluginCallOrigin(e2eManifestName); origins[e2eGuestCallID] != want {
		t.Errorf("the plugin's own call was audited with origin %q, want %q", origins[e2eGuestCallID], want)
	}
}

// The bounds the two resource-limit tests below run under.
const (
	// runawayDeadline is the short deadline the pure-compute loop is given. It
	// is the bound under test, so it is paired with runawayCeiling.
	runawayDeadline = 300 * time.Millisecond

	// runawayCeiling is the independent bound: if the deadline does not
	// interrupt the guest, the test must FAIL at this point rather than hang
	// until the whole package's -timeout kills it with no diagnostic. It is
	// generous enough that a loaded machine cannot trip it.
	runawayCeiling = 20 * time.Second

	// memoryBombCeiling is deliberately generous, for the opposite reason: the
	// memory page cap is what must stop the allocation loop, and a deadline
	// firing first would look identical to a cap that worked. The test reports
	// that case as inconclusive (see the mctx.Err() check), the way
	// TestInvokeMemoryCapTrapsInstance does.
	memoryBombCeiling = 30 * time.Second

	// resourceLimitToolTimeout replaces the fixture descriptor's generous
	// timeout in these two tests. Nothing here calls the tool through
	// Registry.Execute, so it is not the bound on the call; it is what
	// drainDeadline is computed from, and shrinking it keeps a failing test's
	// teardown short instead of waiting the fixture's 30s.
	resourceLimitToolTimeout = 2 * time.Second
)

// activateForResourceLimits activates the fixture plugin under testOwner with a
// registry of its own and a short tool timeout (see resourceLimitToolTimeout),
// and asserts that nothing has been closed yet — so the close counts its caller
// asserts afterwards can only come from the call that caller makes.
//
// memoryPages is passed rather than defaulted because it is the bound that
// keeps a memory bomb from taking the machine with it: it must be visible at
// the call site.
func activateForResourceLimits(t *testing.T, memoryPages uint32) (*Plugin, *lifecycle.Ledger, *atomic.Int64) {
	t.Helper()

	instanceCloses := countInstanceCloses(t)

	spec := fixtureSpec(t)
	spec.MemoryPages = memoryPages
	descriptor := fixtureDescriptor()
	descriptor.Timeout = resourceLimitToolTimeout
	spec.Tools = []tool.Descriptor{descriptor}

	ledger := lifecycle.NewLedger()
	plugin, err := Activate(context.Background(), ledger, testOwner, spec)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	t.Cleanup(func() { _ = ledger.DisposeOwner(testOwner) })

	// The manifest read already borrowed and returned the pool's instance, so a
	// healthy activation has closed nothing. Without this the "exactly one
	// close" assertions below could be satisfied by a close that happened before
	// the call under test.
	if got := instanceCloses.Load(); got != 0 {
		t.Fatalf("closeInstance ran %d times during activation, want 0", got)
	}
	return plugin, ledger, instanceCloses
}

// assertSlotCameBackEmpty asserts the pool's single slot was returned WITHOUT
// its instance — which is what "the dead instance was not pooled" looks like
// from the pool's side. It puts the slot straight back, because a slot dropped
// here would make the drain that follows panic on its own accounting.
//
// It may only be called when no call is in flight, which is the case at both
// call sites below (the call under test has already returned).
func assertSlotCameBackEmpty(t *testing.T, p *pool) {
	t.Helper()

	select {
	case slot := <-p.free:
		defer func() { p.free <- slot }()
		if slot != nil {
			t.Errorf("the pool's slot came back holding instance %p: a dead instance must be discarded, "+
				"not handed to the next caller", slot)
		}
	default:
		t.Fatal("the pool's slot never came back after the call returned: its release did not run, " +
			"so the slot is lost for the pool's whole lifetime")
	}
}

// TestARunawayGuestCallIsInterruptedAndItsInstanceIsDiscarded is the deadline
// half of the resource-limit acceptance: a guest that never yields (op 99 is a
// pure-compute infinite loop) must be cut off by its context, and the instance
// it wrecked must be discarded and closed rather than going back into the pool
// for the next caller to draw.
//
// The deadline is the bound under test, so it is NOT this test's only brake:
// the call runs on its own goroutine and the test fails at runawayCeiling if
// the interrupt never happens, instead of hanging with no diagnostic. The guest
// is stopped either way — teardown closes the plugin's runtime, which is what
// interrupts a loop the deadline did not.
func TestARunawayGuestCallIsInterruptedAndItsInstanceIsDiscarded(t *testing.T) {
	plugin, ledger, instanceCloses := activateForResourceLimits(t, testMemoryPages)

	callCtx, cancel := context.WithTimeout(context.Background(), runawayDeadline)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, cerr := plugin.pool.call(callCtx, opBusyLoop, nil)
		done <- cerr
	}()

	var callErr error
	select {
	case callErr = <-done:
	case <-time.After(runawayCeiling):
		t.Fatalf("the busy-looping guest call had still not returned %s after it started (its context "+
			"expires after %s): nothing interrupted it", runawayCeiling, runawayDeadline)
	}

	if callErr == nil {
		t.Fatal("the busy-looping guest call returned no error, so it was never interrupted")
	}
	if callCtx.Err() == nil {
		t.Fatalf("the call failed with %v while its context is still live: something other than the "+
			"deadline stopped it, so this run proves nothing about the deadline", callErr)
	}
	if want := fmt.Sprintf("op %d", opBusyLoop); !strings.Contains(callErr.Error(), want) {
		t.Errorf("the interrupted call reported %q, want it to name the guest operation (%q)", callErr, want)
	}

	// The wreckage is discarded at release time, not left for the drain: an
	// instance wazero closed under an expired context would fail every
	// subsequent caller that drew its slot.
	if got := instanceCloses.Load(); got != 1 {
		t.Errorf("closeInstance ran %d times after the interrupted call, want exactly 1: the dead instance "+
			"must be closed when it is released, not merely dropped", got)
	}
	assertSlotCameBackEmpty(t, plugin.pool)

	if err := ledger.DisposeOwner(testOwner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}
	if got := instanceCloses.Load(); got != 1 {
		t.Errorf("closeInstance ran %d times in total, want 1: the drain found an instance to close, "+
			"which means the dead one had gone back into the pool", got)
	}
}

// TestAMemoryBombIsTrappedByThePageCapAndItsInstanceIsDiscarded is the other
// half: a guest that keeps allocating (op 98 allocates 1MiB chunks in a loop)
// must be stopped by the runtime's memory page cap, and its instance discarded
// like any other corpse.
//
// The cap is what bounds this test — testMemoryPages is 64 pages, 4MiB — and
// the timeout around it is deliberately generous, because a deadline firing
// first would look exactly like a cap that worked. That case is reported as
// inconclusive rather than as a pass (see the mctx.Err() check, and
// TestInvokeMemoryCapTrapsInstance, which set the precedent).
func TestAMemoryBombIsTrappedByThePageCapAndItsInstanceIsDiscarded(t *testing.T) {
	plugin, ledger, instanceCloses := activateForResourceLimits(t, testMemoryPages)

	mctx, cancel := context.WithTimeout(context.Background(), memoryBombCeiling)
	defer cancel()

	out, err := plugin.pool.call(mctx, opMemBomb, nil)
	if err == nil {
		t.Fatalf("the memory bomb completed with no error: %s", out)
	}
	if mctx.Err() != nil {
		t.Fatalf("the memory bomb hit the %s test deadline instead of the %d-page memory cap, so this run "+
			"says nothing about the cap: %v", memoryBombCeiling, testMemoryPages, err)
	}

	if got := instanceCloses.Load(); got != 1 {
		t.Errorf("closeInstance ran %d times after the trapped call, want exactly 1: a trapped guest leaves "+
			"its module open, so the corpse must be closed when it is released", got)
	}
	assertSlotCameBackEmpty(t, plugin.pool)

	if err := ledger.DisposeOwner(testOwner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}
	if got := instanceCloses.Load(); got != 1 {
		t.Errorf("closeInstance ran %d times in total, want 1: the drain found an instance to close, "+
			"which means the trapped one had gone back into the pool", got)
	}
}
