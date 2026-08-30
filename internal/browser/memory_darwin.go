//go:build darwin

package browser

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// SampleProcessMemory 用 ps 读 RSS（单位 KiB）。
//
// 不用 task_info：拿别的进程的 task port 在现代 macOS 上需要 taskgated 授权
// （com.apple.security.get-task-allow / 调试签名），一个终端里跑的 agent 拿不到，
// 于是那条路在真机上只会稳定失败。ps 走的是内核统计接口，不需要任何授权，代价是
// 一次 fork。采样发生在健康检查的节拍上（秒级），这个代价可以接受。
//
// 读不到就报错：把「这个进程不存在」答成 0，健康检查会把一个已经死掉的浏览器读成
// 「内存占用健康」。
func (darwinAdapter) SampleProcessMemory(pid int) (uint64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, fmt.Errorf("ps -o rss for process %d: %w", pid, err)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		// ps 对不存在的 pid 以退出码 1 返回，但空输出同样意味着「没有这个进程」。
		return 0, fmt.Errorf("no such process %d", pid)
	}
	kb, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse ps rss output %q for process %d: %w", text, pid, err)
	}
	return kb * 1024, nil
}

// AvailableSystemMemory 用 vm_stat 的口径估算可用物理内存：空闲页 + 可回收页
// （inactive + purgeable）。
//
// 只算 free 会得出「内存永远不够」——macOS 会尽量把物理内存用满作缓存，free 在一
// 台正常工作的机器上一直很小。
func (darwinAdapter) AvailableSystemMemory() (uint64, error) {
	pageSize := uint64(unix.Getpagesize())
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, fmt.Errorf("vm_stat: %w", err)
	}
	wanted := map[string]uint64{
		"Pages free":                   0,
		"Pages inactive":               0,
		"Pages purgeable":              0,
		"File-backed pages":            0,
		"Pages speculative":            0,
		"Pages occupied by compressor": 0,
	}
	seen := 0
	for _, line := range strings.Split(string(out), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if _, want := wanted[name]; !want {
			continue
		}
		digits := strings.TrimSuffix(strings.TrimSpace(value), ".")
		pages, err := strconv.ParseUint(digits, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse vm_stat line %q: %w", line, err)
		}
		wanted[name] = pages
		seen++
	}
	if seen == 0 {
		return 0, fmt.Errorf("vm_stat produced no recognizable counters")
	}
	pages := wanted["Pages free"] + wanted["Pages inactive"] + wanted["Pages purgeable"] +
		wanted["Pages speculative"]
	return pages * pageSize, nil
}
