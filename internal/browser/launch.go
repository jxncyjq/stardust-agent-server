package browser

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

// devToolsLine 匹配 Chromium 启动时写在 stderr 上的那一行：
//
//	DevTools listening on ws://127.0.0.1:51234/devtools/browser/9a1f-4c2e
//
// 这是**唯一**能知道它监听在哪的途径（端口给 0 让系统分配，正是为了不撞端口）。
var devToolsLine = regexp.MustCompile(`ws://[^\s]+`)

// maxRememberedOutputLines 是启动失败时回带多少行浏览器自己的输出。
//
// 存在的理由是一次真实的排查体验：go-rod 的 launcher 把 stderr 吃掉，启动失败时
// 运维只看到「launch chromium: context deadline exceeded」，而 Chromium 其实已经把
// 原因写得很清楚（缺库、沙箱起不来、用户目录被别的进程占着）。
const maxRememberedOutputLines = 20

// launchSpec 是起一个 Chromium 需要的全部东西。
type launchSpec struct {
	Bin  string
	Args []string
	// PAL 在**进程创建之前**拿到 Cmd。外层沙箱（Linux namespaces+seccomp、
	// macOS sandbox-exec）只能在这一刻建立——这正是把启动从 go-rod 手里收回来的
	// 理由：此前 PAL 有这个钩子却没有调用方，因为没有一个我们自己的 Cmd。
	PAL PlatformAdapter
}

// launchedBrowser 是一个我们自己起的、自己看着的 Chromium。
type launchedBrowser struct {
	cmd        *exec.Cmd
	controlURL string

	// mu 守 output：启动之后仍有一个 goroutine 在读 stderr（见 drainOutput），
	// 而诊断可能从别的 goroutine 来读。
	mu sync.Mutex
	// output 是最近几行 stderr。它有两个用途，第二个才是必须的：
	//
	//  1. 启动失败时回带浏览器自己说的话；
	//  2. **让 stderr 一直有人读**。管道的缓冲只有几十 KB，Chromium 是个话很多的
	//     程序（每次 GPU/字体/证书告警都写一行）；宣告完地址就不再读，它迟早会在
	//     写 stderr 时阻塞，而症状是整个浏览器无声地卡住。
	output []string

	waitOnce sync.Once
	waitErr  error
}

// RecentOutput 返回浏览器最近写的几行 stderr，供诊断（崩溃前它通常会说点什么）。
func (b *launchedBrowser) RecentOutput() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.output...)
}

// appendOutput 把一行记进环形缓冲。
func (b *launchedBrowser) appendOutput(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.output = append(b.output, line)
	if len(b.output) > maxRememberedOutputLines {
		b.output = b.output[1:]
	}
}

// drainOutput 在启动之后继续读 stderr 直到进程结束。
//
// 它不是可选的：不读就会把 Chromium 卡在一次 write 上（见 output 字段的注释）。
func (b *launchedBrowser) drainOutput(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		b.appendOutput(scanner.Text())
	}
}

// PID 是浏览器主进程的 pid（收束与内存采样按它走）。
func (b *launchedBrowser) PID() int {
	if b.cmd == nil || b.cmd.Process == nil {
		return 0
	}
	return b.cmd.Process.Pid
}

// Wait 等进程退出。多次调用安全：Manager 的关闭路径与 reaper 都可能走到。
func (b *launchedBrowser) Wait() error {
	b.waitOnce.Do(func() { b.waitErr = b.cmd.Wait() })
	return b.waitErr
}

// launchChromium 起一个 Chromium 并返回它宣告的 DevTools 地址。
//
// 与 go-rod 的 launcher.Launch 的差别只有一处，但那一处是这次重构的全部理由：
// **Cmd 是我们建的**，于是它在 Start 之前经过 PAL，之后又能被按 pid 收束、按 pid
// 采样、由进程池决定生死。
func launchChromium(ctx context.Context, spec launchSpec) (*launchedBrowser, error) {
	cmd := exec.Command(spec.Bin, spec.Args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe chromium stderr: %w", err)
	}
	if spec.PAL != nil {
		prepared, err := spec.PAL.PrepareCommand(cmd)
		if err != nil {
			return nil, err
		}
		cmd = prepared
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start chromium %s: %w", spec.Bin, err)
	}

	browser := &launchedBrowser{cmd: cmd}
	var startupLines []string
	url, err := readDevToolsURL(ctx, stderr, &startupLines)
	for _, line := range startupLines {
		browser.appendOutput(line)
	}
	if err != nil {
		// 起来了但没宣告地址 = 一个我们连不上、又还在跑的进程。杀掉它，否则每
		// 一次失败的启动都在机器上留一个。
		if killErr := cmd.Process.Kill(); killErr != nil {
			err = fmt.Errorf("%w (and killing it failed: %v)", err, killErr)
		}
		_ = browser.Wait()
		return nil, err
	}
	browser.controlURL = url
	// 接着读：宣告完地址就撒手不管，Chromium 会在几十 KB 之后卡在写 stderr 上。
	go browser.drainOutput(stderr)
	return browser, nil
}

// readDevToolsURL 从浏览器的 stderr 里读出它宣告的地址。
//
// 三种结束方式，各自说清楚：读到地址（成功）、流结束了也没有（带上浏览器自己说的
// 话）、ctx 到期（浏览器卡住了）。最后一种单独分出来，是因为「卡住」与「说了别的」
// 的排查方向完全不同。
//
// recent 非 nil 时把最近几行留在那里，供调用方在别处引用。
func readDevToolsURL(ctx context.Context, stderr io.Reader, recent *[]string) (string, error) {
	type result struct {
		url   string
		lines []string
		err   error
	}
	done := make(chan result, 1)

	go func() {
		var lines []string
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			lines = append(lines, line)
			if len(lines) > maxRememberedOutputLines {
				lines = lines[1:]
			}
			if !strings.Contains(line, "DevTools listening on") {
				continue
			}
			if match := devToolsLine.FindString(line); match != "" {
				done <- result{url: match, lines: lines}
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = fmt.Errorf("the browser exited without announcing a DevTools address")
		}
		done <- result{lines: lines, err: err}
	}()

	select {
	case r := <-done:
		if recent != nil {
			*recent = r.lines
		}
		if r.err != nil {
			return "", fmt.Errorf("read the browser's startup output: %w; it said:\n%s",
				r.err, strings.Join(r.lines, "\n"))
		}
		return r.url, nil
	case <-ctx.Done():
		return "", fmt.Errorf("waiting for the browser to announce its DevTools address: %w", ctx.Err())
	}
}
