package legionplugin_test

// This file runs the SDK's example guest against the REAL host: the same
// wazero runtime, the same capability gate and the same ABI a mounted plugin
// runs under. Unit tests on the host platform (tool_test.go) cover the SDK's
// decoding and encoding; they cannot say anything about whether the exports
// are there, whether registration survives instantiation, or whether Go's
// garbage collector eats a buffer the host is about to write into.
//
// It is a black-box test package (legionplugin_test) on purpose: it may only
// use what a plugin author can use.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/plugin/host"
	"github.com/stardust/legion-agent/internal/plugin/perm"
)

// guestMemoryPages is the linear-memory ceiling these tests instantiate under.
// A Go guest carries a runtime and a GC, so it needs far more than the 64
// pages (4 MiB) a Rust guest is comfortable in: 512 pages is 32 MiB.
const guestMemoryPages = 512

// buildGuest compiles testdata/hello for wasip1 and returns the module bytes.
//
// It BUILDS rather than reads a committed artifact. A Go guest is ~3 MB, and a
// committed one would go stale at exactly the moment this test needs to be
// honest — the moment the SDK changes. Building also proves the SDK itself
// still compiles for the target, which no committed artifact can.
func buildGuest(t *testing.T) []byte {
	t.Helper()

	out := filepath.Join(t.TempDir(), "hello.wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, "./testdata/hello")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the wasip1 guest: %v\n%s", err, output)
	}
	wasm, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the built guest: %v", err)
	}
	return wasm
}

// newGuestInstance compiles and instantiates the example guest under grant,
// returning it plus the buffer the host logger writes to.
func newGuestInstance(t *testing.T, ctx context.Context, grant perm.Grant) (*host.Instance, *strings.Builder) {
	t.Helper()

	rt := host.NewRuntime(ctx, guestMemoryPages)
	t.Cleanup(func() {
		if err := rt.Close(ctx); err != nil {
			t.Errorf("close runtime: %v", err)
		}
	})

	compiled, err := host.Compile(ctx, rt, buildGuest(t))
	if err != nil {
		t.Fatalf("compile the guest: %v", err)
	}
	if err := host.CheckImports(compiled, grant); err != nil {
		t.Fatalf("check imports under %+v: %v", grant, err)
	}

	var logged strings.Builder
	deps := host.Deps{
		PluginName: "legion-hello-go",
		Logger:     slog.New(slog.NewTextHandler(&logged, nil)),
	}
	if _, err := host.BuildHostModule(ctx, rt, grant, deps); err != nil {
		t.Fatalf("build host module: %v", err)
	}

	inst, err := host.NewInstance(ctx, rt, compiled)
	if err != nil {
		t.Fatalf("instantiate the guest: %v", err)
	}
	return inst, &logged
}

// TestGoGuestSelfDescribesFromItsRegistry pins the property the SDK exists to
// guarantee: op 0's `provides` comes from what Serve registered, so the guest's
// self-description and its dispatch table cannot drift apart. A mismatch is
// what makes activation's cross-check refuse to mount a package.
//
// It also proves registration in init() survives instantiation — the whole
// reason the SDK's API is "register in init, leave main empty".
func TestGoGuestSelfDescribesFromItsRegistry(t *testing.T) {
	ctx := context.Background()
	inst, _ := newGuestInstance(t, ctx, perm.Grant{Log: true})

	out, err := inst.Invoke(ctx, abi.OpManifest, nil)
	if err != nil {
		t.Fatalf("invoke op manifest: %v", err)
	}
	var self struct {
		Name     string   `json:"name"`
		Version  string   `json:"version"`
		Provides []string `json:"provides"`
	}
	if err := json.Unmarshal(out, &self); err != nil {
		t.Fatalf("decode self-description %q: %v", out, err)
	}
	if self.Name != "legion-hello-go" || self.Version != "0.1.0" {
		t.Errorf("self-description = %s %s, want legion-hello-go 0.1.0", self.Name, self.Version)
	}
	if len(self.Provides) != 2 || self.Provides[0] != "hello_echo" || self.Provides[1] != "live_buffers" {
		t.Errorf("provides = %v, want [hello_echo live_buffers] (sorted, from the registry)", self.Provides)
	}
}

// TestGoGuestToolCallReturnsAResultAndLogsThroughTheHost is the closed loop:
// the host hands the guest a call, the guest calls BACK through the granted
// log capability, and answers with a domain.ToolResult.
func TestGoGuestToolCallReturnsAResultAndLogsThroughTheHost(t *testing.T) {
	ctx := context.Background()
	inst, logged := newGuestInstance(t, ctx, perm.Grant{Log: true})

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
		t.Fatalf("tool result success = false, error = %q, want a greeting", result.Error)
	}
	if result.Output != "hello, legion!" {
		t.Errorf("tool result output = %q, want %q", result.Output, "hello, legion!")
	}
	// Without this the call could have succeeded on pure computation: the log
	// line is what proves the capability actually reached the guest.
	if !strings.Contains(logged.String(), "hello_echo called with name=legion") {
		t.Errorf("host logger recorded %q, want the guest's log line", logged.String())
	}
}

