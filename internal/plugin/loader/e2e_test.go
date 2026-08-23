package loader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/toolauth"
)

// This file is the acceptance pass over the declarative plugin deployment: it
// drives the SAME entry point serve drives — a plugins.json file on disk, read
// and parsed, then handed to Loader.Apply — and asserts what an operator and a
// model can observe afterwards. Nothing here reaches into the Loader's
// internals; every claim is made through the real *tool.Registry, the real
// process-global gateable catalog, the real lifecycle ledger and the published
// event stream.
//
// # Bounds (fork-bomb regime)
//
// These tests activate and dispose REAL wasm instances. Every loop and every
// wait in this file is bounded by a literal written inline, and every bound
// fails the test rather than extending it:
//
//   - the one test that applies in a LOOP (TestE2ERepeatedApplyRemountsNothing)
//     runs a literal 3 rounds and asserts the ledger's owner count against a
//     declared ceiling in EVERY round, so a convergence that leaked an instance
//     per round fails on the first leaked one rather than after "enough" rounds.
//     Every other test applies a fixed, written-out number of times;
//   - the one test with a task in flight bounds each wait with
//     boundaryPendingBound / boundaryApplyBound (boundary_test.go), and reaching
//     either one is a t.Fatalf;
//   - the in-flight task's own work is a literal 3 tool calls, not "until the
//     apply lands";
//   - the guest-driven call path carries e2eBudget, whose limit is a hard cap
//     chosen here rather than by anything under test.
//
// No test in this file waits on an unbounded channel, and none uses the feature
// under test as its termination condition.

// e2eInnerToolName is the tool the e2e guest calls back into through call_tool.
// The name is hard-coded in the fixture (see
// internal/plugin/host/testdata/README.md), so it is spelled here to match: a
// rebuilt fixture that renamed it must fail these tests loudly rather than let
// the inner-call assertions pass vacuously against a tool nobody called.
const e2eInnerToolName = "e2e_inner_tool"

// The call id and argument the e2e guest hard-codes into its inner call. The
// test asserts on both, which is what proves the inner call really came from
// inside the guest rather than from the test.
const (
	e2eGuestInnerCallID  = "guest-inner-call"
	e2eGuestInnerProbe   = "from-guest"
	e2eInnerOutputPrefix = "inner:"
)

// e2eBudgetLimit is the shared per-task tool budget these tests dispatch with.
//
// It is a HARD bound of this test's own, not a value derived from anything
// under test: the guest is what drives calls through it, and host.callTool
// denies a call the moment the recorded count passes the limit. A guest that
// called in a loop is therefore stopped by this counter after 4 calls, whatever
// the code under test does.
const e2eBudgetLimit = 4

// e2eCallerAgent is the identity a model-shaped call carries. Its ID is
// deliberately NOT the identity the Loader's Deps factory hands the plugin
// (harness.deps uses "test-agent"), so a test can tell the two apart — see
// TestE2EPluginToolCallsRunUnderTheDeploymentIdentity.
func e2eCallerAgent() domain.Agent {
	return domain.Agent{ID: "agent-model-caller", CompanyID: "co-1", Role: "developer", Status: domain.AgentActive}
}

// harnessDepsAgentID is the agent id harness.deps puts on every plugin's
// host.Deps. It is what a plugin's own call_tool evaluates under.
const harnessDepsAgentID = "test-agent"

// e2eBudget is a tool.LoopBudget with a fixed limit that records every name it
// is charged for.
type e2eBudget struct {
	mu    sync.Mutex
	names []string
}

// Record counts one call and reports the running total together with the limit
// in force, as tool.LoopBudget requires.
func (b *e2eBudget) Record(name string) (count, limit int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.names = append(b.names, name)
	return len(b.names), e2eBudgetLimit
}

// charged returns the names this budget was charged for, in order.
func (b *e2eBudget) charged() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.names...)
}

// e2eInnerCall is one observation of the tool a plugin called back into.
type e2eInnerCall struct {
	callID    string
	probe     string
	callerID  string
	hasCaller bool
	origin    string
}

// e2eInnerRecorder is the host-side tool the e2e guest calls through call_tool,
// recording who it ran as.
type e2eInnerRecorder struct {
	mu    sync.Mutex
	calls []e2eInnerCall
}

// observed returns every call the inner tool has served, in order.
func (r *e2eInnerRecorder) observed() []e2eInnerCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]e2eInnerCall(nil), r.calls...)
}

// registerInnerTool registers the tool the e2e guest calls back into, on the
// same registry the plugins contribute to, and returns the recorder.
//
// It is registered directly (not through a plugin) because it stands in for a
// tool the HOST already offers: the point of the fixture's inner call is that a
// plugin reaches the host's own tools through the one registry.
func (h *harness) registerInnerTool() *e2eInnerRecorder {
	h.t.Helper()

	recorder := &e2eInnerRecorder{}
	revoke := h.registry.Register(e2eInnerToolName, tool.HandlerFunc(
		func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			caller, hasCaller := tool.CallerFrom(ctx)
			recorder.mu.Lock()
			recorder.calls = append(recorder.calls, e2eInnerCall{
				callID:    call.ID,
				probe:     call.Arguments["probe"],
				callerID:  caller.ID,
				hasCaller: hasCaller,
				origin:    tool.CallOriginFrom(ctx),
			})
			recorder.mu.Unlock()
			return domain.ToolResult{
				CallID:  call.ID,
				Success: true,
				Output:  e2eInnerOutputPrefix + call.Arguments["probe"],
			}, nil
		}))
	h.t.Cleanup(revoke)
	return recorder
}

