package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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

// noConsentTrustSet is the loader.TrustSet of a deployment that recognises no
// signing key at all: a zero manifest.TrustInput, which makes every package
// manifest.ProvenanceUnsigned. It is what the majority of the fixtures below
// want -- they are about declarations, grants and convergence, not provenance.
func noConsentTrustSet() (manifest.TrustInput, error) { return manifest.TrustInput{}, nil }

// consentTrustSetOf is the loader.TrustSet of a deployment whose trust set is
// exactly keyring, with publishers as the display names that go with its key
// ids. A nil publishers map is the deployment whose trust set came from a local
// keyring document alone: those register key ids and public keys and carry no
// names (see trustlist.Merge, which takes the names from the fetched list
// half).
func consentTrustSetOf(keyring *sign.Keyring, publishers map[sign.KeyID]string) loader.TrustSet {
	return func() (manifest.TrustInput, error) {
		return manifest.TrustInput{Keyring: keyring, Publishers: publishers}, nil
	}
}

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

	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins, noConsentTrustSet, loader.RemoteConfig{}, testConsentLogger())
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
		func() *loader.Loader { return nil }, noConsentTrustSet, loader.RemoteConfig{}, testConsentLogger())
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
		f.application.Plugins, noConsentTrustSet, loader.RemoteConfig{}, testConsentLogger())
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

	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins, noConsentTrustSet, loader.RemoteConfig{}, testConsentLogger())
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
		noConsentTrustSet, remote, testConsentLogger())
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
		noConsentTrustSet, f.resolveFixtureRemote(), testConsentLogger())
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
		noConsentTrustSet, f.resolveFixtureRemote(), testConsentLogger())
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
		noConsentTrustSet, loader.RemoteConfig{}, testConsentLogger())
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
		noConsentTrustSet, f.resolveFixtureRemote(), testConsentLogger())
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

	// The trust set provider answers with a nil keyring -- the same "skip
	// signature verification" input
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
		noConsentTrustSet, f.resolveFixtureRemote(), logger)
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
// The trust set provider is called from inside Grant, after the snapshot read
// and before the write, which makes it the barrier point: both goroutines are held
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
		func() (manifest.TrustInput, error) {
			barrier.arrive()
			return manifest.TrustInput{}, nil
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
		func() *loader.Loader { return nil }, noConsentTrustSet,
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
// signatures (requireSignature: false) and the service's own trust set is
// noConsentTrustSet, exactly like every List/Grant test fixture above -- signature
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
		noConsentTrustSet, f.resolveFixtureRemote(), testConsentLogger())
	return &consentFixture{pluginFixture: f, svc: svc, pluginName: testEchoPlugin, origin: srv}
}

// newConsentFixtureWithUntrustedPackage is newConsentFixture, except the
// service's own trust set holds a REAL keyring that trusts a different key
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
		consentTrustSetOf(keyring, nil), f.resolveFixtureRemote(), testConsentLogger())
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
// service's trust set must come back with manifest.ErrUntrustedPackage on the
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
		noConsentTrustSet, loader.RemoteConfig{}, nil)
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

