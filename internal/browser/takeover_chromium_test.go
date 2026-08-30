//go:build chromium

package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestInjectInputAppliesModifiers 是修饰键契约的真机证据：注入 ctrl+c 之后，页面
// 看到的必须是「按下 c 且 ctrlKey 为真」，而不是一个字母 c 被打进输入框。
//
// 之前这条路根本走不通：`keydown Control` 被键名白名单拒掉，GUI 随后把 c 当普通
// 字符发出去，于是「复制」变成了「输入 c」。
func TestInjectInputAppliesModifiers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><input id="box" autofocus>
			<script>
			window.seen = [];
			addEventListener('keydown', e => { window.seen.push(e.key + ':ctrl=' + e.ctrlKey + ':shift=' + e.shiftKey); });
			</script></body></html>`))
	})
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
	if err := rt.InjectInput(sid, []InputEvent{
		{Type: "keydown", Key: "c", Modifiers: []string{"ctrl"}},
		{Type: "keyup", Key: "c", Modifiers: []string{"ctrl"}},
	}); err != nil {
		t.Fatalf("InjectInput(ctrl+c): %v", err)
	}

	_, page, err := rt.activePage(sid)
	if err != nil {
		t.Fatalf("activePage: %v", err)
	}
	seenJSON := func() string {
		t.Helper()
		res, err := page.Eval("() => JSON.stringify(window.seen)")
		if err != nil {
			t.Fatalf("eval window.seen: %v", err)
		}
		return res.Value.String()
	}
	pollUntil(t, 5*time.Second, func() bool {
		return strings.Contains(seenJSON(), "c:ctrl=true:shift=false")
	})

	// The focused input must still be empty: ctrl+c is a command, not text.
	// This is the defect stated as an assertion — the old path turned a copy
	// into a literal "c" in the box. Checked BEFORE anything else is typed, so
	// the box can only hold what ctrl+c put there.
	boxValue := func() string {
		t.Helper()
		value, err := page.Eval("() => document.getElementById('box').value")
		if err != nil {
			t.Fatalf("eval box value: %v", err)
		}
		return value.Value.String()
	}
	if got := boxValue(); got != "" {
		t.Errorf("input value = %q after ctrl+c, want empty: the shortcut was typed as text", got)
	}

	// And no modifier may be left stuck down: a later plain keystroke has to
	// arrive plain, and it types.
	if err := rt.InjectInput(sid, []InputEvent{{Type: "keydown", Key: "a"}}); err != nil {
		t.Fatalf("InjectInput(a): %v", err)
	}
	pollUntil(t, 5*time.Second, func() bool {
		return strings.Contains(seenJSON(), "a:ctrl=false:shift=false")
	})
	if got := boxValue(); got != "a" {
		t.Errorf("input value = %q after a plain keydown, want %q", got, "a")
	}
}