// manifestPath is where these tests keep plugins.json: inside the deployment
// root, exactly where an operator keeps it.
func (h *harness) manifestPath() string {
	return filepath.Join(h.root, "plugins.json")
}

// writeManifest writes doc as the deployment manifest, replacing whatever was
// there. Returning nothing is deliberate: every reader goes through
// harness.manifestPath, so a test cannot accidentally apply a stale path.
func (h *harness) writeManifest(doc string) {
	h.t.Helper()

	if err := os.WriteFile(h.manifestPath(), []byte(doc), 0o644); err != nil {
		h.t.Fatalf("write deployment manifest %s: %v", h.manifestPath(), err)
	}
}

// readManifest reads and parses the deployment manifest from disk, exactly as
// serve does (internal/cli's readPluginDeployment: os.ReadFile followed by
// manifest.ParseDeployment). Reading it back rather than reusing what a test
// built in memory is what makes these tests end-to-end: a plugins.json whose
// JSON shape no longer parses fails here.
//
// It fails the test rather than returning an error: an unparseable manifest is
// this test's own fixture being wrong, never a result under test.
func (h *harness) readManifest() manifest.Deployment {
	h.t.Helper()

	data, err := os.ReadFile(h.manifestPath())
	if err != nil {
		h.t.Fatalf("read deployment manifest %s: %v", h.manifestPath(), err)
	}
	deployment, err := manifest.ParseDeployment(data)
	if err != nil {
		h.t.Fatalf("parse deployment manifest %s: %v", h.manifestPath(), err)
	}
	return deployment
}

// applyManifest converges toward the manifest currently on disk and returns
// what Apply reported, so a test can assert either success or a specific
// refusal.
func (h *harness) applyManifest(ctx context.Context) error {
	h.t.Helper()

	return h.loader.Apply(ctx, h.readManifest(), h.root)
}

// requireInstanceCeiling fails the test when the ledger holds more plugin
// owners than this round's declared ceiling.
//
// It is the fork-bomb bound for anything that applies more than once: an
// activation that leaked its predecessor shows up as an extra owner in the
// round it happened, so a test never has to run "until it converges" to notice.
func (h *harness) requireInstanceCeiling(when string, ceiling int) {
	h.t.Helper()

	if owners := h.owners(); len(owners) > ceiling {
		h.t.Fatalf("%s: %d plugin owners are filed in the ledger (%v), want at most %d; "+
			"an activation is leaking instances", when, len(owners), owners, ceiling)
	}
}

// executeAsModel runs one tool call the way a task's tool loop does: with the
// caller's identity and with the shared per-task budget attached, which
// call_tool requires (a call carrying no budget is DENIED as broken wiring).
// The budget comes back so a test can assert what a plugin-initiated call was
// charged for.
func (h *harness) executeAsModel(ctx context.Context, call domain.ToolCall) (domain.ToolResult, *e2eBudget, error) {
	h.t.Helper()

	budget := &e2eBudget{}
	result, err := h.registry.Execute(tool.WithLoopBudget(ctx, budget), e2eCallerAgent(), call)
	return result, budget, err
}

// requireGateable asserts that a tool name is (or is not) in the PROCESS-GLOBAL
// gateable catalog, which is what a per-agent disabled_tools list resolves
// against. A tool that is callable but not gateable is an authorization bypass.
func requireGateable(t *testing.T, name string, want bool, when string) {
	t.Helper()

	if got := toolauth.IsGateable(name); got != want {
		t.Errorf("%s: toolauth.IsGateable(%q) = %v, want %v", when, name, got, want)
	}
}

// The deployment manifests these tests write. They are literal JSON — the
// document an operator edits — rather than a marshalled struct, so a change to
// plugins.json's shape breaks them instead of travelling silently through the
// same tags on both sides. The plugin and tool names are interpolated from the
// fixture constants so a renamed fixture fails loudly.

// e2eBothEnabledManifest declares both plugins, enabled.
func e2eBothEnabledManifest() string {
	return fmt.Sprintf(`{
  "plugins": [
    {
      "name": %q,
      "source": "echo",
      "enabled": true,
      "tools": [{"name": %q}]
    },
    {
      "name": %q,
      "source": "proxy",
      "enabled": true,
      "grant": {"capabilities": ["tool"]},
      "tools": [{"name": %q}]
    }
  ]
}`, echoPluginName, echoToolName, proxyPluginName, proxyToolName)
}

// e2eEchoDisabledManifest is the same target state with the echo entry switched
// off — the operator's own doing, which converges to exactly the same action as
// deleting it.
func e2eEchoDisabledManifest() string {
	return fmt.Sprintf(`{
  "plugins": [
    {
      "name": %q,
      "source": "echo",
      "enabled": false,
      "tools": [{"name": %q}]
    },
    {
      "name": %q,
      "source": "proxy",
      "enabled": true,
      "grant": {"capabilities": ["tool"]},
      "tools": [{"name": %q}]
    }
  ]
}`, echoPluginName, echoToolName, proxyPluginName, proxyToolName)
}

// e2eEmptyManifest is the target state that says "no plugins at all".
func e2eEmptyManifest() string { return `{"plugins": []}` }

// e2eEchoOnlyManifest declares the echo plugin alone.
func e2eEchoOnlyManifest() string {
	return fmt.Sprintf(`{
  "plugins": [
    {
      "name": %q,
      "source": "echo",
      "enabled": true,
      "tools": [{"name": %q}]
    }
  ]
}`, echoPluginName, echoToolName)
}

