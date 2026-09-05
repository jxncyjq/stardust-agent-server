//go:build !windows

package trustlist

import (
	"os"
	"syscall"
	"testing"
)

// TestLockCreateIsContended_LeavesPOSIXPermissionErrorsFatal 钉住平台判据的另一
// 边：POSIX 上 EACCES 说的就是这个进程不许建这个文件，先等满整个锁等待时限再讲，
// 会把一个权限问题变成一次挂起。
func TestLockCreateIsContended_LeavesPOSIXPermissionErrorsFatal(t *testing.T) {
	t.Parallel()

	for _, err := range []error{
		&os.PathError{Op: "open", Path: "x.lock", Err: syscall.EACCES},
		&os.PathError{Op: "open", Path: "x.lock", Err: syscall.EPERM},
		&os.PathError{Op: "open", Path: "x.lock", Err: syscall.ENOENT},
	} {
		if lockCreateIsContended(err) {
			t.Errorf("%v 在这个平台上不是争用，必须立刻报出来", err)
		}
	}
}
