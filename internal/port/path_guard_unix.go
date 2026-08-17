//go:build !windows

package port

// checkPlatform has nothing to add outside Windows: on POSIX filesystems a
// colon and the names CON/NUL/COM1 are ordinary filename characters and
// ordinary filenames, so rejecting them would refuse legitimate files rather
// than close a hole.
func checkPlatform(string) error { return nil }
