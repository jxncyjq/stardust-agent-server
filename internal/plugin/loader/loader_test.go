package loader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/host"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/toolauth"
)

// The two guests these tests mount, as they describe THEMSELVES through
// abi.OpManifest (see internal/plugin/host/testdata/README.md). host.Activate
// cross-checks a Spec against that self-description, so a plugin.json this test
// writes must claim exactly these names — a rebuilt fixture that changed one of
// them has to fail here loudly rather than let a cross-check pass vacuously.
//
// Two DIFFERENT guests are needed wherever a test mounts two plugins at once:
// both the tool registry and the process-global gateable catalog refuse a
// duplicate tool name (fail-loud by panic), so two entries can never contribute
// the same tool.
const (
	echoPluginName = "legion-test-plugin"
	echoToolName   = "echo_tool"
	echoWasmFile   = "plugin.wasm"

	proxyPluginName = "legion-e2e-plugin"
	proxyToolName   = "e2e_proxy_tool"
	proxyWasmFile   = "e2e.wasm"
)

// ghostToolName is a tool a plugin.json can DECLARE but the guest does not
// provide. It is how these tests make one activation fail on its own merits:
// LoadPackage and AssembleSpec both accept it (the deployment accepts a tool the
// plugin declares), and host.Activate then refuses at the manifest cross-check.
// No test seam in production code is involved.
const ghostToolName = "ghost_tool"

// fixtureWasm reads one of the host package's committed guest binaries.
//
// It is read by relative path across package boundaries, the same way
// internal/runtime's tests already read hostcall.wasm; there is no compile-time
// link, so internal/plugin/host/testdata/README.md records that this package
// reads them too.
func fixtureWasm(t *testing.T, file string) []byte {
	t.Helper()

	path := filepath.Join("..", "host", "testdata", file)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wasm fixture %s: %v", path, err)
	}
	return data
}

// appendCustomSection returns wasm with an empty WASM custom section named name
// appended: a module that behaves identically and hashes differently. It is how
// a test produces "the operator rebuilt the plugin" — a new sha256 for the same
// plugin name and version — without needing a second toolchain.
//
// The encoding is the binary format's: section id 0x00, then the section's byte
// length, then the name as a vector (length-prefixed bytes), then the section's
// contents — one byte here, because wazero refuses to read a custom section
// whose contents are empty. All lengths are LEB128 u32s, and each fits in one
// byte here because the name is short; a longer name would need real LEB128
// encoding, so this refuses one instead of writing a malformed module.
func appendCustomSection(t *testing.T, wasm []byte, name string) []byte {
	t.Helper()

	if len(name) >= 0x80 {
		t.Fatalf("appendCustomSection: name %q is too long for the single-byte length this helper writes", name)
	}
	payload := append([]byte{byte(len(name))}, name...)
	payload = append(payload, 0x00)
	if len(payload) >= 0x80 {
		t.Fatalf("appendCustomSection: section payload of %d bytes is too long for a single-byte length", len(payload))
	}
	out := make([]byte, 0, len(wasm)+len(payload)+2)
	out = append(out, wasm...)
	out = append(out, 0x00, byte(len(payload)))
	out = append(out, payload...)
	return out
}

// pkg describes one plugin package directory a test writes to disk.
type pkg struct {
	wasm         []byte
	name         string
	version      string
	capabilities []string
	tools        []string
}

// writePackage writes a plugin package (plugin.json + plugin.wasm) into dir,
// with plugin.json's sha256 computed from the bytes actually written so
// LoadPackage's digest check passes. Tests that need a MISMATCH corrupt one of
// the two afterwards, on purpose.
func writePackage(t *testing.T, dir string, p pkg) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create package dir %s: %v", dir, err)
	}
	sum := sha256.Sum256(p.wasm)
	decls := make([]manifest.ToolDecl, 0, len(p.tools))
	for _, name := range p.tools {
		decls = append(decls, manifest.ToolDecl{
			Name:        name,
			Description: "fixture tool " + name,
			Group:       "plugins",
			RiskLevel:   "low",
			TimeoutMs:   1000,
		})
	}
	pm := manifest.PluginManifest{
		Name:         p.name,
		Version:      p.version,
		ABI:          1,
		SHA256:       hex.EncodeToString(sum[:]),
		Capabilities: p.capabilities,
		Limits:       manifest.Limits{TimeoutMs: 5000, MaxMemoryPages: 64, MaxInstances: 1},
		Tools:        decls,
	}
	data, err := json.Marshal(pm)
	if err != nil {
		t.Fatalf("encode plugin.json for %s: %v", p.name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), data, 0o644); err != nil {
		t.Fatalf("write plugin.json for %s: %v", p.name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), p.wasm, 0o644); err != nil {
		t.Fatalf("write plugin.wasm for %s: %v", p.name, err)
	}
}

