//go:build !windows

package taskledger

// lockNameIsInTransition reports no transient case on POSIX systems.
//
// There a name is unlinked the moment it is removed, so there is no
// delete-pending window for a create to land in: O_CREAT|O_EXCL answers "the
// file is already there" with EEXIST and nothing else. EACCES here means what
// it says — this process may not create that file — and retrying it would only
// delay the report.
func lockNameIsInTransition(error) bool { return false }
