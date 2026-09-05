//go:build windows

package trustlist

import (
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// TestLockCreateIsContended_TreatsWindowsAccessDeniedAsContention 钉住判据里属于
// 平台的那一半：ERROR_ACCESS_DENIED 正是一次 create 撞上锁文件名 delete-pending
// 时看到的东西——另一个写者在放手——所以必须等待，而不是把一次刷新报成失败。
func TestLockCreateIsContended_TreatsWindowsAccessDeniedAsContention(t *testing.T) {
	t.Parallel()

	err := &os.PathError{Op: "open", Path: "x.lock", Err: syscall.ERROR_ACCESS_DENIED}
	if !lockCreateIsContended(err) {
		t.Error("锁 create 上的 ERROR_ACCESS_DENIED 是 delete-pending 窗口，不是致命错误")
	}
}

// TestLockCreateIsContended_LeavesOtherWindowsErrorsFatal 把这次放宽限定在恰好
// 一个 errno 上：其余的仍须立刻失败，而不是先白等满整个等待时限。
func TestLockCreateIsContended_LeavesOtherWindowsErrorsFatal(t *testing.T) {
	t.Parallel()

	for _, err := range []error{
		&os.PathError{Op: "open", Path: "x.lock", Err: syscall.ERROR_PATH_NOT_FOUND},
		&os.PathError{Op: "open", Path: "x.lock", Err: syscall.ERROR_FILE_NOT_FOUND},
		&os.PathError{Op: "open", Path: "x.lock", Err: fs.ErrInvalid},
	} {
		if lockCreateIsContended(err) {
			t.Errorf("%v 不是争用，必须立刻报出来", err)
		}
	}
}
