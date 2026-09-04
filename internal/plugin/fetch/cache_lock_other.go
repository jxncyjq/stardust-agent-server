//go:build !windows

package fetch

// lockCreateIsContendedOnThisPlatform reports no additional contended case.
//
// On POSIX systems O_CREAT|O_EXCL answers "the file is already there" with
// EEXIST and nothing else; a name is unlinked the moment it is removed, so
// there is no delete-pending window for a create to land in. EACCES here means
// what it says — this process may not create that file — and waiting on it
// would turn a permission problem into a 30-second hang before reporting the
// same thing.
func lockCreateIsContendedOnThisPlatform(error) bool { return false }
