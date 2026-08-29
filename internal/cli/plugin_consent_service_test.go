package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/plugin/loader"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/plugin/sign"
	"github.com/stardust/legion-agent/internal/server"
)

// TestPluginConsentServiceListReturnsDeclaredAndGrantedSeparately pins the
// whole reason PluginView carries two sets of fields: a plugin.json that
// declares more than the deployment currently grants must show up as two
// different lists, not one merged one -- see PluginView's own doc comment
// (internal/server/plugins.go) for why collapsing them would make "this
// plugin WANTS http" and "http IS authorized" indistinguishable.
func TestPluginConsentServiceListReturnsDeclaredAndGrantedSeparately(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackageWithNetwork("echo", testEchoWasm, testEchoPlugin, "1.0.0",
		[]string{"http", "log"}, []string{testEchoTool},
		manifest.Network{AllowedHosts: []string{"jira.example.com"}}, manifest.Filesystem{})
	// enabled: false keeps this test entirely clear of activation/reconcile
	// concerns -- List's Declared/Granted resolution reads plugin.json and
	// the manifest entry directly, independently of whether the loader ever
	// mounted the plugin.
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false,
		capabilities: []string{"log"}, tools: []string{testEchoTool},
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins, func() *sign.Keyring { return nil }, loader.RemoteConfig{}, testConsentLogger())
	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1: %+v", len(views), views)
	}
	got := views[0]
	if got.Name != testEchoPlugin {
		t.Fatalf("Name = %q, want %q", got.Name, testEchoPlugin)
	}
	if !slices.Equal(got.DeclaredCaps, []string{"http", "log"}) {
		t.Errorf("DeclaredCaps = %v, want [http log]", got.DeclaredCaps)
	}
	if !slices.Equal(got.GrantedCaps, []string{"log"}) {
		t.Errorf("GrantedCaps = %v, want [log]", got.GrantedCaps)
	}
	if !slices.Equal(got.DeclaredHosts, []string{"jira.example.com"}) {
		t.Errorf("DeclaredHosts = %v, want [jira.example.com]", got.DeclaredHosts)
	}
	if len(got.GrantedHosts) != 0 {
		t.Errorf("GrantedHosts = %v, want empty (the manifest entry grants no allowed_hosts)", got.GrantedHosts)
	}
	// The mutation this pins: Declared and Granted capabilities must be
	// reported from two separate fields. A plugin.json that declares two
	// capabilities but is only granted one is exactly the case that would
	// stop being visible if DeclaredCaps and GrantedCaps were ever merged.
	if slices.Equal(got.DeclaredCaps, got.GrantedCaps) {
		t.Fatalf("DeclaredCaps and GrantedCaps must be reported separately, both came back %v", got.DeclaredCaps)
	}
	if got.DeclaredUnresolved {
		t.Errorf("DeclaredUnresolved = true, want false: a local entry's declarations are always resolvable")
	}
}

// TestPluginConsentServiceListErrorsWithoutALoader verifies the fail-loud
// path for pluginsFn returning nil (e.g. drainPlugins detached the loader
// mid-shutdown): List reports it by name instead of dereferencing a nil
// Loader.
func TestPluginConsentServiceListErrorsWithoutALoader(t *testing.T) {
	svc := NewPluginConsentService("does-not-matter.json", "root",
		func() *loader.Loader { return nil }, func() *sign.Keyring { return nil }, loader.RemoteConfig{}, testConsentLogger())
	_, err := svc.List(context.Background())
	if err == nil {
		t.Fatal("List() error = nil, want an error naming the missing loader")
	}
	if !strings.Contains(err.Error(), "no plugin loader") {
		t.Fatalf("List() error = %v, want it to name the missing loader", err)
	}
}

// TestPluginConsentServiceListErrorsWhenManifestUnreadable verifies that a
// manifest path that cannot be read fails List loudly rather than returning
// an empty plugin list that would read as "no plugins declared".
func TestPluginConsentServiceListErrorsWhenManifestUnreadable(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writeManifest()
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	// Points the service at a manifest path that was never written, isolating
	// the read failure from the loader (which assembled fine against the
	// fixture's real, empty manifest).
	svc := NewPluginConsentService(filepath.Join(f.dir, "missing-plugins.json"), f.root,
		f.application.Plugins, func() *sign.Keyring { return nil }, loader.RemoteConfig{}, testConsentLogger())
	if _, err := svc.List(context.Background()); err == nil {
		t.Fatal("List() error = nil, want an error naming the unreadable manifest")
	}
}

// TestPluginConsentServiceListReportsBrokenLocalPackagePerRow is Important-3
// of the whole-branch final review: a local entry whose plugin.json cannot
// be loaded (a corrupted plugin.wasm, here) must be reported as THAT ROW's
// own DeclaredUnresolved/DeclaredError, with the rest of List's response
// still a 200 -- not fail List outright, which would 500 the whole GET
// /v1/plugins and take down every other row's deny button along with it (see
// Deny's own doc comment for why the panel must survive exactly this case).
// A second, healthy entry in the SAME manifest proves the failure stays
// scoped to its own row.
func TestPluginConsentServiceListReportsBrokenLocalPackagePerRow(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
	const healthyPlugin = "healthy-plugin"
	const healthyTool = "healthy_tool"
	f.writePackage("healthy", testEchoWasm, healthyPlugin, "1.0.0", nil, []string{healthyTool})
	f.writeManifest(
		manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}},
		manifestEntry{name: healthyPlugin, source: "healthy", enabled: true, tools: []string{healthyTool}},
	)
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	// Corrupt the on-disk package AFTER assembly: the loader already mounted
	// it from the original bytes, but List's own LoadPackage call re-reads
	// plugin.json from disk and must surface this per-row rather than
	// silently reporting an empty declaration OR failing the whole call.
	pluginJSON := filepath.Join(f.root, "echo", "plugin.json")
	if err := os.WriteFile(pluginJSON, []byte("not json"), 0o644); err != nil {
		t.Fatalf("corrupt plugin.json: %v", err)
	}

	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins, func() *sign.Keyring { return nil }, loader.RemoteConfig{}, testConsentLogger())
	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v, want nil: one broken package must not fail the whole list", err)
	}
	if len(views) != 2 {
		t.Fatalf("len(views) = %d, want 2: %+v", len(views), views)
	}
	var broken, healthy *server.PluginView
	for i := range views {
		switch views[i].Name {
		case testEchoPlugin:
			broken = &views[i]
		case healthyPlugin:
			healthy = &views[i]
		}
	}
	if broken == nil {
		t.Fatalf("no row named %q: %+v", testEchoPlugin, views)
	}
	if !broken.DeclaredUnresolved {
		t.Error("broken entry DeclaredUnresolved = false, want true")
	}
	if broken.DeclaredUnresolvedReason != server.DeclaredUnresolvedLoadFailed {
		t.Errorf("broken entry DeclaredUnresolvedReason = %q, want %q: a package that fails to load is not fetchable, and a consent UI must not offer a fetch for it",
			broken.DeclaredUnresolvedReason, server.DeclaredUnresolvedLoadFailed)
	}
	if broken.DeclaredError == "" {
		t.Error("broken entry DeclaredError = empty, want the load failure reason")
	}
	if !strings.Contains(broken.DeclaredError, testEchoPlugin) {
		t.Errorf("broken entry DeclaredError = %q, want it to name plugin %q", broken.DeclaredError, testEchoPlugin)
	}

	if healthy == nil {
		t.Fatalf("no row named %q: the healthy entry must still render when its sibling is broken: %+v", healthyPlugin, views)
	}
	if healthy.DeclaredUnresolved {
		t.Errorf("healthy entry DeclaredUnresolved = true, want false: it was not the broken one")
	}
	if healthy.DeclaredError != "" {
		t.Errorf("healthy entry DeclaredError = %q, want empty", healthy.DeclaredError)
	}
	if healthy.DeclaredUnresolvedReason != "" {
		t.Errorf("healthy entry DeclaredUnresolvedReason = %q, want empty: a resolved row has no unresolved reason to report",
			healthy.DeclaredUnresolvedReason)
	}
}

