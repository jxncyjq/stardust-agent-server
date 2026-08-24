package loader

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/fetch"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/plugin/sign"
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
//
// The remote-source scenarios appended at the end of this file carry their own
// bounds note; every server they use is an httptest.Server serving one fixed,
// pre-built archive, and none of them loops over Apply.

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

// ---------------------------------------------------------------------------
// A4b acceptance: dependency convergence, driven from a plugins.json on disk.
//
// suspend_test.go already covers the convergence entry by entry, through
// manifest.Entry values built in memory. What follows is the outside-in pass:
// the same three-plugin chain written as a plugins.json FILE, read back and
// parsed the way serve does, with every claim made through the surfaces an
// operator and a model actually see — the real *tool.Registry, the
// process-global gateable catalog, the lifecycle ledger, the published event
// stream and the InstanceStatus rows `agent plugins status` renders.
//
// # Bounds (fork-bomb regime)
//
// None of these tests loops over Apply: each is a written-out sequence of at
// most three applies, and every one of them is followed by
// requireInstanceCeiling with that stage's own literal ceiling, so a
// convergence that leaked an instance fails at the apply that leaked it. There
// is no channel, no goroutine and no sleep anywhere below, so nothing here can
// wait on the feature under test.
//
// # Why the status assertions stop at InstanceStatus
//
// `agent plugins status` renders in internal/cli, which imports this package,
// so a test here cannot call the renderer without an import cycle. The rows
// below are its INPUT, and the two helpers requireDirectSuspension and
// requireCascadedSuspension assert exactly the facts internal/cli's
// suspendedWaitingOn branches on: whether any plugin provides the unresolved
// tool, and what state that provider is in. The rendering itself is covered
// against a real deployment in internal/cli's
// TestPluginsStatusNamesTheToolADirectSuspensionIsWaitingOn and
// TestPluginsStatusDistinguishesCascadedSuspensionFromDirect.

// The dependency chain these tests deploy from a manifest file:
//
//	e2e-a  provides ea_tool, requires nothing
//	e2e-b  provides eb_tool (and, where a squatter is needed, eb_aux),
//	       requires ea_tool
//	e2e-c  provides ec_tool, requires eb_tool
//
// They carry their own names rather than reusing suspend_test.go's dep-* chain
// so a failure names the pass it came from, and the names are short because
// patchIdentity has to fit each plugin's name and tools into a fixed-length
// literal.
const (
	e2eChainAPlugin = "e2e-a"
	e2eChainATool   = "ea_tool"

	e2eChainBPlugin = "e2e-b"
	e2eChainBTool   = "eb_tool"
	e2eChainBAux    = "eb_aux"

	e2eChainCPlugin = "e2e-c"
	e2eChainCTool   = "ec_tool"
)

// The ledger labels host.Activate files under a plugin's INSTANCE owner,
// spelled here to match internal/plugin/host/activate.go's ledgerLabelRuntime,
// ledgerLabelPool and ledgerLabelContributions. They are unexported there, so
// this is a copy; a rename that made the two disagree fails
// requireSuspendedInstanceIntact loudly rather than letting it assert nothing.
const (
	e2eLabelRuntime       = "wasm-runtime"
	e2eLabelPool          = "wasm-instance-pool"
	e2eLabelContributions = "tool-contributions"
)

// e2eChainEntryJSON renders one plugins.json entry for a chain plugin: literal
// JSON, the text an operator edits, rather than a marshalled struct.
func e2eChainEntryJSON(name, source string, tools ...string) string {
	accepts := make([]string, 0, len(tools))
	for _, toolName := range tools {
		accepts = append(accepts, fmt.Sprintf(`{"name": %q}`, toolName))
	}
	return fmt.Sprintf(`    {
      "name": %q,
      "source": %q,
      "enabled": true,
      "tools": [%s]
    }`, name, source, strings.Join(accepts, ", "))
}

// e2eChainManifestJSON assembles whole-file plugins.json text out of the
// entries e2eChainEntryJSON produced.
func e2eChainManifestJSON(entries ...string) string {
	return "{\n  \"plugins\": [\n" + strings.Join(entries, ",\n") + "\n  ]\n}"
}

// chainOwnerOf is the ledger instance owner one chain plugin is filed under.
// Every chain package is written at writeDep's version, 0.1.0.
func chainOwnerOf(name string) string { return "plugin:" + name + "@0.1.0" }

// requireLedgerLabels asserts the exact set of live ledger entries filed under
// one owner.
//
// Exact rather than "contains": a suspension that had also torn down the
// instance pool, and one that had left a second copy of it behind, are both
// states this has to fail on, and neither shows up in a containment check.
func (h *harness) requireLedgerLabels(when, owner string, want ...string) {
	h.t.Helper()

	got := append([]string(nil), h.ledger.Snapshot()[lifecycle.Owner(owner)]...)
	slices.Sort(got)
	sorted := append([]string(nil), want...)
	slices.Sort(sorted)
	wantStrings(h.t, fmt.Sprintf("%s: ledger entries under %s", when, owner), got, sorted)
}

// requireSuspendedInstanceIntact is the other half of "suspended is not
// unloaded": the plugin's contributions are gone, and its INSTANCE — the wasm
// runtime and the instance pool holding the guest's linear memory — is exactly
// as it was.
//
// The contribution owner must be absent, which is what makes the claim mean
// something: an implementation that unregistered the tools but left the
// contribution entries filed would pass every registry assertion and fail here.
func (h *harness) requireSuspendedInstanceIntact(when, name string) {
	h.t.Helper()

	owner := chainOwnerOf(name)
	// tool-contributions stays: that entry is filed under the INSTANCE owner and
	// is what a later teardown uses to withdraw whatever a Resume files. Only
	// the contribution owner's own entries go away.
	h.requireLedgerLabels(when+" ("+name+")", owner, e2eLabelRuntime, e2eLabelPool, e2eLabelContributions)
	if labels, still := h.ledger.Snapshot()[lifecycle.Owner(owner+"/tools")]; still {
		h.t.Fatalf("%s: plugin %q still holds contribution entries %v under %s; suspension must withdraw them",
			when, name, labels, owner+"/tools")
	}
}

// providersInStatus maps every tool name to the plugin whose Status row claims
// it, exactly as internal/cli's pluginToolProviders does. It is what makes a
// suspended row's blame legible: a tool with no provider here is one nobody
// installed, and a tool whose provider is itself suspended is a cascade.
func (h *harness) providersInStatus() map[string]string {
	h.t.Helper()

	providerOf := make(map[string]string)
	for _, row := range h.loader.Status() {
		for _, toolName := range row.Tools {
			if previous, taken := providerOf[toolName]; taken {
				h.t.Fatalf("Status reports two providers for tool %q (%s and %s); a suspended row's blame "+
					"would be ambiguous", toolName, previous, row.Name)
			}
			providerOf[toolName] = row.Name
		}
	}
	return providerOf
}

// requireDirectSuspension asserts that name is suspended waiting on toolName
// and that NO Status row provides that tool — the input internal/cli renders as
// "<tool>(no plugin provides it)".
func (h *harness) requireDirectSuspension(when, name, toolName string) {
	h.t.Helper()

	h.wantState(name, StateSuspended, toolName)
	if provider, provided := h.providersInStatus()[toolName]; provided {
		h.t.Fatalf("%s: plugin %q is blamed on %q, which %q provides; that is a cascade, not a direct "+
			"suspension", when, name, toolName, provider)
	}
}

// requireCascadedSuspension asserts that name is suspended waiting on toolName,
// that provider is the plugin whose Status row claims that tool, and that the
// provider is itself not working — the input internal/cli renders as
// "<tool>(cascade: <provider> is suspended)".
//
// The provider's Tools assertion is the load-bearing one: a suspended plugin
// that stopped reporting what it contributes would leave the cascade
// indistinguishable from a tool nobody ever installed, and would send an
// operator looking for a plugin to install instead of at the plugin that is
// actually down.
func (h *harness) requireCascadedSuspension(when, name, toolName, provider string) {
	h.t.Helper()

	h.wantState(name, StateSuspended, toolName)
	got, provided := h.providersInStatus()[toolName]
	if !provided {
		h.t.Fatalf("%s: plugin %q is blamed on %q, but no Status row reports providing it; the cascade back "+
			"to %q is invisible to an operator", when, name, toolName, provider)
	}
	if got != provider {
		h.t.Fatalf("%s: Status reports %q as the provider of %q, want %q", when, got, toolName, provider)
	}
	if state := h.statusOf(provider).State; state == StateLoaded {
		h.t.Fatalf("%s: plugin %q is blamed on %q, whose provider %q reports %q; a cascade means the provider "+
			"is not working either", when, name, toolName, provider, state)
	}
}

