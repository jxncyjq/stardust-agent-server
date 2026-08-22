package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	agentruntime "github.com/stardust/legion-agent/internal/runtime"
	"github.com/stardust/legion-agent/internal/toolauth"
)

// The two guests these tests mount, as they describe THEMSELVES through the
// plugin ABI (see internal/plugin/host/testdata/README.md). host.Activate
// cross-checks a Spec against that self-description, so a plugin.json written
// here must claim exactly these names.
//
// Two DIFFERENT guests are needed wherever a test mounts two plugins at once:
// the tool registry and the process-global gateable catalog both refuse a
// duplicate tool name, so two entries can never contribute the same tool.
const (
	testEchoPlugin = "legion-test-plugin"
	testEchoTool   = "echo_tool"
	testEchoWasm   = "plugin.wasm"

	testProxyPlugin = "legion-e2e-plugin"
	testProxyTool   = "e2e_proxy_tool"
	testProxyWasm   = "e2e.wasm"
)

// testGhostTool is a tool a plugin.json can DECLARE but the guest does not
// provide. It is how these tests make one activation fail on its own merits:
// the package loads and the spec assembles, and host.Activate then refuses at
// the manifest cross-check. No test seam in production code is involved.
const testGhostTool = "ghost_tool"

// pluginFixture is one test's whole plugin world: a deployment manifest and
// package tree on disk, an agent.json pointing at them, and an App carrying the
// loader that serve assembly would have built from exactly that config.
type pluginFixture struct {
	t            *testing.T
	dir          string
	root         string
	manifestPath string
	configPath   string
	application  *app.App
	gate         *agentruntime.TaskGate
}

// newPluginFixture writes a config with a plugins section pointing at a
// manifest inside a fresh temp dir. It does NOT assemble the loader — a test
// calls assemble once its packages and manifest are on disk.
//
// The cleanup disposes every owner left in the App's ledger. It is not
// optional: toolauth's gateable catalog is PROCESS-GLOBAL, so a test that left
// a plugin mounted would make the next test's contribution of the same tool
// name panic. It goes through the ledger rather than the Loader so it holds
// even for a test that left the Loader unable to unwind itself.
func newPluginFixture(t *testing.T, applyWaitMs int) *pluginFixture {
	t.Helper()

	dir := t.TempDir()
	f := &pluginFixture{
		t:            t,
		dir:          dir,
		root:         filepath.Join(dir, "plugins"),
		manifestPath: filepath.Join(dir, "plugins.json"),
		configPath:   filepath.Join(dir, "agent.json"),
		application:  app.New(),
		gate:         agentruntime.NewTaskGate(),
	}
	f.writeConfig(fmt.Sprintf(`{
		"storage": {"driver": "memory"},
		"context_files": {"root": %s},
		"plugins": {
			"manifest": %s,
			"root": %s,
			"limits": {"timeout_ms": 5000, "max_memory_pages": 64, "max_instances": 1},
			"apply_wait_ms": %d
		}
	}`, jsonString(dir), jsonString(f.manifestPath), jsonString(f.root), applyWaitMs))

	t.Cleanup(func() {
		ledger := f.application.PluginLedger()
		for owner := range ledger.Snapshot() {
			if err := ledger.DisposeOwner(owner); err != nil {
				t.Errorf("cleanup: dispose owner %s: %v", owner, err)
			}
		}
	})
	return f
}

// jsonString quotes s as a JSON string literal, so a Windows temp path's
// backslashes reach the decoder escaped rather than as broken escapes.
func jsonString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("encode %q as a JSON string: %v", s, err))
	}
	return string(encoded)
}

func (f *pluginFixture) writeConfig(body string) {
	f.t.Helper()

	if err := os.WriteFile(f.configPath, []byte(body), 0o600); err != nil {
		f.t.Fatalf("write config %s: %v", f.configPath, err)
	}
}