// TestPluginConsentServiceListSeparatesNoCacheFromCacheMiss is I-4 of the
// whole-branch final review. Both of these report DeclaredUnresolved=true
// with an EMPTY DeclaredError, and before DeclaredUnresolvedReason existed
// they were byte-identical JSON:
//
//   - a remote entry this deployment simply has not fetched yet, which
//     PluginConsent.Resolve CAN remedy, and
//   - a remote entry in a deployment that configured no "plugins.cache" at
//     all, which Resolve can never remedy -- resolvePluginPackageDir refuses
//     it outright, so a fetch button on that row is a control that cannot
//     work.
//
// A GUI deciding whether to offer a fetch must not have to guess between
// them, so List reports which one it is.
func TestPluginConsentServiceListSeparatesNoCacheFromCacheMiss(t *testing.T) {
	// No "cache" key at all: resolvePluginRemote leaves RemoteConfig.Cache
	// nil, which is the deployment-config fact this half of the test is
	// about. enabled:false keeps assemble() from ever trying to activate
	// (and so fetch) the entry.
	noCache := newPluginFixture(t, 30_000)
	noCache.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)})
	noCache.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "https://example.invalid/echo.tgz", enabled: false,
		tools: []string{testEchoTool}, digest: digestOfArchive([]byte("never fetched")),
	})
	if err := noCache.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	remote := noCache.resolveFixtureRemote()
	if remote.Cache != nil {
		t.Fatalf("fixture remote.Cache = %v, want nil: this test needs a deployment with no configured plugin cache", remote.Cache)
	}
	svc := NewPluginConsentService(noCache.manifestPath, noCache.root, noCache.application.Plugins,
		func() *sign.Keyring { return nil }, remote, testConsentLogger())
	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1: %+v", len(views), views)
	}
	got := views[0]
	if !got.DeclaredUnresolved {
		t.Fatal("DeclaredUnresolved = false, want true: with no cache configured the declaration cannot be resolved")
	}
	if got.DeclaredError != "" {
		t.Errorf("DeclaredError = %q, want empty: no cache configured is a deployment fact, not a load failure", got.DeclaredError)
	}
	if got.DeclaredUnresolvedReason != server.DeclaredUnresolvedNoCache {
		t.Errorf("DeclaredUnresolvedReason = %q, want %q: this row must be distinguishable from a plain cache miss, or the panel offers a fetch that can never succeed",
			got.DeclaredUnresolvedReason, server.DeclaredUnresolvedNoCache)
	}
	if got.DeclaredUnresolvedReason == server.DeclaredUnresolvedNotCached {
		t.Error("DeclaredUnresolvedReason reports a plain cache miss, but this deployment has no cache to miss")
	}
}

// resolveFixtureRemote loads the fixture's own agent.json and resolves it
// into a loader.RemoteConfig exactly the way BuildServeService does for
// PluginConsentService (resolvePluginRemote) -- so a test's RemoteConfig
// points at the SAME cache directory and policy assemble() already built its
// running loader from.
func (f *pluginFixture) resolveFixtureRemote() loader.RemoteConfig {
	f.t.Helper()

	cfg, err := config.Load(context.Background(), config.Options{Path: f.configPath})
	if err != nil {
		f.t.Fatalf("config.Load(%s) error = %v, want nil", f.configPath, err)
	}
	remote, err := resolvePluginRemote(cfg.Plugins)
	if err != nil {
		f.t.Fatalf("resolvePluginRemote() error = %v, want nil", err)
	}
	return remote
}

// TestPluginConsentServiceListResolvesRemoteEntryOnCacheHit is gpc-task-2
// review Finding 1: a remote entry that is ALREADY cached locally must still
// report its real declared capabilities/hosts, not empty ones -- resolving a
// cache hit costs no network I/O, so there is no reason to skip it the way a
// genuine cache miss (which WOULD need a fetch) has to be.
func TestPluginConsentServiceListResolvesRemoteEntryOnCacheHit(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackageWithNetwork("staging", testEchoWasm, testEchoPlugin, "1.0.0",
		[]string{"http", "log"}, []string{testEchoTool},
		manifest.Network{AllowedHosts: []string{"jira.example.com"}}, manifest.Filesystem{})
	f.signPackageWithAnyKey("staging")
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	srv := serveArchive(t, archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)},
		fmt.Sprintf("\"cache\": %s", jsonString(cacheDir)),
		`"allow_insecure_sources": true`)
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: srv.URL + "/echo.tgz", enabled: true,
		capabilities: []string{"log"}, tools: []string{testEchoTool}, digest: digest,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	// assemble() already fetched the archive once to mount the loader, filing
	// it under its digest in cacheDir. Close the server BEFORE building the
	// consent service: if List ever attempted a second, unnecessary fetch for
	// this cache hit, that request would fail (connection refused) instead of
	// quietly succeeding, so this test can only pass by resolving the package
	// straight from the cache.
	srv.Close()

	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
		func() *sign.Keyring { return nil }, f.resolveFixtureRemote(), testConsentLogger())
	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1: %+v", len(views), views)
	}
	got := views[0]
	if got.DeclaredUnresolved {
		t.Fatalf("DeclaredUnresolved = true, want false: the package is already cached, resolvable without any network call")
	}
	if !slices.Equal(got.DeclaredCaps, []string{"http", "log"}) {
		t.Errorf("DeclaredCaps = %v, want [http log]", got.DeclaredCaps)
	}
	if !slices.Equal(got.DeclaredHosts, []string{"jira.example.com"}) {
		t.Errorf("DeclaredHosts = %v, want [jira.example.com]", got.DeclaredHosts)
	}
	if !slices.Equal(got.GrantedCaps, []string{"log"}) {
		t.Errorf("GrantedCaps = %v, want [log]", got.GrantedCaps)
	}
	// Same mutation this file's local-entry test pins, now for a remote one:
	// Declared and Granted must stay two separate fields.
	if slices.Equal(got.DeclaredCaps, got.GrantedCaps) {
		t.Fatalf("DeclaredCaps and GrantedCaps must be reported separately, both came back %v", got.DeclaredCaps)
	}
}

// TestPluginConsentServiceListReportsUnresolvedForRemoteCacheMiss is the
// other half of Finding 1: a remote entry whose package the cache does NOT
// hold must report DeclaredUnresolved=true with empty Declared* fields, not
// an error and not an empty-but-"resolved" declaration -- the whole point of
// the new field is telling "declares nothing" apart from "we do not know".
// List must not attempt a network fetch to find out.
func TestPluginConsentServiceListReportsUnresolvedForRemoteCacheMiss(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)},
		fmt.Sprintf("\"cache\": %s", jsonString(cacheDir)),
		`"allow_insecure_sources": true`)
	// enabled: false means assemble() never activates (and so never fetches)
	// this entry -- nothing is ever put in the cache under this digest, which
	// is exactly the genuine cache-miss this test is about.
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "https://example.invalid/echo.tgz", enabled: false,
		tools: []string{testEchoTool}, digest: digestOfArchive([]byte("never fetched")),
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
		func() *sign.Keyring { return nil }, f.resolveFixtureRemote(), testConsentLogger())
	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v, want nil: a cache miss must be reported, not fail the whole call", err)
	}
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1: %+v", len(views), views)
	}
	got := views[0]
	if !got.DeclaredUnresolved {
		t.Fatal("DeclaredUnresolved = false, want true: this entry was never fetched, so its cache does not hold it")
	}
	if got.DeclaredUnresolvedReason != server.DeclaredUnresolvedNotCached {
		t.Errorf("DeclaredUnresolvedReason = %q, want %q: a plain cache miss is the ONE case a deliberate Resolve can remedy, and the panel keys its fetch button on it",
			got.DeclaredUnresolvedReason, server.DeclaredUnresolvedNotCached)
	}
	if len(got.DeclaredCaps) != 0 || len(got.DeclaredHosts) != 0 || len(got.DeclaredPaths) != 0 {
		t.Errorf("Declared* = caps=%v hosts=%v paths=%v, want all empty when unresolved",
			got.DeclaredCaps, got.DeclaredHosts, got.DeclaredPaths)
	}
}

