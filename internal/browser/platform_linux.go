//go:build linux

package browser

import (
	"fmt"
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

func (linuxAdapter) WrapWithSandbox(cmd *exec.Cmd) *exec.Cmd { return cmd } // 占位：Phase 5 namespaces+seccomp
