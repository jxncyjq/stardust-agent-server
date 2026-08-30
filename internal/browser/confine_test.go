package browser

import (
	"errors"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// 外层沙箱此前是一个**没有调用方**的占位：PlatformAdapter.WrapWithSandbox 接一个
// exec.Cmd 原样返回，而 Chromium 的进程是 go-rod 的 launcher 自己起的——那个 Cmd
// 从来不存在。三个平台把它实现完，浏览器进程照样一点约束都没有。
//
// 于是这一期换成一个**真的被调用**的接缝：进程起来之后按 pid 收束它。Windows 上
// 是 Job Object（kill-on-close + 内存上限），这台机器能真验；另外两个平台目前**明说
// 没有实现**，而不是假装收束成功——后者会让部署以为自己有一层它并没有的隔离。

func TestConfinementIsAttemptedForEveryBrowserProcess(t *testing.T) {
	t.Parallel()

	pal := NewPlatformAdapter()
	// 用一个自己起的短命进程当被试：不需要 Chromium 就能验「按 pid 收束」这件事。
	cmd := sleeperCommand(t)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the test process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	closer, err := pal.ConfineProcess(cmd.Process.Pid)
	switch {
	case err == nil:
		if closer == nil {
			t.Fatal("ConfineProcess returned no closer and no error: the caller has nothing to release")
		}
		if err := closer.Close(); err != nil {
			t.Errorf("close confinement: %v", err)
		}
	case errors.Is(err, ErrConfinementUnsupported):
		if runtime.GOOS == "windows" {
			t.Errorf("windows reports confinement unsupported, but a Job Object is implemented there")
		}
	default:
		t.Errorf("ConfineProcess: %v", err)
	}
}

// TestUnsupportedConfinementIsSaidOutLoud: the sentinel is the whole contract.
// A platform that quietly returned (nil, nil) would tell a deployment it is
// confined when nothing confines it — and this is a security baseline, so the
// difference between "confined" and "believes it is confined" is the point.
func TestUnsupportedConfinementIsSaidOutLoud(t *testing.T) {
	t.Parallel()

	if ErrConfinementUnsupported == nil {
		t.Fatal("there is no sentinel to distinguish an unconfined platform from a confined one")
	}
	if got := ErrConfinementUnsupported.Error(); got == "" {
		t.Error("the sentinel carries no explanation for an operator reading a log line")
	}
}

// sleeperCommand returns a process that lives long enough to be confined and
// does nothing else.
func sleeperCommand(t *testing.T) *exec.Cmd {
	t.Helper()

	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", "ping", "-n", "30", "127.0.0.1")
	}
	return exec.Command("sleep", "30")
}

// TestClosingTheConfinementTakesTheProcessWithIt 是这层隔离最实在的那条性质：
// agent 没了，浏览器不能留在机器上。
//
// Chromium 是多进程的，杀主进程带不走 renderer/GPU——此前 agent 崩一次就在机器上
// 留下一串孤儿 Chromium，直到用户自己去任务管理器里清。
func TestClosingTheConfinementTakesTheProcessWithIt(t *testing.T) {
	t.Parallel()

	pal := NewPlatformAdapter()
	cmd := sleeperCommand(t)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the test process: %v", err)
	}
	closer, err := pal.ConfineProcess(cmd.Process.Pid)
	if errors.Is(err, ErrConfinementUnsupported) {
		t.Skipf("no outer confinement on %s; this property is only claimed where one exists", runtime.GOOS)
	}
	if err != nil {
		t.Fatalf("ConfineProcess: %v", err)
	}

	if err := closer.Close(); err != nil {
		t.Fatalf("close confinement: %v", err)
	}

	// Wait returns once the process is gone. A confinement that did not kill it
	// would leave this blocking until the test's own deadline.
	done := make(chan error, 1)
	go func() { _, err := cmd.Process.Wait(); done <- err }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the process outlived its confinement: closing the job did not take it with it")
	}
}
