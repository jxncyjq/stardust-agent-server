package browser

import (
	"errors"
	"io"
	"os/exec"
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

	// 资源
	//
	// 两者都返回 error 而不是「取不到就 0」：0 是一个**看起来正常的数字**，健康
	// 检查读到它会认为浏览器没占内存，于是「内存水位超阈就回收」这条策略永远不
	// 触发——一个失控的页面可以一直涨到把机器拖垮，而监控上是一条平的 0。
	SampleProcessMemory(pid int) (uint64, error)
	AvailableSystemMemory() (uint64, error)

	// 文件系统
	AppDataDir() string // XDG / ~/Library / %LOCALAPPDATA%
	CacheDir() string
	ToNativePath(posix string) string
	SafeDelete(path string) error // Windows 强制锁：先关句柄/重试

	// 隔离
	//
	// PrepareCommand 在进程**创建之前**改写它的启动方式：Linux 的
	// namespaces/seccomp、macOS 的 sandbox-exec 都只能在这一刻建立，事后按 pid 补
	// 不上。返回的 Cmd 就是接下来会被 Start 的那个（平台可以原样返回）。
	//
	// 它此前存在过（叫 WrapWithSandbox）却**没有调用方**——Chromium 的进程是
	// go-rod 的 launcher 起的，那个 Cmd 从来不存在。现在启动收回自管，它才真正
	// 处在路径上。
	PrepareCommand(cmd *exec.Cmd) *exec.Cmd

	// ConfineProcess 把一个**已经起来的**进程放进本平台的外层隔离里，返回释放它的
	// Closer。它与 PrepareCommand 是两个时刻：Windows 的 Job Object 只能事后按 pid
	// 加入（AssignProcessToJobObject），而 namespaces 只能事前。
	//
	// 平台没有实现时返回 ErrConfinementUnsupported，绝不假装成功。
	ConfineProcess(pid int) (io.Closer, error)
}

// NewPlatformAdapter 返回当前 OS 的实现（各 platform_{os}.go 提供 newPlatformAdapter）。
func NewPlatformAdapter() PlatformAdapter {
	return newPlatformAdapter()
}
