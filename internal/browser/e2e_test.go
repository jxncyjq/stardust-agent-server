//go:build chromium

package browser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestE2EOpenReadTypeClick 走完最小闭环：打开一个带表单的页，读到 ref，输入，点提交。
func TestE2EOpenReadTypeClick(t *testing.T) {
	var submitted bool
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body>
			<input aria-label="关键词框">
			<button onclick="fetch('/submit')">搜索</button>
		</body></html>`))
	})
	mux.HandleFunc("/submit", func(w http.ResponseWriter, _ *http.Request) { submitted = true })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})
	ctx := context.Background()

	open, err := rt.Open(ctx, OpenReq{URL: srv.URL, TaskID: "e2e"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sid := open.SessionID

	read, err := rt.Read(ctx, ReadReq{SessionID: sid})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var kwRef, btnRef string
	for _, e := range read.Elements {
		if strings.Contains(e.Name, "关键词") {
			kwRef = e.Ref
		}
		if strings.Contains(e.Name, "搜索") {
			btnRef = e.Ref
		}
	}
	if kwRef == "" || btnRef == "" {
		t.Fatalf("refs missing: kw=%q btn=%q in %q", kwRef, btnRef, read.Text)
	}

	if _, err := rt.Type(ctx, TypeReq{SessionID: sid, Ref: kwRef, Text: "legion"}); err != nil {
		t.Fatalf("Type: %v", err)
	}
	if _, err := rt.Click(ctx, ClickReq{SessionID: sid, Ref: btnRef}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	// 给 fetch 一点时间落地
	if err := waitFlag(&submitted); err != nil {
		t.Fatalf("submit not observed: %v", err)
	}
}

// waitFlag 轮询等待布尔标志变为 true，最多约 2s（每 10ms 一跳）。
// 用轮询而非固定 sleep（spec §5.1 反对裸 sleep）。属测试内自由函数，
// 不挂在 *Runtime 上以免污染生产类型。
func waitFlag(flag *bool) error {
	for i := 0; i < 200; i++ {
		if *flag {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if *flag {
		return nil
	}
	return errors.New("submit timeout")
}
