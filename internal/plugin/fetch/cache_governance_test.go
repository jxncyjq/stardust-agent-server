package fetch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests cover the two operations that make the cache something other
// than a write-only hole: enumerating what is in it, and taking something out.
// Until they existed the only way to inspect or reclaim a plugin cache was to
// go read the directory by hand, and a package that failed signature
// verification stayed on disk forever.

// defaultLimits mirrors what a deployment passes Put; the exact numbers do not
// matter here, only that they are positive.
func governanceLimits() UnpackLimits {
	return UnpackLimits{MaxEntries: 16, MaxTotalBytes: 1 << 20, MaxEntryBytes: 1 << 20}
}

func TestCacheListReportsCompleteAndIncompleteEntries(t *testing.T) {
	c, root := newTestCache(t)
	archive, digest := testArchive(t)
	if _, err := c.Put(digest, archive, governanceLimits()); err != nil {
		t.Fatalf("Put(%q): %v", digest, err)
	}
	// A half-written entry is exactly what an operator needs to SEE: it takes
	// up space and Has() never counts it as a hit, so filtering it out of the
	// listing would hide the only thing they could act on.
	incompleteHex := strings.Repeat("ab", 32)
	writeIncompleteEntry(t, filepath.Join(root, "sha256", incompleteHex))

	entries, err := c.List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List() returned %d entries, want 2: %+v", len(entries), entries)
	}

	byDigest := map[string]CacheEntry{}
	for _, entry := range entries {
		byDigest[entry.Digest] = entry
	}
	complete, ok := byDigest[digest]
	if !ok {
		t.Fatalf("List() has no entry for the stored package %q: %+v", digest, entries)
	}
	if !complete.Complete {
		t.Errorf("stored package Complete = false, want true")
	}
	if complete.Bytes <= 0 {
		t.Errorf("stored package Bytes = %d, want the sum of its files", complete.Bytes)
	}
	if complete.ModTime.IsZero() {
		t.Error("stored package ModTime is zero, want the directory's modification time")
	}

	partial, ok := byDigest["sha256:"+incompleteHex]
	if !ok {
		t.Fatalf("List() dropped the incomplete entry: %+v", entries)
	}
	if partial.Complete {
		t.Error("incomplete entry Complete = true, want false")
	}
}

// TestCacheListSkipsTempAndLockArtifacts: an operator reading the listing must
// see cache ENTRIES. A half-finished unpack directory or a lock file is
// bookkeeping, and reporting one as a package would send them looking for a
// plugin called ".unpack-1234".
func TestCacheListSkipsTempAndLockArtifacts(t *testing.T) {
	c, root := newTestCache(t)
	archive, digest := testArchive(t)
	if _, err := c.Put(digest, archive, governanceLimits()); err != nil {
		t.Fatalf("Put(%q): %v", digest, err)
	}
	shard := filepath.Join(root, "sha256")
	if err := os.MkdirAll(filepath.Join(shard, ".unpack-123456"), 0o700); err != nil {
		t.Fatalf("create temp unpack dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shard, strings.Repeat("cd", 32)+".lock"), nil, 0o600); err != nil {
		t.Fatalf("create lock file: %v", err)
	}
	// A directory whose name is not a digest at all: neither an entry nor a
	// known artifact, and still not something to report as a package.
	writeJunkDirectory(t, shard, "not-a-digest")

	entries, err := c.List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(entries) != 1 || entries[0].Digest != digest {
		t.Errorf("List() = %+v, want exactly the stored package %q", entries, digest)
	}
}

func TestCacheListOnAnEmptyCacheIsEmptyNotAnError(t *testing.T) {
	c, _ := newTestCache(t)

	entries, err := c.List()
	if err != nil {
		t.Fatalf("List() on an empty cache: %v, want nil error — an empty cache is a normal state", err)
	}
	if len(entries) != 0 {
		t.Errorf("List() = %+v, want no entries", entries)
	}
}

func TestCacheRemoveDeletesTheEntryAndReportsIt(t *testing.T) {
	c, _ := newTestCache(t)
	archive, digest := testArchive(t)
	dir, err := c.Put(digest, archive, governanceLimits())
	if err != nil {
		t.Fatalf("Put(%q): %v", digest, err)
	}
	requireHas(t, c, digest, true)

	removed, err := c.Remove(digest)
	if err != nil {
		t.Fatalf("Remove(%q): %v", digest, err)
	}
	if !removed {
		t.Error("Remove reported false for an entry that was there, want true")
	}
	requireHas(t, c, digest, false)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("stat %s after Remove: %v, want the directory to be gone", dir, err)
	}
}

