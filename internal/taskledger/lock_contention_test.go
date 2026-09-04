package taskledger

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestAcquireLock_UnderContention_NeverReportsARawPlatformErrno pins what a
// contended acquisition may and may not fail with.
//
// Failing IS allowed here: this lock does not wait — a writer that finds a
// fresh lock reports "held by another writer" and the caller decides. What is
// not allowed is failing with a raw platform errno, because that is the shape
// the Windows delete-pending window produces: while a releasing writer's name
// is still pending deletion, CreateFile answers ERROR_ACCESS_DENIED instead of
// "already exists", and acquireLock's "anything that is not ErrExist is fatal"
// rule turns an ordinary contended release into a hard ledger failure.
//
// Measured in internal/plugin/fetch, which had the same defect: about 2% of
// contended creates. So this hammers the lock from several Ledgers over one
// root (l.mu only serializes one Ledger) and fails on the first raw errno.
func TestAcquireLock_UnderContention_NeverReportsARawPlatformErrno(t *testing.T) {
	if testing.Short() {
		t.Skip("contention hammer: thousands of lock cycles, skipped under -short")
	}
	root := t.TempDir()

	const ledgers = 8
	const cycles = 300
	var wg sync.WaitGroup
	bad := make(chan error, ledgers*cycles)
	deadline := time.Now().Add(60 * time.Second)
	for i := 0; i < ledgers; i++ {
		l, err := New(Config{WorkspaceRoot: root})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		wg.Add(1)
		go func(l *Ledger) {
			defer wg.Done()
			for c := 0; c < cycles && time.Now().Before(deadline); c++ {
				unlock, err := l.acquireLock(context.Background())
				if err != nil {
					var errno syscall.Errno
					if errors.As(err, &errno) {
						bad <- err
						return
					}
					// "held by another writer" is this lock's contract.
					continue
				}
				if err := unlock(); err != nil {
					bad <- err
					return
				}
			}
		}(l)
	}
	wg.Wait()
	close(bad)
	for err := range bad {
		t.Fatalf("a contended acquisition failed with a raw platform errno instead of the lock's own contention answer: %v", err)
	}
}