// TestGoGuestReportsAMissingArgumentAsAFailedResult pins the rule a plugin
// author is most likely to get wrong: a tool that cannot do its job answers,
// it does not trap.
func TestGoGuestReportsAMissingArgumentAsAFailedResult(t *testing.T) {
	ctx := context.Background()
	inst, _ := newGuestInstance(t, ctx, perm.Grant{Log: true})

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

// TestGoGuestRefusesToLinkWithoutItsCapability pins that a Go guest obeys the
// same link-time rule as a Rust one: an ungranted capability is a missing
// import, so the module never links.
func TestGoGuestRefusesToLinkWithoutItsCapability(t *testing.T) {
	ctx := context.Background()
	rt := host.NewRuntime(ctx, guestMemoryPages)
	t.Cleanup(func() {
		if err := rt.Close(ctx); err != nil {
			t.Errorf("close runtime: %v", err)
		}
	})

	compiled, err := host.Compile(ctx, rt, buildGuest(t))
	if err != nil {
		t.Fatalf("compile the guest: %v", err)
	}
	if err := host.CheckImports(compiled, perm.Grant{}); err == nil {
		t.Fatal("CheckImports with no capabilities granted = nil, want a refusal: the guest imports legion.log")
	}
}

// TestGoGuestKeepsHostBuffersAlive is the test that exists because Go has a GC
// and Rust does not.
//
// After plugin_alloc returns, the only reference the host holds to that buffer
// is an INTEGER ADDRESS, which Go's collector cannot see. The SDK therefore
// keeps its own reference until plugin_free. If it did not, a collection
// between the allocation and the host's write would hand the host recycled
// memory — a corruption that shows up under load, in production, and nowhere
// else.
//
// The assertion is on the guard itself (LiveBuffers, reported by the example
// guest's live_buffers tool) rather than on "many calls still worked": a first
// attempt at this test drove 200 calls and passed even with the guard deleted,
// because the collector simply had not run. A test that cannot fail is worse
// than no test.
func TestGoGuestKeepsHostBuffersAlive(t *testing.T) {
	ctx := context.Background()
	inst, _ := newGuestInstance(t, ctx, perm.Grant{Log: true})

	// A non-empty request body forces the host to allocate through
	// plugin_alloc, so the guard must be holding at least that buffer while
	// the call runs.
	out, err := inst.Invoke(ctx, abi.OpCallTool,
		[]byte(`{"call_id":"c","tool":"live_buffers","arguments":{}}`))
	if err != nil {
		t.Fatalf("invoke op call_tool: %v", err)
	}
	var result domain.ToolResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("decode tool result %q: %v", out, err)
	}
	held, err := strconv.Atoi(result.Output)
	if err != nil {
		t.Fatalf("live_buffers answered %q, want a number: %v", result.Output, err)
	}
	if held < 1 {
		t.Errorf("the guest holds %d host-allocated buffers during a call with a request body, want >= 1: "+
			"nothing is keeping them alive against the collector", held)
	}
}

// TestGoGuestStaysCorrectAcrossManyCalls is the other half: the guard must not
// only register buffers but also release them, and the guest must keep
// answering correctly while its own collector runs.
func TestGoGuestStaysCorrectAcrossManyCalls(t *testing.T) {
	ctx := context.Background()
	inst, _ := newGuestInstance(t, ctx, perm.Grant{Log: true})

	for i := 0; i < 200; i++ {
		out, err := inst.Invoke(ctx, abi.OpCallTool,
			[]byte(`{"call_id":"c","tool":"hello_echo","arguments":{"name":"legion"}}`))
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		var result domain.ToolResult
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatalf("call %d: decode %q: %v", i, out, err)
		}
		if result.Output != "hello, legion!" {
			t.Fatalf("call %d: output = %q, want %q", i, result.Output, "hello, legion!")
		}
	}

	// Every buffer the 200 calls allocated must have been freed: a guard that
	// registered without releasing would be a leak that grows for as long as
	// the plugin is mounted.
	out, err := inst.Invoke(ctx, abi.OpCallTool,
		[]byte(`{"call_id":"c","tool":"live_buffers","arguments":{}}`))
	if err != nil {
		t.Fatalf("invoke live_buffers: %v", err)
	}
	var result domain.ToolResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	held, err := strconv.Atoi(result.Output)
	if err != nil {
		t.Fatalf("live_buffers answered %q, want a number: %v", result.Output, err)
	}
	// One is the request body of this very call; anything beyond that is what
	// the previous 200 calls failed to release.
	if held > 1 {
		t.Errorf("the guest still holds %d buffers after 200 completed calls, want 1 (this call's own "+
			"request body): the rest were never freed", held)
	}
}
