package fetch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// digestAlgorithmDirName is the directory level between the cache root and a
// package's own directory. It exists so that the algorithm a digest was
// computed with is written down in the layout rather than assumed: if a second
// algorithm is ever supported, its packages land beside these instead of being
// indistinguishable from them.
const digestAlgorithmDirName = "sha256"

// tempUnpackDirPrefix prefixes the temporary directory Put unpacks into before
// moving it into place. The leading dot guarantees the name can never be
// mistaken for a digest directory — a digest directory's name is 64 hex
// digits and nothing else — so a temporary directory left behind by a killed
// process is never read as a cached package.
const tempUnpackDirPrefix = ".unpack-"

// Cache stores unpacked plugin packages under the digest that names them, so
// that a package fetched once is not fetched again.
//
// # The digest is the identity
//
// A sha256 digest names one exact sequence of bytes. Two packages with the
// same digest are the same package, so a cached entry can never be out of
// date: there is nothing to revalidate and nothing to expire, and a hit is
// usable without touching the network. That is the whole reason this cache
// exists — it is what lets a second start, or a machine with no network at
// all, load the same plugins the first start loaded.
//
// # Layout
//
//	<root>/sha256/<64 lowercase hex digits>/{plugin.json,plugin.wasm,plugin.sig}
//
// The digest directory holds exactly what Unpack writes and nothing else. A
// digest is accepted in the same shape Fetch accepts ("sha256:" followed by 64
// hex digits, either case) and is lowercased for the path, so the same package
// occupies one directory however its digest was spelled.
//
// # An entry is either whole or absent
//
// Put never writes into the digest directory: it unpacks into a temporary
// directory beside it and moves the finished package into place as a unit. Has
// correspondingly refuses to call a directory a hit unless all three package
// files are there. Together those two rules mean a half-written package can
// neither be produced by an interrupted Put nor mistaken for a usable one by
// the next start — which matters more here than elsewhere, because a digest
// directory that exists is a directory nothing will ever fetch again.
//
// # What Cache does not do
//
// It does not fetch (that is Fetch), does not verify that archive actually
// hashes to digest (that is Fetch's job, done before these bytes ever reach
// Put — Put treats digest purely as the name to file the package under), does
// not read the manifest or the module it stores, reads no configuration, and
// never evicts: nothing here deletes an entry to reclaim space, and no entry
// carries a size or an age. The one thing it does delete is an incomplete
// directory standing where a package belongs, and only as part of replacing it
// with a complete one.
//
// A Cache is safe for concurrent use.
type Cache struct {
	// root is absolute, so that the paths Cache hands out do not move when
	// the process changes its working directory.
	root string

	// mu serializes Put. Put's work — decide the entry is missing, unpack,
	// move it into place — is a read-modify-write of the digest directory,
	// and holding one lock across the whole of it is what keeps two Puts in
	// this process from interleaving their decisions. It deliberately covers
	// every digest rather than one lock per digest: Put runs when a plugin is
	// first installed, not on any hot path, and one lock is one thing to
	// reason about instead of a map of them.
	//
	// It says nothing about a *second process* over the same root. That case
	// is handled where it has to be, at the move into place: see commit.
	mu sync.Mutex
}

// NewCache returns a Cache rooted at root, creating the directory (mode 0700,
// parents included) if it does not exist. Creating it here rather than at the
// first Put means an unusable cache location — a path that is a regular file,
// a directory the process may not write — is reported when the cache is
// configured, not halfway through a plugin install.
//
// root must not be empty. It is resolved to an absolute path, so a later
// change of working directory cannot move the cache.
func NewCache(root string) (*Cache, error) {
	if root == "" {
		return nil, errors.New("cache root path is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve cache root %s: %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create cache root %s: %w", abs, err)
	}
	return &Cache{root: abs}, nil
}

