package taskledger

import (
	"fmt"
	"os"
	"time"
)

// lockCreateContentionBudget bounds how long createLockFile keeps retrying a
// create that failed for a reason meaning "another writer is touching this
// name right now" (see lockNameIsInTransition). It is deliberately
// short: the condition it covers is a releasing writer's unlink, which clears
// in well under a millisecond, NOT a writer holding the lock across its work.
// Holding is answered by this lock's own contract — report it and let the
// caller decide — and stretching this budget would quietly turn that
// no-waiting contract into a wait.
const lockCreateContentionBudget = 250 * time.Millisecond

// lockCreateContentionPoll is how often createLockFile retries inside that
// budget.
const lockCreateContentionPoll = 2 * time.Millisecond

// createLockFile creates the ledger lock file with O_CREATE|O_EXCL, retrying
// only while the failure means another writer is mid-operation on the same
// name.
//
// It exists because "the create failed" is not one condition. Three are mixed
// together at the syscall: the lock is HELD (answered with ErrExist, which
// this function returns unchanged for the caller's staleness logic), the lock
// is being RELEASED right now (a platform-specific error — on Windows the name
// is briefly delete-pending and the create is refused with
// ERROR_ACCESS_DENIED, not "already exists"), and the lock CANNOT be created
// at all (permissions, a read-only location). Reporting the second as the
// third is what makes a concurrent ledger write fail at random on Windows;
// internal/plugin/fetch had the identical defect, measured at about 2% of
// contended creates.
//
// The retry deliberately does not reach the caller's staleness path. A create
// refused because a name is delete-pending says nothing about how old the lock
// is or whether its holder is alive, so it must never be fed to the mtime
// check that decides whether to RECLAIM a lock — reclaiming on that evidence
// would delete a live writer's lock.
func createLockFile(lockPath string) (*os.File, error) {
	deadline := time.Now().Add(lockCreateContentionBudget)
	for {
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return file, nil
		}
		if !lockNameIsInTransition(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			// Still refused after the whole budget. Report it with the
			// original error in hand rather than as a phantom holder: past
			// this point the likely cause is no longer a release window but
			// the location's permissions, and an operator needs to see which.
			return nil, fmt.Errorf(
				"lock file still could not be created after retrying for %s; if this is not another writer releasing it, the location's permissions are what to check: %w",
				lockCreateContentionBudget, err)
		}
		time.Sleep(lockCreateContentionPoll)
	}
}
