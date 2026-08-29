// Package plugin_example_test exercises the example plugin in plugin_example/
// against the REAL host: the same wazero runtime, the same capability gate and
// the same ABI a mounted plugin runs under.
//
// It exists so the example cannot rot silently. A committed package is a claim
// about three things at once — that plugin.json's digest matches plugin.wasm,
// that the guest answers both ABI ops, and that the capability gate really is
// link-time — and every one of them breaks quietly if nothing checks it.
//
// It deliberately does NOT go through the loader, the deployment manifest or
// the consent flow: those have their own tests, and an example test that
// assembled a whole service would break for reasons that have nothing to do
// with the example.
package plugin_example_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/plugin/host"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/plugin/perm"
)

// examplePages is the linear-memory ceiling this test instantiates under. It
// matches package/plugin.json's limits.max_memory_pages so the test runs the
// guest under the same ceiling a deployment would.
const examplePages = 64

func packageDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("package")
	if err != nil {
		t.Fatalf("resolve package dir: %v", err)
	}
	return dir
}

// readExampleManifest parses package/plugin.json the same way the host does.
func readExampleManifest(t *testing.T) manifest.PluginManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(packageDir(t), "plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	pm, err := manifest.ParsePlugin(data)
	if err != nil {
		t.Fatalf("parse plugin.json: %v", err)
	}
	return pm
}

func readExampleWasm(t *testing.T) []byte {
	t.Helper()
	wasm, err := os.ReadFile(filepath.Join(packageDir(t), "plugin.wasm"))
	if err != nil {
		t.Fatalf("read plugin.wasm (run plugin_example/scripts/build.sh): %v", err)
	}
	return wasm
}

// newExampleInstance compiles the example under grant and returns a live
// instance plus the buffer the host logger writes to, so a test can assert on
// what the guest logged.
func newExampleInstance(t *testing.T, ctx context.Context, grant perm.Grant) (*host.Instance, *bytes.Buffer) {
	t.Helper()
	rt := host.NewRuntime(ctx, examplePages)
	t.Cleanup(func() {
		if err := rt.Close(ctx); err != nil {
			t.Errorf("close runtime: %v", err)
		}
	})

	compiled, err := host.Compile(ctx, rt, readExampleWasm(t))
	if err != nil {
		t.Fatalf("compile example plugin: %v", err)
	}
	if err := host.CheckImports(compiled, grant); err != nil {
		t.Fatalf("check imports under %+v: %v", grant, err)
	}

	var logged bytes.Buffer
	deps := host.Deps{
		PluginName: "legion-hello",
		Logger:     slog.New(slog.NewTextHandler(&logged, nil)),
	}
	if _, err := host.BuildHostModule(ctx, rt, grant, deps); err != nil {
		t.Fatalf("build host module: %v", err)
	}

	inst, err := host.NewInstance(ctx, rt, compiled)
	if err != nil {
		t.Fatalf("instantiate example plugin: %v", err)
	}
	return inst, &logged
}

// TestExamplePackageDigestMatchesTheModule pins the one thing an author breaks
// by rebuilding the wasm and forgetting scripts/build.sh: plugin.json carries
// plugin.wasm's digest, and a mismatch is a load-time refusal, not a warning.
func TestExamplePackageDigestMatchesTheModule(t *testing.T) {
	pm := readExampleManifest(t)
	sum := sha256.Sum256(readExampleWasm(t))
	actual := hex.EncodeToString(sum[:])
	if !strings.EqualFold(pm.SHA256, actual) {
		t.Errorf("plugin.json declares sha256 %s, plugin.wasm hashes to %s; re-run plugin_example/scripts/build.sh",
			pm.SHA256, actual)
	}
}

// TestExampleGuestSelfDescriptionCoversItsDeclaredTools is the cross-check
// activation performs: the guest's op 0 answer must name every tool
// plugin.json says the plugin contributes, or the host refuses to mount it.
func TestExampleGuestSelfDescriptionCoversItsDeclaredTools(t *testing.T) {
	ctx := context.Background()
	inst, _ := newExampleInstance(t, ctx, perm.Grant{Log: true})

	out, err := inst.Invoke(ctx, abi.OpManifest, nil)
	if err != nil {
		t.Fatalf("invoke op manifest: %v", err)
	}
	var self struct {
		Name       string   `json:"name"`
		Version    string   `json:"version"`
		Provides   []string `json:"provides"`
		Extensions []string `json:"extensions"`
	}
	if err := json.Unmarshal(out, &self); err != nil {
		t.Fatalf("decode self-description %q: %v", out, err)
	}

	pm := readExampleManifest(t)
	if self.Name != pm.Name {
		t.Errorf("guest says name %q, plugin.json says %q", self.Name, pm.Name)
	}
	for _, declared := range pm.Tools {
		if !slices.Contains(self.Provides, declared.Name) {
			t.Errorf("plugin.json declares tool %q, guest provides %v; activation would refuse this package",
				declared.Name, self.Provides)
		}
	}
	// The same cross-check for extension points, and it runs in the other
	// direction too: a deployment may only GRANT an extension the guest says
	// it implements, so a plugin.json that declares one the binary does not
	// is a package whose grant activation will refuse.
	for _, declared := range pm.Extensions {
		if !slices.Contains(self.Extensions, declared) {
			t.Errorf("plugin.json declares extension %q, guest implements %v; a grant naming it "+
				"would be refused at activation", declared, self.Extensions)
		}
	}
}

