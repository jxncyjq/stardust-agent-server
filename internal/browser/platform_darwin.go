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

// PrepareCommand 在 macOS 上做两件事：把命令包进 sandbox-exec，并建进程组。
//
// **缺了 sandbox-exec 就拒绝启动**，与 Linux 侧缺 bwrap 同一条策略：一个以为自己被
// 沙箱包着、实际没有的部署，比一个起不来的部署危险得多。sandbox-exec 随 macOS 发，
// 缺了它说明这台机器不正常。
//
// Setpgid 让 Chromium 与它 fork 出的 renderer/GPU 同属一个进程组，关闭时能按组一次
// 杀干净——杀主进程带不走它们，那正是孤儿的来源。
//
// 没有 Pdeathsig（Linux 特有），所以「agent 被 SIGKILL 之后浏览器还活着」这条缺口
// 由 ConfineProcess 起的看门狗补，见那里。
func (a darwinAdapter) PrepareCommand(cmd *exec.Cmd) (*exec.Cmd, error) {
	wrapped, err := a.wrapWithSeatbelt(cmd)
	if err != nil {
		return nil, err
	}
	if wrapped.SysProcAttr == nil {
		wrapped.SysProcAttr = &syscall.SysProcAttr{}
	}
	wrapped.SysProcAttr.Setpgid = true
	return wrapped, nil
}

// wrapWithSeatbelt 把命令包进 sandbox-exec。
func (darwinAdapter) wrapWithSeatbelt(cmd *exec.Cmd) (*exec.Cmd, error) {
	if _, err := os.Stat(seatbeltBinary); err != nil {
		return nil, seatbeltUnavailableError(err)
	}
	userDataDir := userDataDirFromArgs(cmd.Args)
	profile, err := seatbeltProfile(seatbeltSpec{
		UserDataDir: userDataDir,
		TempDir:     os.TempDir(),
		// 出网只留回环：浏览器的全部流量本就经本机的出口代理，这层把「绕过代理
		// 直连」也堵死。真机探针确认这么关之后浏览器照常起来。
		OnlyLoopbackEgress: true,
	})
	if err != nil {
		return nil, err
	}
	// profile 写进 user-data-dir：它与浏览器同生共死，清理路径已经在管这个目录。
	// 放 /tmp 反而要再管一份生命周期。
	profilePath := filepath.Join(userDataDir, "seatbelt.sb")
	if err := os.WriteFile(profilePath, []byte(profile), 0o600); err != nil {
		return nil, fmt.Errorf("write the sandbox profile %q: %w", profilePath, err)
	}

	args := append([]string{"-f", profilePath}, cmd.Args...)
	wrapped := exec.Command(seatbeltBinary, args...)
	wrapped.Env = cmd.Env
	wrapped.Dir = cmd.Dir
	wrapped.Stdin = cmd.Stdin
	wrapped.Stdout = cmd.Stdout
	wrapped.Stderr = cmd.Stderr
	wrapped.ExtraFiles = cmd.ExtraFiles
	return wrapped, nil
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
	return &darwinWatchdog{cmd: watchdog}, nil
}

// darwinWatchdog 关掉那个看门狗。正常关闭时浏览器已经被我们自己收掉了，看门狗留着
// 只会白等一个不会来的事件。
type darwinWatchdog struct{ cmd *exec.Cmd }

func (w *darwinWatchdog) Close() error {
	if w.cmd.Process == nil {
		return nil
	}
	if err := w.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stop the orphan watchdog: %w", err)
	}
	_, err := w.cmd.Process.Wait()
	if err != nil && !errors.Is(err, syscall.ECHILD) {
		return fmt.Errorf("reap the orphan watchdog: %w", err)
	}
	return nil
}

func fileExistsDarwin(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