// e2eProxyOnlyManifest declares the proxy plugin alone — the target state the
// boundary test reloads TOWARD, so the change is both an unload and an
// activation.
func e2eProxyOnlyManifest() string {
	return fmt.Sprintf(`{
  "plugins": [
    {
      "name": %q,
      "source": "proxy",
      "enabled": true,
      "grant": {"capabilities": ["tool"]},
      "tools": [{"name": %q}]
    }
  ]
}`, proxyPluginName, proxyToolName)
}

// TestE2EDeploymentLifecycleFromManifestFileToEmptyLedger is Step 1 of the
// acceptance pass: the whole declarative lifecycle, driven from a plugins.json
// on disk.
//
//	two entries -> startup Apply -> both tools in the registry AND in the
//	gateable catalog -> a model-shaped Registry.Execute succeeds on each ->
//	edit the manifest (one disabled, one at a new version) -> reload -> the
//	disabled one's tool is gone (not gateable, ErrToolNotFound) and the other
//	runs under a NEW owner with no trace of the old one -> remove every entry
//	-> reload -> the ledger is empty
//
// Every "after" claim is paired with its "before": both tools are proven absent
// from the registry and from the gateable catalog BEFORE the first apply, and
// proven working BEFORE the manifest is edited, so no assertion here can be
// satisfied by a state that was already true.
//
// There is no loop: the sequence is four applies written out in order.
func TestE2EDeploymentLifecycleFromManifestFileToEmptyLedger(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Nothing this test asserts may be true before it starts. The gateable
	// catalog is process-global, so a leak from an earlier test would make the
	// gateable assertions below vacuous.
	for _, name := range []string{echoToolName, proxyToolName} {
		if toolauth.IsGateable(name) {
			t.Fatalf("%q is already gateable before any apply: an earlier test leaked its contribution "+
				"and this test's gateable assertions would be vacuous", name)
		}
	}
	if names := h.toolNames(); len(names) != 0 {
		t.Fatalf("registry already advertises %v before any apply", names)
	}

	h.writeEcho("1.0.0")
	h.writeProxy("1.0.0")
	inner := h.registerInnerTool()
	h.writeManifest(e2eBothEnabledManifest())

	// Round 1: the startup convergence, through the same read-parse-Apply the
	// serve assembly performs.
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("startup Apply: %v", err)
	}
	h.requireInstanceCeiling("after the startup apply", 2)

	wantStrings(t, "registry tools after the startup apply", h.toolNames(),
		[]string{e2eInnerToolName, proxyToolName, echoToolName})
	requireGateable(t, echoToolName, true, "after the startup apply")
	requireGateable(t, proxyToolName, true, "after the startup apply")
	// The host's own inner tool is NOT gateable: only a plugin's contribution
	// enters the catalog, and asserting it here keeps the two gateable claims
	// above from reading as "everything registered is gateable".
	requireGateable(t, e2eInnerToolName, false, "after the startup apply")

	wantStrings(t, "ledger owners after the startup apply", h.owners(),
		[]string{"plugin:" + proxyPluginName + "@1.0.0", "plugin:" + echoPluginName + "@1.0.0"})

	status := h.loader.Status()
	if len(status) != 2 {
		t.Fatalf("Status() after the startup apply = %#v, want two rows", status)
	}
	for _, row := range status {
		if row.State != StateLoaded {
			t.Errorf("Status() row %q state = %q, want %q", row.Name, row.State, StateLoaded)
		}
		if row.Version != "1.0.0" {
			t.Errorf("Status() row %q version = %q, want %q", row.Name, row.Version, "1.0.0")
		}
		if row.LastError != "" {
			t.Errorf("Status() row %q carries error %q, want none", row.Name, row.LastError)
		}
	}

	// A model-shaped call on the first plugin. The fixture echoes
	// "<tool>:<arguments as compact JSON>", so the answer proves the arguments
	// travelled into the guest and the guest's answer came back.
	echoResult, _, err := h.executeAsModel(ctx, domain.ToolCall{
		ID:        "model-call-echo",
		Name:      echoToolName,
		Arguments: map[string]string{"text": "hi"},
	})
	if err != nil {
		t.Fatalf("Execute(%q): %v", echoToolName, err)
	}
	if !echoResult.Success {
		t.Errorf("Execute(%q) result.Success = false (error %q), want true", echoToolName, echoResult.Error)
	}
	if want := echoToolName + `:{"text":"hi"}`; echoResult.Output != want {
		t.Errorf("Execute(%q) result.Output = %q, want %q", echoToolName, echoResult.Output, want)
	}
	// The fixture always answers with the literal "guest-call-id", so this pins
	// that the HOST owns the correlation id.
	if echoResult.CallID != "model-call-echo" {
		t.Errorf("Execute(%q) result.CallID = %q, want %q: the host, not the guest, owns the correlation id",
			echoToolName, echoResult.CallID, "model-call-echo")
	}

	// And on the second plugin, which reaches back through call_tool into a
	// host tool. Its answer is the INNER tool's output, so it proves the whole
	// round trip.
	proxyResult, budget, err := h.executeAsModel(ctx, domain.ToolCall{ID: "model-call-proxy", Name: proxyToolName})
	if err != nil {
		t.Fatalf("Execute(%q): %v", proxyToolName, err)
	}
	if !proxyResult.Success {
		t.Fatalf("Execute(%q) result.Success = false (error %q), want true", proxyToolName, proxyResult.Error)
	}
	if want := e2eInnerOutputPrefix + e2eGuestInnerProbe; proxyResult.Output != want {
		t.Errorf("Execute(%q) result.Output = %q, want %q (the inner tool's answer, carried back out of the guest)",
			proxyToolName, proxyResult.Output, want)
	}
	// A plugin-initiated call spends the task's own allowance, not a counter of
	// the plugin's own.
	if charged := budget.charged(); len(charged) != 1 || charged[0] != e2eInnerToolName {
		t.Errorf("the shared per-task budget was charged %v, want exactly [%s]", charged, e2eInnerToolName)
	}
	calls := inner.observed()
	if len(calls) != 1 {
		t.Fatalf("the inner tool ran %d times, want exactly 1: %+v", len(calls), calls)
	}
	// The guest chose both of these, so they prove the inner call came from
	// inside the guest rather than from this test.
	if calls[0].callID != e2eGuestInnerCallID || calls[0].probe != e2eGuestInnerProbe {
		t.Errorf("the inner tool was called with id %q and probe %q, want %q and %q (the guest's own values)",
			calls[0].callID, calls[0].probe, e2eGuestInnerCallID, e2eGuestInnerProbe)
	}

	// Round 2: the operator edits the manifest — one entry off, one entry's
	// package rebuilt at a new version — and reloads.
	h.writeProxy("2.0.0")
	h.writeManifest(e2eEchoDisabledManifest())
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("reload Apply: %v", err)
	}
	h.requireInstanceCeiling("after the reload", 2)

	// The disabled entry is gone from every surface a model or an agent config
	// can see.
	requireGateable(t, echoToolName, false, "after the reload disabled it")
	if names := h.toolNames(); slices.Contains(names, echoToolName) {
		t.Errorf("registry still advertises %q after the reload disabled it: %v", echoToolName, names)
	}
	if _, _, err := h.executeAsModel(ctx, domain.ToolCall{ID: "model-call-gone", Name: echoToolName}); !errors.Is(err, tool.ErrToolNotFound) {
		t.Errorf("Execute(%q) after the reload disabled it = %v, want ErrToolNotFound", echoToolName, err)
	}

	// The re-versioned entry runs under its NEW owner, and the old owner is not
	// left behind holding a live instance.
	wantStrings(t, "ledger owners after the reload", h.owners(),
		[]string{"plugin:" + proxyPluginName + "@2.0.0"})

	status = h.loader.Status()
	if len(status) != 1 || status[0].Name != proxyPluginName ||
		status[0].State != StateLoaded || status[0].Version != "2.0.0" {
		t.Fatalf("Status() after the reload = %#v, want one loaded %q at 2.0.0", status, proxyPluginName)
	}

	// The replacement is not merely registered — it serves. A remount that
	// registered the tool but left a disposed guest behind would pass every
	// assertion above and fail here.
	replaced, _, err := h.executeAsModel(ctx, domain.ToolCall{ID: "model-call-proxy-2", Name: proxyToolName})
	if err != nil {
		t.Fatalf("Execute(%q) against the replacement: %v", proxyToolName, err)
	}
	if want := e2eInnerOutputPrefix + e2eGuestInnerProbe; !replaced.Success || replaced.Output != want {
		t.Errorf("Execute(%q) against the replacement = %+v, want a successful %q", proxyToolName, replaced, want)
	}
	requireGateable(t, proxyToolName, true, "after the reload replaced it")

	// Round 3: every entry removed. This is also the drain serve runs at
	// shutdown (internal/cli's drainPlugins converges toward an empty
	// Deployment), so "the ledger is empty" is the claim that lets one process
	// assemble serve twice.
	h.writeManifest(e2eEmptyManifest())
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("drain Apply: %v", err)
	}
	if snapshot := h.loader.Status(); len(snapshot) != 0 {
		t.Errorf("Status() after every entry was removed = %#v, want empty", snapshot)
	}
	if owners := h.ledger.Snapshot(); len(owners) != 0 {
		t.Errorf("ledger.Snapshot() after every entry was removed = %v, want empty", owners)
	}
	requireGateable(t, echoToolName, false, "after every entry was removed")
	requireGateable(t, proxyToolName, false, "after every entry was removed")
	// The host's own tool is untouched by the drain: only what the plugins
	// contributed goes away.
	wantStrings(t, "registry tools after every entry was removed", h.toolNames(), []string{e2eInnerToolName})
}

