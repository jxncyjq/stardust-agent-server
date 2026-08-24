package loader

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/fetch"
	"github.com/stardust/legion-agent/internal/plugin/host"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// remotePackageFileNames are the three files a plugin package archive carries
// — the same three fetch.Unpack insists on. A test archive is packed from a
// package directory these tests wrote and signed, so what travels over the
// wire is byte-for-byte what a local deployment would hold on disk.
var remotePackageFileNames = [...]string{"plugin.json", "plugin.wasm", "plugin.sig"}

// testFetchLimits are the fetch bounds every test here runs under: generous
// enough for the committed wasm fixtures, finite in every field.
func testFetchLimits() fetch.Limits {
	return fetch.Limits{MaxBytes: 32 << 20, Timeout: 30 * time.Second}
}

// testUnpackLimits are the unpack bounds every test here runs under.
func testUnpackLimits() fetch.UnpackLimits {
	return fetch.UnpackLimits{MaxEntries: 16, MaxTotalBytes: 64 << 20, MaxEntryBytes: 32 << 20}
}

// tarGzPackage packs dir's three package files into the gzipped tar a remote
// source serves.
func tarGzPackage(t *testing.T, dir string) []byte {
	t.Helper()

	buf := &bytes.Buffer{}
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)
	for _, name := range remotePackageFileNames {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s in %s: %v", name, dir, err)
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("write tar header for %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write tar body for %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// signedEchoArchive writes the echo package into a directory of its OWN —
// deliberately not under any deployment root — signs it, and returns both the
// directory (for tests that then tamper with it) and the archive a server
// hands out. Because the package never exists under the deployment root, a
// mount in these tests can only have come from the fetched bytes.
func signedEchoArchive(t *testing.T, priv ed25519.PrivateKey, version string) (string, []byte) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "echo")
	writePackage(t, dir, pkg{
		wasm:    fixtureWasm(t, echoWasmFile),
		name:    echoPluginName,
		version: version,
		tools:   []string{echoToolName},
	})
	signPackage(t, dir, priv)
	return dir, tarGzPackage(t, dir)
}

// digestOf spells data's sha256 the way a deployment entry does.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// newTestCache builds a Cache under a per-test temporary root.
func newTestCache(t *testing.T) *fetch.Cache {
	t.Helper()

	cache, err := fetch.NewCache(filepath.Join(t.TempDir(), "plugin-cache"))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	return cache
}

// servePlaintext starts a plaintext (http://) server that hands archive to
// every GET and counts the requests it received. It exists for the tests that
// are specifically about the insecure-source policy; every other test uses the
// https variant, which is what a real deployment uses.
func servePlaintext(t *testing.T, archive []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	return serveCounting(t, httptest.NewServer, archive)
}

// serveTLS is servePlaintext over https. Its client (srv.Client()) is the only
// one that trusts the test certificate, so it must be the one the Loader is
// given.
func serveTLS(t *testing.T, archive []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	return serveCounting(t, httptest.NewTLSServer, archive)
}

