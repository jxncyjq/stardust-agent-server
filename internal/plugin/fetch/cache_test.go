package fetch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Cache fixtures.

// newTestCache builds a Cache rooted in a fresh temp directory and returns it
// together with that root, so a test can assert on the layout underneath it.
func newTestCache(t *testing.T) (*Cache, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "plugin-cache")
	c, err := NewCache(root)
	if err != nil {
		t.Fatalf("NewCache(%q): %v", root, err)
	}
	return c, root
}

// testArchive returns a well-formed package archive and the digest that names
// it — the pair a caller would hand Put after Fetch verified the bytes.
func testArchive(t *testing.T) ([]byte, string) {
	t.Helper()
	archive := buildTarGz(t, validEntries("")...)
	return archive, sha256Digest(archive)
}

// requireHas asserts Has(digest) reports want without erroring.
func requireHas(t *testing.T, c *Cache, digest string, want bool) {
	t.Helper()
	got, err := c.Has(digest)
	if err != nil {
		t.Fatalf("Has(%q): %v", digest, err)
	}
	if got != want {
		t.Fatalf("Has(%q) = %v, want %v", digest, got, want)
	}
}

// requireCompletePackage asserts dir holds exactly the three package files,
// each a regular file with the content the fixture archive carries.
func requireCompletePackage(t *testing.T, dir string) {
	t.Helper()
	for _, e := range validEntries("") {
		p := filepath.Join(dir, e.name)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("%s is not a regular file (mode %v)", p, info.Mode())
		}
		requireFileContent(t, dir, e.name, e.content)
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	if len(names) != len(packageFileNames) {
		t.Fatalf("%s holds %d entries, want exactly the %d package files", dir, len(names), len(packageFileNames))
	}
}

// requireOnlyEntry asserts dir contains exactly one entry, named want. It is
// how the tests check that Put left no temporary directory behind: a leaked
// temp directory shows up here as a second entry.
func requireOnlyEntry(t *testing.T, dir, want string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("%s contains %v, want exactly [%s]", dir, got, want)
	}
}

// mustPanicPath calls fn, which is expected to panic, and returns the panic
// value's message. If fn returns a path instead, the test fails naming that
// path — the failure an unvalidated digest shape would produce, so the message
// shows the path that escaped rather than only "no panic".
func mustPanicPath(t *testing.T, what string, fn func() string) string {
	t.Helper()

	var (
		msg       string
		returned  string
		panicked  bool
		recovered any
	)
	func() {
		defer func() {
			if recovered = recover(); recovered != nil {
				panicked = true
				msg = fmt.Sprint(recovered)
			}
		}()
		returned = fn()
	}()
	if !panicked {
		t.Fatalf("%s: want panic, got path %q", what, returned)
	}
	return msg
}

// requireDigestShapeMessage asserts msg is the refusal parseDigest produces —
// the one that names the shape it wanted. dirForHex's assertion panics with a
// different message ("cache path built from unvalidated digest"), so an entry
// point that stopped validating and fell through to it fails here even though
// something still panicked.
func requireDigestShapeMessage(t *testing.T, what, msg string) {
	t.Helper()
	for _, want := range []string{"sha256:", "64 hex digits"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("%s: panic message %q does not name the digest shape it wanted (no %q in it)", what, msg, want)
		}
	}
}

// incompleteEntryMarker is the content of the one file an incomplete entry
// holds, so a test can tell "still the entry I planted" from "replaced".
const incompleteEntryMarker = "half written"

// writeIncompleteEntry plants at dir the state a process killed mid-unpack
// leaves behind: the digest directory exists and holds one package file, so
// Has refuses it and Put has to replace it rather than take the
// already-complete path.
func writeIncompleteEntry(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), []byte(incompleteEntryMarker), 0o600); err != nil {
		t.Fatalf("write %s: %v", filepath.Join(dir, manifestFileName), err)
	}
}

