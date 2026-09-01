package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// storedEvent 是夹具里「库里那一行」的最小形状：seq、类型与 JSON 载荷原文。
type storedEvent struct {
	Seq  int64
	Type string
	Data string
}

// sessionEventsTestStore 是一个只服务一条会话的 SessionStore 假实现，照
// files_route_test.go 的 fileTestSessionStore 写法来（同一个接口、同一种最小桩）。
//
// 它的 ReadFrom **必须与真 store（storage.SQLiteRepository.ReadFrom）语义一致**：
// 负 fromSeq 报错、只回后缀、返回段自身连续且首条恰好等于 fromSeq，否则报损坏。
// 夹具比真 store 宽松，会让一条真实的功能回归对整套测试隐形。
type sessionEventsTestStore struct {
	sessionID string
	events    []domain.SessionEvent

	// deleted 记下 DeleteAgentSession 是否被调用过，供路由守卫的测试断言
	// 「这条请求没有变成一次删除」。
	deleted bool
}

func newSessionEventsTestStore(sessionID string, stored []storedEvent) *sessionEventsTestStore {
	events := make([]domain.SessionEvent, 0, len(stored))
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for _, e := range stored {
		events = append(events, domain.SessionEvent{
			Seq:  e.Seq,
			Type: domain.SessionEventType(e.Type),
			Time: base.Add(time.Duration(e.Seq) * time.Second),
			Data: json.RawMessage(e.Data),
		})
	}
	return &sessionEventsTestStore{sessionID: sessionID, events: events}
}

func (s *sessionEventsTestStore) ListAgentSessions(ctx context.Context, companyID string, agentID string) ([]domain.AgentSession, error) {
	return nil, nil
}

func (s *sessionEventsTestStore) ListConversationTurns(ctx context.Context, sessionID string, limit int) ([]domain.ConversationTurn, error) {
	return nil, nil
}

func (s *sessionEventsTestStore) GetAgentSession(ctx context.Context, sessionID string) (domain.AgentSession, bool, error) {
	if sessionID != s.sessionID {
		return domain.AgentSession{}, false, nil
	}
	return domain.AgentSession{ID: sessionID}, true, nil
}

func (s *sessionEventsTestStore) SaveAgentSession(ctx context.Context, session domain.AgentSession) error {
	return nil
}

func (s *sessionEventsTestStore) DeleteAgentSession(ctx context.Context, sessionID string) error {
	s.deleted = true
	return nil
}

// ReadFrom 复刻真 store 的读契约（见 internal/storage/session_events.go 的
// SQLiteRepository.ReadFrom）：负 fromSeq 报错，只回 seq >= fromSeq 的升序后缀，
// 相邻断裂与「窗口起点落在洞里」都算日志损坏。
func (s *sessionEventsTestStore) ReadFrom(ctx context.Context, sessionID string, fromSeq int64) ([]domain.SessionEvent, error) {
	if fromSeq < 0 {
		return nil, fmt.Errorf("read session events for %q: fromSeq %d is negative", sessionID, fromSeq)
	}
	if sessionID != s.sessionID {
		return nil, nil
	}
	var (
		out      []domain.SessionEvent
		expected int64 = -1
	)
	for _, event := range s.events {
		if event.Seq < fromSeq {
			continue
		}
		if expected >= 0 && event.Seq != expected {
			return nil, fmt.Errorf("session log for %q is broken: seq jumps from %d to %d", sessionID, expected-1, event.Seq)
		}
		out = append(out, event)
		expected = event.Seq + 1
	}
	if len(out) > 0 && out[0].Seq != fromSeq {
		return nil, fmt.Errorf("session log for %q is broken: requested from seq %d but the first event actually present is seq %d",
			sessionID, fromSeq, out[0].Seq)
	}
	return out, nil
}

var _ SessionStore = (*sessionEventsTestStore)(nil)