// TestListDoesNotCallAnUntrustedPackageMerelyUncached 来自 V1 真机验证。
//
// 真机上把一个由**不受信任的密钥**签名的包挂上去（require_signature: true），
// `GET /v1/plugins` 回的是：
//
//	state = failed
//	detail = ... 「key id "attacker" is not in the keyring」
//	declared_unresolved_reason = "not_cached"
//
// 最后一行是错的，而且是会误导人的那种错。`not_cached` 在契约里的意思是**「包取得到、
// 什么都没出错、取一下就好」**，而 GUI 的插件面板**只在这个原因上给「获取」按钮**
// （见 PluginsPage.tsx 里那段注释：一个按不动的按钮正是这个面板要避免的谎）。于是
// 运维看到的是「远程包尚未缓存」加一个按钮，按下去重新下载、再次被拒，永远如此——
// 而真相是供应链信任失败，按钮救不了。
//
// 分类逻辑问的是「缓存里有没有」，而被拒的包当然不在缓存里。要问的是**「取一次能不能
// 解决」**：加载器刚刚就取过、并且拒了。
func TestListDoesNotCallAnUntrustedPackageMerelyUncached(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	_, keyringPath := f.newKeyring("keyring.json")
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log"}, []string{testEchoTool})
	// 用一把从未登记的钥匙签：keyring 里那把是另一个公钥。
	f.signPackageWithAnyKey("staging")
	archive := f.archivePackage("staging")
	digest := digestOfArchive(archive)
	srv := serveArchive(t, archive)
	t.Cleanup(srv.Close)

	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath, requireSignature: boolPtr(true)},
		fmt.Sprintf("\"cache\": %s", jsonString(cacheDir)),
		`"allow_insecure_sources": true`)
	// enabled: true —— 加载器会真的去取，取回来之后因为签名不被信任而拒掉。
	// 这正是真机上的形状，也是与既有「从没取过」用例的分界。
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: srv.URL + "/echo.tgz", enabled: true,
		capabilities: []string{"log"}, tools: []string{testEchoTool}, digest: digest,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	cfg, err := config.Load(context.Background(), config.Options{Path: f.configPath})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	keyring, _, err := resolvePluginKeyring(cfg.Plugins)
	if err != nil {
		t.Fatalf("resolvePluginKeyring: %v", err)
	}
	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
		consentTrustSetOf(keyring, nil), f.resolveFixtureRemote(), testConsentLogger())

	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1: %+v", len(views), views)
	}
	got := views[0]

	if got.DeclaredUnresolvedReason == server.DeclaredUnresolvedNotCached {
		t.Errorf("DeclaredUnresolvedReason = %q for a package the loader fetched and REFUSED as untrusted.\n"+
			"That reason means \"obtainable, nothing went wrong, a fetch may fix it\", and the plugin panel "+
			"offers its fetch button on that reason and no other — so the operator gets a button that "+
			"re-downloads and is refused again, forever.\nstate = %q\ndetail = %s",
			got.DeclaredUnresolvedReason, got.State, got.Detail)
	}
	if got.DeclaredError == "" {
		t.Error("DeclaredError is empty: the row says the declaration is unresolved but not why, " +
			"so the panel has nothing to show beyond a generic note")
	}
	// 这段文字是要显示给人的，不能带 `agent plugins status` 的 logfmt 标签。
	// 三个写点各写各的，所以每个都得有断言——只守住其中一个，另外两个照样能把
	// `error=…` 送到屏幕上（实测：只加一条断言时，另两处的变异都不红）。
	for _, label := range []string{"error=", "reason=", "waiting_on="} {
		if strings.HasPrefix(got.DeclaredError, label) {
			t.Errorf("declared_error 以 %q 开头：logfmt 标签跑到了给人看的字段上\n%s", label, got.DeclaredError)
		}
	}
}

// pluginStatusRow.Detail 是给 `agent plugins status` 用的**带标签**字符串
// （error= / reason= / waiting_on=，终端里可 grep 出一类）。那是正当的 CLI 呈现。
//
// 问题是同一个字段被 HTTP 直接当作给人看的说明送出去：V1 真机验证里，一个签名不被
// 信任的包在 GET /v1/plugins 的 detail 与 declared_error 里都是
// `error=load plugin package "C:\..."：…`——GUI 的插件面板把它当句子渲染，于是一个
// logfmt 片段出现在用户眼前，前面那个 `error=` 谁都不知道是什么。
//
// 两边要的本来就不是同一件东西：终端要标签（分类 + 可 grep），API 要原话（类别已经
// 在 state 字段里）。所以是两个字段，不是把标签在出口剥掉——剥字符串会在
// waiting_on= 与 reason= 拼在一起的那种行上出错。

func TestTheAPIDoesNotShipLogfmtLabelsToPeople(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log"}, []string{testEchoTool})
	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)})
	// 装一个**本地**包然后把 wasm 弄坏：加载器会带着一个真实的失败说明报 failed，
	// 而那正是会被送到界面上的那段文字。
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "staging", enabled: true,
		capabilities: []string{"log"}, tools: []string{testEchoTool},
	})
	if err := os.WriteFile(filepath.Join(f.root, "staging", "plugin.wasm"), []byte("not wasm"), 0o644); err != nil {
		t.Fatalf("corrupt the module: %v", err)
	}
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
		noConsentTrustSet, f.resolveFixtureRemote(), testConsentLogger())
	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1", len(views))
	}
	got := views[0]

	for _, field := range []struct {
		name  string
		value string
	}{{"detail", got.Detail}, {"declared_error", got.DeclaredError}} {
		if field.value == "" {
			continue
		}
		for _, label := range []string{"error=", "reason=", "waiting_on="} {
			if strings.HasPrefix(field.value, label) {
				t.Errorf("%s 以 %q 开头：这是 `agent plugins status` 的 logfmt 标签，"+
					"而这个字段是要显示给人的。\n%s = %s", field.name, label, field.name, field.value)
			}
		}
	}
	// 剥掉标签不能把话也剥掉：失败原因本身必须还在。
	if got.State == "failed" && got.Detail == "" {
		t.Error("state=failed 但 detail 是空的：界面上只剩一个「失败」，没有任何可查的东西")
	}
}