// requireIncompleteEntry asserts dir is still exactly what writeIncompleteEntry
// planted — nothing removed it, nothing wrote into it.
func requireIncompleteEntry(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	if len(entries) != 1 || entries[0].Name() != manifestFileName {
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		t.Fatalf("%s holds %v, want exactly the planted [%s]", dir, got, manifestFileName)
	}
	content, err := os.ReadFile(filepath.Join(dir, manifestFileName))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(dir, manifestFileName), err)
	}
	if string(content) != incompleteEntryMarker {
		t.Fatalf("%s content = %q, want the planted %q", manifestFileName, content, incompleteEntryMarker)
	}
}

// waitForTempUnpackDirs blocks until dir holds exactly want entries named with
// tempUnpackDirPrefix — one per Put that has reached the unpack stage and not
// yet published — and fails at the deadline instead of waiting forever. It is
// how the contention test *proves* every writer is contending rather than
// assuming it: a Put that finished early has already removed its temporary
// directory, so the count never reaches want.
func waitForTempUnpackDirs(t *testing.T, dir string, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		var temps []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), tempUnpackDirPrefix) {
				temps = append(temps, e.Name())
			}
		}
		if len(temps) == want {
			return
		}
		if len(temps) > want {
			t.Fatalf("%s holds %d temporary unpack directories, want %d: %v", dir, len(temps), want, temps)
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d Puts reached the unpack stage within %s: %v", len(temps), want, within, temps)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// writeJunkDirectory creates dir/name as a NON-EMPTY directory. os.Remove
// refuses a non-empty directory on every platform, so a directory shaped like
// this standing where a package file belongs makes a write into that directory
// fail partway through — which is how the tests below reproduce an unpack that
// died mid-write.
func writeJunkDirectory(t *testing.T, dir, name string) {
	t.Helper()
	junk := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(junk, "junk"), 0o700); err != nil {
		t.Fatalf("create junk directory %s: %v", junk, err)
	}
	if err := os.WriteFile(filepath.Join(junk, "junk", "leftover"), []byte("leftover"), 0o600); err != nil {
		t.Fatalf("write junk file under %s: %v", junk, err)
	}
}

// --- NewCache.

func TestNewCache_CreatesItsRootDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "plugin-cache")

	c, err := NewCache(root)
	if err != nil {
		t.Fatalf("NewCache(%q): %v", root, err)
	}
	if c == nil {
		t.Fatal("NewCache returned a nil Cache with a nil error")
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat %s: %v", root, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", root)
	}
}

func TestNewCache_EmptyRoot_ReturnsError(t *testing.T) {
	c, err := NewCache("")
	requireErrorContains(t, err, "root")
	if c != nil {
		t.Fatalf("NewCache(\"\") returned a Cache alongside its error: %+v", c)
	}
}

func TestNewCache_RootPathIsAFile_ReturnsError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("write %s: %v", root, err)
	}

	c, err := NewCache(root)
	if err == nil {
		t.Fatalf("NewCache over a regular file should fail, got cache %+v", c)
	}
	if c != nil {
		t.Fatalf("NewCache returned a Cache alongside its error: %+v", c)
	}
}

// --- Rule 4: the digest's shape is validated before any path is built.

func TestCache_Dir_ReturnsAlgorithmScopedPath(t *testing.T) {
	c, root := newTestCache(t)
	hex := strings.Repeat("ab", 32)

	got := c.Dir("sha256:" + hex)

	want := filepath.Join(root, "sha256", hex)
	if got != want {
		t.Fatalf("Dir = %q, want %q", got, want)
	}
}

func TestCache_Dir_UppercaseHexDigest_MapsToTheLowercasePath(t *testing.T) {
	c, root := newTestCache(t)
	lower := strings.Repeat("ab", 32)
	upper := strings.ToUpper(lower)

	got := c.Dir("sha256:" + upper)

	want := filepath.Join(root, "sha256", lower)
	if got != want {
		t.Fatalf("Dir with an uppercase digest = %q, want the lowercase path %q", got, want)
	}
}

