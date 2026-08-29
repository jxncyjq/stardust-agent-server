package fetch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
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

// digestLockSuffix names the lock file guarding one digest directory:
// "<64 hex digits>.lock", a sibling of the directory it guards. The suffix
// makes the name longer than 64 characters, so — like tempUnpackDirPrefix —
// it can never be mistaken for a digest directory.
const digestLockSuffix = ".lock"

// digestLockTimeout bounds how long a writer waits for another writer's lock
// on the same digest before giving up with an error. The lock is held only
// across a removal and a rename, so a wait anywhere near this long means the
// holder died: the bound exists so that a leftover lock file fails loudly
// instead of hanging a plugin install forever.
const digestLockTimeout = 30 * time.Second

// digestLockPollInterval is how often a waiting writer retries the exclusive
// create. There is no cross-platform way to wait on a file's disappearance
// with only the standard library, so this polls; the interval is short enough
// that the common uncontended case is unaffected and the contended one costs a
// few milliseconds.
const digestLockPollInterval = 10 * time.Millisecond

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
// Once an entry is whole it is never removed, by this process or another one
// over the same root: the only removal here is of an *incomplete* entry, and
// it happens under a lock file that serializes it against every other writer
// that obeys these rules (see commit and lockDigestDir). A directory this
// package handed a caller therefore does not disappear underneath it.
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
	// It says nothing about a *second process* over the same root — a mutex
	// in this process cannot. That case is handled where it has to be, at
	// the move into place: the rename itself is atomic on every platform
	// this runs on, and the one step that is not a single operation —
	// replacing an incomplete entry, which is a check followed by a removal
	// followed by a rename — is serialized by a lock file, the only
	// construct that serializes across processes. See commit and
	// lockDigestDir.
	mu sync.Mutex

	// lockWait bounds how long commit waits for another writer's lock on the
	// same digest, and lockPoll is how often it retries while waiting. They
	// are fields rather than the constants read directly so that a caller
	// building a Cache for a test can bound the wait tightly instead of
	// standing in front of the production bound for half a minute. NewCache
	// sets them once and nothing writes them afterwards, so they need no
	// synchronization; a Cache that did not come from NewCache carries zeroes
	// and is refused where they are used.
	lockWait time.Duration
	lockPoll time.Duration
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
	abs, err := CacheRoot(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create cache root %s: %w", abs, err)
	}
	return &Cache{root: abs, lockWait: digestLockTimeout, lockPoll: digestLockPollInterval}, nil
}

// CacheRoot returns the absolute directory a Cache rooted at root would use,
// without creating anything or touching the filesystem's contents.
//
// It is the resolution NewCache performs, exported for the one caller that
// must compare a cache location it has only read from a config against the
// root of a Cache that is already running (`agent plugins reload` does, to
// refuse a reload after "plugins.cache" moved). That comparison has to use
// this function rather than its own idea of resolving a path: two spellings of
// the resolution would eventually disagree, and a disagreement here reports
// "unchanged" for a cache that moved — the silent half-applied change the
// comparison exists to prevent.
//
// An empty root is an error, not the working directory: a cache location the
// caller did not state is not a location.
func CacheRoot(root string) (string, error) {
	if root == "" {
		return "", errors.New("cache root path is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve cache root %s: %w", root, err)
	}
	return abs, nil
}

