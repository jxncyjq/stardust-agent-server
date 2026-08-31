package browser

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
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
//
// 60 行而不是 20：Chromium 崩溃时先打一行 CHECK 失败、再打二三十行调用栈，20 行的
// 窗口只留下栈尾——**那句唯一有用的话正好被挤掉**。CI 上排查沙箱内启动失败时就是
// 这样看了一轮什么也没看到。
const maxRememberedOutputLines = 60

// launchSpec 是起一个 Chromium 需要的全部东西。
type launchSpec struct {
	Bin  string
	Args []string
	// Env 是这个进程的环境；空表示继承本进程的。macOS 的沙箱要靠它把 TMPDIR 指进
	// profile 目录——否则可写列表就得整片放行 /private/var/folders，那等于不设防。
	Env []string
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
	if len(spec.Env) > 0 {
		cmd.Env = spec.Env
	}
	// 自己开管道，而不是 cmd.StderrPipe()：那个管道的写端由 os/exec 在 Wait 里关，
	// 而我们在 Wait 之前就要读。父进程手里留着一个写端，子进程死了读端也不会 EOF
	// ——于是「浏览器秒退」会表现成「等满整个启动超时」。CI 上 bwrap 立刻失败、
	// 而我们干等 45 秒，就是这个。
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("pipe chromium stderr: %w", err)
	}
	// 读端的归属：成功时交给 drainOutput 那个 goroutine（它一直读到进程结束），
	// 失败时由这里关掉。不能用一个无条件的 defer——那会在成功路径上把 drainer 脚下
	// 的管道抽走，而症状是浏览器跑着跑着无声地卡住（见 output 字段的注释）。
	closeReader := true
	defer func() {
		if closeReader {
			_ = reader.Close()
		}
	}()
	cmd.Stderr = writer
	if spec.PAL != nil {
		prepared, err := spec.PAL.PrepareCommand(cmd)
		if err != nil {
			_ = writer.Close()
			return nil, err
		}
		cmd = prepared
	}
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("start chromium %s: %w", spec.Bin, err)
	}
	// 父进程立刻放掉写端：只有当**子进程**手里那一份也没了（它退出了），读端才会
	// EOF。留着它就是上面说的那个 45 秒。
	_ = writer.Close()

	browser := &launchedBrowser{cmd: cmd}
	url, err := readDevToolsURL(ctx, reader, browser)
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
	// 读端从这里起归 drainer 所有（它读到进程结束、管道 EOF 为止）。
	closeReader = false
	go func() {
		defer func() { _ = reader.Close() }()
		browser.drainOutput(reader)
	}()
	return browser, nil
}

// readDevToolsURL 从浏览器的 stderr 里读出它宣告的地址。
//
// 三种结束方式，各自说清楚：读到地址（成功）、流结束了也没有（带上浏览器自己说的
// 话）、ctx 到期（浏览器卡住了）。最后一种单独分出来，是因为「卡住」与「说了别的」
// 的排查方向完全不同。
//
// recent 非 nil 时把最近几行留在那里，供调用方在别处引用。
func readDevToolsURL(ctx context.Context, stderr io.Reader, sink *launchedBrowser) (string, error) {
	if sink == nil {
		sink = &launchedBrowser{}
	}
	type result struct {
		url string
		err error
	}
	done := make(chan result, 1)

	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			// 边读边存，而不是攒到最后再一起交出去：**超时那条路上读到的行归谁**，
			// 决定了运维能不能看见浏览器说了什么。此前超时只回一句 deadline
			// exceeded，而 Chromium 早已把原因写在 stderr 上——那正是自管启动要修的
			// 东西，却在超时这一支上又漏了一次。
			sink.appendOutput(line)
			if !strings.Contains(line, "DevTools listening on") {
				continue
			}
			if match := devToolsLine.FindString(line); match != "" {
				done <- result{url: match}
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = fmt.Errorf("the browser exited without announcing a DevTools address")
		}
		done <- result{err: err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return "", fmt.Errorf("read the browser's startup output: %w; it said:\n%s",
				r.err, strings.Join(sink.RecentOutput(), "\n"))
		}
		return r.url, nil
	case <-ctx.Done():
		said := strings.Join(sink.RecentOutput(), "\n")
		if said == "" {
			said = "(it printed nothing at all)"
		}
		return "", fmt.Errorf("waiting for the browser to announce its DevTools address: %w; it said:\n%s",
			ctx.Err(), said)
	}
}
