package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/plugin/loader"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/plugin/sign"
	"github.com/stardust/legion-agent/internal/taskgate"
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

// testMissingRequiredTool is a tool name no fixture in this file ever
// contributes. A plugin whose plugin.json requires it can never resolve the
// requirement, so it is how these tests put a plugin into StateSuspended for
// a reason that is NOT a cascade: nobody, known to the loader or not, is ever
// going to provide this name.
const testMissingRequiredTool = "totally_missing_tool"

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
	gate         *taskgate.TaskGate
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
		gate:         taskgate.NewTaskGate(),
	}
	// The signature policy is written EXPLICITLY off: every test in this file
	// but the signature ones is about something else, and an absent
	// require_signature means "required" (config.PluginsConfig), which would
	// make each of them fail assembly on a keyring it never meant to configure.
	f.writeSignatureConfig(applyWaitMs, signaturePolicy{requireSignature: boolPtr(false)})

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

// signaturePolicy is the plugins-section signature policy a fixture writes into
// its agent.json. A nil requireSignature omits the key entirely, which is how a
// test exercises the DEFAULT rather than a value it wrote down; an empty
// keyring omits that key the same way.
type signaturePolicy struct {
	keyring          string
	requireSignature *bool
}

// boolPtr is the one-line spelling of an explicitly-written JSON boolean.
func boolPtr(v bool) *bool { return &v }

// writeSignatureConfig rewrites the fixture's agent.json with the given
// signature policy, leaving every other plugin setting as newPluginFixture
// wrote it. Any extra settings are spliced into the plugins section verbatim,
// which is how the remote-source tests add "cache", "allow_insecure_sources"
// and "fetch" without every other test having to know they exist.
func (f *pluginFixture) writeSignatureConfig(applyWaitMs int, policy signaturePolicy, extra ...string) {
	f.t.Helper()

	settings := []string{
		fmt.Sprintf("\"manifest\": %s", jsonString(f.manifestPath)),
		fmt.Sprintf("\"root\": %s", jsonString(f.root)),
		`"limits": {"timeout_ms": 5000, "max_memory_pages": 64, "max_instances": 1}`,
		fmt.Sprintf(`"apply_wait_ms": %d`, applyWaitMs),
	}
	if policy.keyring != "" {
		settings = append(settings, fmt.Sprintf("\"keyring\": %s", jsonString(policy.keyring)))
	}
	if policy.requireSignature != nil {
		settings = append(settings, fmt.Sprintf(`"require_signature": %t`, *policy.requireSignature))
	}
	settings = append(settings, extra...)
	f.writeConfig(fmt.Sprintf(`{
		"storage": {"driver": "memory"},
		"context_files": {"root": %s},
		"plugins": {%s}
	}`, jsonString(f.dir), strings.Join(settings, ", ")))
}

// testPluginKeyID is the key id every signed fixture package in this file is
// signed and trusted under.
const testPluginKeyID = sign.KeyID("fixture-key")

// newKeyring mints a key pair, writes a keyring document trusting its public
// half into the fixture directory, and returns the private key and that file's
// path. The private key is generated per test and never written to disk:
// committing one -- even a test one -- trains the wrong habit.
func (f *pluginFixture) newKeyring(name string) (ed25519.PrivateKey, string) {
	f.t.Helper()

	pub, priv, err := sign.GenerateKey()
	if err != nil {
		f.t.Fatalf("GenerateKey: %v", err)
	}
	data, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"id":         string(testPluginKeyID),
		"algorithm":  "ed25519",
		"public_key": base64.StdEncoding.EncodeToString(pub),
	}}})
	if err != nil {
		f.t.Fatalf("encode keyring: %v", err)
	}
	path := filepath.Join(f.dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		f.t.Fatalf("write keyring %s: %v", path, err)
	}
	return priv, path
}

// signPackage signs the raw bytes of one package's plugin.json and writes
// plugin.sig beside it, which is what the loader verifies.
func (f *pluginFixture) signPackage(source string, priv ed25519.PrivateKey) {
	f.t.Helper()

	dir := filepath.Join(f.root, source)
	manifestData, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		f.t.Fatalf("read plugin.json in %s: %v", dir, err)
	}
	sig, err := sign.Sign(priv, testPluginKeyID, manifestData)
	if err != nil {
		f.t.Fatalf("sign plugin.json in %s: %v", dir, err)
	}
	doc, err := sign.MarshalSignature(sig)
	if err != nil {
		f.t.Fatalf("encode plugin.sig for %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.sig"), doc, 0o644); err != nil {
		f.t.Fatalf("write plugin.sig in %s: %v", dir, err)
	}
}

// signPackageWithAnyKey signs the package at source with a freshly generated
// key pair that is never registered in any keyring, and writes plugin.sig
// beside it via signPackage. It is for a test whose deployment does not
// require, or does not care about, signature verification but still needs a
// plugin.sig file physically present: archivePackage requires all three
// files fetch.Unpack insists an archive holds, plugin.sig among them,
// regardless of whether the deployment ever checks it.
func (f *pluginFixture) signPackageWithAnyKey(source string) {
	f.t.Helper()

	_, priv, err := sign.GenerateKey()
	if err != nil {
		f.t.Fatalf("GenerateKey: %v", err)
	}
	f.signPackage(source, priv)
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

	f.writePackageWithRequires(source, wasmFile, name, version, capabilities, tools, nil)
}

// writePackageWithRequires is writePackage plus a plugin.json "requires"
// declaration, for a fixture that wants ITS OWN plugin suspended (or another
// plugin cascading off it) rather than merely mounted. Requires is the
// package-manifest field the dependency convergence reads — it names tools
// this plugin calls into, not tools it contributes — so it has no bearing on
// host.Activate's cross-check against the guest's own self-description
// (name and provides); a fixture may declare any requires here without
// touching the two committed guest binaries' fixed identities.
func (f *pluginFixture) writePackageWithRequires(source, wasmFile, name, version string, capabilities, tools, requires []string) {
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
		Requires:     requires,
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

	// digest is the entry's "digest" key, mandatory on a remote source and
	// refused on a local one (manifest.ParseDeployment). An empty one omits
	// the key entirely, which is what every local fixture wants.
	digest string

	// omitGrant, when true, leaves the JSON "grant" key out of this entry
	// entirely — the shape `agent plugins install` writes (manifest.DraftEntry
	// leaves GrantStated false), and what Part A's GrantStated exists to tell
	// apart from an entry whose grant block is merely empty. capabilities is
	// meaningless when this is true and every existing caller leaves it
	// false, so every entry written before this field existed keeps carrying
	// an explicit (if possibly empty) "grant" block exactly as before.
	omitGrant bool
}

// writeManifest writes plugins.json with the given entries.
func (f *pluginFixture) writeManifest(entries ...manifestEntry) {
	f.t.Helper()

	type rawEntry struct {
		Name    string                `json:"name"`
		Source  string                `json:"source"`
		Digest  string                `json:"digest,omitempty"`
		Enabled bool                  `json:"enabled"`
		Grant   *manifest.GrantDecl   `json:"grant,omitempty"`
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
		var grant *manifest.GrantDecl
		if !e.omitGrant {
			grant = &manifest.GrantDecl{Capabilities: e.capabilities}
		}
		raw.Plugins = append(raw.Plugins, rawEntry{
			Name:    e.name,
			Source:  e.source,
			Digest:  e.digest,
			Enabled: e.enabled,
			Grant:   grant,
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

// TestPluginsStatusDistinguishesUnauthorizedFromDisabled is rule 4 (and the
// mutation-verification target for Part B: collapsing pluginStateUnauthorized
// back into pluginStateDisabled must fail this test). An entry nobody has
// ever made a grant decision about ("grant" key never present) reads
// differently from one an operator explicitly denied, and each row names a
// different next step.
func TestPluginsStatusDistinguishesUnauthorizedFromDisabled(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if !strings.Contains(out, "unauthorized") {
		t.Fatalf("plugins status output = %q, want the never-granted entry reported as unauthorized", out)
	}
	if !strings.Contains(out, "agent plugins grant") {
		t.Errorf("plugins status output = %q, want the unauthorized row to name `agent plugins grant` as the "+
			"next step", out)
	}
	if strings.Contains(out, "disabled") {
		t.Errorf("plugins status output = %q, want the never-granted entry not labelled disabled", out)
	}

	// Record an explicit decision the direct way: `agent plugins deny` sets
	// GrantStated true while keeping the entry disabled, which is what
	// distinguishes the row from the one above.
	if _, err := f.run("deny", testEchoPlugin); err != nil {
		t.Fatalf("plugins deny error = %v, want nil", err)
	}
	out, err = f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if !strings.Contains(out, "disabled") {
		t.Fatalf("plugins status output = %q, want the denied entry reported as disabled", out)
	}
	if strings.Contains(out, "unauthorized") {
		t.Errorf("plugins status output = %q, want the denied entry no longer reported as unauthorized", out)
	}
}

// TestPluginsStatusNeverLabelsAnEnabledOldStyleEntryUnauthorized is rule 5's
// companion at the layer an operator actually reads: an entry written before
// Part A existed — "enabled": true, no grant block at all — must never show
// up as unauthorized just because manifest.Entry.GrantStated is false. This
// holds structurally (the unauthorized branch only runs when !entry.Enabled),
// but this test pins it against a regression that checked GrantStated
// without also checking Enabled.
func TestPluginsStatusNeverLabelsAnEnabledOldStyleEntryUnauthorized(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if strings.Contains(out, "unauthorized") {
		t.Fatalf("plugins status output = %q, want an enabled old-style entry never labelled unauthorized", out)
	}
	if !strings.Contains(out, "loaded") {
		t.Errorf("plugins status output = %q, want the entry mounted and loaded", out)
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
			"require_signature": false,
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

// rowFor returns the single line of out whose OWN plugin name (the first
// column, after the table's leading indent) is name, failing the test when
// there is none or more than one. Matching on containment instead would give
// a false match here on purpose: a cascade's waiting_on= names another row's
// plugin inline (see TestPluginsStatusDistinguishesCascadedSuspensionFromDirect),
// so a line can legitimately CONTAIN a name that is not its own.
func rowFor(t *testing.T, out, name string) string {
	t.Helper()

	found := ""
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != name {
			continue
		}
		if found != "" {
			t.Fatalf("plugins status output has more than one row for %q:\n%s\n%s", name, found, line)
		}
		found = line
	}
	if found == "" {
		t.Fatalf("plugins status output = %q, want a row for %q", out, name)
	}
	return found
}

// TestPluginsStatusNamesTheToolADirectSuspensionIsWaitingOn is decision #1 of
// the a4b-task-5 brief: a suspended row must name the unresolved tool, not
// just say "suspended" and send the operator to read code. The plugin here
// requires a tool NOTHING in this deployment (or anywhere else) contributes,
// so the row's explanation must say so plainly rather than point at some
// other plugin -- there is no cascade to point at.
//
// This is also the mandated mutation's target: dropping SuspendedBy from the
// row must make the testMissingRequiredTool assertion below fail.
func TestPluginsStatusNamesTheToolADirectSuspensionIsWaitingOn(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackageWithRequires("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil,
		[]string{testEchoTool}, []string{testMissingRequiredTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil (an unresolved requirement suspends, it does not fail startup)", err)
	}

	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	row := rowFor(t, out, testEchoPlugin)
	if !strings.Contains(row, "suspended") {
		t.Fatalf("plugins status row = %q, want the suspended state", row)
	}
	if !strings.Contains(row, testMissingRequiredTool) {
		t.Fatalf("plugins status row = %q, want it to name the unresolved tool %q", row, testMissingRequiredTool)
	}
	// The withdrawal is real: the tool this plugin WOULD contribute is not
	// currently gateable.
	if toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = true, want false: a suspended entry's tools are withdrawn", testEchoTool)
	}
	// Nobody provides testMissingRequiredTool, known or not -- this must not
	// be reported as a cascade onto some other plugin.
	if strings.Contains(row, "cascade") {
		t.Errorf("plugins status row = %q, want no cascade label: nothing in this deployment provides %q",
			row, testMissingRequiredTool)
	}
}

// TestPluginsStatusDistinguishesCascadedSuspensionFromDirect is decision #2:
// a plugin suspended because the tool it needs is missing entirely, and a
// plugin suspended because the PLUGIN that provides that tool is itself
// suspended, are different operator problems, and the row has to say which
// one this is.
//
// Two plugins are enough to build a cascade here without a third guest
// fixture: echo requires a tool nobody provides (so it suspends directly),
// and proxy requires echo's own tool -- which is provided by a plugin that is
// mounted but not active, so proxy's suspension cascades off echo's.
func TestPluginsStatusDistinguishesCascadedSuspensionFromDirect(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackageWithRequires("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil,
		[]string{testEchoTool}, []string{testMissingRequiredTool})
	f.writePackageWithRequires("proxy", testProxyWasm, testProxyPlugin, "3.4.0", []string{"tool"},
		[]string{testProxyTool}, []string{testEchoTool})
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

	echoRow := rowFor(t, out, testEchoPlugin)
	if !strings.Contains(echoRow, "suspended") || !strings.Contains(echoRow, testMissingRequiredTool) {
		t.Fatalf("echo row = %q, want suspended naming %q", echoRow, testMissingRequiredTool)
	}
	if strings.Contains(echoRow, "cascade") {
		t.Errorf("echo row = %q, want no cascade label: %q has no provider at all", echoRow, testMissingRequiredTool)
	}

	proxyRow := rowFor(t, out, testProxyPlugin)
	if !strings.Contains(proxyRow, "suspended") {
		t.Fatalf("proxy row = %q, want the suspended state", proxyRow)
	}
	if !strings.Contains(proxyRow, testEchoTool) {
		t.Fatalf("proxy row = %q, want it to name the unresolved tool %q", proxyRow, testEchoTool)
	}
	if !strings.Contains(proxyRow, "cascade") {
		t.Errorf("proxy row = %q, want it labelled a cascade: %q is provided by %q, which is itself suspended",
			proxyRow, testEchoTool, testEchoPlugin)
	}
	if !strings.Contains(proxyRow, testEchoPlugin) {
		t.Errorf("proxy row = %q, want it to name %q as the plugin its cascade traces back to", proxyRow, testEchoPlugin)
	}
}

// TestPluginsStatusShowsAllFourStatesTogetherWithoutSwallowing is decision #3:
// disabled, failed, suspended and pending must each keep their own row when
// all four are on screen at once, and no row's explanation must bleed into
// another's.
func TestPluginsStatusShowsAllFourStatesTogetherWithoutSwallowing(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackageWithRequires("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil,
		[]string{testEchoTool}, []string{testMissingRequiredTool})
	// The proxy guest does not provide testGhostTool, so this entry fails at
	// host.Activate's manifest cross-check.
	f.writePackage("proxy", testProxyWasm, testProxyPlugin, "3.4.0", []string{"tool"}, []string{testProxyTool, testGhostTool})
	f.writeManifest(
		manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}},
		manifestEntry{name: testProxyPlugin, source: "proxy", enabled: true, capabilities: []string{"tool"}, tools: []string{testProxyTool, testGhostTool}},
		// Reuses the echo package on disk; a disabled entry is never read
		// (Loader.Apply skips it before the package is ever opened), so the
		// mismatched identity is harmless.
		manifestEntry{name: "disabled-plugin", source: "echo", enabled: false, tools: []string{testEchoTool}},
	)
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil (no bad entry may stop startup)", err)
	}

	// A fourth entry, added to the manifest on disk WITHOUT a reload: enabled,
	// but nothing has converged it yet.
	f.writeManifest(
		manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}},
		manifestEntry{name: testProxyPlugin, source: "proxy", enabled: true, capabilities: []string{"tool"}, tools: []string{testProxyTool, testGhostTool}},
		manifestEntry{name: "disabled-plugin", source: "echo", enabled: false, tools: []string{testEchoTool}},
		manifestEntry{name: "pending-plugin", source: "echo", enabled: true, tools: []string{testEchoTool}},
	)

	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}

	suspendedRow := rowFor(t, out, testEchoPlugin)
	failedRow := rowFor(t, out, testProxyPlugin)
	disabledRow := rowFor(t, out, "disabled-plugin")
	pendingRow := rowFor(t, out, "pending-plugin")

	if !strings.Contains(suspendedRow, "suspended") || !strings.Contains(suspendedRow, testMissingRequiredTool) {
		t.Errorf("suspended row = %q, want suspended naming %q", suspendedRow, testMissingRequiredTool)
	}
	if !strings.Contains(failedRow, "failed") || !strings.Contains(failedRow, testGhostTool) {
		t.Errorf("failed row = %q, want failed naming %q", failedRow, testGhostTool)
	}
	if !strings.Contains(disabledRow, "disabled") {
		t.Errorf("disabled row = %q, want the disabled state", disabledRow)
	}
	if !strings.Contains(pendingRow, "pending") {
		t.Errorf("pending row = %q, want the pending state", pendingRow)
	}

	// No swallowing: the suspended entry's missing tool must not leak into the
	// failed row's ghost-tool complaint, or vice versa, and neither the
	// disabled nor the pending row (which explain nothing about a tool) may
	// carry either one.
	for _, row := range []struct {
		label string
		text  string
	}{
		{"failed", failedRow}, {"disabled", disabledRow}, {"pending", pendingRow},
	} {
		if strings.Contains(row.text, testMissingRequiredTool) {
			t.Errorf("%s row = %q, must not carry the suspended row's unresolved tool %q", row.label, row.text, testMissingRequiredTool)
		}
	}
	for _, row := range []struct {
		label string
		text  string
	}{
		{"suspended", suspendedRow}, {"disabled", disabledRow}, {"pending", pendingRow},
	} {
		if strings.Contains(row.text, testGhostTool) {
			t.Errorf("%s row = %q, must not carry the failed row's ghost tool %q", row.label, row.text, testGhostTool)
		}
	}
}

