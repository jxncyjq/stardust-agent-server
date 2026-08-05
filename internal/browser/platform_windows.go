//go:build windows

package browser

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type windowsAdapter struct{}

func newPlatformAdapter() PlatformAdapter { return windowsAdapter{} }

func (windowsAdapter) ResolveChromiumPath() string {
	// 系统 Chrome/Edge 常见路径；内置捆绑与 config 覆盖在 chromium_dist.go 统一编排。
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
	}
	for _, p := range candidates {
		if p != "" && fileExists(p) {
			return p
		}
	}
	return "" // 交给 chromium_dist.go 兜底（go-rod 下载）
}

func (windowsAdapter) DefaultLaunchArgs() []string {
	return []string{"--disable-gpu", "--no-first-run", "--no-default-browser-check"}
}

func (windowsAdapter) KillProcess(pid int, graceful bool) error {
	// Windows 无 POSIX 信号；os.Process.Kill 走 TerminateProcess。graceful 在
	// Windows 上无对应轻量信号，这里直接终止（进程组清理由 Job Object 负责，Phase 5）。
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := p.Kill(); err != nil {
		return fmt.Errorf("terminate process %d: %w", pid, err)
	}
	return nil
}

func (windowsAdapter) SampleProcessMemory(pid int) uint64 { return 0 } // 占位：Phase 6 用 PSAPI
func (windowsAdapter) AvailableSystemMemory() uint64      { return 0 } // 占位：Phase 8

func (windowsAdapter) AppDataDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = filepath.Join(os.Getenv("USERPROFILE"), `AppData\Local`)
	}
	return filepath.Join(base, "stardust", "browser")
}

func (windowsAdapter) CacheDir() string {
	return filepath.Join(windowsAdapter{}.AppDataDir(), "cache")
}

func (windowsAdapter) ToNativePath(posix string) string {
	return strings.ReplaceAll(posix, "/", `\`)
}

// SafeDelete 处理 Windows 强制文件锁：占用中不可删，短暂重试；不存在视为成功（幂等）。
func (windowsAdapter) SafeDelete(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	var lastErr error
	for i := 0; i < 5; i++ {
		if err := os.RemoveAll(path); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond) // 等占用句柄释放
	}
	return fmt.Errorf("safe delete %q after retries: %w", path, lastErr)
}

func (windowsAdapter) WrapWithSandbox(cmd *exec.Cmd) *exec.Cmd {
	return cmd // 占位：Phase 5 接 AppContainer + Job Object
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