// requireSuspendedEventCascade asserts that exactly one plugin/suspended event
// was published for name and that it carries the cascade= label wanted.
//
// This is the Loader's OWN rendering of the same distinction, on the stream an
// operator tails rather than in the table they poll, and it is asserted
// separately because the two are produced by different code: unresolvedRequires
// decides the event's cascade=, while a status row's is re-derived from the
// provider set.
func (h *harness) requireSuspendedEventCascade(name, cascade string) {
	h.t.Helper()

	var matched []string
	for _, message := range h.messagesOfType(RuntimeEventSuspended) {
		if strings.Contains(message, "plugin="+name+" ") {
			matched = append(matched, message)
		}
	}
	if len(matched) != 1 {
		h.t.Fatalf("plugin/suspended events for %s: got %d (%v), want exactly 1", name, len(matched), matched)
	}
	if !strings.Contains(matched[0], "cascade="+cascade) {
		h.t.Fatalf("plugin/suspended message for %s = %q, want cascade=%s", name, matched[0], cascade)
	}
}

// requireLoadCount asserts how many plugin/loaded events one plugin has
// published so far. One across a whole suspend-and-resume cycle is the proof
// that the resume reused the mounted instance instead of rebuilding the guest.
func (h *harness) requireLoadCount(when, name string, want int) {
	h.t.Helper()

	loads := 0
	for _, message := range h.messagesOfType(RuntimeEventLoaded) {
		if strings.Contains(message, "plugin="+name+" ") {
			loads++
		}
	}
	if loads != want {
		h.t.Fatalf("%s: plugin/loaded events for %s = %d, want %d", when, name, loads, want)
	}
}

// requireChainToolServes calls one chain plugin's tool the way a model does and
// asserts the guest answered. The fixture echoes "<tool>:<arguments as compact
// JSON>", so the answer proves the call reached the guest and came back — a
// plugin whose tool is merely REGISTERED, against a pool that was disposed,
// fails here while passing every catalog assertion.
func (h *harness) requireChainToolServes(ctx context.Context, when, toolName string) {
	h.t.Helper()

	result, _, err := h.executeAsModel(ctx, domain.ToolCall{
		ID:        "model-call-" + toolName + "-" + when,
		Name:      toolName,
		Arguments: map[string]string{"probe": when},
	})
	if err != nil {
		h.t.Fatalf("%s: Execute(%q): %v", when, toolName, err)
	}
	want := fmt.Sprintf(`%s:{"probe":%q}`, toolName, when)
	if !result.Success || result.Output != want {
		h.t.Fatalf("%s: Execute(%q) = %+v, want a successful %q", when, toolName, result, want)
	}
}

// TestE2EDependencyChainSuspendsAndResumesFromTheManifestFile is Step 1 of the
// A4b acceptance, with Step 2's observability asserted at every stage of it.
//
//	a plugins.json declaring e2e-a -> e2e-b -> e2e-c -> Apply -> all three
//	active, all three tools in the registry AND in the gateable catalog, and
//	each one SERVES -> the operator deletes e2e-a from the file -> reload ->
//	e2e-b and e2e-c are suspended, every tool of theirs is gone from the
//	registry and the gateable catalog, e2e-a's ledger owner is gone, but
//	e2e-b's and e2e-c's instances (wasm runtime + instance pool) are untouched
//	-> the operator puts e2e-a back -> reload -> ONE convergence brings the
//	whole chain back, on the instances it never rebuilt.
//
// Every "after" claim is paired with its "before": the three tools are proven
// absent from the gateable catalog before the first apply and proven serving
// before the manifest is edited, so nothing here is satisfied by a state that
// was already true.
//
// There is no loop: three applies, written out, each followed by its own
// instance ceiling.
func TestE2EDependencyChainSuspendsAndResumesFromTheManifestFile(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	chainTools := []string{e2eChainATool, e2eChainBTool, e2eChainCTool}
	// The gateable catalog is process-global: a leak from an earlier test would
	// make this test's gateable assertions vacuous.
	for _, name := range chainTools {
		if toolauth.IsGateable(name) {
			t.Fatalf("%q is already gateable before any apply: an earlier test leaked its contribution", name)
		}
	}
	if names := h.toolNames(); len(names) != 0 {
		t.Fatalf("registry already advertises %v before any apply", names)
	}

	h.writeDep("e2ea", e2eChainAPlugin, e2eChainATool)
	h.writeDep("e2eb", e2eChainBPlugin, e2eChainBTool, e2eChainATool)
	h.writeDep("e2ec", e2eChainCPlugin, e2eChainCTool, e2eChainBTool)

	entryA := e2eChainEntryJSON(e2eChainAPlugin, "e2ea", e2eChainATool)
	entryB := e2eChainEntryJSON(e2eChainBPlugin, "e2eb", e2eChainBTool)
	entryC := e2eChainEntryJSON(e2eChainCPlugin, "e2ec", e2eChainCTool)
	wholeChain := e2eChainManifestJSON(entryA, entryB, entryC)
	withoutProvider := e2eChainManifestJSON(entryB, entryC)

	// Stage 1: the whole chain, from the file.
	h.writeManifest(wholeChain)
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("startup Apply of the whole chain: %v", err)
	}
	h.requireInstanceCeiling("after the startup apply", 3)

	wantStrings(t, "registry after the startup apply", h.toolNames(), chainTools)
	for _, name := range chainTools {
		requireGateable(t, name, true, "after the startup apply")
	}
	wantStrings(t, "ledger instance owners after the startup apply", h.owners(),
		[]string{chainOwnerOf(e2eChainAPlugin), chainOwnerOf(e2eChainBPlugin), chainOwnerOf(e2eChainCPlugin)})
	for _, name := range []string{e2eChainAPlugin, e2eChainBPlugin, e2eChainCPlugin} {
		h.wantState(name, StateLoaded)
	}
	for _, name := range chainTools {
		h.requireChainToolServes(ctx, "up", name)
	}
	if got := len(h.messagesOfType(RuntimeEventSuspended)); got != 0 {
		t.Fatalf("plugin/suspended events while the whole chain is up: got %d, want 0", got)
	}

	// Stage 2: the operator deletes the root entry from plugins.json.
	h.writeManifest(withoutProvider)
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("reload after the provider was deleted from the manifest: %v", err)
	}
	h.requireInstanceCeiling("after the provider left", 2)

	// Nothing the chain provides is reachable any more — not the tool whose
	// provider left, and not the two whose providers are merely suspended.
	wantStrings(t, "registry after the provider left", h.toolNames(), nil)
	for _, name := range chainTools {
		requireGateable(t, name, false, "after the provider left")
	}
	if _, _, err := h.executeAsModel(ctx, domain.ToolCall{ID: "call-suspended", Name: e2eChainBTool}); !errors.Is(err, tool.ErrToolNotFound) {
		t.Errorf("Execute(%q) against a suspended plugin = %v, want ErrToolNotFound", e2eChainBTool, err)
	}

	// The deleted entry is really gone, and the two suspended ones really are
	// still mounted: same owners, instances intact.
	wantStrings(t, "ledger instance owners after the provider left", h.owners(),
		[]string{chainOwnerOf(e2eChainBPlugin), chainOwnerOf(e2eChainCPlugin)})
	if labels, still := h.ledger.Snapshot()[lifecycle.Owner(chainOwnerOf(e2eChainAPlugin))]; still {
		t.Errorf("the deleted plugin %q still holds ledger entries %v; it was unloaded, not suspended",
			e2eChainAPlugin, labels)
	}
	h.requireSuspendedInstanceIntact("after the provider left", e2eChainBPlugin)
	h.requireSuspendedInstanceIntact("after the provider left", e2eChainCPlugin)

	// Step 2: what an operator reads. The direct suspension and the cascade are
	// told apart, in the status rows and in the event stream alike.
	h.requireDirectSuspension("after the provider left", e2eChainBPlugin, e2eChainATool)
	h.requireCascadedSuspension("after the provider left", e2eChainCPlugin, e2eChainBTool, e2eChainBPlugin)
	h.requireSuspendedEventCascade(e2eChainBPlugin, cascadeNo)
	h.requireSuspendedEventCascade(e2eChainCPlugin, cascadeYes)

	// Stage 3: the operator puts the entry back. ONE convergence, whole chain.
	h.writeManifest(wholeChain)
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("reload after the provider was restored: %v", err)
	}
	h.requireInstanceCeiling("after the provider returned", 3)

	for _, name := range []string{e2eChainAPlugin, e2eChainBPlugin, e2eChainCPlugin} {
		h.wantState(name, StateLoaded)
	}
	wantStrings(t, "registry after the provider returned", h.toolNames(), chainTools)
	for _, name := range chainTools {
		requireGateable(t, name, true, "after the provider returned")
		h.requireChainToolServes(ctx, "back", name)
	}
	// The two that came back were RESUMED, not rebuilt: one plugin/loaded each
	// across the whole sequence, against the root's two — it really was unloaded
	// and mounted again, which is what keeps "1" from being a number every
	// plugin here happens to have.
	h.requireLoadCount("after the provider returned", e2eChainBPlugin, 1)
	h.requireLoadCount("after the provider returned", e2eChainCPlugin, 1)
	h.requireLoadCount("after the provider returned", e2eChainAPlugin, 2)
	if got := len(h.messagesOfType(RuntimeEventResumed)); got != 2 {
		t.Errorf("plugin/resumed events: got %d (%v), want 2",
			got, h.messagesOfType(RuntimeEventResumed))
	}
}