// TestCache_Dir_TraversalDigest_IsRefusedAndBuildsNoPath is the traversal
// test: Dir is the last place a hostile digest could turn into a path, so a
// digest that is not "sha256:" plus 64 hex digits must never produce one.
func TestCache_Dir_TraversalDigest_IsRefusedAndBuildsNoPath(t *testing.T) {
	c, root := newTestCache(t)

	for _, digest := range []string{
		"../../etc",
		"sha256:../../etc",
		"sha256:" + strings.Repeat("ab", 31) + "/../../../etc",
		"../../../etc/passwd",
		// The same forms written the way they escape on this platform: a
		// backslash separator and a drive letter. Both are refused by the
		// same anchored shape check, and pinning them here keeps a future
		// "be lenient about separators" edit from opening a hole that only
		// shows up on Windows.
		`sha256:..\..\etc`,
		`sha256:` + strings.Repeat("ab", 31) + `\..\..\..\etc`,
		`C:\Windows`,
	} {
		t.Run(digest, func(t *testing.T) {
			msg := mustPanicPath(t, fmt.Sprintf("Dir(%q)", digest), func() string {
				return c.Dir(digest)
			})
			requireDigestShapeMessage(t, fmt.Sprintf("Dir(%q)", digest), msg)
			// Nothing may have been created outside — or inside — the root
			// on the strength of a refused digest.
			escaped := filepath.Join(filepath.Dir(root), "etc")
			if _, err := os.Stat(escaped); err == nil {
				t.Fatalf("a refused digest created %s outside the cache root", escaped)
			}
		})
	}
}

func TestCache_Dir_MalformedDigests_AreRefused(t *testing.T) {
	c, _ := newTestCache(t)

	for name, digest := range map[string]string{
		"empty":            "",
		"no algorithm":     strings.Repeat("ab", 32),
		"wrong algorithm":  "sha512:" + strings.Repeat("ab", 32),
		"too short":        "sha256:" + strings.Repeat("ab", 31),
		"too long":         "sha256:" + strings.Repeat("ab", 33),
		"non hex":          "sha256:" + strings.Repeat("zz", 32),
		"trailing slash":   "sha256:" + strings.Repeat("ab", 32) + "/",
		"embedded null":    "sha256:" + strings.Repeat("ab", 32) + "\x00",
		"absolute looking": "/etc/passwd",
	} {
		t.Run(name, func(t *testing.T) {
			msg := mustPanicPath(t, fmt.Sprintf("Dir(%q)", digest), func() string {
				return c.Dir(digest)
			})
			// The message matters as much as the panic: dirForHex's own
			// assertion also panics, so a Dir that stopped validating
			// would still "panic" here. Only the shape message proves the
			// refusal happened where Dir promises it does.
			requireDigestShapeMessage(t, fmt.Sprintf("Dir(%q)", digest), msg)
		})
	}
}

func TestCache_Has_MalformedDigest_ReturnsError(t *testing.T) {
	c, _ := newTestCache(t)

	for name, digest := range map[string]string{
		"traversal":       "../../etc",
		"empty":           "",
		"wrong algorithm": "sha512:" + strings.Repeat("ab", 32),
		"too short":       "sha256:" + strings.Repeat("ab", 31),
	} {
		t.Run(name, func(t *testing.T) {
			ok, err := c.Has(digest)
			requireErrorContains(t, err, "sha256")
			if ok {
				t.Fatal("Has reported a hit for a malformed digest")
			}
		})
	}
}

func TestCache_Put_MalformedDigest_ReturnsError(t *testing.T) {
	c, root := newTestCache(t)
	archive, _ := testArchive(t)

	dir, err := c.Put("../../etc", archive, testUnpackLimits())
	requireErrorContains(t, err, "sha256")
	if dir != "" {
		t.Fatalf("Put returned path %q alongside its error", dir)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "etc")); err == nil {
		t.Fatal("Put unpacked outside the cache root on a malformed digest")
	}
}

// --- Storing and finding a package.