// Dir returns the directory the package named by digest occupies, whether or
// not anything has been stored there yet. digest must be "sha256:" followed by
// 64 hex digits — the shape Fetch requires and Task 1's manifest package
// validates — and the returned path is always inside the cache root.
//
// Dir panics if digest is not that shape. This is the last point at which a
// digest could become a path, so it is the last point at which a hostile one
// ("../../etc") could escape the cache root, and a caller that reaches it with
// an unvalidated digest has a bug that must not be papered over with a path
// that happens to be harmless. Callers that may hold an unvalidated digest —
// anything reading one out of a manifest, a flag or a request — should call
// Has or Put, which validate the same shape and report a malformed digest as
// an error.
func (c *Cache) Dir(digest string) string {
	hexDigits, err := parseDigest(digest)
	if err != nil {
		panic(fmt.Sprintf("fetch: Cache.Dir: %v", err))
	}
	return c.dirForHex(hexDigits)
}

// Has reports whether the package named by digest is stored whole.
//
// It is a hit only if the digest directory exists and holds all three package
// files as regular files. A directory that exists but is missing one of them
// is a miss, not a hit: it is what an unpack that died mid-write would have
// left behind, and calling it a hit would load a partial plugin and — because
// the directory already exists — never fetch the rest. A miss is reported as
// (false, nil); only a filesystem failure, a malformed digest, or something
// other than a directory standing at the digest path is an error.
func (c *Cache) Has(digest string) (bool, error) {
	hexDigits, err := parseDigest(digest)
	if err != nil {
		return false, fmt.Errorf("cache lookup: %w", err)
	}
	ok, err := isCompletePackage(c.dirForHex(hexDigits))
	if err != nil {
		return false, fmt.Errorf("cache lookup %s: %w", digest, err)
	}
	return ok, nil
}

// Put stores archive — a gzipped tar, normally the bytes Fetch just verified
// against digest — as the package named by digest, and returns the directory
// it now occupies. limits bound the unpack exactly as they do for Unpack.
//
// Put does not verify that archive hashes to digest: Fetch does that before
// these bytes exist as a value, and repeating it here would suggest Put is a
// place where unverified bytes may safely arrive. It is not.
//
// # Already present is a success
//
// If the digest is already stored whole, Put returns its directory without
// reading archive at all. Same digest, same bytes — there is nothing a second
// unpack could produce that the first did not.
//
// # Either the whole package appears, or none of it
//
// Put unpacks into a temporary directory beside the digest directory and moves
// the result into place with a single rename, so the digest directory never
// exists in a partly written state: a Put that fails — a refused archive, a
// full disk, a killed process — leaves the cache exactly as it found it, minus
// a temporary directory that the next Put's rename does not depend on. This is
// what Unpack itself cannot promise: Unpack refuses a bad archive without
// writing anything, but an I/O failure partway through its writes leaves the
// directory it was given half-populated, with nothing marking it incomplete.
// Put makes that directory a temporary one so it never matters.
//
// An incomplete directory that is already at the digest path — left by a
// process killed before this rule existed, or by a hand-edited cache — is
// replaced whole rather than written into or left to wedge the digest forever.
func (c *Cache) Put(digest string, archive []byte, limits UnpackLimits) (dir string, err error) {
	hexDigits, parseErr := parseDigest(digest)
	if parseErr != nil {
		return "", fmt.Errorf("cache put: %w", parseErr)
	}
	if limitsErr := limits.validate(); limitsErr != nil {
		return "", fmt.Errorf("cache put %s: %w", digest, limitsErr)
	}
	final := c.dirForHex(hexDigits)

	c.mu.Lock()
	defer c.mu.Unlock()

	complete, err := isCompletePackage(final)
	if err != nil {
		return "", fmt.Errorf("cache put %s: %w", digest, err)
	}
	if complete {
		return final, nil
	}

	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("cache put %s: create %s: %w", digest, parent, err)
	}
	// The temporary directory is created inside parent, not in the system
	// temp directory: a rename is only atomic — and on most systems only
	// possible at all — within one filesystem, and parent is the one place
	// guaranteed to be on the same filesystem as the destination.
	tmp, err := os.MkdirTemp(parent, tempUnpackDirPrefix)
	if err != nil {
		return "", fmt.Errorf("cache put %s: create temporary unpack directory in %s: %w", digest, parent, err)
	}
	defer func() {
		// After a successful commit tmp no longer exists and this is a
		// no-op. On every other path it is the cleanup that keeps a failed
		// Put from leaving anything behind — and if even that fails, the
		// leftover is reported rather than dropped, because a directory
		// nothing can delete in the cache root is a condition worth hearing
		// about.
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary unpack directory %s: %w", tmp, rmErr))
			dir = ""
		}
	}()

	if err := Unpack(archive, tmp, limits); err != nil {
		return "", fmt.Errorf("cache put %s: %w", digest, err)
	}
	if err := commit(tmp, final); err != nil {
		return "", fmt.Errorf("cache put %s: %w", digest, err)
	}
	return final, nil
}

