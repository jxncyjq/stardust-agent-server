//go:build windows

package fetch

import (
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// TestLockCreateIsContended_TreatsWindowsAccessDeniedAsContention pins the
// platform half of the split. ERROR_ACCESS_DENIED is what a create sees when
// the lock file's name is delete-pending — another writer is releasing it —
// so it must be waited out rather than reported as a failed install.
func TestLockCreateIsContended_TreatsWindowsAccessDeniedAsContention(t *testing.T) {
	err := &os.PathError{Op: "open", Path: "x.lock", Err: syscall.ERROR_ACCESS_DENIED}
	if !lockCreateIsContended(err) {
		t.Error("ERROR_ACCESS_DENIED on a lock create is the delete-pending window, not a fatal condition")
	}
}

// TestLockCreateIsContended_LeavesOtherWindowsErrorsFatal keeps the widening
// to exactly one errno: anything else must still fail at once instead of
// spending the whole lock wait first.
func TestLockCreateIsContended_LeavesOtherWindowsErrorsFatal(t *testing.T) {
	for _, err := range []error{
		&os.PathError{Op: "open", Path: "x.lock", Err: syscall.ERROR_PATH_NOT_FOUND},
		&os.PathError{Op: "open", Path: "x.lock", Err: syscall.ERROR_FILE_NOT_FOUND},
		&os.PathError{Op: "open", Path: "x.lock", Err: fs.ErrInvalid},
	} {
		if lockCreateIsContended(err) {
			t.Errorf("%v is not contention and must be reported immediately", err)
		}
	}
}