// TestE2EPluginToolCallsRunUnderTheDeploymentIdentity pins the invariant the
// A4a phase carried into acceptance: a plugin's own tool calls evaluate under
// the identity the DEPLOYMENT gave the plugin (host.Deps.Agent, which serve
// fills from serveDefaultAgent), never under the identity of whoever called the
// plugin's tool.
//
// This is the first place the invariant is observable end to end, so it is
// asserted rather than assumed — and asserted from both sides: the inner tool
// must see the deployment's identity AND must not see the caller's. The two
// identities are deliberately different strings, so the assertion cannot pass by
// coincidence.
//
// It documents today's behaviour; it does not endorse it. See the task report
// for why it is the safer of the two options today and what A4b has to decide.
func TestE2EPluginToolCallsRunUnderTheDeploymentIdentity(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.writeProxy("1.0.0")
	inner := h.registerInnerTool()
	h.writeManifest(e2eProxyOnlyManifest())
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	caller := e2eCallerAgent()
	if caller.ID == harnessDepsAgentID {
		t.Fatalf("the caller's identity %q is the same as the deployment's; this test could not tell "+
			"the two apart", caller.ID)
	}
	if _, _, err := h.executeAsModel(ctx, domain.ToolCall{ID: "model-call-identity", Name: proxyToolName}); err != nil {
		t.Fatalf("Execute(%q): %v", proxyToolName, err)
	}

	calls := inner.observed()
	if len(calls) != 1 {
		t.Fatalf("the inner tool ran %d times, want exactly 1: %+v", len(calls), calls)
	}
	got := calls[0]
	if !got.hasCaller {
		t.Fatalf("the inner tool ran with no caller identity at all; a tool that scopes its result to the "+
			"caller would have had to refuse this call (%+v)", got)
	}
	if got.callerID != harnessDepsAgentID {
		t.Errorf("the inner tool ran as %q, want the deployment's own identity %q: a plugin's tool calls "+
			"evaluate under host.Deps.Agent", got.callerID, harnessDepsAgentID)
	}
	if got.callerID == caller.ID {
		t.Errorf("the inner tool ran as the CALLER %q; the plugin's calls must not inherit the identity of "+
			"whoever invoked its tool", caller.ID)
	}
	// The other half of attribution: the call is marked as the plugin's, so an
	// audit pass can tell a plugin's calls from the agent's own.
	if want := "plugin:" + proxyPluginName; got.origin != want {
		t.Errorf("the inner tool saw call origin %q, want %q", got.origin, want)
	}
}

