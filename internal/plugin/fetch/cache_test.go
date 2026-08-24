package fetch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	} {
		t.Run(digest, func(t *testing.T) {
			msg := mustPanicPath(t, fmt.Sprintf("Dir(%q)", digest), func() string {
				return c.Dir(digest)
			})
			if !strings.Contains(msg, "sha256") {
				t.Fatalf("panic message %q does not explain the digest shape it wanted", msg)
			}
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
			mustPanicPath(t, fmt.Sprintf("Dir(%q)", digest), func() string {
				return c.Dir(digest)
			})
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