// entryFor builds one deployment entry accepting the named tools.
func entryFor(name, source string, capabilities []string, tools ...string) manifest.Entry {
	accepts := make([]manifest.ToolAccept, 0, len(tools))
	for _, toolName := range tools {
		accepts = append(accepts, manifest.ToolAccept{Name: toolName})
	}
	return manifest.Entry{
		Name:    name,
		Source:  source,
		Enabled: true,
		Grant:   manifest.GrantDecl{Capabilities: capabilities},
		Tools:   accepts,
	}
}

// harness is one test's whole world: a ledger, a registry, an in-memory event
// bus, and a Loader wired to them, rooted at a temp directory.
type harness struct {
	t        *testing.T
	root     string
	ledger   *lifecycle.Ledger
	registry *tool.Registry
	events   *adapter.MemoryEventBus
	loader   *Loader
}

// newHarness builds that world and — this is not optional — disposes every
// owner still in the ledger when the test ends. toolauth's gateable catalog is
// PROCESS-GLOBAL: a test that left a plugin mounted would make the next test's
// contribution of the same tool name panic. The cleanup goes through
// lifecycle.Ledger, not through the Loader, so it holds even for a test that
// left the Loader in a state the feature under test could not unwind.
func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		t:        t,
		root:     t.TempDir(),
		ledger:   lifecycle.NewLedger(),
		registry: tool.NewRegistry(nil, nil, nil),
		events:   adapter.NewMemoryEventBus(),
	}
	loader, err := New(Config{
		Ledger:       h.ledger,
		Deps:         h.deps,
		Events:       h.events,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		DeployLimits: manifest.Limits{TimeoutMs: 5000, MaxMemoryPages: 64, MaxInstances: 1},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.loader = loader

	t.Cleanup(func() {
		for owner := range h.ledger.Snapshot() {
			if err := h.ledger.DisposeOwner(owner); err != nil {
				t.Errorf("cleanup: dispose owner %s: %v", owner, err)
			}
		}
	})
	return h
}

// deps is the harness's host.Deps factory. It hands every plugin the same
// registry (which is also where its tools are contributed), the same event bus
// and a discarding logger; the per-plugin parts — name and config — are what the
// Loader passes in.
func (h *harness) deps(name string, cfg json.RawMessage) host.Deps {
	return host.Deps{
		PluginName: name,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config:     cfg,
		Events:     h.events,
		Tools:      h.registry,
		Agent:      domain.Agent{ID: "test-agent"},
	}
}

// writeEcho writes the echo plugin package (no capabilities, so it needs no
// host module at all) at the given version, and returns its deployment entry.
func (h *harness) writeEcho(version string, tools ...string) manifest.Entry {
	h.t.Helper()

	if len(tools) == 0 {
		tools = []string{echoToolName}
	}
	writePackage(h.t, filepath.Join(h.root, "echo"), pkg{
		wasm:    fixtureWasm(h.t, echoWasmFile),
		name:    echoPluginName,
		version: version,
		tools:   tools,
	})
	return entryFor(echoPluginName, "echo", nil, tools...)
}

// writeProxy writes the e2e plugin package. That guest imports call_tool, so it
// needs the "tool" capability declared AND granted.
func (h *harness) writeProxy(version string) manifest.Entry {
	h.t.Helper()

	writePackage(h.t, filepath.Join(h.root, "proxy"), pkg{
		wasm:         fixtureWasm(h.t, proxyWasmFile),
		name:         proxyPluginName,
		version:      version,
		capabilities: []string{"tool"},
		tools:        []string{proxyToolName},
	})
	return entryFor(proxyPluginName, "proxy", []string{"tool"}, proxyToolName)
}

// owners returns the ledger's live owners, sorted.
func (h *harness) owners() []string {
	h.t.Helper()

	snapshot := h.ledger.Snapshot()
	names := make([]string, 0, len(snapshot))
	for owner := range snapshot {
		names = append(names, string(owner))
	}
	sort.Strings(names)
	return names
}

// toolNames returns the registry's registered tool names, sorted.
func (h *harness) toolNames() []string {
	h.t.Helper()

	descriptors := h.registry.Descriptors()
	names := make([]string, 0, len(descriptors))
	for _, d := range descriptors {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return names
}

// eventsOfType returns every published runtime event of one type.
//
// Counting events is how these tests observe activations, deliberately: the
// alternative — a counter on the Loader that only tests read — would be a
// test-only seam in production code.
func (h *harness) eventsOfType(eventType string) []domain.RuntimeEvent {
	h.t.Helper()

	all, err := h.events.Events()
	if err != nil {
		h.t.Fatalf("read published events: %v", err)
	}
	var matched []domain.RuntimeEvent
	for _, event := range all {
		if event.Type == eventType {
			matched = append(matched, event)
		}
	}
	return matched
}

// apply runs one convergence and fails the test if it reports anything.
func (h *harness) apply(entries ...manifest.Entry) {
	h.t.Helper()

	if err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: entries}, h.root); err != nil {
		h.t.Fatalf("Apply: %v", err)
	}
}