// TestE2ECyclicManifestIsRefusedWithoutTouchingWhatIsRunning is the first of
// Step 3's rejection paths, driven from the manifest file: a plugins.json whose
// entries require each other in a loop.
//
// The refusal has to name the plugins on the cycle — an operator whose only
// clue is "dependency cycle" has to go read every manifest themselves — and it
// must not disturb the plugin that was already working. "Untouched" is asserted
// on every surface separately: the ledger owner, the gateable catalog, the
// status row, and one real tool call.
func TestE2ECyclicManifestIsRefusedWithoutTouchingWhatIsRunning(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.writeEcho("1.0.0")
	h.writeManifest(e2eEchoOnlyManifest())
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("startup Apply: %v", err)
	}
	h.requireInstanceCeiling("after the startup apply", 1)
	requireGateable(t, echoToolName, true, "after the startup apply")
	beforeOwners := h.owners()
	wantStrings(t, "ledger instance owners after the startup apply", beforeOwners,
		[]string{"plugin:" + echoPluginName + "@1.0.0"})

	// Two entries that require each other: an unresolvable order, and operator
	// data rather than a programming error.
	h.writeDep("e2ea", e2eChainAPlugin, e2eChainATool, e2eChainBTool)
	h.writeDep("e2eb", e2eChainBPlugin, e2eChainBTool, e2eChainATool)
	h.writeManifest(e2eChainManifestJSON(
		e2eChainEntryJSON(echoPluginName, "echo", echoToolName),
		e2eChainEntryJSON(e2eChainAPlugin, "e2ea", e2eChainATool),
		e2eChainEntryJSON(e2eChainBPlugin, "e2eb", e2eChainBTool),
	))

	err := h.applyManifest(ctx)
	if err == nil {
		t.Fatalf("Apply() of a cyclic manifest error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("Apply() error = %q, want it to name the cycle", err)
	}
	for _, name := range []string{e2eChainAPlugin, e2eChainBPlugin} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("Apply() error = %q, want it to name %q, a plugin on the cycle", err, name)
		}
	}
	h.requireInstanceCeiling("after the cyclic manifest", 3)

	// The plugin that was already running is exactly where it was. The owner set
	// is compared EXACTLY, and against the owner recorded BEFORE the refusal:
	// "changes nothing" is violated by a convergence that remounted it under a
	// new owner just as much as by one that unmounted it. The two cyclic entries
	// are listed because pass 3 does mount them — the cycle is only discovered
	// afterwards, by a convergence that then declines to move anybody.
	wantStrings(t, "ledger instance owners after the cyclic manifest", h.owners(), []string{
		chainOwnerOf(e2eChainAPlugin),
		chainOwnerOf(e2eChainBPlugin),
		beforeOwners[0],
	})
	requireGateable(t, echoToolName, true, "after the cyclic manifest")
	h.wantState(echoPluginName, StateLoaded)
	result, _, execErr := h.executeAsModel(ctx, domain.ToolCall{
		ID:        "model-call-after-cycle",
		Name:      echoToolName,
		Arguments: map[string]string{"text": "still here"},
	})
	if execErr != nil {
		t.Fatalf("Execute(%q) after the cyclic manifest was refused: %v", echoToolName, execErr)
	}
	if want := echoToolName + `:{"text":"still here"}`; result.Output != want {
		t.Errorf("Execute(%q) after the cyclic manifest result.Output = %q, want %q", echoToolName, result.Output, want)
	}
	// Nobody was suspended and nobody was unloaded: a graph that never produced
	// an answer decides nothing.
	if got := len(h.messagesOfType(RuntimeEventSuspended)); got != 0 {
		t.Errorf("plugin/suspended events over a cyclic manifest: got %d (%v), want 0",
			got, h.messagesOfType(RuntimeEventSuspended))
	}
	if got := len(h.eventsOfType(RuntimeEventUnloaded)); got != 0 {
		t.Errorf("plugin/unloaded events over a cyclic manifest: got %d, want 0", got)
	}
}

// TestE2EResumeRefusedByATakenToolNameStaysSuspendedWithItsReason is Step 3's
// second rejection: while a plugin is suspended its tool names are free, so
// somebody else may legitimately take one — and the plugin then cannot come
// back.
//
// The requirement is that this is an ERROR, not a panic: both
// tool.Registry.RegisterDescriptor and toolauth.Contribute answer a duplicate
// name by panicking, so a resume that contributed without checking first would
// take the whole process down over a state its operator can fix. The plugin has
// to stay suspended with a reason a status row can show.
//
// The squatter takes eb_aux — a name e2e-b provides and nobody requires —
// rather than eb_tool itself, because taking eb_tool would leave that name
// registered by the squatter and entitle e2e-c to come back, which is the
// opposite of the state under test.
func TestE2EResumeRefusedByATakenToolNameStaysSuspendedWithItsReason(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.writeDep("e2ea", e2eChainAPlugin, e2eChainATool)
	h.writeDepTools("e2eb", e2eChainBPlugin, []string{e2eChainBTool, e2eChainBAux}, e2eChainATool)
	h.writeDep("e2ec", e2eChainCPlugin, e2eChainCTool, e2eChainBTool)

	entryA := e2eChainEntryJSON(e2eChainAPlugin, "e2ea", e2eChainATool)
	entryB := e2eChainEntryJSON(e2eChainBPlugin, "e2eb", e2eChainBTool, e2eChainBAux)
	entryC := e2eChainEntryJSON(e2eChainCPlugin, "e2ec", e2eChainCTool)

	// The chain without its root: both dependents mount and suspend.
	h.writeManifest(e2eChainManifestJSON(entryB, entryC))
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("startup Apply without the root entry: %v", err)
	}
	h.requireInstanceCeiling("after the startup apply", 2)
	h.wantState(e2eChainBPlugin, StateSuspended, e2eChainATool)
	h.wantState(e2eChainCPlugin, StateSuspended, e2eChainBTool)
	// The name the squatter is about to take is free precisely BECAUSE e2e-b is
	// suspended; asserting it here is what makes the registration below a
	// legitimate one rather than a duplicate the registry would have refused.
	if names := h.toolNames(); slices.Contains(names, e2eChainBAux) {
		t.Fatalf("registry advertises %q while %q is suspended: %v", e2eChainBAux, e2eChainBPlugin, names)
	}

	revokeSquatter := h.registry.Register(e2eChainBAux, tool.HandlerFunc(
		func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{}, nil
		}))
	t.Cleanup(revokeSquatter)

	// The operator restores the root entry. The resume of e2e-b now cannot
	// happen, and e2e-c's cannot either.
	h.writeManifest(e2eChainManifestJSON(entryA, entryB, entryC))
	err := h.applyManifest(ctx)
	if err == nil {
		t.Fatalf("Apply() whose resume hits a taken tool name error = nil, want a refusal")
	}
	h.requireInstanceCeiling("after the refused resume", 3)
	// Both hops are named: the resume that failed on the taken name, and the one
	// downstream of it that stayed down because of it.
	for _, want := range []string{e2eChainBPlugin, e2eChainBAux, e2eChainCPlugin, e2eChainBTool} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Apply() error = %q, want it to name %q", err, want)
		}
	}

	// The root came up regardless — one entry's failure never aborts the rest —
	// and it serves.
	h.wantState(e2eChainAPlugin, StateLoaded)
	requireGateable(t, e2eChainATool, true, "after the refused resume")
	h.requireChainToolServes(ctx, "root", e2eChainATool)

	// e2e-b stayed suspended, and its reason is the taken name rather than a
	// dependency: ea_tool is back and registered, so blaming that would send an
	// operator after something which is already fixed.
	h.wantState(e2eChainBPlugin, StateSuspended)
	if row := h.statusOf(e2eChainBPlugin); !strings.Contains(row.LastError, e2eChainBAux) {
		t.Errorf("Status row for %q LastError = %q, want it to name the taken tool %q",
			e2eChainBPlugin, row.LastError, e2eChainBAux)
	}
	// e2e-c's reason IS a missing dependency: eb_tool never came back.
	h.requireCascadedSuspension("after the refused resume", e2eChainCPlugin, e2eChainBTool, e2eChainBPlugin)
	if row := h.statusOf(e2eChainCPlugin); row.LastError == "" {
		t.Errorf("Status row for %q reports no LastError after a refused resume: %+v", e2eChainCPlugin, row)
	}

	// Neither of the two contributed anything: the registry holds the root's
	// tool and the squatter's, and nothing either of them provides is gateable.
	// The squatter's own name is not gateable either — it was registered
	// directly on the registry, so a gateable eb_aux would mean the refused
	// resume filed its catalog entries anyway.
	wantStrings(t, "registry after the refused resume", h.toolNames(),
		[]string{e2eChainATool, e2eChainBAux})
	for _, name := range []string{e2eChainBTool, e2eChainBAux, e2eChainCTool} {
		requireGateable(t, name, false, "after the refused resume")
	}
	h.requireSuspendedInstanceIntact("after the refused resume", e2eChainBPlugin)
	h.requireSuspendedInstanceIntact("after the refused resume", e2eChainCPlugin)
}