func TestCache_PutThenHas_StoresTheThreeFilesAtTheDigestPath(t *testing.T) {
	c, root := newTestCache(t)
	archive, digest := testArchive(t)

	requireHas(t, c, digest, false)

	dir, err := c.Put(digest, archive, testUnpackLimits())
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if want := c.Dir(digest); dir != want {
		t.Fatalf("Put returned %q, want %q", dir, want)
	}
	if want := filepath.Join(root, "sha256", strings.TrimPrefix(digest, "sha256:")); dir != want {
		t.Fatalf("Put stored the package at %q, want %q", dir, want)
	}

	requireHas(t, c, digest, true)
	requireCompletePackage(t, dir)
	requireOnlyEntry(t, filepath.Join(root, "sha256"), strings.TrimPrefix(digest, "sha256:"))
}

func TestCache_Has_AbsentDigest_IsAMiss(t *testing.T) {
	c, _ := newTestCache(t)

	requireHas(t, c, "sha256:"+strings.Repeat("cd", 32), false)
}

// TestCache_Has_PartialDirectory_IsNotAHit covers rule 3: a directory that
// exists but does not hold all three files is a miss. Treating it as a hit
// would load half a plugin, and — because the digest directory already exists
// — the deployment would never fetch the missing half.
func TestCache_Has_PartialDirectory_IsNotAHit(t *testing.T) {
	c, _ := newTestCache(t)
	_, digest := testArchive(t)
	dir := c.Dir(digest)

	for _, present := range [][]string{
		{},
		{manifestFileName},
		{manifestFileName, moduleFileName},
		{moduleFileName, signatureFileName},
	} {
		t.Run(fmt.Sprintf("%d of %d files", len(present), len(packageFileNames)), func(t *testing.T) {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatalf("clear %s: %v", dir, err)
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("create %s: %v", dir, err)
			}
			for _, name := range present {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("partial"), 0o600); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}
			requireHas(t, c, digest, false)
		})
	}
}

func TestCache_Has_PackageFileIsNotARegularFile_IsNotAHit(t *testing.T) {
	c, _ := newTestCache(t)
	_, digest := testArchive(t)
	dir := c.Dir(digest)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	for _, name := range []string{manifestFileName, signatureFileName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("partial"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	writeJunkDirectory(t, dir, moduleFileName)

	requireHas(t, c, digest, false)
}

func TestCache_Has_DigestPathIsAFile_ReturnsError(t *testing.T) {
	c, _ := newTestCache(t)
	_, digest := testArchive(t)
	dir := c.Dir(digest)

	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(dir), err)
	}
	if err := os.WriteFile(dir, []byte("not a package directory"), 0o600); err != nil {
		t.Fatalf("write %s: %v", dir, err)
	}

	ok, err := c.Has(digest)
	requireErrorContains(t, err, "not a directory")
	if ok {
		t.Fatal("Has reported a hit for a regular file standing where a package directory belongs")
	}
}

// --- Rule 2: Put on a digest already present is idempotent.

// TestCache_Put_ExistingDigest_DoesNotUnpackAgain proves the second Put never
// touches the archive it is handed: the bytes passed the second time are not a
// gzip stream at all, so any attempt to unpack them would fail loudly.
func TestCache_Put_ExistingDigest_DoesNotUnpackAgain(t *testing.T) {
	c, _ := newTestCache(t)
	archive, digest := testArchive(t)

	first, err := c.Put(digest, archive, testUnpackLimits())
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}

	second, err := c.Put(digest, []byte("this is not a gzip stream"), testUnpackLimits())
	if err != nil {
		t.Fatalf("second Put on an already-present digest should succeed without unpacking: %v", err)
	}
	if second != first {
		t.Fatalf("second Put returned %q, want %q", second, first)
	}
	requireCompletePackage(t, first)
}

// --- Rule 1: Put is atomic — an unpack that dies mid-write leaves nothing
// that reads as a hit, and a leftover from one is replaced whole.

