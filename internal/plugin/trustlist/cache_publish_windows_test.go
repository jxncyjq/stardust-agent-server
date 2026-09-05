//go:build windows

package trustlist

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRenameIsContendedOnThisPlatform_TreatsAccessDeniedAsContention 钉住判据里
// 属于平台的那一半：ERROR_ACCESS_DENIED 正是一次覆盖式改名撞上目标名上一个打开的
// 句柄时看到的东西——有人正在读——所以必须等待，而不是把一次刷新报成失败。
func TestRenameIsContendedOnThisPlatform_TreatsAccessDeniedAsContention(t *testing.T) {
	t.Parallel()

	err := &os.LinkError{Op: "rename", Old: "a.tmp", New: "a", Err: syscall.ERROR_ACCESS_DENIED}
	if !renameIsContendedOnThisPlatform(err) {
		t.Error("rename 上的 ERROR_ACCESS_DENIED 是目标名被开着，不是致命错误")
	}
}

// TestRenameIsContendedOnThisPlatform_LeavesOtherErrorsFatal 把这次放宽限定在恰好
// 一个 errno 上：其余的仍须立刻失败，而不是先白等满整个等待时限。
func TestRenameIsContendedOnThisPlatform_LeavesOtherErrorsFatal(t *testing.T) {
	t.Parallel()

	for _, err := range []error{
		&os.LinkError{Op: "rename", Old: "a.tmp", New: "a", Err: syscall.ERROR_PATH_NOT_FOUND},
		&os.LinkError{Op: "rename", Old: "a.tmp", New: "a", Err: syscall.ERROR_FILE_NOT_FOUND},
		&os.LinkError{Op: "rename", Old: "a.tmp", New: "a", Err: fs.ErrInvalid},
	} {
		if renameIsContendedOnThisPlatform(err) {
			t.Errorf("%v 不是争用，必须立刻报出来", err)
		}
	}
}

// TestPublishRenameWaitsOutAnOpenReader 是这个平台上那个窗口的确定性版本：目标名
// 上开着一个纯读的句柄，rename 必须先失败、等着，等句柄关掉之后成功。
//
// 并发用例只能概率性地撞上它；这一个把窗口写死了。
func TestPublishRenameWaitsOutAnOpenReader(t *testing.T) {
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
	// 先确认这个平台确实拦下了它——否则这个用例什么也没测。
	if err := os.Rename(from, to); err == nil {
		t.Fatal("目标名开着句柄，os.Rename 却成功了：这个用例守的窗口不存在了")
	} else if !renameIsContendedOnThisPlatform(err) {
		t.Fatalf("目标名开着句柄时 rename 的失败没有被判成争用：%v", err)
	}

	closed := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = reader.Close()
		close(closed)
	}()
	if err := publishRenameWithin(from, to, 30*time.Second, publishPoll); err != nil {
		t.Fatalf("读者放手之后 rename 仍然失败：%v", err)
	}
	<-closed
	got, err := os.ReadFile(to)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("发布之后 %s 的内容是 %q，want %q", to, got, "new")
	}
}

// TestPublishRenameGivesUpAndNamesTheLastFailure：折叠 errno 5 的代价是一个真正
// 不许写的位置也要等满时限才被报出来，所以那个错误必须**说得清**。
//
// 这里用一个永远不放手的读者当替身：判据分不开它与一份没有写权限的目标，那正是
// 这条规则存在的原因。错误必须保住原始的 errno（调用方还能 errors.Is 到它），
// 并说明它等了多久——「等了这么久还是 errno 5」是操作者据以怀疑权限而不是怀疑
// 幽灵读者的唯一线索。
func TestPublishRenameGivesUpAndNamesTheLastFailure(t *testing.T) {
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

	const wait = 30 * time.Millisecond
	start := time.Now()
	err = publishRenameWithin(from, to, wait, time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("读者一直开着，rename 却成功了")
	}
	if !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		t.Errorf("错误链里丢了原始的 errno，权限问题就无从辨认：%v", err)
	}
	if !strings.Contains(err.Error(), "retried for") {
		t.Errorf("错误没说它等过，读的人无从判断这是争用还是权限：%v", err)
	}
	if elapsed < wait {
		t.Errorf("只等了 %v 就放弃了，want >= %v", elapsed, wait)
	}
}
