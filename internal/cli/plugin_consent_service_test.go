package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins, func() *sign.Keyring { return nil }, loader.RemoteConfig{})
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
		func() *loader.Loader { return nil }, func() *sign.Keyring { return nil }, loader.RemoteConfig{})
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
		f.application.Plugins, func() *sign.Keyring { return nil }, loader.RemoteConfig{})
	if _, err := svc.List(context.Background()); err == nil {
		t.Fatal("List() error = nil, want an error naming the unreadable manifest")
	}
}

// TestPluginConsentServiceListErrorsWhenLocalPackageIsBroken verifies that a
// local entry whose plugin.json cannot be loaded fails List outright (fail
// loud), rather than reporting that entry with an empty declaration that
// would misrepresent "unreadable" as "declares nothing".
func TestPluginConsentServiceListErrorsWhenLocalPackageIsBroken(t *testing.T) {
	f := newPluginFixture(t, 30_000)
	f.writePackage("echo", testEchoWasm, testEchoPlugin, "1.0.0", nil, []string{testEchoTool})
	f.writeManifest(manifestEntry{name: testEchoPlugin, source: "echo", enabled: true, tools: []string{testEchoTool}})
	if err := f.assemble(); err != nil {
		t.Fatalf("assemblePlugins() error = %v, want nil", err)
	}

	// Corrupt the on-disk package AFTER assembly: the loader already mounted
	// it from the original bytes, but List's own LoadPackage call re-reads
	// plugin.json from disk and must surface this rather than silently
	// reporting an empty declaration.
	pluginJSON := filepath.Join(f.root, "echo", "plugin.json")
	if err := os.WriteFile(pluginJSON, []byte("not json"), 0o644); err != nil {
		t.Fatalf("corrupt plugin.json: %v", err)
	}

	svc := NewPluginConsentService(f.manifestPath, f.root, f.application.Plugins, func() *sign.Keyring { return nil }, loader.RemoteConfig{})
	_, err := svc.List(context.Background())
	if err == nil {
		t.Fatal("List() error = nil, want an error naming the unreadable plugin.json")
	}
	if !strings.Contains(err.Error(), testEchoPlugin) {
		t.Fatalf("List() error = %v, want it to name plugin %q", err, testEchoPlugin)
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
		func() *sign.Keyring { return nil }, f.resolveFixtureRemote())
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
		func() *sign.Keyring { return nil }, f.resolveFixtureRemote())
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
		func() *sign.Keyring { return nil }, loader.RemoteConfig{})
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
		func() *sign.Keyring { return nil }, f.resolveFixtureRemote())
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