// serveCounting is the body servePlaintext and serveTLS share, parameterized
// by which httptest constructor starts the server.
//
// The counter is atomic because it is written by the server's handler
// goroutine and read by the test's: a completed HTTP round trip is not a
// happens-before edge the race detector knows about, so a plain int here would
// be a data race that passes by luck rather than by construction.
func serveCounting(t *testing.T, start func(http.Handler) *httptest.Server, archive []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	hits := &atomic.Int64{}
	srv := start(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if _, err := w.Write(archive); err != nil {
			t.Errorf("write archive to client: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// serveNothing starts a server that FAILS THE TEST if it is contacted at all.
// It is how "a cache hit does not go online" is proved: not by counting zero
// requests afterwards, but by making a single request a failure at the moment
// it arrives.
func serveNothing(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("the remote source was contacted (%s %s); this test must be served entirely from disk, "+
			"with no network access at all", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// remoteEntry is the deployment entry for a remote package: the entry a local
// one produces, with its source replaced by a URL and the mandatory digest
// attached.
func remoteEntry(source, digest string) manifest.Entry {
	entry := entryFor(echoPluginName, source, nil, echoToolName)
	entry.Digest = digest
	return entry
}

// applyExpectingFailure converges entries and returns the error Apply
// reported, failing the test if it reported none.
func applyExpectingFailure(t *testing.T, h *harness, entries ...manifest.Entry) error {
	t.Helper()

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: entries}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil, want an error")
	}
	return err
}

// remoteFor builds the RemoteConfig these tests hand the harness: a fresh
// cache, the server's own client, and finite limits in every field.
func remoteFor(t *testing.T, srv *httptest.Server, cache *fetch.Cache, allowInsecure bool) RemoteConfig {
	t.Helper()

	return RemoteConfig{
		Cache:                cache,
		Client:               srv.Client(),
		FetchLimits:          testFetchLimits(),
		UnpackLimits:         testUnpackLimits(),
		AllowInsecureSources: allowInsecure,
	}
}

// TestApplyMountsARemotePackageItFetchedAndVerified is the whole path in one
// test: the entry names a URL and a digest, the loader fetches it, files it
// under its digest and mounts it — with signatures REQUIRED throughout, so the
// package went through manifest.LoadPackage exactly as a local one does.
func TestApplyMountsARemotePackageItFetchedAndVerified(t *testing.T) {
	priv, keyring := newTestKey(t)
	_, archive := signedEchoArchive(t, priv, "1.0.0")
	digest := digestOf(archive)
	srv, hits := serveTLS(t, archive)
	cache := newTestCache(t)
	h := newHarnessWithRemote(t, keyring, remoteFor(t, srv, cache, false))

	h.apply(remoteEntry(srv.URL+"/echo.tgz", digest))

	row := h.statusOf(echoPluginName)
	if row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
	if row.Version != "1.0.0" {
		t.Errorf("plugin %q: Version = %q, want 1.0.0", echoPluginName, row.Version)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("the source was requested %d times, want exactly 1", got)
	}
	has, err := cache.Has(digest)
	if err != nil {
		t.Fatalf("cache.Has(%s): %v", digest, err)
	}
	if !has {
		t.Errorf("cache.Has(%s) = false, want true: a fetched package must be filed under its digest", digest)
	}
}

// TestApplyServesACachedDigestWithoutContactingTheSource is rule 3: a cache
// hit does not go online. The server fails the test the moment it is
// contacted, so this cannot pass by counting requests that were never made for
// some other reason.
func TestApplyServesACachedDigestWithoutContactingTheSource(t *testing.T) {
	priv, keyring := newTestKey(t)
	_, archive := signedEchoArchive(t, priv, "1.0.0")
	digest := digestOf(archive)
	cache := newTestCache(t)
	if _, err := cache.Put(digest, archive, testUnpackLimits()); err != nil {
		t.Fatalf("cache.Put: %v", err)
	}
	srv := serveNothing(t)
	h := newHarnessWithRemote(t, keyring, remoteFor(t, srv, cache, false))

	h.apply(remoteEntry(srv.URL+"/echo.tgz", digest))

	if row := h.statusOf(echoPluginName); row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
}

// TestApplyKeepsConvergingWhenARemoteEntryCannotBeFetched is rule 2: a fetch
// failure travels the ordinary failure channel — the other entries still
// converge, Apply returns the joined error, and the entry lands in StateFailed
// with a LastError that says the fetch is what went wrong.
func TestApplyKeepsConvergingWhenARemoteEntryCannotBeFetched(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such artifact", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	h := newHarnessWithRemote(t, nil, remoteFor(t, srv, newTestCache(t), false))
	local := h.writeProxy("1.0.0")
	remote := remoteEntry(srv.URL+"/echo.tgz", digestOf([]byte("an artifact this server does not have")))

	err := applyExpectingFailure(t, h, remote, local)

	if !strings.Contains(err.Error(), echoPluginName) {
		t.Errorf("Apply() error = %v, want it to name the failed entry %q", err, echoPluginName)
	}
	row := h.statusOf(echoPluginName)
	if row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q", echoPluginName, row.State, StateFailed)
	}
	if !strings.Contains(row.LastError, "404") {
		t.Errorf("plugin %q: LastError = %q, want it to say the fetch failed", echoPluginName, row.LastError)
	}
	if got := h.statusOf(proxyPluginName); got.State != StateLoaded {
		t.Errorf("plugin %q: State = %q, want %q: one entry's fetch failure must not stop the others",
			proxyPluginName, got.State, StateLoaded)
	}
}

// TestApplyRefusesAnInsecureRemoteSourceByDefault is rule 6's loader half:
// with allow_insecure_sources off, an http:// entry fails, naming the entry
// and its URL — and the source is never contacted, because the refusal
// happens before any request is built.
func TestApplyRefusesAnInsecureRemoteSourceByDefault(t *testing.T) {
	priv, keyring := newTestKey(t)
	_, archive := signedEchoArchive(t, priv, "1.0.0")
	srv, hits := servePlaintext(t, archive)
	h := newHarnessWithRemote(t, keyring, remoteFor(t, srv, newTestCache(t), false))
	source := srv.URL + "/echo.tgz"

	err := applyExpectingFailure(t, h, remoteEntry(source, digestOf(archive)))

	if !strings.Contains(err.Error(), source) {
		t.Errorf("Apply() error = %v, want it to name the offending URL %s", err, source)
	}
	if !strings.Contains(err.Error(), echoPluginName) {
		t.Errorf("Apply() error = %v, want it to name the entry %q", err, echoPluginName)
	}
	// The remedy, not just the refusal: this is the message an operator meets
	// on the reload path, where the assembly-time error that names the setting
	// is never reached.
	if !strings.Contains(err.Error(), "allow_insecure_sources") {
		t.Errorf("Apply() error = %v, want it to name the switch that turns plaintext on", err)
	}
	if row := h.statusOf(echoPluginName); row.State != StateFailed {
		t.Errorf("plugin %q: State = %q, want %q", echoPluginName, row.State, StateFailed)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("the plaintext source was contacted %d times, want 0: a refused scheme must never be fetched", got)
	}
}

// TestApplyFetchesAnInsecureRemoteSourceWhenItIsAllowed is the other half of
// rule 6: the switch, once on, lets the plaintext entry through.
func TestApplyFetchesAnInsecureRemoteSourceWhenItIsAllowed(t *testing.T) {
	priv, keyring := newTestKey(t)
	_, archive := signedEchoArchive(t, priv, "1.0.0")
	srv, hits := servePlaintext(t, archive)
	h := newHarnessWithRemote(t, keyring, remoteFor(t, srv, newTestCache(t), true))

	h.apply(remoteEntry(srv.URL+"/echo.tgz", digestOf(archive)))

	if row := h.statusOf(echoPluginName); row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("the source was requested %d times, want exactly 1", got)
	}
}

// TestApplyStillEnforcesTheDigestOnAnAllowedInsecureSource is rule 8: the
// switch relaxes the SCHEME and nothing else. With it on, an entry whose bytes
// do not hash to its digest still fails, and nothing is filed in the cache.
func TestApplyStillEnforcesTheDigestOnAnAllowedInsecureSource(t *testing.T) {
	priv, keyring := newTestKey(t)
	_, archive := signedEchoArchive(t, priv, "1.0.0")
	srv, _ := servePlaintext(t, archive)
	cache := newTestCache(t)
	h := newHarnessWithRemote(t, keyring, remoteFor(t, srv, cache, true))
	wrongDigest := digestOf([]byte("not the archive that will be served"))

	err := applyExpectingFailure(t, h, remoteEntry(srv.URL+"/echo.tgz", wrongDigest))

	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("Apply() error = %v, want it to report a digest mismatch", err)
	}
	if row := h.statusOf(echoPluginName); row.State != StateFailed {
		t.Errorf("plugin %q: State = %q, want %q", echoPluginName, row.State, StateFailed)
	}
	has, err := cache.Has(wrongDigest)
	if err != nil {
		t.Fatalf("cache.Has(%s): %v", wrongDigest, err)
	}
	if has {
		t.Error("bytes that failed their digest check must never reach the cache")
	}
}

// TestApplyStillVerifiesTheSignatureOfARemotePackage is the gate this task
// must not open: the digest and the signature answer two different questions,
// so a package whose bytes are exactly the ones its digest names still has to
// pass manifest.LoadPackage. The package here is signed and THEN retagged, so
// its digest is correct end to end and only the signature can fail.
func TestApplyStillVerifiesTheSignatureOfARemotePackage(t *testing.T) {
	priv, keyring := newTestKey(t)
	dir, _ := signedEchoArchive(t, priv, "1.0.0")
	retagVersion(t, dir, "1.0.1")
	archive := tarGzPackage(t, dir)
	srv, _ := serveTLS(t, archive)
	h := newHarnessWithRemote(t, keyring, remoteFor(t, srv, newTestCache(t), false))

	err := applyExpectingFailure(t, h, remoteEntry(srv.URL+"/echo.tgz", digestOf(archive)))

	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("Apply() error = %v, want it to report a signature failure: a fetched package is not a verified one", err)
	}
	row := h.statusOf(echoPluginName)
	if row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q", echoPluginName, row.State, StateFailed)
	}
	if !strings.Contains(row.LastError, "signature") {
		t.Errorf("plugin %q: LastError = %q, want it to name the signature", echoPluginName, row.LastError)
	}
}