// TestExampleToolCallReturnsAResultAndLogsThroughTheHost is the closed loop:
// the host hands the guest a tool call, the guest calls BACK into the host
// through the granted log capability, and answers with a domain.ToolResult.
func TestExampleToolCallReturnsAResultAndLogsThroughTheHost(t *testing.T) {
	ctx := context.Background()
	inst, logged := newExampleInstance(t, ctx, perm.Grant{Log: true})

	request := []byte(`{"call_id":"call-1","tool":"hello_echo","arguments":{"name":"legion"}}`)
	out, err := inst.Invoke(ctx, abi.OpCallTool, request)
	if err != nil {
		t.Fatalf("invoke op call_tool: %v", err)
	}

	var result domain.ToolResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("decode tool result %q: %v", out, err)
	}
	if !result.Success {
		t.Fatalf("tool result success = false, error = %q, want a successful greeting", result.Error)
	}
	if result.Output != "hello, legion!" {
		t.Errorf("tool result output = %q, want %q", result.Output, "hello, legion!")
	}
	// The log line is the proof the capability actually reached the guest:
	// without it the call could have succeeded on pure computation alone.
	if !strings.Contains(logged.String(), "hello_echo called with name=legion") {
		t.Errorf("host logger recorded %q, want the guest's log line", logged.String())
	}
}

// TestExampleToolCallReportsAMissingArgumentAsAFailedResult pins the rule a
// plugin author is most likely to get wrong: a tool that cannot do its job
// answers with success:false, it does not trap. A trap would take the whole
// module down with it.
func TestExampleToolCallReportsAMissingArgumentAsAFailedResult(t *testing.T) {
	ctx := context.Background()
	inst, _ := newExampleInstance(t, ctx, perm.Grant{Log: true})

	out, err := inst.Invoke(ctx, abi.OpCallTool, []byte(`{"call_id":"c","tool":"hello_echo","arguments":{}}`))
	if err != nil {
		t.Fatalf("invoke op call_tool: %v; a missing argument must be a RESULT, not a trap", err)
	}
	var result domain.ToolResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("decode tool result %q: %v", out, err)
	}
	if result.Success {
		t.Fatal("tool result success = true for a call with no name argument, want a failed result")
	}
	if !strings.Contains(result.Error, "name") {
		t.Errorf("tool result error = %q, want it to name the missing argument", result.Error)
	}
}

// TestExampleRefusesToLinkWithoutItsCapability is why plugin.json's
// capabilities list is not documentation: an ungranted capability is a MISSING
// IMPORT, so the module never links — the guest gets no chance to call the
// function and no DENIED to handle.
func TestExampleRefusesToLinkWithoutItsCapability(t *testing.T) {
	ctx := context.Background()
	rt := host.NewRuntime(ctx, examplePages)
	t.Cleanup(func() {
		if err := rt.Close(ctx); err != nil {
			t.Errorf("close runtime: %v", err)
		}
	})

	compiled, err := host.Compile(ctx, rt, readExampleWasm(t))
	if err != nil {
		t.Fatalf("compile example plugin: %v", err)
	}
	if err := host.CheckImports(compiled, perm.Grant{}); err == nil {
		t.Fatal("CheckImports with no capabilities granted = nil, want a refusal: the guest imports legion.log")
	}
}

// TestExampleObserverIsNotifiedThroughTheHost drives abi.OpObserveToolResult
// exactly as the host does for a plugin granted the observe extension: the
// guest's observer runs, calls BACK through the log capability, and the
// notification's answer is a well-formed body the host discards.
//
// The log line is the proof. The seam returns nothing by construction, so
// "did the observer run?" can only be answered by an effect it had somewhere
// else — which is also the shape a real observer plugin has to live with.
func TestExampleObserverIsNotifiedThroughTheHost(t *testing.T) {
	ctx := context.Background()
	inst, logged := newExampleInstance(t, ctx, perm.Grant{Log: true})

	observation := []byte(`{"call_id":"c1","tool":"write_file","arguments":{"path":"/tmp/x"},` +
		`"success":true,"output":"wrote 3 bytes","error":""}`)
	if _, err := inst.Invoke(ctx, abi.OpObserveToolResult, observation); err != nil {
		t.Fatalf("invoke op observe: %v", err)
	}

	if !strings.Contains(logged.String(), "observed tool=write_file success=true") {
		t.Errorf("host log = %q, want the observer's line naming the tool it was told about", logged.String())
	}
}

