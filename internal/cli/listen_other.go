//go:build !windows

package cli

import "syscall"

// windowsWSAENOBUFS has no meaning off Windows. It is aliased to ENOBUFS so
// the one comparison in isBufferExhaustion stays a plain expression instead of
// growing a build tag of its own; the check is then simply redundant here,
// which is cheaper than two copies of the function.
const windowsWSAENOBUFS = syscall.ENOBUFS
