//go:build chromium

package server

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/browser"
)

func TestBrowserStreamE2EObservationProgressFrame(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// charset=utf-8 + <meta charset> 必需：否则 headless Chrome 按 legacy 编码
		// 误解码 aria-label 的 UTF-8 字节，a11y name 变 mojibake，Click 按名匹配落空。
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 按钮供 Read/Click 产生 observation+progress；setInterval 持续重绘背景，
		// 保证 headless Chrome 的 CDP screencast 持续产帧（静态页在 headless 下可能不产帧，
		// 与 Task 4 集成测试同一手法）。
		_, _ = w.Write([]byte(`<html><head><meta charset="utf-8"></head><body><button aria-label="按钮">click</button>` +
			`<script>setInterval(()=>document.body.style.background=Math.random()>0.5?'#fff':'#000',100)</script>` +
			`</body></html>`))
	}))
	defer target.Close()

	rt, err := browser.NewRuntime(browser.RuntimeConfig{
		Headless: true, AllowPrivateHosts: true,
		BinPath: systemChromeForTest(),
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), browser.CloseReq{})

	open, err := rt.Open(context.Background(), browser.OpenReq{URL: target.URL, TaskID: "e2e"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	srv := &HTTPServer{browser: rt} // rt 满足 BrowserStreamer
	ts := httptest.NewServer(http.HandlerFunc(srv.handleBrowserStream))
	defer ts.Close()

	// 订阅 SSE
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/browser/sessions/"+open.SessionID+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()

	// 触发一次动作，产生 observation+progress（+frame）。
	//
	// 这里**不 sleep 等订阅建立**：handleBrowserStream 是先 Subscribe 再写响应头的，
	// 所以上面 Do() 返回的那一刻订阅已经在了。原先那个 200ms 的 sleep 既不必要，也
	// 掩盖了下面这件事——
	//
	// 动作的两个错误以前都被丢弃（`read, _ :=` / `_, _ =`）。Read 失败或页面里找不到
	// 那个按钮时，ref 是空串，Click 必然失败，于是永远不会有 progress；而测试只会报
	// 一句 "missing events: progress=false"，看不出真正的原因。这个 e2e 在 CI 的
	// Windows runner 上有过约 40% 的红（近 5 次红 3 次，重跑即绿，症状正是
	// progress=false frame=false），而丢掉的这两个错误正是它一直查不下去的原因。
	//
	// 现在错误经 channel 回到主 goroutine：测试仍然可能因为超时而失败，但失败信息会
	// 说清是「动作本身失败了」还是「动作成功了但帧没到」——这是两种完全不同的病。
	actionErr := make(chan error, 1)
	go func() {
		read, err := rt.Read(context.Background(), browser.ReadReq{SessionID: open.SessionID})
		if err != nil {
			actionErr <- fmt.Errorf("Read: %w", err)
			return
		}
		var ref string
		for _, e := range read.Elements {
			if strings.Contains(e.Name, "按钮") {
				ref = e.Ref
			}
		}
		if ref == "" {
			names := make([]string, 0, len(read.Elements))
			for _, e := range read.Elements {
				names = append(names, e.Name)
			}
			actionErr <- fmt.Errorf("页面里没有名字含「按钮」的元素，读到的是 %q；"+
				"没有 ref 就点不动，progress 事件永远不会来", names)
			return
		}
		if _, err := rt.Click(context.Background(), browser.ClickReq{
			SessionID: open.SessionID, Ref: ref}); err != nil {
			actionErr <- fmt.Errorf("Click(ref=%s): %w", ref, err)
			return
		}
		actionErr <- nil
	}()

	// 扫描放到自己的 goroutine 里：bufio.Scanner.Scan() 是阻塞的，原先写成
	// `for sc.Scan() && time.Now().Before(deadline)` 时，那个 deadline 只在**每次
	// Scan 返回之后**才被检查——流里迟迟没有新行时它根本不起作用，测试会一直挂到
	// go test 的整体超时，而不是在 4 秒后给出诊断。
	type seen struct{ progress, obs, frame bool }
	done := make(chan seen, 1)
	go func() {
		var s seen
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: progress"):
				s.progress = true
			case strings.HasPrefix(line, "event: observation"):
				s.obs = true
			case strings.HasPrefix(line, "event: frame"):
				s.frame = true
			}
			if s.progress && s.obs && s.frame {
				break
			}
		}
		done <- s
	}()

	var got seen
	select {
	case got = <-done:
	case <-time.After(streamE2EDeadline):
		// 超时了。先看动作本身是不是早就失败了——那才是根因，事件没来只是后果。
		select {
		case err := <-actionErr:
			if err != nil {
				t.Fatalf("%.0fs 内没等齐事件，而触发动作本身就失败了：%v",
					streamE2EDeadline.Seconds(), err)
			}
			t.Fatalf("%.0fs 内没等齐事件；触发动作**成功**了，所以问题在事件通道或产帧一侧，"+
				"不在动作上", streamE2EDeadline.Seconds())
		default:
			t.Fatalf("%.0fs 内没等齐事件，且触发动作到此刻还没返回——"+
				"Read/Click 卡住了，不是帧慢", streamE2EDeadline.Seconds())
		}
	}

	// 事件齐了，动作也必须是成功的：ref 找不到时 Click 会失败，而 observation/frame
	// 仍可能因为页面自身的重绘而到达，光看事件会漏掉这种「点空了」。
	select {
	case err := <-actionErr:
		if err != nil {
			t.Fatalf("事件收齐了，但触发动作报错：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("事件收齐了，但触发动作迟迟没有返回")
	}

	if !got.progress || !got.obs || !got.frame {
		t.Fatalf("missing events: progress=%v obs=%v frame=%v", got.progress, got.obs, got.frame)
	}
}

// streamE2EDeadline 是等齐三种事件的窗口。
//
// 本机（Windows + 系统 Chrome）跑满 5 次都在 1.4–1.6s 完成，所以 8s 不是「把超时调大
// 遮住问题」，而是给 CI 上更慢的 runner 留出余量——真正的诊断力来自上面那些能说出
// 「动作失败了」还是「动作成功但帧没到」的分支，不是这个数字。
const streamE2EDeadline = 8 * time.Second

// systemChromeForTest 经 PAL 定位本机 Chromium/Chrome，跨 OS 可移植
// （go-rod 自动下载在部分 Windows 环境损坏）。返回 "" 时 go-rod 自动下载。
func systemChromeForTest() string {
	return browser.NewPlatformAdapter().ResolveChromiumPath()
}