// TestCache_Put_OverAnInterruptedUnpack_ReplacesItWhole reproduces the state a
// crashed unpack leaves behind — a digest directory holding some of the
// package plus something that makes a write into it fail partway — and
// requires Put to come out of it with a complete package. Put must therefore
// build the new package somewhere else and move it into place as a unit; an
// unpack that writes straight into the digest directory fails on the
// obstruction, having already overwritten part of the directory, and leaves a
// half package behind.
func TestCache_Put_OverAnInterruptedUnpack_ReplacesItWhole(t *testing.T) {
	c, root := newTestCache(t)
	archive, digest := testArchive(t)
	dir := c.Dir(digest)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), []byte("half written"), 0o600); err != nil {
		t.Fatalf("write %s: %v", manifestFileName, err)
	}
	writeJunkDirectory(t, dir, moduleFileName)

	// The half-written directory is not a hit, so the deployment fetches
	// again and puts the package a second time.
	requireHas(t, c, digest, false)

	got, err := c.Put(digest, archive, testUnpackLimits())
	if err != nil {
		t.Fatalf("Put over an interrupted unpack: %v", err)
	}
	if got != dir {
		t.Fatalf("Put returned %q, want %q", got, dir)
	}

	requireHas(t, c, digest, true)
	requireCompletePackage(t, dir)
	requireOnlyEntry(t, filepath.Join(root, "sha256"), strings.TrimPrefix(digest, "sha256:"))
}

func TestCache_Put_RefusedArchive_LeavesNoDigestDirectory(t *testing.T) {
	c, root := newTestCache(t)
	archive := []byte("this is not a gzip stream")
	digest := sha256Digest(archive)

	dir, err := c.Put(digest, archive, testUnpackLimits())
	if err == nil {
		t.Fatalf("Put of a non-archive should fail, got %q", dir)
	}
	if dir != "" {
		t.Fatalf("Put returned path %q alongside its error", dir)
	}
	if _, statErr := os.Stat(c.Dir(digest)); statErr == nil {
		t.Fatalf("a refused Put created the digest directory %s", c.Dir(digest))
	}
	requireHas(t, c, digest, false)

	entries, err := os.ReadDir(filepath.Join(root, "sha256"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", filepath.Join(root, "sha256"), err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused Put left %d entries behind under sha256/", len(entries))
	}
}

func TestCache_Put_DigestPathIsAFile_IsReportedAndNothingIsDeleted(t *testing.T) {
	c, _ := newTestCache(t)
	archive, digest := testArchive(t)
	dir := c.Dir(digest)

	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(dir), err)
	}
	const occupied = "not a package directory"
	if err := os.WriteFile(dir, []byte(occupied), 0o600); err != nil {
		t.Fatalf("write %s: %v", dir, err)
	}

	got, err := c.Put(digest, archive, testUnpackLimits())
	requireErrorContains(t, err, "not a directory")
	if got != "" {
		t.Fatalf("Put returned path %q alongside its error", got)
	}
	// Put replaces an incomplete package directory, but a cache holding
	// something that is not a directory at all was not built by these rules,
	// so nothing here may delete it.
	content, readErr := os.ReadFile(dir)
	if readErr != nil {
		t.Fatalf("read %s: %v", dir, readErr)
	}
	if string(content) != occupied {
		t.Fatalf("%s content = %q, want it untouched (%q)", dir, content, occupied)
	}
}

func TestCache_Put_InvalidLimits_ReturnsError(t *testing.T) {
	c, _ := newTestCache(t)
	archive, digest := testArchive(t)

	dir, err := c.Put(digest, archive, UnpackLimits{})
	requireErrorContains(t, err, "MaxEntries")
	if dir != "" {
		t.Fatalf("Put returned path %q alongside its error", dir)
	}
	requireHas(t, c, digest, false)
}

// --- Rule 2, concurrently: two Puts of the same digest must not corrupt each
// other.

