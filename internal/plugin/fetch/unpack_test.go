package fetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// --- Fixtures.
//
// Every fixture below — including every adversarial one — is built here, in
// memory, with archive/tar and compress/gzip. None of them is ever committed
// to the repository: a path-traversal tarball sitting in a source tree is
// eventually found by a scanner or by a well-meaning reader and treated as a
// real finding, and a decompression bomb on disk is a foot-gun for anyone who
// unpacks it by hand.

// tarEntry describes one entry buildTarGz should write. Non-regular entries
// (symlink, hard link, device, FIFO) carry no content; only typeflag and, for
// links, linkname matter for them.
type tarEntry struct {
	name     string
	typeflag byte
	content  []byte
	linkname string
}

// regularEntry is the common case: one regular file with content.
func regularEntry(name string, content []byte) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeReg, content: content}
}

// buildTarGz writes entries into a gzipped tar in memory and returns its
// bytes. It writes exactly what it is told, including entries a well-behaved
// packaging tool would never produce — that is the point.
func buildTarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     0o644,
			Linkname: e.linkname,
		}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %q: %v", e.name, err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write(e.content); err != nil {
				t.Fatalf("write tar content %q: %v", e.name, err)
			}
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

// validEntries returns the three files the layout contract requires, each
// prefixed with prefix (pass "" for a flat archive, "pkg/" for one wrapped in
// a single top-level directory).
func validEntries(prefix string) []tarEntry {
	return []tarEntry{
		regularEntry(prefix+"plugin.json", []byte(`{"name":"demo"}`)),
		regularEntry(prefix+"plugin.wasm", []byte("\x00asm\x01\x00\x00\x00")),
		regularEntry(prefix+"plugin.sig", []byte("signature-bytes")),
	}
}

// testUnpackLimits are generous enough that a well-formed fixture never trips
// them, so a test that wants to exercise one limit can lower just that one.
func testUnpackLimits() UnpackLimits {
	return UnpackLimits{MaxEntries: 16, MaxTotalBytes: 64 << 10, MaxEntryBytes: 32 << 10}
}

// newDest returns a destination path inside a fresh temp dir that does NOT
// exist yet, so "was anything written?" can be answered by asking whether it
// exists at all.
func newDest(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "pkg")
}

// requireNothingWritten asserts dir is either absent or empty — the check that
// a refusal refused the package *whole*, leaving no partial extraction behind.
func requireNothingWritten(t *testing.T, dir string) {
	t.Helper()
	names, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read dir %s: %v", dir, err)
	}
	if len(names) != 0 {
		t.Fatalf("destination %s should be empty after a refusal, contains %d entries", dir, len(names))
	}
}

// requireFileContent asserts dir/name holds want.
func requireFileContent(t *testing.T, dir, name string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s content = %q, want %q", name, got, want)
	}
}

// requirePerm asserts p's permission bits. Windows does not model Unix
// permission bits at all (os.Stat reports 0666/0444 from the read-only
// attribute), so the assertion is skipped there rather than asserted falsely.
func requirePerm(t *testing.T, p string, want fs.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Logf("skipping permission assertion for %s: Windows does not model Unix permission bits", p)
		return
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s permissions = %#o, want %#o", p, got, want)
	}
}

// --- Positive cases: the layout contract, satisfied.

func TestUnpack_FlatLayout_WritesTheThreeFiles(t *testing.T) {
	dest := newDest(t)
	entries := validEntries("")

	if err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits()); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	for _, e := range entries {
		requireFileContent(t, dest, e.name, e.content)
		requirePerm(t, filepath.Join(dest, e.name), 0o600)
	}
	requirePerm(t, dest, 0o700)

	got, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("read dir %s: %v", dest, err)
	}
	if len(got) != len(entries) {
		t.Fatalf("destination holds %d entries, want exactly %d", len(got), len(entries))
	}
}

func TestUnpack_SingleTopLevelDirectory_IsStripped(t *testing.T) {
	dest := newDest(t)
	entries := append([]tarEntry{{name: "pkg/", typeflag: tar.TypeDir}}, validEntries("pkg/")...)

	if err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits()); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	for _, e := range validEntries("") {
		requireFileContent(t, dest, e.name, e.content)
	}
	if _, err := os.Stat(filepath.Join(dest, "pkg")); !os.IsNotExist(err) {
		t.Fatalf("top-level directory was not stripped: stat pkg = %v", err)
	}
}