// TestApplyFailsARemoteEntryWhenNoCacheIsConfigured is rule 1's loader half:
// where downloaded code is written is a deployment decision, so a Loader with
// no cache refuses the entry instead of picking a directory of its own.
func TestApplyFailsARemoteEntryWhenNoCacheIsConfigured(t *testing.T) {
	priv, keyring := newTestKey(t)
	_, archive := signedEchoArchive(t, priv, "1.0.0")
	srv, hits := serveTLS(t, archive)
	h := newHarnessWithRemote(t, keyring, RemoteConfig{})

	err := applyExpectingFailure(t, h, remoteEntry(srv.URL+"/echo.tgz", digestOf(archive)))

	if !strings.Contains(err.Error(), "cache") {
		t.Errorf("Apply() error = %v, want it to say a remote source needs a configured cache directory", err)
	}
	if row := h.statusOf(echoPluginName); row.State != StateFailed {
		t.Errorf("plugin %q: State = %q, want %q", echoPluginName, row.State, StateFailed)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("the source was contacted %d times, want 0: with nowhere to put the package, nothing may be downloaded", got)
	}
}

// TestApplyLeavesALocalEntryUnchangedWhenRemoteSourcesAreConfigured is rule 5:
// configuring remote sources changes nothing for a local entry — it still
// resolves against the deployment root, and nothing is requested for it.
func TestApplyLeavesALocalEntryUnchangedWhenRemoteSourcesAreConfigured(t *testing.T) {
	priv, keyring := newTestKey(t)
	srv := serveNothing(t)
	h := newHarnessWithRemote(t, keyring, remoteFor(t, srv, newTestCache(t), true))
	entry := h.writeEcho("1.0.0")
	signPackage(t, filepath.Join(h.root, "echo"), priv)

	h.apply(entry)

	row := h.statusOf(echoPluginName)
	if row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
	if row.Version != "1.0.0" {
		t.Errorf("plugin %q: Version = %q, want 1.0.0", echoPluginName, row.Version)
	}
}

