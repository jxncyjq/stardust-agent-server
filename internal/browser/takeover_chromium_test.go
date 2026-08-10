//go:build chromium

package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// pollUntil 轮询 fn 直到为真或超时，超时即 t.Fatal（fail-loud，不静默放过）。
func pollUntil(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !fn() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

// 集成测试用系统 Chrome（PAL 定位），进接管后注入 click，断言页面 onclick 触发。
func TestInjectInputClickFiresOnClick(t *testing.T) {
	var clicked int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>
			<button id="b" style="position:absolute;left:0;top:0;width:100%;height:100%"
			onclick="fetch('/hit')">X</button></body></html>`))
	})
	mux.HandleFunc("/hit", func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&clicked, 1) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	obs, err := rt.Open(context.Background(), OpenReq{URL: srv.URL})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sid := obs.SessionID
	if err := rt.SetTakeover(sid, true); err != nil {
		t.Fatalf("SetTakeover: %v", err)
	}
	// 按钮铺满视口，点中心必命中。
	if err := rt.InjectInput(sid, []InputEvent{{Type: "click", X: 0.5, Y: 0.5, Button: "left"}}); err != nil {
		t.Fatalf("InjectInput: %v", err)
	}
	// 轮询等 onclick 的 fetch 落地。
	pollUntil(t, 5*time.Second, func() bool { return atomic.LoadInt32(&clicked) == 1 })
}

func TestInjectInputRequiresTakeover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>blank</body></html>`))
	}))
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	obs, err := rt.Open(context.Background(), OpenReq{URL: srv.URL})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	err = rt.InjectInput(obs.SessionID, []InputEvent{{Type: "mousemove", X: 0.5, Y: 0.5}})
	var be *BrowserError
	if !asBrowserError(err, &be) || be.Code != CodeTakeover {
		t.Fatalf("inject without takeover should error CodeTakeover, got %v", err)
	}
}