// TestPluginsStatusKeepsWaitingOnForASuspendedEntryTheManifestHasSinceDisabled
// is the a4b-task-5 review's Important #1: "status" re-reads the manifest from
// disk on every call, independently of the loader's live state, so a plugin
// that is currently mounted-and-suspended can pick up a stale "enabled": false
// in the manifest before anyone reloads. The row must still be suspended and
// still name what it is waiting on -- it must not fall into the
// disabled-but-known branch and lose the waiting_on= explanation, which is the
// entire reason SuspendedBy exists.
func TestPluginsStatusKeepsWaitingOnForASuspendedEntryTheManifestHasSinceDisabled(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackageWithRequires("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil,
		[]string{testEchoTool}, []string{testMissingRequiredTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil (an unresolved requirement suspends, it does not fail startup)", err)
	}

	// Edited on disk WITHOUT a reload: the loader's live state is still
	// mounted-and-suspended, but the manifest now says the entry should be off.
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}})

	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	row := rowFor(t, out, testEchoPlugin)
	if !strings.Contains(row, "suspended") {
		t.Fatalf("plugins status row = %q, want the suspended state: the loader has not been reloaded yet", row)
	}
	if !strings.Contains(row, testMissingRequiredTool) {
		t.Fatalf("plugins status row = %q, want it to still name the unresolved tool %q, "+
			"not have waiting_on= swallowed by the disabled-but-known branch", row, testMissingRequiredTool)
	}
}

// TestPluginsStatusMarksASuspendedRowsToolsAsWithdrawn is the a4b-task-5
// review's Important #2: tools= must not read identically for a row that is
// actually serving a tool and a row that merely WOULD serve it once
// unblocked. A loaded row's tools= is unmarked; a suspended row's is
// annotated "(withdrawn)" so a reader scanning only that column cannot
// mistake a suspended plugin for an active provider.
func TestPluginsStatusMarksASuspendedRowsToolsAsWithdrawn(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writePackageWithRequires("proxy", testProxyWasm, testProxyPlugin, "3.4.0", []string{"tool"},
		[]string{testProxyTool}, []string{testMissingRequiredTool})
	f.writeManifest(
		manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}},
		manifestEntry{name: testProxyPlugin, source: "proxy", enabled: true, capabilities: []string{"tool"}, tools: []string{testProxyTool}},
	)
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil (an unresolved requirement suspends, it does not fail startup)", err)
	}

	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}

	loadedRow := rowFor(t, out, testEchoPlugin)
	if !strings.Contains(loadedRow, "tools=["+testEchoTool+"]") {
		t.Fatalf("loaded row = %q, want an unmarked tools= column: %q is actually being served", loadedRow, testEchoTool)
	}
	if strings.Contains(loadedRow, "withdrawn") {
		t.Errorf("loaded row = %q, must not read withdrawn: it is serving %q right now", loadedRow, testEchoTool)
	}

	suspendedRow := rowFor(t, out, testProxyPlugin)
	if !strings.Contains(suspendedRow, "tools=["+testProxyTool+"](withdrawn)") {
		t.Errorf("suspended row = %q, want tools=[%s](withdrawn): the tool it would contribute is not served right now",
			suspendedRow, testProxyTool)
	}
}

// TestSuspendedRowDetailCombinesWaitingOnAndError is Minor #1 of the
// a4b-task-5 review: the combined-Detail path at plugins_command.go:101-110 is
// reachable whenever a suspend's OWN teardown fails -- SuspendedBy is recorded
// before Suspend is called (suspend.go:117), so a failing Suspend call
// (suspend.go:237-252) leaves both a non-empty SuspendedBy and a non-empty
// LastError on the same instance. Neither may swallow the other.
func TestSuspendedRowDetailCombinesWaitingOnAndError(t *testing.T) {
	st := loader.InstanceStatus{
		Name:        testEchoPlugin,
		State:       loader.StateSuspended,
		SuspendedBy: []string{testMissingRequiredTool},
		LastError:   "suspend plugin: disposer failed",
	}
	detail := suspendedRowDetail(st, map[string]string{}, map[string]loader.InstanceStatus{})
	if !strings.Contains(detail, "waiting_on="+testMissingRequiredTool) {
		t.Errorf("suspendedRowDetail() = %q, want it to still name the unresolved tool %q", detail, testMissingRequiredTool)
	}
	if !strings.Contains(detail, "error=suspend plugin: disposer failed") {
		t.Errorf("suspendedRowDetail() = %q, want the suspend failure to appear alongside waiting_on=", detail)
	}
}

// TestSuspendedRowDetailFallsBackToErrorAloneWhenSuspendedByIsEmpty is Minor #1's
// other half: internal/plugin/loader's TestApplyReportsAResumeWhoseToolNameWasTaken
// is the scenario where a plugin's own dependency resolves (SuspendedBy comes
// back empty) but its RESUME then fails because its own tool name was taken
// while it was down (suspend.go:140-151) -- LastError is the whole story then,
// and the row must still say something rather than reading bare "suspended".
func TestSuspendedRowDetailFallsBackToErrorAloneWhenSuspendedByIsEmpty(t *testing.T) {
	st := loader.InstanceStatus{
		Name:      testEchoPlugin,
		State:     loader.StateSuspended,
		LastError: `register tool "echo_aux": name already taken`,
	}
	detail := suspendedRowDetail(st, map[string]string{}, map[string]loader.InstanceStatus{})
	if strings.Contains(detail, "waiting_on=") {
		t.Errorf("suspendedRowDetail() = %q, want no waiting_on=: SuspendedBy is empty", detail)
	}
	want := `error=register tool "echo_aux": name already taken`
	if detail != want {
		t.Errorf("suspendedRowDetail() = %q, want %q (the error alone)", detail, want)
	}
}

// TestSuspendedWaitingOnNamesEveryUnresolvedTool is Minor #2 of the a4b-task-5
// review: suspendedWaitingOn's loop is correct by inspection for
// len(suspendedBy) > 1, but no test built that case. Two unresolved tools are
// named here, one direct and one cascaded, and the join must keep both without
// either overwriting the other.
func TestSuspendedWaitingOnNamesEveryUnresolvedTool(t *testing.T) {
	providerOf := map[string]string{testProxyTool: testProxyPlugin}
	byName := map[string]loader.InstanceStatus{
		testProxyPlugin: {Name: testProxyPlugin, State: loader.StateSuspended},
	}
	got := suspendedWaitingOn([]string{testMissingRequiredTool, testProxyTool}, providerOf, byName)
	want := fmt.Sprintf("waiting_on=%s(no plugin provides it) %s(cascade: %s is %s)",
		testMissingRequiredTool, testProxyTool, testProxyPlugin, loader.StateSuspended)
	if got != want {
		t.Errorf("suspendedWaitingOn() = %q, want %q", got, want)
	}
}

// TestAssemblePluginsRefusesRequiredSignaturesWithNoKeyring is the policy's
// central rule. "Signatures are required" and "there is no trust set" is not a
// runnable deployment: it would either refuse every plugin or, far worse,
// quietly verify nothing. Serve does not start, and the error names both ways
// out.
func TestAssemblePluginsRefusesRequiredSignaturesWithNoKeyring(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	// No keyring, and no require_signature either: the absent setting is the
	// strict one, so this config demands signatures it cannot check.
	f.writeSignatureConfig(30_000, signaturePolicy{})
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

	err := f.assemble()
	if err == nil {
		t.Fatal("assemblePlugins() error = nil, want an error: signatures are required with no keyring configured")
	}
	if !strings.Contains(err.Error(), "plugins.keyring") {
		t.Errorf("assemblePlugins() error = %v, want it to name plugins.keyring", err)
	}
	if !strings.Contains(err.Error(), "require_signature") {
		t.Errorf("assemblePlugins() error = %v, want it to name the setting that turns the requirement off", err)
	}
	if toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = true, want false: nothing may mount from a refused assembly", testEchoTool)
	}
}

// TestAssemblePluginsFailsWhenTheKeyringCannotBeRead is the anti-degradation
// rule: a keyring that cannot be read must NOT become "no keyring", which the
// loader would read as "this deployment does not require signatures". A
// security control that switches itself off when its own configuration breaks
// is worse than none, because the logs look normal.
func TestAssemblePluginsFailsWhenTheKeyringCannotBeRead(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	missing := filepath.Join(f.dir, "no-such-keyring.json")
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: missing})
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

	err := f.assemble()
	if err == nil {
		t.Fatal("assemblePlugins() error = nil, want an error: the configured keyring does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("assemblePlugins() error = %v, want it to name the keyring path %s", err, missing)
	}
	if toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = true, want false: nothing may mount from a refused assembly", testEchoTool)
	}
}

// TestAssemblePluginsFailsWhenTheKeyringIsUnparseable is the same rule one step
// later: a keyring file that exists but is not a keyring is a broken trust set,
// not an absent one.
func TestAssemblePluginsFailsWhenTheKeyringIsUnparseable(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	broken := filepath.Join(f.dir, "keyring.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write keyring %s: %v", broken, err)
	}
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: broken})
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

	err := f.assemble()
	if err == nil {
		t.Fatal("assemblePlugins() error = nil, want an error: the configured keyring does not parse")
	}
	// %q-quoted, which is how the wrapper renders it -- on Windows that means
	// escaped separators, so the raw path is not a substring of the message.
	if !strings.Contains(err.Error(), fmt.Sprintf("%q", broken)) {
		t.Errorf("assemblePlugins() error = %v, want it to name the keyring path %s", err, broken)
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("assemblePlugins() error = %v, want it to say the keyring could not be parsed", err)
	}
	if toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = true, want false: nothing may mount from a refused assembly", testEchoTool)
	}
}

// TestAssemblePluginsFailsOnAnUnreadableKeyringEvenWithSignaturesOff pins the
// "configured means you meant it" rule the manifest path already follows. An
// operator who both named a keyring and turned signatures off wrote down two
// contradictory things; the broken file is reported rather than skipped,
// because skipping it is exactly the silent degradation this task exists to
// prevent.
func TestAssemblePluginsFailsOnAnUnreadableKeyringEvenWithSignaturesOff(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	missing := filepath.Join(f.dir, "no-such-keyring.json")
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: missing, requireSignature: boolPtr(false)})
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

	err := f.assemble()
	if err == nil {
		t.Fatal("assemblePlugins() error = nil, want an error: a configured keyring that cannot be read is never silently dropped")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("assemblePlugins() error = %v, want it to name the keyring path %s", err, missing)
	}
}

// TestAssemblePluginsMountsASignedPackageUnderTheDefaultPolicy is the
// end-to-end positive: a config that says nothing about signatures verifies
// them, and a properly signed package mounts through the real assembly.
func TestAssemblePluginsMountsASignedPackageUnderTheDefaultPolicy(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	priv, keyringPath := f.newKeyring("keyring.json")
	// requireSignature is left unwritten on purpose: the default is strict.
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath})
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.signPackage("echo", priv)
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = false, want true: a signed package must mount", testEchoTool)
	}
}

// TestPluginsStatusReportsASignatureFailureWhereAnOperatorLooks pins rule 4 at
// the surface an operator actually reads: a package that fails verification is
// a FAILED entry -- no new state -- whose LastError says it was the signature,
// and startup carries on.
func TestPluginsStatusReportsASignatureFailureWhereAnOperatorLooks(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	_, keyringPath := f.newKeyring("keyring.json")
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath})
	// Written but never signed.
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

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
	if !strings.Contains(out, "plugin.sig") {
		t.Errorf("plugins status output = %q, want the failure to say it was the signature", out)
	}
	if toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = true, want false: an unverified plugin contributes nothing", testEchoTool)
	}
}

// TestAssemblePluginsSkipsVerificationWhenSignaturesAreExplicitlyOff is the
// only door to a nil keyring, and it is guarded here: an unsigned package
// mounts, but only because the config says in so many words that this
// deployment does not require signatures.
func TestAssemblePluginsSkipsVerificationWhenSignaturesAreExplicitlyOff(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)})
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil: signatures are explicitly not required", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = false, want true: an unsigned package mounts where signatures are off", testEchoTool)
	}
}

// TestPluginsReloadRefusesATightenedSignaturePolicy is the a5a-task-3 review's
// Important #1. reload re-reads the config and converges toward the manifest it
// finds there, but the trust set it would verify with was frozen when serve
// assembled the Loader and cannot be swapped under a running one. An operator
// who turns "require_signature" back on (or adds a keyring) and reloads must
// therefore NOT get a convergence: their new manifest would be applied under
// the old, unenforcing policy, with startup's warning long gone from the log
// and nothing on screen saying the policy did not take.
func TestPluginsReloadRefusesATightenedSignaturePolicy(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	// Started with signatures explicitly off, and an unsigned package mounted.
	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)})
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Fatalf("IsGateable(%q) = false after startup apply, want true", testEchoTool)
	}

	// The operator tightens the policy: a keyring is configured and
	// require_signature goes back to its (strict) default.
	_, keyringPath := f.newKeyring("keyring.json")
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath})

	_, err := f.run("reload")
	if err == nil {
		t.Fatal("plugins reload error = nil, want an error: the signature policy in the config is not the one this process enforces")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("plugins reload error = %v, want it to say serve must be restarted for a signature-policy change", err)
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("plugins reload error = %v, want it to name the signature policy as what changed", err)
	}
	// Refusing is not tearing down: the deployment that IS running stays
	// exactly as it was, so an operator who reloads by mistake loses nothing.
	if !toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = false, want true: a refused reload must leave the running deployment alone", testEchoTool)
	}
}