// ---------------------------------------------------------------------------
// a5a Task 5: the signature acceptance pass, at the seam where a refusal is
// observable — the real *tool.Registry and the process-global gateable
// catalog — driven from a plugins.json on disk through the real Loader.Apply.
//
// The operator-facing half of this acceptance (the real `agent plugins
// keygen`/`sign`/`reload` commands, the real serve assembly, and the
// require_signature switch) lives in internal/cli/plugins_command_test.go:
// those seams are in package cli, which imports THIS package, so an in-package
// test here cannot reach them without an import cycle. See
// TestSignedDeploymentAcceptanceFromKeygenThroughEveryTamper there.
//
// # Bounds (fork-bomb regime)
//
// Neither test below loops.
// TestE2ESignedDeploymentKeepsServingTheVerifiedInstanceThroughEveryTamper
// performs a literal FIVE Apply calls, written out one after another, and
// asserts the ledger's owner count against a declared ceiling of 2 after every
// one of them;
// TestE2EAnUnverifiableEntryContributesNothingWhileItsSignedSiblingServes
// performs exactly one. Nothing here waits on anything: no channel, no sleep,
// no polling.

// signPackageAs signs dir/plugin.json's RAW BYTES under the given key id and
// writes dir/plugin.sig beside it. The key id is a parameter because a
// signature naming a key the deployment does not trust is one of the refusals
// this acceptance has to provoke, and it is indistinguishable from a trusted
// one until the keyring is consulted.
func signPackageAs(t *testing.T, dir string, priv ed25519.PrivateKey, id sign.KeyID) {
	t.Helper()

	manifestData, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json in %s: %v", dir, err)
	}
	sig, err := sign.Sign(priv, id, manifestData)
	if err != nil {
		t.Fatalf("sign plugin.json in %s: %v", dir, err)
	}
	doc, err := sign.MarshalSignature(sig)
	if err != nil {
		t.Fatalf("encode plugin.sig for %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.sig"), doc, 0o644); err != nil {
		t.Fatalf("write plugin.sig in %s: %v", dir, err)
	}
}

// readFileForRestore reads a file a test is about to tamper with, so the
// tampering can be undone exactly. It returns the bytes rather than writing
// them anywhere: a test that forgets to restore leaves a temp directory
// behind, never the repository.
func readFileForRestore(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return append([]byte(nil), data...)
}

// writeBack restores a file readFileForRestore saved.
func writeBack(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("restore %s: %v", path, err)
	}
}

// flipOneByte rewrites path with its LAST byte incremented — one byte changed,
// the file's length and every other byte identical. It is the smallest edit
// that can be made to a binary, which is the point: the sha256 the signed
// plugin.json pins is what has to notice it.
func flipOneByte(t *testing.T, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s to tamper with it: %v", path, err)
	}
	if len(data) == 0 {
		t.Fatalf("%s is empty; there is no byte to flip and this test would prove nothing", path)
	}
	data[len(data)-1]++
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write the tampered %s: %v", path, err)
	}
}

// requireEchoStillServes calls the echo plugin's tool through the real
// registry and fails unless it answers exactly what the VERIFIED module
// answers.
//
// It is the load-bearing assertion of the tamper rounds below, and it is not
// satisfiable by a plugin that merely stayed in the ledger: the call reaches
// the wasm instance, and the fixture echoes its own arguments back, so an
// answer proves a working instance of the module that was verified is still
// the one serving. A tampered module that had been mounted would either fail
// to instantiate or answer differently.
func (h *harness) requireEchoStillServes(ctx context.Context, when string) {
	h.t.Helper()

	result, _, err := h.executeAsModel(ctx, domain.ToolCall{
		ID:        "model-call-echo-" + when,
		Name:      echoToolName,
		Arguments: map[string]string{"probe": when},
	})
	if err != nil {
		h.t.Fatalf("%s: Execute(%q): %v", when, echoToolName, err)
	}
	if !result.Success {
		h.t.Fatalf("%s: Execute(%q) result.Success = false (error %q), want true", when, echoToolName, result.Error)
	}
	if want := fmt.Sprintf("%s:{%q:%q}", echoToolName, "probe", when); result.Output != want {
		h.t.Fatalf("%s: Execute(%q) result.Output = %q, want %q", when, echoToolName, result.Output, want)
	}
}

// requireRefusedConvergenceLeftEverythingAlone is the "nothing moved" half of
// every tamper round: the same two owners are in the ledger under the same
// versions, both tools are still gateable, both status rows still say loaded
// at 1.0.0, and the echo plugin still answers a real call.
//
// Checking the OWNER STRINGS rather than a count is what makes it strong: an
// owner carries the plugin's version, so an instance that had been swapped for
// the tampered package would show up here as a different owner even though the
// count is unchanged.
func (h *harness) requireRefusedConvergenceLeftEverythingAlone(ctx context.Context, when string) {
	h.t.Helper()

	h.requireInstanceCeiling(when, 2)
	wantStrings(h.t, "ledger owners "+when, h.owners(),
		[]string{"plugin:" + proxyPluginName + "@1.0.0", "plugin:" + echoPluginName + "@1.0.0"})
	requireGateable(h.t, echoToolName, true, when)
	requireGateable(h.t, proxyToolName, true, when)
	for _, row := range h.loader.Status() {
		if row.State != StateLoaded {
			h.t.Fatalf("%s: Status row %q state = %q, want %q", when, row.Name, row.State, StateLoaded)
		}
		if row.Version != "1.0.0" {
			h.t.Fatalf("%s: Status row %q version = %q, want 1.0.0: a refused package must not become the "+
				"mounted one", when, row.Name, row.Version)
		}
	}
	h.requireEchoStillServes(ctx, when)
}

// requireEchoFailureSays fails unless the echo entry's status row explains the
// refusal with the given substring. The row is the running instance's — a
// convergence that refuses a tampered package does NOT unmount the verified
// instance it already has — so the reason has to travel onto that row, which
// is where `agent plugins status` reads it from (cli.mergePluginStatus renders
// a loaded row's LastError as error=...).
func (h *harness) requireEchoFailureSays(when, want string) {
	h.t.Helper()

	row := h.statusOf(echoPluginName)
	if row.LastError == "" {
		h.t.Fatalf("%s: Status row for %q carries no LastError; the refusal reached no operator", when, echoPluginName)
	}
	if !strings.Contains(row.LastError, want) {
		h.t.Fatalf("%s: Status row for %q LastError = %q, want it to mention %q", when, echoPluginName, row.LastError, want)
	}
}