// writePackage writes a plugin package (plugin.json + plugin.wasm) under the
// deployment root, with plugin.json's sha256 computed from the bytes actually
// written so the loader's digest check passes.
func (f *pluginFixture) writePackage(source, wasmFile, name, version string, capabilities, tools []string) {
	f.t.Helper()

	dir := filepath.Join(f.root, source)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatalf("create package dir %s: %v", dir, err)
	}
	wasm := readWasmFixture(f.t, wasmFile)
	sum := sha256.Sum256(wasm)
	decls := make([]manifest.ToolDecl, 0, len(tools))
	for _, toolName := range tools {
		decls = append(decls, manifest.ToolDecl{
			Name:        toolName,
			Description: "fixture tool " + toolName,
			Group:       "plugins",
			RiskLevel:   "low",
			TimeoutMs:   1000,
		})
	}
	data, err := json.Marshal(manifest.PluginManifest{
		Name:         name,
		Version:      version,
		ABI:          1,
		SHA256:       hex.EncodeToString(sum[:]),
		Capabilities: capabilities,
		Limits:       manifest.Limits{TimeoutMs: 5000, MaxMemoryPages: 64, MaxInstances: 1},
		Tools:        decls,
	})
	if err != nil {
		f.t.Fatalf("encode plugin.json for %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), data, 0o644); err != nil {
		f.t.Fatalf("write plugin.json for %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), wasm, 0o644); err != nil {
		f.t.Fatalf("write plugin.wasm for %s: %v", name, err)
	}
}

// readWasmFixture reads one of the host package's committed guest binaries by
// relative path, the same way internal/plugin/loader's tests do.
func readWasmFixture(t *testing.T, file string) []byte {
	t.Helper()

	path := filepath.Join("..", "plugin", "host", "testdata", file)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wasm fixture %s: %v", path, err)
	}
	return data
}

// manifestEntry is one entry as a test wants it written into plugins.json.
type manifestEntry struct {
	name         string
	source       string
	enabled      bool
	capabilities []string
	tools        []string
}

// writeManifest writes plugins.json with the given entries.
func (f *pluginFixture) writeManifest(entries ...manifestEntry) {
	f.t.Helper()

	type rawEntry struct {
		Name    string                `json:"name"`
		Source  string                `json:"source"`
		Enabled bool                  `json:"enabled"`
		Grant   manifest.GrantDecl    `json:"grant"`
		Tools   []manifest.ToolAccept `json:"tools"`
	}
	raw := struct {
		Plugins []rawEntry `json:"plugins"`
	}{}
	for _, e := range entries {
		accepts := make([]manifest.ToolAccept, 0, len(e.tools))
		for _, toolName := range e.tools {
			accepts = append(accepts, manifest.ToolAccept{Name: toolName})
		}
		raw.Plugins = append(raw.Plugins, rawEntry{
			Name:    e.name,
			Source:  e.source,
			Enabled: e.enabled,
			Grant:   manifest.GrantDecl{Capabilities: e.capabilities},
			Tools:   accepts,
		})
	}
	data, err := json.Marshal(raw)
	if err != nil {
		f.t.Fatalf("encode plugins.json: %v", err)
	}
	if err := os.WriteFile(f.manifestPath, data, 0o644); err != nil {
		f.t.Fatalf("write plugins.json: %v", err)
	}
}