// TestPluginsReloadConvergesWhenTheSignaturePolicyIsUnchanged is the other half
// of that check, on the enforcing side: with the SAME keyring in the config as
// the running Loader was built with, the policies compare equal and the reload
// goes through. Without this, "refuse whenever signatures are enforced" would
// pass the test above and break every reload of a signing deployment.
func TestPluginsReloadConvergesWhenTheSignaturePolicyIsUnchanged(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	priv, keyringPath := f.newKeyring("keyring.json")
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath})
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.signPackage("echo", priv)
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Fatalf("IsGateable(%q) = false after startup apply, want true", testEchoTool)
	}

	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}})
	if _, err := f.run("reload"); err != nil {
		t.Fatalf("plugins reload error = %v, want nil: the signature policy did not change", err)
	}
	if toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = true after reload, want false: the disabled entry must be unmounted", testEchoTool)
	}
}

// TestAssemblePluginsDropsALoadedKeyringWhenSignaturesAreExplicitlyOff is the
// a5a-task-3 review's Minor #3: the branch where a keyring loads FINE and is
// then discarded by policy had no test at all -- the three "off" tests either
// configured no keyring or one that would not read. A valid keyring plus an
// unsigned package is what tells the two apart: if the keyring were kept, this
// package could not mount.
//
// It also pins Minor #4: exactly one line in a startup log says this deployment
// verifies nothing. The cli's warning carries only what the cli knows (a trust
// set is configured, and which file), because two warnings meaning the same
// thing teach an operator to skip both.
func TestAssemblePluginsDropsALoadedKeyringWhenSignaturesAreExplicitlyOff(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	_, keyringPath := f.newKeyring("keyring.json")
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath, requireSignature: boolPtr(false)})
	// Written and never signed: it mounts only if the keyring was truly dropped.
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

	logs := &bytes.Buffer{}
	if err := f.assembleWithLogger(slog.New(slog.NewTextHandler(logs, nil))); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil: a valid keyring plus an explicit off is a legal deployment", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Fatalf("IsGateable(%q) = false, want true: an unsigned package mounting is what proves the keyring was discarded",
			testEchoTool)
	}

	log := logs.String()
	if !strings.Contains(log, "plugin trust keyring is configured but not enforced") {
		t.Errorf("startup log = %q, want the cli to report the trust set it loaded and dropped", log)
	}
	if !strings.Contains(log, "keyring.json") {
		t.Errorf("startup log = %q, want it to name the keyring file that is not being enforced", log)
	}
	if got := strings.Count(log, "signature verification is"); got != 1 {
		t.Errorf("startup log says %q %d times, want exactly 1 (the loader's): a second warning saying the same thing "+
			"is how an operator learns to ignore both.\nlog = %q", "signature verification is", got, log)
	}
}

// TestAssemblePluginsFailsWhenTheKeyringPathIsADirectory is the a5a-task-3
// review's Minor #5(a): a keyring path that names a directory is a configured
// trust set that cannot be read, and os.ReadFile failing on it must fail
// assembly like any other unreadable one rather than leave a nil keyring
// behind.
func TestAssemblePluginsFailsWhenTheKeyringPathIsADirectory(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	keyringDir := filepath.Join(f.dir, "keyring-dir")
	if err := os.MkdirAll(keyringDir, 0o755); err != nil {
		t.Fatalf("create directory %s: %v", keyringDir, err)
	}
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringDir})
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

	err := f.assemble()
	if err == nil {
		t.Fatal("assemblePlugins() error = nil, want an error: a directory is not a readable keyring")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%q", keyringDir)) {
		t.Errorf("assemblePlugins() error = %v, want it to name the keyring path %s", err, keyringDir)
	}
	if toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = true, want false: nothing may mount from a refused assembly", testEchoTool)
	}
}

// TestPluginsResolveARelativeKeyringAgainstTheProcessWorkingDirectory is the
// a5a-task-3 review's Minor #5(b): plugins.keyring follows the same rule as
// plugins.manifest and plugins.root -- a relative path resolves against the
// PROCESS working directory, not the directory --config was read from. The
// config below lives in a subdirectory holding no keyring at all, so the two
// rules give different answers and only one of them can pass. A later tidy-up
// that "fixed" keyring resolution to be config-relative fails here.
func TestPluginsResolveARelativeKeyringAgainstTheProcessWorkingDirectory(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	priv, _ := f.newKeyring("keyring.json")
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.signPackage("echo", priv)
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

	confDir := filepath.Join(f.dir, "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatalf("create config dir %s: %v", confDir, err)
	}
	f.configPath = filepath.Join(confDir, "agent.json")
	// manifest and root stay absolute on purpose: the only relative path under
	// test here is the keyring, so nothing else can explain the outcome.
	f.writeConfig(fmt.Sprintf(`{
		"storage": {"driver": "memory"},
		"context_files": {"root": %s},
		"plugins": {
			"manifest": %s,
			"root": %s,
			"keyring": "keyring.json",
			"limits": {"timeout_ms": 5000, "max_memory_pages": 64, "max_instances": 1},
			"apply_wait_ms": 30000
		}
	}`, jsonString(f.dir), jsonString(f.manifestPath), jsonString(f.root)))

	// Started anywhere else, "keyring.json" names nothing -- and the config
	// file's own directory does not hold one either, so a config-relative rule
	// would fail here too. What must NOT happen is a nil keyring: signatures
	// are required in this config, so assembly refuses.
	t.Chdir(t.TempDir())
	err := f.assemble()
	if err == nil {
		t.Fatal("assemblePlugins() error = nil from an unrelated working directory, want an error: the relative keyring resolves to nothing there")
	}
	if !strings.Contains(err.Error(), "keyring.json") {
		t.Errorf("assemblePlugins() error = %v, want it to name the keyring path that could not be read", err)
	}
	if toolauth.IsGateable(testEchoTool) {
		t.Fatalf("IsGateable(%q) = true, want false: nothing may mount from a refused assembly", testEchoTool)
	}

	// Started from the deployment's directory, the same relative path finds the
	// keyring and the signed package mounts.
	t.Chdir(f.dir)
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil: a relative keyring resolves against the process working directory", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = false, want true: the signed package must have mounted", testEchoTool)
	}
}

// TestAssemblePluginsReportsAConfiguredKeyringWhilePluginsAreOff is the
// a5a-task-3 review's Minor #6. With no plugins.manifest nothing is loaded, so
// assembly returns before the keyring is ever read -- a broken one would sit
// there unreported until the day plugins are switched on. There is no security
// consequence (nothing mounts), but "configured means you meant it" holds
// everywhere else on this path, so the exception is announced rather than
// silent: the keyring below does not even exist, and startup still succeeds.
func TestAssemblePluginsReportsAConfiguredKeyringWhilePluginsAreOff(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	keyringPath := filepath.Join(dir, "no-such-keyring.json")
	body := fmt.Sprintf(`{
		"storage": {"driver": "memory"},
		"context_files": {"root": %s},
		"plugins": {"keyring": %s}
	}`, jsonString(dir), jsonString(keyringPath))
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config %s: %v", configPath, err)
	}
	cfg, err := config.Load(context.Background(), config.Options{Path: configPath})
	if err != nil {
		t.Fatalf("load config %s: %v", configPath, err)
	}

	logs := &bytes.Buffer{}
	if err := assemblePlugins(context.Background(), app.New(), cfg, pluginHostDeps{
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
		Gate:   taskgate.NewTaskGate(),
	}); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil: no manifest is the documented plugins-are-off state", err)
	}

	log := logs.String()
	if !strings.Contains(log, "plugin trust keyring is configured but plugins are not enabled") {
		t.Errorf("startup log = %q, want it to say the configured keyring is doing nothing", log)
	}
	if !strings.Contains(log, "no-such-keyring.json") {
		t.Errorf("startup log = %q, want it to name the keyring that is not in use", log)
	}
}

// --- keygen / sign ---------------------------------------------------------

// runAgent runs the root command with args and returns EVERYTHING the process
// would have shown an operator: the writer the commands print to, cobra's own
// stdout and its stderr are all pointed at one buffer. The private-key leak
// test below depends on that: a key that reached any of the three would be a
// key an operator (or a CI log) can read, and a test watching only one stream
// would miss it.
func runAgent(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot(app.New(), &out)
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	root.SetContext(context.Background())
	err := root.Execute()
	return out.String(), err
}

// keyEntryFrom extracts the keyring entry `agent plugins keygen` printed —
// the substring from its first "{" to its last "}" — and parses it as a
// one-key trust set. It is how these tests take the operator's own route:
// paste what keygen printed into a keyring, and use that keyring to verify.
func keyEntryFrom(t *testing.T, out string) *sign.Keyring {
	t.Helper()
	doc := keyringDocFrom(t, out)
	kr, err := sign.ParseKeyring(doc)
	if err != nil {
		t.Fatalf("the entry keygen printed (%s) does not parse inside a keyring: %v", doc, err)
	}
	return kr
}

// mustKeygen runs keygen into dir under id and returns the private key path
// together with the keyring built from the entry it printed.
func mustKeygen(t *testing.T, dir string, id string) (string, *sign.Keyring) {
	t.Helper()
	keyPath := filepath.Join(dir, id+".key")
	out, err := runAgent(t, "plugins", "keygen", "--key-id", id, "--private-key", keyPath)
	if err != nil {
		t.Fatalf("plugins keygen error = %v, want nil (output: %s)", err, out)
	}
	return keyPath, keyEntryFrom(t, out)
}

// writeSignablePackage writes a plugin.json into a fresh directory and
// returns the directory and the manifest's raw bytes — the exact bytes a
// signature must be made over.
func writeSignablePackage(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	body := []byte(`{"name":"legion-jira","version":"1.2.0","abi":1,"sha256":"` +
		strings.Repeat("ab", 32) + `","tools":["jira_search"],"capabilities":["log"]}`)
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), body, 0o600); err != nil {
		t.Fatalf("write plugin.json: %v", err)
	}
	return dir, body
}

func TestPluginsKeygenWritesAPrivateKeyAndPrintsAPastableKeyringEntry(t *testing.T) {
	dir := t.TempDir()
	keyPath, kr := mustKeygen(t, dir, "ops-2026")

	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read the generated private key: %v", err)
	}
	id, priv, err := sign.ParsePrivateKey(data)
	if err != nil {
		t.Fatalf("the private key keygen wrote does not parse: %v", err)
	}
	if id != "ops-2026" {
		t.Errorf("private key names key id %q, want %q", id, "ops-2026")
	}
	// The round trip that matters: the key on disk and the entry on stdout
	// must be halves of the same pair, or an operator who pastes the entry
	// into their keyring gets a trust set that refuses everything this key
	// signs.
	sig, err := sign.Sign(priv, id, []byte("message"))
	if err != nil {
		t.Fatalf("sign with the generated private key: %v", err)
	}
	if err := kr.Verify(sig, []byte("message")); err != nil {
		t.Fatalf("the printed entry does not verify what the written key signs: %v", err)
	}
}

// TestPluginsKeygenRefusesToOverwriteAnExistingPrivateKey pins the one
// irreversible thing this command could do. Overwriting a private key
// destroys every signature ever made with it, with no way back, so the
// refusal is checked together with the file being byte-for-byte untouched:
// "it errored" is not enough if it errored after truncating.
func TestPluginsKeygenRefusesToOverwriteAnExistingPrivateKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ops-2026.key")
	original := []byte("the production signing key, irreplaceable\n")
	if err := os.WriteFile(keyPath, original, 0o600); err != nil {
		t.Fatalf("write the pre-existing key: %v", err)
	}

	out, err := runAgent(t, "plugins", "keygen", "--key-id", "ops-2026", "--private-key", keyPath)
	if err == nil {
		t.Fatalf("plugins keygen error = nil, want an error: %s already exists (output: %s)", keyPath, out)
	}
	if !strings.Contains(err.Error(), keyPath) {
		t.Errorf("plugins keygen error = %v, want it to name the file it refused to overwrite", err)
	}

	after, readErr := os.ReadFile(keyPath)
	if readErr != nil {
		t.Fatalf("read the pre-existing key after the refusal: %v", readErr)
	}
	if !bytes.Equal(after, original) {
		t.Errorf("the pre-existing key changed: got %q, want %q", after, original)
	}
}

func TestPluginsKeygenWritesThePrivateKeyReadableOnlyByItsOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping the 0600 assertion: Go maps a file mode onto Windows' read-only " +
			"attribute only, so os.Stat reports 0666/0444 whatever mode the file was created " +
			"with. Asserting it here would pin the mapping, not the permission.")
	}
	dir := t.TempDir()
	keyPath, _ := mustKeygen(t, dir, "ops-2026")

	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat the generated private key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("private key mode = %#o, want %#o", perm, 0o600)
	}
}

// assertNeverPrintsPrivateKey fails t if text contains priv or its seed, in
// every encoding a leak might plausibly take. When fileData is non-nil, the
// whole private key file's contents are searched for too. It is shared
// between the success-path and forced-failure cases of the keygen leak test
// below, so both search with the SAME technique.
func assertNeverPrintsPrivateKey(t *testing.T, label string, text string, priv ed25519.PrivateKey, fileData []byte) {
	t.Helper()
	seed := priv.Seed()
	forbidden := map[string]string{
		"the private key, raw":      string(priv),
		"the private key, base64":   base64.StdEncoding.EncodeToString(priv),
		"the private key, hex":      hex.EncodeToString(priv),
		"the private key, url-safe": base64.URLEncoding.EncodeToString(priv),
		"the seed, raw":             string(seed),
		"the seed, base64":          base64.StdEncoding.EncodeToString(seed),
		"the seed, hex":             hex.EncodeToString(seed),
	}
	if fileData != nil {
		forbidden["the whole private key file"] = string(fileData)
	}
	for what, needle := range forbidden {
		if strings.Contains(text, needle) {
			t.Errorf("%s contains %s", label, what)
		}
	}
}

// fixedReader is an io.Reader over a fixed byte slice. crypto/rand.Reader is
// swapped for one of these, and restored immediately after, to make
// sign.GenerateKey's output predictable for exactly the duration of one
// call: without that, there would be no way to know what key a
// forced-failure keygen call held, and therefore nothing to search its error
// message for.
type fixedReader struct{ data []byte }