// TestE2ESignedDeploymentKeepsServingTheVerifiedInstanceThroughEveryTamper is
// the acceptance for what a signature is FOR, at the seam a task actually
// touches.
//
// Two signed plugins are mounted from a plugins.json on disk. Then each of the
// three ways a package can stop being the one that was signed is applied to
// ONE of them, and after each the convergence must refuse, say which check
// failed, and leave the running deployment exactly as it was:
//
//  1. one byte of plugin.wasm changes -> refused on the sha256 the signed
//     plugin.json pins (this is the transitivity claim: the signature covers
//     plugin.json, and plugin.json covers the binary);
//  2. plugin.json changes, digest kept correct -> refused on the signature,
//     which is the only check that can catch it;
//  3. the package is re-signed by a key the keyring does not hold -> refused
//     by key id, naming the key that was offered.
//
// The positive state is established BEFORE any tampering — both tools serving
// real calls, both owners in the ledger — so no assertion below can be
// satisfied by a state that was already true.
//
// A fourth round puts the package back the way it was signed and converges
// cleanly, which is the control that makes the three refusals mean something:
// without it, "reload had stopped working" would pass rounds 1 to 3 just as
// well as "verification works".
//
// Bound: a literal five Apply calls, written out. No loop, no wait.
func TestE2ESignedDeploymentKeepsServingTheVerifiedInstanceThroughEveryTamper(t *testing.T) {
	priv, keyring := newTestKey(t)
	h := newHarnessWith(t, defaultTestApplyWait, keyring)
	ctx := context.Background()

	for _, name := range []string{echoToolName, proxyToolName} {
		if toolauth.IsGateable(name) {
			t.Fatalf("%q is already gateable before any apply: an earlier test leaked its contribution "+
				"and this test's gateable assertions would be vacuous", name)
		}
	}

	h.writeEcho("1.0.0")
	h.writeProxy("1.0.0")
	echoDir := filepath.Join(h.root, "echo")
	signPackageAs(t, echoDir, priv, testKeyID)
	signPackageAs(t, filepath.Join(h.root, "proxy"), priv, testKeyID)
	h.writeManifest(e2eBothEnabledManifest())

	// Apply 1 of 5: the startup convergence.
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("startup Apply: %v", err)
	}
	h.requireInstanceCeiling("after the startup apply", 2)
	wantStrings(t, "ledger owners after the startup apply", h.owners(),
		[]string{"plugin:" + proxyPluginName + "@1.0.0", "plugin:" + echoPluginName + "@1.0.0"})
	requireGateable(t, echoToolName, true, "after the startup apply")
	requireGateable(t, proxyToolName, true, "after the startup apply")
	for _, row := range h.loader.Status() {
		if row.State != StateLoaded || row.LastError != "" {
			t.Fatalf("Status row %q after the startup apply = %+v, want loaded with no error", row.Name, row)
		}
	}
	h.requireEchoStillServes(ctx, "after the startup apply")

	wasmPath := filepath.Join(echoDir, "plugin.wasm")
	manifestFile := filepath.Join(echoDir, "plugin.json")
	signatureFile := filepath.Join(echoDir, "plugin.sig")
	originalWasm := readFileForRestore(t, wasmPath)
	originalManifest := readFileForRestore(t, manifestFile)
	originalSignature := readFileForRestore(t, signatureFile)

	// Round 1 (Apply 2 of 5): the BINARY is tampered with and nothing else.
	// plugin.sig still verifies — it covers plugin.json, which nobody touched —
	// so only the digest that signed manifest pins can catch this.
	flipOneByte(t, wasmPath)
	err := h.applyManifest(ctx)
	if err == nil {
		t.Fatal("Apply() error = nil after one byte of plugin.wasm changed, want a refusal")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("Apply() error = %v, want it to name the sha256 mismatch", err)
	}
	h.requireEchoFailureSays("after the tampered module was refused", "sha256")
	h.requireRefusedConvergenceLeftEverythingAlone(ctx, "after the tampered module was refused")
	writeBack(t, wasmPath, originalWasm)

	// Round 2 (Apply 3 of 5): the MANIFEST is tampered with, and its declared
	// sha256 is left correct on purpose. Every check but the signature passes,
	// so a refusal here can only come from the signature.
	retagVersion(t, echoDir, "9.9.9")
	err = h.applyManifest(ctx)
	if err == nil {
		t.Fatal("Apply() error = nil after plugin.json changed under its signature, want a refusal")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("Apply() error = %v, want it to say the signature did not verify", err)
	}
	h.requireEchoFailureSays("after the re-tagged manifest was refused", "signature")
	h.requireRefusedConvergenceLeftEverythingAlone(ctx, "after the re-tagged manifest was refused")
	writeBack(t, manifestFile, originalManifest)
	writeBack(t, signatureFile, originalSignature)

	// Round 3 (Apply 4 of 5): the package is intact and properly signed — by a
	// key this deployment does not trust. Nothing about the signature is
	// malformed; the only thing wrong with it is who made it.
	const rogueKeyID = sign.KeyID("rogue-key")
	rogue, _ := newTestKeyWithID(t, rogueKeyID)
	signPackageAs(t, echoDir, rogue, rogueKeyID)
	err = h.applyManifest(ctx)
	if err == nil {
		t.Fatal("Apply() error = nil for a package signed by an untrusted key, want a refusal")
	}
	if !strings.Contains(err.Error(), string(rogueKeyID)) {
		t.Errorf("Apply() error = %v, want it to name the key id the signature was made with", err)
	}
	if !strings.Contains(err.Error(), string(testKeyID)) {
		t.Errorf("Apply() error = %v, want it to name the keys this deployment does trust", err)
	}
	h.requireEchoFailureSays("after the untrusted signature was refused", string(rogueKeyID))
	h.requireRefusedConvergenceLeftEverythingAlone(ctx, "after the untrusted signature was refused")

	// Round 4 (Apply 5 of 5): the control, and the regression test for the
	// defect this acceptance pass found (see the a5a-task-5 report). The
	// package is put back exactly as it was signed, and the same deployment
	// converges cleanly — which is what makes the three refusals above mean
	// something rather than "reload had stopped working".
	//
	// The entry is UNCHANGED as far as the fingerprint is concerned, so it is
	// not activated again; before the fix, that meant nothing ever cleared the
	// note round 3 left behind, and `agent plugins status` kept reporting the
	// untrusted key for the rest of the process's life.
	signPackageAs(t, echoDir, priv, testKeyID)
	if err := h.applyManifest(ctx); err != nil {
		t.Fatalf("Apply() error = %v, want nil: the package is intact and signed by a trusted key again", err)
	}
	h.requireInstanceCeiling("after the package was signed properly again", 2)
	wantStrings(t, "ledger owners after the package was signed properly again", h.owners(),
		[]string{"plugin:" + proxyPluginName + "@1.0.0", "plugin:" + echoPluginName + "@1.0.0"})
	for _, row := range h.loader.Status() {
		if row.State != StateLoaded {
			t.Fatalf("Status row %q state = %q, want %q", row.Name, row.State, StateLoaded)
		}
		if row.LastError != "" {
			t.Fatalf("Status row %q still carries error %q after a convergence that succeeded; an operator "+
				"who fixed the package can never tell a stale failure from a live one", row.Name, row.LastError)
		}
	}
	h.requireEchoStillServes(ctx, "after the package was signed properly again")
}

// TestE2EAnUnverifiableEntryContributesNothingWhileItsSignedSiblingServes is
// the refusal's blast radius, at the two authorities a refused plugin must not
// reach: the tool registry a model calls through, and the PROCESS-GLOBAL
// gateable catalog a per-agent disabled_tools list resolves against.
//
// A tool that is callable but not gateable is an authorization bypass, and a
// tool that is gateable but belongs to a plugin that never mounted is a ghost
// entry in every agent's permission UI. Neither may survive a refusal, and
// "the plugin is not in the ledger" does not prove either of them on its own.
//
// The signed sibling in the same plugins.json is the control: it proves the
// deployment converged at all, so the unsigned entry's absence is a refusal
// rather than an Apply that did nothing.
//
// Bound: exactly one Apply. No loop, no wait.
func TestE2EAnUnverifiableEntryContributesNothingWhileItsSignedSiblingServes(t *testing.T) {
	priv, keyring := newTestKey(t)
	h := newHarnessWith(t, defaultTestApplyWait, keyring)
	ctx := context.Background()

	for _, name := range []string{echoToolName, proxyToolName} {
		if toolauth.IsGateable(name) {
			t.Fatalf("%q is already gateable before any apply: an earlier test leaked its contribution "+
				"and this test's gateable assertions would be vacuous", name)
		}
	}

	h.writeEcho("1.0.0")
	h.writeProxy("1.0.0")
	// Only the echo package is signed. The proxy package is left exactly as it
	// was written: a complete, correct, UNSIGNED package.
	signPackageAs(t, filepath.Join(h.root, "echo"), priv, testKeyID)
	h.writeManifest(e2eBothEnabledManifest())

	err := h.applyManifest(ctx)
	if err == nil {
		t.Fatal("Apply() error = nil, want the unsigned entry's refusal reported")
	}
	if !strings.Contains(err.Error(), proxyPluginName) {
		t.Errorf("Apply() error = %v, want it to name the entry that was refused", err)
	}
	if !strings.Contains(err.Error(), "plugin.sig") {
		t.Errorf("Apply() error = %v, want it to name the missing plugin.sig", err)
	}

	h.requireInstanceCeiling("after the mixed apply", 1)
	wantStrings(t, "ledger owners after the mixed apply", h.owners(),
		[]string{"plugin:" + echoPluginName + "@1.0.0"})

	// The signed sibling really converged: its tool is registered, gateable and
	// answers a model-shaped call.
	wantStrings(t, "registry tools after the mixed apply", h.toolNames(), []string{echoToolName})
	requireGateable(t, echoToolName, true, "after the mixed apply")
	h.requireEchoStillServes(ctx, "after the mixed apply")

	// The refused one reached neither authority.
	requireGateable(t, proxyToolName, false, "after the mixed apply")
	_, _, execErr := h.executeAsModel(ctx, domain.ToolCall{ID: "model-call-proxy", Name: proxyToolName})
	if execErr == nil {
		t.Fatalf("Execute(%q) error = nil, want an error: a refused plugin's tool must not be callable", proxyToolName)
	}
	if !errors.Is(execErr, tool.ErrToolNotFound) {
		t.Errorf("Execute(%q) error = %v, want %v", proxyToolName, execErr, tool.ErrToolNotFound)
	}

	// And the operator can see why, on a row of its own.
	row := h.statusOf(proxyPluginName)
	if row.State != StateFailed {
		t.Fatalf("Status row for %q state = %q, want %q", proxyPluginName, row.State, StateFailed)
	}
	if !strings.Contains(row.LastError, "plugin.sig") {
		t.Errorf("Status row for %q LastError = %q, want it to say the signature was the problem", proxyPluginName, row.LastError)
	}
}

