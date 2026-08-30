//go:build linux

package browser

import (
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

func (linuxAdapter) KillProcess(pid int, graceful bool) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	sig := syscall.SIGKILL
	if graceful {
		sig = syscall.SIGTERM
	}
	if err := p.Signal(sig); err != nil {
		return fmt.Errorf("signal %v to %d: %w", sig, pid, err)
	}
	return nil
}

func (linuxAdapter) SampleProcessMemory(pid int) uint64 { return 0 } // 占位：Phase 6 读 /proc/<pid>/status
func (linuxAdapter) AvailableSystemMemory() uint64      { return 0 } // 占位：Phase 8 读 /proc/meminfo

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
func (linuxAdapter) ConfineProcess(int) (io.Closer, error) { return nil, ErrConfinementUnsupported }
