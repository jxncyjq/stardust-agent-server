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

// TestE2EOpenReusesBrowserSessionPerChat 验证同一 chat session 内、SessionID 为空的
// 多次 Open 复用同一个浏览器会话（id 不自增），不同 chat session 得到不同会话，且复用会
// 命中同一个正被接管的会话（接管态跨消息延续 → Agent 的 Open 被门控拒绝）。
func TestE2EOpenReusesBrowserSessionPerChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body>ok</body></html>`))
	}))
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})
	ctx := context.Background()

	// 同一 chat session，SessionID 为空，两次 Open → 同一浏览器会话（复用，不自增）。
	o1, err := rt.Open(ctx, OpenReq{URL: srv.URL, TaskID: "t1", ChatSessionID: "chat-1"})
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	o2, err := rt.Open(ctx, OpenReq{URL: srv.URL, TaskID: "t2", ChatSessionID: "chat-1"})
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	if o1.SessionID != o2.SessionID {
		t.Fatalf("same chat session got different browser sessions %q vs %q (not reused)", o1.SessionID, o2.SessionID)
	}

	// 不同 chat session → 不同浏览器会话。
	o3, err := rt.Open(ctx, OpenReq{URL: srv.URL, TaskID: "t3", ChatSessionID: "chat-2"})
	if err != nil {
		t.Fatalf("Open 3: %v", err)
	}
	if o3.SessionID == o1.SessionID {
		t.Fatalf("different chat sessions share a browser session %q", o3.SessionID)
	}

	// 接管 chat-1 的会话后，同 chat session 再 Open（Agent 写动作）应被门控拒绝——
	// 证明复用命中了那个被接管的会话（接管态延续）。
	if err := rt.SetTakeover(o1.SessionID, true); err != nil {
		t.Fatalf("SetTakeover: %v", err)
	}
	_, err = rt.Open(ctx, OpenReq{URL: srv.URL, TaskID: "t4", ChatSessionID: "chat-1"})
	var be *BrowserError
	if !errors.As(err, &be) || be.Code != CodeTakeover {
		t.Fatalf("Open on taken-over reused session: err = %v, want CodeTakeover", err)
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