// --- remote sources (a5b) --------------------------------------------------
//
// The acceptance for a plugins.json entry whose source is a URL. Everything
// below drives the same seam the rest of this file does — a plugins.json on
// disk, parsed and handed to Loader.Apply — with the real fetch, the real
// content-addressed cache and the real signature verification behind it.
//
// # Where the packages come from
//
// A remote package is written, signed and packed into a gzipped tar in a
// directory of its OWN, never under the deployment root. A mount in these
// tests can therefore only have come from the fetched bytes: there is nothing
// under the root to load instead.
//
// The signing is done with sign.GenerateKey and sign.Sign — the exact two
// calls `agent plugins keygen` and `agent plugins sign` make (see
// runPluginsKeygen and runPluginsSign in internal/cli). The commands
// themselves cannot be invoked from here: internal/cli imports this package,
// so a test in `package loader` that imported it back would not compile. What
// the commands add on top of these calls — where the private key file lives,
// what is printed, the refusal to overwrite a key — is covered by their own
// tests in internal/cli, and none of it changes the bytes that reach a
// verifier.
//
// # Bounds (fork-bomb regime)
//
//   - Every server here is an httptest.Server. No test in this file resolves a
//     name or opens a socket to anything else.
//   - Every handler writes ONE fixed archive, built once before the server
//     starts. Nothing generates bytes on demand, so no response can grow.
//   - No test below loops over Apply. Each one applies a written-out number of
//     times (one, or two for the pair that proves a restart), and asserts the
//     ledger's owner count against a declared ceiling after every one.
//   - No test below waits on anything: no channel, no sleep, no polling. The
//     only timeout in play is testFetchLimits().Timeout, which bounds a
//     download against a server that is in this process.
//
// # What is NOT asserted here, and where it is
//
// The Warn every allowed plaintext source gets is written at serve ASSEMBLY,
// by cli.checkRemoteSources — before any Loader exists. It is asserted in
// internal/cli by TestAssemblePluginsWarnsOnEveryAllowedInsecureRemoteSource,
// which counts one warning per plaintext entry. It cannot be asserted from
// here for the import reason above, and the Loader deliberately does not warn
// a second time: the policy is fixed when serve starts.

// e2eSource is an httptest server handing out one plugin archive, which a test
// can CUT OFF mid-run.
//
// Taking a source offline is how "a cache hit does not go online" is proved in
// the strong form: not by counting requests that were never made, but by
// making a request a test failure at the moment it arrives, at the SAME URL
// the first apply used. A second server on a second port would prove only that
// the second port was quiet.
type e2eSource struct {
	srv     *httptest.Server
	hits    atomic.Int64
	offline atomic.Bool
}

// newE2ESource starts a source serving archive over the transport start
// builds. The counter and the offline flag are atomic because they are written
// by the server's handler goroutine and read by the test's; a completed HTTP
// round trip is not a happens-before edge the race detector knows about.
//
// t is the test that owns the server's whole lifetime — an outer test when
// subtests share the source — so a request arriving from a subtest that has
// already finished still reports against a live *testing.T.
func newE2ESource(t *testing.T, start func(http.Handler) *httptest.Server, archive []byte) *e2eSource {
	t.Helper()

	src := &e2eSource{}
	src.srv = start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src.hits.Add(1)
		if src.offline.Load() {
			t.Errorf("the remote source was contacted (%s %s) after it was taken offline; this apply had to be "+
				"served entirely from the plugin cache, with no network access at all", r.Method, r.URL.Path)
			http.Error(w, "this source is offline", http.StatusServiceUnavailable)
			return
		}
		// One fixed archive, written once: this response cannot grow.
		if _, err := w.Write(archive); err != nil {
			t.Errorf("write archive to client: %v", err)
		}
	}))
	t.Cleanup(src.srv.Close)
	return src
}

// goOffline makes every later request to this source fail the test.
func (s *e2eSource) goOffline() { s.offline.Store(true) }

// url is the address a deployment entry names.
func (s *e2eSource) url() string { return s.srv.URL + "/echo.tgz" }

// requireHits fails the test unless the source has served exactly want
// requests.
func (s *e2eSource) requireHits(t *testing.T, when string, want int64) {
	t.Helper()

	if got := s.hits.Load(); got != want {
		t.Errorf("%s: the remote source was requested %d times, want exactly %d", when, got, want)
	}
}

// e2eRemoteEchoManifest is the plugins.json an operator writes for a remote
// package: the same document as a local entry's, with a URL for a source and
// the mandatory digest beside it. It is literal JSON — including the "digest"
// field NAME, which only a manifest read off disk exercises — so a change to
// plugins.json's shape breaks these tests instead of travelling silently
// through the same struct tags on both sides.
func e2eRemoteEchoManifest(source, digest string) string {
	return fmt.Sprintf(`{
  "plugins": [
    {
      "name": %q,
      "source": %q,
      "digest": %q,
      "enabled": true,
      "tools": [{"name": %q}]
    }
  ]
}`, echoPluginName, source, digest, echoToolName)
}

// e2eCachedPackageDir is where the cache files the package named by digest,
// spelled out from the documented layout rather than asked of the Cache. The
// layout is what an operator inspects and what an offline deployment ships, so
// a test that asked cache.Dir would be checking the code under test against
// itself.
func e2eCachedPackageDir(cache *fetch.Cache, digest string) string {
	return filepath.Join(cache.Root(), "sha256", strings.TrimPrefix(digest, "sha256:"))
}

// requireCachedPackage fails unless the cache holds the package named by
// digest as three non-empty regular files, and nothing else, in the documented
// directory.
func requireCachedPackage(t *testing.T, cache *fetch.Cache, digest, when string) {
	t.Helper()

	dir := e2eCachedPackageDir(cache, digest)
	for _, name := range remotePackageFileNames {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: stat %s in %s: %v; a fetched package is filed under its digest as the three "+
				"files a package consists of", when, name, dir, err)
		}
		if !info.Mode().IsRegular() {
			t.Errorf("%s: %s in the cache has mode %s, want a regular file", when, name, info.Mode())
		}
		if info.Size() == 0 {
			t.Errorf("%s: %s in the cache is empty", when, name)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("%s: read the cached package directory %s: %v", when, dir, err)
	}
	if len(entries) != len(remotePackageFileNames) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s: the cached package directory holds %v, want exactly %v",
			when, names, remotePackageFileNames)
	}
}

// requireNoFilesUnder fails the test if dir holds any file at all, at any
// depth, naming everything it found.
//
// It is how the refusals below prove "nothing was written", and it is
// deliberately a walk of the whole tree rather than a check of one expected
// path: an unpack that escaped its destination would put its file somewhere no
// single Stat would look, which is the entire point of escaping.
func requireNoFilesUnder(t *testing.T, dir, when string) {
	t.Helper()

	var found []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s: walk %s: %v", when, dir, err)
	}
	if len(found) > 0 {
		t.Fatalf("%s: %s holds %v, want no files at all: bytes a convergence refused must never reach disk",
			when, dir, found)
	}
}