// A tarball built with `tar -czf pkg.tgz -C dir .` names its entries "./" and
// "./plugin.json". Those are not traversal — path.Clean resolves them to the
// same flat layout — and refusing them would refuse the most obvious way an
// operator packages a directory.
func TestUnpack_DotSlashPrefixedNames_AreAccepted(t *testing.T) {
	dest := newDest(t)
	entries := append([]tarEntry{{name: "./", typeflag: tar.TypeDir}}, validEntries("./")...)

	if err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits()); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	for _, e := range validEntries("") {
		requireFileContent(t, dest, e.name, e.content)
	}
}

// Task 4 will re-unpack into a directory that may already hold an older
// package. The files it leaves behind must still end up 0600, which
// os.WriteFile alone would not guarantee: it keeps an existing file's mode.
func TestUnpack_ExistingDestination_OverwritesWithFreshPermissions(t *testing.T) {
	dest := newDest(t)
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dest, err)
	}
	stale := filepath.Join(dest, "plugin.wasm")
	if err := os.WriteFile(stale, []byte("stale"), 0o666); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	if err := Unpack(buildTarGz(t, validEntries("")...), dest, testUnpackLimits()); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	requireFileContent(t, dest, "plugin.wasm", []byte("\x00asm\x01\x00\x00\x00"))
	requirePerm(t, stale, 0o600)
}

// --- Rule 1: ".." / absolute / escaping names refuse the whole package.

func TestUnpack_EscapingTraversalEntry_IsRejected(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "pkg")
	entries := append(validEntries(""), regularEntry("../evil.json", []byte("pwned")))

	err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits())

	requireErrorContains(t, err, `"../evil.json"`)
	requireErrorContains(t, err, "..")
	requireNothingWritten(t, dest)
	if _, statErr := os.Stat(filepath.Join(root, "evil.json")); !os.IsNotExist(statErr) {
		t.Fatalf("a file escaped the destination directory: stat = %v", statErr)
	}
}

// This entry does not escape: path.Clean resolves "pkg/nested/../plugin.sig"
// to "pkg/plugin.sig", which the layout contract would happily accept. Rule 1
// refuses it anyway — a name that contains ".." at all is refused, so the
// refusal never depends on getting the resolution arithmetic right.
func TestUnpack_NonEscapingDotDotElement_IsStillRejected(t *testing.T) {
	dest := newDest(t)
	entries := []tarEntry{
		regularEntry("pkg/plugin.json", []byte(`{"name":"demo"}`)),
		regularEntry("pkg/plugin.wasm", []byte("\x00asm\x01\x00\x00\x00")),
		regularEntry("pkg/nested/../plugin.sig", []byte("signature-bytes")),
	}

	err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits())

	requireErrorContains(t, err, `"pkg/nested/../plugin.sig"`)
	requireErrorContains(t, err, "..")
	requireNothingWritten(t, dest)
}

func TestUnpack_AbsoluteEntryName_IsRejected(t *testing.T) {
	dest := newDest(t)
	entries := append(validEntries(""), regularEntry("/etc/legion-plugin.json", []byte("pwned")))

	err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits())

	requireErrorContains(t, err, "/etc/legion-plugin.json")
	requireErrorContains(t, err, "absolute")
	requireNothingWritten(t, dest)
}

// A backslash is a path separator on Windows, so a name that carries one is
// refused rather than reasoned about per-platform.
func TestUnpack_BackslashEntryName_IsRejected(t *testing.T) {
	dest := newDest(t)
	entries := append(validEntries(""), regularEntry(`..\evil.json`, []byte("pwned")))

	err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits())

	requireErrorContains(t, err, "backslash")
	requireNothingWritten(t, dest)
}

// --- Rule 2: only regular files (and directories) may appear.