// assemble runs the same plugin assembly serve does — build the loader from the
// config, attach it to the App, converge once — and returns its error.
func (f *pluginFixture) assemble() error {
	f.t.Helper()

	return f.assembleWithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// assembleWithLogger is assemble with a caller-chosen logger, for the test that
// pins what a failed startup convergence actually writes to the log.
func (f *pluginFixture) assembleWithLogger(logger *slog.Logger) error {
	f.t.Helper()

	cfg, err := config.Load(context.Background(), config.Options{Path: f.configPath})
	if err != nil {
		f.t.Fatalf("load config %s: %v", f.configPath, err)
	}
	return assemblePlugins(context.Background(), f.application, cfg, pluginHostDeps{
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
		Logger: logger,
		Gate:   f.gate,
	})
}

// run executes one `agent plugins ...` invocation against the fixture's App and
// returns its output and error. A non-nil error is what the binary turns into a
// non-zero exit code.
func (f *pluginFixture) run(args ...string) (string, error) {
	f.t.Helper()

	out := &bytes.Buffer{}
	full := append([]string{"plugins"}, args...)
	full = append(full, "--config", f.configPath)
	err := Execute(f.application, out, full)
	return out.String(), err
}

// TestPluginsStatusWithoutAConfiguredManifest pins the documented "plugins are
// off" state: a config with no plugins.manifest is a supported deployment, so
// status reports it readably and succeeds (exit code 0) rather than erroring.
func TestPluginsStatusWithoutAConfiguredManifest(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(configPath, []byte(`{"storage": {"driver": "memory"}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	out := &bytes.Buffer{}
	if err := Execute(app.New(), out, []string{"plugins", "status", "--config", configPath}); err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "plugins.manifest") {
		t.Fatalf("plugins status output = %q, want it to name the missing plugins.manifest setting", out.String())
	}
}

// TestPluginsStatusWithAnEmptyManifest covers the other empty state: plugins
// ARE configured, the manifest simply declares none.
func TestPluginsStatusWithAnEmptyManifest(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writeManifest()
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if !strings.Contains(out, f.manifestPath) {
		t.Errorf("plugins status output = %q, want it to name the manifest %s", out, f.manifestPath)
	}
	if !strings.Contains(out, "no plugins") {
		t.Errorf("plugins status output = %q, want a readable empty state", out)
	}
}

// TestPluginsStatusReportsLoadedPlugins is the ordinary case: two plugins are
// mounted, and status names each one, its version, its state and the tools it
// contributed.
func TestPluginsStatusReportsLoadedPlugins(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writePackage("proxy", testProxyWasm, testProxyPlugin, "3.4.0", []string{"tool"}, []string{testProxyTool})
	f.writeManifest(
		manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}},
		manifestEntry{name: testProxyPlugin, source: "proxy", enabled: true, capabilities: []string{"tool"}, tools: []string{testProxyTool}},
	)
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	for _, want := range []string{
		testEchoPlugin, "1.2.0", testEchoTool,
		testProxyPlugin, "3.4.0", testProxyTool,
		"loaded",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plugins status output = %q, want it to contain %q", out, want)
		}
	}
}

// TestPluginsAssemblyWiresEveryGrantGatedDependency mounts a plugin granted
// log, config, http and fs. host.BuildHostModule refuses to build a module for
// a granted capability whose dependency is missing or zero-valued — a nil
// *http.Client, a zero WorkspacePathGuard, an empty Config body — so this
// entry activating AT ALL is the proof that serve assembly hands the plugin
// host real ones. It is also what pins the "an entry with no config block gets
// an explicit {}" contract: the manifest entry below carries no config.
func TestPluginsAssemblyWiresEveryGrantGatedDependency(t *testing.T) {
	granted := []string{"log", "config", "http", "fs"}
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", granted, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: true,
		capabilities: granted, tools: []string{testEchoTool},
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if !strings.Contains(out, "loaded") || !strings.Contains(out, testEchoTool) {
		t.Fatalf("plugins status output = %q, want the plugin loaded with its tool; "+
			"a missing Deps field for any granted capability would have failed the activation", out)
	}
}

// TestPluginsAssemblyRefusesAKVGrantItCannotBack is the honest-gap guard: this
// deployment wires no key-value store, so an entry granted "kv" must fail
// loudly naming the missing dependency rather than be handed a throwaway map
// whose contents vanish without anyone being told.
func TestPluginsAssemblyRefusesAKVGrantItCannotBack(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", []string{"kv"}, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: true,
		capabilities: []string{"kv"}, tools: []string{testEchoTool},
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil (a failed entry must not stop startup)", err)
	}

	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if !strings.Contains(out, "failed") {
		t.Fatalf("plugins status output = %q, want the entry reported as failed", out)
	}
	if !strings.Contains(out, "Deps.KV") {
		t.Errorf("plugins status output = %q, want the failure to name the missing Deps.KV", out)
	}
}

// TestPluginsStatusKeepsAFailedEntryVisibleWithItsReason is the guard the
// mandated mutation targets: one entry that cannot activate must not vanish
// from the diagnosis — its state is failed, its reason is printed, and the
// healthy entry alongside it is unaffected.
func TestPluginsStatusKeepsAFailedEntryVisibleWithItsReason(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	// The proxy guest does not provide testGhostTool, so its activation fails
	// at host.Activate's manifest cross-check.
	f.writePackage("proxy", testProxyWasm, testProxyPlugin, "3.4.0", []string{"tool"}, []string{testProxyTool, testGhostTool})
	f.writeManifest(
		manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}},
		manifestEntry{name: testProxyPlugin, source: "proxy", enabled: true, capabilities: []string{"tool"}, tools: []string{testProxyTool, testGhostTool}},
	)
	// Startup assembly must NOT fail: one bad plugin does not ground the agent.
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil (a failed entry must not stop startup)", err)
	}

	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if !strings.Contains(out, "failed") {
		t.Fatalf("plugins status output = %q, want the failed entry's state", out)
	}
	if !strings.Contains(out, testGhostTool) {
		t.Errorf("plugins status output = %q, want the failure reason naming %q", out, testGhostTool)
	}
	if !strings.Contains(out, testEchoTool) {
		t.Errorf("plugins status output = %q, want the healthy entry still reporting %q", out, testEchoTool)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = false, want true: the healthy entry must still be mounted", testEchoTool)
	}
}

// TestAssemblePluginsLogsAFailedEntryAtErrorLevel is the other half of "loud":
// startup continues, but the failure must reach the log at Error level naming
// the entry. Without this the only trace of a plugin that never came up would
// be a status command nobody ran.
func TestAssemblePluginsLogsAFailedEntryAtErrorLevel(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("proxy", testProxyWasm, testProxyPlugin, "3.4.0", []string{"tool"}, []string{testProxyTool, testGhostTool})
	f.writeManifest(manifestEntry{
		name: testProxyPlugin, source: "proxy", enabled: true,
		capabilities: []string{"tool"}, tools: []string{testProxyTool, testGhostTool},
	})

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := f.assembleWithLogger(logger); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil (a failed entry must not stop startup)", err)
	}
	written := logs.String()
	// The message is asserted, not merely "some Error record": the loader logs
	// an Error of its OWN for the entry that failed, so a test happy with any
	// ERROR line would pass with the assembly's report deleted.
	if !strings.Contains(written, `msg="converge plugin deployment at startup"`) {
		t.Fatalf("startup log = %q, want the assembly's own Error record for the failed convergence", written)
	}
	if !strings.Contains(written, "level=ERROR") {
		t.Errorf("startup log = %q, want the record at Error level", written)
	}
	if !strings.Contains(written, testProxyPlugin) {
		t.Errorf("startup log = %q, want it to name the failed plugin %q", written, testProxyPlugin)
	}
}

// TestPluginsStatusDistinguishesDisabledFromNotConverged pins §8's requirement
// that the three "why isn't my plugin working" cases are told apart: an entry
// the operator disabled reads differently from one that is enabled but has not
// been converged yet.
func TestPluginsStatusDistinguishesDisabledFromNotConverged(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if !strings.Contains(out, "disabled") {
		t.Fatalf("plugins status output = %q, want the disabled entry reported as disabled", out)
	}

	// Now enable it on disk WITHOUT reloading: the entry is in the target state
	// the operator wrote, and nothing has converged it.
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})
	out, err = f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if !strings.Contains(out, "pending") {
		t.Fatalf("plugins status output = %q, want the unconverged entry reported as pending", out)
	}
	if strings.Contains(out, "disabled") {
		t.Errorf("plugins status output = %q, want the entry no longer reported as disabled", out)
	}
}

// TestPluginsReloadRereadsTheManifestAndConverges is the reload contract: the
// command re-reads plugins.json from disk (it does not replay what startup
// read) and applies it, so disabling an entry really unmounts its tool.
func TestPluginsReloadRereadsTheManifestAndConverges(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Fatalf("IsGateable(%q) = false after startup apply, want true", testEchoTool)
	}

	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}})
	out, err := f.run("reload")
	if err != nil {
		t.Fatalf("plugins reload error = %v, want nil", err)
	}
	if toolauth.IsGateable(testEchoTool) {
		t.Fatalf("IsGateable(%q) = true after reload, want false: the disabled entry must be unmounted", testEchoTool)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("plugins reload output = %q, want it to report the entry as disabled", out)
	}
}

// TestPluginsReloadFailsWhenNoTaskBoundaryIsReached is the error path: with a
// task in flight and an apply wait that expires, reload must report a non-nil
// error (a non-zero exit code) whose message says the task boundary was never
// reached — not succeed silently having applied nothing.
func TestPluginsReloadFailsWhenNoTaskBoundaryIsReached(t *testing.T) {
	f := newPluginFixture(t, 50)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	// One task in flight for the whole of the reload, retired before the test
	// ends. The gate's wait is 50ms, so the reload gives up quickly and this
	// never becomes an unbounded wait.
	end, err := f.gate.Begin()
	if err != nil {
		t.Fatalf("gate.Begin() error = %v, want nil", err)
	}
	defer end()

	_, err = f.run("reload")
	if err == nil {
		t.Fatal("plugins reload error = nil, want an error: no task boundary was reached")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("plugins reload error = %v, want it to say tasks were still running", err)
	}
	if !strings.Contains(err.Error(), "nothing was applied") {
		t.Errorf("plugins reload error = %v, want it to say nothing was applied", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = false, want true: a refused reload must change nothing", testEchoTool)
	}
}

// TestDrainPluginsUnmountsEverythingAndReleasesTheLoaderSlot covers the drain
// function itself: everything the loader mounted goes away, AND the App's
// loader slot is released so the same process can assemble again. The second
// half is not tidiness either -- App.SetPlugins refuses a second attachment, so
// a drain that did not detach would make an in-process serve restart fail at
// assembly even though nothing is mounted any more.
//
// Whether serve's own shutdown path actually calls this is a separate question,
// pinned through the real ServeResult.Close by
// TestServeResultCloseDrainsPluginsAndAllowsAnInProcessRestart.
func TestDrainPluginsUnmountsEverythingAndReleasesTheLoaderSlot(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Fatalf("IsGateable(%q) = false after startup apply, want true", testEchoTool)
	}

	drainPlugins(f.application, f.root, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = true after the shutdown drain, want false", testEchoTool)
	}
	if left := f.application.PluginResources(); len(left) != 0 {
		t.Errorf("PluginResources() = %v after the shutdown drain, want empty", left)
	}
	if got := f.application.Plugins(); got != nil {
		t.Errorf("App.Plugins() = %v after the drain, want nil: the slot must be released", got)
	}
	// The slot really is reusable: a second assembly in the same process mounts
	// the deployment again instead of being refused by SetPlugins.
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() after a drain: error = %v, want nil (a drained slot must be reusable)", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = false after the second assembly, want true", testEchoTool)
	}
}

// TestServeResultCloseDrainsPluginsAndAllowsAnInProcessRestart is the wiring
// guard the drain function's own test cannot give: it goes through the REAL
// serve assembly and the ServeResult.Close a host actually calls, so dropping
// the one-line drainPlugins call from Close is caught here.
//
// The restart half is the reason the drain exists. An embedded host (the Wails
// GUI) stops and restarts serve inside one process; a plugin's tool name lives
// in the process-global toolauth catalog, so a Close that left the first run's
// plugins mounted would make the second assembly panic on the duplicate name.
func TestServeResultCloseDrainsPluginsAndAllowsAnInProcessRestart(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := BuildServeService(ctx, ServeOptions{
		ConfigPath: f.configPath,
		Addr:       "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		App:        f.application,
	})
	if err != nil {
		t.Fatalf("BuildServeService() error = %v, want nil", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Fatalf("IsGateable(%q) = false after serve assembly, want true", testEchoTool)
	}

	result.Close()

	if toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = true after ServeResult.Close, want false: Close must drain the plugins", testEchoTool)
	}
	if left := f.application.PluginResources(); len(left) != 0 {
		t.Errorf("PluginResources() = %v after ServeResult.Close, want empty", left)
	}

	restarted, err := BuildServeService(ctx, ServeOptions{
		ConfigPath: f.configPath,
		Addr:       "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		App:        f.application,
	})
	if err != nil {
		t.Fatalf("BuildServeService() after Close: error = %v, want nil (a drained App must accept the next serve)", err)
	}
	defer restarted.Close()
	if !toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = false after the in-process restart, want true", testEchoTool)
	}
}

// TestBuildServeServiceUnmountsPluginsWhenAssemblyFailsAfterTheMount is the
// leak guard: the plugins are mounted early in serve assembly, and a dozen
// failure returns come after that point. The most ordinary one there is -- the
// address is already taken -- must leave the process as it found it: nothing in
// the process-global gateable catalog, no live ledger entry, and the App's
// loader slot free for the retry.
//
// Without that, an embedded host retrying after "address already in use" hits
// toolauth.Contribute's duplicate-name panic on the second attempt.
func TestBuildServeServiceUnmountsPluginsWhenAssemblyFailsAfterTheMount(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

	// Hold the address serve is about to be pointed at, so its net.Listen --
	// well past the plugin mount -- fails.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("take a port for the test: %v", err)
	}
	defer func() {
		if cerr := taken.Close(); cerr != nil {
			t.Errorf("close the held listener: %v", cerr)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := BuildServeService(ctx, ServeOptions{
		ConfigPath: f.configPath,
		Addr:       taken.Addr().String(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		App:        f.application,
	})
	if err == nil {
		result.Close()
		t.Fatal("BuildServeService() error = nil, want an error: the address is already in use")
	}
	// Pinned so the test cannot silently start failing BEFORE the plugin mount,
	// which would make every assertion below vacuous.
	if !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("BuildServeService() error = %v, want the listen failure", err)
	}

	if toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = true after a failed assembly, want false: the mounted plugins must be drained", testEchoTool)
	}
	if left := f.application.PluginResources(); len(left) != 0 {
		t.Errorf("PluginResources() = %v after a failed assembly, want empty", left)
	}
	if got := f.application.Plugins(); got != nil {
		t.Errorf("App.Plugins() = %v after a failed assembly, want nil: the retry needs the slot", got)
	}
}

// TestPluginsReloadWithoutALoaderFails pins the honest answer in a process that
// never assembled a loader: reload cannot pretend to have reloaded anything.
func TestPluginsReloadWithoutALoaderFails(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writeManifest()
	// Deliberately no assemble(): the App has no loader.
	if _, err := f.run("reload"); err == nil {
		t.Fatal("plugins reload error = nil, want an error: there is no loader to reload")
	}
}

// TestAssemblePluginsFailsWhenTheManifestIsMissing is the "configured means
// meant it" rule: a plugins.manifest path that cannot be read fails serve
// assembly instead of quietly running with no plugins.
func TestAssemblePluginsFailsWhenTheManifestIsMissing(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	// No writeManifest: the configured path does not exist.
	err := f.assemble()
	if err == nil {
		t.Fatal("assemblePlugins() error = nil, want an error: the configured manifest does not exist")
	}
	if !strings.Contains(err.Error(), f.manifestPath) {
		t.Errorf("assemblePlugins() error = %v, want it to name the manifest path %s", err, f.manifestPath)
	}
}

// TestBuildServeServiceFailsWhenTheManifestIsMissing proves the same rule
// through the real serve assembly, so a future refactor that drops the plugin
// assembly call from BuildServeService is caught.
func TestBuildServeServiceFailsWhenTheManifestIsMissing(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	missing := filepath.Join(dir, "does-not-exist.json")
	body := fmt.Sprintf(`{
		"storage": {"driver": "memory"},
		"plugins": {"manifest": %s, "root": %s, "limits": {"timeout_ms": 5000}, "apply_wait_ms": 1000}
	}`, jsonString(missing), jsonString(filepath.Join(dir, "plugins")))
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := BuildServeService(ctx, ServeOptions{
		ConfigPath: configPath,
		Addr:       "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err == nil {
		result.Close()
		t.Fatal("BuildServeService() error = nil, want an error: the configured plugin manifest does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("BuildServeService() error = %v, want it to name the missing manifest %s", err, missing)
	}
}

// TestConfigRejectsAnIncompletePluginSection covers the config section's own
// fail-loud branches: naming a manifest makes the other fields load-bearing,
// and each missing one is reported by name.
func TestConfigRejectsAnIncompletePluginSection(t *testing.T) {
	cases := []struct {
		name    string
		plugins string
		want    string
	}{
		{
			name:    "no root",
			plugins: `{"manifest": "p.json", "root": "", "apply_wait_ms": 1000, "limits": {"timeout_ms": 1000}}`,
			want:    "plugins.root",
		},
		{
			name:    "no apply wait",
			plugins: `{"manifest": "p.json", "root": "plugins", "apply_wait_ms": 0, "limits": {"timeout_ms": 1000}}`,
			want:    "plugins.apply_wait_ms",
		},
		{
			name:    "no call timeout",
			plugins: `{"manifest": "p.json", "root": "plugins", "apply_wait_ms": 1000, "limits": {"timeout_ms": 0}}`,
			want:    "plugins.limits.timeout_ms",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "agent.json")
			body := fmt.Sprintf(`{"storage": {"driver": "memory"}, "plugins": %s}`, tc.plugins)
			if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			_, err := config.Load(context.Background(), config.Options{Path: configPath})
			if err == nil {
				t.Fatalf("config.Load() error = nil, want an error naming %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("config.Load() error = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// TestPluginsStatusStillReportsMountedPluginsWhenTheManifestIsUnreadable is the
// diagnostic's own contract. Failing serve assembly on an unreadable manifest
// is right; blinding `plugins status` is not -- an operator whose plugins.json
// was deleted or corrupted under a running serve is exactly the operator who
// needs to know what is still mounted. So the command reports the read failure,
// still prints the loader's view, and only then exits non-zero.
func TestPluginsStatusStillReportsMountedPluginsWhenTheManifestIsUnreadable(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	// The manifest goes away under the running deployment; the plugin stays
	// mounted.
	if err := os.Remove(f.manifestPath); err != nil {
		t.Fatalf("remove manifest %s: %v", f.manifestPath, err)
	}

	out, err := f.run("status")
	if err == nil {
		t.Fatal("plugins status error = nil, want an error: the configured manifest cannot be read")
	}
	if !strings.Contains(err.Error(), f.manifestPath) {
		t.Errorf("plugins status error = %v, want it to name the manifest %s", err, f.manifestPath)
	}
	if !strings.Contains(out, testEchoPlugin) {
		t.Fatalf("plugins status output = %q, want the mounted plugin %q still reported", out, testEchoPlugin)
	}
	if !strings.Contains(out, testEchoTool) {
		t.Errorf("plugins status output = %q, want the mounted plugin's tool %q reported", out, testEchoTool)
	}
	if !strings.Contains(out, "no longer in the manifest") {
		t.Errorf("plugins status output = %q, want the row to say the manifest no longer declares it", out)
	}
}

// TestPluginsStatusLabelsAFailedEntryTheManifestNoLongerDeclares covers the one
// row nothing else reaches: the loader still remembers a FAILED entry that the
// manifest on disk has since dropped. Its failure must keep its own "error="
// label instead of being folded into the row's "reason=" -- a failure under the
// wrong label, mid-sentence, is a failure nobody greps for.
func TestPluginsStatusLabelsAFailedEntryTheManifestNoLongerDeclares(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	// The proxy guest does not provide testGhostTool, so this entry fails at
	// host.Activate's manifest cross-check.
	f.writePackage("proxy", testProxyWasm, testProxyPlugin, "3.4.0", []string{"tool"}, []string{testProxyTool, testGhostTool})
	f.writeManifest(manifestEntry{
		name: testProxyPlugin, source: "proxy", enabled: true,
		capabilities: []string{"tool"}, tools: []string{testProxyTool, testGhostTool},
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil (a failed entry must not stop startup)", err)
	}

	// The entry leaves the manifest, but nobody has reloaded: the loader still
	// holds its failure.
	f.writeManifest()

	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	line := ""
	for _, candidate := range strings.Split(out, "\n") {
		if strings.Contains(candidate, testProxyPlugin) {
			line = candidate
			break
		}
	}
	if line == "" {
		t.Fatalf("plugins status output = %q, want a row for %q", out, testProxyPlugin)
	}
	if !strings.Contains(line, "no longer in the manifest") {
		t.Errorf("plugins status row = %q, want the reason that it left the manifest", line)
	}
	at := strings.Index(line, "error=")
	if at < 0 {
		t.Fatalf("plugins status row = %q, want the failure labelled error=", line)
	}
	if !strings.Contains(line[at:], testGhostTool) {
		t.Errorf("plugins status row = %q, want the failure text to follow the error= label", line)
	}
}

// TestAssemblePluginsFailsWhenTheManifestIsUnparseable is the other half of
// "configured means meant it": a manifest that exists but is not valid JSON is
// as much a startup failure as one that is missing, and the error names the
// file so the operator knows which one to fix.
func TestAssemblePluginsFailsWhenTheManifestIsUnparseable(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	if err := os.WriteFile(f.manifestPath, []byte("{"), 0o644); err != nil {
		t.Fatalf("write truncated manifest %s: %v", f.manifestPath, err)
	}

	err := f.assemble()
	if err == nil {
		t.Fatal("assemblePlugins() error = nil, want an error: the manifest is not parseable")
	}
	// %q-quoted, which is how the wrapper renders it -- on Windows that means
	// escaped separators, so the raw path is not a substring of the message.
	if !strings.Contains(err.Error(), fmt.Sprintf("%q", f.manifestPath)) {
		t.Errorf("assemblePlugins() error = %v, want it to name the manifest path %s", err, f.manifestPath)
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("assemblePlugins() error = %v, want it to say the manifest could not be parsed", err)
	}
	if got := f.application.Plugins(); got != nil {
		t.Errorf("App.Plugins() = %v after a refused assembly, want nil", got)
	}
}

// TestPluginsStatusWithoutALoaderFails is status's half of the "no loader in
// this process" answer (reload's half is TestPluginsReloadWithoutALoaderFails).
// A manifest IS configured, so reporting an empty plugin list would be a lie
// about a deployment that may well be running in the serve process next door.
func TestPluginsStatusWithoutALoaderFails(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writeManifest()
	// Deliberately no assemble(): the App has no loader.
	_, err := f.run("status")
	if err == nil {
		t.Fatal("plugins status error = nil, want an error: there is no loader in this process")
	}
	if !strings.Contains(err.Error(), "agent serve") {
		t.Errorf("plugins status error = %v, want it to say a loader belongs to `agent serve`", err)
	}
}

// TestPluginsReloadWithoutAConfiguredManifestFails pins a DELIBERATE asymmetry
// with status: with no plugins.manifest configured, status answers "plugins are
// off" and exits 0, while reload fails. They are different questions -- "what is
// the state?" has an answer here, "converge toward the target state" has no
// target to converge toward, and reporting success would claim a reload that
// never happened.
func TestPluginsReloadWithoutAConfiguredManifestFails(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(configPath, []byte(`{"storage": {"driver": "memory"}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out := &bytes.Buffer{}
	err := Execute(app.New(), out, []string{"plugins", "reload", "--config", configPath})
	if err == nil {
		t.Fatal("plugins reload error = nil, want an error: there is no deployment to reload")
	}
	if !strings.Contains(err.Error(), "plugins.manifest") {
		t.Errorf("plugins reload error = %v, want it to name the missing plugins.manifest setting", err)
	}

	// The same config, the same process: status still succeeds.
	statusOut := &bytes.Buffer{}
	if err := Execute(app.New(), statusOut, []string{"plugins", "status", "--config", configPath}); err != nil {
		t.Fatalf("plugins status error = %v, want nil: the asymmetry with reload is deliberate", err)
	}
}

// TestPluginsResolveRelativePathsAgainstTheProcessWorkingDirectory pins where a
// relative plugins.manifest / plugins.root actually resolves: the PROCESS
// working directory, not the directory --config was read from. The config below
// lives in a subdirectory that contains neither the manifest nor the packages,
// so the two rules give different answers and only one of them can pass.
func TestPluginsResolveRelativePathsAgainstTheProcessWorkingDirectory(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

	confDir := filepath.Join(f.dir, "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatalf("create config dir %s: %v", confDir, err)
	}
	f.configPath = filepath.Join(confDir, "agent.json")
	f.writeConfig(fmt.Sprintf(`{
		"storage": {"driver": "memory"},
		"context_files": {"root": %s},
		"plugins": {
			"manifest": "plugins.json",
			"root": "plugins",
			"limits": {"timeout_ms": 5000, "max_memory_pages": 64, "max_instances": 1},
			"apply_wait_ms": 30000
		}
	}`, jsonString(f.dir)))

	// Started from anywhere else, those same relative paths resolve to nothing:
	// the config file's own directory is NOT what they are relative to.
	t.Chdir(t.TempDir())
	if err := f.assemble(); err == nil {
		t.Fatal("assemblePlugins() error = nil from an unrelated working directory, want an error")
	}

	// Started from the deployment's directory, the same config works.
	t.Chdir(f.dir)
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil: relative paths resolve against the process working directory", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = false, want true: the relative deployment must have mounted", testEchoTool)
	}
}

// TestWritePluginStatusAlignsNonASCIINames pins that the status table's columns
// are measured in runes. fmt's width verb pads to a BYTE count, so a plugin
// whose name is not ASCII would otherwise push its whole row out of the
// columns -- and the table is read by a human looking down one column.
func TestWritePluginStatusAlignsNonASCIINames(t *testing.T) {
	out := &bytes.Buffer{}
	rows := []pluginStatusRow{
		{Name: "\u63d2\u4ef6", Version: "1.0.0", State: "loaded"},
		{Name: "ascii-name", Version: "1.0.0", State: "failed"},
	}
	if err := writePluginStatus(out, "plugins.json", "plugins", rows); err != nil {
		t.Fatalf("writePluginStatus() error = %v, want nil", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("writePluginStatus() wrote %d lines (%q), want a header and two rows", len(lines), out.String())
	}
	column := func(line string) int {
		at := strings.Index(line, "version=")
		if at < 0 {
			t.Fatalf("status row = %q, want a version= column", line)
		}
		return utf8.RuneCountInString(line[:at])
	}
	if first, second := column(lines[1]), column(lines[2]); first != second {
		t.Errorf("version= column starts at rune %d on %q but %d on %q; the columns must line up for a reader",
			first, lines[1], second, lines[2])
	}
}
