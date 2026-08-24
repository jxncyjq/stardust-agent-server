package fetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// The three files a plugin package consists of, and nothing else: the
// manifest, the wasm module it describes, and the signature that authorizes
// the module. Unpack refuses any archive that carries a fourth file, because
// "refuse the unexpected" is one fewer path by which extra content lands in a
// directory later code trusts.
const (
	manifestFileName  = "plugin.json"
	moduleFileName    = "plugin.wasm"
	signatureFileName = "plugin.sig"
)

// packageFileNames is the layout contract in list form, in the order Unpack
// writes the files.
var packageFileNames = [...]string{manifestFileName, moduleFileName, signatureFileName}

// errTotalBytesExceeded is what boundedReader hands back to archive/tar once
// a stream has produced more decompressed bytes than UnpackLimits.MaxTotalBytes
// allows. It never reaches a caller of Unpack: decodeEntries recognizes the
// condition and reports it in terms of the limit that was actually configured.
var errTotalBytesExceeded = errors.New("decompressed archive exceeds its total byte limit")

// UnpackLimits bounds one Unpack call. All three fields must be positive —
// none of them has a "zero means unlimited" reading, and Unpack refuses a
// non-positive one rather than treating it as no limit at all.
//
// All three are necessary, and none substitutes for another: a total-bytes
// bound alone does not stop a hundred million empty files, and an entry-count
// bound alone does not stop a single 10 GB one.
type UnpackLimits struct {
	// MaxEntries is the largest number of tar entries — regular files and
	// directories alike — Unpack will read before refusing the archive. It
	// bounds the extraction loop itself.
	MaxEntries int

	// MaxTotalBytes bounds the total number of DECOMPRESSED bytes Unpack will
	// read out of the gzip stream: file contents plus the 512-byte tar headers
	// and padding around them. It deliberately bounds the decompressed size
	// rather than len(archive), because a compression ratio can be enormous
	// and bounding the input therefore bounds nothing. Callers should leave a
	// little headroom above the sum of the file sizes they expect, to cover
	// the per-entry header and padding blocks.
	MaxTotalBytes int64

	// MaxEntryBytes bounds the decompressed size of any single entry.
	MaxEntryBytes int64
}

// validate reports whether every limit is positive, naming the offending
// field. A caller that leaves one unset gets told so, rather than silently
// getting an unbounded extraction.
func (l UnpackLimits) validate() error {
	if l.MaxEntries <= 0 {
		return fmt.Errorf("UnpackLimits.MaxEntries must be positive, got %d", l.MaxEntries)
	}
	if l.MaxTotalBytes <= 0 {
		return fmt.Errorf("UnpackLimits.MaxTotalBytes must be positive, got %d", l.MaxTotalBytes)
	}
	if l.MaxEntryBytes <= 0 {
		return fmt.Errorf("UnpackLimits.MaxEntryBytes must be positive, got %d", l.MaxEntryBytes)
	}
	return nil
}

// packageEntry is one regular file read out of the archive, held in memory
// until the whole package has been judged acceptable.
type packageEntry struct {
	name    string
	content []byte
}

