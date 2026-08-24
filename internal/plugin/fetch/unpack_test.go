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
// links, linkname matter for them. paxRecords is only used for extended
// header entries (tar.TypeXHeader / tar.TypeXGlobalHeader), the shape a
// `git archive`-produced pax_global_header entry takes.
type tarEntry struct {
	name       string
	typeflag   byte
	content    []byte
	linkname   string
	paxRecords map[string]string
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
		// A TypeXGlobalHeader entry (a `git archive` pax_global_header) is
		// special-cased by archive/tar itself: it refuses any field but
		// Name, Typeflag and PAXRecords ("only PAXRecords should be set for
		// TypeXGlobalHeader"), matching what git archive actually produces.
		var hdr *tar.Header
		if e.typeflag == tar.TypeXGlobalHeader {
			hdr = &tar.Header{Name: e.name, Typeflag: e.typeflag, PAXRecords: e.paxRecords}
		} else {
			hdr = &tar.Header{
				Name:       e.name,
				Typeflag:   e.typeflag,
				Mode:       0o644,
				Linkname:   e.linkname,
				PAXRecords: e.paxRecords,
			}
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

// --- Review follow-up: git archive, untested fail-loud branches, declared
// vs. actual entry size, and an over-permissive existing destDir.

// A `git archive --format=tar.gz` output always contains a pax_global_header
// entry (typeflag 'g'). It is refused whole like any other non-regular
// entry — the reviewer's own warning governs here: honoring it would mean
// opening a second "skip this entry" path, and "refuse whole, never skip"
// is the structural invariant this file exists to hold. What changes is the
// wording: "is a tar extended header" is accurate but useless to an operator
// who has no idea their packaging tool produced one, so the refusal names
// the actual cause and the fix (use tar, not git archive) instead.
func TestUnpack_GitArchiveExtendedHeader_IsRejectedWithActionableMessage(t *testing.T) {
	dest := newDest(t)
	entries := append([]tarEntry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, paxRecords: map[string]string{"comment": "deadbeef"}},
	}, validEntries("")...)

	err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits())

	requireErrorContains(t, err, "git archive")
	requireErrorContains(t, err, "tar")
	requireNothingWritten(t, dest)
}

// A regular file may not be named "." — that name is accepted only for a
// directory entry (see TestUnpack_DotSlashPrefixedNames_AreAccepted), where
// it means "the archive root itself". A *regular file* claiming that name is
// nonsensical and decodeEntries refuses it outright (unpack.go's
// `if name == "."` branch) rather than trying to make sense of it.
func TestUnpack_RegularFileNamedDot_IsRejected(t *testing.T) {
	dest := newDest(t)
	entries := append(validEntries(""), regularEntry(".", []byte("x")))

	err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits())

	requireErrorContains(t, err, "archive root")
	requireNothingWritten(t, dest)
}

// An empty entry name is reachable, not theoretical: archive/tar's Writer
// accepts Name == "" without error and Reader.Next hands it back unchanged
// (verified directly — WriteHeader on a Header with an empty Name returns
// nil, and the resulting entry round-trips through Reader.Next with
// Name == ""). validateEntryName refuses it before any name-shape check
// would even have something to look at.
func TestUnpack_EmptyEntryName_IsRejected(t *testing.T) {
	dest := newDest(t)
	entries := append(validEntries(""), tarEntry{name: "", typeflag: tar.TypeReg, content: []byte("x")})

	err := Unpack(buildTarGz(t, entries...), dest, testUnpackLimits())

	requireErrorContains(t, err, "empty name")
	requireNothingWritten(t, dest)
}

// buildTarGzWithLyingEntrySize hand-assembles a gzipped tar whose one entry
// declares declaredSize in its header while len(actualContent) bytes
// physically follow it. archive/tar's own Writer refuses to produce this
// (tw.Write returns "archive/tar: write too long" the moment written bytes
// would exceed the header's declared Size), so the header is written
// through tar.Writer — self-contained: a short ASCII name stays within one
// 512-byte USTAR block, confirmed by inspection — and the content that
// follows is written directly to the underlying buffer instead, padded to
// its own (not the declared) 512-byte boundary, then closed with a bare
// end-of-archive marker. There is deliberately no valid way to append
// further entries after this one; see the comment on the test that calls
// this for why.
func buildTarGzWithLyingEntrySize(t *testing.T, name string, declaredSize int64, actualContent []byte) []byte {
	t.Helper()

	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: declaredSize}); err != nil {
		t.Fatalf("write lying header: %v", err)
	}
	if _, err := raw.Write(actualContent); err != nil {
		t.Fatalf("write physical content: %v", err)
	}
	if pad := (512 - len(actualContent)%512) % 512; pad > 0 {
		raw.Write(make([]byte, pad))
	}
	raw.Write(make([]byte, 1024)) // end-of-archive marker: two zero blocks

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return gzBuf.Bytes()
}

// M-4 asked for a fixture whose header declares a small size while the
// entry body physically carries MaxEntryBytes+1 bytes, to pin that Unpack
// counts bytes actually read rather than trusting hdr.Size. Direct
// experimentation (see the report's fix-pass section for the probe and its
// output) established that this cannot be built as a fixture that
// distinguishes the two: archive/tar.Reader's Read never yields more than
// the header's own declared Size for an entry, no matter what bytes
// physically follow it in the stream — "trust hdr.Size" and "count bytes
// actually read from a *tar.Reader" are the same operation, because the
// library enforces the boundary itself before any caller-side code sees a
// byte. Placing MaxEntryBytes+1 bytes after a 10-byte declared header
// therefore does not smuggle those bytes into the entry: decodeEntries reads
// exactly 10 bytes for it (well under the limit, not rejected on that
// ground), and the stream is left desynced, so archive/tar refuses the
// *next* header as corrupt. That still refuses this archive as a whole —
// nothing is written — but not for the reason M-4 wanted pinned, and the
// mutation it was meant to catch (checking hdr.Size instead of bytes read)
// does not make this test fail, because it produces the identical desync.
// Kept anyway: it is still a real regression test for "a size-inconsistent
// entry is refused, never partially accepted."
func TestUnpack_EntryDeclaredSizeSmallerThanPhysicalContent_IsRejected(t *testing.T) {
	dest := newDest(t)
	limits := testUnpackLimits()
	limits.MaxEntryBytes = 1 << 10

	archive := buildTarGzWithLyingEntrySize(t, "plugin.wasm", 10, bytes.Repeat([]byte("A"), int(limits.MaxEntryBytes)+1))

	err := Unpack(archive, dest, limits)

	if err == nil {
		t.Fatal("want error for an entry whose declared size disagrees with its physical content, got nil")
	}
	requireNothingWritten(t, dest)
}

// unpack.go's explicit Chmod(destDir, 0o700) exists for exactly this case: a
// destination directory that already exists and is more permissive than a
// directory holding an executable wasm module and its signature should be.
func TestUnpack_ExistingDestDirTooPermissive_IsTightened(t *testing.T) {
	dest := newDest(t)
	if err := os.MkdirAll(dest, 0o777); err != nil {
		t.Fatalf("mkdir %s: %v", dest, err)
	}
	if err := os.Chmod(dest, 0o777); err != nil {
		t.Fatalf("chmod %s: %v", dest, err)
	}

	if err := Unpack(buildTarGz(t, validEntries("")...), dest, testUnpackLimits()); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	requirePerm(t, dest, 0o700)
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