// --- Task 3: PluginConsentService.Grant / .Deny -----------------------------

// newGrantTestService is NewPluginConsentService for Grant/Deny tests, wired
// the same way every List test above wires it.
func (f *pluginFixture) newGrantTestService() *PluginConsentService {
	return NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
		func() *sign.Keyring { return nil }, loader.RemoteConfig{}, testConsentLogger())
}

// TestPluginConsentServiceGrantRefusesAStrictCapabilitySubset is rule 2 of
// `agent plugins grant`, enforced through consent.ResolveCapabilities: a
// grant that covers only PART of what the plugin declares would produce an
// entry manifest.reconcileCapabilities can never load, so it is refused
// outright with nothing written to plugins.json.
func TestPluginConsentServiceGrantRefusesAStrictCapabilitySubset(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log", "http"}, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json before grant: %v", err)
	}

	svc := f.newGrantTestService()
	_, err = svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{Capabilities: []string{"log"}})
	if err == nil {
		t.Fatal("Grant() error = nil, want an error: the plugin also declares \"http\", which a partial grant would leave ungranted")
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("Grant() error = %v, want it to name the missing declared capability %q", err, "http")
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json after grant: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("plugins.json changed after a refused grant:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestPluginConsentServiceGrantRefusesHTTPWithNoAllowedHosts is
// RefuseUnnamedAllowlist's own rule: granting "http" while the plugin
// declares a non-empty "network"."allowed_hosts" and naming none of them
// here would authorize http with an allowlist that reaches nothing.
// Refused outright, nothing written.
func TestPluginConsentServiceGrantRefusesHTTPWithNoAllowedHosts(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackageWithNetwork("echo", testEchoWasm, testEchoPlugin, "1.0.0",
		[]string{"http"}, []string{testEchoTool},
		manifest.Network{AllowedHosts: []string{"jira.example.com"}}, manifest.Filesystem{})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json before grant: %v", err)
	}

	svc := f.newGrantTestService()
	_, err = svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{Capabilities: []string{"http"}})
	if err == nil {
		t.Fatal("Grant() error = nil, want an error: http is named with no allowed_hosts, while the plugin declares some")
	}
	if !strings.Contains(err.Error(), "allowed_hosts") {
		t.Errorf("Grant() error = %v, want it to name \"allowed_hosts\"", err)
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json after grant: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("plugins.json changed after a refused grant:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestPluginConsentServiceGrantRefusesAConcurrentEditDuringTheDownload
// mirrors TestPluginsGrantRefusesAConcurrentEditDuringTheDownload
// (plugins_command_test.go) at the service level: resolvePluginPackageDir
// performs a full artifact download on a cache miss, and Grant used to (like
// `agent plugins grant` before BLOCKING-1) write a document built from the
// read it took before that download, with no compare-and-swap in between. A
// server handler that mutates plugins.json from inside the very request the
// fetch is blocked on stands in for a concurrent edit. Grant must refuse
// rather than silently rewrite the file from its now-stale snapshot, and the
// concurrent edit must survive exactly as written.
func TestPluginConsentServiceGrantRefusesAConcurrentEditDuringTheDownload(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log"}, []string{testEchoTool})
	f.signPackageWithAnyKey("staging")
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")

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

	f.writeInstallConfig(signaturePolicy{requireSignature: boolPtr(false)}, cacheDir)
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: srv.URL + "/echo.tgz", digest: digest, enabled: false,
		tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
		func() *sign.Keyring { return nil }, f.resolveFixtureRemote(), testConsentLogger())
	_, err := svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{Capabilities: []string{"log"}})
	if err == nil {
		t.Fatal("Grant() error = nil, want an error: plugins.json changed while the package was downloading")
	}
	if !strings.Contains(err.Error(), f.manifestPath) {
		t.Errorf("Grant() error = %v, want it to name the manifest path %q", err, f.manifestPath)
	}
	if !strings.Contains(err.Error(), "changed") {
		t.Errorf("Grant() error = %v, want it to say the manifest changed underneath it", err)
	}
	// gpc-task-3 review Minor-8: a concurrent edit is a CONFLICT, and the
	// handler is only able to answer 409 instead of a blanket 400 because
	// the error carries the class rather than only the words.
	if !errors.Is(err, server.ErrPluginDeploymentChanged) {
		t.Errorf("Grant() error = %v, want it to wrap server.ErrPluginDeploymentChanged", err)
	}

	after := f.readDeployment()
	if len(after.Plugins) != 1 || after.Plugins[0].Name != "concurrently-installed-plugin" {
		t.Fatalf("plugins.json after Grant() = %+v, want ONLY the concurrent edit still present -- Grant must "+
			"refuse rather than silently revert it", after.Plugins)
	}
}

// TestPluginConsentServiceDenyKeepsFieldsAndGrantStated is rule 3 of `agent
// plugins deny`, exercised through the service: deny flips Enabled false and
// empties Grant.Capabilities, but keeps GrantStated true (a decision WAS
// made) and leaves Source, Digest and Tools untouched field by field.
func TestPluginConsentServiceDenyKeepsFieldsAndGrantStated(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", []string{"log"}, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: true, capabilities: []string{"log"}, tools: []string{testEchoTool},
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	before := f.requireEntry(f.readDeployment(), testEchoPlugin)

	svc := f.newGrantTestService()
	result, err := svc.Deny(context.Background(), testEchoPlugin)
	if err != nil {
		t.Fatalf("Deny() error = %v, want nil", err)
	}
	if result.PendingConvergence {
		t.Errorf("Deny() PendingConvergence = true, want false")
	}

	after := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if after.Enabled {
		t.Errorf("entry.Enabled = true, want false after Deny()")
	}
	if len(after.Grant.Capabilities) != 0 {
		t.Errorf("entry.Grant.Capabilities = %v, want empty after Deny()", after.Grant.Capabilities)
	}
	if !after.GrantStated {
		t.Errorf("entry.GrantStated = false, want true after Deny(): a decision WAS made")
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
	// Deny never loads the plugin's own plugin.json (see Deny's own doc
	// comment): its result View must say so explicitly rather than reading
	// as "declares nothing".
	if !result.View.DeclaredUnresolved {
		t.Errorf("Deny() View.DeclaredUnresolved = false, want true: Deny never resolves declarations")
	}
	if result.View.DeclaredUnresolvedReason != server.DeclaredUnresolvedNotInspected {
		t.Errorf("Deny() View.DeclaredUnresolvedReason = %q, want %q: nothing failed here, nobody looked",
			result.View.DeclaredUnresolvedReason, server.DeclaredUnresolvedNotInspected)
	}
}

// TestPluginConsentServiceDenyThenGrantReauthorizes is deny -> grant as a
// supported recovery path: after Deny (which keeps the grant block present
// but empty -- GrantStated stays true), a subsequent Grant must produce a
// fully authorized entry again.
func TestPluginConsentServiceDenyThenGrantReauthorizes(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.2.0", []string{"log"}, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: true, capabilities: []string{"log"}, tools: []string{testEchoTool},
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	svc := f.newGrantTestService()
	if _, err := svc.Deny(context.Background(), testEchoPlugin); err != nil {
		t.Fatalf("Deny() error = %v, want nil", err)
	}
	denied := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if denied.Enabled {
		t.Fatalf("entry.Enabled = true after Deny(), want false")
	}

	result, err := svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{Capabilities: []string{"log"}})
	if err != nil {
		t.Fatalf("Grant() error = %v, want nil: deny -> grant must be a supported recovery path", err)
	}
	if result.PendingConvergence {
		t.Errorf("Grant() PendingConvergence = true, want false")
	}
	if result.View.State != loader.StateLoaded {
		t.Errorf("Grant() View.State = %q, want %q: the re-granted entry should converge and load", result.View.State, loader.StateLoaded)
	}

	after := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if !after.Enabled {
		t.Errorf("entry.Enabled = false, want true after re-grant")
	}
	wantCaps := []string{"log"}
	if !slices.Equal(after.Grant.Capabilities, wantCaps) {
		t.Errorf("entry.Grant.Capabilities = %v, want %v after re-grant", after.Grant.Capabilities, wantCaps)
	}
}

// TestPluginConsentServiceGrantReportsPendingConvergenceOnBoundaryTimeout is
// the core invariant's outcome 3, exercised against the REAL taskgate: a
// task the fixture's own gate holds open (never retired) means Apply's
// boundary wait can never reach zero in flight, so it times out. Grant must
// report PendingConvergence=true with a nil error -- the write already
// landed -- not an error, and not a plain success.
func TestPluginConsentServiceGrantReportsPendingConvergenceOnBoundaryTimeout(t *testing.T) {
	// A short apply_wait_ms keeps this test fast: Apply only needs to wait
	// long enough to observe the held-open task and give up.
	f := newPluginFixture(t, 200)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	// Hold the gate open: a task that started and never ended, standing in
	// for a long-running task in flight when Grant tries to converge.
	end, err := f.gate.Begin()
	if err != nil {
		t.Fatalf("gate.Begin() error = %v, want nil", err)
	}
	t.Cleanup(end)

	svc := f.newGrantTestService()
	result, err := svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{})
	if err != nil {
		t.Fatalf("Grant() error = %v, want nil: the write already landed, this must be a ConsentResult not an error", err)
	}
	if !result.PendingConvergence {
		t.Fatalf("Grant() PendingConvergence = false, want true: a task is still in flight, convergence could not run")
	}
	if result.ConvergenceDetail == "" {
		t.Errorf("Grant() ConvergenceDetail is empty, want it to name why convergence did not run")
	}

	// The write in step 6 already landed regardless of convergence.
	written := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if !written.Enabled {
		t.Errorf("entry.Enabled = false, want true: the write happens before Apply, so it must have landed even though convergence did not run")
	}
}