func TestUnpack_NonRegularEntries_AreRejected(t *testing.T) {
	cases := []struct {
		name     string
		entry    tarEntry
		wantKind string
	}{
		{
			name:     "symlink",
			entry:    tarEntry{name: "evil", typeflag: tar.TypeSymlink, linkname: "../../../../etc/passwd"},
			wantKind: "symbolic link",
		},
		{
			name:     "hard link",
			entry:    tarEntry{name: "evil", typeflag: tar.TypeLink, linkname: "plugin.wasm"},
			wantKind: "hard link",
		},
		{
			name:     "fifo",
			entry:    tarEntry{name: "evil", typeflag: tar.TypeFifo},
			wantKind: "FIFO",
		},
		{
			name:     "character device",
			entry:    tarEntry{name: "evil", typeflag: tar.TypeChar},
			wantKind: "character device",
		},
		{
			name:     "block device",
			entry:    tarEntry{name: "evil", typeflag: tar.TypeBlock},
			wantKind: "block device",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := newDest(t)
			// The three required files are all present and well-formed: the
			// package is refused *because of* the extra entry, not because
			// something else was missing. An implementation that skipped the
			// entry instead of refusing would accept this archive.
			entries := append(validEntries(""), tc.entry)

			err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits())

			requireErrorContains(t, err, `"evil"`)
			requireErrorContains(t, err, tc.wantKind)
			requireNothingWritten(t, dest)
		})
	}
}

// --- Rules 3, 4, 5: three separate, all-necessary bounds.

func TestUnpack_TooManyEntries_IsRejected(t *testing.T) {
	dest := newDest(t)
	limits := testUnpackLimits()
	limits.MaxEntries = 2

	err := Unpack(buildTarGz(t, validEntries("")...), dest, limits)

	requireErrorContains(t, err, "more than 2 entries")
	requireNothingWritten(t, dest)
}

func TestUnpack_EntryOverPerEntryLimit_IsRejected(t *testing.T) {
	dest := newDest(t)
	limits := testUnpackLimits()
	limits.MaxEntryBytes = 1 << 10

	entries := validEntries("")
	entries[1] = regularEntry("plugin.wasm", bytes.Repeat([]byte("A"), int(limits.MaxEntryBytes)+1))

	err := Unpack(buildTarGz(t, entries...), dest, limits)

	requireErrorContains(t, err, `"plugin.wasm"`)
	requireErrorContains(t, err, "1024 byte per-entry limit")
	requireNothingWritten(t, dest)
}

// The bomb is deliberately tiny: it decompresses to MaxTotalBytes + 1KB, which
// is 65KB here, not gigabytes. A real bomb would be a hazard to the machine
// running the test the moment the bound under test is removed — which is
// exactly what the mutation step does on purpose.
//
// All three required files are present and each is under MaxEntryBytes, so the
// only rule this archive breaks is the total-bytes bound: an implementation
// without that bound would accept this package outright.
func TestUnpack_DecompressionBomb_IsRejected(t *testing.T) {
	dest := newDest(t)
	limits := testUnpackLimits()
	limits.MaxEntryBytes = 1 << 20

	bomb := bytes.Repeat([]byte{0}, int(limits.MaxTotalBytes)+1024)
	entries := validEntries("")
	entries[1] = regularEntry("plugin.wasm", bomb)

	archive := buildTarGz(t, entries...)
	if int64(len(archive)) >= limits.MaxTotalBytes {
		t.Fatalf("fixture is not compressible enough to prove the bound is on decompressed bytes: archive is %d bytes, limit is %d", len(archive), limits.MaxTotalBytes)
	}

	err := Unpack(archive, dest, limits)

	requireErrorContains(t, err, "65536 byte total limit")
	requireNothingWritten(t, dest)
}

// --- Rules 6 and 7: exactly three files, no more and no fewer.

func TestUnpack_UnexpectedFileName_IsRejectedByName(t *testing.T) {
	dest := newDest(t)
	entries := append(validEntries(""), regularEntry("README.md", []byte("hello")))

	err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits())

	requireErrorContains(t, err, `"README.md"`)
	requireNothingWritten(t, dest)
}

func TestUnpack_NestedFileUnderStrippedDirectory_IsRejected(t *testing.T) {
	dest := newDest(t)
	entries := append(validEntries("pkg/"), regularEntry("pkg/nested/extra.txt", []byte("hello")))

	err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits())

	requireErrorContains(t, err, "nested/extra.txt")
	requireNothingWritten(t, dest)
}