// TestE2ERepeatedApplyRemountsNothing is the idempotence half of a declarative
// deployment: applying the SAME manifest again converges to the same state
// without touching anything that is already running.
//
// It is the only test in this file that loops over Apply, and it is bounded by
// a literal 3 rounds with an instance-count ceiling asserted in every round —
// never "loop until it converges". A convergence that remounted an unchanged
// entry per round would fail on round 1 (the plugin/loaded count), and one that
// leaked an instance per round would fail on the ceiling in the round it leaked.
func TestE2ERepeatedApplyRemountsNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.writeEcho("1.0.0")
	h.writeProxy("1.0.0")
	h.writeManifest(e2eBothEnabledManifest())
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	const wantOwners = 2
	if loaded := h.eventsOfType(RuntimeEventLoaded); len(loaded) != wantOwners {
		t.Fatalf("plugin/loaded events after the first apply = %d, want %d", len(loaded), wantOwners)
	}

	// A literal round count. Three is enough to distinguish "never touched"
	// from "touched every time"; it is not a budget to be raised until
	// something converges.
	for round := 1; round <= 3; round++ {
		if err := h.applyManifest(ctx); err != nil {
			t.Fatalf("round %d: Apply of the unchanged manifest: %v", round, err)
		}
		h.requireInstanceCeiling(fmt.Sprintf("round %d", round), wantOwners)
		if owners := h.owners(); len(owners) != wantOwners {
			t.Fatalf("round %d: ledger owners = %v, want exactly %d", round, owners, wantOwners)
		}
		if loaded := h.eventsOfType(RuntimeEventLoaded); len(loaded) != wantOwners {
			t.Fatalf("round %d: plugin/loaded events = %d, want still %d; an unchanged entry was remounted",
				round, len(loaded), wantOwners)
		}
		if unloaded := h.eventsOfType(RuntimeEventUnloaded); len(unloaded) != 0 {
			t.Fatalf("round %d: plugin/unloaded events = %d, want none; an unchanged entry was unmounted",
				round, len(unloaded))
		}
		// Still serving, not merely still registered.
		result, _, err := h.executeAsModel(ctx, domain.ToolCall{
			ID:        fmt.Sprintf("model-call-round-%d", round),
			Name:      echoToolName,
			Arguments: map[string]string{"round": fmt.Sprint(round)},
		})
		if err != nil {
			t.Fatalf("round %d: Execute(%q): %v", round, echoToolName, err)
		}
		if want := fmt.Sprintf(`%s:{"round":"%d"}`, echoToolName, round); result.Output != want {
			t.Fatalf("round %d: Execute(%q) result.Output = %q, want %q", round, echoToolName, result.Output, want)
		}
	}
}