// TestPluginConsentServiceGrantReportsFailedNotPendingWhenEntryFailsToActivate
// is the core invariant's outcome 4, exercised against a REAL activation
// failure: the DEPLOYMENT requires a signature (the loader's own keyring
// trusts a key, "require_signature": true), but the fixture package is
// never signed, so the loader's OWN LoadPackage call inside convergence
// (loader.go's prepare, keyed off l.keyring) refuses it. Grant's own
// consent-side LoadPackage call (step 3) deliberately uses a nil keyring --
// exactly the way List's declaration-reading already does -- so it still
// succeeds and the write still lands; only the LOADER's later, independent
// read enforces the signature. Convergence therefore runs to completion --
// Apply returns a non-nil error, since this entry really did fail -- but
// Grant must still report PendingConvergence=FALSE (the opposite of the
// boundary-timeout test above) with View.State="failed" and a detail naming
// why, never propagate Apply's own error as a write failure.
func TestPluginConsentServiceGrantReportsFailedNotPendingWhenEntryFailsToActivate(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	_, keyringPath := f.newKeyring("keyring.json")
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath, requireSignature: boolPtr(true)})
	// Deliberately never signed: no plugin.sig is written, so the loader's
	// own signature check (which the deployment now requires) fails it.
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	if status := f.application.Plugins().Status(); len(status) != 0 {
		t.Fatalf("fixture setup: Status() = %+v, want empty before Grant(): the entry starts disabled", status)
	}

	// keyringFn returns nil -- the same "skip signature verification" input
	// List's own declaration-reading uses -- so Grant's own LoadPackage
	// (step 3) can still read the plugin's declared capabilities even though
	// this package carries no valid signature; only the loader's later,
	// independent read enforces one.
	svc := f.newGrantTestService()
	result, err := svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{})
	if err != nil {
		t.Fatalf("Grant() error = %v, want nil: the write already landed and this entry's own failed state is "+
			"the honest answer, not a Go error", err)
	}
	if result.PendingConvergence {
		t.Fatalf("Grant() PendingConvergence = true, want false: convergence RAN, this entry just failed to " +
			"activate -- reporting it as pending would wait for a convergence that will never come again on its own")
	}
	if result.View.State != loader.StateFailed {
		t.Fatalf("Grant() View.State = %q, want %q", result.View.State, loader.StateFailed)
	}
	if result.View.Detail == "" {
		t.Errorf("Grant() View.Detail is empty, want it to name why the entry failed to activate")
	}

	// The write in step 6 already landed regardless of the activation
	// failure that followed it.
	written := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if !written.Enabled {
		t.Errorf("entry.Enabled = false, want true: the write happens before Apply, so it must have landed even though this entry then failed to activate")
	}
}

// --- gpc-task-3 review fixes ------------------------------------------------

const (
	// testConsentSecondPlugin and testConsentSecondTool name a SECOND
	// deployment entry, for the tests that need two rows to act on at once.
	// The committed guest binary self-describes as testEchoPlugin, so an
	// entry under this name never activates cleanly -- which is fine for
	// every test here, none of which asserts its loader state.
	testConsentSecondPlugin = "legion-test-plugin-b"
	testConsentSecondTool   = "echo_tool_b"
)

// testConsentLogger is the discarding logger every PluginConsentService test
// that does not inspect the log wires in. It is not optional: the
// constructor refuses a nil logger, because a convergence that reported
// errors and was recorded nowhere is the failure Important-2 was about.
func testConsentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// lockedBuffer is an io.Writer over a bytes.Buffer safe for a logger that
// may be written from more than one goroutine, so -race has nothing to say
// about a test that reads back what was logged.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newCapturingConsentLogger returns a logger and a func reading back
// everything written through it.
func newCapturingConsentLogger() (*slog.Logger, func() string) {
	sink := &lockedBuffer{}
	return slog.New(slog.NewTextHandler(sink, nil)), sink.String
}