// e2eTarGzWithTraversalEntry packs dir's three package files and then ONE more
// regular-file entry whose name escapes the directory it would be unpacked
// into.
//
// The escaping entry is written LAST on purpose: the three legitimate files
// are read and held before the hostile one is even seen, so an unpack that
// wrote as it went, or that skipped the bad entry and kept the rest, would
// leave those three behind. Refusing the whole archive is the invariant, and
// this ordering is what can tell the two apart.
//
// The archive is built here and never committed. Its four entries and its few
// hundred bytes of payload are fixed by this function.
func e2eTarGzWithTraversalEntry(t *testing.T, dir, escapingName string) []byte {
	t.Helper()

	buf := &bytes.Buffer{}
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)
	write := func(name string, data []byte) {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("write tar header for %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write tar body for %s: %v", name, err)
		}
	}
	for _, name := range remotePackageFileNames {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s in %s: %v", name, dir, err)
		}
		write(name, data)
	}
	write(escapingName, []byte("this file must never be written\n"))
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// requireEchoNotDeployed is the shared "the refused entry reached nothing"
// assertion: its tool is in neither the registry nor the PROCESS-GLOBAL
// gateable catalog, and calling it reports ErrToolNotFound.
//
// All three are checked because none implies the others: a tool that is
// callable but not gateable is an authorization bypass, and a tool that is
// gateable but belongs to a plugin that never mounted is a ghost entry in
// every agent's permission UI.
func (h *harness) requireEchoNotDeployed(ctx context.Context, when string) {
	h.t.Helper()

	h.requireInstanceCeiling(when, 0)
	wantStrings(h.t, "ledger owners "+when, h.owners(), nil)
	wantStrings(h.t, "registry tools "+when, h.toolNames(), nil)
	requireGateable(h.t, echoToolName, false, when)
	_, _, err := h.executeAsModel(ctx, domain.ToolCall{ID: "model-call-echo-" + when, Name: echoToolName})
	if err == nil {
		h.t.Fatalf("%s: Execute(%q) error = nil, want an error: a refused plugin's tool must not be callable",
			when, echoToolName)
	}
	if !errors.Is(err, tool.ErrToolNotFound) {
		h.t.Errorf("%s: Execute(%q) error = %v, want %v", when, echoToolName, err, tool.ErrToolNotFound)
	}
}

// requireRemoteEchoMounted is the shared "the remote package really mounted"
// assertion: one owner at the expected version, the tool registered and
// gateable, the status row loaded and clean, and — the load-bearing part — a
// model-shaped call that reaches the wasm instance and comes back with what
// the fixture echoes. A plugin that merely sat in the ledger cannot satisfy
// the last one.
func (h *harness) requireRemoteEchoMounted(ctx context.Context, when, version string) {
	h.t.Helper()

	h.requireInstanceCeiling(when, 1)
	wantStrings(h.t, "ledger owners "+when, h.owners(), []string{"plugin:" + echoPluginName + "@" + version})
	wantStrings(h.t, "registry tools "+when, h.toolNames(), []string{echoToolName})
	requireGateable(h.t, echoToolName, true, when)
	row := h.statusOf(echoPluginName)
	if row.State != StateLoaded {
		h.t.Fatalf("%s: plugin %q State = %q, want %q (LastError %q)", when, echoPluginName, row.State,
			StateLoaded, row.LastError)
	}
	if row.Version != version {
		h.t.Errorf("%s: plugin %q Version = %q, want %q", when, echoPluginName, row.Version, version)
	}
	if row.LastError != "" {
		h.t.Errorf("%s: plugin %q LastError = %q, want empty", when, echoPluginName, row.LastError)
	}
	h.requireEchoStillServes(ctx, when)
}

// requireRemoteEchoFailed is the shared refusal assertion: Apply reported an
// error naming the entry, the operator can read the reason off the status row,
// and the entry reached none of the authorities a mounted plugin reaches.
func requireRemoteEchoFailed(ctx context.Context, t *testing.T, h *harness, applyErr error, when string, want ...string) {
	t.Helper()

	if applyErr == nil {
		t.Fatalf("%s: Apply() error = nil, want a refusal", when)
	}
	if !strings.Contains(applyErr.Error(), echoPluginName) {
		t.Errorf("%s: Apply() error = %v, want it to name the entry %q", when, applyErr, echoPluginName)
	}
	row := h.statusOf(echoPluginName)
	if row.State != StateFailed {
		t.Fatalf("%s: plugin %q State = %q, want %q (LastError %q)", when, echoPluginName, row.State,
			StateFailed, row.LastError)
	}
	if row.LastError == "" {
		t.Fatalf("%s: plugin %q carries no LastError; the refusal reached no operator", when, echoPluginName)
	}
	for _, substring := range want {
		if !strings.Contains(applyErr.Error(), substring) {
			t.Errorf("%s: Apply() error = %v, want it to mention %q", when, applyErr, substring)
		}
		if !strings.Contains(row.LastError, substring) {
			t.Errorf("%s: plugin %q LastError = %q, want it to mention %q", when, echoPluginName,
				row.LastError, substring)
		}
	}
	h.requireEchoNotDeployed(ctx, when)
}

// requireGateableIsClean fails the test if the echo fixture's tool is already
// in the process-global gateable catalog. Every gateable assertion below would
// be vacuous if an earlier test had leaked its contribution, so each test says
// so before it starts.
func requireGateableIsClean(t *testing.T) {
	t.Helper()

	if toolauth.IsGateable(echoToolName) {
		t.Fatalf("%q is already gateable before any apply: an earlier test leaked its contribution and "+
			"this test's gateable assertions would be vacuous", echoToolName)
	}
}

// TestE2EARemotePackageIsFetchedVerifiedCachedAndThenMountedOffline is the
// remote closed loop, from a key nobody has yet to a deployment that no longer
// needs the network:
//
//	mint a key -> sign the package with it -> pack it as a tar.gz -> serve it
//	-> write a plugins.json naming that https URL and the archive's sha256 ->
//	Apply -> the plugin mounts, its tool answers a real call, and the cache
//	holds the package under its digest -> take the source OFFLINE (a request
//	is now a test failure) -> a FRESH loader, ledger and registry over the SAME
//	cache -> Apply -> it mounts again, from disk, having contacted nothing
//
// The second half is a separate subtest so that the first one's cleanup — the
// ledger disposal that unregisters the tool and its gateable entry — runs
// before it begins. That makes it a restart rather than a re-apply: the second
// Apply meets an empty ledger, an empty registry and a deployment root with no
// package in it, so the only place the plugin can come from is the cache the
// first half filled.
//
// Signatures are REQUIRED throughout: a fetched package goes through
// manifest.LoadPackage exactly as a local one does.
//
// Bound: two Apply calls, written out, one per subtest, each followed by an
// owner-count ceiling. No loop, no wait.
func TestE2EARemotePackageIsFetchedVerifiedCachedAndThenMountedOffline(t *testing.T) {
	requireGateableIsClean(t)
	ctx := context.Background()

	// `agent plugins keygen`, then `agent plugins sign`: the package is built,
	// signed and packed OUTSIDE any deployment root.
	priv, keyring := newTestKey(t)
	_, archive := signedEchoArchive(t, priv, "1.4.0")
	digest := digestOf(archive)
	cache := newTestCache(t)
	src := newE2ESource(t, httptest.NewTLSServer, archive)
	// The one document both halves apply, built once so the second half cannot
	// silently apply something else.
	document := e2eRemoteEchoManifest(src.url(), digest)

	requireNoFilesUnder(t, cache.Root(), "before the first apply")

	t.Run("the first start fetches, verifies and caches", func(t *testing.T) {
		h := newHarnessWithRemote(t, keyring, remoteFor(t, src.srv, cache, false))
		h.writeManifest(document)

		// Before: nothing of this plugin exists anywhere.
		wantStrings(t, "registry tools before the first apply", h.toolNames(), nil)
		requireGateable(t, echoToolName, false, "before the first apply")

		if err := h.applyManifest(ctx); err != nil {
			t.Fatalf("startup Apply: %v", err)
		}

		h.requireRemoteEchoMounted(ctx, "after the first apply", "1.4.0")
		src.requireHits(t, "after the first apply", 1)
		requireCachedPackage(t, cache, digest, "after the first apply")
	})

	t.Run("a restart mounts it again with the source unreachable", func(t *testing.T) {
		src.goOffline()
		h := newHarnessWithRemote(t, keyring, remoteFor(t, src.srv, cache, false))
		h.writeManifest(document)

		// This deployment root holds a plugins.json and nothing else: there is
		// no package under it to load instead of the cached one.
		if _, err := os.Stat(filepath.Join(h.root, "echo")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stat %s: %v, want it not to exist: a local package here would make the cache "+
				"assertion below vacuous", filepath.Join(h.root, "echo"), err)
		}
		wantStrings(t, "registry tools before the restart apply", h.toolNames(), nil)
		requireGateable(t, echoToolName, false, "before the restart apply")

		if err := h.applyManifest(ctx); err != nil {
			t.Fatalf("restart Apply: %v", err)
		}

		h.requireRemoteEchoMounted(ctx, "after the restart apply", "1.4.0")
		// The handler fails the test the moment it is contacted; this is the
		// second, cheaper witness to the same claim.
		src.requireHits(t, "after the restart apply", 1)
	})
}

