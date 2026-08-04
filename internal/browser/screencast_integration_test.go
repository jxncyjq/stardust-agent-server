//go:build chromium

package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestScreencastEmitsFrames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><h1>hi</h1><script>setInterval(()=>document.body.style.background=Math.random()>0.5?'#fff':'#000',100)</script></body></html>`))
	}))
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	open, err := rt.Open(context.Background(), OpenReq{URL: srv.URL, TaskID: "sc"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ch, cancel, err := rt.Subscribe(open.SessionID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	// 订阅后应在 2s 内收到至少一个 frame 事件。
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == EventFrame {
				return // 成功
			}
		case <-deadline:
			t.Fatal("no frame event within 2s of subscribing")
		}
	}
}

// systemChromeForTest 返回本机可用 Chrome 路径（go-rod 自动下载在部分 Windows 环境损坏）。
func systemChromeForTest() string {
	return `C:\Program Files\Google\Chrome\Application\chrome.exe`
}
