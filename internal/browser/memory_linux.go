//go:build linux

package browser

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SampleProcessMemory 读 /proc/<pid>/status 的 VmRSS。
//
// 取 VmRSS 而不是 VmSize：VmSize 是**保留的地址空间**，Chromium 每个进程都会保留
// 几十 GB 的虚拟地址（sanitizer/沙箱/GPU 映射），拿它做水位判断会让每一个健康的
// 浏览器看起来都在爆炸。RSS 是真正占着的物理页。
//
// 读不到就报错：进程不在了、/proc 没挂上、字段格式变了，都不能答成 0——把「这个
// 进程不存在」答成 0，健康检查会把一个已经死掉的浏览器读成「内存占用健康」。
func (linuxAdapter) SampleProcessMemory(pid int) (uint64, error) {
	path := fmt.Sprintf("/proc/%d/status", pid)
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s to sample process memory: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		kb, err := parseProcKilobytes(line)
		if err != nil {
			return 0, fmt.Errorf("read VmRSS from %s: %w", path, err)
		}
		return kb * 1024, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	// 内核线程没有 VmRSS。浏览器进程一定有，所以这里的缺席意味着我们采样错了
	// 对象——那是一个要看见的错误，不是一个可以填 0 的空位。
	return 0, fmt.Errorf("%s has no VmRSS line", path)
}

// AvailableSystemMemory 读 /proc/meminfo 的 MemAvailable。
//
// MemAvailable 而不是 MemFree：MemFree 不含可回收的页缓存，在一台正常工作的机器
// 上它总是很小，据它决策会得出「内存永远不够」的结论。
func (linuxAdapter) AvailableSystemMemory() (uint64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("open /proc/meminfo: %w", err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		kb, err := parseProcKilobytes(line)
		if err != nil {
			return 0, fmt.Errorf("read MemAvailable from /proc/meminfo: %w", err)
		}
		return kb * 1024, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	return 0, fmt.Errorf("/proc/meminfo has no MemAvailable line")
}

// parseProcKilobytes 解析 "字段名:   12345 kB" 这一行里的数字（单位 kB）。
func parseProcKilobytes(line string) (uint64, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected line %q", line)
	}
	value, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", fields[1], err)
	}
	return value, nil
}