// newTestServerWithSessionEvents 起一个只认得 sessionID 这一条会话的服务器，
// 它的事件日志就是 stored。
func newTestServerWithSessionEvents(t *testing.T, sessionID string, stored []storedEvent) *HTTPServer {
	t.Helper()
	return NewHTTPServer(Config{Sessions: newSessionEventsTestStore(sessionID, stored)})
}

// 端点是轨迹首屏与翻页的来源。这条断言的是它把 ReadFrom 的结果原样开出来，
// 且 next_seq 指向下一页的起点。
func TestTheEventsEndpointReturnsTheEventsAndTheNextSeq(t *testing.T) {
	srv := newTestServerWithSessionEvents(t, "sess-1", []storedEvent{
		{Seq: 0, Type: "turn/start", Data: `{"turn":0}`},
		{Seq: 1, Type: "user/message", Data: `{"turn":0,"content":"你好"}`},
		{Seq: 2, Type: "turn/end", Data: `{"turn":0,"reason":"completed"}`},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-1/events", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，要 200：%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Events []struct {
			Seq  int64           `json:"seq"`
			Type string          `json:"type"`
			Time time.Time       `json:"time"`
			Data json.RawMessage `json:"data"`
		} `json:"events"`
		NextSeq int64 `json:"next_seq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应：%v，原文 %s", err, rec.Body.String())
	}
	if len(got.Events) != 3 {
		t.Fatalf("返回 %d 条事件，要 3 条", len(got.Events))
	}
	if got.Events[0].Seq != 0 || got.Events[2].Seq != 2 {
		t.Errorf("seq 不对：%d..%d", got.Events[0].Seq, got.Events[2].Seq)
	}
	if got.Events[1].Type != "user/message" {
		t.Errorf("type = %q", got.Events[1].Type)
	}
	if got.Events[1].Time.IsZero() {
		t.Errorf("time 是零值，事件的发生时刻必须开出来")
	}
	if string(got.Events[1].Data) != `{"turn":0,"content":"你好"}` {
		t.Errorf("data 被改写了：%s", got.Events[1].Data)
	}
	if got.NextSeq != 3 {
		t.Errorf("next_seq = %d，要 3（最后一条 seq 2 的下一格）", got.NextSeq)
	}
}

// from_seq 是翻页的续读点：只回它之后的后缀。
func TestFromSeqReturnsOnlyTheSuffix(t *testing.T) {
	srv := newTestServerWithSessionEvents(t, "sess-1", []storedEvent{
		{Seq: 0, Type: "turn/start", Data: `{"turn":0}`},
		{Seq: 1, Type: "user/message", Data: `{"turn":0,"content":"你好"}`},
		{Seq: 2, Type: "turn/end", Data: `{"turn":0,"reason":"completed"}`},
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-1/events?from_seq=2", nil))

	var got struct {
		Events []struct {
			Seq int64 `json:"seq"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if len(got.Events) != 1 || got.Events[0].Seq != 2 {
		t.Fatalf("from_seq=2 返回了 %d 条（首条 seq 见下），要只回 seq 2 那一条：%+v", len(got.Events), got.Events)
	}
}

// 一条没有事件的会话是 200 加空列表（而不是 404）：会话在，只是还没写过东西。
// next_seq 保持调用方给的 from_seq——「从这里继续等」。
func TestASessionWithNoEventsIsAnEmptyPageNotAnError(t *testing.T) {
	srv := newTestServerWithSessionEvents(t, "sess-1", nil)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-1/events?from_seq=7", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，要 200：%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Events  []json.RawMessage `json:"events"`
		NextSeq int64             `json:"next_seq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应：%v，原文 %s", err, rec.Body.String())
	}
	if got.Events == nil {
		t.Errorf("events 是 null，要 []：前端不该为了空列表分一路特判")
	}
	if len(got.Events) != 0 {
		t.Errorf("返回了 %d 条事件，要 0 条", len(got.Events))
	}
	if got.NextSeq != 7 {
		t.Errorf("next_seq = %d，要 7（原样回 from_seq，前端从这里继续等）", got.NextSeq)
	}
}

// limit 压住每屏事件数（spec §7：虚拟滚动先不做，靠 limit 分页压住）。
// 截断时 next_seq 必须指向**被截掉的第一条**，否则前端翻页会跳过事件。
func TestLimitTruncatesAndNextSeqPointsAtTheFirstDroppedEvent(t *testing.T) {
	srv := newTestServerWithSessionEvents(t, "sess-1", []storedEvent{
		{Seq: 0, Type: "turn/start", Data: `{"turn":0}`},
		{Seq: 1, Type: "user/message", Data: `{"turn":0,"content":"你好"}`},
		{Seq: 2, Type: "turn/end", Data: `{"turn":0,"reason":"completed"}`},
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-1/events?limit=2", nil))

	var got struct {
		Events []struct {
			Seq int64 `json:"seq"`
		} `json:"events"`
		NextSeq int64 `json:"next_seq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if len(got.Events) != 2 {
		t.Fatalf("limit=2 返回了 %d 条", len(got.Events))
	}
	if got.NextSeq != 2 {
		t.Errorf("next_seq = %d，要 2（被截掉的第一条）：前端下一页从这里续读，指错会漏事件", got.NextSeq)
	}
}

