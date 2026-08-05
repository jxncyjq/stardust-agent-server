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
	// --no-sandbox 在容器/无 userns 环境常需，但有安全代价——默认不加，由 Phase 5 沙箱策略决定。
	return []string{"--disable-gpu", "--no-first-run", "--no-default-browser-check"}
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
