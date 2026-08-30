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

// ConfineProcess 在 macOS 上目前**没有实现**，明说而不是静默放行。
//
// App Sandbox 与 sandbox-exec 都是**创建进程时**的事（前者靠 entitlements 与签名，
// 后者靠 sandbox-exec 包住命令行），而这里只拿得到一个已经在跑的 pid。与 Linux 同
// 理，它要等启动路径收回自管之后才谈得上。
// PrepareCommand 在 macOS 上建进程组（Setpgid），使关闭时能把 Chromium 连同它
// fork 出来的 renderer/GPU 一起杀掉——杀主进程带不走它们，那正是孤儿的来源。
//
// 没有 Pdeathsig：那是 Linux 特有的。macOS 上「agent 崩了浏览器也得走」目前只靠
// Close 路径，agent 被 SIGKILL 时仍会留下进程——这条缺口写在这里，等 sandbox-exec
// （它的位置也是这里）一并处理。
func (darwinAdapter) PrepareCommand(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return cmd
}

func (darwinAdapter) ConfineProcess(int) (io.Closer, error) { return nil, ErrConfinementUnsupported }

func fileExistsDarwin(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