// serveNeverResponding starts a plaintext server that accepts a request and
// answers it only once the CLIENT gives up, so a fetch against it can end no
// way but on its own deadline. Holding the handler on the request's own ctx
// (rather than on a channel the test closes) is what lets httptest's Close
// return: the moment the fetch's derived deadline fires, the connection
// drops and the handler returns with it.
func serveNeverResponding(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPluginConsentServiceGrantReportsFailedNotPendingWhenConvergenceFetchTimesOut
// is gpc-task-3 review Critical-1: the four-outcome discrimination must not
// be inferred from ctx sentinels.
//
// The deployment holds a SECOND, enabled remote entry whose source never
// answers, so the fetch inside convergence dies on the deadline fetch.Fetch
// derives from the very ctx Apply was given. Apply's returned error
// therefore wraps context.DeadlineExceeded even though convergence RAN to
// completion -- structurally identical, to errors.Is, to the error a
// boundary wait that never reached fn produces.
//
// The granted entry meanwhile fails on its own (this deployment requires a
// signature the fixture package does not carry, exactly as the
// activation-failure test above arranges), so the honest answer is outcome
// 4: convergence ran, this entry failed. Reporting it as pending -- which
// keying on context.DeadlineExceeded did -- leaves an operator waiting for a
// convergence that already happened and will never come again on its own.
func TestPluginConsentServiceGrantReportsFailedNotPendingWhenConvergenceFetchTimesOut(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	_, keyringPath := f.newKeyring("keyring.json")
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	hung := serveNeverResponding(t)
	// A 200ms fetch timeout keeps this fast: the download only has to run
	// long enough to die on its own deadline rather than on anything else.
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath, requireSignature: boolPtr(true)},
		fmt.Sprintf("\"cache\": %s", jsonString(cacheDir)),
		`"allow_insecure_sources": true`,
		`"fetch": {"timeout_ms": 200, "max_bytes": 33554432}`)
	// Deliberately never signed, so the LOADER's own signature check (which
	// this deployment requires) fails it while Grant's own nil-keyring read
	// still succeeds -- see the activation-failure test above.
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
	f.writeManifest(
		manifestEntry{
			name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
		},
		manifestEntry{
			name: testConsentSecondPlugin, source: hung.URL + "/never.tgz", enabled: true,
			tools: []string{testConsentSecondTool}, digest: digestOfArchive([]byte("never served")),
		},
	)
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	logger, logged := newCapturingConsentLogger()
	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
		func() *sign.Keyring { return nil }, f.resolveFixtureRemote(), logger)
	result, err := svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{})
	if err != nil {
		t.Fatalf("Grant() error = %v, want nil: the write already landed, so this is a ConsentResult not an error", err)
	}
	if result.PendingConvergence {
		t.Fatalf("Grant() PendingConvergence = true, want false: convergence RAN -- the deadline that fired "+
			"was the DOWNLOAD's, inside it, not a boundary wait that never reached it (detail=%q)",
			result.ConvergenceDetail)
	}
	if result.View.State != loader.StateFailed {
		t.Fatalf("Grant() View.State = %q, want %q", result.View.State, loader.StateFailed)
	}
	// Important-2: the convergence error is kept, not dropped -- both on the
	// wire and in the log.
	if result.ConvergenceDetail == "" {
		t.Errorf("Grant() ConvergenceDetail is empty, want the errors convergence reported")
	}
	if out := logged(); !strings.Contains(out, testEchoPlugin) {
		t.Errorf("nothing naming plugin %q was logged for a convergence that reported errors; log = %q",
			testEchoPlugin, out)
	}
}

// TestPluginConsentServiceGrantReportsPendingWhenAnotherApplyHoldsTheGate is
// gpc-task-3 review Minor-5: taskgate.ErrApplyInProgress was exported for
// exactly this path and nothing pinned it -- the line consuming it could be
// deleted with every test still green.
//
// A second apply already inside its fn holds the gate, so Grant's own Apply
// is refused before it can converge anything. That is outcome 3 (nothing was
// applied), and it must be reported as pending rather than as this entry's
// state, which is still whatever the previous convergence left.
//
// It also pins Minor-7: the pending View carries the facts already on disk.
func TestPluginConsentServiceGrantReportsPendingWhenAnotherApplyHoldsTheGate(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log"}, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	inside := make(chan struct{})
	release := make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- f.gate.ApplyAtBoundary(context.Background(), time.Minute, func() error {
			close(inside)
			<-release
			return nil
		})
	}()
	// No sleep and no polling: the other apply has provably entered fn by
	// the time this returns, so the gate is provably held.
	<-inside

	svc := f.newGrantTestService()
	result, err := svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{Capabilities: []string{"log"}})
	close(release)
	if holdErr := <-held; holdErr != nil {
		t.Fatalf("the apply holding the gate failed: %v", holdErr)
	}
	if err != nil {
		t.Fatalf("Grant() error = %v, want nil: the write already landed", err)
	}
	if !result.PendingConvergence {
		t.Fatalf("Grant() PendingConvergence = false, want true: another apply held the gate, so nothing was applied")
	}
	if !strings.Contains(result.ConvergenceDetail, "already being applied") {
		t.Errorf("Grant() ConvergenceDetail = %q, want it to name the concurrent apply", result.ConvergenceDetail)
	}
	// Minor-7: Name and the just-written Granted* are KNOWN here even though
	// nothing converged. A pending response with no name is one the GUI
	// cannot match back to the row it came from.
	if result.View.Name != testEchoPlugin {
		t.Errorf("Grant() pending View.Name = %q, want %q", result.View.Name, testEchoPlugin)
	}
	if !slices.Equal(result.View.GrantedCaps, []string{"log"}) {
		t.Errorf("Grant() pending View.GrantedCaps = %v, want [log]: it is already on disk", result.View.GrantedCaps)
	}
	if result.View.State != "" {
		t.Errorf("Grant() pending View.State = %q, want empty: no convergence produced any loader state", result.View.State)
	}
}

// grantBarrier releases once two goroutines have reached the same point, or
// after wait if only one ever does.
//
// It is how the concurrency test below observes an interleaving instead of
// racing for one: the two Grants are held together at a point INSIDE the
// read -> check -> write sequence, so a service with no lock is guaranteed
// to have both of them holding the same stale snapshot. The timeout is the
// other side of the same coin -- once the sequence really is serialized the
// second goroutine cannot reach the barrier at all, so the first has to be
// able to go on alone rather than deadlock.
type grantBarrier struct {
	mu      sync.Mutex
	arrived int
	both    chan struct{}
	wait    time.Duration
}

func newGrantBarrier(wait time.Duration) *grantBarrier {
	return &grantBarrier{both: make(chan struct{}), wait: wait}
}

func (b *grantBarrier) arrive() {
	b.mu.Lock()
	b.arrived++
	reached := b.arrived == 2
	if reached {
		close(b.both)
	}
	b.mu.Unlock()
	if reached {
		return
	}
	select {
	case <-b.both:
	case <-time.After(b.wait):
	}
}