func (r *fixedReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if n < len(p) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

// TestPluginsKeygenNeverPrintsThePrivateKey searches EVERYTHING the command
// wrote for the key it just produced, in every encoding it could plausibly
// have been rendered in, and for the seed alone (the first 32 bytes) — half a
// private key is a whole private key.
//
// The public key is asserted to BE present on the success path, so this test
// cannot pass by the search being broken.
//
// It also forces a write failure — --private-key pointing into a directory
// that does not exist, which fails at the os.OpenFile call rather than at
// O_EXCL — and searches the returned error the same way. keygen's error
// paths hold priv and keyDoc in scope right up until they return (see
// plugins_command.go, the OpenFile/Write/Close/Sync branches), so an error
// string is exactly as capable of leaking the key as stdout is; the success
// path alone left those branches checked only by inspection.
func TestPluginsKeygenNeverPrintsThePrivateKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ops-2026.key")
	out, err := runAgent(t, "plugins", "keygen", "--key-id", "ops-2026", "--private-key", keyPath)
	if err != nil {
		t.Fatalf("plugins keygen error = %v, want nil", err)
	}

	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read the generated private key: %v", err)
	}
	_, priv, err := sign.ParsePrivateKey(data)
	if err != nil {
		t.Fatalf("the private key keygen wrote does not parse: %v", err)
	}
	assertNeverPrintsPrivateKey(t, "keygen output", out, priv, data)

	// The counter-assertion: the search above is only meaningful if this
	// technique can find a key that IS printed.
	pub := priv.Public().(ed25519.PublicKey)
	if !strings.Contains(out, base64.StdEncoding.EncodeToString(pub)) {
		t.Error("keygen output does not contain the PUBLIC key, so the searches above prove nothing")
	}

	// The forced-failure case: the key generated for THIS call is made
	// predictable by swapping crypto/rand.Reader for a fixed 32-byte stream,
	// restored immediately after the call returns.
	seed := bytes.Repeat([]byte{0x42}, ed25519.SeedSize)
	wantPriv := ed25519.NewKeyFromSeed(seed)
	original := rand.Reader
	rand.Reader = &fixedReader{data: append([]byte(nil), seed...)}
	badPath := filepath.Join(dir, "no-such-directory", "ops-2026.key")
	_, failErr := runAgent(t, "plugins", "keygen", "--key-id", "ops-2026", "--private-key", badPath)
	rand.Reader = original
	if failErr == nil {
		t.Fatal("plugins keygen into a non-existent directory error = nil, want an error")
	}
	assertNeverPrintsPrivateKey(t, "keygen error", failErr.Error(), wantPriv, nil)
}

func TestPluginsKeygenRefusesAnEmptyKeyID(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "blank.key")
	_, err := runAgent(t, "plugins", "keygen", "--key-id", "", "--private-key", keyPath)
	if err == nil {
		t.Fatal("plugins keygen error = nil, want an error: an unnamed key can never be resolved in a keyring")
	}
	if _, statErr := os.Stat(keyPath); !os.IsNotExist(statErr) {
		t.Errorf("a key file was created for a refused key id: stat error = %v, want not-exist", statErr)
	}
}

// failAfterWriter fails every Write call after the first n succeed, letting
// a test force an output failure at a specific point in a command without
// breaking every line that comes before it.
type failAfterWriter struct {
	n     int
	calls int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls > w.n {
		return 0, errors.New("simulated output failure")
	}
	return len(p), nil
}

// TestPluginsKeygenCleansUpWhenItCannotPrintTheKeyringEntry pins the fix for
// the strand M-3 describes: a private key that reaches disk successfully but
// whose keyring entry the operator never gets to see (because the output
// stream itself failed, e.g. a broken pipe) must not be left behind. Left in
// place, it would exist with no public entry ever shown for it, and the next
// keygen attempt at the same path would be refused by O_EXCL — an operator
// stuck with a key they cannot use and cannot regenerate. Both Fprintf calls
// in runPluginsKeygen are exercised: the first (mode/path line) and the
// second (the keyring entry itself).
func TestPluginsKeygenCleansUpWhenItCannotPrintTheKeyringEntry(t *testing.T) {
	for _, n := range []int{0, 1} {
		t.Run(fmt.Sprintf("failAfter=%d", n), func(t *testing.T) {
			dir := t.TempDir()
			keyPath := filepath.Join(dir, "ops-2026.key")
			w := &failAfterWriter{n: n}

			err := runPluginsKeygen(w, "ops-2026", keyPath)
			if err == nil {
				t.Fatal("runPluginsKeygen error = nil, want an error: printing the keyring entry failed")
			}
			if _, statErr := os.Stat(keyPath); !os.IsNotExist(statErr) {
				t.Errorf("the private key survived a failed keyring-entry print: stat error = %v, want not-exist",
					statErr)
			}
		})
	}
}

func TestPluginsSignProducesASignatureTheVerifierAccepts(t *testing.T) {
	keyDir := t.TempDir()
	keyPath, kr := mustKeygen(t, keyDir, "ops-2026")
	pkgDir, manifestData := writeSignablePackage(t)

	out, err := runAgent(t, "plugins", "sign", pkgDir, "--private-key", keyPath)
	if err != nil {
		t.Fatalf("plugins sign error = %v, want nil (output: %s)", err, out)
	}

	sigData, err := os.ReadFile(filepath.Join(pkgDir, "plugin.sig"))
	if err != nil {
		t.Fatalf("read the plugin.sig sign wrote: %v", err)
	}
	sig, err := sign.ParseSignature(sigData)
	if err != nil {
		t.Fatalf("the plugin.sig sign wrote does not parse: %v", err)
	}
	if err := kr.Verify(sig, manifestData); err != nil {
		t.Fatalf("the plugin.sig sign wrote does not verify against the keyring keygen printed: %v", err)
	}
	if sig.KeyID != "ops-2026" {
		t.Errorf("plugin.sig names key %q, want %q", sig.KeyID, "ops-2026")
	}
}

func TestPluginsSignFailsWhenThePackageHasNoManifest(t *testing.T) {
	keyDir := t.TempDir()
	keyPath, _ := mustKeygen(t, keyDir, "ops-2026")
	pkgDir := t.TempDir()

	_, err := runAgent(t, "plugins", "sign", pkgDir, "--private-key", keyPath)
	if err == nil {
		t.Fatal("plugins sign error = nil, want an error: there is no plugin.json to sign")
	}
	if !strings.Contains(err.Error(), "plugin.json") {
		t.Errorf("plugins sign error = %v, want it to name the missing plugin.json", err)
	}
	if _, statErr := os.Stat(filepath.Join(pkgDir, "plugin.sig")); !os.IsNotExist(statErr) {
		t.Errorf("a plugin.sig was written for a package with no manifest: stat error = %v, want not-exist", statErr)
	}
}

func TestPluginsSignFailsOnACorruptPrivateKeyFile(t *testing.T) {
	keyDir := t.TempDir()
	keyPath := filepath.Join(keyDir, "corrupt.key")
	if err := os.WriteFile(keyPath, []byte("-----BEGIN NOT A KEY-----\n"), 0o600); err != nil {
		t.Fatalf("write the corrupt key: %v", err)
	}
	pkgDir, _ := writeSignablePackage(t)

	_, err := runAgent(t, "plugins", "sign", pkgDir, "--private-key", keyPath)
	if err == nil {
		t.Fatal("plugins sign error = nil, want an error: the private key file does not parse")
	}
	if !strings.Contains(err.Error(), keyPath) {
		t.Errorf("plugins sign error = %v, want it to name the private key file", err)
	}
	if _, statErr := os.Stat(filepath.Join(pkgDir, "plugin.sig")); !os.IsNotExist(statErr) {
		t.Errorf("a plugin.sig was written from a corrupt key: stat error = %v, want not-exist", statErr)
	}
}

func TestPluginsSignFailsWhenThePrivateKeyFileIsMissing(t *testing.T) {
	pkgDir, _ := writeSignablePackage(t)
	keyPath := filepath.Join(t.TempDir(), "absent.key")

	_, err := runAgent(t, "plugins", "sign", pkgDir, "--private-key", keyPath)
	if err == nil {
		t.Fatal("plugins sign error = nil, want an error: the private key file does not exist")
	}
	if !strings.Contains(err.Error(), keyPath) {
		t.Errorf("plugins sign error = %v, want it to name the private key file", err)
	}
}

// TestPluginsSignRefusesToWriteASignatureItCannotVerifyItself is the test the
// self-check exists for. The private key below is well-formed in every
// checkable way — 64 bytes, base64, named — but its two halves come from
// DIFFERENT key pairs, so ed25519.Sign happily produces a signature that
// verifies against nothing at all. A signer without a self-check would write
// that signature out and report success, and the operator would discover it
// at deployment time as "verification is broken".
func TestPluginsSignRefusesToWriteASignatureItCannotVerifyItself(t *testing.T) {
	_, privA, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key A: %v", err)
	}
	pubB, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key B: %v", err)
	}
	mixed := make([]byte, ed25519.PrivateKeySize)
	copy(mixed, privA.Seed())
	copy(mixed[ed25519.SeedSize:], pubB)
	doc, err := sign.MarshalPrivateKey("ops-2026", ed25519.PrivateKey(mixed))
	if err != nil {
		t.Fatalf("marshal the mismatched private key: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "mismatched.key")
	if err := os.WriteFile(keyPath, doc, 0o600); err != nil {
		t.Fatalf("write the mismatched key: %v", err)
	}
	pkgDir, _ := writeSignablePackage(t)

	out, err := runAgent(t, "plugins", "sign", pkgDir, "--private-key", keyPath)
	if err == nil {
		t.Fatalf("plugins sign error = nil, want an error: the signature it produced verifies against nothing (output: %s)", out)
	}
	if !strings.Contains(err.Error(), "verif") {
		t.Errorf("plugins sign error = %v, want it to say the self-check failed", err)
	}
	if _, statErr := os.Stat(filepath.Join(pkgDir, "plugin.sig")); !os.IsNotExist(statErr) {
		t.Errorf("plugin.sig was written despite the self-check failing: stat error = %v, want not-exist", statErr)
	}
}

// TestPluginsSignSaysWhenItReplacedAnExistingSignature pins the deliberate
// asymmetry with keygen: re-signing a package is routine and overwriting
// plugin.sig is allowed, but it is announced — a re-sign that silently
// replaced a signature made by a DIFFERENT key would be the quietest way to
// swap out which key a deployment trusts.
func TestPluginsSignSaysWhenItReplacedAnExistingSignature(t *testing.T) {
	keyDir := t.TempDir()
	keyPath, kr := mustKeygen(t, keyDir, "ops-2026")
	pkgDir, manifestData := writeSignablePackage(t)
	sigPath := filepath.Join(pkgDir, "plugin.sig")
	if err := os.WriteFile(sigPath, []byte(`{"key_id":"stale","algorithm":"ed25519","signature":""}`), 0o600); err != nil {
		t.Fatalf("write the stale plugin.sig: %v", err)
	}

	out, err := runAgent(t, "plugins", "sign", pkgDir, "--private-key", keyPath)
	if err != nil {
		t.Fatalf("plugins sign error = %v, want nil: re-signing is a normal operation", err)
	}
	if !strings.Contains(out, "replac") {
		t.Errorf("plugins sign output = %q, want it to say it replaced the existing plugin.sig", out)
	}

	sigData, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatalf("read the replaced plugin.sig: %v", err)
	}
	sig, err := sign.ParseSignature(sigData)
	if err != nil {
		t.Fatalf("the replaced plugin.sig does not parse: %v", err)
	}
	if err := kr.Verify(sig, manifestData); err != nil {
		t.Fatalf("the replaced plugin.sig does not verify: %v", err)
	}
}

// TestPluginsSignNeverPrintsThePrivateKey is keygen's leak test applied to the
// other command that holds key material: sign reads a whole private key off
// disk, and neither its output nor its errors may carry any of it.
func TestPluginsSignNeverPrintsThePrivateKey(t *testing.T) {
	keyDir := t.TempDir()
	keyPath, _ := mustKeygen(t, keyDir, "ops-2026")
	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read the generated private key: %v", err)
	}
	_, priv, err := sign.ParsePrivateKey(data)
	if err != nil {
		t.Fatalf("the private key keygen wrote does not parse: %v", err)
	}
	pkgDir, _ := writeSignablePackage(t)

	out, signErr := runAgent(t, "plugins", "sign", pkgDir, "--private-key", keyPath)
	if signErr != nil {
		t.Fatalf("plugins sign error = %v, want nil", signErr)
	}
	// The failing path holds the same key just as long, so it is searched too.
	failOut, failErr := runAgent(t, "plugins", "sign", filepath.Join(pkgDir, "no-such-package"), "--private-key", keyPath)
	if failErr == nil {
		t.Fatal("plugins sign on a missing package error = nil, want an error")
	}
	everything := out + failOut + failErr.Error()

	seed := priv.Seed()
	for what, needle := range map[string]string{
		"the private key, raw":    string(priv),
		"the private key, base64": base64.StdEncoding.EncodeToString(priv),
		"the private key, hex":    hex.EncodeToString(priv),
		"the seed, base64":        base64.StdEncoding.EncodeToString(seed),
		"the seed, hex":           hex.EncodeToString(seed),
		"the private key file":    string(data),
	} {
		if strings.Contains(everything, needle) {
			t.Errorf("sign output or error contains %s", what)
		}
	}
}

// ---------------------------------------------------------------------------
// a5a Task 5: the operator-facing signature acceptance.
//
// Everything below drives the REAL commands — `agent plugins keygen`, `agent
// plugins sign`, `agent plugins status`, `agent plugins reload` — and the real
// serve assembly (assemblePlugins), over packages on disk. The loader-side
// half, which asserts what a refusal does to the tool registry and to the
// process-global gateable catalog, lives in
// internal/plugin/loader/e2e_test.go.
//
// # Bounds (fork-bomb regime)
//
// Neither test loops. TestSignedDeploymentAcceptanceFromKeygenThroughEveryTamper
// performs a literal FIVE convergences (one assembly plus four reloads),
// written out one after another, and asserts the plugin ledger's size against
// a declared ceiling after every one of them.
// TestTheSignatureRequirementIsASwitchOverTheSamePackageOnDisk performs two.
// Nothing here waits on anything: no channel, no sleep, no polling.

// signedDeploymentLedgerCeiling is the most ledger owners the two-plugin
// deployment below may ever have filed: one instance owner and one
// contribution owner per mounted plugin (see host.ToolsOwner). It is this
// acceptance's fork-bomb bound — a convergence that leaked an instance shows
// up as an owner over the ceiling in the round it happened, rather than after
// "enough" rounds.
const signedDeploymentLedgerCeiling = 4

// requireLedgerCeiling fails the test when the App's plugin ledger holds more
// owners than the round's declared ceiling.
func (f *pluginFixture) requireLedgerCeiling(when string, ceiling int) {
	f.t.Helper()

	snapshot := f.application.PluginLedger().Snapshot()
	if len(snapshot) > ceiling {
		owners := make([]string, 0, len(snapshot))
		for owner := range snapshot {
			owners = append(owners, string(owner))
		}
		sort.Strings(owners)
		f.t.Fatalf("%s: the plugin ledger holds %d owners (%v), want at most %d; a convergence is leaking instances",
			when, len(snapshot), owners, ceiling)
	}
}

// keyringDocFrom builds a keyring DOCUMENT out of what `agent plugins keygen`
// printed: the substring from its first "{" to its last "}", wrapped in the
// "keys" array the command's own output tells the operator to paste it into.
//
// Going through the printed text rather than the key pair in memory is the
// point. It is the only step of the operator's route that is not code calling
// code, and an entry that does not paste into a working keyring is a signing
// story that ends at "verification is broken" on someone's deployment.
func keyringDocFrom(t *testing.T, out string) []byte {
	t.Helper()

	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start < 0 || end < start {
		t.Fatalf("keygen output %q contains no JSON object to paste into a keyring", out)
	}
	return []byte(`{"keys":[` + out[start:end+1] + `]}`)
}

// mustKeygenIntoKeyring runs the real `agent plugins keygen`, writes the entry
// it printed into a keyring file of its own, and returns the private key path
// and that keyring's path — the two files an operator ends up with.
func (f *pluginFixture) mustKeygenIntoKeyring(id string) (keyPath, keyringPath string) {
	f.t.Helper()

	keyPath = filepath.Join(f.dir, id+".key")
	out, err := runAgent(f.t, "plugins", "keygen", "--key-id", id, "--private-key", keyPath)
	if err != nil {
		f.t.Fatalf("plugins keygen error = %v, want nil (output: %s)", err, out)
	}
	keyringPath = filepath.Join(f.dir, id+"-keyring.json")
	if err := os.WriteFile(keyringPath, keyringDocFrom(f.t, out), 0o600); err != nil {
		f.t.Fatalf("write keyring %s: %v", keyringPath, err)
	}
	return keyPath, keyringPath
}

