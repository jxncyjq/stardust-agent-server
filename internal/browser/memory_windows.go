//go:build windows

package browser

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processMemoryCountersEx 是 PSAPI 的 PROCESS_MEMORY_COUNTERS_EX。
//
// x/sys/windows 没有导出它，所以这里照 Win32 头文件写一份。字段顺序与类型就是
// ABI 本身：错一个字段，读到的是相邻字段的值——一个**看起来合理**的错数字，比读
// 失败更难发现。
type processMemoryCountersEx struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

var (
	modpsapi                     = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo     = modpsapi.NewProc("GetProcessMemoryInfo")
	modkernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx     = modkernel32.NewProc("GlobalMemoryStatusEx")
	_                        any = procGlobalMemoryStatusEx
)

// memoryStatusEx 是 kernel32 的 MEMORYSTATUSEX。
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// SampleProcessMemory 读一个进程的 PrivateUsage（提交的私有字节）。
//
// 取 PrivateUsage 而不是 WorkingSetSize：后者是**当前在物理内存里**的部分，系统
// 一有压力就会把它压下去，于是「内存涨上来了」这件事在监控上表现为「内存降下去
// 了」。要判断一个页面是不是在失控，看的是它承诺占用了多少。
func (windowsAdapter) SampleProcessMemory(pid int) (uint64, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, fmt.Errorf("open process %d to sample its memory: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	counters := processMemoryCountersEx{}
	counters.CB = uint32(unsafe.Sizeof(counters))
	ret, _, callErr := procGetProcessMemoryInfo.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&counters)),
		uintptr(counters.CB),
	)
	if ret == 0 {
		return 0, fmt.Errorf("GetProcessMemoryInfo for process %d: %w", pid, callErr)
	}
	return uint64(counters.PrivateUsage), nil
}

// AvailableSystemMemory 读整机可用物理内存。
//
// 它是「还能不能再开一个浏览器」这个决定的输入。返回 0 表示不可知时，那个决定会
// 变成「永远可以再开一个」，所以这里读失败就报错。
func (windowsAdapter) AvailableSystemMemory() (uint64, error) {
	status := memoryStatusEx{}
	status.Length = uint32(unsafe.Sizeof(status))
	ret, _, callErr := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if ret == 0 {
		return 0, fmt.Errorf("GlobalMemoryStatusEx: %w", callErr)
	}
	return status.AvailPhys, nil
}