// Unpack extracts archive — a gzipped tar, normally the bytes Fetch just
// verified against a digest — into destDir, which it creates (mode 0700) if it
// does not exist. This is where verified bytes become files: everything before
// it is bytes in memory that a digest vouched for, and everything after it is
// files on disk that later code will read, verify a signature over, and
// execute as wasm. A digest says "these are the bytes the deployment asked
// for"; it says nothing about whether those bytes are a well-formed package or
// a bomb, which is what Unpack decides.
//
// # A malformed package is refused whole, never partially
//
// Unpack reads the entire archive into memory (bounded — see limits) and
// validates all of it before creating destDir or writing a single file. A
// refusal therefore leaves nothing behind: there is no state in which a
// hostile entry was rejected but the entries beside it were kept. Skipping a
// bad entry and continuing is exactly how a hostile package becomes an
// accepted package minus one file, so Unpack never does it.
//
// # The layout contract
//
// The archive must contain exactly plugin.json, plugin.wasm and plugin.sig,
// either at its root or under a single top-level directory whose name is
// stripped (so both "plugin.json" and "pkg/plugin.json" are accepted, but
// "a/plugin.json" beside "b/plugin.sig" is not). Directory entries are read,
// name-checked and otherwise ignored — a directory carries no content, and the
// only directory Unpack ever creates is destDir itself. Leading "./" is
// resolved away, so an archive built with `tar -czf pkg.tgz -C dir .` is
// accepted. Any other file name, a repeated name, or a missing required file
// refuses the archive, naming the entry at fault.
//
// # What is refused, and why
//
//   - An entry whose name contains a ".." element, is absolute, or contains a
//     backslash (a path separator on Windows) — refused on the name alone,
//     without regard for whether it would in fact have escaped, so that the
//     refusal never depends on getting the resolution arithmetic right. The
//     resolved path is checked against destDir afterward as well.
//   - Anything that is not a regular file or a directory: symbolic links, hard
//     links, devices, FIFOs. Symlinks are the most common traversal vector and
//     no legitimate plugin package needs one.
//   - Archives that exceed any of limits' three bounds (see UnpackLimits).
//
// Files are written 0600 and destDir is 0700, and an existing file is removed
// before being rewritten so that it cannot keep a more permissive mode from an
// earlier unpack: this directory holds a wasm module that will be executed and
// the signature that authorizes it.
//
// # What Unpack does not do
//
// Unpack does not fetch (that is Fetch), does not cache or decide where a
// package should live, does not read the manifest it just wrote, and does not
// verify the signature beside it. It panics if destDir is empty, which is a
// programming error in the caller rather than a runtime condition.
func Unpack(archive []byte, destDir string, limits UnpackLimits) error {
	if destDir == "" {
		panic("fetch: destDir is empty")
	}
	if err := limits.validate(); err != nil {
		return fmt.Errorf("unpack into %s: %w", destDir, err)
	}

	entries, err := decodeEntries(archive, destDir, limits)
	if err != nil {
		return fmt.Errorf("unpack into %s: %w", destDir, err)
	}
	files, err := applyLayout(entries)
	if err != nil {
		return fmt.Errorf("unpack into %s: %w", destDir, err)
	}
	if err := writeFiles(destDir, files); err != nil {
		return fmt.Errorf("unpack into %s: %w", destDir, err)
	}
	return nil
}

