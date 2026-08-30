//go:build linux

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

type linuxAdapter struct{}

func newPlatformAdapter() PlatformAdapter { return linuxAdapter{} }

func (linuxAdapter) ResolveChromiumPath() string {
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func (linuxAdapter) DefaultLaunchArgs() []string {
	// --no-sandbox：容器/CI/无 user-namespaces 的 Linux 上，Chromium 内部 zygote 沙箱
	// 无法初始化（ZygoteHostImpl::Init fatal），必须关闭内部沙箱；服务模式目标本就是
	// Linux 服务器（多在容器内），真正的隔离边界由外层 OS 沙箱 WrapWithSandbox（Phase 5）
	// 提供，而非 Chromium 内部沙箱。--disable-dev-shm-usage 规避 CI 上 /dev/shm 过小导致的崩溃。
	return []string{
		"--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--no-sandbox", "--disable-dev-shm-usage",
	}
}

// KillProcess 杀一个进程**连同它的整个进程组**。
//
// 按组而不是按 pid：Chromium 是多进程的，杀主进程带不走 renderer/GPU——那正是孤儿
// 进程的来源。进程组由 PrepareCommand 的 Setpgid 建立；没有组（别处起的进程）时
// 退回按 pid，并且这不是兜底：负号 pid 在没有该组时返回 ESRCH，那种情况下按 pid
// 杀才是正确答案。
func (linuxAdapter) KillProcess(pid int, graceful bool) error {
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

func (linuxAdapter) AppDataDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".local", "share")
	}
	return filepath.Join(base, "stardust", "browser")
}

func (linuxAdapter) CacheDir() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	return filepath.Join(base, "stardust", "browser")
}

func (linuxAdapter) ToNativePath(posix string) string { return posix } // 已是 POSIX

func (linuxAdapter) SafeDelete(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("safe delete %q: %w", path, err)
	}
	return nil
}

// ConfineProcess 在 Linux 上目前**没有实现**，明说而不是静默放行。
//
// 真正的外层隔离（user namespace + seccomp）必须在**创建进程时**建立，而 Chromium
// 的进程是 go-rod 的 launcher 起的，这里只拿得到一个 pid——对一个已经跑起来的进程
// 补 namespace 是做不到的。把它做出来要先把启动路径收回自管（Phase 6 进程池），
// 那时它属于「怎么起这个进程」，而不是「起来之后补一层」。
//
// 在此之前，Linux 上的边界是 Chromium 自己的渲染沙箱加部署侧的容器；本函数返回
// ErrConfinementUnsupported，让部署自己决定是照常跑还是拒绝启动
// （browser.require_sandbox）。
// PrepareCommand 在 Linux 上做两件**不需要任何特权**的事：
//
//  1. Pdeathsig=SIGKILL —— agent 进程一死，内核立刻杀掉这个 Chromium。这是 Windows
//     那个 Job Object 的 kill-on-close 在 Linux 上的对应物：崩溃、被 kill -9、被
//     容器停掉，都不会在机器上留下一串浏览器进程。
//  2. Setpgid —— 让 Chromium 与它 fork 出来的 renderer/GPU 进程同属一个进程组，
//     于是关闭时可以按组一次杀干净。杀主进程带不走它们（这正是孤儿的来源）。
//
// 它**不是**外层沙箱。namespaces+seccomp 的位置也在这里，但那要么依赖 bubblewrap
// 之类的外部程序，要么需要 user namespace（CI runner 上常常没有——Chromium 自己的
// zygote 沙箱就是因此在那里崩过，见 DefaultLaunchArgs 的 --no-sandbox）。选哪条路
// 是一次要单独做的决定，不该混在这次「别留孤儿」里悄悄带过。
func (linuxAdapter) PrepareCommand(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	return cmd
}

func (linuxAdapter) ConfineProcess(int) (io.Closer, error) { return nil, ErrConfinementUnsupported }
