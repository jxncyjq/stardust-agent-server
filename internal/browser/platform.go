package browser

import "os/exec"

// PlatformAdapter 收敛所有 OS 差异（spec §11.2）。除本文件与 platform_{os}.go 外，
// browser 包禁止出现 runtime.GOOS 分支。
type PlatformAdapter interface {
	// 进程
	ResolveChromiumPath() string              // 定位可执行文件（分发优先级见 chromium_dist.go）
	DefaultLaunchArgs() []string              // 平台相关启动参数
	KillProcess(pid int, graceful bool) error // 信号 vs TerminateProcess

	// 资源（本期 best-effort 占位，Phase 6/8 精化）
	SampleProcessMemory(pid int) uint64
	AvailableSystemMemory() uint64

	// 文件系统
	AppDataDir() string           // XDG / ~/Library / %LOCALAPPDATA%
	CacheDir() string
	ToNativePath(posix string) string
	SafeDelete(path string) error // Windows 强制锁：先关句柄/重试

	// 隔离（本期文档化占位——透传；真实沙箱 = Phase 5）
	WrapWithSandbox(cmd *exec.Cmd) *exec.Cmd
}

// NewPlatformAdapter 返回当前 OS 的实现（各 platform_{os}.go 提供 newPlatformAdapter）。
func NewPlatformAdapter() PlatformAdapter {
	return newPlatformAdapter()
}
