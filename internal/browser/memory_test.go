package browser

import (
	"errors"
	"os"
	"testing"
)

// 内存采样此前三个平台一律返回 0。0 是一个**看起来正常的数字**：健康检查读到它会
// 认为浏览器没占内存，于是「内存水位超阈就回收」这条策略永远不触发——一个失控的
// 页面可以一直涨到把机器拖垮，而监控上是一条平的 0。
//
// 这也是为什么这两个函数的契约是「取不到就报错」而不是「取不到返回 0」。

func TestSamplingThisProcessReportsRealUsage(t *testing.T) {
	t.Parallel()

	pal := NewPlatformAdapter()
	used, err := pal.SampleProcessMemory(os.Getpid())
	if err != nil {
		t.Fatalf("SampleProcessMemory(self): %v", err)
	}
	// 一个跑着 Go 运行时的进程不可能只占几 KB。下界取 1 MiB：足以排除「返回 0」
	// 与「读到了错误的字段」，又不会因为机器差异而抖。
	if used < 1<<20 {
		t.Errorf("SampleProcessMemory(self) = %d bytes, want at least 1MiB: this looks like a placeholder", used)
	}
}

// TestSamplingAProcessThatIsNotThereFailsLoudly: 0 与「这个进程不存在」必须区分。
// 把后者答成 0，健康检查会把一个已经死掉的浏览器读成「内存占用健康」。
func TestSamplingAProcessThatIsNotThereFailsLoudly(t *testing.T) {
	t.Parallel()

	// 一个几乎不可能存在的 pid。就算它碰巧存在，这条断言也只是变宽松而不会误报。
	if _, err := NewPlatformAdapter().SampleProcessMemory(0x7FFFFFF0); err == nil {
		t.Error("sampling a non-existent process returned no error; a dead browser would read as healthy")
	}
}

func TestAvailableSystemMemoryIsAReadableNumber(t *testing.T) {
	t.Parallel()

	available, err := NewPlatformAdapter().AvailableSystemMemory()
	if err != nil {
		t.Fatalf("AvailableSystemMemory: %v", err)
	}
	if available < 1<<20 {
		t.Errorf("AvailableSystemMemory = %d bytes, want a real reading", available)
	}
}

// 内存采样只有被用来做决定才算数：一台已经没有内存的机器上再开一个浏览器会话，
// 换来的是整机开始换页，而那台机器上跑的**别的**东西一起被拖垮。
//
// 这条策略只挡**新建**：已经开着的会话继续可用（把用户正在看的页面掐掉，比多占
// 一点内存更糟），复用同一个 chat session 的也照旧。
func TestANewSessionIsRefusedWhenTheMachineIsOutOfMemory(t *testing.T) {
	t.Parallel()

	rt := &Runtime{
		sessions: NewSessionStore(),
		hubs:     newHubRegistry(),
		cfg:      RuntimeConfig{MinFreeMemoryBytes: 8 << 30},
	}
	rt.availableMemory = func() (uint64, error) { return 1 << 30, nil } // 1GiB 可用，低于门槛

	err := rt.admitNewSession()
	if err == nil {
		t.Fatal("admitNewSession succeeded with 1GiB free against an 8GiB floor")
	}
	var be *BrowserError
	if !asBrowserError(err, &be) || be.Code != CodeResourceExhausted {
		t.Errorf("error = %v, want %s so the agent can tell this from a broken page", err, CodeResourceExhausted)
	}
}

func TestAMemoryFloorOfZeroAdmitsEverything(t *testing.T) {
	t.Parallel()

	rt := &Runtime{sessions: NewSessionStore(), hubs: newHubRegistry()}
	rt.availableMemory = func() (uint64, error) { return 1, nil }

	if err := rt.admitNewSession(); err != nil {
		t.Errorf("admitNewSession = %v, want nil when no floor is configured", err)
	}
}

// TestAnUnreadableMemoryReadingDoesNotBlockWork: refusing to browse because the
// platform could not answer would turn a diagnostic gap into an outage. It is
// logged instead — the floor is a safety margin, not an authorization check.
func TestAnUnreadableMemoryReadingDoesNotBlockWork(t *testing.T) {
	t.Parallel()

	rt := &Runtime{
		sessions: NewSessionStore(),
		hubs:     newHubRegistry(),
		cfg:      RuntimeConfig{MinFreeMemoryBytes: 8 << 30},
	}
	rt.availableMemory = func() (uint64, error) { return 0, errors.New("no reading available") }

	if err := rt.admitNewSession(); err != nil {
		t.Errorf("admitNewSession = %v, want nil: an unreadable gauge must not become an outage", err)
	}
}