// TestTheCLIKeepsItsLabels 是上面那条的边界：终端那边**要**标签，它是分类也是
// grep 的抓手。把标签一并去掉会让 `agent plugins status` 退回到「一行字，说不清是
// 失败还是被禁用」。
func TestTheCLIKeepsItsLabels(t *testing.T) {
	t.Parallel()

	rows := mergePluginStatus(
		manifest.Deployment{Plugins: []manifest.Entry{{Name: "p", Source: "staging", Enabled: true}}},
		[]loader.InstanceStatus{{Name: "p", Version: "1.0.0", State: loader.StateFailed, LastError: "boom"}},
	)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if !strings.HasPrefix(rows[0].Detail, "error=") {
		t.Errorf("CLI 行的 Detail = %q，want 以 error= 开头", rows[0].Detail)
	}
}

// TestGrantsResponseCarriesNoLogfmtLabelsEither 是第三个写点：Grant/Deny 回的那个
// view 也带 State/Detail，而它同样直接进 GUI。
//
// 单独一条，是因为 List 的断言守不住它——变异实测：只改 Grant 那一行，List 的两条
// 测试全绿。
func TestGrantsResponseCarriesNoLogfmtLabelsEither(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("staging", testEchoWasm, testEchoPlugin, "1.0.0", []string{"log"}, []string{testEchoTool})
	f.writeSignatureConfig(30_000, signaturePolicy{requireSignature: boolPtr(false)})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "staging", enabled: true,
		capabilities: []string{"log"}, tools: []string{testEchoTool},
	})
	if err := os.WriteFile(filepath.Join(f.root, "staging", "plugin.wasm"), []byte("not wasm"), 0o644); err != nil {
		t.Fatalf("corrupt the module: %v", err)
	}
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
		noConsentTrustSet, f.resolveFixtureRemote(), testConsentLogger())
	result, err := svc.Deny(context.Background(), testEchoPlugin)
	if err != nil {
		t.Fatalf("Deny() error = %v, want nil", err)
	}
	for _, label := range []string{"error=", "reason=", "waiting_on="} {
		if strings.HasPrefix(result.View.Detail, label) {
			t.Errorf("Deny 回的 detail 以 %q 开头：logfmt 标签跑到了给人看的字段上\n%s",
				label, result.View.Detail)
		}
	}
}

// --- Task 6: PluginView trust fields ----------------------------------------

// revokedTrustFixtureReason is the revocation reason newRevokedTrustFixture
// records, so every subtest asserting on it names the exact same string.
const revokedTrustFixtureReason = "laptop stolen"

// newRevokedTrustFixture builds a single local, disabled plugin entry signed
// with a key this deployment's own keyring has since revoked, so a
// manifest.LoadPackage call against it comes back manifest.ProvenanceRevoked
// with a Reason and RevokedAt -- one verdict, reachable without a network
// fetch, that exercises two of PluginView's three trust fields at once
// (TrustState and TrustDetail; see TestAssemblePluginsRefusesARevokedKeyEvenWithSignaturesOff
// for why the keyring registers a second, unrelated key: sign.ParseKeyring
// refuses a document whose every registered key is revoked).
//
// The entry is left disabled (enabled: false, omitGrant: true) so that
// assemble() never attempts to activate -- and so never itself judges the
// signature of -- this entry: assemble() resolves its OWN trust set from the
// fixture's config file, entirely separate from the keyring this function
// hands to NewPluginConsentService, and a disabled entry keeps those two
// paths from interacting.
func newRevokedTrustFixture(t *testing.T) (*pluginFixture, *sign.Keyring) {
	t.Helper()

	f := newPluginFixture(t, 30_000)

	signingPub, signingPriv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sparePub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	keyringPath := filepath.Join(f.dir, "keyring.json")
	writeKeyringDoc(t, keyringPath, []map[string]string{
		{"id": string(testPluginKeyID), "algorithm": "ed25519",
			"public_key": base64.StdEncoding.EncodeToString(signingPub)},
		{"id": "spare", "algorithm": "ed25519",
			"public_key": base64.StdEncoding.EncodeToString(sparePub)},
	}, []map[string]string{{"key_id": string(testPluginKeyID), "reason": revokedTrustFixtureReason}})
	keyringData, err := os.ReadFile(keyringPath)
	if err != nil {
		t.Fatalf("read keyring %s: %v", keyringPath, err)
	}
	keyring, err := sign.ParseKeyring(keyringData)
	if err != nil {
		t.Fatalf("parse keyring %s: %v", keyringPath, err)
	}

	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
	f.signPackage("echo", signingPriv)
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	return f, keyring
}