// TestE2EReloadLandsOnlyAfterTheInFlightTaskAndOnlyNewTasksSeeIt is Step 2: the
// task-boundary contract proved end to end, from a manifest file to a running
// task's tool calls.
//
// The claims, each asserted while it is still falsifiable:
//
//   - a task that is already running FINISHES, and every one of its tool calls
//     resolves against the tool set it started with — asserted while the reload
//     is demonstrably pending, so a convergence that landed mid-task fails here;
//   - the reload does NOT land while that task is in flight — the new plugin's
//     tool is absent and Apply has not returned;
//   - a new task cannot start into a catalog that is mid-change — Begin reports
//     ErrApplyPending;
//   - once the task ends the reload lands, and a task started afterwards sees
//     the NEW catalog: the old tool is gone and the new one works.
//
// Bounds: the in-flight task does a literal 3 tool calls; the wait for the apply
// to become pending is boundaryPendingBound; the wait for it to return is
// boundaryApplyBound; reaching either fails the test.
func TestE2EReloadLandsOnlyAfterTheInFlightTaskAndOnlyNewTasksSeeIt(t *testing.T) {
	h := newHarnessWithApplyWait(t, boundaryApplyBound)
	ctx := context.Background()

	h.writeEcho("1.0.0")
	h.writeProxy("1.0.0")
	inner := h.registerInnerTool()
	h.writeManifest(e2eEchoOnlyManifest())
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("startup Apply: %v", err)
	}
	if names := h.toolNames(); !slices.Contains(names, echoToolName) {
		t.Fatalf("registry tools after the startup apply = %v, want %q among them", names, echoToolName)
	}

	// The task starts, holding the gate exactly as runtime.RunTask does.
	end, err := h.gate.Begin()
	if err != nil {
		t.Fatalf("Begin() error = %v, want nil", err)
	}

	// The operator edits the manifest and reloads. The parse happens here, on
	// the test's own goroutine, so the background goroutine only ever calls
	// Apply — no t.Fatalf is reachable off the main goroutine.
	h.writeManifest(e2eProxyOnlyManifest())
	deployment := h.readManifest()
	done := make(chan error, 1)
	go func() { done <- h.loader.Apply(ctx, deployment, h.root) }()

	// Retiring the task and draining the backgrounded Apply are cleanup rather
	// than a defer, so they also run on the failure paths below — and BEFORE the
	// harness disposes the ledger (t.Cleanup runs last-registered-first, and the
	// harness registered its own first). A failing run would otherwise dispose
	// owners while a convergence was still filing them.
	var (
		endOnce       sync.Once
		applyReturned bool
	)
	t.Cleanup(func() {
		endOnce.Do(end)
		if applyReturned {
			return
		}
		select {
		case <-done:
		case <-time.After(boundaryApplyBound):
			t.Errorf("Apply() did not return within %s of the task ending", boundaryApplyBound)
		}
	})

	h.awaitReloadPending(t, done, &applyReturned)

	// The task's own work, with the reload demonstrably pending. A literal three
	// calls: this is a bounded piece of work, not a poll that runs until
	// something else happens.
	for call := 1; call <= 3; call++ {
		result, _, err := h.executeAsModel(ctx, domain.ToolCall{
			ID:        fmt.Sprintf("in-flight-call-%d", call),
			Name:      echoToolName,
			Arguments: map[string]string{"call": fmt.Sprint(call)},
		})
		if err != nil {
			t.Fatalf("in-flight call %d to %q: %v; the running task lost the tool set it started with",
				call, echoToolName, err)
		}
		if want := fmt.Sprintf(`%s:{"call":"%d"}`, echoToolName, call); result.Output != want {
			t.Fatalf("in-flight call %d to %q result.Output = %q, want %q", call, echoToolName, result.Output, want)
		}
	}
	// The new target state has NOT landed on the running task.
	if names := h.toolNames(); slices.Contains(names, proxyToolName) {
		t.Fatalf("registry advertises %q while a task is in flight = %v; the reload did not wait for a "+
			"task boundary", proxyToolName, names)
	}
	if toolauth.IsGateable(proxyToolName) {
		t.Fatalf("%q is gateable while a task is in flight; the reload did not wait for a task boundary",
			proxyToolName)
	}
	// And a NEW task may not start into a catalog that is mid-change.
	if probe, err := h.gate.Begin(); err == nil {
		probe()
		t.Fatalf("Begin() started a new task while a reload was pending, error = nil, want ErrApplyPending")
	} else if !errors.Is(err, taskgate.ErrApplyPending) {
		t.Fatalf("Begin() while a reload was pending error = %v, want one matching ErrApplyPending", err)
	}

	// The boundary: the task ends and the reload may land.
	endOnce.Do(end)
	select {
	case err := <-done:
		applyReturned = true
		if err != nil {
			t.Fatalf("Apply() error = %v, want nil", err)
		}
	case <-time.After(boundaryApplyBound):
		t.Fatalf("Apply() did not return within %s of the task ending", boundaryApplyBound)
	}

	// A task started AFTER the boundary sees the new catalog.
	next, err := h.gate.Begin()
	if err != nil {
		t.Fatalf("Begin() after the reload landed error = %v, want nil", err)
	}
	defer next()

	if _, _, err := h.executeAsModel(ctx, domain.ToolCall{ID: "next-task-old-tool", Name: echoToolName}); !errors.Is(err, tool.ErrToolNotFound) {
		t.Errorf("Execute(%q) in a task started after the reload = %v, want ErrToolNotFound", echoToolName, err)
	}
	requireGateable(t, echoToolName, false, "in a task started after the reload")
	requireGateable(t, proxyToolName, true, "in a task started after the reload")

	result, _, err := h.executeAsModel(ctx, domain.ToolCall{ID: "next-task-new-tool", Name: proxyToolName})
	if err != nil {
		t.Fatalf("Execute(%q) in a task started after the reload: %v", proxyToolName, err)
	}
	if want := e2eInnerOutputPrefix + e2eGuestInnerProbe; !result.Success || result.Output != want {
		t.Errorf("Execute(%q) in a task started after the reload = %+v, want a successful %q",
			proxyToolName, result, want)
	}
	if calls := inner.observed(); len(calls) != 1 {
		t.Errorf("the inner tool ran %d times, want exactly 1 (only the task after the boundary reached the "+
			"new plugin): %+v", len(calls), calls)
	}
}

