package fetch

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestLockCreateIsContended_ClassifiesTheCreateErrors pins the one decision
// lockDigestDir makes about a failed create: "another writer holds it, wait"
// versus "this will never work, report it". Getting that split wrong is not a
// cosmetic mistake — a contended create misread as fatal fails a plugin
// install that had nothing wrong with it.
func TestLockCreateIsContended_ClassifiesTheCreateErrors(t *testing.T) {
	if !lockCreateIsContended(fs.ErrExist) {
		t.Error("an existing lock file is the ordinary contended case and must be waited out")
	}
	if !lockCreateIsContended(&os.PathError{Op: "open", Path: "x.lock", Err: fs.ErrExist}) {
		t.Error("ErrExist wrapped in *os.PathError — the shape os.OpenFile actually returns — must be waited out")
	}
	if lockCreateIsContended(fs.ErrNotExist) {
		t.Error("a missing-parent error is not contention: waiting cannot fix it")
	}
	if lockCreateIsContended(fs.ErrInvalid) {
		t.Error("an unrelated error must not be absorbed into the wait loop")
	}
}

// TestLockDigestDir_UnderContention_NeverFailsSpuriously is the regression
// that started this: on Windows a create racing another goroutine's delete of
// the same lock file returns ERROR_ACCESS_DENIED (errno 5) rather than
// ErrExist, because the name is briefly delete-pending. Measured on this
// machine that is ~2% of contended creates — enough to fail the concurrent Put
// test at random and nowhere near rare enough to dismiss.
//
// It hammers the real lock the way commit uses it (take, release, repeat) from
// several goroutines. A single spurious failure fails the test: under
// contention there is no correct outcome other than acquiring the lock or
// waiting for it.
func TestLockDigestDir_UnderContention_NeverFailsSpuriously(t *testing.T) {
	if testing.Short() {
		t.Skip("contention hammer: hundreds of lock cycles, skipped under -short")
	}
	c, root := newTestCache(t)
	dir := filepath.Join(root, digestAlgorithmDirName, "42a362e28ad56ec9fa68cf4489084b8380e5b3ed142b53d35c371db7681d026a")
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatalf("create shard directory: %v", err)
	}

	const goroutines = 8
	const cycles = 250
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*cycles)
	deadline := time.Now().Add(60 * time.Second)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < cycles && time.Now().Before(deadline); i++ {
				unlock, err := c.lockDigestDir(dir)
				if err != nil {
					errs <- err
					return
				}
				if err := unlock(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a contended lock acquisition failed instead of waiting: %v", err)
	}
}