// mustSign runs the real `agent plugins sign` over one package under the
// deployment root.
func (f *pluginFixture) mustSign(source, keyPath string) {
	f.t.Helper()

	dir := filepath.Join(f.root, source)
	out, err := runAgent(f.t, "plugins", "sign", dir, "--private-key", keyPath)
	if err != nil {
		f.t.Fatalf("plugins sign %s error = %v, want nil (output: %s)", dir, err, out)
	}
}

// packageFile is the path of one file inside a package under the deployment
// root.
func (f *pluginFixture) packageFile(source, name string) string {
	return filepath.Join(f.root, source, name)
}

// saveFile reads a file this test is about to tamper with so the tampering can
// be undone exactly.
func saveFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return append([]byte(nil), data...)
}

// restoreFile puts back what saveFile saved.
func restoreFile(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("restore %s: %v", path, err)
	}
}

// flipLastByte rewrites path with its last byte incremented: one byte changed,
// the length and every other byte identical. It is the smallest edit that can
// be made to a binary, which is the point — the sha256 the signed plugin.json
// pins is what has to notice it.
func flipLastByte(t *testing.T, path string) {
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

// retagPackageVersion rewrites a package's plugin.json with a different
// version and a still-CORRECT sha256 for the plugin.wasm beside it.
//
// It is how this test tampers with a package without breaking the digest
// check, so that the refusal it provokes can only have come from the
// signature — which is the reason the signature exists at all.
func retagPackageVersion(t *testing.T, dir, version string) {
	t.Helper()

	path := filepath.Join(dir, "plugin.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var pm manifest.PluginManifest
	if err := json.Unmarshal(data, &pm); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	pm.Version = version
	wasm, err := os.ReadFile(filepath.Join(dir, "plugin.wasm"))
	if err != nil {
		t.Fatalf("read the plugin.wasm beside %s: %v", path, err)
	}
	sum := sha256.Sum256(wasm)
	pm.SHA256 = hex.EncodeToString(sum[:])
	rewritten, err := json.Marshal(pm)
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	if err := os.WriteFile(path, rewritten, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// requireBothPluginsStillMounted is the "nothing moved" half of every tamper
// round below: both plugins' tools are still gateable, and `agent plugins
// status` still reports both as loaded at the version that was signed.
//
// The VERSION is what gives it teeth. A convergence that had accepted the
// re-tagged package would report 9.9.9 here; a count of mounted plugins would
// not tell the two apart.
func (f *pluginFixture) requireBothPluginsStillMounted(when, wantVersion string) {
	f.t.Helper()

	if !toolauth.IsGateable(testEchoTool) {
		f.t.Fatalf("%s: IsGateable(%q) = false, want true: a refused package must not unmount the verified one",
			when, testEchoTool)
	}
	if !toolauth.IsGateable(testProxyTool) {
		f.t.Fatalf("%s: IsGateable(%q) = false, want true: one entry's refusal must not touch another",
			when, testProxyTool)
	}
	out, err := f.run("status")
	if err != nil {
		f.t.Fatalf("%s: plugins status error = %v, want nil", when, err)
	}
	if strings.Count(out, "loaded") != 2 {
		f.t.Fatalf("%s: plugins status output = %q, want both entries reported as loaded", when, out)
	}
	if !strings.Contains(out, wantVersion) {
		f.t.Fatalf("%s: plugins status output = %q, want the mounted version to still be %s", when, out, wantVersion)
	}
	f.requireLedgerCeiling(when, signedDeploymentLedgerCeiling)
}

// requireStatusExplains fails unless `agent plugins status` — the place an
// operator actually looks — carries the given substring.
func (f *pluginFixture) requireStatusExplains(when, want string) {
	f.t.Helper()

	out, err := f.run("status")
	if err != nil {
		f.t.Fatalf("%s: plugins status error = %v, want nil", when, err)
	}
	if !strings.Contains(out, want) {
		f.t.Fatalf("%s: plugins status output = %q, want it to mention %q", when, out, want)
	}
}

// TestSignedDeploymentAcceptanceFromKeygenThroughEveryTamper is the whole
// supply-chain story end to end, through the commands an operator types.
//
//	keygen -> paste the printed entry into a keyring -> sign both packages ->
//	point plugins.json at them -> serve assembly mounts them ->
//	  flip one byte of plugin.wasm            -> reload refused on the sha256
//	  edit plugin.json, keep the digest right -> reload refused on the signature
//	  re-sign with a key the keyring lacks    -> reload refused by key id
//	  re-sign with the trusted key            -> reload converges again
//
// The signature policy is never written down: require_signature is left out of
// the config on purpose, because an unstated policy is the STRICT one and this
// is the deployment shape most installations will actually run.
//
// Each refusal is checked three ways — the command's own error, what `agent
// plugins status` shows an operator, and the running deployment being
// untouched — and the last round is the control that makes the other three
// mean something: the same packages, the same config, the same manifest, only
// signed properly again, converge without complaint. Without it, "reload is
// broken" would pass rounds 1 to 3 just as well as "verification works".
//
// Bound: a literal five convergences, written out. No loop, no wait.
func TestSignedDeploymentAcceptanceFromKeygenThroughEveryTamper(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	for _, name := range []string{testEchoTool, testProxyTool} {
		if toolauth.IsGateable(name) {
			t.Fatalf("%q is already gateable before this test assembled anything: an earlier test leaked its "+
				"contribution and every gateable assertion here would be vacuous", name)
		}
	}

	// The operator mints a key and pastes what keygen printed into a keyring.
	keyPath, keyringPath := f.mustKeygenIntoKeyring("ops-2026")
	// require_signature is deliberately left unwritten: the default is strict.
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath})

	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writePackage("proxy", testProxyWasm, testProxyPlugin, "1.2.0", []string{"tool"}, []string{testProxyTool})
	f.mustSign("echo", keyPath)
	f.mustSign("proxy", keyPath)
	f.writeManifest(
		manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}},
		manifestEntry{
			name: testProxyPlugin, source: "proxy", enabled: true,
			capabilities: []string{"tool"}, tools: []string{testProxyTool},
		},
	)

	// Convergence 1 of 5: startup.
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil: two properly signed packages under the default policy", err)
	}
	f.requireBothPluginsStillMounted("after the startup assembly", "1.2.0")
	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if strings.Contains(out, "error=") {
		t.Fatalf("plugins status output after a clean startup = %q, want no error= on any row", out)
	}

	wasmPath := f.packageFile("echo", "plugin.wasm")
	manifestFile := f.packageFile("echo", "plugin.json")
	originalWasm := saveFile(t, wasmPath)
	originalManifest := saveFile(t, manifestFile)

	// Convergence 2 of 5: the BINARY is tampered with and nothing else.
	// plugin.sig still verifies — it covers plugin.json, which nobody touched —
	// so the only thing that can catch this is the sha256 that signed manifest
	// pins. This is the transitivity claim, tested.
	flipLastByte(t, wasmPath)
	_, err = f.run("reload")
	if err == nil {
		t.Fatal("plugins reload error = nil after one byte of plugin.wasm changed, want a refusal")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("plugins reload error = %v, want it to name the sha256 mismatch", err)
	}
	f.requireStatusExplains("after the tampered module was refused", "sha256")
	f.requireBothPluginsStillMounted("after the tampered module was refused", "1.2.0")
	restoreFile(t, wasmPath, originalWasm)

	// Convergence 3 of 5: the MANIFEST is tampered with, and its declared
	// sha256 is left correct on purpose. Every other check passes, so this
	// refusal can only be the signature's doing.
	retagPackageVersion(t, filepath.Join(f.root, "echo"), "9.9.9")
	_, err = f.run("reload")
	if err == nil {
		t.Fatal("plugins reload error = nil after plugin.json changed under its signature, want a refusal")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("plugins reload error = %v, want it to say the signature did not verify", err)
	}
	f.requireStatusExplains("after the re-tagged manifest was refused", "signature")
	f.requireBothPluginsStillMounted("after the re-tagged manifest was refused", "1.2.0")
	if statusOut, statusErr := f.run("status"); statusErr != nil {
		t.Fatalf("plugins status error = %v, want nil", statusErr)
	} else if strings.Contains(statusOut, "9.9.9") {
		t.Fatalf("plugins status output = %q, want no trace of 9.9.9: the re-tagged package must not have mounted",
			statusOut)
	}
	restoreFile(t, manifestFile, originalManifest)

	// Convergence 4 of 5: the package is intact and correctly signed — by a key
	// this deployment does not trust. Nothing about the signature is malformed;
	// the only thing wrong with it is who made it.
	roguePath, _ := f.mustKeygenIntoKeyring("rogue-2026")
	f.mustSign("echo", roguePath)
	_, err = f.run("reload")
	if err == nil {
		t.Fatal("plugins reload error = nil for a package signed by an untrusted key, want a refusal")
	}
	if !strings.Contains(err.Error(), "rogue-2026") {
		t.Errorf("plugins reload error = %v, want it to name the key id the signature was made with", err)
	}
	if !strings.Contains(err.Error(), "ops-2026") {
		t.Errorf("plugins reload error = %v, want it to name the key this deployment does trust", err)
	}
	f.requireStatusExplains("after the untrusted signature was refused", "rogue-2026")
	f.requireBothPluginsStillMounted("after the untrusted signature was refused", "1.2.0")

	// Convergence 5 of 5: the control. Re-signed by the trusted key, with
	// nothing else changed, the very same deployment reloads cleanly — so the
	// three refusals above were caused by the tampering and not by a reload
	// that had stopped working.
	f.mustSign("echo", keyPath)
	if _, err := f.run("reload"); err != nil {
		t.Fatalf("plugins reload error = %v, want nil: the package is intact and signed by a trusted key again", err)
	}
	f.requireBothPluginsStillMounted("after the package was signed properly again", "1.2.0")
	// And the status an operator reads goes back to clean. This is the defect
	// this acceptance pass found (see the a5a-task-5 report): the entry is
	// unchanged as far as the fingerprint is concerned, so it is not activated
	// again, and before the fix nothing ever took the previous round's failure
	// off the row — leaving `agent plugins status` reporting an untrusted key
	// for a package that had long since been signed properly.
	out, err = f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if strings.Contains(out, "error=") {
		t.Fatalf("plugins status output after a reload that succeeded = %q, want no error= on any row: an "+
			"operator who fixed the package cannot tell a stale failure from a live one", out)
	}
	if strings.Contains(out, "rogue-2026") {
		t.Fatalf("plugins status output = %q, still names the untrusted key after the package was re-signed", out)
	}
}

// TestTheSignatureRequirementIsASwitchOverTheSamePackageOnDisk proves that
// require_signature is a switch and not a decoration, using the strongest
// available control: ONE package tree, ONE manifest, ONE keyring file, and the
// single config setting flipped between the two assemblies.
//
// Two separate fixtures could each be right for the wrong reason — a package
// that mounts in one and not the other proves nothing if the two packages are
// not the same bytes. Here they are literally the same files: the only
// difference between the deployment that refuses and the deployment that
// accepts is the word false.
//
// Bound: a literal two convergences, written out. No loop, no wait.
func TestTheSignatureRequirementIsASwitchOverTheSamePackageOnDisk(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	if toolauth.IsGateable(testEchoTool) {
		t.Fatalf("%q is already gateable before this test assembled anything: an earlier test leaked its "+
			"contribution and every gateable assertion here would be vacuous", testEchoTool)
	}

	_, keyringPath := f.mustKeygenIntoKeyring("ops-2026")
	// Written and never signed. Nothing about the package changes from here on.
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})

	// Convergence 1 of 2: signatures required (the unstated default), keyring
	// configured, package unsigned.
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil: one refused entry must not stop startup", err)
	}
	if toolauth.IsGateable(testEchoTool) {
		t.Fatalf("IsGateable(%q) = true where signatures are required and the package is unsigned, want false",
			testEchoTool)
	}
	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if !strings.Contains(out, "failed") || !strings.Contains(out, "plugin.sig") {
		t.Fatalf("plugins status output = %q, want a failed row saying the signature was the problem", out)
	}
	f.requireLedgerCeiling("after the refused assembly", 0)

	// The operator writes down, in so many words, that this deployment does not
	// require signatures. Nothing else changes: same package, same manifest,
	// same keyring file, same root.
	drainPlugins(f.application, f.root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath, requireSignature: boolPtr(false)})

	// Convergence 2 of 2: the same unsigned package, now mounting.
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil: signatures are explicitly not required", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Fatalf("IsGateable(%q) = false with require_signature explicitly off, want true: the switch did not switch",
			testEchoTool)
	}
	f.requireLedgerCeiling("after the unenforced assembly", 2)
	if out, err := f.run("status"); err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	} else if !strings.Contains(out, "loaded") {
		t.Fatalf("plugins status output = %q, want the entry reported as loaded", out)
	}
}

// remotePluginPackageFiles are the three files a plugin package archive
// carries, the same three fetch.Unpack insists on.
var remotePluginPackageFiles = [...]string{"plugin.json", "plugin.wasm", "plugin.sig"}

// archivePackage packs the package the fixture wrote at source into the
// gzipped tar a remote source serves, and then REMOVES the directory: a test
// that mounts the result can only have got it over the wire, because nothing
// is left under the deployment root to load instead.
func (f *pluginFixture) archivePackage(source string) []byte {
	f.t.Helper()

	dir := filepath.Join(f.root, source)
	buf := &bytes.Buffer{}
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)
	for _, name := range remotePluginPackageFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			f.t.Fatalf("read %s in %s: %v", name, dir, err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			f.t.Fatalf("write tar header for %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			f.t.Fatalf("write tar body for %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		f.t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		f.t.Fatalf("close gzip writer: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		f.t.Fatalf("remove staged package %s: %v", dir, err)
	}
	return buf.Bytes()
}

// digestOfArchive spells data's sha256 the way a deployment entry does.
func digestOfArchive(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// serveArchive starts a plaintext server handing archive to every GET. Plain
// http is deliberate: the assembly builds its own HTTP client, which does not
// trust httptest's TLS certificate, so a plaintext source with
// allow_insecure_sources on is the only way to exercise the real assembled
// client end to end.
func serveArchive(t *testing.T, archive []byte) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write(archive); err != nil {
			t.Errorf("write archive to client: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serveNotFound starts a plaintext server that 404s every request, for the
// tests that are about assembly-time policy rather than about a download.
func serveNotFound(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such artifact", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAssemblePluginsRefusesARemoteEntryWithNoCacheConfigured is rule 1: where
// downloaded plugin code is written is a deployment decision. Falling back to
// a temporary directory would be a silent one, so serve refuses to start.
func TestAssemblePluginsRefusesARemoteEntryWithNoCacheConfigured(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	source := "https://example.invalid/echo.tgz"
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: source, enabled: true, tools: []string{testEchoTool},
		digest: digestOfArchive([]byte("any artifact")),
	})

	err := f.assemble()

	if err == nil {
		t.Fatal("assemblePlugins() error = nil, want an error: a remote entry needs a configured cache directory")
	}
	if !strings.Contains(err.Error(), "plugins.cache") {
		t.Errorf("assemblePlugins() error = %v, want it to name the missing plugins.cache setting", err)
	}
	if !strings.Contains(err.Error(), testEchoPlugin) {
		t.Errorf("assemblePlugins() error = %v, want it to name the remote entry %q", err, testEchoPlugin)
	}
}

// TestAssemblePluginsRefusesAnInsecureRemoteSourceByDefault is rule 6: the
// config says nothing about plaintext sources, so a http:// entry stops serve
// with an error naming the entry, its URL and the way out.
func TestAssemblePluginsRefusesAnInsecureRemoteSourceByDefault(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)},
		fmt.Sprintf("\"cache\": %s", jsonString(filepath.Join(f.dir, "plugin-cache"))))
	source := "http://example.invalid/echo.tgz"
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: source, enabled: true, tools: []string{testEchoTool},
		digest: digestOfArchive([]byte("any artifact")),
	})

	err := f.assemble()

	if err == nil {
		t.Fatal("assemblePlugins() error = nil, want an error: a plaintext source is refused unless it is turned on explicitly")
	}
	if !strings.Contains(err.Error(), source) {
		t.Errorf("assemblePlugins() error = %v, want it to name the offending URL %s", err, source)
	}
	if !strings.Contains(err.Error(), testEchoPlugin) {
		t.Errorf("assemblePlugins() error = %v, want it to name the entry %q", err, testEchoPlugin)
	}
	if !strings.Contains(err.Error(), "allow_insecure_sources") {
		t.Errorf("assemblePlugins() error = %v, want it to name the switch that turns plaintext on", err)
	}
}