// TestPluginConsentServiceConcurrentGrantsDoNotRevertEachOther is gpc-task-3
// review Important-3: consent.RefuseDeploymentChanged is a read-then-check
// with nothing holding the file between it and manifest.WriteDeployment, so
// two in-process authorizations of two DIFFERENT plugins could both pass the
// check and the loser's write could silently revert the winner's -- with
// both requests reporting success. A silent rollback on an authorization
// boundary is worse than a crash.
//
// keyringFn is called from inside Grant, after the snapshot read and before
// the write, which makes it the barrier point: both goroutines are held
// there together, so without s.mu both hold a snapshot that predates the
// other's write. The run then ends one of three ways and ALL of them are
// failures this test catches: the compare-and-swap notices and one Grant
// returns a conflict; it does not, and one authorization is gone from disk;
// or the two atomic rewrites collide in the filesystem (which is what
// Windows reports, where a rename onto a file another handle is replacing
// fails outright). All three are the same unsynchronized read-modify-write.
// With s.mu the second goroutine never reaches the barrier while the first
// holds the lock, so it reads AFTER that write and both survive.
func TestPluginConsentServiceConcurrentGrantsDoNotRevertEachOther(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo-a", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log"}, []string{testEchoTool})
	f.writePackage("echo-b", testEchoWasm, testConsentSecondPlugin, "1.0.0",
		[]string{"log"}, []string{testConsentSecondTool})
	f.writeManifest(
		manifestEntry{name: testEchoPlugin, source: "echo-a", enabled: false, tools: []string{testEchoTool}, omitGrant: true},
		manifestEntry{
			name: testConsentSecondPlugin, source: "echo-b", enabled: false,
			tools: []string{testConsentSecondTool}, omitGrant: true,
		},
	)
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	barrier := newGrantBarrier(time.Second)
	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
		func() *sign.Keyring {
			barrier.arrive()
			return nil
		}, loader.RemoteConfig{}, testConsentLogger())

	errs := make(chan error, 2)
	for _, name := range []string{testEchoPlugin, testConsentSecondPlugin} {
		go func() {
			_, err := svc.Grant(context.Background(), name, server.GrantRequest{Capabilities: []string{"log"}})
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Grant() error = %v, want nil: two authorizations of two different plugins must both succeed", err)
		}
	}

	dep := f.readDeployment()
	for _, name := range []string{testEchoPlugin, testConsentSecondPlugin} {
		entry := f.requireEntry(dep, name)
		if !entry.Enabled {
			t.Errorf("entry %q Enabled = false, want true: one concurrent authorization silently reverted the other", name)
		}
		if !slices.Equal(entry.Grant.Capabilities, []string{"log"}) {
			t.Errorf("entry %q Grant.Capabilities = %v, want [log]", name, entry.Grant.Capabilities)
		}
	}
}

// TestPluginConsentServiceGrantReportsDeclarationAndRecordsGrantStated pins
// two of gpc-task-3 review Minor-6's surviving mutations at once: deleting
// Grant's `result.View.Declared* = pm.*` lines, and deleting its
// `e.GrantStated = true`, both left every test green.
//
// The plugin declares two hosts and is granted one, so Declared and Granted
// cannot both be read off a single field.
func TestPluginConsentServiceGrantReportsDeclarationAndRecordsGrantStated(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackageWithNetwork("echo", testEchoWasm, testEchoPlugin, "1.0.0",
		[]string{"log"}, []string{testEchoTool},
		manifest.Network{AllowedHosts: []string{"jira.example.com", "wiki.example.com"}},
		manifest.Filesystem{AllowedPaths: []string{"/srv/echo"}})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	svc := f.newGrantTestService()
	result, err := svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{
		Capabilities: []string{"log"},
		AllowedHosts: []string{"jira.example.com"},
		AllowedPaths: []string{"/srv/echo"},
	})
	if err != nil {
		t.Fatalf("Grant() error = %v, want nil", err)
	}
	if result.View.DeclaredUnresolved {
		t.Errorf("Grant() View.DeclaredUnresolved = true, want false: Grant loaded the package, so it knows")
	}
	if !slices.Equal(result.View.DeclaredCaps, []string{"log"}) {
		t.Errorf("Grant() View.DeclaredCaps = %v, want [log]", result.View.DeclaredCaps)
	}
	wantDeclaredHosts := []string{"jira.example.com", "wiki.example.com"}
	if !slices.Equal(result.View.DeclaredHosts, wantDeclaredHosts) {
		t.Errorf("Grant() View.DeclaredHosts = %v, want %v", result.View.DeclaredHosts, wantDeclaredHosts)
	}
	if !slices.Equal(result.View.DeclaredPaths, []string{"/srv/echo"}) {
		t.Errorf("Grant() View.DeclaredPaths = %v, want [/srv/echo]", result.View.DeclaredPaths)
	}
	if !slices.Equal(result.View.GrantedHosts, []string{"jira.example.com"}) {
		t.Errorf("Grant() View.GrantedHosts = %v, want [jira.example.com]", result.View.GrantedHosts)
	}
	if slices.Equal(result.View.DeclaredHosts, result.View.GrantedHosts) {
		t.Fatalf("DeclaredHosts and GrantedHosts must be reported separately, both came back %v", result.View.DeclaredHosts)
	}

	entry := f.requireEntry(f.readDeployment(), testEchoPlugin)
	if !entry.GrantStated {
		t.Errorf("entry.GrantStated = false, want true: an authorization IS a stated decision")
	}
}