// TestCache_ConcurrentPut_SameDigest_AllSucceed runs a fixed number of Puts of
// the same digest at once. Every count here is a literal and the wait is
// bounded: if the Puts deadlock, the test fails at the deadline instead of
// hanging.
//
// What it does and does not prove: these Puts share one Cache, so mu makes one
// of them unpack and the other nine take the already-complete fast path. It is
// therefore a test of idempotency under contention for the lock, not of the
// rename race — no two callers here are ever inside commit at once. The rename
// race belongs to TestCache_ConcurrentPut_SeparateCachesOneRoot_AllSucceed
// (separate Cache values share no mutex), and the case where every writer is
// *guaranteed* to be contending at the rename is
// TestCache_ConcurrentPut_OverAnIncompleteEntry_NeverDeletesAPublishedPackage,
// which arranges the pile-up instead of hoping for it.
func TestCache_ConcurrentPut_SameDigest_AllSucceed(t *testing.T) {
	const goroutines = 10

	c, root := newTestCache(t)
	archive, digest := testArchive(t)
	want := c.Dir(digest)

	var (
		wg    sync.WaitGroup
		dirs  [goroutines]string
		errs  [goroutines]error
		start = make(chan struct{})
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			dirs[i], errs[i] = c.Put(digest, archive, testUnpackLimits())
		}(i)
	}
	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("%d concurrent Puts did not finish within 60s", goroutines)
	}

	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("Put %d: %v", i, errs[i])
		}
		if dirs[i] != want {
			t.Fatalf("Put %d returned %q, want %q", i, dirs[i], want)
		}
	}

	requireHas(t, c, digest, true)
	requireCompletePackage(t, want)
	requireOnlyEntry(t, filepath.Join(root, "sha256"), strings.TrimPrefix(digest, "sha256:"))
}

// TestCache_ConcurrentPut_SeparateCachesOneRoot_AllSucceed covers the shape a
// second process takes: two Cache values over the same root share no mutex, so
// the losing rename is the only thing keeping them from corrupting each other.
// This is the one of the two plain concurrency tests that can actually race the
// rename — but it races it only if the scheduler happens to overlap the calls,
// which is why the guaranteed-contention case is a separate test below.
// Counts are literals and the wait is bounded, as above.
func TestCache_ConcurrentPut_SeparateCachesOneRoot_AllSucceed(t *testing.T) {
	const goroutines = 10

	root := filepath.Join(t.TempDir(), "plugin-cache")
	archive, digest := testArchive(t)

	var (
		wg     sync.WaitGroup
		dirs   [goroutines]string
		errs   [goroutines]error
		caches [goroutines]*Cache
		start  = make(chan struct{})
	)
	for i := 0; i < goroutines; i++ {
		c, err := NewCache(root)
		if err != nil {
			t.Fatalf("NewCache(%q): %v", root, err)
		}
		caches[i] = c
	}
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			dirs[i], errs[i] = caches[i].Put(digest, archive, testUnpackLimits())
		}(i)
	}
	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("%d concurrent Puts across separate caches did not finish within 60s", goroutines)
	}

	want := caches[0].Dir(digest)
	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("Put %d: %v", i, errs[i])
		}
		if dirs[i] != want {
			t.Fatalf("Put %d returned %q, want %q", i, dirs[i], want)
		}
	}

	requireHas(t, caches[0], digest, true)
	requireCompletePackage(t, want)
	requireOnlyEntry(t, filepath.Join(root, "sha256"), strings.TrimPrefix(digest, "sha256:"))
}

// --- Important-1: replacing an incomplete entry is serialized across writers.

