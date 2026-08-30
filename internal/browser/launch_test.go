package browser

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Chromium 的进程此前是 go-rod 的 launcher 自己起的，我们只拿得到一个 pid。这挡住
// 了三件事，而它们都属于「进程怎么被创建」而不是「起来之后补一层」：
//
//   - Linux 的 namespaces+seccomp 与 macOS 的 sandbox-exec 必须在**创建时**建立；
//   - 进程池要自己决定何时起、起几个、什么时候回收；
//   - 起不来时的诊断（Chromium 往 stderr 写的那几行）现在被 launcher 吃掉了。
//
// 所以先把启动收回来：自己拼参数、自己 exec、自己从 stderr 读出 DevTools 的地址。

func TestTheDevToolsAddressIsReadFromTheBrowsersOwnOutput(t *testing.T) {
	t.Parallel()

	stderr := strings.Join([]string{
		"[0830/145501.123:WARNING:bluetooth_adapter_winrt.cc(1211)] Getting Default Adapter failed.",
		"DevTools listening on ws://127.0.0.1:51234/devtools/browser/9a1f-4c2e",
		"[0830/145501.456:INFO:CONSOLE(1)] something else entirely",
	}, "\n")

	url, err := readDevToolsURL(context.Background(), strings.NewReader(stderr), nil)
	if err != nil {
		t.Fatalf("readDevToolsURL: %v", err)
	}
	if url != "ws://127.0.0.1:51234/devtools/browser/9a1f-4c2e" {
		t.Errorf("url = %q, want the address Chromium announced", url)
	}
}

// TestAChromiumThatDiesTakesItsOutputWithIt: 启动失败时 Chromium 会把原因写在
// stderr 上（缺库、沙箱起不来、用户目录被占）。此前那几行被 launcher 吃掉，运维只
// 看到一句「launch chromium: context deadline exceeded」。
func TestAChromiumThatDiesTakesItsOutputWithIt(t *testing.T) {
	t.Parallel()

	stderr := "FATAL: Failed to create a ProcessSingleton for your profile directory.\n"

	_, err := readDevToolsURL(context.Background(), strings.NewReader(stderr), nil)
	if err == nil {
		t.Fatal("readDevToolsURL succeeded on output that never announced an address")
	}
	if !strings.Contains(err.Error(), "ProcessSingleton") {
		t.Errorf("error = %v, want it to carry what the browser actually said", err)
	}
}

func TestWaitingForTheAddressRespectsItsDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// 一个永远不结束、也永远不说话的输出流：这正是「Chromium 卡住了」的样子。
	_, err := readDevToolsURL(ctx, blockingReader{ctx: ctx}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the deadline to surface", err)
	}
}

// TestThePlatformGetsTheCommandBeforeItStarts 是这次重构的**理由本身**：外层沙箱
// 必须在创建进程时建立。此前 PAL 有一个接 *exec.Cmd 的钩子却没有调用方——因为那时
// 根本没有一个我们自己的 Cmd。
func TestThePlatformGetsTheCommandBeforeItStarts(t *testing.T) {
	t.Parallel()

	pal := &recordingPAL{PlatformAdapter: NewPlatformAdapter()}
	spec := launchSpec{
		Bin:  helperCommandPath(t),
		Args: []string{"--version"},
		PAL:  pal,
	}
	// 这个进程不会宣告 DevTools 地址，所以启动必然以「没等到地址」失败——本测试
	// 关心的只是 PAL 有没有在 Start 之前拿到那个 Cmd。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = launchChromium(ctx, spec)

	if !pal.prepared {
		t.Error("the platform adapter never saw the command; an outer sandbox cannot be applied after exec")
	}
}

// recordingPAL 记下 PrepareCommand 是否被调用过。
type recordingPAL struct {
	PlatformAdapter
	prepared bool
}

func (p *recordingPAL) PrepareCommand(cmd *exec.Cmd) (*exec.Cmd, error) {
	p.prepared = true
	return cmd, nil
}

// blockingReader 阻塞到 ctx 结束，然后报错——模拟一个不说话的浏览器。
type blockingReader struct{ ctx context.Context }

func (r blockingReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// sleeperWithProfile 返回一个「长得像浏览器」的替身进程：活得够久，并且带着
// --user-data-dir。
//
// 带那个参数不是装样子：Linux 的 PrepareCommand 会把命令包进 bwrap，而 bwrap 需要
// 知道**哪个目录必须可写**，它是从命令行里读的（见 userDataDirFromArgs）。不带就会
// 被正当地拒绝——CI 的 ubuntu 那条就是这么红的。
func sleeperWithProfile(t *testing.T) *exec.Cmd {
	t.Helper()

	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		// Windows 的 PrepareCommand 不读参数（隔离是事后加的 Job Object），所以
		// 这里不必也不能给 ping 塞一个它不认的参数。
		return exec.Command("cmd", "/c", "ping", "-n", "30", "127.0.0.1")
	}
	// sh -c 'sleep 30' 之后的参数是 $0/$1，对被执行的脚本无害。
	return exec.Command("/bin/sh", "-c", "sleep 30", "sleeper", "--user-data-dir="+dir)
}