// TestE2EARemoteDigestMismatchFailsTheEntryAndLeavesTheCacheEmpty is the first
// refusal: the bytes the source served are not the bytes the deployment asked
// for.
//
// What makes it more than "an error came back" is the pair of facts asserted
// together: the source WAS contacted and did serve the whole archive (one
// hit), and yet the cache holds no file at all. The bytes existed and were
// streamed; they never reached disk. Either assertion alone would be satisfied
// by a fetch that never happened.
//
// Bound: exactly one Apply. No loop, no wait.
func TestE2EARemoteDigestMismatchFailsTheEntryAndLeavesTheCacheEmpty(t *testing.T) {
	requireGateableIsClean(t)
	ctx := context.Background()

	priv, keyring := newTestKey(t)
	_, archive := signedEchoArchive(t, priv, "1.4.0")
	cache := newTestCache(t)
	src := newE2ESource(t, httptest.NewTLSServer, archive)
	h := newHarnessWithRemote(t, keyring, remoteFor(t, src.srv, cache, false))
	// A well-formed digest of some other bytes: the entry parses, and only the
	// comparison against what arrives can catch it.
	wrongDigest := digestOf([]byte("not the archive this source serves"))
	h.writeManifest(e2eRemoteEchoManifest(src.url(), wrongDigest))

	err := h.applyManifest(ctx)

	requireRemoteEchoFailed(ctx, t, h, err, "after the digest mismatch", "digest mismatch")
	src.requireHits(t, "after the digest mismatch", 1)
	requireNoFilesUnder(t, cache.Root(), "after the digest mismatch")
}

// TestE2EARemotePackageThatPassesItsDigestStillHasToPassItsSignature is the
// two-gates claim, and it is the reason a digest does not make a signature
// redundant.
//
// The package is signed and THEN re-tagged, leaving its own declared sha256
// correct. The archive is packed after that, and the entry names the archive's
// real digest — so the transport gate passes on every byte, and the only thing
// left that can refuse the package is the signature over its plugin.json.
//
// The proof that the two gates are independent is not the error message: it is
// that the package IS in the cache, filed whole under its digest, while the
// entry is failed. Gate one said yes and wrote the bytes down; gate two said
// no and nothing mounted.
//
// Bound: exactly one Apply. No loop, no wait.
func TestE2EARemotePackageThatPassesItsDigestStillHasToPassItsSignature(t *testing.T) {
	requireGateableIsClean(t)
	ctx := context.Background()

	priv, keyring := newTestKey(t)
	dir, _ := signedEchoArchive(t, priv, "1.4.0")
	retagVersion(t, dir, "1.4.1")
	archive := tarGzPackage(t, dir)
	digest := digestOf(archive)
	cache := newTestCache(t)
	src := newE2ESource(t, httptest.NewTLSServer, archive)
	h := newHarnessWithRemote(t, keyring, remoteFor(t, src.srv, cache, false))
	h.writeManifest(e2eRemoteEchoManifest(src.url(), digest))

	err := h.applyManifest(ctx)

	requireRemoteEchoFailed(ctx, t, h, err, "after the re-tagged package was refused", "signature")
	src.requireHits(t, "after the re-tagged package was refused", 1)
	// Gate one passed on exactly these bytes, and said so by filing them.
	requireCachedPackage(t, cache, digest, "after the re-tagged package was refused")
}

// TestE2EARemoteArchiveThatEscapesItsDirectoryIsRefusedWhole is the unpack
// refusal, at the level where it matters: an archive whose digest is perfectly
// correct — the deployment asked for exactly these bytes — and which is still
// not a package.
//
// The hostile entry is the LAST of four, after the three legitimate files, so
// "refused whole" and "the bad entry was skipped" cannot both pass: a skip
// would leave plugin.json, plugin.wasm and plugin.sig behind. The assertion is
// a walk of the cache root's PARENT, which catches both the files that would
// have been kept and the one that would have escaped — "../evil" unpacked
// under the cache resolves to a path no single Stat would think to look at.
//
// Bound: exactly one Apply, over a four-entry archive built in this test. No
// loop, no wait.
func TestE2EARemoteArchiveThatEscapesItsDirectoryIsRefusedWhole(t *testing.T) {
	requireGateableIsClean(t)
	ctx := context.Background()

	priv, keyring := newTestKey(t)
	dir, _ := signedEchoArchive(t, priv, "1.4.0")
	archive := e2eTarGzWithTraversalEntry(t, dir, "../evil")
	digest := digestOf(archive)
	cache := newTestCache(t)
	src := newE2ESource(t, httptest.NewTLSServer, archive)
	h := newHarnessWithRemote(t, keyring, remoteFor(t, src.srv, cache, false))
	h.writeManifest(e2eRemoteEchoManifest(src.url(), digest))

	err := h.applyManifest(ctx)

	requireRemoteEchoFailed(ctx, t, h, err, "after the traversing archive was refused", "../evil", "path element")
	src.requireHits(t, "after the traversing archive was refused", 1)
	// Neither the escaping entry nor the three legitimate files beside it.
	requireNoFilesUnder(t, filepath.Dir(cache.Root()), "after the traversing archive was refused")
}

// TestE2EThePlaintextSwitchIsTheOnlyDifferenceBetweenARefusalAndAMount is the
// debugging channel, both halves, over ONE plugins.json document.
//
// The same bytes are written as the manifest twice and served from the same
// http:// source; the only thing that differs between the two convergences is
// plugins.allow_insecure_sources. With it unstated the entry is refused and
// the source is never contacted at all — the refusal is decided before a
// request is built. With it on, the identical document mounts.
//
// Building the document once is what makes that claim exact: neither half can
// be quietly applying a different manifest from the other.
//
// The Warn that accompanies the second half is written at serve assembly, not
// by the Loader; see this section's header for where it is asserted.
//
// Bound: two Apply calls, written out, over two harnesses. The first mounts
// nothing, so the two cannot collide over the fixture's tool name. No loop, no
// wait.
func TestE2EThePlaintextSwitchIsTheOnlyDifferenceBetweenARefusalAndAMount(t *testing.T) {
	requireGateableIsClean(t)
	ctx := context.Background()

	priv, keyring := newTestKey(t)
	_, archive := signedEchoArchive(t, priv, "1.4.0")
	digest := digestOf(archive)
	cache := newTestCache(t)
	src := newE2ESource(t, httptest.NewServer, archive)
	document := e2eRemoteEchoManifest(src.url(), digest)

	// Half one: the switch is not written down at all, which is the default
	// and the safe side.
	refusing := newHarnessWithRemote(t, keyring, remoteFor(t, src.srv, cache, false))
	refusing.writeManifest(document)

	err := refusing.applyManifest(ctx)

	requireRemoteEchoFailed(ctx, t, refusing, err, "with plaintext refused",
		src.url(), "allow_insecure_sources")
	src.requireHits(t, "with plaintext refused", 0)
	requireNoFilesUnder(t, cache.Root(), "with plaintext refused")

	// Half two: the same document, the same source, the same cache — with the
	// switch on.
	allowing := newHarnessWithRemote(t, keyring, remoteFor(t, src.srv, cache, true))
	allowing.writeManifest(document)

	if err := allowing.applyManifest(ctx); err != nil {
		t.Fatalf("Apply() error = %v, want nil: allow_insecure_sources is on, so this entry is fetchable", err)
	}

	allowing.requireRemoteEchoMounted(ctx, "with plaintext allowed", "1.4.0")
	src.requireHits(t, "with plaintext allowed", 1)
	requireCachedPackage(t, cache, digest, "with plaintext allowed")
}

// TestE2EAnAllowedPlaintextSourceIsStillHeldToItsDigest is the switch's blast
// radius: it relaxes the URL SCHEME and nothing else.
//
// This is the half of the plaintext story an operator most needs to be able to
// rely on. Plaintext costs confidentiality and availability — the download can
// be watched and blocked — but not integrity, because the digest still checks
// every byte. With the switch on and the digest wrong, the entry fails and the
// cache stays empty, exactly as it does over https.
//
// The paired hit count is what keeps it honest: the source served the whole
// archive over plaintext, and none of it was kept.
//
// Bound: exactly one Apply. No loop, no wait.
func TestE2EAnAllowedPlaintextSourceIsStillHeldToItsDigest(t *testing.T) {
	requireGateableIsClean(t)
	ctx := context.Background()

	priv, keyring := newTestKey(t)
	_, archive := signedEchoArchive(t, priv, "1.4.0")
	cache := newTestCache(t)
	src := newE2ESource(t, httptest.NewServer, archive)
	h := newHarnessWithRemote(t, keyring, remoteFor(t, src.srv, cache, true))
	wrongDigest := digestOf([]byte("not the archive this plaintext source serves"))
	h.writeManifest(e2eRemoteEchoManifest(src.url(), wrongDigest))

	err := h.applyManifest(ctx)

	requireRemoteEchoFailed(ctx, t, h, err, "with plaintext allowed and the digest wrong", "digest mismatch")
	src.requireHits(t, "with plaintext allowed and the digest wrong", 1)
	requireNoFilesUnder(t, cache.Root(), "with plaintext allowed and the digest wrong")
}
