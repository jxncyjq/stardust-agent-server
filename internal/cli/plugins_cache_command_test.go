package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The cache commands are the operator's only handle on the plugin cache: until
// they existed, inspecting it meant reading the directory by hand and
// reclaiming space meant rm -rf.
//
// Policy lives here rather than in internal/plugin/fetch because "which
// digests are still referenced" is a fact about plugins.json, and the cache
// package has no business knowing a deployment manifest exists.

// cacheFixture adds a configured cache directory to the plugin fixture and
// writes entries into it by hand — the shape is all these commands read, and
// building real tarballs would test fetch's unpacker again rather than these
// commands.
type cacheFixture struct {
	*pluginFixture
	cacheDir string
}

func newCacheFixture(t *testing.T) *cacheFixture {
	t.Helper()

	// apply_wait_ms must be positive (config validation); the value is
	// irrelevant here because no cache command converges anything.
	f := newPluginFixture(t, 1000)
	cacheDir := filepath.Join(f.dir, "plugin-cache")
	f.writeSignatureConfig(1000, signaturePolicy{requireSignature: boolPtr(false)},
		fmt.Sprintf("\"cache\": %s", jsonString(cacheDir)))
	return &cacheFixture{pluginFixture: f, cacheDir: cacheDir}
}

// writeCacheEntry creates a complete package directory for hexDigest and
// returns the digest string the commands print.
func (f *cacheFixture) writeCacheEntry(hexDigest string, bytes int) string {
	f.t.Helper()

	dir := filepath.Join(f.cacheDir, "sha256", hexDigest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatalf("create cache entry %s: %v", dir, err)
	}
	for _, name := range []string{"plugin.json", "plugin.wasm", "plugin.sig"} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, bytes), 0o600); err != nil {
			f.t.Fatalf("write %s: %v", name, err)
		}
	}
	return "sha256:" + hexDigest
}

func (f *cacheFixture) cacheEntryExists(hexDigest string) bool {
	f.t.Helper()

	_, err := os.Stat(filepath.Join(f.cacheDir, "sha256", hexDigest))
	return err == nil
}

// Three well-formed digests: 64 hex characters each, distinct in their first
// bytes so a failure message says which entry it means.
var (
	referencedHex   = "aa11" + strings.Repeat("0", 60)
	unreferencedHex = "bb22" + strings.Repeat("0", 60)
	olderHex        = "cc33" + strings.Repeat("0", 60)
)

func TestPluginsCacheListShowsReferencedAndUnreferencedEntries(t *testing.T) {
	f := newCacheFixture(t)
	referenced := f.writeCacheEntry(referencedHex, 16)
	f.writeCacheEntry(unreferencedHex, 16)
	f.writeManifest(manifestEntry{
		name: "plugin-a", source: "https://example.test/a.tar.gz", digest: referenced, enabled: true,
	})

	out, err := f.run("cache", "list")
	if err != nil {
		t.Fatalf("plugins cache list: %v\n%s", err, out)
	}
	if !strings.Contains(out, referencedHex) || !strings.Contains(out, unreferencedHex) {
		t.Errorf("list output does not name both entries:\n%s", out)
	}
	// The listing has to say which entries a prune would take, or an operator
	// has to cross-reference plugins.json by hand — which is the chore this
	// command exists to remove.
	if !strings.Contains(out, "referenced") || !strings.Contains(out, "unreferenced") {
		t.Errorf("list output does not mark which entries are still referenced:\n%s", out)
	}
}

func TestPluginsCachePruneRemovesOnlyUnreferencedEntries(t *testing.T) {
	f := newCacheFixture(t)
	referenced := f.writeCacheEntry(referencedHex, 16)
	f.writeCacheEntry(unreferencedHex, 16)
	f.writeManifest(manifestEntry{
		name: "plugin-a", source: "https://example.test/a.tar.gz", digest: referenced, enabled: true,
	})

	out, err := f.run("cache", "prune")
	if err != nil {
		t.Fatalf("plugins cache prune: %v\n%s", err, out)
	}
	if !f.cacheEntryExists(referencedHex) {
		t.Error("prune removed an entry the deployment still points at")
	}
	if f.cacheEntryExists(unreferencedHex) {
		t.Error("prune left an unreferenced entry behind")
	}
}

