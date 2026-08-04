//go:build chromium

package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCaptureRestoreCookiesRoundTrip 用系统 Chrome 打开一个下发 cookie 的本地页，
// 断言 captureCookies 能抓到该 cookie（真 CDP 往返，验证 go-rod GetCookies 适配正确）。
func TestCaptureRestoreCookiesRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "phase3", Path: "/"})
		_, _ = w.Write([]byte(`<html><body>ok</body></html>`))
	}))
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer func() { _ = rt.Close(context.Background(), CloseReq{}) }()

	open, err := rt.Open(context.Background(), OpenReq{URL: srv.URL, TaskID: "t"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sess, ok := rt.sessions.Get(open.SessionID)
	if !ok {
		t.Fatalf("session %q not found", open.SessionID)
	}
	cookies, err := rt.captureCookies(sess)
	if err != nil {
		t.Fatalf("captureCookies: %v", err)
	}
	var found bool
	for _, c := range cookies {
		if c.Name == "sid" && c.Value == "phase3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("captured cookies missing sid=phase3: %+v", cookies)
	}
}