func wantStrings(t *testing.T, what string, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
	}
}

func TestNewRequiresEveryDependency(t *testing.T) {
	full := func() Config {
		return Config{
			Ledger: lifecycle.NewLedger(),
			Deps:   func(string, json.RawMessage) host.Deps { return host.Deps{} },
			Events: adapter.NewMemoryEventBus(),
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}
	cases := []struct {
		name    string
		corrupt func(*Config)
		field   string
	}{
		{name: "no ledger", corrupt: func(c *Config) { c.Ledger = nil }, field: "Ledger"},
		{name: "no deps", corrupt: func(c *Config) { c.Deps = nil }, field: "Deps"},
		{name: "no events", corrupt: func(c *Config) { c.Events = nil }, field: "Events"},
		{name: "no logger", corrupt: func(c *Config) { c.Logger = nil }, field: "Logger"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := full()
			tc.corrupt(&cfg)
			loader, err := New(cfg)
			if err == nil {
				t.Fatalf("New must refuse a Config with no %s, got loader %v", tc.field, loader)
			}
			if loader != nil {
				t.Fatalf("New must not return a Loader alongside its error, got %v", loader)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("New's error must name the missing field %q, got %q", tc.field, err)
			}
		})
	}

	if _, err := New(full()); err != nil {
		t.Fatalf("New with every dependency present: %v", err)
	}
}

func TestApplyRefusesAnEmptyRoot(t *testing.T) {
	h := newHarness(t)
	entry := h.writeEcho("1.0.0")

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, "")
	if err == nil {
		t.Fatal("Apply must refuse an empty root")
	}
	if len(h.owners()) != 0 {
		t.Fatalf("a refused Apply must mount nothing, ledger holds %v", h.owners())
	}
}

func TestApplyActivatesEveryEnabledEntry(t *testing.T) {
	h := newHarness(t)
	echo := h.writeEcho("1.0.0")
	proxy := h.writeProxy("1.0.0")

	h.apply(echo, proxy)

	wantStrings(t, "ledger owners", h.owners(), []string{
		"plugin:" + proxyPluginName + "@1.0.0",
		"plugin:" + echoPluginName + "@1.0.0",
	})
	wantStrings(t, "registered tools", h.toolNames(), []string{proxyToolName, echoToolName})
	for _, name := range []string{echoToolName, proxyToolName} {
		if !toolauth.IsGateable(name) {
			t.Fatalf("tool %q must be gateable once its plugin is mounted", name)
		}
	}

	loaded := h.eventsOfType(RuntimeEventLoaded)
	if len(loaded) != 2 {
		t.Fatalf("want one %s event per plugin, got %d: %v", RuntimeEventLoaded, len(loaded), loaded)
	}
	if !strings.Contains(loaded[0].Message, echoPluginName) || !strings.Contains(loaded[0].Message, "1.0.0") {
		t.Fatalf("%s must name the plugin and version, got %q", RuntimeEventLoaded, loaded[0].Message)
	}
	if !strings.Contains(loaded[0].Message, echoToolName) {
		t.Fatalf("%s must name the contributed tools, got %q", RuntimeEventLoaded, loaded[0].Message)
	}

	status := h.loader.Status()
	if len(status) != 2 {
		t.Fatalf("want two statuses, got %v", status)
	}
	if status[0].Name != proxyPluginName || status[1].Name != echoPluginName {
		t.Fatalf("Status must be sorted by name, got %v", status)
	}
	for _, s := range status {
		if s.State != StateLoaded {
			t.Fatalf("plugin %q state: got %q, want %q", s.Name, s.State, StateLoaded)
		}
		if s.Version != "1.0.0" {
			t.Fatalf("plugin %q version: got %q, want 1.0.0", s.Name, s.Version)
		}
		if s.LastError != "" {
			t.Fatalf("plugin %q must carry no error, got %q", s.Name, s.LastError)
		}
		if len(s.Tools) != 1 {
			t.Fatalf("plugin %q must report its contributed tool, got %v", s.Name, s.Tools)
		}
	}
}

