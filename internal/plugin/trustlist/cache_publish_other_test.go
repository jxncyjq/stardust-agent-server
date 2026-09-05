//go:build !windows

package trustlist

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRenameIsContendedOnThisPlatform_LeavesPOSIXErrorsFatal 钉住平台判据的另一
// 边：POSIX 的 rename(2) 不看目标名有没有打开的句柄，没有可等的争用，所以这里
// 一律不等。EACCES 说的就是这个进程不许改名，先等满时限再讲会把一个权限问题变成
// 一次挂起。
func TestRenameIsContendedOnThisPlatform_LeavesPOSIXErrorsFatal(t *testing.T) {
	t.Parallel()

	for _, err := range []error{
		&os.LinkError{Op: "rename", Old: "a.tmp", New: "a", Err: syscall.EACCES},
		&os.LinkError{Op: "rename", Old: "a.tmp", New: "a", Err: syscall.EPERM},
		&os.LinkError{Op: "rename", Old: "a.tmp", New: "a", Err: syscall.EBUSY},
		&os.LinkError{Op: "rename", Old: "a.tmp", New: "a", Err: syscall.ENOENT},
	} {
		if renameIsContendedOnThisPlatform(err) {
			t.Errorf("%v 在这个平台上不是争用，必须立刻报出来", err)
		}
	}
}

// TestPublishRenameOverAnOpenReader 记下这个平台与 Windows 的分野：目标名上开着
// 一个读句柄，rename 照样成功，读者继续读它自己那一份。这正是重试循环在这里只跑
// 一趟的原因。
func TestPublishRenameOverAnOpenReader(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	to := filepath.Join(dir, "trustlist.sig")
	if err := os.WriteFile(to, []byte("old"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	from := filepath.Join(dir, "trustlist.sig.tmp-1")
	if err := os.WriteFile(from, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	reader, err := os.Open(to)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = reader.Close() }()

	start := time.Now()
	if err := publishRenameWithin(from, to, 30*time.Second, publishPoll); err != nil {
		t.Fatalf("目标名开着读句柄时 rename 失败了：%v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("发布等了 %v：这个平台上不该有可等的争用", elapsed)
	}
	got, err := os.ReadFile(to)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("发布之后 %s 的内容是 %q，want %q", to, got, "new")
	}
}