// TestAssemblePluginsWarnsOnEveryAllowedInsecureRemoteSource is rule 7: with
// the switch on, EVERY plaintext entry gets a Warn naming it and its URL. A
// deployment that quietly used plaintext because one entry's warning was
// dropped is exactly how this switch gets abused.
func TestAssemblePluginsWarnsOnEveryAllowedInsecureRemoteSource(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)},
		fmt.Sprintf("\"cache\": %s", jsonString(filepath.Join(f.dir, "plugin-cache"))),
		`"allow_insecure_sources": true`)
	srv := serveNotFound(t)
	echoSource := srv.URL + "/echo.tgz"
	proxySource := srv.URL + "/proxy.tgz"
	f.writeManifest(
		manifestEntry{
			name: testEchoPlugin, source: echoSource, enabled: true, tools: []string{testEchoTool},
			digest: digestOfArchive([]byte("echo artifact")),
		},
		manifestEntry{
			name: testProxyPlugin, source: proxySource, enabled: true,
			capabilities: []string{"tool"}, tools: []string{testProxyTool},
			digest: digestOfArchive([]byte("proxy artifact")),
		},
	)

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	// The entries themselves cannot be fetched from this server, which is
	// beside the point: a failed entry does not stop startup, and the warning
	// is written before anything is downloaded.
	if err := f.assembleWithLogger(logger); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil (an unfetchable entry must not stop startup)", err)
	}
	written := logs.String()
	for _, want := range []string{echoSource, proxySource, testEchoPlugin, testProxyPlugin} {
		if !strings.Contains(written, want) {
			t.Errorf("startup log = %q, want a warning naming %s", written, want)
		}
	}
	if got := strings.Count(written, insecurePluginSourceWarning); got != 2 {
		t.Errorf("startup log = %q, want exactly 2 plaintext warnings, got %d", written, got)
	}
}

// TestAssemblePluginsMountsARemotePackageThroughTheConfiguredCache is the
// wiring in one test: a manifest entry naming a URL and a digest is fetched,
// filed under its digest in the configured cache, and mounted — through the
// same assembly serve runs, with the HTTP client the assembly builds.
func TestAssemblePluginsMountsARemotePackageThroughTheConfiguredCache(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	priv, keyringPath := f.newKeyring("keyring.json")
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.signPackage("staging", priv)
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	srv := serveArchive(t, archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	// Signatures are REQUIRED here on purpose: a fetched package must pass the
	// same verification a local one does.
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath, requireSignature: boolPtr(true)},
		fmt.Sprintf("\"cache\": %s", jsonString(cacheDir)),
		`"allow_insecure_sources": true`)
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: srv.URL + "/echo.tgz", enabled: true,
		tools: []string{testEchoTool}, digest: digest,
	})

	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if !strings.Contains(out, "loaded") || !strings.Contains(out, testEchoPlugin) {
		t.Fatalf("plugins status output = %q, want the remote entry mounted", out)
	}
	cached := filepath.Join(cacheDir, "sha256", strings.TrimPrefix(digest, "sha256:"))
	for _, name := range remotePluginPackageFiles {
		if _, err := os.Stat(filepath.Join(cached, name)); err != nil {
			t.Errorf("stat %s in the cache: %v; a fetched package must be filed under its digest", name, err)
		}
	}
}

// TestPluginsReloadRefusesATightenedRemoteSourcePolicy is the a5b-task-5
// review's Important #1, and the same shape the signature-policy check above
// exists for. The remote policy — where downloads land, and whether plaintext
// may be fetched at all — is frozen when serve assembles the Loader. An
// operator who starts with "allow_insecure_sources": true, thinks better of it,
// sets it back to false and reloads must therefore NOT be told the reload
// succeeded: this process would keep fetching over plaintext, and the warning
// that says so is written by assembly, which a reload never runs.
func TestPluginsReloadRefusesATightenedRemoteSourcePolicy(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	priv, keyringPath := f.newKeyring("keyring.json")
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.signPackage("staging", priv)
	archive := f.archivePackage("staging")
	srv := serveArchive(t, archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	cacheSetting := fmt.Sprintf("\"cache\": %s", jsonString(cacheDir))
	// Started with plaintext explicitly allowed, and a plaintext entry fetched
	// and mounted under it.
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath, requireSignature: boolPtr(true)},
		cacheSetting, `"allow_insecure_sources": true`)
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: srv.URL + "/echo.tgz", enabled: true,
		tools: []string{testEchoTool}, digest: digestOfArchive(archive),
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Fatalf("IsGateable(%q) = false after startup apply, want true", testEchoTool)
	}

	// The operator tightens the policy: the switch goes back to its (safe)
	// default by leaving it out entirely.
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath, requireSignature: boolPtr(true)}, cacheSetting)

	_, err := f.run("reload")
	if err == nil {
		t.Fatal("plugins reload error = nil, want an error: the remote policy in the config is not the one this process uses")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("plugins reload error = %v, want it to say serve must be restarted for a remote-policy change", err)
	}
	if !strings.Contains(err.Error(), "allow_insecure_sources") {
		t.Errorf("plugins reload error = %v, want it to name the setting that changed", err)
	}
	if !strings.Contains(err.Error(), "plaintext sources refused") ||
		!strings.Contains(err.Error(), "plaintext sources allowed") {
		t.Errorf("plugins reload error = %v, want it to say what the config asks for AND what this process is doing", err)
	}
	// Refusing is not tearing down: what IS running stays exactly as it was.
	if !toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = false, want true: a refused reload must leave the running deployment alone", testEchoTool)
	}
}

// TestPluginsReloadRefusesAMovedPluginCache is the other half of the same
// freeze, and the quieter one: a reload after "plugins.cache" moved would keep
// filing downloads under the OLD directory, with the new one sitting empty and
// nothing on screen saying which is in use.
func TestPluginsReloadRefusesAMovedPluginCache(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	oldCache := filepath.Join(f.dir, "plugin-cache")
	newCache := filepath.Join(f.dir, "plugin-cache-moved")
	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)},
		fmt.Sprintf("\"cache\": %s", jsonString(oldCache)))
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)},
		fmt.Sprintf("\"cache\": %s", jsonString(newCache)))

	_, err := f.run("reload")
	if err == nil {
		t.Fatal("plugins reload error = nil, want an error: this process is still filing downloads under the old cache")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("plugins reload error = %v, want it to say serve must be restarted for a remote-policy change", err)
	}
	if !strings.Contains(err.Error(), "plugins.cache") {
		t.Errorf("plugins reload error = %v, want it to name the setting that changed", err)
	}
	if !strings.Contains(err.Error(), oldCache) || !strings.Contains(err.Error(), newCache) {
		t.Errorf("plugins reload error = %v, want it to name both the cache in use (%s) and the one configured (%s)",
			err, oldCache, newCache)
	}
	// Nothing was converged: the entry the manifest still declares is mounted.
	if !toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = false, want true: a refused reload must leave the running deployment alone", testEchoTool)
	}
}

// TestPluginsReloadConvergesWhenTheRemotePolicyIsUnchanged is the check's other
// side: with the same cache and the same switch in the config as the running
// Loader was built with, the policies compare equal and the reload goes
// through. Without this, "refuse whenever a cache is configured" would pass
// both tests above and break every reload of a deployment that fetches.
func TestPluginsReloadConvergesWhenTheRemotePolicyIsUnchanged(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	remoteSettings := []string{
		fmt.Sprintf("\"cache\": %s", jsonString(filepath.Join(f.dir, "plugin-cache"))),
		`"allow_insecure_sources": true`,
	}
	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)}, remoteSettings...)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Fatalf("IsGateable(%q) = false after startup apply, want true", testEchoTool)
	}

	// Only the manifest changes; the config's remote policy is rewritten
	// identically, which is what an operator editing something else does.
	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)}, remoteSettings...)
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}})

	if _, err := f.run("reload"); err != nil {
		t.Fatalf("plugins reload error = %v, want nil: the remote policy did not change", err)
	}
	if toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = true after reload, want false: the disabled entry must be unmounted", testEchoTool)
	}
}

// TestAssemblePluginsFetchesUnderTheConfiguredByteCap pins the pass-through
// from "plugins.fetch" to the loader's fetch bounds, which nothing else does:
// the loader's own tests build their RemoteConfig by hand, and the assembly
// tests above only check that a package mounted. A hard-coded 32 MiB here
// would pass every one of them.
//
// One byte is a cap no real artifact fits under, so an entry that would
// otherwise mount fails on the size limit -- and says so where an operator
// looks. Startup itself still succeeds: one unfetchable entry does not stop
// serve.
func TestAssemblePluginsFetchesUnderTheConfiguredByteCap(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	priv, keyringPath := f.newKeyring("keyring.json")
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.signPackage("staging", priv)
	archive := f.archivePackage("staging")
	srv := serveArchive(t, archive)
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath, requireSignature: boolPtr(true)},
		fmt.Sprintf("\"cache\": %s", jsonString(filepath.Join(f.dir, "plugin-cache"))),
		`"allow_insecure_sources": true`,
		`"fetch": {"max_bytes": 1}`)
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: srv.URL + "/echo.tgz", enabled: true,
		tools: []string{testEchoTool}, digest: digestOfArchive(archive),
	})

	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil: an unfetchable entry must not stop startup", err)
	}

	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if !strings.Contains(out, "failed") {
		t.Fatalf("plugins status output = %q, want the entry to have failed under a 1 byte download cap", out)
	}
	if !strings.Contains(out, "1 byte limit") {
		t.Errorf("plugins status output = %q, want it to report the CONFIGURED byte cap as the reason", out)
	}
	if toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = true, want false: nothing may mount from a download that was refused", testEchoTool)
	}
}

// --- agent plugins install --------------------------------------------------

// writeInstallConfig writes the fixture's agent.json with a plugin cache
// configured (which `agent plugins install` requires) and plaintext sources
// allowed, since serveArchive and serveNotFound only ever serve http. A test
// that wants a DIFFERENT remote policy — no cache, no insecure sources —
// calls writeSignatureConfig directly instead, the same way the assembly
// tests above do.
func (f *pluginFixture) writeInstallConfig(policy signaturePolicy, cacheDir string) {
	f.t.Helper()

	f.writeSignatureConfig(30_000, policy,
		fmt.Sprintf("\"cache\": %s", jsonString(cacheDir)),
		`"allow_insecure_sources": true`)
}

// readDeployment re-reads and parses the fixture's plugins.json from disk —
// not what a test just wrote in memory, but what `agent plugins install`
// actually left behind.
func (f *pluginFixture) readDeployment() manifest.Deployment {
	f.t.Helper()

	data, err := os.ReadFile(f.manifestPath)
	if err != nil {
		f.t.Fatalf("read plugins.json %s: %v", f.manifestPath, err)
	}
	dep, err := manifest.ParseDeployment(data)
	if err != nil {
		f.t.Fatalf("parse plugins.json %s: %v", f.manifestPath, err)
	}
	return dep
}

// requireEntry returns the entry named name from dep, failing the test
// immediately if none exists.
func (f *pluginFixture) requireEntry(dep manifest.Deployment, name string) manifest.Entry {
	f.t.Helper()

	for _, entry := range dep.Plugins {
		if entry.Name == name {
			return entry
		}
	}
	f.t.Fatalf("plugins.json has no entry named %q (have: %v)", name, dep.Plugins)
	return manifest.Entry{}
}