// TestApplyLeavesAnUnchangedEntryAlone is the idempotency requirement: an entry
// whose content did not change must NOT be torn down and rebuilt (that would
// drop the guest's memory state and pay a fresh instantiation for nothing).
//
// It is asserted through the event stream — exactly one plugin/loaded and zero
// plugin/unloaded, however many times Apply runs — because "the same instance is
// still there" is not observable from the registry, where a rebuilt plugin looks
// identical.
//
// The loop is bounded to a literal 3 rounds and asserts a ceiling on the mounted
// instance count every round: it must never become "apply until it converges"
// (see the plan's fork-bomb rules).
func TestApplyLeavesAnUnchangedEntryAlone(t *testing.T) {
	h := newHarness(t)
	echo := h.writeEcho("1.0.0")

	const rounds = 3
	for round := 0; round < rounds; round++ {
		h.apply(echo)

		if got := len(h.owners()); got > 1 {
			t.Fatalf("round %d: at most one plugin may be mounted, ledger holds %d: %v", round, got, h.owners())
		}
		if got := len(h.loader.Status()); got > 1 {
			t.Fatalf("round %d: at most one instance may exist, got %d", round, got)
		}
	}

	if loaded := h.eventsOfType(RuntimeEventLoaded); len(loaded) != 1 {
		t.Fatalf("an unchanged entry must be activated exactly once across %d applies, got %d: %v",
			rounds, len(loaded), loaded)
	}
	if unloaded := h.eventsOfType(RuntimeEventUnloaded); len(unloaded) != 0 {
		t.Fatalf("an unchanged entry must never be unloaded, got %v", unloaded)
	}
	wantStrings(t, "registered tools", h.toolNames(), []string{echoToolName})
}

func TestApplyUnloadsADisabledEntry(t *testing.T) {
	h := newHarness(t)
	echo := h.writeEcho("1.0.0")
	h.apply(echo)

	disabled := echo
	disabled.Enabled = false
	h.apply(disabled)

	if got := h.owners(); len(got) != 0 {
		t.Fatalf("a disabled entry must leave no owner behind, got %v", got)
	}
	if got := h.toolNames(); len(got) != 0 {
		t.Fatalf("a disabled entry's tools must be gone, got %v", got)
	}
	if toolauth.IsGateable(echoToolName) {
		t.Fatalf("tool %q must stop being gateable once its plugin is unmounted", echoToolName)
	}
	if got := h.loader.Status(); len(got) != 0 {
		t.Fatalf("a disabled entry must not be reported as an instance, got %v", got)
	}

	unloaded := h.eventsOfType(RuntimeEventUnloaded)
	if len(unloaded) != 1 {
		t.Fatalf("want one %s event, got %v", RuntimeEventUnloaded, unloaded)
	}
	if !strings.Contains(unloaded[0].Message, reasonDisabled) {
		t.Fatalf("%s must carry reason %q, got %q", RuntimeEventUnloaded, reasonDisabled, unloaded[0].Message)
	}
}

