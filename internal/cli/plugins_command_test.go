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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	_, err = assemblePlugins(context.Background(), f.application, cfg, pluginHostDeps{
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
		Logger: logger,
		Gate:   f.gate,
	})
	return err
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

// TestDrainPluginsUnmountsEverythingOnShutdown pins serve's shutdown step. It
// is not tidiness: a plugin's tool name lives in the process-global gateable
// catalog, so an embedded host that restarts serve in one process would hit a
// duplicate-name panic if shutdown left the first run's plugins mounted.
func TestDrainPluginsUnmountsEverythingOnShutdown(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Fatalf("IsGateable(%q) = false after startup apply, want true", testEchoTool)
	}

	drainPlugins(f.application.Plugins(), f.root, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = true after the shutdown drain, want false", testEchoTool)
	}
	if left := f.application.PluginResources(); len(left) != 0 {
		t.Errorf("PluginResources() = %v after the shutdown drain, want empty", left)
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