// TestExampleObserverDoesNotTrapOnAnUnreadableObservation: the seam is
// one-way, so a body the guest cannot parse has nobody to report to — and
// trapping would take down the tool call that is merely waiting for its
// observers to be told.
func TestExampleObserverDoesNotTrapOnAnUnreadableObservation(t *testing.T) {
	ctx := context.Background()
	inst, _ := newExampleInstance(t, ctx, perm.Grant{Log: true})

	if _, err := inst.Invoke(ctx, abi.OpObserveToolResult, []byte(`not json at all`)); err != nil {
		t.Fatalf("invoke op observe with an unreadable body: %v, want an answer rather than a trap", err)
	}
	// The instance is still usable: a trap would have poisoned it.
	if _, err := inst.Invoke(ctx, abi.OpManifest, nil); err != nil {
		t.Fatalf("invoke op manifest after a bad observation: %v", err)
	}
}

// TestExampleDeciderAnswersBothWays drives abi.OpDecideToolCall as the host
// does for a plugin granted the decide extension. Both branches are part of
// the contract: the host fails closed, so a decider that could only ever deny
// would be indistinguishable from a broken one.
func TestExampleDeciderAnswersBothWays(t *testing.T) {
	ctx := context.Background()
	inst, _ := newExampleInstance(t, ctx, perm.Grant{Log: true})

	for _, tc := range []struct {
		name         string
		request      string
		wantDecision string
	}{
		{name: "allows an ordinary tool", request: `{"call_id":"c1","tool":"read_file"}`, wantDecision: "allow"},
		{name: "refuses the one it guards", request: `{"call_id":"c2","tool":"forbidden_tool"}`, wantDecision: "deny"},
		{name: "asks for the one under review", request: `{"call_id":"c3","tool":"reviewed_tool"}`, wantDecision: "ask"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := inst.Invoke(ctx, abi.OpDecideToolCall, []byte(tc.request))
			if err != nil {
				t.Fatalf("invoke op decide: %v", err)
			}
			var answer struct {
				Decision string `json:"decision"`
				Reason   string `json:"reason"`
			}
			if err := json.Unmarshal(out, &answer); err != nil {
				t.Fatalf("decode decision %q: %v", out, err)
			}
			if answer.Decision != tc.wantDecision {
				t.Errorf("decision = %q, want %q", answer.Decision, tc.wantDecision)
			}
			if tc.wantDecision != "allow" && answer.Reason == "" {
				t.Error("a deny with no reason leaves the operator nothing to act on")
			}
		})
	}
}

// TestExampleDeciderRefusesARequestItCannotRead: an unreadable QUESTION must
// not become an allow. The guest answers deny and says why — the host denies
// an unreadable ANSWER anyway, so this only makes the reason legible.
func TestExampleDeciderRefusesARequestItCannotRead(t *testing.T) {
	ctx := context.Background()
	inst, _ := newExampleInstance(t, ctx, perm.Grant{Log: true})

	out, err := inst.Invoke(ctx, abi.OpDecideToolCall, []byte(`not json at all`))
	if err != nil {
		t.Fatalf("invoke op decide with an unreadable request: %v, want an answer rather than a trap", err)
	}
	if !strings.Contains(string(out), `"decision":"deny"`) {
		t.Errorf("answer = %s, want a deny", out)
	}
}

// TestExamplePromptSegmentIsReadableByTheHost drives abi.OpPromptSegment as
// activation does. The host REFUSES TO MOUNT a plugin whose segment it cannot
// decode, so "the answer is a well-formed document" is the difference between
// this package mounting and not.
func TestExamplePromptSegmentIsReadableByTheHost(t *testing.T) {
	ctx := context.Background()
	inst, _ := newExampleInstance(t, ctx, perm.Grant{Log: true})

	out, err := inst.Invoke(ctx, abi.OpPromptSegment, nil)
	if err != nil {
		t.Fatalf("invoke op prompt segment: %v", err)
	}
	var answer struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		t.Fatalf("decode prompt segment %q: %v", out, err)
	}
	if !strings.Contains(answer.Text, "use the exact name the caller gave") {
		t.Errorf("text = %q, want the segment this plugin declares", answer.Text)
	}
	if got := len([]rune(answer.Text)); got > 2048 {
		t.Errorf("segment is %d runes; the host truncates past 2048, and this one is paid for on every inference", got)
	}
}