// TestApplyUnloadsARemovedEntry pins that a vanished entry and a disabled one
// behave identically apart from the reason they report.
func TestApplyUnloadsARemovedEntry(t *testing.T) {
	h := newHarness(t)
	echo := h.writeEcho("1.0.0")
	h.apply(echo)

	h.apply()

	if got := h.owners(); len(got) != 0 {
		t.Fatalf("a removed entry must leave no owner behind, got %v", got)
	}
	if got := h.toolNames(); len(got) != 0 {
		t.Fatalf("a removed entry's tools must be gone, got %v", got)
	}
	if toolauth.IsGateable(echoToolName) {
		t.Fatalf("tool %q must stop being gateable once its plugin is unmounted", echoToolName)
	}
	if got := h.loader.Status(); len(got) != 0 {
		t.Fatalf("a removed entry must not be reported as an instance, got %v", got)
	}

	unloaded := h.eventsOfType(RuntimeEventUnloaded)
	if len(unloaded) != 1 {
		t.Fatalf("want one %s event, got %v", RuntimeEventUnloaded, unloaded)
	}
	if !strings.Contains(unloaded[0].Message, reasonManifestRemoved) {
		t.Fatalf("%s must carry reason %q, got %q", RuntimeEventUnloaded, reasonManifestRemoved, unloaded[0].Message)
	}
}

func TestApplyReplacesAnEntryWhoseVersionChanged(t *testing.T) {
	h := newHarness(t)
	h.apply(h.writeEcho("1.0.0"))

	h.apply(h.writeEcho("2.0.0"))

	wantStrings(t, "ledger owners", h.owners(), []string{"plugin:" + echoPluginName + "@2.0.0"})
	wantStrings(t, "registered tools", h.toolNames(), []string{echoToolName})

	unloaded := h.eventsOfType(RuntimeEventUnloaded)
	if len(unloaded) != 1 || !strings.Contains(unloaded[0].Message, reasonReplaced) {
		t.Fatalf("a replaced entry must report reason %q, got %v", reasonReplaced, unloaded)
	}
	if loaded := h.eventsOfType(RuntimeEventLoaded); len(loaded) != 2 {
		t.Fatalf("want one %s event per activation, got %v", RuntimeEventLoaded, loaded)
	}
	status := h.loader.Status()
	if len(status) != 1 || status[0].Version != "2.0.0" {
		t.Fatalf("Status must report the new version, got %v", status)
	}
}

// TestApplyReplacesAnEntryWhoseModuleChanged is the same convergence for a
// rebuild that keeps the version: the sha256 changed, so the entry changed, and
// the owner is REUSED — which only works because the old instance is disposed
// before the new one is activated (host.Activate refuses a non-empty owner).
func TestApplyReplacesAnEntryWhoseModuleChanged(t *testing.T) {
	h := newHarness(t)
	h.apply(h.writeEcho("1.0.0"))

	rebuilt := appendCustomSection(t, fixtureWasm(t, echoWasmFile), "rebuild")
	writePackage(t, filepath.Join(h.root, "echo"), pkg{
		wasm:    rebuilt,
		name:    echoPluginName,
		version: "1.0.0",
		tools:   []string{echoToolName},
	})
	h.apply(entryFor(echoPluginName, "echo", nil, echoToolName))

	wantStrings(t, "ledger owners", h.owners(), []string{"plugin:" + echoPluginName + "@1.0.0"})
	wantStrings(t, "registered tools", h.toolNames(), []string{echoToolName})
	if unloaded := h.eventsOfType(RuntimeEventUnloaded); len(unloaded) != 1 {
		t.Fatalf("a rebuilt module must replace the running instance, got %v", unloaded)
	}
	if loaded := h.eventsOfType(RuntimeEventLoaded); len(loaded) != 2 {
		t.Fatalf("want one %s event per activation, got %v", RuntimeEventLoaded, loaded)
	}
}

// TestApplyReplacesAnEntryWhoseConfigChanged pins the rest of the change
// detection: config is not part of the package on disk, so an implementation
// that fingerprinted only the wasm digest would leave the plugin running with
// the operator's old configuration.
func TestApplyReplacesAnEntryWhoseConfigChanged(t *testing.T) {
	h := newHarness(t)
	echo := h.writeEcho("1.0.0")
	echo.Config = json.RawMessage(`{"mode":"a"}`)
	h.apply(echo)

	reconfigured := echo
	reconfigured.Config = json.RawMessage(`{"mode":"b"}`)
	h.apply(reconfigured)

	if unloaded := h.eventsOfType(RuntimeEventUnloaded); len(unloaded) != 1 {
		t.Fatalf("a changed config must replace the running instance, got %v", unloaded)
	}
	if loaded := h.eventsOfType(RuntimeEventLoaded); len(loaded) != 2 {
		t.Fatalf("want one %s event per activation, got %v", RuntimeEventLoaded, loaded)
	}
}