// decodeEntries reads every regular file out of archive and returns them in
// archive order, holding their contents in memory. Nothing is written to disk
// here — that is what makes an all-or-nothing refusal possible — and the
// memory that holds them is bounded by limits.MaxTotalBytes.
func decodeEntries(archive []byte, destDir string, limits UnpackLimits) ([]packageEntry, error) {
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return nil, fmt.Errorf("resolve destination directory %s: %w", destDir, err)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("read gzip header: %w", err)
	}

	// bounded is what makes the gzip stream safe to read at all: it stops the
	// decompressor after MaxTotalBytes+1 bytes no matter how small the archive
	// that produced them was. Reading exactly one byte past the limit is what
	// distinguishes "the archive is exactly at the limit" (legal) from "the
	// archive exceeds it" (refused).
	bounded := &boundedReader{r: gz, remaining: limits.MaxTotalBytes + 1}
	tr := tar.NewReader(bounded)

	var entries []packageEntry
	seen := make(map[string]bool)
	count := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if bounded.over {
				return nil, totalBytesError(limits)
			}
			return nil, fmt.Errorf("read tar entry: %w", err)
		}

		count++
		if count > limits.MaxEntries {
			return nil, fmt.Errorf("archive contains more than %d entries", limits.MaxEntries)
		}

		name, err := validateEntryName(hdr.Name, absDest)
		if err != nil {
			return nil, err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			// Directories carry no content and none of them is ever created:
			// after the single top-level directory is stripped the layout is
			// flat. The name was still checked above, so a directory cannot
			// smuggle a traversal past this point either.
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf(
				"entry %q is a %s; a plugin package may contain only regular files and directories",
				hdr.Name, entryKind(hdr.Typeflag),
			)
		}

		if name == "." {
			return nil, fmt.Errorf("entry %q names the archive root but is a regular file", hdr.Name)
		}
		if seen[name] {
			return nil, fmt.Errorf("entry %q appears more than once", name)
		}
		seen[name] = true

		var content bytes.Buffer
		n, err := io.Copy(&content, io.LimitReader(tr, limits.MaxEntryBytes+1))
		if err != nil {
			if bounded.over {
				return nil, totalBytesError(limits)
			}
			return nil, fmt.Errorf("read entry %q: %w", name, err)
		}
		if n > limits.MaxEntryBytes {
			return nil, fmt.Errorf("entry %q exceeds the %d byte per-entry limit", name, limits.MaxEntryBytes)
		}

		entries = append(entries, packageEntry{name: name, content: content.Bytes()})
	}

	// Closed only here, on the path where the archive was read to its tar
	// end-of-archive marker. gzip.Reader wraps a bytes.Reader and holds no OS
	// resource, so an early return above leaks nothing; what Close can still
	// report on this path is a decompressor error, which must not be dropped.
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("close gzip stream: %w", err)
	}
	return entries, nil
}

// totalBytesError reports the total-decompressed-bytes refusal in terms of the
// limit the caller actually configured.
func totalBytesError(limits UnpackLimits) error {
	return fmt.Errorf("decompressed archive exceeds the %d byte total limit", limits.MaxTotalBytes)
}

// validateEntryName refuses any entry name that could place a file outside
// absDest — an absolute path, a backslash (a path separator on Windows, so a
// name carrying one is refused rather than reasoned about per platform), or a
// ".." element — and returns the cleaned, slash-separated name of one that
// survives. It returns "." for an entry that names the archive root itself
// ("./"), which decodeEntries accepts only for a directory.
//
// A ".." element is refused even when it would resolve back inside absDest
// ("pkg/nested/../plugin.sig"), so that the refusal never depends on the
// resolution arithmetic being right. The containment check afterward is the
// second, independent line: it catches anything the name-shape rules did not
// anticipate.
func validateEntryName(rawName, absDest string) (string, error) {
	if rawName == "" {
		return "", errors.New("archive contains an entry with an empty name")
	}
	if strings.ContainsRune(rawName, '\\') {
		return "", fmt.Errorf("entry %q contains a backslash, which is a path separator on some platforms", rawName)
	}
	if strings.HasPrefix(rawName, "/") || filepath.IsAbs(rawName) || filepath.VolumeName(rawName) != "" {
		return "", fmt.Errorf("entry %q is an absolute path", rawName)
	}
	for _, elem := range strings.Split(rawName, "/") {
		if elem == ".." {
			return "", fmt.Errorf(`entry %q contains a ".." path element`, rawName)
		}
	}

	cleaned := path.Clean(rawName)
	if cleaned == "." {
		return ".", nil
	}
	target := filepath.Join(absDest, filepath.FromSlash(cleaned))
	if !strings.HasPrefix(target, absDest+string(filepath.Separator)) {
		return "", fmt.Errorf("entry %q resolves to %s, outside the destination directory", rawName, target)
	}
	return cleaned, nil
}

// entryKind names a tar type flag in the words a refusal should use, so that
// an operator reading the error learns what was actually in the archive.
func entryKind(typeflag byte) string {
	switch typeflag {
	case tar.TypeSymlink:
		return "symbolic link"
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	case tar.TypeFifo:
		return "FIFO"
	case tar.TypeXHeader, tar.TypeXGlobalHeader:
		return "tar extended header"
	default:
		return fmt.Sprintf("tar entry of type %q", string(rune(typeflag)))
	}
}

