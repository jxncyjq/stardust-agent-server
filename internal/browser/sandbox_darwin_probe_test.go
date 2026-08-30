//go:build darwin && sandboxprobe

package browser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 这是一组**探针**，不是回归测试：`go test -tags sandboxprobe ./internal/browser/`，
// 只在 macOS runner 上手动跑。它要回答的是「macOS 上这层沙箱到底做不做得成」，
// 而这台开发机是 Windows，猜不出来。
//
// 上一次在 Linux 上把 bubblewrap 一路写完才发现 CI 起不来（Crashpad 要 /dev/shm、
// bind 源路径必须先存在、stderr 被丢掉所以什么都看不到），返工了四轮。所以这次先探。
//
// 每个变体是一个子测试：过了就是这条路走得通，红了就是走不通，CI 日志里直接是一张
// 表。探针失败不代表代码有 bug——它就是这次要收集的数据。

const probeLaunchTimeout = 45 * time.Second

// chromePathForProbe 找**生产会用的那个**浏览器。
//
// 顺序与 PAL 一致（先 /Applications，再 CHROME_PATH），而不是反过来：此前反过来，
// 于是探针一直在探 setup-chrome 装的 Chromium，而 e2e 跑的是 /Applications 里的
// Google Chrome——探针全绿、e2e 全红，差的就是这个。两个浏览器要写的地方并不一样。
func chromePathForProbe(t *testing.T) string {
	t.Helper()

	if p := newPlatformAdapter().ResolveChromiumPath(); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("CHROME_PATH")); p != "" {
		return p
	}
	t.Skip("no chromium on this machine (set CHROME_PATH)")
	return ""
}

// TestProbeSandboxExecExists：`sandbox-exec` 在 Apple 的文档里被标了 deprecated
// 很多年，却仍然随系统发。它在不在，决定后面所有事。
func TestProbeSandboxExecExists(t *testing.T) {
	info, err := os.Stat("/usr/bin/sandbox-exec")
	if err != nil {
		t.Fatalf("stat /usr/bin/sandbox-exec: %v", err)
	}
	t.Logf("sandbox-exec present: mode=%v size=%d", info.Mode(), info.Size())

	out, err := exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)",
		"/bin/echo", "ok").CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox-exec cannot even run /bin/echo: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("unexpected output: %q", out)
	}
}

// probeProfile 是一个候选 profile。
type probeProfile struct {
	name string
	sbpl func(userDataDir string) string
	// expectItToStart 说明这个变体**应该**能起来。
	expectItToStart bool
}

// 结论（八轮探针，2026-08-30）：**Google Chrome 跑不进 sandbox-exec**，而 macOS 上
// 绝大多数机器只装着它。下面第一条就是判据——一份「什么都不限、只是包了一层」的
// profile：Chromium（开源构建）在它下面起得来，Google Chrome 起不来，浏览器一个字都
// 不说就退出。既然连不限制都不行，收紧到什么程度都无从谈起。
//
// 这些留在仓库里是为了：下一次有人想在 macOS 上加外层沙箱时，先跑一遍这个，而不是
// 从头再猜一遍。
var probeProfiles = []probeProfile{
	{
		name:            "no-restrictions-at-all",
		expectItToStart: true,
		sbpl:            func(string) string { return "(version 1)\n(allow default)\n" },
	},
}

// TestProbeChromeUnderSandboxExec：把真的浏览器包进 sandbox-exec，看它还认不认得
// 出 DevTools 地址——那是「起来了」的唯一硬标准，`--version` 能跑不算数。
func TestProbeChromeUnderSandboxExec(t *testing.T) {
	chrome := chromePathForProbe(t)

	for _, profile := range probeProfiles {
		t.Run(profile.name, func(t *testing.T) {
			dir := t.TempDir()
			userDataDir := filepath.Join(dir, "profile")
			if err := os.MkdirAll(userDataDir, 0o700); err != nil {
				t.Fatalf("mkdir profile: %v", err)
			}
			profilePath := filepath.Join(dir, "profile.sb")
			if err := os.WriteFile(profilePath, []byte(profile.sbpl(userDataDir)), 0o600); err != nil {
				t.Fatalf("write sbpl: %v", err)
			}

			args := []string{"-f", profilePath, chrome}
			args = append(args,
				"--headless=new",
				"--remote-debugging-port=0",
				"--user-data-dir="+userDataDir,
				"--no-first-run", "--no-default-browser-check", "--disable-gpu",
				"about:blank",
			)

			ctx, cancel := context.WithTimeout(context.Background(), probeLaunchTimeout)
			defer cancel()

			// TMPDIR 指进 profile：不这么做就得整片放行 /private/var/folders，
			// 而那正是上一轮探针发现的那个大洞。
			tmp := filepath.Join(userDataDir, "tmp")
			if err := os.MkdirAll(tmp, 0o700); err != nil {
				t.Fatalf("mkdir tmp: %v", err)
			}
			browser, err := launchChromium(ctx, launchSpec{
				Bin:  "/usr/bin/sandbox-exec",
				Args: args,
				Env:  append(os.Environ(), "TMPDIR="+tmp),
				PAL:  newPlatformAdapter(),
			})
			if err != nil {
				// 浏览器一个字都没说就退出，那句话在**内核**那边：sandbox 的拒绝
				// 记在统一日志里，不在进程的 stderr 上。不去捞它，就只能靠猜。
				t.Logf("chrome did not come up under %s: %v", profile.name, err)
				t.Logf("kernel said:\n%s", sandboxDenials(t))
				t.Fail()
				return
			}
			defer func() {
				_ = browser.cmd.Process.Kill()
				_ = browser.Wait()
			}()
			t.Logf("%s: devtools at %s (pid %d)", profile.name, browser.controlURL, browser.PID())
		})
	}
}