// TestApplyRefetchesAPartiallyDeletedCacheEntry pins WHICH question decides a
// cache hit: fetch.Cache.Has ("all three package files are there"), not "the
// digest directory exists". The two differ exactly in the state an unpack that
// died mid-write leaves behind, and treating that as a hit would mount a
// partial plugin and -- because the directory is already there -- never fetch
// the rest.
//
// The half-written directory is built by deleting one file from a complete
// entry, which is that state with none of the timing. This is the wiring
// level: fetch's own tests cover Has, but nothing pinned that remoteDir asks
// Has rather than the filesystem.
func TestApplyRefetchesAPartiallyDeletedCacheEntry(t *testing.T) {
	priv, keyring := newTestKey(t)
	_, archive := signedEchoArchive(t, priv, "1.0.0")
	digest := digestOf(archive)
	cache := newTestCache(t)
	dir, err := cache.Put(digest, archive, testUnpackLimits())
	if err != nil {
		t.Fatalf("cache.Put: %v", err)
	}
	// One file short of a package: the directory still stands, so an existence
	// check would call this a hit.
	if err := os.Remove(filepath.Join(dir, "plugin.sig")); err != nil {
		t.Fatalf("remove plugin.sig from the cached package: %v", err)
	}
	srv, hits := serveTLS(t, archive)
	h := newHarnessWithRemote(t, keyring, remoteFor(t, srv, cache, false))

	h.apply(remoteEntry(srv.URL+"/echo.tgz", digest))

	if got := hits.Load(); got != 1 {
		t.Errorf("the source was requested %d times, want exactly 1: an incomplete cache entry must be refetched", got)
	}
	if row := h.statusOf(echoPluginName); row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
	// The refetch repaired the entry rather than mounting around it.
	has, err := cache.Has(digest)
	if err != nil {
		t.Fatalf("cache.Has(%s): %v", digest, err)
	}
	if !has {
		t.Errorf("cache.Has(%s) = false, want true: the refetch must leave a whole entry behind", digest)
	}
}

