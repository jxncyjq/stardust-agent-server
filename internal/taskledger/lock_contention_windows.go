//go:build windows

package taskledger

import (
	"errors"
	"syscall"
)

// lockNameIsInTransition reports whether a failed operation on the lock file's
// NAME (the create, or the stat that follows a "held" answer) failed because
// another writer is mid-operation on that same name.
//
// On Windows, removing a file does not necessarily free its name at once:
// while any handle to it is still open the name is delete-pending, and
// CreateFile against a delete-pending name fails with ERROR_ACCESS_DENIED
// rather than "already exists". A writer releasing the lock is therefore a
// window in which another writer's create sees errno 5 for a lock that is
// merely being handed over.
//
// The same window is why the stat needs this too: acquireLock stats the lock
// after an ErrExist to judge staleness, and a stat of a delete-pending name is
// refused the same way. That stat failure means the lock is being released —
// the same situation the ErrNotExist branch already treats as "retry" — and
// reporting it instead fails a ledger write for a lock that no longer exists.
//
// ErrExist is NOT included here: that answer means the lock is held, which is
// this lock's ordinary contended case and is answered by the caller (stat the
// file, judge staleness, report or reclaim) rather than by retrying.
func lockNameIsInTransition(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.ERROR_ACCESS_DENIED
}