// sandboxDenials 从统一日志里捞最近的 sandbox 拒绝记录。
func sandboxDenials(t *testing.T) string {
	t.Helper()

	// 只要我们这个进程的：不过滤的话，日志里全是系统守护进程自己的拒绝记录。
	//
	// 试过给 deny 加 (with report) 让它上报——sandbox-exec 直接拒绝那份 profile
	// （"report modifier does not apply to deny action"），而那次的症状是**所有变体
	// 一起红**，看上去像 Chrome 起不来，实际是 profile 根本没编译过。
	out, err := exec.Command("log", "show", "--last", "2m", "--style", "compact",
		"--predicate", `eventMessage CONTAINS "deny" AND (eventMessage CONTAINS "Chromium" OR eventMessage CONTAINS "sandbox-exec" OR eventMessage CONTAINS "Google Chrome")`).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(could not read the unified log: %v)", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, "\n")
}

// TestProbeAnOrphanSurvivesTheParentBeingKilled 是 D4 的那条缺口，先把它**证明出来**
// 再谈怎么补：macOS 没有 Linux 的 Pdeathsig，agent 被 SIGKILL 时（崩溃、被系统杀、
// 被用户强退）我们的 Close 路径根本不会运行。
func TestProbeAnOrphanSurvivesTheParentBeingKilled(t *testing.T) {
	// 用 /bin/sleep 冒充浏览器：这条探针问的是内核行为，与是不是 Chromium 无关。
	parent := exec.Command("/bin/sh", "-c", "/bin/sleep 60 & echo $!; sleep 60")
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := parent.Start(); err != nil {
		t.Fatalf("start parent: %v", err)
	}
	var childPID int
	if _, err := fmt.Fscanln(out, &childPID); err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	if err := parent.Process.Kill(); err != nil {
		t.Fatalf("kill parent: %v", err)
	}
	_ = parent.Wait()
	time.Sleep(500 * time.Millisecond)

	if err := exec.Command("/bin/kill", "-0", fmt.Sprint(childPID)).Run(); err != nil {
		t.Logf("the child died with its parent on this system: %v", err)
		return
	}
	t.Logf("CONFIRMED: pid %d outlived its killed parent — this is the macOS orphan gap", childPID)
	_ = exec.Command("/bin/kill", "-9", fmt.Sprint(childPID)).Run()
}

// TestProbeAWatchdogReapsTheOrphan 试一条补法：跟浏览器一起起一个极小的看门狗，
// 它盯着 agent 的 pid，agent 一没就把浏览器的**进程组**杀掉。
//
// 它不占用浏览器进程本身的位置（pid 仍是 Chromium 的，内存采样与收束照旧），代价
// 是每个浏览器进程多一个在睡觉的 sh。
func TestProbeAWatchdogReapsTheOrphan(t *testing.T) {
	fakeAgent := exec.Command("/bin/sleep", "60")
	if err := fakeAgent.Start(); err != nil {
		t.Fatalf("start fake agent: %v", err)
	}
	defer func() { _ = fakeAgent.Process.Kill(); _ = fakeAgent.Wait() }()

	fakeBrowser := exec.Command("/bin/sleep", "60")
	if err := fakeBrowser.Start(); err != nil {
		t.Fatalf("start fake browser: %v", err)
	}
	browserPID := fakeBrowser.Process.Pid

	watchdog := exec.Command("/bin/sh", "-c",
		fmt.Sprintf("while kill -0 %d 2>/dev/null; do sleep 0.2; done; kill -9 %d 2>/dev/null",
			fakeAgent.Process.Pid, browserPID))
	if err := watchdog.Start(); err != nil {
		t.Fatalf("start watchdog: %v", err)
	}
	defer func() { _ = watchdog.Process.Kill(); _ = watchdog.Wait() }()

	if err := fakeAgent.Process.Kill(); err != nil {
		t.Fatalf("kill the fake agent: %v", err)
	}
	_, _ = fakeAgent.Process.Wait()

	done := make(chan error, 1)
	go func() { done <- fakeBrowser.Wait() }()
	select {
	case <-done:
		t.Logf("the watchdog reaped pid %d after its agent died", browserPID)
	case <-time.After(10 * time.Second):
		_ = fakeBrowser.Process.Kill()
		t.Fatalf("the watchdog did not reap pid %d within 10s", browserPID)
	}
}
