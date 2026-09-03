package server

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/observability"
)

// 一条 SSE 是长连接：它在**建立时**鉴权一次，然后可以挂着几个小时。于是一个被
// 吊销的 token 依然在替它的持有者接收事件——泄露一次凭证，泄露的是从此以后的
// 全部事件流，而运维手里没有任何除了重启进程之外的办法。
//
// 这些测试钉三件事：吊销之后旧 token 立刻不能再发起请求；**已经挂着的流被主动
// 断开**，并在断开前收到一条 `event: reauth`（客户端据此知道该拿新凭证重连，而不
// 是当成网络抖动无限重试）；以及吊销给出的新 token 立刻可用。

func newTokenTestServer(t *testing.T, token string) (*HTTPServer, *TokenStore) {
	t.Helper()

	tokens := NewTokenStore(token)
	srv := NewHTTPServer(Config{AdminToken: token, Tokens: tokens, PlatformEvents: observability.NewEventBus(8)})
	return srv, tokens
}

func TestARevokedTokenStopsWorkingImmediately(t *testing.T) {
	srv, tokens := newTokenTestServer(t, "old-token")

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer old-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("the token did not work before revocation: %s", rec.Body.String())
	}

	fresh := tokens.Rotate()

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer old-token")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status with the revoked token = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+fresh)
	srv.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Errorf("the freshly minted token was refused: %s", rec.Body.String())
	}
}

// TestAnOpenStreamIsToldToReauthenticateAndThenClosed: this is the whole point.
// A stream that keeps delivering events after its credential was revoked is the
// leak the revocation was meant to stop.
func TestAnOpenStreamIsToldToReauthenticateAndThenClosed(t *testing.T) {
	srv, tokens := newTokenTestServer(t, "old-token")

	listener := httptest.NewServer(srv)
	defer listener.Close()

	req, err := http.NewRequest(http.MethodGet, listener.URL+"/v1/events", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer old-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	tokens.Rotate()

	lines := make(chan string, 8)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	sawReauth := false
	deadline := time.After(5 * time.Second)
	for !sawReauth {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("the stream closed without ever sending event: reauth; the client cannot tell " +
					"a revoked credential from a dropped connection")
			}
			if strings.HasPrefix(line, "event: reauth") {
				sawReauth = true
			}
		case <-deadline:
			t.Fatal("no reauth within 5s: the stream outlived the credential that opened it")
		}
	}

	// And the stream must actually end — telling a client to reauthenticate
	// while continuing to feed it events would be theatre.
	for {
		select {
		case _, ok := <-lines:
			if !ok {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the stream is still open 5s after reauth was sent")
		}
	}
}

// rotateOnWriteHeader 在 handler 写响应头的**那一刻**同步轮换 token。
//
// 它把一个真实存在过的时序竞态变成确定性的：吊销的失效信号必须在写头之前就取到，
// 否则轮换关闭的是当前这一代 channel、handler 随后取到的是新那一代，那条流就再也
// 不会被断开——而不被断开的长连接正是这套机制要堵的漏。
//
// 这个竞态曾以「CI 上偶发的 5s 超时」的面目出现过两次（P2 与 P5 各一次），本地永远
// 复现不了，因为它取决于 handler 在 Flush 之后、取信号之前被调度器停留多久。
type rotateOnWriteHeader struct {
	rec    *httptest.ResponseRecorder
	tokens *TokenStore
	once   sync.Once
}

func (w *rotateOnWriteHeader) Header() http.Header { return w.rec.Header() }

func (w *rotateOnWriteHeader) Write(b []byte) (int, error) { return w.rec.Write(b) }

func (w *rotateOnWriteHeader) WriteHeader(code int) {
	// 恰在这一刻轮换：比 handler 里任何一行「写头之后」的代码都早。
	w.once.Do(func() { w.tokens.Rotate() })
	w.rec.WriteHeader(code)
}

func (w *rotateOnWriteHeader) Flush() { w.rec.Flush() }

// TestTheRevocationSignalIsTakenBeforeTheResponseHeadersGoOut 守的是取信号的**位置**，
// 不是某个超时窗口够不够宽。
//
// 修复前：`revoked := s.tokens.Changed()` 在 WriteHeader/Flush 之后，于是本测试构造的
// 那次轮换（发生在写头的瞬间）会被彻底错过，handler 挂在 select 上直到客户端断开——
// 一条被吊销的凭证继续喂着事件。
//
// 它不需要真 socket、不等墙钟：handler 收到失效信号后会写 reauth 并返回，所以
// ServeHTTP 自己就会结束。
func TestTheRevocationSignalIsTakenBeforeTheResponseHeadersGoOut(t *testing.T) {
	srv, tokens := newTokenTestServer(t, "old-token")

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("Authorization", "Bearer old-token")
	w := &rotateOnWriteHeader{rec: httptest.NewRecorder(), tokens: tokens}

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler 没有返回：写头那一刻的轮换被错过了，这条流不会被断开——" +
			"失效信号必须在写响应头**之前**取")
	}

	if body := w.rec.Body.String(); !strings.Contains(body, "event: reauth") {
		t.Errorf("响应里没有 reauth（写头那一刻发生的轮换必须仍能断开这条流）：%q", body)
	}
}

// TestRotationIsAvailableToAnOperatorOverHTTP: an in-process API alone would
// mean the only way to burn a leaked token is a restart, which drops every
// running task with it.
func TestRotationIsAvailableToAnOperatorOverHTTP(t *testing.T) {
	srv, _ := newTokenTestServer(t, "old-token")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/rotate", nil)
	req.Header.Set("Authorization", "Bearer old-token")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"token"`) {
		t.Errorf("rotate body = %q, want the new token so the caller can keep working", rec.Body.String())
	}

	// The token that authorized the rotation is now dead — including for
	// rotating again, which is what makes this a revocation rather than a
	// second key cut for the same lock.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/auth/rotate", nil)
	req.Header.Set("Authorization", "Bearer old-token")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("second rotate with the old token = %d, want 401", rec.Code)
	}
}

// TestADeploymentWithoutATokenIsUnchanged: an open local serve has nothing to
// revoke, and this endpoint must not become a way to lock it.
func TestADeploymentWithoutATokenIsUnchanged(t *testing.T) {
	srv := NewHTTPServer(Config{})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/auth/rotate", nil))
	if rec.Code != http.StatusConflict {
		t.Errorf("rotate on a token-less serve = %d, want 409: there is no credential to rotate", rec.Code)
	}
}
