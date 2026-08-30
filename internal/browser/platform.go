package browser

import (
	"errors"
	"io"
)

// ErrConfinementUnsupported 表示这个平台目前没有外层沙箱实现。
//
// 它是一个**明说**而不是一次静默放行：部署可以据它决定「宁可不启动浏览器」
// （browser.require_sandbox），日志里也能看见「这台机器上的 Chromium 没有外层
// 隔离」。返回 (nil, nil) 装作收束成功，会让一个部署相信自己有一层并不存在的
// 隔离——在安全基线里，「被收束」与「以为自己被收束」的差别正是全部意义。
var ErrConfinementUnsupported = errors.New("this platform has no outer sandbox for the browser process")

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
	AppDataDir() string // XDG / ~/Library / %LOCALAPPDATA%
	CacheDir() string
	ToNativePath(posix string) string
	SafeDelete(path string) error // Windows 强制锁：先关句柄/重试

	// 隔离
	//
	// ConfineProcess 把一个**已经起来的**进程放进本平台的外层隔离里，返回释放它的
	// Closer。按 pid 而不是按 exec.Cmd，是因为 Chromium 的进程是 go-rod 的 launcher
	// 自己起的：此前那个接 *exec.Cmd 的 WrapWithSandbox 从来没有调用方，三个平台
	// 把它实现完，浏览器照样一点约束都没有。
	//
	// 平台没有实现时返回 ErrConfinementUnsupported，绝不假装成功。
	ConfineProcess(pid int) (io.Closer, error)
}

// NewPlatformAdapter 返回当前 OS 的实现（各 platform_{os}.go 提供 newPlatformAdapter）。
func NewPlatformAdapter() PlatformAdapter {
	return newPlatformAdapter()
}