// TestPluginsInstallLeavesPluginsJSONByteForByteUnchangedWhenSignatureVerificationFails
// is rule 1, the core invariant of `agent plugins install`: nothing is ever
// written to plugins.json before manifest.LoadPackage's signature check has
// passed. The fetched package here is signed under the keyring's trusted key
// ID by a DIFFERENT, untrusted key pair, so LoadPackage refuses it — and
// plugins.json must come out of the call exactly as it went in, not merely
// "no error was able to change it", the actual bytes.
func TestPluginsInstallLeavesPluginsJSONByteForByteUnchangedWhenSignatureVerificationFails(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	// The keyring trusts ONE key (registered under testPluginKeyID). The
	// package below is signed under the SAME key id, by a DIFFERENT, untrusted
	// key pair — a signature that is present, well-formed, and refused, not
	// merely absent. archivePackage requires a plugin.sig file to exist
	// (it is one of the three files fetch.Unpack insists an archive holds), so
	// an untrusted-but-present signature is how this test reaches LoadPackage's
	// verification failure through the full fetch -> cache -> load pipeline.
	_, keyringPath := f.newKeyring("keyring.json")
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
	_, untrustedPriv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	f.signPackage("staging", untrustedPriv)
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	srv := serveArchive(t, archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeInstallConfig(signaturePolicy{keyring: keyringPath, requireSignature: boolPtr(true)}, cacheDir)
	f.writeManifest()

	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json before install: %v", err)
	}

	out, err := f.run("install", srv.URL+"/echo.tgz", "--digest", digest)
	if err == nil {
		t.Fatalf("plugins install output = %q, error = nil, want an error: the package is signed by a key "+
			"this deployment's keyring does not trust", out)
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("plugins install error = %v, want it to name the signature verification failure", err)
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json after install: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("plugins.json changed after a failed signature verification:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestPluginsInstallLeavesNothingBehindOnADigestMismatch is rule 2: a fetched
// package whose bytes do not match --digest never reaches plugins.json, and
// (since fetch.Fetch never returns unverified bytes to its caller)
// remote.Cache.Put is never even called, so nothing is left in the plugin
// cache directory either.
func TestPluginsInstallLeavesNothingBehindOnADigestMismatch(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
	f.signPackageWithAnyKey("staging")
	archive := f.archivePackage("staging")
	srv := serveArchive(t, archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeInstallConfig(signaturePolicy{requireSignature: boolPtr(false)}, cacheDir)
	f.writeManifest()

	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json before install: %v", err)
	}

	wrongDigest := digestOfArchive([]byte("not the archive that was actually served"))
	out, err := f.run("install", srv.URL+"/echo.tgz", "--digest", wrongDigest)
	if err == nil {
		t.Fatalf("plugins install output = %q, error = nil, want an error: the digest does not match the "+
			"fetched bytes", out)
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("plugins install error = %v, want it to report a digest mismatch", err)
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json after install: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("plugins.json changed after a digest mismatch:\nbefore=%s\nafter=%s", before, after)
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("read plugin cache dir %s: %v", cacheDir, err)
	}
	if len(entries) != 0 {
		t.Errorf("plugin cache dir %s has %d entries after a digest mismatch, want 0: nothing may reach the "+
			"cache from a fetch that was never verified", cacheDir, len(entries))
	}
}

// TestPluginsInstallRefusesAMissingDigest is rule 3: a remote entry always
// requires a digest, and install offers no "install now, verify later" hatch
// — the command refuses before ever resolving a config or building a request.
func TestPluginsInstallRefusesAMissingDigest(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeInstallConfig(signaturePolicy{requireSignature: boolPtr(false)}, cacheDir)
	f.writeManifest()

	out, err := f.run("install", "https://example.invalid/echo.tgz")
	if err == nil {
		t.Fatalf("plugins install output = %q, error = nil, want an error: --digest is required", out)
	}
	if !strings.Contains(err.Error(), "--digest") {
		t.Errorf("plugins install error = %v, want it to name --digest", err)
	}
}

// TestPluginsInstallWithoutGrantRegistersWithNoCapabilitiesAndStaysDisabled is
// rule 4: with no --grant, the written entry has empty capabilities AND
// "enabled": false, and the command's own output tells the operator plainly
// that the plugin is registered but not authorized, naming both `agent
// plugins grant` and `agent plugins reload` as the remaining steps.
func TestPluginsInstallWithoutGrantRegistersWithNoCapabilitiesAndStaysDisabled(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log"}, []string{testEchoTool})
	f.signPackageWithAnyKey("staging")
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	srv := serveArchive(t, archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeInstallConfig(signaturePolicy{requireSignature: boolPtr(false)}, cacheDir)
	f.writeManifest()

	out, err := f.run("install", srv.URL+"/echo.tgz", "--digest", digest)
	if err != nil {
		t.Fatalf("plugins install error = %v, want nil", err)
	}
	if !strings.Contains(out, "NOT authorized") {
		t.Errorf("plugins install output = %q, want it to say the entry is registered but NOT authorized", out)
	}
	if !strings.Contains(out, "agent plugins grant") || !strings.Contains(out, "agent plugins reload") {
		t.Errorf("plugins install output = %q, want it to name both `agent plugins grant` and `agent plugins "+
			"reload` as the remaining steps", out)
	}

	entry := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if entry.Enabled {
		t.Errorf("entry.Enabled = true, want false: install must never authorize a plugin to run")
	}
	if len(entry.Grant.Capabilities) != 0 {
		t.Errorf("entry.Grant.Capabilities = %v, want empty: --grant was not given", entry.Grant.Capabilities)
	}
}

// TestPluginsInstallRefusesAGrantForAnUndeclaredCapability is rule 5:
// granting a capability the plugin's own plugin.json never declared is a
// config error, not generosity, and it must be refused by name — with
// nothing written to plugins.json, since the refusal happens after
// verification but before DraftEntry/AddEntry/WriteDeployment ever run.
func TestPluginsInstallRefusesAGrantForAnUndeclaredCapability(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log"}, []string{testEchoTool})
	f.signPackageWithAnyKey("staging")
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	srv := serveArchive(t, archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeInstallConfig(signaturePolicy{requireSignature: boolPtr(false)}, cacheDir)
	f.writeManifest()

	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json before install: %v", err)
	}

	out, err := f.run("install", srv.URL+"/echo.tgz", "--digest", digest, "--grant", "http")
	if err == nil {
		t.Fatalf("plugins install output = %q, error = nil, want an error: the plugin only declares \"log\"", out)
	}
	// F8: "http" alone is a weak assertion -- every error in this command
	// that names the source URL also contains "http". Assert on the
	// distinguishing fragment instead, so this test cannot pass on the
	// wrong error for the right-looking reason.
	if !strings.Contains(err.Error(), "does not declare") {
		t.Errorf("plugins install error = %v, want it to say the plugin does not declare the capability", err)
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("plugins install error = %v, want it to name the undeclared capability %q", err, "http")
	}
	if !strings.Contains(err.Error(), testEchoPlugin) {
		t.Errorf("plugins install error = %v, want it to name the plugin %q", err, testEchoPlugin)
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json after install: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("plugins.json changed after a refused --grant:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestPluginsInstallRefusesAPartialGrantThatCanNeverMount is F1 [blocking]:
// --grant naming a STRICT SUBSET of what the plugin declares used to be
// accepted (resolveInstallGrants only checked that every granted capability
// was declared, never the reverse), producing an entry
// manifest.reconcileCapabilities (assemble.go) can never accept -- install
// reported success, `agent serve` started fine, and the plugin sat silently
// in `failed`, discoverable only by reading `agent plugins status`. The
// plugin here declares log AND http; --grant names only log, and must be
// refused by naming the missing capability, with plugins.json left
// byte-for-byte unchanged -- the same contract `agent plugins grant`'s
// resolveGrantCapabilities already enforces (equal sets, not a subset).
func TestPluginsInstallRefusesAPartialGrantThatCanNeverMount(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log", "http"}, []string{testEchoTool})
	f.signPackageWithAnyKey("staging")
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	srv := serveArchive(t, archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeInstallConfig(signaturePolicy{requireSignature: boolPtr(false)}, cacheDir)
	f.writeManifest()

	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json before install: %v", err)
	}

	out, err := f.run("install", srv.URL+"/echo.tgz", "--digest", digest, "--grant", "log")
	if err == nil {
		t.Fatalf("plugins install output = %q, error = nil, want an error: the plugin declares log AND http, "+
			"--grant only named log", out)
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("plugins install error = %v, want it to name the missing capability %q", err, "http")
	}
	if !strings.Contains(err.Error(), "partial") {
		t.Errorf("plugins install error = %v, want it to say a partial grant produces an entry that can "+
			"never load", err)
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json after install: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("plugins.json changed after a refused partial grant:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestPluginsInstallWithACompleteGrantAuthorizesTheEntryImmediately is D9: a
// non-empty --grant is an authorization decision, not a draft of one --
// naming the plugin's COMPLETE declared capability set must set
// Enabled=true AND GrantStated=true in the same write, or the written entry
// would be enabled with no capabilities recorded at all (MarshalDeployment
// omits the whole grant block when GrantStated is false), which
// reconcileCapabilities refuses just as surely as F1's partial grant did.
// The success output must say so too: an entry this call just enabled
// cannot be reported as "NOT authorized".
func TestPluginsInstallWithACompleteGrantAuthorizesTheEntryImmediately(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log", "http"}, []string{testEchoTool})
	f.signPackageWithAnyKey("staging")
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	srv := serveArchive(t, archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeInstallConfig(signaturePolicy{requireSignature: boolPtr(false)}, cacheDir)
	f.writeManifest()

	out, err := f.run("install", srv.URL+"/echo.tgz", "--digest", digest, "--grant", "log,http")
	if err != nil {
		t.Fatalf("plugins install error = %v, want nil", err)
	}
	if strings.Contains(out, "NOT authorized") {
		t.Errorf("plugins install output = %q, want it NOT to say NOT authorized: a complete --grant just "+
			"enabled this entry", out)
	}
	if !strings.Contains(out, "AND authorized") {
		t.Errorf("plugins install output = %q, want it to say the entry is registered AND authorized", out)
	}
	if strings.Contains(out, "agent plugins grant") {
		t.Errorf("plugins install output = %q, want it NOT to tell the operator to run `agent plugins grant`: "+
			"the entry is already authorized", out)
	}

	entry := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if !entry.Enabled {
		t.Errorf("entry.Enabled = false, want true: --grant named the plugin's complete capability set (D9)")
	}
	if !entry.GrantStated {
		t.Errorf("entry.GrantStated = false, want true: Enabled=true with GrantStated=false would write an " +
			"authorized entry with no capabilities on disk, which reconcileCapabilities refuses")
	}
	wantCaps := []string{"log", "http"}
	if !slices.Equal(entry.Grant.Capabilities, wantCaps) {
		t.Errorf("entry.Grant.Capabilities = %v, want %v", entry.Grant.Capabilities, wantCaps)
	}
}

// TestPluginsInstallRefusesAnExplicitlyEmptyGrant is F2: the doc comment on
// resolveInstallGrants states that an empty --grant item is refused by name
// rather than silently dropped, but the code used to special-case the
// WHOLE-FLAG-empty case (an explicitly passed "--grant" or "--grant   ")
// and return nil, nil for it -- installing successfully with no capabilities
// granted and no error, contradicting the doc comment and (for a plugin that
// declares required capabilities, as here) letting a scripted
// `--grant "$CAPS"` with an unset CAPS install unauthorized-and-silent
// rather than fail loudly. An OMITTED --grant must still behave exactly as
// before (rule 4 covers that; this test only covers the flag being present
// and empty).
func TestPluginsInstallRefusesAnExplicitlyEmptyGrant(t *testing.T) {
	for _, grantValue := range []string{"", "   "} {
		t.Run(fmt.Sprintf("grant=%q", grantValue), func(t *testing.T) {
			f := newPluginFixture(t, 30_000)
			f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log"}, []string{testEchoTool})
			f.signPackageWithAnyKey("staging")
			archive := f.archivePackage("staging")
			digest := digestOfArchive(archive)
			srv := serveArchive(t, archive)
			cacheDir := filepath.Join(f.dir, "plugin-cache")
			f.writeInstallConfig(signaturePolicy{requireSignature: boolPtr(false)}, cacheDir)
			f.writeManifest()

			before, err := os.ReadFile(f.manifestPath)
			if err != nil {
				t.Fatalf("read plugins.json before install: %v", err)
			}

			out, err := f.run("install", srv.URL+"/echo.tgz", "--digest", digest, "--grant", grantValue)
			if err == nil {
				t.Fatalf("plugins install output = %q, error = nil, want an error: --grant was explicitly "+
					"given but empty", out)
			}
			if !strings.Contains(err.Error(), "--grant") {
				t.Errorf("plugins install error = %v, want it to name --grant", err)
			}

			after, err := os.ReadFile(f.manifestPath)
			if err != nil {
				t.Fatalf("read plugins.json after install: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("plugins.json changed after a refused empty --grant:\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

// TestPluginsInstallRefusesADuplicateName is rule 6, inherited from
// manifest.AddEntry (Task 2): an entry already named in plugins.json is left
// alone, and the operator is pointed at `agent plugins grant` or a manual
// edit instead of having their existing authorization decision silently
// erased.
func TestPluginsInstallRefusesADuplicateName(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
	f.signPackageWithAnyKey("staging")
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	srv := serveArchive(t, archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeInstallConfig(signaturePolicy{requireSignature: boolPtr(false)}, cacheDir)
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "already-here", enabled: false, tools: []string{testEchoTool},
	})

	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json before install: %v", err)
	}

	out, err := f.run("install", srv.URL+"/echo.tgz", "--digest", digest)
	if err == nil {
		t.Fatalf("plugins install output = %q, error = nil, want an error: %q is already in plugins.json",
			out, testEchoPlugin)
	}
	if !strings.Contains(err.Error(), testEchoPlugin) {
		t.Errorf("plugins install error = %v, want it to name the existing entry %q", err, testEchoPlugin)
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json after install: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("plugins.json changed after a refused duplicate install:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestPluginsInstallRefusesAnUnconfiguredManifest is rule 7: install needs a
// desired-state manifest to register the new entry into, and refuses,
// naming the setting, rather than inventing a location of its own.
func TestPluginsInstallRefusesAnUnconfiguredManifest(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(configPath, []byte(`{"storage": {"driver": "memory"}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out := &bytes.Buffer{}
	err := Execute(app.New(), out, []string{"plugins", "install", "https://example.invalid/echo.tgz",
		"--digest", digestOfArchive([]byte("x")), "--config", configPath})
	if err == nil {
		t.Fatalf("plugins install output = %q, error = nil, want an error: no plugins.manifest is configured",
			out.String())
	}
	if !strings.Contains(err.Error(), "plugins.manifest") {
		t.Errorf("plugins install error = %v, want it to name plugins.manifest", err)
	}
}

// TestPluginsInstallRefusesAPlaintextSourceByDefault is rule 8 (added by the
// brief, not the plan): a plaintext http:// source is refused BEFORE
// fetching, the same way (*loader.Loader).remoteDir refuses one already
// deployed. The source names a domain ("example.invalid") that will never
// resolve, so if the refusal were checked too late — after a request was
// already attempted — this test would fail on a DNS error instead of the
// expected message, which is exactly how a regression here would be caught.
func TestPluginsInstallRefusesAPlaintextSourceByDefault(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	// Deliberately no "allow_insecure_sources" -- the safe default.
	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)},
		fmt.Sprintf("\"cache\": %s", jsonString(cacheDir)))
	f.writeManifest()

	source := "http://example.invalid/echo.tgz"
	out, err := f.run("install", source, "--digest", digestOfArchive([]byte("x")))
	if err == nil {
		t.Fatalf("plugins install output = %q, error = nil, want an error: plaintext is refused by default", out)
	}
	if !strings.Contains(err.Error(), source) {
		t.Errorf("plugins install error = %v, want it to name the offending URL %s", err, source)
	}
	if !strings.Contains(err.Error(), "allow_insecure_sources") {
		t.Errorf("plugins install error = %v, want it to name the switch that turns plaintext on", err)
	}
}

// TestPluginsInstallRefusesAnUnconfiguredCache is rule 9 (added by the brief,
// not the plan): a remote package has to be written somewhere, and install
// will not pick a location on the operator's behalf any more than
// (*loader.Loader).remoteDir does for an already-deployed entry.
func TestPluginsInstallRefusesAnUnconfiguredCache(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	// Deliberately no "cache" key at all.
	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)})
	f.writeManifest()

	source := "https://example.invalid/echo.tgz"
	out, err := f.run("install", source, "--digest", digestOfArchive([]byte("x")))
	if err == nil {
		t.Fatalf("plugins install output = %q, error = nil, want an error: no plugins.cache is configured", out)
	}
	if !strings.Contains(err.Error(), "plugins.cache") {
		t.Errorf("plugins install error = %v, want it to name plugins.cache", err)
	}
	if !strings.Contains(err.Error(), source) {
		t.Errorf("plugins install error = %v, want it to name the source %s", err, source)
	}
}

// TestPluginsInstallNamesTheRemedyWhenPluginsManifestDoesNotExist is D10: not
// bootstrapping a missing plugins.json is the right call (reusing
// readPluginDeployment keeps install agreeing with status and reload on what
// "the deployment manifest" means), but the resulting first-run error used to
// be a bare "open ...: no such file", naming only the path. install is
// exactly the command an operator reaches for when no deployment exists yet,
// so its error has to say that install will not create the file itself and
// what minimal content starts one.
func TestPluginsInstallNamesTheRemedyWhenPluginsManifestDoesNotExist(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeInstallConfig(signaturePolicy{requireSignature: boolPtr(false)}, cacheDir)
	// Deliberately no f.writeManifest() call: cfg.Plugins.Manifest is
	// configured (writeInstallConfig sets it via writeSignatureConfig), but
	// nothing has ever been written to that path.
	if _, err := os.Stat(f.manifestPath); err == nil {
		t.Fatalf("test setup is wrong: %s must not exist yet", f.manifestPath)
	}

	out, err := f.run("install", "https://example.invalid/echo.tgz", "--digest", digestOfArchive([]byte("x")))
	if err == nil {
		t.Fatalf("plugins install output = %q, error = nil, want an error: plugins.json does not exist yet", out)
	}
	if !strings.Contains(err.Error(), f.manifestPath) {
		t.Errorf("plugins install error = %v, want it to name the path %q", err, f.manifestPath)
	}
	if !strings.Contains(err.Error(), "will not create") {
		t.Errorf("plugins install error = %v, want it to say install will not create the file itself", err)
	}
	if !strings.Contains(err.Error(), `{"plugins": []}`) {
		t.Errorf(`plugins install error = %v, want it to name {"plugins": []} as the minimal starting content`, err)
	}
}

// TestPluginsInstallRefusesAConcurrentEditDuringTheDownload is F5: the
// deployment is snapshotted before the download and a document built from
// that snapshot is written after it, with no compare-and-swap in between --
// a window that spans an entire artifact download, seconds to minutes under
// the configured timeout and byte cap. A server handler that mutates
// plugins.json itself, from inside the request the fetch is blocked on,
// stands in for an operator (or another `agent plugins` invocation) editing
// the file while this install is still downloading -- reproduced
// deterministically instead of racing a real clock. install must refuse
// rather than silently rewrite the file from its now-stale snapshot, and the
// concurrent edit must survive exactly as written.
func TestPluginsInstallRefusesAConcurrentEditDuringTheDownload(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
	f.signPackageWithAnyKey("staging")
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeInstallConfig(signaturePolicy{requireSignature: boolPtr(false)}, cacheDir)
	f.writeManifest()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Stand in for a concurrent edit landing WHILE this download is in
		// flight, from inside the very request the fetch is blocked on.
		concurrent := manifest.Deployment{Plugins: []manifest.Entry{{
			Name:    "concurrently-installed-plugin",
			Source:  "elsewhere",
			Enabled: true,
			Tools:   []manifest.ToolAccept{{Name: testEchoTool}},
		}}}
		if err := manifest.WriteDeployment(f.manifestPath, concurrent); err != nil {
			t.Errorf("write concurrent edit to plugins.json: %v", err)
		}
		if _, err := w.Write(archive); err != nil {
			t.Errorf("write archive to client: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	out, err := f.run("install", srv.URL+"/echo.tgz", "--digest", digest)
	if err == nil {
		t.Fatalf("plugins install output = %q, error = nil, want an error: plugins.json changed while this "+
			"package was downloading", out)
	}
	if !strings.Contains(err.Error(), f.manifestPath) {
		t.Errorf("plugins install error = %v, want it to name the manifest path %q", err, f.manifestPath)
	}
	if !strings.Contains(err.Error(), "changed") {
		t.Errorf("plugins install error = %v, want it to say the manifest changed underneath it", err)
	}

	after := f.readDeployment()
	if len(after.Plugins) != 1 || after.Plugins[0].Name != "concurrently-installed-plugin" {
		t.Fatalf("plugins.json after install = %+v, want ONLY the concurrent edit still present -- install "+
			"must refuse rather than silently revert it", after.Plugins)
	}
}

// TestPluginsInstallAndServeAgreeOnCacheAndTrustSettings is the reuse
// invariant the brief calls out by name: install resolves its cache, HTTP
// client, fetch/unpack limits and remote-source policy through
// resolvePluginRemote, and its trust set through resolvePluginKeyring — the
// same two functions newPluginLoader calls to build the loader `agent serve`
// runs. If install derived its own copies of either, a package could install
// cleanly through this command and then have serve refuse to fetch or load
// it, with the contradiction invisible in both commands' output.
//
// The fixture declares TWO capabilities and --grant names both: a
// single-capability fixture would make "the granted set" and "the declared
// set" trivially identical no matter which direction resolveInstallGrants
// checked, so it could never tell F1's fix (require EQUAL sets) apart from
// the original one-directional (subset only) check — this is the review's
// F1 finding about the original version of this test. With two capabilities,
// the happy path only succeeds because --grant genuinely names the complete
// declared set.
//
// This test installs the package with a complete --grant, which (D9) both
// registers AND authorizes the entry in the same step, then also runs
// `agent plugins grant` (Task 4) — a re-grant of an already-authorized entry
// — to prove grant's resolvers agree with install's and serve's too, not
// only install's. It then runs the SAME assembly `agent serve` runs, against
// the SAME config. That assembly re-resolves the cache and the trust set
// from scratch — it only succeeds, and only mounts the plugin, if its
// resolution of both agrees with what install already used.
func TestPluginsInstallAndServeAgreeOnCacheAndTrustSettings(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	priv, keyringPath := f.newKeyring("keyring.json")
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log", "http"}, []string{testEchoTool})
	f.signPackage("staging", priv)
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	srv := serveArchive(t, archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeInstallConfig(signaturePolicy{keyring: keyringPath, requireSignature: boolPtr(true)}, cacheDir)
	f.writeManifest()

	if _, err := f.run("install", srv.URL+"/echo.tgz", "--digest", digest, "--grant", "log,http"); err != nil {
		t.Fatalf("plugins install error = %v, want nil", err)
	}

	wantCaps := []string{"log", "http"}
	entry := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if !entry.Enabled {
		t.Fatalf("entry.Enabled = false right after install with a complete --grant, want true (D9)")
	}
	if !entry.GrantStated {
		t.Fatalf("entry.GrantStated = false right after install with a complete --grant, want true (D9): " +
			"MarshalDeployment omits the whole grant block unless GrantStated is also set")
	}
	if !slices.Equal(entry.Grant.Capabilities, wantCaps) {
		t.Fatalf("entry.Grant.Capabilities = %v, want %v right after install", entry.Grant.Capabilities, wantCaps)
	}

	// install already filed the package under the path resolvePluginRemote
	// resolves for this exact "cache" setting. If serve's assembly resolved a
	// DIFFERENT path, this file would not be sitting here.
	cached := filepath.Join(cacheDir, "sha256", strings.TrimPrefix(digest, "sha256:"))
	for _, name := range remotePluginPackageFiles {
		if _, err := os.Stat(filepath.Join(cached, name)); err != nil {
			t.Errorf("stat %s in the cache install populated: %v", name, err)
		}
	}

	// `agent plugins grant` (Task 4) resolves the SAME plugin package (from
	// the SAME cache, under the SAME trust set) install did, re-checking
	// "log,http" against the plugin's own declaration on an entry install
	// already authorized -- a legal re-grant, and proof grant's resolvers
	// agree too, not only install's.
	if _, err := f.run("grant", testEchoPlugin, "--capabilities", "log,http"); err != nil {
		t.Fatalf("plugins grant error = %v, want nil", err)
	}
	granted := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if !granted.Enabled || !granted.GrantStated || !slices.Equal(granted.Grant.Capabilities, wantCaps) {
		t.Fatalf("entry after grant = %+v, want Enabled=true GrantStated=true Grant.Capabilities=%v", granted, wantCaps)
	}

	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil: serve must resolve the same cache and trust set "+
			"install used", err)
	}
	if !toolauth.IsGateable(testEchoTool) {
		t.Errorf("IsGateable(%q) = false after serve assembly, want true: serve should have mounted the "+
			"plugin install registered", testEchoTool)
	}
	out, err := f.run("status")
	if err != nil {
		t.Fatalf("plugins status error = %v, want nil", err)
	}
	if !strings.Contains(out, "loaded") || !strings.Contains(out, testEchoPlugin) {
		t.Fatalf("plugins status output = %q, want the entry mounted", out)
	}
}

// --- agent plugins grant ----------------------------------------------------

// TestPluginsGrantAuthorizesAnEntryWithItsDeclaredCapabilities is rule 1 (and
// rule 6's second half — a grant round-trips with GrantStated true): grant
// makes the entry Enabled, records GrantStated, carries the named
// capabilities and the named hosts/paths — all read back from disk, not
// merely from the command's own claim.
func TestPluginsGrantAuthorizesAnEntryWithItsDeclaredCapabilities(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", []string{"log", "http"}, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})

	before := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if before.GrantStated {
		t.Fatalf("test setup is wrong: the entry must start with GrantStated false (rule 6's first half)")
	}

	out, err := f.run("grant", testEchoPlugin,
		"--capabilities", "log,http", "--allowed-hosts", "example.com", "--allowed-paths", "/data")
	if err != nil {
		t.Fatalf("plugins grant error = %v, want nil", err)
	}
	if !strings.Contains(out, "agent plugins reload") {
		t.Errorf("plugins grant output = %q, want it to say changes take effect on `agent plugins reload`", out)
	}

	entry := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if !entry.Enabled {
		t.Errorf("entry.Enabled = false, want true after grant")
	}
	if !entry.GrantStated {
		t.Errorf("entry.GrantStated = false, want true after grant (rule 6's second half)")
	}
	wantCaps := []string{"log", "http"}
	if !slices.Equal(entry.Grant.Capabilities, wantCaps) {
		t.Errorf("entry.Grant.Capabilities = %v, want %v", entry.Grant.Capabilities, wantCaps)
	}
	if want := []string{"example.com"}; !slices.Equal(entry.Grant.AllowedHosts, want) {
		t.Errorf("entry.Grant.AllowedHosts = %v, want %v", entry.Grant.AllowedHosts, want)
	}
	if want := []string{"/data"}; !slices.Equal(entry.Grant.AllowedPaths, want) {
		t.Errorf("entry.Grant.AllowedPaths = %v, want %v", entry.Grant.AllowedPaths, want)
	}
}

// TestPluginsGrantAllowsAPureComputePluginWithNoCapabilities is the whole
// motivation for GrantStated made concrete: a plugin that declares zero
// capabilities is a legitimate target of an explicit, empty grant — an
// operator decided it needs nothing and is still allowed to run — and must
// come out authorized (Enabled true, GrantStated true), not indistinguishable
// from one nobody has ever decided about.
func TestPluginsGrantAllowsAPureComputePluginWithNoCapabilities(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})

	if _, err := f.run("grant", testEchoPlugin); err != nil {
		t.Fatalf("plugins grant error = %v, want nil: a pure-compute plugin may be granted zero capabilities", err)
	}

	entry := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if !entry.Enabled {
		t.Errorf("entry.Enabled = false, want true after grant")
	}
	if !entry.GrantStated {
		t.Errorf("entry.GrantStated = false, want true after grant")
	}
	if len(entry.Grant.Capabilities) != 0 {
		t.Errorf("entry.Grant.Capabilities = %v, want empty", entry.Grant.Capabilities)
	}
}

// TestPluginsGrantRefusesAnUndeclaredCapability is rule 2: naming a
// capability the plugin itself never declared in plugin.json is a config
// error, not generosity, refused by name with nothing written to
// plugins.json.
func TestPluginsGrantRefusesAnUndeclaredCapability(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", []string{"log"}, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json before grant: %v", err)
	}

	out, err := f.run("grant", testEchoPlugin, "--capabilities", "log,http")
	if err == nil {
		t.Fatalf("plugins grant output = %q, error = nil, want an error: the plugin only declares \"log\"", out)
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("plugins grant error = %v, want it to name the undeclared capability %q", err, "http")
	}
	if !strings.Contains(err.Error(), testEchoPlugin) {
		t.Errorf("plugins grant error = %v, want it to name the plugin %q", err, testEchoPlugin)
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json after grant: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("plugins.json changed after a refused grant:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestPluginsGrantRefusesAPartialGrantMissingADeclaredCapability is the Task
// 3 review's correction, folded into Task 4: manifest.reconcileCapabilities
// (assemble.go) refuses any entry whose grant does not cover EVERY capability
// the plugin declares, so granting a strict subset would write an entry that
// can never load — discoverable only as a StateFailed row after the next
// reload. grant refuses it up front instead, naming the missing capability,
// with nothing written to plugins.json.
func TestPluginsGrantRefusesAPartialGrantMissingADeclaredCapability(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", []string{"log", "http"}, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json before grant: %v", err)
	}

	out, err := f.run("grant", testEchoPlugin, "--capabilities", "log")
	if err == nil {
		t.Fatalf("plugins grant output = %q, error = nil, want an error: the plugin also declares \"http\", "+
			"which a partial grant would leave ungranted", out)
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("plugins grant error = %v, want it to name the missing declared capability %q", err, "http")
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json after grant: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("plugins.json changed after a refused partial grant:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestPluginsGrantRefusesAnUnknownEntry pins findPluginEntry's error path:
// grant cannot authorize an entry that was never registered, and the error
// names both the requested name and (when there is one) what does exist.
func TestPluginsGrantRefusesAnUnknownEntry(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writeManifest()

	_, err := f.run("grant", "does-not-exist", "--capabilities", "log")
	if err == nil {
		t.Fatal("plugins grant error = nil, want an error: no such entry")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("plugins grant error = %v, want it to name the unknown entry", err)
	}
}

// --- agent plugins deny ------------------------------------------------------

// TestPluginsDenyRevokesAuthorizationButKeepsRegistration is rule 3: deny
// flips Enabled false and empties capabilities, but keeps GrantStated true (a
// decision WAS made) and leaves source, digest and tools untouched.
func TestPluginsDenyRevokesAuthorizationButKeepsRegistration(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", []string{"log"}, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: true, capabilities: []string{"log"}, tools: []string{testEchoTool},
	})
	before := f.requireEntry(f.readDeployment(), testEchoPlugin)

	out, err := f.run("deny", testEchoPlugin)
	if err != nil {
		t.Fatalf("plugins deny error = %v, want nil", err)
	}
	if !strings.Contains(out, "agent plugins reload") {
		t.Errorf("plugins deny output = %q, want it to say changes take effect on `agent plugins reload`", out)
	}

	after := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if after.Enabled {
		t.Errorf("entry.Enabled = true, want false after deny")
	}
	if len(after.Grant.Capabilities) != 0 {
		t.Errorf("entry.Grant.Capabilities = %v, want empty after deny", after.Grant.Capabilities)
	}
	if !after.GrantStated {
		t.Errorf("entry.GrantStated = false, want true after deny: a decision WAS made")
	}
	if after.Source != before.Source {
		t.Errorf("entry.Source = %q, want unchanged %q", after.Source, before.Source)
	}
	if after.Digest != before.Digest {
		t.Errorf("entry.Digest = %q, want unchanged %q", after.Digest, before.Digest)
	}
	if len(after.Tools) != len(before.Tools) {
		t.Fatalf("entry.Tools = %+v, want unchanged %+v", after.Tools, before.Tools)
	}
	for i := range before.Tools {
		if after.Tools[i] != before.Tools[i] {
			t.Errorf("entry.Tools[%d] = %+v, want unchanged %+v", i, after.Tools[i], before.Tools[i])
		}
	}
}

// TestPluginsDenyDoesNotNeedThePackageOnDisk pins the design decision that
// deny never loads the plugin package and never touches a loader: unlike
// grant, it only edits plugins.json, so it must succeed even when the
// package files that install/grant would need are entirely absent.
func TestPluginsDenyDoesNotNeedThePackageOnDisk(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	// Deliberately no writePackage call.
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: true, capabilities: []string{"log"}, tools: []string{testEchoTool},
	})

	if _, err := f.run("deny", testEchoPlugin); err != nil {
		t.Fatalf("plugins deny error = %v, want nil: deny only edits plugins.json, it never loads the package", err)
	}
}

// TestPluginsDenyRefusesAnUnknownEntry mirrors
// TestPluginsGrantRefusesAnUnknownEntry for deny's own UpdateEntry call.
func TestPluginsDenyRefusesAnUnknownEntry(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writeManifest()

	_, err := f.run("deny", "does-not-exist")
	if err == nil {
		t.Fatal("plugins deny error = nil, want an error: no such entry")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("plugins deny error = %v, want it to name the unknown entry", err)
	}
}