// Two different top-level directories are not "a single top-level directory",
// so nothing is stripped and every name fails the contract.
func TestUnpack_TwoTopLevelDirectories_IsRejected(t *testing.T) {
	dest := newDest(t)
	entries := []tarEntry{
		regularEntry("a/plugin.json", []byte(`{"name":"demo"}`)),
		regularEntry("a/plugin.wasm", []byte("\x00asm\x01\x00\x00\x00")),
		regularEntry("b/plugin.sig", []byte("signature-bytes")),
	}

	err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits())

	requireErrorContains(t, err, "a/plugin.json")
	requireNothingWritten(t, dest)
}

func TestUnpack_MissingRequiredFile_IsRejectedByName(t *testing.T) {
	for _, missing := range []string{"plugin.json", "plugin.wasm", "plugin.sig"} {
		t.Run(missing, func(t *testing.T) {
			dest := newDest(t)
			var entries []tarEntry
			for _, e := range validEntries("") {
				if e.name != missing {
					entries = append(entries, e)
				}
			}

			err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits())

			requireErrorContains(t, err, "missing")
			requireErrorContains(t, err, missing)
			requireNothingWritten(t, dest)
		})
	}
}

func TestUnpack_EmptyArchive_IsRejected(t *testing.T) {
	dest := newDest(t)

	err := Unpack(buildTarGz(t), dest, testUnpackLimits())

	requireErrorContains(t, err, "missing")
	requireNothingWritten(t, dest)
}

// A second entry for a name already seen would overwrite the file the first
// one produced — after that first one had been judged acceptable.
func TestUnpack_DuplicateEntry_IsRejected(t *testing.T) {
	dest := newDest(t)
	entries := append(validEntries(""), regularEntry("plugin.wasm", []byte("second")))

	err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits())

	requireErrorContains(t, err, `"plugin.wasm"`)
	requireErrorContains(t, err, "more than once")
	requireNothingWritten(t, dest)
}

// --- Malformed input and degenerate limits.

func TestUnpack_NotAGzipStream_IsRejected(t *testing.T) {
	dest := newDest(t)

	err := Unpack([]byte("this is not a gzip stream"), dest, testUnpackLimits())

	requireErrorContains(t, err, "gzip")
	requireNothingWritten(t, dest)
}

func TestUnpack_TruncatedArchive_IsRejected(t *testing.T) {
	dest := newDest(t)
	archive := buildTarGz(t, validEntries("")...)

	err := Unpack(archive[:len(archive)/2], dest, testUnpackLimits())

	if err == nil {
		t.Fatal("want error for a truncated archive, got nil")
	}
	requireNothingWritten(t, dest)
}

// No limit has a "zero means unlimited" reading: a caller that leaves one
// unset is told so, rather than silently getting an unbounded extraction.
func TestUnpack_NonPositiveLimits_AreRejected(t *testing.T) {
	cases := []struct {
		name   string
		limits UnpackLimits
		want   string
	}{
		{"zero MaxEntries", UnpackLimits{MaxEntries: 0, MaxTotalBytes: 1024, MaxEntryBytes: 1024}, "MaxEntries"},
		{"zero MaxTotalBytes", UnpackLimits{MaxEntries: 4, MaxTotalBytes: 0, MaxEntryBytes: 1024}, "MaxTotalBytes"},
		{"zero MaxEntryBytes", UnpackLimits{MaxEntries: 4, MaxTotalBytes: 1024, MaxEntryBytes: 0}, "MaxEntryBytes"},
		{"negative MaxEntryBytes", UnpackLimits{MaxEntries: 4, MaxTotalBytes: 1024, MaxEntryBytes: -1}, "MaxEntryBytes"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := newDest(t)

			err := Unpack(buildTarGz(t, validEntries("")...), dest, tc.limits)

			requireErrorContains(t, err, tc.want)
			requireNothingWritten(t, dest)
		})
	}
}

// An empty destination is a programming error in the caller, not a runtime
// condition to be reported: there is no directory it could ever mean.
func TestUnpack_EmptyDestDir_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic for an empty destDir, got none")
		}
	}()

	_ = Unpack(buildTarGz(t, validEntries("")...), "", testUnpackLimits())
}
