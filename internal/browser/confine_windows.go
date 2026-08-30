//go:build windows

package browser

import (
	"fmt"
	"io"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobConfinement 是一个 Job Object 加上被放进去的那个进程。
//
// Job Object 是 Windows 上现成的「一组进程的边界」：本期用它做两件事。
//
//  1. **KILL_ON_JOB_CLOSE**：句柄关闭（含 agent 进程崩溃）时，Job 里的所有进程连同
//     它们后来 fork 的子进程一起被杀。Chromium 是多进程的，杀掉主进程并不会带走
//     renderer/GPU 进程——此前 agent 崩一次就在机器上留下一串孤儿 Chromium，直到
//     用户自己去任务管理器里清。
//  2. **内存上限**：整个 job 的提交内存越界即失败分配，把一个失控页面的代价挡在
//     这台机器的其它进程之外。
//
// 它**不是**AppContainer：AppContainer 要在**创建进程时**指定安全能力，而
// Chromium 的进程是 go-rod 的 launcher 起的，我们只拿得到 pid。二者不冲突，等
// launch 路径自管（Phase 6 进程池）之后可以叠加。这一点写在这里，免得后来的人
// 以为「已经有 AppContainer 了」。
type jobConfinement struct {
	job windows.Handle
}

// jobMemoryLimitBytes 是一个 job 的提交内存上限。
//
// 2 GiB 是按「一个浏览器会话的量级」定的：正常浏览多在几百 MB，越过 2 GiB 的多半
// 是一个失控的页面，而这台机器上还有别的进程要活。它不是配置项——先有一个真实的
// 上限，再谈把它做成可调。
const jobMemoryLimitBytes = 2 << 30

// Close 关闭 job 句柄，从而（按 KILL_ON_JOB_CLOSE）终止 job 里的全部进程。
func (c *jobConfinement) Close() error {
	if c.job == 0 {
		return nil
	}
	handle := c.job
	c.job = 0
	if err := windows.CloseHandle(handle); err != nil {
		return fmt.Errorf("close the browser job object: %w", err)
	}
	return nil
}

// ConfineProcess 建一个 Job Object、设好限制、把 pid 指的进程放进去。
//
// 失败一律返回错误：一个「建好了 job 但没放进去」的中间状态，看起来像收束成功而
// 实际什么也没约束，正是这个接缝要消除的东西。
func (windowsAdapter) ConfineProcess(pid int) (io.Closer, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create a job object for browser pid %d: %w", pid, err)
	}
	confinement := &jobConfinement{job: job}

	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
				windows.JOB_OBJECT_LIMIT_JOB_MEMORY,
		},
		JobMemoryLimit: jobMemoryLimitBytes,
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		_ = confinement.Close()
		return nil, fmt.Errorf("set limits on the browser job object: %w", err)
	}

	// PROCESS_SET_QUOTA + PROCESS_TERMINATE 是 AssignProcessToJobObject 要求的最小
	// 权限集合；多要一个权限就是多一份这个句柄被误用的可能。
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(pid),
	)
	if err != nil {
		_ = confinement.Close()
		return nil, fmt.Errorf("open browser process %d to confine it: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(process) }()

	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = confinement.Close()
		return nil, fmt.Errorf("assign browser process %d to its job object: %w", pid, err)
	}
	return confinement, nil
}