// TestPluginsCachePruneDryRunDeletesNothing: a command that deletes disk must
// be previewable, or the only way to learn what it would do is to let it do it.
func TestPluginsCachePruneDryRunDeletesNothing(t *testing.T) {
	f := newCacheFixture(t)
	f.writeCacheEntry(unreferencedHex, 16)
	f.writeManifest()

	out, err := f.run("cache", "prune", "--dry-run")
	if err != nil {
		t.Fatalf("plugins cache prune --dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, unreferencedHex) {
		t.Errorf("dry run does not name what it would remove:\n%s", out)
	}
	if !f.cacheEntryExists(unreferencedHex) {
		t.Error("dry run deleted an entry")
	}
}

// TestPluginsCachePruneMaxBytesNeverEvictsAReferencedEntry is the rule that
// keeps a disk-pressure command from turning into an outage: the deployment's
// own packages are off limits, and a target that cannot be met is reported
// rather than met by deleting them.
func TestPluginsCachePruneMaxBytesNeverEvictsAReferencedEntry(t *testing.T) {
	f := newCacheFixture(t)
	referenced := f.writeCacheEntry(referencedHex, 4096)
	f.writeManifest(manifestEntry{
		name: "plugin-a", source: "https://example.test/a.tar.gz", digest: referenced, enabled: true,
	})

	out, err := f.run("cache", "prune", "--max-bytes", "16")
	if err == nil {
		t.Fatalf("prune --max-bytes below the referenced size = nil error, want a report that it "+
			"could not get there:\n%s", out)
	}
	if !f.cacheEntryExists(referencedHex) {
		t.Fatal("prune --max-bytes deleted a referenced entry to hit its target")
	}
	if !strings.Contains(err.Error(), "referenced") && !strings.Contains(out, "referenced") {
		t.Errorf("the refusal does not explain that the remaining bytes are referenced: %v\n%s", err, out)
	}
}

func TestPluginsCachePruneMaxBytesEvictsOldestUnreferencedFirst(t *testing.T) {
	f := newCacheFixture(t)
	f.writeCacheEntry(olderHex, 4096)
	newer := f.writeCacheEntry(unreferencedHex, 4096)
	// Make the "older" entry unambiguously older than the other.
	old := filepath.Join(f.cacheDir, "sha256", olderHex)
	past := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatalf("age the entry: %v", err)
	}
	f.writeManifest()

	// Room for one of the two entries only.
	out, err := f.run("cache", "prune", "--max-bytes", "20000")
	if err != nil {
		t.Fatalf("plugins cache prune --max-bytes: %v\n%s", err, out)
	}
	if f.cacheEntryExists(olderHex) {
		t.Error("prune kept the oldest entry and evicted a newer one")
	}
	if !f.cacheEntryExists(strings.TrimPrefix(newer, "sha256:")) {
		t.Error("prune evicted more than it had to")
	}
}

func TestPluginsCacheRemoveWarnsWhenTheEntryIsStillReferenced(t *testing.T) {
	f := newCacheFixture(t)
	referenced := f.writeCacheEntry(referencedHex, 16)
	f.writeManifest(manifestEntry{
		name: "plugin-a", source: "https://example.test/a.tar.gz", digest: referenced, enabled: true,
	})

	out, err := f.run("cache", "remove", referenced)
	if err != nil {
		t.Fatalf("plugins cache remove: %v\n%s", err, out)
	}
	// The operator named it, so it goes — but they are told what they just
	// did, because the next reload will re-download it.
	if f.cacheEntryExists(referencedHex) {
		t.Error("remove left the entry the operator named")
	}
	if !strings.Contains(out, "plugin-a") {
		t.Errorf("remove of a referenced entry does not say which plugin still points at it:\n%s", out)
	}
}

func TestPluginsCacheRemoveOfAnAbsentDigestSaysSo(t *testing.T) {
	f := newCacheFixture(t)
	f.writeManifest()

	out, err := f.run("cache", "remove", "sha256:"+unreferencedHex)
	if err != nil {
		t.Fatalf("plugins cache remove of an absent digest: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not in the cache") {
		t.Errorf("output does not say the digest was not cached:\n%s", out)
	}
}

// TestPluginsCacheRefusesWhenNoCacheIsConfigured: with no plugins.cache there
// is no directory to operate on, and guessing one would have these commands
// deleting from a path nobody named.
func TestPluginsCacheRefusesWhenNoCacheIsConfigured(t *testing.T) {
	f := newPluginFixture(t, 1000) // writes a config with no "cache"
	f.writeManifest()

	for _, args := range [][]string{
		{"cache", "list"},
		{"cache", "prune"},
		{"cache", "remove", "sha256:" + unreferencedHex},
	} {
		out, err := f.run(args...)
		if err == nil {
			t.Errorf("%v = nil error, want a refusal naming plugins.cache:\n%s", args, out)
			continue
		}
		if !strings.Contains(err.Error(), "plugins.cache") {
			t.Errorf("%v error = %v, want it to name the missing setting", args, err)
		}
	}
}