// TestPluginConsentServiceGrantRefusesADuplicateCapabilityOverHTTP is
// gpc-task-3 review Minor-9: `agent plugins grant --capabilities log,log`
// was refused by splitFlagList while the JSON body
// {"capabilities":["log","log"]} was accepted and written to plugins.json
// verbatim -- a duplicate makes neither direction of ResolveCapabilities'
// set-equality test notice anything missing. Both paths now go through the
// same consent.NormalizeList.
func TestPluginConsentServiceGrantRefusesADuplicateCapabilityOverHTTP(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log"}, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json before grant: %v", err)
	}

	svc := f.newGrantTestService()
	_, err = svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{Capabilities: []string{"log", "log"}})
	if err == nil {
		t.Fatal(`Grant() error = nil, want an error: "log" is named twice`)
	}
	if !strings.Contains(err.Error(), "more than once") {
		t.Errorf("Grant() error = %v, want it to say a capability was named more than once", err)
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json after grant: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("plugins.json changed after a refused duplicate capability:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestPluginConsentServiceReportsUnknownPluginAsNotFound is gpc-task-3
// review Minor-8 on both mutating methods: an unknown name is not a
// malformed request, and the handler can only answer 404 instead of a
// blanket 400 because the error carries its class.
func TestPluginConsentServiceReportsUnknownPluginAsNotFound(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log"}, []string{testEchoTool})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	svc := f.newGrantTestService()

	_, grantErr := svc.Grant(context.Background(), "no-such-plugin", server.GrantRequest{})
	if !errors.Is(grantErr, server.ErrPluginNotFound) {
		t.Errorf("Grant(no-such-plugin) error = %v, want it to wrap server.ErrPluginNotFound", grantErr)
	}
	_, denyErr := svc.Deny(context.Background(), "no-such-plugin")
	if !errors.Is(denyErr, server.ErrPluginNotFound) {
		t.Errorf("Deny(no-such-plugin) error = %v, want it to wrap server.ErrPluginNotFound", denyErr)
	}
}

// TestPluginConsentServiceReportsAMissingLoaderAsUnavailable is the last of
// Minor-8's classes: a process with no loader attached (mid-shutdown, say)
// got a well-formed request it simply cannot serve right now, which is a 503
// rather than "your request is wrong".
func TestPluginConsentServiceReportsAMissingLoaderAsUnavailable(t *testing.T) {
	svc := NewPluginConsentService("does-not-matter.json", "root",
		func() *loader.Loader { return nil }, func() *sign.Keyring { return nil },
		loader.RemoteConfig{}, testConsentLogger())

	if _, err := svc.Grant(context.Background(), "any", server.GrantRequest{}); !errors.Is(err, server.ErrPluginUnavailable) {
		t.Errorf("Grant() error = %v, want it to wrap server.ErrPluginUnavailable", err)
	}
	if _, err := svc.Deny(context.Background(), "any"); !errors.Is(err, server.ErrPluginUnavailable) {
		t.Errorf("Deny() error = %v, want it to wrap server.ErrPluginUnavailable", err)
	}
}

// --- Task 2: PluginConsentService.Resolve -----------------------------------

// consentFixture is Resolve's fixture: a single remote deployment entry whose
// package is served by origin and not yet cached, plus a PluginConsentService
// wired against it the same way the List/Grant tests above wire theirs (see
// resolveFixtureRemote) -- Resolve's whole point is fetching a package
// assembly never touched, so the entry is left disabled and origin stays
// reachable until a test closes it itself.
type consentFixture struct {
	*pluginFixture
	svc        *PluginConsentService
	pluginName string
	origin     *httptest.Server
}

// newConsentFixture builds a consentFixture around a healthy, unsigned-but-
// untrusted-doesn't-matter package: the deployment does not require
// signatures (requireSignature: false) and the service's own keyringFn
// returns nil, exactly like every List/Grant test fixture above -- signature
// verification is exercised separately, by
// newConsentFixtureWithUntrustedPackage.
func newConsentFixture(t *testing.T) *consentFixture {
	t.Helper()

	f := newPluginFixture(t, 30_000)
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log"}, []string{testEchoTool})
	f.signPackageWithAnyKey("staging")
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	srv := serveArchive(t, archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeInstallConfig(signaturePolicy{requireSignature: boolPtr(false)}, cacheDir)
	// enabled: false keeps assemble() from ever fetching this entry itself --
	// Resolve is what is supposed to put it in the cache, not assembly.
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: srv.URL + "/echo.tgz", enabled: false,
		tools: []string{testEchoTool}, digest: digest, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
		func() *sign.Keyring { return nil }, f.resolveFixtureRemote(), testConsentLogger())
	return &consentFixture{pluginFixture: f, svc: svc, pluginName: testEchoPlugin, origin: srv}
}

// newConsentFixtureWithUntrustedPackage is newConsentFixture, except the
// service's own keyringFn returns a REAL keyring that trusts a different key
// than the one the package is signed with -- the "signature does not verify"
// path manifest.ErrUntrustedPackage marks (see that sentinel's own doc
// comment). The deployment's own assembly-time signature policy is left off
// (requireSignature: false), same as newConsentFixture: the entry is disabled
// so assembly never loads it, and only Resolve's own LoadPackage call is
// under test here.
func newConsentFixtureWithUntrustedPackage(t *testing.T) *consentFixture {
	t.Helper()

	f := newPluginFixture(t, 30_000)
	_, keyringPath := f.newKeyring("keyring.json")
	keyringData, err := os.ReadFile(keyringPath)
	if err != nil {
		t.Fatalf("read keyring %s: %v", keyringPath, err)
	}
	keyring, err := sign.ParseKeyring(keyringData)
	if err != nil {
		t.Fatalf("parse keyring %s: %v", keyringPath, err)
	}

	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log"}, []string{testEchoTool})
	// Signed with a freshly generated key that was never registered anywhere:
	// the keyring above trusts a DIFFERENT public key under the same
	// testPluginKeyID, so Verify fails on "signature does not verify against
	// key", not on an unknown key id.
	f.signPackageWithAnyKey("staging")
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	srv := serveArchive(t, archive)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeInstallConfig(signaturePolicy{requireSignature: boolPtr(false)}, cacheDir)
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: srv.URL + "/echo.tgz", enabled: false,
		tools: []string{testEchoTool}, digest: digest, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
		func() *sign.Keyring { return keyring }, f.resolveFixtureRemote(), testConsentLogger())
	return &consentFixture{pluginFixture: f, svc: svc, pluginName: testEchoPlugin, origin: srv}
}

// TestPluginConsentServiceResolveFillsDeclarationsWithoutTouchingTheManifest
// is invariant 1 and invariant 4 together: a successful Resolve against a
// remote entry with a reachable origin fetches and verifies the package,
// reports its real declared capabilities, and leaves plugins.json byte for
// byte as it found it -- Resolve is "look", never "write".
func TestPluginConsentServiceResolveFillsDeclarationsWithoutTouchingTheManifest(t *testing.T) {
	// 一条远程条目，缓存未命中，源站可达
	f := newConsentFixture(t) // 既有夹具助手
	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json: %v", err)
	}

	view, err := f.svc.Resolve(context.Background(), f.pluginName)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if view.DeclaredUnresolved {
		t.Error("view.DeclaredUnresolved = true after a successful Resolve, want false")
	}
	if view.DeclaredUnresolvedReason != "" {
		t.Errorf("view.DeclaredUnresolvedReason = %q, want empty: a resolved view has no unresolved reason", view.DeclaredUnresolvedReason)
	}
	if len(view.DeclaredCaps) == 0 {
		t.Error("view.DeclaredCaps is empty after a successful Resolve, want the plugin's declared capabilities")
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("re-read plugins.json: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("plugins.json changed during Resolve:\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestPluginConsentServiceResolveDoesNotRefetchOnACacheHit pins
// resolvePluginPackageDir's cache-hit short circuit reached through Resolve:
// the first call fetches and caches the package, and the second -- with the
// origin now offline -- must be served from that cache rather than attempt a
// second fetch. 缓存命中不联网：先取回一次填满缓存，然后 CLOSE 掉源站再取回一次。
// 任何意外的第二次 fetch 都会 connection-refused 而不是静默成功。
func TestPluginConsentServiceResolveDoesNotRefetchOnACacheHit(t *testing.T) {
	f := newConsentFixture(t)
	if _, err := f.svc.Resolve(context.Background(), f.pluginName); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	f.origin.Close() // 源站下线

	view, err := f.svc.Resolve(context.Background(), f.pluginName)
	if err != nil {
		t.Fatalf("second Resolve after the origin went away = %v, want it served from cache", err)
	}
	if view.DeclaredUnresolved {
		t.Error("view.DeclaredUnresolved = true on a cache hit, want false")
	}
}

// TestPluginConsentServiceResolveReportsAnUntrustedPackage is Resolve's new
// error class: a package whose signature does not verify against the
// service's keyring must come back with manifest.ErrUntrustedPackage on the
// error chain, so a caller can tell "not trustworthy" apart from "could not
// be obtained" and refrain from offering a pointless retry -- and, like every
// other Resolve failure, must leave plugins.json untouched.
func TestPluginConsentServiceResolveReportsAnUntrustedPackage(t *testing.T) {
	f := newConsentFixtureWithUntrustedPackage(t) // 源站给出的包签名不被 keyring 信任
	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json: %v", err)
	}

	_, err = f.svc.Resolve(context.Background(), f.pluginName)
	if err == nil {
		t.Fatal("Resolve on an untrusted package = nil error, want an error")
	}
	if !errors.Is(err, manifest.ErrUntrustedPackage) {
		t.Errorf("Resolve error = %v, want it to wrap manifest.ErrUntrustedPackage", err)
	}
	// Both sentinels must be on the same error: manifest.ErrUntrustedPackage is
	// the classification, server.ErrPluginUntrusted is what pluginConsentStatus
	// actually keys its 422 response on (internal/server/plugins.go). A test
	// that only checked the first would stay green even if Resolve stopped
	// attaching the second, and an untrusted package would then report 400
	// instead of 422 in production.
	if !errors.Is(err, server.ErrPluginUntrusted) {
		t.Errorf("Resolve error = %v, want it to also wrap server.ErrPluginUntrusted", err)
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("re-read plugins.json: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("plugins.json changed while Resolve was rejecting an untrusted package")
	}
}

// TestPluginConsentServiceResolveReportsAnUnknownEntry mirrors Grant/Deny's
// own "no such plugin" classification (server.ErrPluginNotFound) through the
// same consent.FindEntry call.
func TestPluginConsentServiceResolveReportsAnUnknownEntry(t *testing.T) {
	f := newConsentFixture(t)
	_, err := f.svc.Resolve(context.Background(), "no-such-plugin")
	if !errors.Is(err, server.ErrPluginNotFound) {
		t.Errorf("Resolve error = %v, want it to wrap server.ErrPluginNotFound", err)
	}
}

// TestNewPluginConsentServicePanicsOnANilLogger pins the constructor's
// fail-loud refusal: a nil logger would silently discard every record of a
// convergence that reported errors, which is the very thing Important-2
// added the logging for.
func TestNewPluginConsentServicePanicsOnANilLogger(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewPluginConsentService(nil logger) did not panic, want a panic naming the nil logger")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "logger") {
			t.Fatalf("panic = %q, want it to name the nil logger", msg)
		}
	}()
	NewPluginConsentService("m.json", "root", func() *loader.Loader { return nil },
		func() *sign.Keyring { return nil }, loader.RemoteConfig{}, nil)
}

// TestPluginConsentServiceResolveEvictsAnUntrustedPackageFromTheCache: a
// package that failed signature verification is poison, and until this it
// stayed on disk forever — the next List reported the row as load_failed and
// the panel stopped offering to fetch it, so those bytes sat in a directory
// the deployment reads from with nothing able to remove them.
func TestPluginConsentServiceResolveEvictsAnUntrustedPackageFromTheCache(t *testing.T) {
	f := newConsentFixtureWithUntrustedPackage(t)

	if _, err := f.svc.Resolve(context.Background(), f.pluginName); err == nil {
		t.Fatal("Resolve on an untrusted package = nil error, want an error")
	}

	dep, err := readPluginDeployment(f.manifestPath)
	if err != nil {
		t.Fatalf("read deployment: %v", err)
	}
	digest := dep.Plugins[0].Digest
	cached, err := f.resolveFixtureRemote().Cache.Has(digest)
	if err != nil {
		t.Fatalf("cache lookup %s: %v", digest, err)
	}
	if cached {
		t.Error("the untrusted package is still in the cache after Resolve refused it")
	}
}

// TestPluginConsentServiceResolveKeepsTheCacheWhenTheFailureIsNotATrustFailure
// is the other half of the rule. A package that merely will not load — a
// corrupt module, a missing file — is not a trust problem, and evicting it
// would only make the next attempt download an identical broken package.
func TestPluginConsentServiceResolveKeepsTheCacheWhenTheFailureIsNotATrustFailure(t *testing.T) {
	f := newConsentFixture(t)
	cache := f.resolveFixtureRemote().Cache

	// A first Resolve fetches and caches the healthy package.
	if _, err := f.svc.Resolve(context.Background(), f.pluginName); err != nil {
		t.Fatalf("Resolve on a healthy package: %v", err)
	}
	dep, err := readPluginDeployment(f.manifestPath)
	if err != nil {
		t.Fatalf("read deployment: %v", err)
	}
	digest := dep.Plugins[0].Digest
	dir := cache.Dir(digest)

	// Break the CACHED copy in a way that is not a signature failure — and
	// keep all three files present, because a MISSING file makes the entry
	// incomplete, which reads as a cache miss and simply re-downloads. Corrupt
	// content instead: the digest in plugin.json no longer matches the module,
	// which LoadPackage refuses without ErrUntrustedPackage.
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), []byte("not a wasm module"), 0o600); err != nil {
		t.Fatalf("corrupt the cached package: %v", err)
	}

	if _, err := f.svc.Resolve(context.Background(), f.pluginName); err == nil {
		t.Fatal("Resolve on a broken cached package = nil error, want an error")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("stat %s: %v; a package that merely fails to load must stay cached — "+
			"evicting it only re-downloads the same broken bytes", dir, err)
	}
}