// TestRemotePolicyDistinguishesAMovedCacheAndARelaxedSwitch pins what a remote
// policy comparison has to be able to tell apart, because `agent plugins
// reload` refuses on exactly this difference: the plaintext switch flipping
// either way, and the cache moving. A policy that compared equal across those
// would report a successful reload for a process still fetching under the old
// rules.
func TestRemotePolicyDistinguishesAMovedCacheAndARelaxedSwitch(t *testing.T) {
	cache := newTestCache(t)
	strict := RemotePolicyOf(RemoteConfig{Cache: cache, AllowInsecureSources: false})
	relaxed := RemotePolicyOf(RemoteConfig{Cache: cache, AllowInsecureSources: true})
	moved := RemotePolicyOf(RemoteConfig{Cache: newTestCache(t), AllowInsecureSources: false})

	if !strict.Equal(RemotePolicyOf(RemoteConfig{Cache: cache})) {
		t.Errorf("RemotePolicy.Equal(itself) = false, want true")
	}
	if strict.Equal(relaxed) {
		t.Errorf("%s compared equal to %s, want unequal: the plaintext switch is the policy", strict, relaxed)
	}
	if strict.Equal(moved) {
		t.Errorf("%s compared equal to %s, want unequal: a moved cache is a changed policy", strict, moved)
	}
	if unconfigured := RemotePolicyOf(RemoteConfig{}); unconfigured.Equal(strict) {
		t.Errorf("%s compared equal to %s, want unequal: no cache is a policy of its own", unconfigured, strict)
	}
	if got := strict.String(); !strings.Contains(got, cache.Root()) || !strings.Contains(got, "refused") {
		t.Errorf("RemotePolicy.String() = %q, want it to name the cache root and say plaintext is refused", got)
	}
	if got := relaxed.String(); !strings.Contains(got, "allowed") {
		t.Errorf("RemotePolicy.String() = %q, want it to say plaintext sources are allowed", got)
	}
	if got := (RemotePolicy{}).String(); !strings.Contains(got, "no plugin cache") {
		t.Errorf("RemotePolicy.String() = %q, want it to say no cache is configured", got)
	}
}