// TestApplyRestoresThePreviousInstanceWhenAReplacementFails is design §5.4's
// hard requirement: the old instance comes BACK when its replacement cannot be
// brought up. The replacement fails on its own merits — its plugin.json declares
// a tool the guest does not provide, so host.Activate refuses at the cross-check.
func TestApplyRestoresThePreviousInstanceWhenAReplacementFails(t *testing.T) {
	h := newHarness(t)
	h.apply(h.writeEcho("1.0.0"))

	broken := h.writeEcho("2.0.0", echoToolName, ghostToolName)
	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{broken}}, h.root)
	if err == nil {
		t.Fatal("Apply must report a replacement that could not be activated")
	}
	if !strings.Contains(err.Error(), ghostToolName) {
		t.Fatalf("the error must explain why the replacement failed, got %q", err)
	}

	wantStrings(t, "ledger owners", h.owners(), []string{"plugin:" + echoPluginName + "@1.0.0"})
	wantStrings(t, "registered tools", h.toolNames(), []string{echoToolName})
	if !toolauth.IsGateable(echoToolName) {
		t.Fatalf("the restored instance's tool must be gateable again")
	}

	status := h.loader.Status()
	if len(status) != 1 {
		t.Fatalf("want the restored instance in Status, got %v", status)
	}
	if status[0].Version != "1.0.0" || status[0].State != StateLoaded {
		t.Fatalf("Status must report the previous instance as running, got %+v", status[0])
	}
	if status[0].LastError == "" {
		t.Fatalf("Status must keep the failure that forced the restore, got %+v", status[0])
	}

	failed := h.eventsOfType(RuntimeEventActivationFailed)
	if len(failed) != 1 {
		t.Fatalf("want one %s event, got %v", RuntimeEventActivationFailed, failed)
	}
	if !strings.Contains(failed[0].Message, "restored="+restoredYes) {
		t.Fatalf("%s must state that the previous instance was restored, got %q",
			RuntimeEventActivationFailed, failed[0].Message)
	}
	if !strings.Contains(failed[0].Message, "step="+stepActivate) {
		t.Fatalf("%s must name the failing step, got %q", RuntimeEventActivationFailed, failed[0].Message)
	}
	if !strings.Contains(failed[0].Message, "rolled_back=") {
		t.Fatalf("%s must report how many entries were rolled back, got %q",
			RuntimeEventActivationFailed, failed[0].Message)
	}
	// The restore is a real activation, so it publishes its own plugin/loaded:
	// one for the original mount, one for the restore, and none for the
	// replacement that never came up.
	if loaded := h.eventsOfType(RuntimeEventLoaded); len(loaded) != 2 {
		t.Fatalf("want the original activation and the restore, got %v", loaded)
	}
}

// TestApplyReportsBothFailuresWhenTheRestoreAlsoFails covers the second half of
// §5.4: when the previous instance cannot be brought back either, BOTH errors
// travel out, each identifiable, and the event says the old instance was NOT
// restored.
//
// The restore is made to fail through the ledger, using only its public API: a
// disposer filed under the plugin's owner files a NEW entry under that same
// owner while the unload runs, so by the time the restore calls host.Activate,
// the owner is no longer exclusive — the precondition host.Activate refuses on.
// That models any foreign resource filed under a plugin's owner, which is
// exactly the state that check exists to catch, and it needs no seam in
// production code.
func TestApplyReportsBothFailuresWhenTheRestoreAlsoFails(t *testing.T) {
	h := newHarness(t)
	h.apply(h.writeEcho("1.0.0"))

	owner := ownerFor(echoPluginName, "1.0.0")
	h.ledger.Add(owner, "test-squatter-installer", func() error {
		h.ledger.Add(owner, "test-squatter", func() error { return nil })
		return nil
	})

	broken := h.writeEcho("2.0.0", echoToolName, ghostToolName)
	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{broken}}, h.root)
	if err == nil {
		t.Fatal("Apply must report both the failed replacement and the failed restore")
	}
	if !strings.Contains(err.Error(), ghostToolName) {
		t.Fatalf("the joined error must still carry the replacement's own failure, got %q", err)
	}
	if !strings.Contains(err.Error(), "restore") {
		t.Fatalf("the joined error must carry the restore failure too, got %q", err)
	}

	failed := h.eventsOfType(RuntimeEventActivationFailed)
	if len(failed) != 1 {
		t.Fatalf("want one %s event, got %v", RuntimeEventActivationFailed, failed)
	}
	if !strings.Contains(failed[0].Message, "restored="+restoredNo) {
		t.Fatalf("%s must state that the previous instance was NOT restored, got %q",
			RuntimeEventActivationFailed, failed[0].Message)
	}

	status := h.loader.Status()
	if len(status) != 1 {
		t.Fatalf("want the failed entry in Status, got %v", status)
	}
	if status[0].State != StateFailed {
		t.Fatalf("Status must report the entry as failed, got %+v", status[0])
	}
	if status[0].LastError == "" {
		t.Fatalf("a failed entry must carry its reason, got %+v", status[0])
	}
	if got := h.toolNames(); len(got) != 0 {
		t.Fatalf("nothing may stay registered when neither instance came up, got %v", got)
	}
	// Not an assertion about the Loader: it proves the harness's own blocking
	// entry really was filed, so the assertions above cannot be passing because
	// the restore failed for some unrelated reason.
	if held := h.ledger.Snapshot()[owner]; len(held) != 1 || held[0] != "test-squatter" {
		t.Fatalf("the test's blocking ledger entry was never filed (owner holds %v); "+
			"the restore did not fail for the intended reason", held)
	}
}

