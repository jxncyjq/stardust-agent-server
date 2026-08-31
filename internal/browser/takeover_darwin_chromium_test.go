//go:build darwin && chromium

package browser

import (
	"context"
	"testing"
)

// TestTheBrowserProcessIsWatchedOnMacOS 是 macOS 那条孤儿缺口的真机证据。
//
// macOS 没有 Linux 的 Pdeathsig：agent 被 SIGKILL 时我们的关闭路径根本不会运行，
// 浏览器就留在机器上（探针实测确认）。补法是一个看门狗进程，而「它确实被起了」这件
// 事在参数测试里看不出来——那正是这条存在的理由。
func TestTheBrowserProcessIsWatchedOnMacOS(t *testing.T) {
	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	chromium := firstChromium(t, rt)
	if chromium.confinement == nil {
		t.Fatal("no confinement was established: nothing will reap this browser if the agent is killed")
	}
	watchdog, ok := chromium.confinement.(*darwinWatchdog)
	if !ok {
		t.Fatalf("confinement is %T, want the orphan watchdog", chromium.confinement)
	}
	if watchdog.cmd.Process == nil || watchdog.cmd.ProcessState != nil {
		t.Errorf("the watchdog is not running; the orphan gap is open")
	}
}