// applyLayout strips a single shared top-level directory if there is one and
// then checks what remains against the layout contract: exactly plugin.json,
// plugin.wasm and plugin.sig. It returns them in packageFileNames order, so
// that what gets written does not depend on the order the archive happened to
// list them in.
func applyLayout(entries []packageEntry) ([]packageEntry, error) {
	stripped := stripTopLevelDir(entries)

	byName := make(map[string]packageEntry, len(stripped))
	for _, e := range stripped {
		if !isPackageFileName(e.name) {
			return nil, fmt.Errorf(
				"entry %q is not one of the %d files a plugin package may contain (%s)",
				e.name, len(packageFileNames), strings.Join(packageFileNames[:], ", "),
			)
		}
		if _, ok := byName[e.name]; ok {
			return nil, fmt.Errorf("entry %q appears more than once", e.name)
		}
		byName[e.name] = e
	}

	files := make([]packageEntry, 0, len(packageFileNames))
	for _, want := range packageFileNames {
		e, ok := byName[want]
		if !ok {
			return nil, fmt.Errorf("archive is missing required file %q", want)
		}
		files = append(files, e)
	}
	return files, nil
}

// stripTopLevelDir removes the leading path element shared by every entry, but
// only when every entry has one and they all agree — "pkg/plugin.json" and its
// two siblings become the flat three. Anything else (a flat archive, or two
// different top-level directories) is returned untouched, and then fails the
// contract check in applyLayout under its full name.
func stripTopLevelDir(entries []packageEntry) []packageEntry {
	if len(entries) == 0 {
		return entries
	}
	top, _, ok := strings.Cut(entries[0].name, "/")
	if !ok {
		return entries
	}
	for _, e := range entries {
		prefix, rest, ok := strings.Cut(e.name, "/")
		if !ok || prefix != top || rest == "" {
			return entries
		}
	}

	out := make([]packageEntry, len(entries))
	for i, e := range entries {
		_, rest, _ := strings.Cut(e.name, "/")
		out[i] = packageEntry{name: rest, content: e.content}
	}
	return out
}

// isPackageFileName reports whether name is one of the three the layout
// contract allows.
func isPackageFileName(name string) bool {
	for _, want := range packageFileNames {
		if name == want {
			return true
		}
	}
	return false
}

// writeFiles creates destDir (0700) and writes files into it (0600). It runs
// only after the whole archive has been validated, so it is the first moment
// anything reaches the filesystem. An existing file is removed before being
// rewritten: os.WriteFile keeps the mode of a file that already exists, which
// would let a previous unpack's more permissive mode survive into this one.
func writeFiles(destDir string, files []packageEntry) error {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}
	if err := os.Chmod(destDir, 0o700); err != nil {
		return fmt.Errorf("restrict destination directory permissions: %w", err)
	}

	for _, f := range files {
		p := filepath.Join(destDir, f.name)
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove existing %s: %w", f.name, err)
		}
		if err := os.WriteFile(p, f.content, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	return nil
}

// boundedReader caps how many bytes may be read from an underlying reader —
// here, out of a gzip decompressor, where the number of bytes that come out
// bears no relation to the number that went in. Once the cap is exhausted
// every further read fails with errTotalBytesExceeded and over stays set, so
// the condition is still recognizable after archive/tar has had a chance to
// translate the error on its way back up.
//
// It is not io.LimitReader: LimitReader reports exhaustion as io.EOF, which
// archive/tar would read as a truncated archive rather than as an archive that
// was refused for being too large.
type boundedReader struct {
	r         io.Reader
	remaining int64
	over      bool
}

// Read implements io.Reader.
func (b *boundedReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		b.over = true
		return 0, errTotalBytesExceeded
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= int64(n)
	return n, err
}
