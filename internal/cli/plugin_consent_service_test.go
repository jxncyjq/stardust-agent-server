package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/plugin/loader"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/plugin/sign"
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