// TestPluginConsentServiceGrantRecordsASubsetOfDeclaredExtensions is the HTTP
// half of the extension grant, and it pins the rule that separates extensions
// from capabilities: a plugin may declare an extension and be granted NONE of
// it. The plugin still contributes its tools; it simply is not consulted at
// that seam.
func TestPluginConsentServiceGrantRecordsASubsetOfDeclaredExtensions(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackageWithExtensions("echo", testEchoWasm, testEchoPlugin, "1.0.0",
		[]string{testEchoTool}, []string{"observe"})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	svc := f.newGrantTestService()
	result, err := svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{})
	if err != nil {
		t.Fatalf("Grant() error = %v, want nil", err)
	}
	if !slices.Equal(result.View.DeclaredExtensions, []string{"observe"}) {
		t.Errorf("Grant() View.DeclaredExtensions = %v, want [observe]", result.View.DeclaredExtensions)
	}
	if len(result.View.GrantedExtensions) != 0 {
		t.Errorf("Grant() View.GrantedExtensions = %v, want none: an absent list grants nothing",
			result.View.GrantedExtensions)
	}
	if got := f.requireEntry(f.readDeployment(), testEchoPlugin).Grant.Extensions; len(got) != 0 {
		t.Errorf("entry.Grant.Extensions = %v, want none", got)
	}
}

func TestPluginConsentServiceGrantRecordsAGrantedExtension(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackageWithExtensions("echo", testEchoWasm, testEchoPlugin, "1.0.0",
		[]string{testEchoTool}, []string{"observe"})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	svc := f.newGrantTestService()
	result, err := svc.Grant(context.Background(), testEchoPlugin,
		server.GrantRequest{Extensions: []string{"observe"}})
	if err != nil {
		t.Fatalf("Grant() error = %v, want nil", err)
	}
	if !slices.Equal(result.View.GrantedExtensions, []string{"observe"}) {
		t.Errorf("Grant() View.GrantedExtensions = %v, want [observe]", result.View.GrantedExtensions)
	}
	if got := f.requireEntry(f.readDeployment(), testEchoPlugin).Grant.Extensions; !slices.Equal(got, []string{"observe"}) {
		t.Errorf("entry.Grant.Extensions = %v, want [observe]", got)
	}
}

// TestPluginConsentServiceGrantRefusesAnUndeclaredExtensionOverHTTP: the same
// refusal the CLI gives, on the same shared consent rule. A grant naming a
// seam the plugin never asked for is a config error, and writing it would
// leave plugins.json claiming an authorization the loader then refuses.
func TestPluginConsentServiceGrantRefusesAnUndeclaredExtensionOverHTTP(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackageWithExtensions("echo", testEchoWasm, testEchoPlugin, "1.0.0",
		[]string{testEchoTool}, nil)
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	svc := f.newGrantTestService()
	_, err := svc.Grant(context.Background(), testEchoPlugin,
		server.GrantRequest{Extensions: []string{"observe"}})
	if err == nil {
		t.Fatal("Grant() with an undeclared extension = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "observe") {
		t.Errorf("Grant() error = %v, want it to name the extension", err)
	}
	if got := f.requireEntry(f.readDeployment(), testEchoPlugin).Grant.Extensions; len(got) != 0 {
		t.Errorf("entry.Grant.Extensions = %v, want none: a refused grant must not be written", got)
	}
}

// TestPluginConsentServiceListSurfacesExtensionsSeparately: declared and
// granted are two different facts on this seam too, and the consent dialog
// renders the checkbox from the first and its state from the second.
func TestPluginConsentServiceListSurfacesExtensionsSeparately(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackageWithExtensions("echo", testEchoWasm, testEchoPlugin, "1.0.0",
		[]string{testEchoTool}, []string{"observe"})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	svc := f.newGrantTestService()
	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if len(views) != 1 {
		t.Fatalf("List() returned %d views, want 1", len(views))
	}
	if !slices.Equal(views[0].DeclaredExtensions, []string{"observe"}) {
		t.Errorf("List() DeclaredExtensions = %v, want [observe]", views[0].DeclaredExtensions)
	}
	if len(views[0].GrantedExtensions) != 0 {
		t.Errorf("List() GrantedExtensions = %v, want none: nothing has been granted yet",
			views[0].GrantedExtensions)
	}
}