// TestPluginViewCarriesTheTrustState is Task 6's own guard: List, Grant and
// Resolve each run their OWN manifest.LoadPackage call and must each
// translate its Provenance into the view they return -- filling only one of
// the three would mean the same plugin shows a different trust verdict
// depending on which endpoint the GUI asked, which is exactly the drift this
// field exists to prevent. See the mutation test right after this one for the
// proof that each path is independently guarded.
func TestPluginViewCarriesTheTrustState(t *testing.T) {
	wantState := manifest.ProvenanceRevoked.String()

	t.Run("List", func(t *testing.T) {
		f, keyring := newRevokedTrustFixture(t)
		svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
			consentTrustSetOf(keyring, nil), loader.RemoteConfig{}, testConsentLogger())

		views, err := svc.List(context.Background())
		if err != nil {
			t.Fatalf("List() error = %v, want nil", err)
		}
		if len(views) != 1 {
			t.Fatalf("len(views) = %d, want 1: %+v", len(views), views)
		}
		got := views[0]
		if got.TrustState != wantState {
			t.Errorf("TrustState = %q, want %q", got.TrustState, wantState)
		}
		if !strings.Contains(got.TrustDetail, revokedTrustFixtureReason) {
			t.Errorf("TrustDetail = %q, want it to name the revocation reason %q", got.TrustDetail, revokedTrustFixtureReason)
		}
	})

	t.Run("Grant", func(t *testing.T) {
		f, keyring := newRevokedTrustFixture(t)
		svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
			consentTrustSetOf(keyring, nil), loader.RemoteConfig{}, testConsentLogger())

		result, err := svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{})
		if err != nil {
			t.Fatalf("Grant() error = %v, want nil: this design deliberately does not gate Grant on trust", err)
		}
		if result.View.TrustState != wantState {
			t.Errorf("Grant() View.TrustState = %q, want %q", result.View.TrustState, wantState)
		}
		if !strings.Contains(result.View.TrustDetail, revokedTrustFixtureReason) {
			t.Errorf("Grant() View.TrustDetail = %q, want it to name the revocation reason %q",
				result.View.TrustDetail, revokedTrustFixtureReason)
		}
	})

	t.Run("Resolve", func(t *testing.T) {
		f, keyring := newRevokedTrustFixture(t)
		svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
			consentTrustSetOf(keyring, nil), loader.RemoteConfig{}, testConsentLogger())

		view, err := svc.Resolve(context.Background(), testEchoPlugin)
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil: a revoked verdict is a Provenance state, not manifest.ErrUntrustedPackage", err)
		}
		if view.TrustState != wantState {
			t.Errorf("Resolve() TrustState = %q, want %q", view.TrustState, wantState)
		}
		if !strings.Contains(view.TrustDetail, revokedTrustFixtureReason) {
			t.Errorf("Resolve() TrustDetail = %q, want it to name the revocation reason %q", view.TrustDetail, revokedTrustFixtureReason)
		}
	})
}

// TestTrustFieldsForRegisteredCarriesThePublisherName exercises the one
// PluginView.TrustPublisher-populating branch of trustFieldsFor directly
// against a synthetic manifest.Provenance, rather than through a real signed
// package. It is the unit half of a pair: this one pins the translation
// itself -- a registered verdict carries its Publisher across and nothing
// else -- while TestPluginViewCarriesThePublisherName drives the same branch
// end to end, through a real package, a real keyring and a real trust set, on
// each of List, Grant and Resolve. Neither subsumes the other: a mapping that
// is correct in isolation is still worth nothing if no path reaches it, and a
// path that reaches it is still worth nothing if the mapping drops the name.
func TestTrustFieldsForRegisteredCarriesThePublisherName(t *testing.T) {
	prov := manifest.Provenance{State: manifest.ProvenanceRegistered, Publisher: "Acme Corp"}
	state, publisher, detail := trustFieldsFor(prov)
	if state != "registered" {
		t.Errorf("state = %q, want %q", state, "registered")
	}
	if publisher != "Acme Corp" {
		t.Errorf("publisher = %q, want %q", publisher, "Acme Corp")
	}
	if detail != "" {
		t.Errorf("detail = %q, want empty: a registered verdict carries no refusal to explain", detail)
	}
}

// TestTrustFieldsForUnsignedCarriesNeitherPublisherNorDetail pins the third
// state: ProvenanceUnsigned reports only TrustState, since neither a
// publisher name nor a refusal detail applies to a package nobody
// recognisable endorsed.
func TestTrustFieldsForUnsignedCarriesNeitherPublisherNorDetail(t *testing.T) {
	prov := manifest.Provenance{State: manifest.ProvenanceUnsigned, UnrecognizedKeyID: "some-key"}
	state, publisher, detail := trustFieldsFor(prov)
	if state != "unsigned" {
		t.Errorf("state = %q, want %q", state, "unsigned")
	}
	if publisher != "" {
		t.Errorf("publisher = %q, want empty: ProvenanceUnsigned never carries a publisher", publisher)
	}
	if detail != "" {
		t.Errorf("detail = %q, want empty: ProvenanceUnsigned is not a refusal to explain", detail)
	}
}