// awaitReloadPending waits until the backgrounded Apply has claimed the gate and
// is waiting for the boundary, failing the test the moment the convergence
// escapes that discipline instead.
//
// It is boundary_test.go's awaitApplyPending with one difference that matters
// here: the tool it watches for is the one the reload would ADD (the proxy
// plugin's), because in this test the echo plugin's tool is legitimately
// registered the whole time — it is what the in-flight task is using.
//
// applyReturned is the caller's own flag, and this helper owns it on the one
// path where it takes the value out of done: the cleanup would otherwise wait
// the full boundaryApplyBound on a channel that can never deliver again.
func (h *harness) awaitReloadPending(t *testing.T, done <-chan error, applyReturned *bool) {
	t.Helper()

	deadline := time.Now().Add(boundaryPendingBound)
	for {
		if names := h.toolNames(); slices.Contains(names, proxyToolName) {
			t.Fatalf("plugin tool %q was registered while a task was in flight; "+
				"the reload did not wait for a task boundary", proxyToolName)
		}
		select {
		case err := <-done:
			*applyReturned = true
			t.Fatalf("Apply() returned (%v) while a task was still in flight; "+
				"the reload did not wait for a task boundary", err)
		default:
		}

		probe, err := h.gate.Begin()
		if err != nil {
			if !errors.Is(err, taskgate.ErrApplyPending) {
				t.Fatalf("Begin() error = %v, want one matching ErrApplyPending", err)
			}
			return
		}
		probe()

		if time.Now().After(deadline) {
			t.Fatalf("Apply() never claimed the gate within %s; it is not applying at a task boundary",
				boundaryPendingBound)
		}
		time.Sleep(boundaryPollInterval)
	}
}

// TestE2ECorruptedModuleLeavesTheRunningInstanceUntouched is the first of
// Step 3's rejection paths: a plugin.wasm whose bytes no longer match the digest
// its plugin.json declares.
//
// The entry must not activate, the instance that IS running must be left exactly
// as it was — not rebuilt, not disposed — and `plugins status` must say why.
// "Not rebuilt" is asserted through the plugin/loaded event count (the
// convergence's own published record) rather than through a counter only a test
// would read.
func TestE2ECorruptedModuleLeavesTheRunningInstanceUntouched(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.writeEcho("1.0.0")
	h.writeManifest(e2eEchoOnlyManifest())
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("startup Apply: %v", err)
	}
	if loaded := h.eventsOfType(RuntimeEventLoaded); len(loaded) != 1 {
		t.Fatalf("plugin/loaded events after the startup apply = %d, want 1", len(loaded))
	}
	before := h.owners()
	wantStrings(t, "ledger owners after the startup apply", before, []string{"plugin:" + echoPluginName + "@1.0.0"})

	// The module on disk is replaced with different bytes while plugin.json
	// keeps declaring the old digest — a rebuilt module nobody re-approved, or a
	// tampered one.
	corrupted := appendCustomSection(t, fixtureWasm(t, echoWasmFile), "tampered")
	if err := os.WriteFile(filepath.Join(h.root, "echo", "plugin.wasm"), corrupted, 0o644); err != nil {
		t.Fatalf("overwrite plugin.wasm: %v", err)
	}

	err := h.applyManifest(ctx)
	if err == nil {
		t.Fatalf("Apply() with a module that does not match its declared digest error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("Apply() error = %q, want it to name the digest mismatch", err)
	}

	// The running instance is untouched: same owner, no second mount, no
	// unmount, and it still SERVES.
	wantStrings(t, "ledger owners after the refused apply", h.owners(), before)
	if loaded := h.eventsOfType(RuntimeEventLoaded); len(loaded) != 1 {
		t.Errorf("plugin/loaded events after the refused apply = %d, want still 1; the running instance was "+
			"rebuilt", len(loaded))
	}
	if unloaded := h.eventsOfType(RuntimeEventUnloaded); len(unloaded) != 0 {
		t.Errorf("plugin/unloaded events after the refused apply = %d, want none; the running instance was "+
			"torn down for a replacement that never existed", len(unloaded))
	}
	result, _, execErr := h.executeAsModel(ctx, domain.ToolCall{
		ID:        "model-call-after-refusal",
		Name:      echoToolName,
		Arguments: map[string]string{"text": "still here"},
	})
	if execErr != nil {
		t.Fatalf("Execute(%q) after the refused apply: %v", echoToolName, execErr)
	}
	if want := echoToolName + `:{"text":"still here"}`; result.Output != want {
		t.Errorf("Execute(%q) after the refused apply result.Output = %q, want %q", echoToolName, result.Output, want)
	}

	// And the operator can see why, on the row of the plugin that is still
	// running.
	status := h.loader.Status()
	if len(status) != 1 || status[0].Name != echoPluginName || status[0].State != StateLoaded {
		t.Fatalf("Status() after the refused apply = %#v, want one loaded %q row", status, echoPluginName)
	}
	if !strings.Contains(status[0].LastError, "sha256 mismatch") {
		t.Errorf("Status() row LastError = %q, want it to name the digest mismatch", status[0].LastError)
	}
	if failed := h.eventsOfType(RuntimeEventActivationFailed); len(failed) != 1 {
		t.Errorf("plugin/activation_failed events = %d, want exactly 1", len(failed))
	} else if !strings.Contains(failed[0].Message, "step="+stepLoadPackage) {
		t.Errorf("plugin/activation_failed message = %q, want it to name step=%s", failed[0].Message, stepLoadPackage)
	}
}

// requireFailedRow finds the Status row of a refused entry and fails the test
// unless it reports StateFailed with an error containing want.
func requireFailedRow(t *testing.T, status []InstanceStatus, name, want string) {
	t.Helper()

	for _, row := range status {
		if row.Name != name {
			continue
		}
		if row.State != StateFailed {
			t.Errorf("Status() row %q state = %q, want %q", name, row.State, StateFailed)
		}
		if !strings.Contains(row.LastError, want) {
			t.Errorf("Status() row %q LastError = %q, want it to contain %q", name, row.LastError, want)
		}
		if len(row.Tools) != 0 {
			t.Errorf("Status() row %q reports tools %v, want none: nothing was contributed", name, row.Tools)
		}
		return
	}
	t.Fatalf("Status() has no row for the refused entry %q: %#v", name, status)
}