// prepareOrSkip 让 PAL 准备这个命令；平台要求外层沙箱而这台机器上没有时跳过。
//
// 跳过而不是失败：这条测试断言的是**关闭路径**，而在一台没有 bwrap 的 Linux 上
// 根本不存在可断言的浏览器进程——那是那台机器的部署状态，不是这段代码的回归。
func prepareOrSkip(t *testing.T, pal PlatformAdapter, cmd *exec.Cmd) *exec.Cmd {
	t.Helper()

	prepared, err := pal.PrepareCommand(cmd)
	if err != nil {
		t.Skipf("this platform cannot prepare a browser command here: %v", err)
	}
	return prepared
}

// helperCommandPath 返回一个存在、能跑、且不会宣告 DevTools 地址的可执行文件。
func helperCommandPath(t *testing.T) string {
	t.Helper()

	path, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go binary to use as a stand-in process: %v", err)
	}
	return path
}

// TestTheBrowsersOutputKeepsBeingRead 是一条不读就会挂的性质：管道缓冲只有几十
// KB，而 Chromium 每次 GPU/字体/证书告警都写一行。宣告完地址就撒手，它迟早卡在
// 一次 write 上，症状是整个浏览器无声地停住——不是崩溃，是不动了。
func TestTheBrowsersOutputKeepsBeingRead(t *testing.T) {
	t.Parallel()

	browser := &launchedBrowser{}
	// 远超管道缓冲的数据量：不持续读的实现会在这里停住。
	var chatty strings.Builder
	for i := 0; i < 5000; i++ {
		chatty.WriteString("[WARNING:gpu_process_host.cc(1000)] a very chatty browser line\n")
	}
	chatty.WriteString("the last thing it said\n")

	done := make(chan struct{})
	go func() {
		browser.drainOutput(strings.NewReader(chatty.String()))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("draining never finished")
	}

	recent := browser.RecentOutput()
	if len(recent) == 0 || recent[len(recent)-1] != "the last thing it said" {
		t.Errorf("recent output = %v, want it to end with the browser's last line", recent)
	}
	if len(recent) > maxRememberedOutputLines {
		t.Errorf("kept %d lines, want at most %d: this buffer must not grow with a long-running browser",
			len(recent), maxRememberedOutputLines)
	}
}

// TestCloseKillsTheProcessEvenWithoutConfinement 补上真机测试**测不到**的那一半。
//
// 在 Windows 上，Close 里的 Kill 是多余的：Job Object 的 kill-on-close 已经把进程
// 带走了，所以「关掉之后没有残留」那条真机断言即使删掉 Kill 也照样绿（变异实测如
// 此）。而在没有收束实现的平台（Linux/macOS，见各 platform 文件）上，Kill 是唯一
// 让 Chromium 不留在机器上的东西。
//
// 这条测试把 Kill 单独拎出来验：一个没有 confinement 的 Manager，Close 之后进程必须
// 已经不在。
func TestCloseKillsTheProcessEvenWithoutConfinement(t *testing.T) {
	t.Parallel()

	sleeper := sleeperWithProfile(t)
	if err := sleeper.Start(); err != nil {
		t.Fatalf("start the stand-in process: %v", err)
	}
	// 直接构造一个**进程实例**而不是整个 Manager：这条性质属于「一个进程怎么被
	// 送走」，与池、与出口代理都无关。
	instance := &chromiumInstance{
		launched: &launchedBrowser{cmd: sleeper},
		pal:      NewPlatformAdapter(),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	instance.Close()

	assertReaped(t, sleeper)
}

// assertReaped 断言这个进程已经结束、且已经被收尸。
//
// 只看 ProcessState 是否非 nil，**不问它是怎么死的**：Close 用的是 SIGKILL/
// TerminateProcess，而 unix 上被信号杀死的进程 ProcessState.Exited() 是 **false**
// （它是被信号终止的，不是自己退出的），Windows 上则是 true。第一版按 Exited()
// 断言，于是本机绿、CI 的 ubuntu 与 macos 两条全红——正是这个矩阵存在的理由。
//
// 也不能用 Signal(0) 探活：Windows 上 os.Process.Signal 对任何信号都返回错误，
// 那样写等于一条永远绿的断言。
func assertReaped(t *testing.T, cmd *exec.Cmd) {
	t.Helper()

	if cmd.ProcessState == nil {
		_ = cmd.Process.Kill()
		t.Fatal("Close returned without reaping the process: on a platform without confinement " +
			"the browser would be left running")
	}
}

// TestClosingKillsTheWholeBrowserProcessGroup 钉的是「杀主进程带不走它的孩子」这件
// 事——那正是孤儿进程的来源：Chromium 每个标签页、GPU、网络服务都是独立进程。
//
// 用一个自己 fork 的替身而不是真 Chromium：这条性质属于**关闭路径**，与浏览器是谁
// 无关，而真机测试在 Windows 上会被 Job Object 抢先满足，看不出 Kill 这一步。
func TestClosingKillsTheWholeBrowserProcessGroup(t *testing.T) {
	t.Parallel()

	pal := NewPlatformAdapter()
	parent := prepareOrSkip(t, pal, sleeperWithProfile(t))
	if err := parent.Start(); err != nil {
		t.Fatalf("start the stand-in process: %v", err)
	}
	instance := &chromiumInstance{
		launched: &launchedBrowser{cmd: parent},
		pal:      pal,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	instance.Close()

	assertReaped(t, parent)
}