// TestApplyKeepsConvergingAfterOneEntryFails: one bad entry must not abort the
// others, and the joined error must mention only the entry that actually broke.
func TestApplyKeepsConvergingAfterOneEntryFails(t *testing.T) {
	h := newHarness(t)
	echo := h.writeEcho("1.0.0")
	proxy := h.writeProxy("1.0.0")
	missing := entryFor("legion-missing-plugin", "missing", nil, "missing_tool")

	err := h.loader.Apply(
		context.Background(),
		manifest.Deployment{Plugins: []manifest.Entry{echo, missing, proxy}},
		h.root,
	)
	if err == nil {
		t.Fatal("Apply must report the entry whose package does not exist")
	}
	if !strings.Contains(err.Error(), "legion-missing-plugin") {
		t.Fatalf("the error must name the broken entry, got %q", err)
	}
	if strings.Contains(err.Error(), echoPluginName) || strings.Contains(err.Error(), proxyPluginName) {
		t.Fatalf("the error must mention only the broken entry, got %q", err)
	}

	wantStrings(t, "registered tools", h.toolNames(), []string{proxyToolName, echoToolName})
	wantStrings(t, "ledger owners", h.owners(), []string{
		"plugin:" + proxyPluginName + "@1.0.0",
		"plugin:" + echoPluginName + "@1.0.0",
	})

	status := h.loader.Status()
	var failedStatus InstanceStatus
	for _, s := range status {
		if s.Name == "legion-missing-plugin" {
			failedStatus = s
		}
	}
	if failedStatus.Name == "" {
		t.Fatalf("a failed entry must stay visible in Status, got %v", status)
	}
	if failedStatus.State != StateFailed || failedStatus.LastError == "" {
		t.Fatalf("failed entry status: got %+v", failedStatus)
	}
	if failed := h.eventsOfType(RuntimeEventActivationFailed); len(failed) != 1 {
		t.Fatalf("want one %s event, got %v", RuntimeEventActivationFailed, failed)
	}
}

// TestApplyKeepsARunningInstanceWhenItsNewPackageCannotBeRead pins the ordering
// that makes the failure safe: the package is read and assembled BEFORE the
// running instance is torn down, so a corrupted plugin.wasm leaves the old one
// running rather than taking it down for a replacement that never existed.
func TestApplyKeepsARunningInstanceWhenItsNewPackageCannotBeRead(t *testing.T) {
	h := newHarness(t)
	echo := h.writeEcho("1.0.0")
	h.apply(echo)

	corrupted := filepath.Join(h.root, "echo", "plugin.wasm")
	if err := os.WriteFile(corrupted, []byte("not the module plugin.json declares"), 0o644); err != nil {
		t.Fatalf("corrupt plugin.wasm: %v", err)
	}

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{echo}}, h.root)
	if err == nil {
		t.Fatal("Apply must report a package whose sha256 no longer matches")
	}

	wantStrings(t, "ledger owners", h.owners(), []string{"plugin:" + echoPluginName + "@1.0.0"})
	wantStrings(t, "registered tools", h.toolNames(), []string{echoToolName})
	if unloaded := h.eventsOfType(RuntimeEventUnloaded); len(unloaded) != 0 {
		t.Fatalf("nothing may be unloaded when the new package cannot even be read, got %v", unloaded)
	}
	failed := h.eventsOfType(RuntimeEventActivationFailed)
	if len(failed) != 1 || !strings.Contains(failed[0].Message, "step="+stepLoadPackage) {
		t.Fatalf("want one %s event naming step %q, got %v", RuntimeEventActivationFailed, stepLoadPackage, failed)
	}
	if !strings.Contains(failed[0].Message, "restored="+restoredNone) {
		t.Fatalf("a failure that never touched the running instance must not claim a restore, got %q",
			failed[0].Message)
	}
}