// TestE2EUngrantedCapabilityIsRefusedByName is Step 3's second rejection: a
// plugin that declares a capability the deployment does not grant is refused
// outright, and the error names the missing capability.
//
// Refusing is the whole point: a plugin that believes it has a capability it was
// never granted runs half-crippled, which is worse than not running.
//
// The manifest carries a SECOND, well-formed entry, and that is not decoration.
// It makes "nothing was mounted for the refused entry" a real claim rather than
// one the empty starting state already satisfied — the ledger is not empty
// afterwards, it holds exactly the other plugin — and it pins that one entry's
// refusal does not abort the convergence of the rest.
func TestE2EUngrantedCapabilityIsRefusedByName(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// The proxy plugin's package declares the "tool" capability; this manifest
	// grants it nothing at all. The echo plugin declares no capability and is
	// authorized correctly.
	h.writeEcho("1.0.0")
	h.writeProxy("1.0.0")
	h.writeManifest(fmt.Sprintf(`{
  "plugins": [
    {
      "name": %q,
      "source": "echo",
      "enabled": true,
      "tools": [{"name": %q}]
    },
    {
      "name": %q,
      "source": "proxy",
      "enabled": true,
      "tools": [{"name": %q}]
    }
  ]
}`, echoPluginName, echoToolName, proxyPluginName, proxyToolName))

	err := h.applyManifest(ctx)
	if err == nil {
		t.Fatalf("Apply() of a plugin whose declared capability is not granted error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), `capability "tool"`) || !strings.Contains(err.Error(), "does not grant") {
		t.Errorf("Apply() error = %q, want it to name the capability the deployment did not grant", err)
	}

	// The correctly authorized entry converged, and it SERVES.
	wantStrings(t, "ledger owners after the refusal", h.owners(), []string{"plugin:" + echoPluginName + "@1.0.0"})
	if _, _, execErr := h.executeAsModel(ctx, domain.ToolCall{
		ID:        "model-call-sibling",
		Name:      echoToolName,
		Arguments: map[string]string{"text": "ok"},
	}); execErr != nil {
		t.Errorf("Execute(%q) after the sibling entry was refused: %v; one entry's refusal must not abort "+
			"the rest of the convergence", echoToolName, execErr)
	}

	// The refused entry contributed nothing.
	if names := h.toolNames(); slices.Contains(names, proxyToolName) {
		t.Errorf("registry advertises %q after the refusal: %v", proxyToolName, names)
	}
	requireGateable(t, proxyToolName, false, "after the refusal")
	requireFailedRow(t, h.loader.Status(), proxyPluginName, `capability "tool"`)

	if failed := h.eventsOfType(RuntimeEventActivationFailed); len(failed) != 1 {
		t.Errorf("plugin/activation_failed events = %d, want exactly 1", len(failed))
	} else if !strings.Contains(failed[0].Message, "step="+stepAssembleSpec) {
		t.Errorf("plugin/activation_failed message = %q, want it to name step=%s",
			failed[0].Message, stepAssembleSpec)
	}
}

// TestE2EAcceptedToolThePluginNeverDeclaredIsRefused is Step 3's third
// rejection: a deployment that accepts a tool name the plugin never declared.
//
// It is refused rather than ignored because a name in plugins.json that matches
// nothing is far more likely to be a typo — an operator who believes a tool is
// available when it is not — than a deliberate no-op.
//
// It carries a well-formed sibling entry for the same reason the capability test
// does: so that "the refused entry contributed nothing" is a claim about this
// convergence and not about an empty starting state.
func TestE2EAcceptedToolThePluginNeverDeclaredIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	const typo = "echo_tool_typo"
	h.writeEcho("1.0.0")
	h.writeProxy("1.0.0")
	h.writeManifest(fmt.Sprintf(`{
  "plugins": [
    {
      "name": %q,
      "source": "proxy",
      "enabled": true,
      "grant": {"capabilities": ["tool"]},
      "tools": [{"name": %q}]
    },
    {
      "name": %q,
      "source": "echo",
      "enabled": true,
      "tools": [{"name": %q}]
    }
  ]
}`, proxyPluginName, proxyToolName, echoPluginName, typo))

	err := h.applyManifest(ctx)
	if err == nil {
		t.Fatalf("Apply() of a deployment accepting an undeclared tool error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), typo) || !strings.Contains(err.Error(), "does not declare") {
		t.Errorf("Apply() error = %q, want it to name the tool the plugin does not declare", err)
	}

	wantStrings(t, "ledger owners after the refusal", h.owners(), []string{"plugin:" + proxyPluginName + "@1.0.0"})

	// Neither the typo nor the tool the plugin really declares became reachable:
	// a refusal that registered the declared tool anyway would hand the
	// deployment a tool it never accepted.
	if names := h.toolNames(); slices.Contains(names, typo) || slices.Contains(names, echoToolName) {
		t.Errorf("registry advertises the refused entry's tools: %v", names)
	}
	requireGateable(t, typo, false, "after the refusal")
	requireGateable(t, echoToolName, false, "after the refusal")
	requireFailedRow(t, h.loader.Status(), echoPluginName, typo)
}
