//go:build chromium

package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body>
			<input id="kw" aria-label="关键词框">
			<button id="go">搜索</button>
		</body></html>`))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestRuntimeOpenReadType(t *testing.T) {
	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	srv := newTestServer(t)
	open, err := rt.Open(context.Background(), OpenReq{URL: srv.URL, TaskID: "t1"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if open.SessionID == "" {
		t.Fatal("Open returned empty SessionID")
	}
	if !strings.Contains(open.Observation.Text, "搜索") {
		t.Fatalf("observation missing button: %q", open.Observation.Text)
	}

	// 找到关键词框 ref 并输入
	var kwRef string
	for _, e := range open.Observation.Elements {
		if strings.Contains(e.Name, "关键词") {
			kwRef = e.Ref
		}
	}
	if kwRef == "" {
		t.Fatal("no ref for keyword box")
	}
	if _, err := rt.Type(context.Background(), TypeReq{SessionID: open.SessionID, Ref: kwRef, Text: "hello"}); err != nil {
		t.Fatalf("Type: %v", err)
	}

	// 读取当前页面（回归 Read 路径）。
	if _, err := rt.Read(context.Background(), ReadReq{SessionID: open.SessionID}); err != nil {
		t.Fatalf("Read: %v", err)
	}
}

func TestRuntimeOpenBlocksPrivateHost(t *testing.T) {
	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: false})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})
	_, err = rt.Open(context.Background(), OpenReq{URL: "http://127.0.0.1:1/", TaskID: "t1"})
	if err == nil {
		t.Fatal("expected private host to be blocked")
	}
}
