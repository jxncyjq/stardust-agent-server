//go:build windows

package cli

import "syscall"

// windowsWSAENOBUFS is Winsock's WSAENOBUFS (10055): the code a bind reports
// when the machine cannot hand out a socket, which on Windows is what an
// exhausted — or reserved-away — dynamic port range looks like.
//
// The number is written out because the standard syscall package does not
// export it (its Windows errno block carries the POSIX-shaped ENOBUFS, whose
// value is NOT 10055); golang.org/x/sys/windows does, and this repository does
// not depend on it for one constant.
const windowsWSAENOBUFS syscall.Errno = 10055