// TestCache_Put_OverAnIncompleteEntry_WaitsForAnotherWritersLock is the direct
// proof that the replacement is serialized against a writer this process
// shares no mutex with. The test holds the digest lock itself — the same thing
// a second process mid-replacement holds — and requires Put to wait for it
// instead of acting on an observation that writer is about to invalidate.
// Every wait here is bounded, so a Put that never finishes fails the test
// rather than hanging it.
func TestCache_Put_OverAnIncompleteEntry_WaitsForAnotherWritersLock(t *testing.T) {
	c, root := newTestCache(t)
	archive, digest := testArchive(t)
	final := c.Dir(digest)

	writeIncompleteEntry(t, final)
	requireHas(t, c, digest, false)

	unlock, err := c.lockDigestDir(final)
	if err != nil {
		t.Fatalf("lockDigestDir(%q): %v", final, err)
	}

	type putResult struct {
		dir string
		err error
	}
	done := make(chan putResult, 1)
	go func() {
		dir, err := c.Put(digest, archive, testUnpackLimits())
		done <- putResult{dir, err}
	}()

	// While another writer holds the lock, this Put may not reach the
	// removal: that is the window in which it would delete work the lock
	// holder is about to publish.
	select {
	case got := <-done:
		t.Fatalf("Put finished (dir %q, err %v) while another writer held the digest lock: the replacement is not serialized across writers", got.dir, got.err)
	case <-time.After(500 * time.Millisecond):
	}
	requireIncompleteEntry(t, final)

	if err := unlock(); err != nil {
		t.Fatalf("release the digest lock: %v", err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Put after the digest lock was released: %v", got.err)
		}
		if got.dir != final {
			t.Fatalf("Put returned %q, want %q", got.dir, final)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Put did not finish within 60s of the digest lock being released")
	}

	requireHas(t, c, digest, true)
	requireCompletePackage(t, final)
	// The lock file is released on every path, so nothing but the digest
	// directory is left behind.
	requireOnlyEntry(t, filepath.Join(root, digestAlgorithmDirName), strings.TrimPrefix(digest, "sha256:"))
}

// TestCache_ConcurrentPut_OverAnIncompleteEntry_NeverDeletesAPublishedPackage
// is the shape the race actually takes: a crash leftover at the digest path
// and several writers, sharing no mutex, all deciding to replace it.
// Contention is arranged rather than hoped for — the digest lock is held until
// every writer has reached the unpack stage, which the count of temporary
// directories on disk proves — so all of them meet at the replacement. Once
// any of them has published, the entry must never be observed incomplete
// again: a caller holding that directory must not have it deleted underneath
// it.
//
// Counts are literals and every wait is bounded.
func TestCache_ConcurrentPut_OverAnIncompleteEntry_NeverDeletesAPublishedPackage(t *testing.T) {
	const goroutines = 10

	root := filepath.Join(t.TempDir(), "plugin-cache")
	archive, digest := testArchive(t)

	var caches [goroutines]*Cache
	for i := 0; i < goroutines; i++ {
		c, err := NewCache(root)
		if err != nil {
			t.Fatalf("NewCache(%q): %v", root, err)
		}
		caches[i] = c
	}
	final := caches[0].Dir(digest)
	writeIncompleteEntry(t, final)

	unlock, err := caches[0].lockDigestDir(final)
	if err != nil {
		t.Fatalf("lockDigestDir(%q): %v", final, err)
	}

	var (
		wg           sync.WaitGroup
		dirs         [goroutines]string
		errs         [goroutines]error
		complete     [goroutines]bool
		completeErrs [goroutines]error
		published    atomic.Bool
		start        = make(chan struct{})
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			dirs[i], errs[i] = caches[i].Put(digest, archive, testUnpackLimits())
			if errs[i] == nil {
				// The directory a caller is handed must be whole the
				// moment it is handed over, not eventually.
				complete[i], completeErrs[i] = isCompletePackage(dirs[i])
				published.Store(true)
			}
		}(i)
	}
	close(start)

	waitForTempUnpackDirs(t, filepath.Join(root, digestAlgorithmDirName), goroutines, 20*time.Second)

	// From the first publication onward, watch the entry: a writer acting on
	// a stale "it is incomplete" would delete it here.
	watchStop := make(chan struct{})
	watchFailure := make(chan string, 1)
	var watchWG sync.WaitGroup
	watchWG.Add(1)
	go func() {
		defer watchWG.Done()
		for {
			select {
			case <-watchStop:
				return
			default:
			}
			if published.Load() {
				ok, err := isCompletePackage(final)
				switch {
				case err != nil:
					watchFailure <- fmt.Sprintf("published entry %s became unreadable while other writers were still committing: %v", final, err)
					return
				case !ok:
					watchFailure <- fmt.Sprintf("published entry %s was deleted out from under its caller while another writer replaced it", final)
					return
				}
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()

	if err := unlock(); err != nil {
		t.Fatalf("release the digest lock: %v", err)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		close(watchStop)
		watchWG.Wait()
		t.Fatalf("%d concurrent Puts over an incomplete entry did not finish within 60s", goroutines)
	}
	close(watchStop)
	watchWG.Wait()
	select {
	case msg := <-watchFailure:
		t.Fatal(msg)
	default:
	}

	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("Put %d over an incomplete entry: %v", i, errs[i])
		}
		if dirs[i] != final {
			t.Fatalf("Put %d returned %q, want %q", i, dirs[i], final)
		}
		if completeErrs[i] != nil {
			t.Fatalf("Put %d: reading back the directory it returned: %v", i, completeErrs[i])
		}
		if !complete[i] {
			t.Fatalf("Put %d returned %q, but it did not hold a whole package the moment it was returned", i, dirs[i])
		}
	}

	requireHas(t, caches[0], digest, true)
	requireCompletePackage(t, final)
	// Neither a temporary directory nor a lock file survives: every writer
	// released what it took, on the winning path and the losing one alike.
	requireOnlyEntry(t, filepath.Join(root, digestAlgorithmDirName), strings.TrimPrefix(digest, "sha256:"))
}

// TestCache_Put_StaleDigestLock_IsReportedAndNothingIsDeleted covers the price
// of the lock: a process killed while holding it leaves the file behind. The
// wait is bounded, so the next writer reports it — naming the file to delete —
// instead of hanging, and it neither steals the lock nor touches the entry the
// lock guards.
func TestCache_Put_StaleDigestLock_IsReportedAndNothingIsDeleted(t *testing.T) {
	c, _ := newTestCache(t)
	archive, digest := testArchive(t)
	final := c.Dir(digest)

	writeIncompleteEntry(t, final)

	// Bound this Cache's wait tightly rather than stand in front of the
	// production bound for half a minute. Nothing reads these after NewCache
	// set them and this Cache is not shared, so writing them here races
	// nothing.
	c.lockWait = 100 * time.Millisecond
	c.lockPoll = 5 * time.Millisecond

	lockPath := final + digestLockSuffix
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("plant a stale lock file at %s: %v", lockPath, err)
	}

	dir, err := c.Put(digest, archive, testUnpackLimits())
	requireErrorContains(t, err, "cache lock")
	if dir != "" {
		t.Fatalf("Put returned path %q alongside its error", dir)
	}
	if !strings.Contains(err.Error(), lockPath) {
		t.Fatalf("error %q does not name the lock file %q that has to be cleared", err, lockPath)
	}
	// A lock this writer never held is not its to remove, and the entry it
	// could not replace is left exactly as it was.
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("Put removed another writer's lock file: %v", statErr)
	}
	requireIncompleteEntry(t, final)
	requireHas(t, c, digest, false)
}

func TestCache_LockDigestDir_UnusableLockPath_ReturnsError(t *testing.T) {
	c, _ := newTestCache(t)
	dir := filepath.Join(t.TempDir(), "no-such-parent", strings.Repeat("ab", 32))

	unlock, err := c.lockDigestDir(dir)
	requireErrorContains(t, err, "acquire cache lock")
	if unlock != nil {
		t.Fatal("lockDigestDir returned a release func alongside its error")
	}
}

// TestCache_LockDigestDir_UnconfiguredBounds_Panics covers the assertion that
// keeps a Cache built by struct literal — one that never went through NewCache
// and so carries a zero wait — from turning the bounded wait into no wait at
// all.
func TestCache_LockDigestDir_UnconfiguredBounds_Panics(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("ab", 32))

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		if _, err := (&Cache{}).lockDigestDir(dir); err != nil {
			t.Fatalf("want a panic for unconfigured lock bounds, got error %v", err)
		}
	}()
	if recovered == nil {
		t.Fatal("a Cache with unconfigured lock bounds took the lock instead of panicking")
	}
	if msg := fmt.Sprint(recovered); !strings.Contains(msg, "NewCache") {
		t.Fatalf("panic message %q does not say where a configured Cache comes from", msg)
	}
}