// --- publishers reach the consent service ------------------------------------

// registeredTrustFixturePublisher is the display name registeredTrustSet puts
// against testPluginKeyID, so every assertion below names the same string.
//
// A local keyring document could not supply it: it registers key ids and
// public keys and carries no names at all (see trustlist.Merge, which takes
// the names from the fetched trust list half). The name therefore enters
// through the trust set provider, which is exactly the seam these tests hold.
const registeredTrustFixturePublisher = "星尘工作室"

// registeredTrustSet is the trust set every subtest of
// TestPluginViewCarriesThePublisherName runs against: keyring, plus the one
// display name that turns a bare key id into something a person can read.
//
// It is ONE function for all three paths on purpose. The question those
// subtests ask is whether List, Grant and Resolve each carry the provider's
// publishers through to the view they return, and a provider spelled out
// separately in each of them could be emptied in one place and still leave
// the other two green.
func registeredTrustSet(keyring *sign.Keyring) loader.TrustSet {
	return consentTrustSetOf(keyring, map[sign.KeyID]string{testPluginKeyID: registeredTrustFixturePublisher})
}

// newRegisteredTrustFixture is newRevokedTrustFixture's opposite number: a
// single local, disabled plugin entry signed with a key this deployment's
// keyring registers and has NOT revoked, so a manifest.LoadPackage call
// against it comes back manifest.ProvenanceRegistered with the publisher name
// registeredTrustSet carries for that key.
//
// registered is the only state that populates PluginView.TrustPublisher (see
// trustFieldsFor), so it is the only state under which "did the publishers
// map survive the trip from the provider to the view" can be asked at all.
//
// The entry is left disabled (enabled: false, omitGrant: true) for the reason
// newRevokedTrustFixture leaves its own disabled: assemble() resolves its OWN
// trust set from the fixture's config file, separately from the one handed to
// NewPluginConsentService, and a disabled entry keeps the two from
// interacting.
func newRegisteredTrustFixture(t *testing.T) (*pluginFixture, loader.TrustSet) {
	t.Helper()

	f := newPluginFixture(t, 30_000)
	priv, keyringPath := f.newKeyring("keyring.json")
	keyringData, err := os.ReadFile(keyringPath)
	if err != nil {
		t.Fatalf("read keyring %s: %v", keyringPath, err)
	}
	keyring, err := sign.ParseKeyring(keyringData)
	if err != nil {
		t.Fatalf("parse keyring %s: %v", keyringPath, err)
	}

	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
	f.signPackage("echo", priv)
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}
	return f, registeredTrustSet(keyring)
}

// TestPluginViewCarriesThePublisherName is the end-to-end half of
// PluginView.TrustPublisher: with a package a registered publisher really
// signed, and a trust set that really carries that publisher's display name,
// List, Grant and Resolve must each report the name rather than a bare
// verdict. "Registered" with no name beside it is the whole of what the panel
// would otherwise show, which answers "is this endorsed" and leaves "by whom"
// unanswerable.
//
// TestPluginViewCarriesTheTrustState covers the same three paths under a
// REVOKED verdict, where publishers play no part; this one is the registered
// counterpart, and the two together are why neither a state-only nor a
// publisher-only regression can hide.
func TestPluginViewCarriesThePublisherName(t *testing.T) {
	wantState := manifest.ProvenanceRegistered.String()

	t.Run("List", func(t *testing.T) {
		f, trust := newRegisteredTrustFixture(t)
		svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
			trust, loader.RemoteConfig{}, testConsentLogger())

		views, err := svc.List(context.Background())
		if err != nil {
			t.Fatalf("List() error = %v, want nil", err)
		}
		if len(views) != 1 {
			t.Fatalf("len(views) = %d, want 1: %+v", len(views), views)
		}
		got := views[0]
		if got.TrustState != wantState {
			t.Fatalf("List() TrustState = %q, want %q: the rest of this subtest only means something "+
				"for a registered verdict", got.TrustState, wantState)
		}
		if got.TrustPublisher != registeredTrustFixturePublisher {
			t.Errorf("List() TrustPublisher = %q, want %q", got.TrustPublisher, registeredTrustFixturePublisher)
		}
	})

	t.Run("Grant", func(t *testing.T) {
		f, trust := newRegisteredTrustFixture(t)
		svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
			trust, loader.RemoteConfig{}, testConsentLogger())

		result, err := svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{})
		if err != nil {
			t.Fatalf("Grant() error = %v, want nil", err)
		}
		if result.View.TrustState != wantState {
			t.Fatalf("Grant() View.TrustState = %q, want %q: the rest of this subtest only means something "+
				"for a registered verdict", result.View.TrustState, wantState)
		}
		if result.View.TrustPublisher != registeredTrustFixturePublisher {
			t.Errorf("Grant() View.TrustPublisher = %q, want %q",
				result.View.TrustPublisher, registeredTrustFixturePublisher)
		}
	})

	t.Run("Resolve", func(t *testing.T) {
		f, trust := newRegisteredTrustFixture(t)
		svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
			trust, loader.RemoteConfig{}, testConsentLogger())

		view, err := svc.Resolve(context.Background(), testEchoPlugin)
		if err != nil {
			t.Fatalf("Resolve() error = %v, want nil", err)
		}
		if view.TrustState != wantState {
			t.Fatalf("Resolve() TrustState = %q, want %q: the rest of this subtest only means something "+
				"for a registered verdict", view.TrustState, wantState)
		}
		if view.TrustPublisher != registeredTrustFixturePublisher {
			t.Errorf("Resolve() TrustPublisher = %q, want %q",
				view.TrustPublisher, registeredTrustFixturePublisher)
		}
	})
}

