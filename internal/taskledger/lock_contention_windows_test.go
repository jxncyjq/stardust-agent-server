//go:build windows

package taskledger

import (
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// TestLockCreateIsTransientlyContended_WindowsAccessDenied pins the platform
// half: ERROR_ACCESS_DENIED is the delete-pending window — another writer
// releasing the name — and must be retried rather than reported as a failure
// to create the lock.
func TestLockCreateIsTransientlyContended_WindowsAccessDenied(t *testing.T) {
	err := &os.PathError{Op: "open", Path: ".lock", Err: syscall.ERROR_ACCESS_DENIED}
	if !lockNameIsInTransition(err) {
		t.Error("ERROR_ACCESS_DENIED on a lock create is a release window, not a fatal condition")
	}
}

// TestLockCreateIsTransientlyContended_ExcludesErrExist is the assertion that
// keeps the fix from eating the feature. ErrExist means the lock is HELD, and
// acquireLock answers that by stat-ing the file and judging staleness — the
// only path that can ever reclaim an abandoned lock. Folding ErrExist into the
// retry would spend the budget and then report, so a lock left behind by a
// killed process would never be reclaimed again.
func TestLockCreateIsTransientlyContended_ExcludesErrExist(t *testing.T) {
	for _, err := range []error{
		fs.ErrExist,
		&os.PathError{Op: "open", Path: ".lock", Err: fs.ErrExist},
		&os.PathError{Op: "open", Path: ".lock", Err: syscall.ERROR_FILE_NOT_FOUND},
		&os.PathError{Op: "open", Path: ".lock", Err: fs.ErrInvalid},
	} {
		if lockNameIsInTransition(err) {
			t.Errorf("%v must not be retried as a release window", err)
		}
	}
}