// TestApplyRefusesAnEntryNamingADifferentPlugin: the deployment entry's name and
// the plugin's own name are two spellings of one identity. Letting them differ
// would let two entries mount the same plugin, and the second contribution of
// the same tool name is a panic in the registry, not an error.
func TestApplyRefusesAnEntryNamingADifferentPlugin(t *testing.T) {
	h := newHarness(t)
	h.writeEcho("1.0.0")
	mislabeled := entryFor("some-other-name", "echo", nil, echoToolName)

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{mislabeled}}, h.root)
	if err == nil {
		t.Fatal("Apply must refuse an entry whose name is not the plugin's own")
	}
	if !strings.Contains(err.Error(), "some-other-name") || !strings.Contains(err.Error(), echoPluginName) {
		t.Fatalf("the error must name both spellings, got %q", err)
	}
	if got := h.owners(); len(got) != 0 {
		t.Fatalf("nothing may be mounted, got %v", got)
	}
}

func TestApplyReportsADepsFactoryWithNoRegistry(t *testing.T) {
	ledger := lifecycle.NewLedger()
	events := adapter.NewMemoryEventBus()
	root := t.TempDir()
	loader, err := New(Config{
		Ledger:       ledger,
		Deps:         func(string, json.RawMessage) host.Deps { return host.Deps{} },
		Events:       events,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		DeployLimits: manifest.Limits{TimeoutMs: 5000, MaxMemoryPages: 64, MaxInstances: 1},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writePackage(t, filepath.Join(root, "echo"), pkg{
		wasm:    fixtureWasm(t, echoWasmFile),
		name:    echoPluginName,
		version: "1.0.0",
		tools:   []string{echoToolName},
	})
	entry := entryFor(echoPluginName, "echo", nil, echoToolName)

	applyErr := loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, root)
	if applyErr == nil {
		t.Fatal("Apply must refuse to activate with a Deps factory that supplies no registry")
	}
	if !strings.Contains(applyErr.Error(), "Tools") {
		t.Fatalf("the error must name the missing dependency, got %q", applyErr)
	}
	if got := len(ledger.Snapshot()); got != 0 {
		t.Fatalf("nothing may be filed, got %v", ledger.Snapshot())
	}
}

// TestApplyRefusesAnEntryWhoseToolNameIsTaken: two contributors fighting over
// one model-facing name is fail-loud by PANIC in the registry and in the
// gateable catalog alike, and a panic in the middle of a convergence would
// abort every other entry instead of reporting one. The Loader has to turn it
// into an error first.
func TestApplyRefusesAnEntryWhoseToolNameIsTaken(t *testing.T) {
	h := newHarness(t)
	revoke := h.registry.Register(echoToolName, tool.HandlerFunc(
		func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{}, nil
		}))
	t.Cleanup(revoke)
	entry := h.writeEcho("1.0.0")

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)
	if err == nil {
		t.Fatal("Apply must refuse a plugin whose tool name another contributor already owns")
	}
	if !strings.Contains(err.Error(), echoToolName) {
		t.Fatalf("the error must name the contested tool, got %q", err)
	}
	if got := h.owners(); len(got) != 0 {
		t.Fatalf("nothing may be mounted, got %v", got)
	}
	failed := h.eventsOfType(RuntimeEventActivationFailed)
	if len(failed) != 1 || !strings.Contains(failed[0].Message, "step="+stepToolNames) {
		t.Fatalf("want one %s event naming step %q, got %v", RuntimeEventActivationFailed, stepToolNames, failed)
	}
}