// --- the panel and the mount judge against ONE trust set ----------------------

// newRegisteredPackageServeFixture writes the deployment every test below runs
// against: one LOCAL entry whose package is signed by a key the configured
// keyring registers, under "require_signature": false.
//
// That policy is the interesting one, not an incidental setting. It is the
// deployment where the POLICY-enforced keyring is nil (enforcedPluginKeyring)
// while the trust set is not (resolvePluginTrustInput), so the two answer
// differently about this exact package: judged against the merged set it is
// ProvenanceRegistered, and judged against the policy keyring it is
// ProvenanceUnsigned. Any surface that reads the wrong one of the two says so
// out loud here instead of agreeing by accident.
//
// The entry is left unauthorized (no grant block) so nothing mounts during the
// assembly; each test decides for itself whether to authorize it.
func newRegisteredPackageServeFixture(t *testing.T) *pluginFixture {
	t.Helper()

	f := newPluginFixture(t, 30_000)
	priv, keyringPath := f.newKeyring("keyring.json")
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
	f.signPackage("echo", priv)
	f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath, requireSignature: boolPtr(false)})
	f.writeManifest(manifestEntry{
		name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
	})
	return f
}

// TestTheServedPanelJudgesPackagesAgainstTheAssemblysTrustSet is the guard the
// wiring it watches never had. Every other test of the trust fields injects a
// loader.TrustSet the test itself wrote; this one injects nothing. It goes
// through the real chain -- resolvePluginTrustInput -> pluginTrustSet ->
// newPluginLoader -> assemblePlugins -> BuildServeService ->
// NewPluginConsentService -> GET /v1/plugins -- and reads the verdict off the
// HTTP response, so the trust set under it is whatever that assembly actually
// built.
//
// The property: a package signed by a key the deployment's own keyring
// document registers is reported "registered" by the panel. Under
// "require_signature": false the local keyring document is the ONLY half of
// the trust set present (there is no fetched list here), so a panel judging
// against anything but the assembly's own merged set -- a second provider
// missing that half, the policy keyring, an empty set stood in for a failure
// -- reports "unsigned" instead, which is precisely the "endorsed here,
// unsigned there" split this wiring exists to make impossible.
func TestTheServedPanelJudgesPackagesAgainstTheAssemblysTrustSet(t *testing.T) {
	f := newRegisteredPackageServeFixture(t)

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
	t.Cleanup(result.Close)
	runServeInBackground(t, &result)

	views := getPluginViews(t, result.BaseURL, result.Token)
	if len(views) != 1 {
		t.Fatalf("GET /v1/plugins returned %d rows, want 1: %+v", len(views), views)
	}
	got := views[0]
	if got.TrustState != manifest.ProvenanceRegistered.String() {
		t.Errorf("GET /v1/plugins trust_state = %q, want %q.\nThe package is signed by a key this "+
			"deployment's keyring document registers, and that document is the only half of the trust "+
			"set there is here -- so a panel reporting anything else is judging against a set the "+
			"assembly did not build, and the mount that DOES use it will disagree with the screen.\n"+
			"row = %+v", got.TrustState, manifest.ProvenanceRegistered.String(), got)
	}
}