// TestCacheRemoveOfAnAbsentDigestIsFalseNotAnError: removing what is not there
// is not a failure — but it must say "there was nothing", not report a
// deletion that never happened.
func TestCacheRemoveOfAnAbsentDigestIsFalseNotAnError(t *testing.T) {
	c, _ := newTestCache(t)

	removed, err := c.Remove("sha256:" + strings.Repeat("ef", 32))
	if err != nil {
		t.Fatalf("Remove of an absent digest: %v, want nil error (idempotent)", err)
	}
	if removed {
		t.Error("Remove reported true for a digest that was never cached")
	}
}

// TestCacheRemoveRefusesAMalformedDigest guards the only entry point in this
// package that calls os.RemoveAll: the string it is handed must be PROVEN to
// be a digest before any path is built from it.
func TestCacheRemoveRefusesAMalformedDigest(t *testing.T) {
	c, root := newTestCache(t)
	archive, digest := testArchive(t)
	if _, err := c.Put(digest, archive, governanceLimits()); err != nil {
		t.Fatalf("Put(%q): %v", digest, err)
	}

	for _, malformed := range []string{
		"",
		strings.Repeat("ab", 32),             // no algorithm prefix
		"sha256:" + strings.Repeat("z", 64),  // not hex
		"sha256:" + strings.Repeat("ab", 16), // too short
		"sha256:../../etc",                   // path traversal shape
		"md5:" + strings.Repeat("ab", 16),
	} {
		removed, err := c.Remove(malformed)
		if err == nil {
			t.Errorf("Remove(%q) = nil error, want a refusal", malformed)
		}
		if removed {
			t.Errorf("Remove(%q) reported a removal", malformed)
		}
	}

	// Nothing was deleted along the way.
	requireHas(t, c, digest, true)
	if _, err := os.Stat(root); err != nil {
		t.Errorf("stat cache root after malformed removals: %v, want it intact", err)
	}
}

func TestRemoveStaleStagingLeavesFreshDirectoriesAlone(t *testing.T) {
	// A staging directory that was created moments ago belongs to a download in
	// progress. Deleting it would corrupt a fetch that is about to succeed.
	c, root := newTestCache(t)
	fresh := filepath.Join(root, "sha256", ".unpack-fresh")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatalf("create staging dir: %v", err)
	}

	removed, err := c.RemoveStaleStaging(time.Hour, false)
	if err != nil {
		t.Fatalf("RemoveStaleStaging: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed %v, want nothing: the directory is younger than the cutoff", removed)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("stat %s: %v, want it untouched", fresh, err)
	}
}

func TestRemoveStaleStagingRemovesAbandonedDirectories(t *testing.T) {
	c, root := newTestCache(t)
	stale := filepath.Join(root, "sha256", ".unpack-stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("create staging dir: %v", err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatalf("age the staging dir: %v", err)
	}

	removed, err := c.RemoveStaleStaging(time.Hour, false)
	if err != nil {
		t.Fatalf("RemoveStaleStaging: %v", err)
	}
	if len(removed) != 1 || removed[0] != ".unpack-stale" {
		t.Fatalf("removed %v, want [.unpack-stale]", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stat %s: %v, want it gone", stale, err)
	}
}

func TestRemoveStaleStagingDryRunDeletesNothing(t *testing.T) {
	c, root := newTestCache(t)
	stale := filepath.Join(root, "sha256", ".unpack-stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("create staging dir: %v", err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatalf("age the staging dir: %v", err)
	}

	removed, err := c.RemoveStaleStaging(time.Hour, true)
	if err != nil {
		t.Fatalf("RemoveStaleStaging(dryRun): %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("dry run reported %v, want the one stale directory", removed)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("dry run deleted %s: %v", stale, err)
	}
}

// TestRemoveStaleStagingRefusesANonPositiveAge: "older than zero" is every
// directory, including the one being written right now.
func TestRemoveStaleStagingRefusesANonPositiveAge(t *testing.T) {
	c, root := newTestCache(t)
	fresh := filepath.Join(root, "sha256", ".unpack-fresh")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatalf("create staging dir: %v", err)
	}

	if _, err := c.RemoveStaleStaging(0, false); err == nil {
		t.Error("RemoveStaleStaging(0) = nil error, want a refusal")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("stat %s: %v, want it untouched", fresh, err)
	}
}