// TestLoaderReportsTheRemotePolicyItWasBuiltWith is the accessor's half: the
// value reload compares against has to come from the Loader that is actually
// running, not from the config that built it.
func TestLoaderReportsTheRemotePolicyItWasBuiltWith(t *testing.T) {
	srv, _ := serveTLS(t, nil)
	remote := remoteFor(t, srv, newTestCache(t), true)
	h := newHarnessWithRemote(t, nil, remote)

	if got := h.loader.RemotePolicy(); !got.Equal(RemotePolicyOf(remote)) {
		t.Errorf("Loader.RemotePolicy() = %s, want the policy of the RemoteConfig it was built with %s",
			got, RemotePolicyOf(remote))
	}
	if got := newHarness(t).loader.RemotePolicy(); !got.Equal(RemotePolicy{}) {
		t.Errorf("Loader.RemotePolicy() = %s for a Loader with no remote section, want the unconfigured policy", got)
	}
}

// baseTestConfig is a Config with every required dependency present, so that a
// New test can vary the remote section alone.
func baseTestConfig(remote RemoteConfig) Config {
	return Config{
		Ledger:    lifecycle.NewLedger(),
		Deps:      func(string, json.RawMessage) host.Deps { return host.Deps{} },
		Events:    adapter.NewMemoryEventBus(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Gate:      taskgate.NewTaskGate(),
		ApplyWait: defaultTestApplyWait,
		Remote:    remote,
	}
}

// TestNewRejectsARemoteConfigurationItCannotUse pins the wiring errors a
// configured cache makes load-bearing. Each is reported by field name at
// construction rather than at the first fetch: a nil client is a panic inside
// fetch.Fetch, and a non-positive limit is a bound that does not bound.
func TestNewRejectsARemoteConfigurationItCannotUse(t *testing.T) {
	cache := newTestCache(t)
	full := func() RemoteConfig {
		return RemoteConfig{
			Cache:        cache,
			Client:       http.DefaultClient,
			FetchLimits:  testFetchLimits(),
			UnpackLimits: testUnpackLimits(),
		}
	}
	cases := []struct {
		name    string
		corrupt func(*RemoteConfig)
		want    string
	}{
		{"no client", func(r *RemoteConfig) { r.Client = nil }, "Client"},
		{"no fetch timeout", func(r *RemoteConfig) { r.FetchLimits.Timeout = 0 }, "Timeout"},
		{"no fetch byte cap", func(r *RemoteConfig) { r.FetchLimits.MaxBytes = 0 }, "MaxBytes"},
		{"no entry cap", func(r *RemoteConfig) { r.UnpackLimits.MaxEntries = 0 }, "MaxEntries"},
		{"no total byte cap", func(r *RemoteConfig) { r.UnpackLimits.MaxTotalBytes = 0 }, "MaxTotalBytes"},
		{"no per-entry byte cap", func(r *RemoteConfig) { r.UnpackLimits.MaxEntryBytes = 0 }, "MaxEntryBytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remote := full()
			tc.corrupt(&remote)
			_, err := New(baseTestConfig(remote))
			if err == nil {
				t.Fatalf("New() error = nil, want an error naming %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("New() error = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// TestNewAcceptsAnUnconfiguredRemoteSection is the other side of the rule
// above: a deployment with no remote entries configures no cache, and that is
// an ordinary deployment rather than a wiring mistake.
func TestNewAcceptsAnUnconfiguredRemoteSection(t *testing.T) {
	if _, err := New(baseTestConfig(RemoteConfig{})); err != nil {
		t.Fatalf("New() error = %v, want nil: a deployment with no remote entries needs no cache", err)
	}
}
