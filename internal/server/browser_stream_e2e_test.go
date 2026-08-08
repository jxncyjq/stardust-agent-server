//go:build chromium

package server

import (
	"bufio"
	"context"
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

	// 订阅建立后触发一次动作，产生 observation+progress（+frame）
	go func() {
		time.Sleep(200 * time.Millisecond)
		var ref string
		read, _ := rt.Read(context.Background(), browser.ReadReq{SessionID: open.SessionID})
		for _, e := range read.Elements {
			if strings.Contains(e.Name, "按钮") {
				ref = e.Ref
			}
		}
		_, _ = rt.Click(context.Background(), browser.ClickReq{SessionID: open.SessionID, Ref: ref})
	}()

	sc := bufio.NewScanner(resp.Body)
	seenProgress, seenObs, seenFrame := false, false, false
	deadline := time.Now().Add(4 * time.Second)
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: progress"):
			seenProgress = true
		case strings.HasPrefix(line, "event: observation"):
			seenObs = true
		case strings.HasPrefix(line, "event: frame"):
			seenFrame = true
		}
		if seenProgress && seenObs && seenFrame {
			break
		}
	}
	if !seenProgress || !seenObs || !seenFrame {
		t.Fatalf("missing events: progress=%v obs=%v frame=%v", seenProgress, seenObs, seenFrame)
	}
}

// systemChromeForTest 经 PAL 定位本机 Chromium/Chrome，跨 OS 可移植
// （go-rod 自动下载在部分 Windows 环境损坏）。返回 "" 时 go-rod 自动下载。
func systemChromeForTest() string {
	return browser.NewPlatformAdapter().ResolveChromiumPath()
}