// Root returns the absolute directory this Cache files packages under. It is
// what a caller comparing a running Cache against a configured location reads;
// the Cache's own contents are reached through Has, Dir and Put.
func (c *Cache) Root() string {
	return c.root
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
//
// "All three files present" is the whole test: a directory holding the three
// files *plus* something else is still a hit. Put cannot produce such a
// directory — it arrives whole by rename, and Unpack writes exactly those
// three files — so the state only exists in a hand-edited cache, and refusing
// it would turn a stray file into a package that can never be repaired.
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
// That replacement is the one place this package deletes anything, and it runs
// under a per-digest lock file ("<64 hex>.lock", beside the digest directory)
// so that a second process cannot delete an entry a first one has already
// published and handed to a caller: see commit. A process killed while holding
// that lock leaves the file behind, and the next Put fails after a bounded
// wait, naming it — an operator clears it by deleting exactly that file.
// Something that is not a directory at all standing at that path is a
// different matter: that is a cache nobody built by these rules, so Put
// reports it and deletes nothing.
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
	if err := c.commit(tmp, final); err != nil {
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
//     the rename retried. This is the only place the cache deletes anything,
//     and — because the check that calls it incomplete is made under the lock
//     described below and nothing else may publish while that lock is held —
//     it deletes only something no reader was ever allowed to use. Between the
//     removal and the retry a concurrent Has reports a miss for a moment; the
//     worst that costs is one redundant fetch, whereas the alternative —
//     leaving the directory alone — wedges the digest permanently.
//
// # Why the whole of it runs under a lock file
//
// That second case is a check followed by a removal followed by a rename, and
// between the check and the removal another writer can publish. Without a lock
// two processes both see "incomplete", the first publishes and returns final
// to its caller as usable, and the second — still acting on an observation
// that is now stale — deletes the package the first one just handed out. The
// bytes are the same either way, so nothing is corrupted, but a caller that
// opens a file in that window finds it gone, and if the second rename then
// fails the cache is left with no entry at a path a caller was told is usable.
// A mutex cannot prevent that: the two writers are in different processes.
//
// A lock file can, so commit takes one — "<64 hex>.lock", beside the digest
// directory — and holds it across the whole of the check, the removal and the
// rename, never across the unpack. The completeness check therefore happens
// *while the lock is held*, which is what turns check-then-act into
// check-under-lock: a writer that waited for the lock re-reads the entry after
// acquiring it, sees the package the previous holder published, and returns it
// instead of deleting it. Under that rule the promise above is exact — a
// complete entry is never removed by anything here.
//
// Any other rename failure is reported, naming both attempts. So is a lock
// that cannot be taken: see lockDigestDir for the bounded wait and for what a
// leftover lock file costs.
func (c *Cache) commit(tmp, final string) (err error) {
	unlock, lockErr := c.lockDigestDir(final)
	if lockErr != nil {
		return fmt.Errorf("move %s into place: %w", tmp, lockErr)
	}
	defer func() {
		// The lock is released on every path, success or failure, and a
		// release that itself fails is joined into the result rather than
		// dropped: a lock file nothing can remove blocks every later
		// writer of this digest, which is exactly the kind of condition
		// that must not be discovered silently.
		if unlockErr := unlock(); unlockErr != nil {
			err = errors.Join(err, unlockErr)
		}
	}()

	firstErr := os.Rename(tmp, final)
	if firstErr == nil {
		return nil
	}

	// Read the entry here, under the lock, not before taking it: whatever
	// this sees cannot change until the lock is released, so the decision
	// made from it is still true when it is acted on below.
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

// lockDigestDir takes the lock guarding one digest directory and returns the
// function that releases it. The lock is a file named after the directory it
// guards ("<64 hex>.lock", which is not 64 hex digits and so can never be read
// as a digest directory) created with O_CREATE|O_EXCL: that combination is the
// one filesystem operation both POSIX and Windows make atomic against other
// processes, which is what makes this work where a mutex cannot.
//
// The wait is bounded by c.lockWait. A process killed while holding the lock
// leaves the file behind, and rather than block forever on it, a writer that
// has waited that long gives up and returns an error naming the file and
// saying how to clear it: an operator deletes exactly that file, and the next
// Put proceeds. Nothing here ages a lock out on its own, because "old" and
// "abandoned" are not the same thing — a heuristic that broke a live holder's
// lock would reintroduce the very race the lock exists to prevent. That is the
// loud failure: nothing is guessed, nothing proceeds unserialized, and the
// entry the lock guards stays readable throughout, because a stale lock blocks
// only writers.
func (c *Cache) lockDigestDir(dir string) (unlock func() error, err error) {
	if c.lockWait <= 0 || c.lockPoll <= 0 {
		panic(fmt.Sprintf("fetch: cache lock bounds not configured (wait %v, poll %v): Cache must come from NewCache", c.lockWait, c.lockPoll))
	}
	lockPath := dir + digestLockSuffix
	release := func() error {
		if err := os.Remove(lockPath); err != nil {
			return fmt.Errorf("release cache lock %s: %w", lockPath, err)
		}
		return nil
	}

	deadline := time.Now().Add(c.lockWait)
	for {
		f, openErr := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if openErr == nil {
			// The file's existence is the lock, not the open handle, so
			// it is closed immediately; a close that fails still leaves
			// the lock held, so it is reported together with the release
			// rather than swallowed.
			if closeErr := f.Close(); closeErr != nil {
				return nil, errors.Join(
					fmt.Errorf("acquire cache lock %s: close: %w", lockPath, closeErr),
					release(),
				)
			}
			return release, nil
		}
		if !errors.Is(openErr, fs.ErrExist) {
			return nil, fmt.Errorf("acquire cache lock %s: %w", lockPath, openErr)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf(
				"acquire cache lock %s: another writer has held it for more than %s; if no other process is installing plugins, that file is a leftover from one that was killed and clearing it means deleting exactly that file",
				lockPath, c.lockWait)
		}
		time.Sleep(c.lockPoll)
	}
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

// CacheEntry is one directory in the cache, as List reports it.
//
// Complete distinguishes a package that can actually be used from a leftover:
// a directory holding only some of the three package files is what an
// interrupted unpack or a partial deletion leaves behind. Has() never counts
// one as a hit, so it is invisible to everything except a listing — and it
// still occupies disk, which is precisely why it is reported rather than
// filtered out.
//
// ModTime is the entry directory's modification time: when the package was
// written or last replaced. It is NOT a last-used time — nothing in this
// repository records reads, because doing so would mean a disk write on every
// cache hit — so any policy built on it must say "least recently written",
// not "least recently used".
type CacheEntry struct {
	Digest   string
	Bytes    int64
	ModTime  time.Time
	Complete bool
}

// List reports every entry in the cache, complete or not.
//
// It skips the bookkeeping this package keeps alongside entries — the
// ".unpack-*" staging directories and the "<hex>.lock" files — and anything
// else whose name is not a digest: a listing exists to answer "which packages
// are on disk", and reporting a lock file as a package would send an operator
// looking for a plugin that does not exist.
//
// An empty cache lists nothing and is not an error: that is what every
// deployment looks like before its first remote install.
func (c *Cache) List() ([]CacheEntry, error) {
	shard := filepath.Join(c.root, digestAlgorithmDirName)
	dirEntries, err := os.ReadDir(shard)
	if errors.Is(err, fs.ErrNotExist) {
		// The shard directory is created by the first Put. Its absence is an
		// empty cache, not a broken one.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cache directory %s: %w", shard, err)
	}

	entries := make([]CacheEntry, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		if !dirEntry.IsDir() || !isHexDigest(dirEntry.Name()) {
			continue
		}
		dir := filepath.Join(shard, dirEntry.Name())
		info, err := dirEntry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat cache entry %s: %w", dir, err)
		}
		size, err := directorySize(dir)
		if err != nil {
			return nil, err
		}
		complete, err := isCompletePackage(dir)
		if err != nil {
			return nil, fmt.Errorf("inspect cache entry %s: %w", dir, err)
		}
		entries = append(entries, CacheEntry{
			Digest:   digestAlgorithmDirName + ":" + dirEntry.Name(),
			Bytes:    size,
			ModTime:  info.ModTime(),
			Complete: complete,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Digest < entries[j].Digest })
	return entries, nil
}

// Remove deletes one entry, reporting whether there was anything to delete.
//
// A digest that is not cached is not an error — removal is idempotent, and a
// caller pruning a list it read a moment ago races with anyone else doing the
// same — but it answers false rather than claiming a deletion that never
// happened.
//
// It takes the same cross-process digest lock commit does, because the entry
// it is about to delete may be the one another process is writing right now;
// deleting a directory mid-unpack would leave that writer committing into
// nothing.
//
// This is the only function in this package that calls os.RemoveAll, so the
// digest it is handed is PARSED before any path is built from it: a caller
// that passes an unvalidated string gets a refusal, not a deletion somewhere
// outside the cache.
func (c *Cache) Remove(digest string) (removed bool, err error) {
	hexDigits, err := parseDigest(digest)
	if err != nil {
		return false, fmt.Errorf("cache remove: %w", err)
	}
	dir := c.dirForHex(hexDigits)

	// Look before locking. The lock file lives beside the entry, in a shard
	// directory the first Put creates — so on a cache that has never stored
	// anything, taking the lock would fail with "path not found" for a digest
	// that plainly is not there. Answering false first is both correct and
	// cheaper.
	//
	// The check is not a race hazard: if a Put commits between here and the
	// lock, this call simply reports "there was nothing", which is what was
	// true when it looked. What must NOT happen — deleting a directory while
	// somebody unpacks into it — is still prevented, because that case has an
	// entry present and therefore goes through the lock below.
	if _, statErr := os.Stat(dir); errors.Is(statErr, fs.ErrNotExist) {
		return false, nil
	} else if statErr != nil {
		return false, fmt.Errorf("cache remove %s: stat %s: %w", digest, dir, statErr)
	}

	unlock, err := c.lockDigestDir(dir)
	if err != nil {
		return false, fmt.Errorf("cache remove %s: %w", digest, err)
	}
	defer func() {
		if uerr := unlock(); uerr != nil {
			err = errors.Join(err, fmt.Errorf("cache remove %s: release lock: %w", digest, uerr))
		}
	}()

	// Re-check under the lock: between the look above and here, another
	// process holding this lock may have removed the entry itself.
	if _, statErr := os.Stat(dir); errors.Is(statErr, fs.ErrNotExist) {
		return false, nil
	} else if statErr != nil {
		return false, fmt.Errorf("cache remove %s: stat %s: %w", digest, dir, statErr)
	}
	if rmErr := os.RemoveAll(dir); rmErr != nil {
		return false, fmt.Errorf("cache remove %s: %w", digest, rmErr)
	}
	return true, nil
}

// directorySize sums the sizes of every regular file under dir.
//
// Sizes, not disk usage: block rounding is filesystem-specific and the number
// exists to answer "how much would pruning this reclaim", which the sum
// answers closely enough to act on.
func directorySize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure cache entry %s: %w", dir, err)
	}
	return total, nil
}
