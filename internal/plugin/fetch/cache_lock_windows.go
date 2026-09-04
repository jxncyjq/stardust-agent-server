//go:build windows

package fetch

import (
	"errors"
	"syscall"
)

// lockCreateIsContendedOnThisPlatform adds the Windows spelling of "another
// writer holds this lock".
//
// Deleting a file on Windows does not necessarily unlink its name at once:
// while any handle to it is still open the name sits in a delete-pending
// state, and CreateFile against a delete-pending name fails with
// ERROR_ACCESS_DENIED — NOT with "already exists". A writer releasing the lock
// (os.Remove) is therefore a window in which another writer's create sees
// errno 5 for a file that is, logically, simply held.
//
// This is measured, not assumed: a probe hammering create/release on one path
// from 12 goroutines saw 611 ERROR_ACCESS_DENIED against 28160 ErrExist —
// about 2% of contended creates. Reading that as fatal is what made
// TestCache_ConcurrentPut_OverAnIncompleteEntry_NeverDeletesAPublishedPackage
// fail at random on Windows.
//
// The cost of folding errno 5 into the wait: a lock file that really is
// unopenable (an ACL that denies this process, a read-only location) now
// costs the full c.lockWait before it is reported instead of failing at once.
// That is why the timeout message names the last error it saw — a permission
// problem still surfaces, with the errno in hand, rather than being reported
// as a phantom lock holder.
func lockCreateIsContendedOnThisPlatform(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.ERROR_ACCESS_DENIED
}
