//go:build !windows

package fetch

import (
	"os"
	"syscall"
	"testing"
)

// TestLockCreateIsContended_LeavesPOSIXPermissionErrorsFatal pins the other
// side of the platform split: EACCES on POSIX means this process may not
// create the file, and waiting the full lock timeout before saying so would
// turn a permission problem into a hang.
func TestLockCreateIsContended_LeavesPOSIXPermissionErrorsFatal(t *testing.T) {
	for _, err := range []error{
		&os.PathError{Op: "open", Path: "x.lock", Err: syscall.EACCES},
		&os.PathError{Op: "open", Path: "x.lock", Err: syscall.EPERM},
		&os.PathError{Op: "open", Path: "x.lock", Err: syscall.ENOENT},
	} {
		if lockCreateIsContended(err) {
			t.Errorf("%v is not contention on this platform and must be reported immediately", err)
		}
	}
}
