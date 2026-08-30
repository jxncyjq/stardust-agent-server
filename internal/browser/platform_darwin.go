//go:build darwin

package browser

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

type darwinAdapter struct{}

func newPlatformAdapter() PlatformAdapter { return darwinAdapter{} }

func (darwinAdapter) ResolveChromiumPath() string {
	candidates := []string{
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	}
	for _, p := range candidates {
		if fileExistsDarwin(p) {
			return p
		}
	}
	return ""
}

func (darwinAdapter) DefaultLaunchArgs() []string {
	return []string{"--disable-gpu", "--no-first-run", "--no-default-browser-check"}
}

// KillProcess 杀一个进程连同它的整个进程组（见 linux 侧同名方法的说明）。
func (darwinAdapter) KillProcess(pid int, graceful bool) error {
	sig := syscall.SIGKILL
	if graceful {
		sig = syscall.SIGTERM
	}
	if err := syscall.Kill(-pid, sig); err == nil {
		return nil
	} else if !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal %v to process group %d: %w", sig, pid, err)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := p.Signal(sig); err != nil {
		return fmt.Errorf("signal %v to %d: %w", sig, pid, err)
	}
	return nil
}

func (darwinAdapter) AppDataDir() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "stardust", "browser")
}

func (darwinAdapter) CacheDir() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "Caches", "stardust", "browser")
}

func (darwinAdapter) ToNativePath(posix string) string { return posix }

func (darwinAdapter) SafeDelete(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("safe delete %q: %w", path, err)
	}
	return nil
}

// PrepareCommand 在 macOS 上只建进程组：让 Chromium 与它 fork 出的 renderer/GPU 同属
// 一个组，关闭时能按组一次杀干净——杀主进程带不走它们，那正是孤儿的来源。
//
// **这里没有外层沙箱，而且不是没做，是做不成。** sandbox-exec（Seatbelt）是 macOS 上
// 唯一不需要签名与 entitlements 就能包住任意子进程的东西，真机探针的结论是：
// Chromium（开源构建）能跑在里面，**Google Chrome 不能**——连一份「什么都不限、只是
// 包了一层」的 profile 都起不来，浏览器一个字都不说就退出。而 Google Chrome 正是绝
// 大多数 macOS 机器上唯一装着的那个浏览器。
//
// 于是这里的选择只有两种：要么按 Linux 那条策略「缺沙箱就拒绝启动」，那等于让内置
// 浏览器在 macOS 上直接不可用；要么诚实地说这台机器上没有外层沙箱。选后者，并且
// **不假装有**——ConfineProcess 提供的是孤儿保护，不是隔离，两者写清楚，别混为一谈。
//
// 没有 Pdeathsig（Linux 特有），孤儿那条缺口由 ConfineProcess 起的看门狗补。
func (darwinAdapter) PrepareCommand(cmd *exec.Cmd) (*exec.Cmd, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return cmd, nil
}

// ConfineProcess 在 macOS 上补的是**孤儿**那条缺口，不是隔离——隔离在
// PrepareCommand 的 sandbox-exec 里。
//
// macOS 没有 Linux 的 Pdeathsig：agent 被 SIGKILL（崩溃、被系统杀、被用户强退）时，
// 我们的关闭路径根本不会运行，浏览器就留在机器上。真机探针确认过这一点。
//
// 补法是一个极小的看门狗：盯着 agent 自己的 pid，一没就把浏览器的**进程组**杀掉。
// 它不占浏览器进程本身的位置（pid 仍是 Chromium 的，内存采样与进程池照旧），代价是
// 每个浏览器进程多一个在睡觉的 sh。
func (darwinAdapter) ConfineProcess(pid int) (io.Closer, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("confine: invalid pid %d", pid)
	}
	// kill -9 -<pid>：负号是「整个进程组」，而进程组正是 PrepareCommand 建的那个。
	script := fmt.Sprintf("while kill -0 %d 2>/dev/null; do sleep 0.5; done; kill -9 -%d 2>/dev/null",
		os.Getpid(), pid)
	watchdog := exec.Command("/bin/sh", "-c", script)
	if err := watchdog.Start(); err != nil {
		return nil, fmt.Errorf("start the orphan watchdog for pid %d: %w", pid, err)
	}
	return &darwinWatchdog{cmd: watchdog, pid: pid}, nil
}

// darwinWatchdog 是那个浏览器进程的隔离句柄。
//
// Close 遵守与 Windows Job Object **相同的契约：关掉隔离，进程跟着走**。看门狗只覆盖
// 「agent 被 SIGKILL」那条路径；正常关闭走的是这里，所以这里必须自己动手——否则同一个
// 接口在两个平台上意味着两件事，调用方无从写对（共用的那条隔离测试正是这么红的）。
type darwinWatchdog struct {
	cmd *exec.Cmd
	pid int
}

func (w *darwinWatchdog) Close() error {
	var errs []error
	// 先按**进程组**杀（负号 = 整组）：Chromium 是多进程的，杀主进程带不走
	// renderer/GPU。已经没了（ESRCH）是正常的——正常关闭路径可能刚收过它。
	if err := syscall.Kill(-w.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		errs = append(errs, fmt.Errorf("kill the confined process group %d: %w", w.pid, err))
	}
	// 再按 pid 杀一次。进程组那一发只在**这个进程是组长**时命中，而那要 Setpgid——
	// PrepareCommand 会设，别处起的进程不会。少了这一发，一个不是组长的进程会被
	// 悄悄放过：ESRCH 被当成「已经没了」，实际它还好好活着。
	if err := syscall.Kill(w.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		errs = append(errs, fmt.Errorf("kill the confined process %d: %w", w.pid, err))
	}
	if w.cmd.Process != nil {
		if err := w.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			errs = append(errs, fmt.Errorf("stop the orphan watchdog: %w", err))
		}
		if _, err := w.cmd.Process.Wait(); err != nil && !errors.Is(err, syscall.ECHILD) {
			errs = append(errs, fmt.Errorf("reap the orphan watchdog: %w", err))
		}
	}
	return errors.Join(errs...)
}

func fileExistsDarwin(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
