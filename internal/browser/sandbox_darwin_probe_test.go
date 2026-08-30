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

// chromePathForProbe 找这台机器上的浏览器：setup-chrome 会设 CHROME_PATH。
func chromePathForProbe(t *testing.T) string {
	t.Helper()

	if p := strings.TrimSpace(os.Getenv("CHROME_PATH")); p != "" {
		return p
	}
	if p := newPlatformAdapter().ResolveChromiumPath(); p != "" {
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

// probeProfile 是一个候选 profile：名字 + SBPL 文本（%s 处填可写目录）。
type probeProfile struct {
	name string
	// sbpl 收到一个 profile 目录，返回完整的 SBPL 文本。
	sbpl func(userDataDir string) string
	// extraArgs 是这个变体额外要带的浏览器参数。
	extraArgs []string
	// expectItToStart 说明这个变体**应该**能起来。对照组填 false：它红是预期结果，
	// 不该让整个探针变红——那样下一个人会以为环境坏了。
	expectItToStart bool
}

// probeProfiles 是要试的几条路。形状照着 Linux 那份 bwrap profile：**整盘只读、
// 只有自己的 profile 目录可写**——真正要防的是写，读系统字体/证书/共享库一一列举
// 既列不全也会随版本漂移。
// writeConfinedSBPL 是收紧之后的那份：**只有 profile 目录可写**。
//
// 第一版把 /private/tmp 与 /private/var/folders 整片放行，而 macOS 上每个 app 的
// 临时目录都在后者底下——探针实测「profile 之外的写」照样成功，那层沙箱什么都没挡。
// 一个看着像沙箱、实际不挡事的东西比没有更糟，因为它让人以为有。
//
// 浏览器仍然需要一个临时目录，所以把 TMPDIR 指进 profile 里自己那一个（见探针里的
// Env），于是这两片都可以从可写列表里去掉。
func writeConfinedSBPL(dir string) string {
	// **必须是解析过符号链接的真实路径**：macOS 上 /tmp 与 /var 都是指向 /private/…
	// 的软链，而 SBPL 的 subpath 按解析后的路径匹配。写 /var/folders/xx 进 profile，
	// 内核看到的却是 /private/var/folders/xx——两边对不上，于是**连自己的 profile
	// 目录都写不了**，浏览器一个字都说不出来就退出。第二轮探针正是死在这里。
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return fmt.Sprintf(`(version 1)
(allow default)
(deny file-write*)
(allow file-write*
  (subpath %q)
  (literal "/dev/null")
  (literal "/dev/dtracehelper")
  (regex #"^/dev/tty"))
`, dir)
}

// userTempAllowRules 放行本用户自己的临时与缓存目录。
//
// macOS 把它们放在 /private/var/folders/<xx>/<yyy>/{T,C}/：T 是 $TMPDIR，C 是缓存。
// 它们**按用户**分，不按 app 分，所以这仍然比只放 profile 目录宽；但比放行整个
// /private/var/folders 窄得多——后者还含着别的用户与系统自己的那些。
func userTempAllowRules() string {
	tmp := os.Getenv("TMPDIR")
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	tmp = strings.TrimSuffix(tmp, string(os.PathSeparator))
	cache := filepath.Join(filepath.Dir(tmp), "C")
	return fmt.Sprintf(`(allow file-write*
  (subpath %q)
  (subpath %q))
`, tmp, cache)
}

var probeProfiles = []probeProfile{
	{
		// 出货的那一份：见 seatbeltProfile。
		name:            "shipped",
		expectItToStart: true,
		sbpl: func(dir string) string {
			profile, err := seatbeltProfile(seatbeltSpec{
				UserDataDir: dir, TempDir: os.TempDir(), OnlyLoopbackEgress: true,
			})
			if err != nil {
				panic(err)
			}
			return profile
		},
	},
	{
		// 对照：只放 profile 目录，不放本用户的 T/C。**预期起不来**——Chromium 不认
		// TMPDIR 的重定向，这一条留在这里是为了下次有人想收紧时能立刻看到代价。
		name:            "profile-dir-only",
		sbpl:            writeConfinedSBPL,
		expectItToStart: false,
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
			args = append(args, profile.extraArgs...)
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

// TestProbeWritesOutsideTheProfileAreDenied：上面那条只证明它**起得来**。这条问的
// 是这层沙箱到底挡不挡事——一个包着却什么都不挡的沙箱，比没有更糟，因为它会让人
// 以为有。
func TestProbeWritesOutsideTheProfileAreDenied(t *testing.T) {
	dir := t.TempDir()
	userDataDir := filepath.Join(dir, "profile")
	if err := os.MkdirAll(userDataDir, 0o700); err != nil {
		t.Fatalf("mkdir profile: %v", err)
	}
	profilePath := filepath.Join(dir, "profile.sb")
	if err := os.WriteFile(profilePath, []byte(writeConfinedSBPL(userDataDir)), 0o600); err != nil {
		t.Fatalf("write sbpl: %v", err)
	}

	// 上一版把「外面」选在 t.TempDir() 里，而那是 /private/var/folders/... ——
	// 恰好落在当时的可写列表里。于是探针报了绿，测的却是自己挖的那个洞。
	// 现在选 HOME 下的一个路径：那才是「被攻破的浏览器最想写的地方」。
	outside := filepath.Join(os.Getenv("HOME"), ".stardust-sandbox-probe")
	defer func() { _ = os.Remove(outside) }()
	out, err := exec.Command("/usr/bin/sandbox-exec", "-f", profilePath, //nolint:gosec
		"/bin/sh", "-c", "echo pwned > "+outside).CombinedOutput()
	if err == nil {
		t.Fatalf("a write outside the profile succeeded: the sandbox confines nothing\n%s", out)
	}
	t.Logf("write outside the profile was denied as expected: %v (%s)", err, strings.TrimSpace(string(out)))

	resolvedUserDataDir, err := filepath.EvalSymlinks(userDataDir)
	if err != nil {
		t.Fatalf("resolve %s: %v", userDataDir, err)
	}
	inside := filepath.Join(resolvedUserDataDir, "inside.txt")
	if out, err := exec.Command("/usr/bin/sandbox-exec", "-f", profilePath,
		"/bin/sh", "-c", "echo ok > "+inside).CombinedOutput(); err != nil {
		t.Fatalf("a write inside the profile was denied, so the browser cannot run: %v\n%s", err, out)
	}
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

// TestProbeTheProductionWrapperStartsTheBrowser 走**生产那条路**：PrepareCommand
// 自己去拼 sandbox-exec 的命令行。
//
// 上面那些变体证明的是 profile 本身对不对；这条证明的是**接线**对不对。两者会分开
// 坏：出货 profile 在探针里通过的同一次 CI 上，e2e 里的浏览器仍然起不来——那说明
// 差异不在 profile，在包装。
func TestProbeTheProductionWrapperStartsTheBrowser(t *testing.T) {
	chrome := chromePathForProbe(t)

	userDataDir := filepath.Join(t.TempDir(), "user-data")
	cmd := exec.Command(chrome,
		"--headless=new", "--remote-debugging-port=0",
		"--user-data-dir="+userDataDir,
		"--no-first-run", "--no-default-browser-check", "--disable-gpu", "about:blank")

	pal := newPlatformAdapter()
	wrapped, err := pal.PrepareCommand(cmd)
	if err != nil {
		t.Fatalf("PrepareCommand: %v", err)
	}
	t.Logf("wrapped as: %s %v", wrapped.Path, wrapped.Args[:3])

	ctx, cancel := context.WithTimeout(context.Background(), probeLaunchTimeout)
	defer cancel()
	browser, err := launchChromium(ctx, launchSpec{Bin: wrapped.Path, Args: wrapped.Args[1:], PAL: pal})
	if err != nil {
		if profile, readErr := os.ReadFile(filepath.Join(userDataDir, "seatbelt.sb")); readErr == nil {
			t.Logf("the profile the wrapper generated:\n%s", profile)
		} else {
			t.Logf("(no profile at %s: %v)", filepath.Join(userDataDir, "seatbelt.sb"), readErr)
		}
		t.Fatalf("the production wrapper could not start the browser: %v", err)
	}
	defer func() { _ = browser.cmd.Process.Kill(); _ = browser.Wait() }()
	t.Logf("devtools at %s", browser.controlURL)
}