// 坏参数是调用方的错，必须 400 并说清楚哪个参数坏了——不要悄悄当成默认值。
func TestBadPagingParametersAreRefusedByName(t *testing.T) {
	srv := newTestServerWithSessionEvents(t, "sess-1", nil)

	for _, tc := range []struct{ query, wants string }{
		{"?from_seq=-1", "from_seq"},
		{"?from_seq=abc", "from_seq"},
		{"?limit=-5", "limit"},
		{"?limit=abc", "limit"},
		// limit=0 同理：一页零条事件对谁都没用，把它悄悄换成默认的 500
		// 就是「悄悄当成默认值」——调用方会以为自己的分页生效了。
		{"?limit=0", "limit"},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-1/events"+tc.query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s → 状态码 %d，要 400", tc.query, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.wants) {
			t.Errorf("%s → 错误信息没指名 %q：%s", tc.query, tc.wants, rec.Body.String())
		}
	}
}

// 不存在的会话返回 404，而不是空事件列表——「这条会话没有事件」和
// 「这条会话不存在」是两件事，前端要能区分。
func TestAMissingSessionIsNotFound(t *testing.T) {
	srv := newTestServerWithSessionEvents(t, "sess-1", nil)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-nope/events", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("状态码 %d，要 404：%s", rec.Code, rec.Body.String())
	}
}

// 路由守卫：PATCH/DELETE /v1/sessions/{id} 那两条分支原本用「路径不以 /turns 结尾」
// 来判定「这是会话本体」。加了 /events 这个子资源之后那个判定就不成立了，
// DELETE /v1/sessions/{id}/events 会落进会话删除分支——数据损坏级的错误。
// 这条钉住：子资源路径上的写方法一律不是会话本体的写，删除绝不能发生。
func TestWriteMethodsOnTheEventsSubresourceAreNotSessionWrites(t *testing.T) {
	for _, method := range []string{http.MethodDelete, http.MethodPatch} {
		store := newSessionEventsTestStore("sess-1", nil)
		srv := NewHTTPServer(Config{Sessions: store})

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(method, "/v1/sessions/sess-1/events", strings.NewReader(`{"title":"x"}`)))

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s /v1/sessions/sess-1/events → 状态码 %d，要 404（这条路由不存在）：%s",
				method, rec.Code, rec.Body.String())
		}
		if store.deleted {
			t.Errorf("%s /v1/sessions/sess-1/events 删掉了会话本体——子资源路径被当成了会话本体", method)
		}
	}
}
