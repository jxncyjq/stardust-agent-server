//go:build !windows

package taskledger

import (
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// TestLockCreateIsTransientlyContended_NothingIsTransientOnPOSIX pins the
// other side of the split: a name is unlinked the moment it is removed, so
// there is no release window to retry, and EACCES means what it says.
func TestLockCreateIsTransientlyContended_NothingIsTransientOnPOSIX(t *testing.T) {
	for _, err := range []error{
		fs.ErrExist,
		&os.PathError{Op: "open", Path: ".lock", Err: fs.ErrExist},
		&os.PathError{Op: "open", Path: ".lock", Err: syscall.EACCES},
		&os.PathError{Op: "open", Path: ".lock", Err: syscall.EPERM},
		&os.PathError{Op: "open", Path: ".lock", Err: syscall.ENOENT},
	} {
		if lockNameIsInTransition(err) {
			t.Errorf("%v must be reported immediately on this platform", err)
		}
	}
}