// dirForHex builds the path for an already-validated 64-hex-digit digest. It
// is the only place a cache path is assembled, and it is unexported precisely
// so that it can assume its argument was validated: Dir, Has and Put each run
// parseDigest first, and the invariant is asserted here as well so that a
// future caller that forgets cannot quietly build a path out of anything else.
func (c *Cache) dirForHex(hexDigits string) string {
	if !isHexDigest(hexDigits) {
		panic(fmt.Sprintf("fetch: cache path built from unvalidated digest %q", hexDigits))
	}
	return filepath.Join(c.root, digestAlgorithmDirName, strings.ToLower(hexDigits))
}

// isHexDigest reports whether s is exactly 64 hex digits — the name a digest
// directory may have, and nothing else. It is deliberately a hand-written
// check rather than a second regexp: this is the assertion that guards path
// construction, and it should not be able to drift from what it guards.
func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// commit moves a finished package from tmp to final in one step.
//
// The rename is the moment the package becomes visible, and it is a single
// operation: there is no state in which final holds part of tmp. A rename onto
// an existing directory fails on every platform this runs on (POSIX refuses a
// non-empty destination; Windows refuses any existing one), which is exactly
// the signal that something is already there, and there are only two things it
// can be:
//
//   - A complete package. Another writer — another process over the same cache
//     root, since one process serializes its own Puts — won the race. Its
//     bytes are this package's bytes, because the digest says so, so the race
//     is already won by both.
//   - An incomplete one. Nothing may read it (Has refuses it) and nothing else
//     will ever replace it (its digest directory exists), so it is removed and
//     the rename retried. This is the only place the cache deletes anything.
//
// Any other rename failure is reported, naming both attempts.
func commit(tmp, final string) error {
	firstErr := os.Rename(tmp, final)
	if firstErr == nil {
		return nil
	}

	complete, err := isCompletePackage(final)
	if err != nil {
		return fmt.Errorf("move %s into place: %w (after %v)", tmp, err, firstErr)
	}
	if complete {
		return nil
	}

	if err := os.RemoveAll(final); err != nil {
		return fmt.Errorf("replace incomplete cache entry %s: %w (after %v)", final, err, firstErr)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("move %s to %s: %w (first attempt: %v)", tmp, final, err, firstErr)
	}
	return nil
}

// isCompletePackage reports whether dir holds a whole package: it exists, it
// is a directory, and every one of the three package files is present as a
// regular file. Anything less is (false, nil) — a miss, which is a state Put
// knows how to fix — while a filesystem failure, or a non-directory standing
// where the package directory belongs, is an error, because neither is a
// question this package can answer by guessing.
func isCompletePackage(dir string) (bool, error) {
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s exists but is not a directory", dir)
	}

	for _, name := range packageFileNames {
		p := filepath.Join(dir, name)
		fi, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("stat %s: %w", p, err)
		}
		if !fi.Mode().IsRegular() {
			return false, nil
		}
	}
	return true, nil
}