// getPluginViews performs the panel's own GET /v1/plugins against a running
// serve and decodes the rows out of it.
func getPluginViews(t *testing.T, baseURL, token string) []server.PluginView {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/plugins", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Origin", baseURL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s/v1/plugins: %v", baseURL, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/plugins status = %d, want 200: %s", resp.StatusCode, body)
	}
	var decoded struct {
		Plugins []server.PluginView `json:"plugins"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode /v1/plugins body %s: %v", body, err)
	}
	return decoded.Plugins
}

// TestTheCLIAndThePanelGrantJudgeOnePackageAlike is the task-6 review's
// Important-2. `agent plugins grant` and the panel's POST
// /v1/plugins/{name}/grant are two doors onto one decision, and until this
// test they could give an operator two different answers about one package
// under one config: the panel read the merged trust set, the command read the
// POLICY keyring, and under "require_signature": false that one is nil.
//
// A nil keyring makes manifest.LoadPackage judge every package unsigned
// without reading plugin.sig at all, so the command sailed past a signature
// the panel refused with 400 -- same package, same config, same operator.
//
// Both subtests therefore assert AGREEMENT rather than a particular outcome:
// what must not happen is one door opening while the other closes. The
// refusing subtest is where the split was; the accepting one is here so a
// command that started refusing everything could not pass by agreeing on
// "no".
func TestTheCLIAndThePanelGrantJudgeOnePackageAlike(t *testing.T) {
	// grantBothWays authorizes one entry through both doors, panel first, and
	// returns what each of them said.
	grantBothWays := func(t *testing.T, f *pluginFixture) (panelErr, cliErr error) {
		t.Helper()

		trust, err := f.assembleTrustSet(slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("assemblePlugins() error = %v, want nil", err)
		}
		svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
			trust, loader.RemoteConfig{}, testConsentLogger())
		_, panelErr = svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{})

		_, cliErr = f.run("grant", testEchoPlugin)
		return panelErr, cliErr
	}

	t.Run("a signature that does not verify", func(t *testing.T) {
		f := newPluginFixture(t, 30_000)
		_, keyringPath := f.newKeyring("keyring.json")
		f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
		// Signed by a key that is NOT the one the keyring registers, under the
		// id that keyring DOES register: the signature is checked and fails,
		// which is a refusal rather than a verdict.
		f.signPackageWithAnyKey("echo")
		f.writeSignatureConfig(30_000, signaturePolicy{keyring: keyringPath, requireSignature: boolPtr(false)})
		f.writeManifest(manifestEntry{
			name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
		})
		before, err := os.ReadFile(f.manifestPath)
		if err != nil {
			t.Fatalf("read plugins.json: %v", err)
		}

		panelErr, cliErr := grantBothWays(t, f)

		if panelErr == nil {
			t.Fatalf("panel Grant() error = nil for a package whose signature does not verify, want a " +
				"refusal: the rest of this subtest compares the command against it")
		}
		if cliErr == nil {
			t.Errorf("`agent plugins grant` error = nil while the panel refused the SAME package under the "+
				"SAME config with: %v\nOne operator, two doors, two answers -- the command is judging "+
				"against a different trust set than the panel and the mount do.", panelErr)
		}
		if cliErr != nil && !errors.Is(cliErr, manifest.ErrUntrustedPackage) {
			t.Errorf("`agent plugins grant` error = %v, want it to wrap manifest.ErrUntrustedPackage, the "+
				"same refusal the panel gave: agreeing on \"no\" for unrelated reasons is not agreement", cliErr)
		}
		after, err := os.ReadFile(f.manifestPath)
		if err != nil {
			t.Fatalf("re-read plugins.json: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("plugins.json changed while both doors were refusing:\nbefore: %s\nafter:  %s", before, after)
		}
	})

	t.Run("an endorsed package", func(t *testing.T) {
		f := newRegisteredPackageServeFixture(t)

		panelErr, cliErr := grantBothWays(t, f)

		if panelErr != nil {
			t.Errorf("panel Grant() error = %v, want nil: the package is signed by a key this deployment's "+
				"keyring registers", panelErr)
		}
		if cliErr != nil {
			t.Errorf("`agent plugins grant` error = %v, want nil: the panel authorized this same package "+
				"under this same config, and a command that refuses what the panel accepts is the same "+
				"split in the other direction", cliErr)
		}
		entry := f.requireEntry(f.readDeployment(), testEchoPlugin)
		if !entry.Enabled {
			t.Error("the entry is still disabled after both doors reported success")
		}
	})
}

// errConsentTrustSetUnavailable is what failingConsentTrustSet reports, so the
// assertions below can match that exact error rather than any error.
var errConsentTrustSetUnavailable = errors.New("the trust list cache cannot be read")

// failingConsentTrustSet is the provider of a deployment that does not KNOW
// what it trusts -- loader.TrustSet's documented third answer, distinct from
// both "no trust set" and a usable one.
func failingConsentTrustSet() (manifest.TrustInput, error) {
	return manifest.TrustInput{}, errConsentTrustSetUnavailable
}

// TestConsentPathsRefuseAnUnreadableTrustSet is the fail-loud half of the same
// wiring: a provider that returns an error must stop List, Grant and Resolve
// with that error on the chain, never be read as an empty trust set. The two
// are not interchangeable -- an empty set reports every package as
// ProvenanceUnsigned, which is a verdict about the packages, and no verdict is
// available while the set itself cannot be assembled.
//
// Grant is additionally checked to have written nothing: it reads the trust
// set before its compare-and-swap, so an authorization must not be half
// applied on this path.
func TestConsentPathsRefuseAnUnreadableTrustSet(t *testing.T) {
	newSvc := func(t *testing.T) (*pluginFixture, *PluginConsentService) {
		t.Helper()
		f := newPluginFixture(t, 30_000)
		f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
		f.writeManifest(manifestEntry{
			name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
		})
		if err := f.assemble(); err != nil {
			t.Fatalf("assemblePlugins() error = %v, want nil", err)
		}
		return f, NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
			failingConsentTrustSet, loader.RemoteConfig{}, testConsentLogger())
	}

	t.Run("List", func(t *testing.T) {
		_, svc := newSvc(t)
		views, err := svc.List(context.Background())
		if !errors.Is(err, errConsentTrustSetUnavailable) {
			t.Fatalf("List() error = %v, want it to wrap the provider's own error", err)
		}
		if views != nil {
			t.Errorf("List() views = %+v, want nil: a list judged against no trust set is not a list", views)
		}
	})

	t.Run("Grant", func(t *testing.T) {
		f, svc := newSvc(t)
		before, err := os.ReadFile(f.manifestPath)
		if err != nil {
			t.Fatalf("read plugins.json: %v", err)
		}
		_, err = svc.Grant(context.Background(), testEchoPlugin, server.GrantRequest{})
		if !errors.Is(err, errConsentTrustSetUnavailable) {
			t.Fatalf("Grant() error = %v, want it to wrap the provider's own error", err)
		}
		after, err := os.ReadFile(f.manifestPath)
		if err != nil {
			t.Fatalf("re-read plugins.json: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("plugins.json changed while Grant was refusing an unreadable trust set:\nbefore: %s\nafter:  %s",
				before, after)
		}
	})

	t.Run("Resolve", func(t *testing.T) {
		_, svc := newSvc(t)
		if _, err := svc.Resolve(context.Background(), testEchoPlugin); !errors.Is(err, errConsentTrustSetUnavailable) {
			t.Fatalf("Resolve() error = %v, want it to wrap the provider's own error", err)
		}
	})
}

// TestATrustSetThatWillNotAssembleIsAServerSideFault is the task-6 review's
// Important-3. A trust set that cannot be assembled is this deployment's own
// keyring document or trust list cache being unreadable -- a fault on the
// machine, not a defect in the request that arrived. Carrying no class at all,
// it landed in pluginConsentStatus's default branch and the panel was told
// 400 Bad Request, which sends an operator to inspect a request that was
// never the problem while the actual fault sits in the config or the cache.
//
// The status mapping itself is internal/server's
// (TestPluginsConsentErrorClassSelectsStatus); what this side owes is the
// class on the error.
func TestATrustSetThatWillNotAssembleIsAServerSideFault(t *testing.T) {
	newSvc := func(t *testing.T) *PluginConsentService {
		t.Helper()
		f := newPluginFixture(t, 30_000)
		f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
		f.writeManifest(manifestEntry{
			name: testEchoPlugin, source: "echo", enabled: false, tools: []string{testEchoTool}, omitGrant: true,
		})
		if err := f.assemble(); err != nil {
			t.Fatalf("assemblePlugins() error = %v, want nil", err)
		}
		return NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins,
			failingConsentTrustSet, loader.RemoteConfig{}, testConsentLogger())
	}

	t.Run("Grant", func(t *testing.T) {
		_, err := newSvc(t).Grant(context.Background(), testEchoPlugin, server.GrantRequest{})
		if !errors.Is(err, server.ErrPluginTrustSet) {
			t.Errorf("Grant() error = %v, want it to carry server.ErrPluginTrustSet: without a class the "+
				"handler reports 400 and the panel tells the operator their request was malformed", err)
		}
	})

	t.Run("Resolve", func(t *testing.T) {
		_, err := newSvc(t).Resolve(context.Background(), testEchoPlugin)
		if !errors.Is(err, server.ErrPluginTrustSet) {
			t.Errorf("Resolve() error = %v, want it to carry server.ErrPluginTrustSet", err)
		}
	})
}

// TestNewPluginConsentServicePanicsOnANilTrustSet is the constructor's other
// fail-loud refusal: a nil provider has no trust set to judge any package
// against, and every path in the service would nil-dereference on its first
// request instead of naming the wiring mistake.
func TestNewPluginConsentServicePanicsOnANilTrustSet(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewPluginConsentService(nil trust set) did not panic, want a panic naming the nil provider")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "trustFn") {
			t.Errorf("panic value = %v, want a message naming trustFn", r)
		}
	}()

	NewPluginConsentService("m.json", "root", func() *loader.Loader { return nil },
		nil, loader.RemoteConfig{}, testConsentLogger())
}
