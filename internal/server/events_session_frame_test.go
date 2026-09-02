package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/observability"
)

// 帧必须带 session_id 与 seq：前端靠 seq 连续性判断漏帧，漏了回到
// /v1/sessions/{id}/events 从断点补拉。seq 错了比漏帧更糟——前端会以为自己没漏。
//
// 这里断言的是**帧的内容**，不是「代码里有那一行」。
func TestASessionEventFrameCarriesTheSessionIDAndSeq(t *testing.T) {
	bus := observability.NewEventBus(8)
	srv := NewHTTPServer(Config{AdminToken: "token", PlatformEvents: bus})
	req := httptest.NewRequest(http.MethodGet, "/v1/events?type=session_event", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	go func() {
		time.Sleep(10 * time.Millisecond)
		if err := bus.Publish(context.Background(), observability.EventEnvelope{
			Type:      "session_event",
			SubjectID: "sess-1",
			Data: map[string]any{
				"session_id": "sess-1",
				"seq":        7,
				"event_type": "tool/call",
			},
		}); err != nil {
			t.Errorf("Publish(session_event) error = %v", err)
		}
		if err := bus.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "event: session_event") {
		t.Fatalf("响应里没有 session_event 帧：%q", body)
	}
	for _, want := range []string{`"session_id":"sess-1"`, `"seq":7`, `"event_type":"tool/call"`} {
		if !strings.Contains(body, want) {
			t.Errorf("帧里缺 %s：前端靠 session_id+seq 判断漏帧\n完整响应：%s", want, body)
		}
	}
}

// ?type= 过滤是既有契约：只订阅 session_event 的客户端不该收到别的帧，
// 新增一类帧也不能把老客户端的过滤打乱。
func TestTypeFilterStillSelectsOnlyTheRequestedFrames(t *testing.T) {
	bus := observability.NewEventBus(8)
	srv := NewHTTPServer(Config{AdminToken: "token", PlatformEvents: bus})
	req := httptest.NewRequest(http.MethodGet, "/v1/events?type=session_event", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	go func() {
		time.Sleep(10 * time.Millisecond)
		if err := bus.Publish(context.Background(), observability.EventEnvelope{
			Type:      "session_event",
			SubjectID: "sess-1",
			Data:      map[string]any{"session_id": "sess-1", "seq": 0, "event_type": "turn/start"},
		}); err != nil {
			t.Errorf("Publish(session_event) error = %v", err)
		}
		if err := bus.Publish(context.Background(), observability.EventEnvelope{
			Type: "task.completed",
			Data: map[string]any{"task_id": "task-1"},
		}); err != nil {
			t.Errorf("Publish(task.completed) error = %v", err)
		}
		if err := bus.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "event: session_event") {
		t.Fatalf("订阅 session_event 却没收到它：%q", body)
	}
	if strings.Contains(body, "task.completed") {
		t.Errorf("?type=session_event 却收到了 task.completed：过滤被新帧打乱了\n%s", body)
	}
}

// 过滤的另一半，也是真正会被「让 session_event 绕开 ?type= 过滤」这种改动破坏的
// 那一半：订阅**别的**类型的客户端不该收到 session_event。
//
// 上一条用例（订阅 session_event、断言收不到 task.completed）看不见这个方向的
// 破坏——绕开过滤的 session_event 在那条用例里本来就是被订阅的类型。会话事件的数量
// 与工具轮数成正比，漏进一条只订阅 task.completed 的老流里，就是拿轨迹通知去淹一条
// 完全没要它的连接。
func TestASessionEventFrameDoesNotLeakIntoAnotherTypesSubscription(t *testing.T) {
	bus := observability.NewEventBus(8)
	srv := NewHTTPServer(Config{AdminToken: "token", PlatformEvents: bus})
	req := httptest.NewRequest(http.MethodGet, "/v1/events?type=task.completed", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	go func() {
		time.Sleep(10 * time.Millisecond)
		if err := bus.Publish(context.Background(), observability.EventEnvelope{
			Type:      "session_event",
			SubjectID: "sess-1",
			Data:      map[string]any{"session_id": "sess-1", "seq": 4, "event_type": "tool/call"},
		}); err != nil {
			t.Errorf("Publish(session_event) error = %v", err)
		}
		if err := bus.Publish(context.Background(), observability.EventEnvelope{
			Type: "task.completed",
			Data: map[string]any{"task_id": "task-1"},
		}); err != nil {
			t.Errorf("Publish(task.completed) error = %v", err)
		}
		if err := bus.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "event: task.completed") {
		t.Fatalf("订阅 task.completed 却没收到它，这条用例就什么也没证明：%q", body)
	}
	if strings.Contains(body, "session_event") || strings.Contains(body, "sess-1") {
		t.Errorf("?type=task.completed 的流里出现了 session_event：新帧绕开了 ?type= 过滤\n%s", body)
	}
}

// token 轮换要断流。新帧走的是同一个 select 循环，不能为它另起一条不看
// revoked 的路径——那等于给 SSE 开了个绕过鉴权失效的后门。
//
// 断言的是**流真的断了**，不是「代码里引用了 revoked」：先让一条 session_event
// 帧真的送到客户端（证明这条订阅是活的、而且它送的正是新帧），再轮换 token，然后
// 要求先收到 event: reauth、再收到流关闭。三段缺一不可——只断言「收到了 reauth」
// 的话，一条继续喂事件的流也能过；只断言「流关了」的话，任何提前失败（比如订阅
// 根本没建起来）也能过。搭法照 tokenrevoke_test.go 的
// TestAnOpenStreamIsToldToReauthenticateAndThenClosed。
func TestSessionEventFramesStopWhenTheTokenIsRotated(t *testing.T) {
	bus := observability.NewEventBus(8)
	tokens := NewTokenStore("old-token")
	srv := NewHTTPServer(Config{AdminToken: "old-token", Tokens: tokens, PlatformEvents: bus})

	listener := httptest.NewServer(srv)
	defer listener.Close()

	req, err := http.NewRequest(http.MethodGet, listener.URL+"/v1/events?type=session_event", nil)
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

	lines := make(chan string, 32)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	if err := bus.Publish(context.Background(), observability.EventEnvelope{
		Type:      "session_event",
		SubjectID: "sess-1",
		Data:      map[string]any{"session_id": "sess-1", "seq": 3, "event_type": "tool/result"},
	}); err != nil {
		t.Fatalf("Publish(session_event) error = %v", err)
	}

	awaitLine(t, lines, "event: session_event",
		"the session_event frame never reached the client, so this test would not be proving anything about revocation")

	tokens.Rotate()

	awaitLine(t, lines, "event: reauth",
		"the stream carrying session_event frames closed (or kept running) without ever sending event: reauth; "+
			"a session_event path that does not watch the revocation signal is a way around credential revocation")

	// 而且流必须真的结束：一边说「去重新认证」一边继续喂 session_event，等于没断。
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return
			}
			if strings.HasPrefix(line, "event: session_event") {
				t.Fatal("a session_event frame was delivered AFTER reauth: the rotated-away credential is still receiving session events")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the stream is still open 5s after reauth was sent")
		}
	}
}

// awaitLine 读到 prefix 开头的那一行为止；流先关或超时都按 failure 说明原因。
func awaitLine(t *testing.T, lines <-chan string, prefix, why string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stream ended before %q: %s", prefix, why)
			}
			if strings.HasPrefix(line, prefix) {
				return
			}
		case <-deadline:
			t.Fatalf("no %q within 5s: %s", prefix, why)
		}
	}
}
