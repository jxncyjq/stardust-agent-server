package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultSessionEventsLimit 是一页事件的默认上限。
//
// spec §7 定了「虚拟滚动先不做：靠 limit 分页压住每屏事件数」，所以这个默认值就是
// 前端首屏的规模。取 500：一条典型会话的一轮对话约 6-10 条事件，500 足够铺满首屏
// 而不会让单个响应大到需要流式。
const defaultSessionEventsLimit = 500

// sessionEventsResponse 是 GET /v1/sessions/{id}/events 的响应体。
//
// NextSeq 由服务端给出而不是让前端从 events 末尾自己算：那样会把「这一页恰好读完」
// 与「还有下一页」混为一谈。截断时它指向**被截掉的第一条**，前端据此续读不会漏。
type sessionEventsResponse struct {
	Events  []sessionEventDTO `json:"events"`
	NextSeq int64             `json:"next_seq"`
}

// sessionEventDTO 是一条会话事件在 HTTP 上的形状。
type sessionEventDTO struct {
	Seq int64 `json:"seq"`
	// Type 是 domain.SessionEventType 的字符串形式（闭集，见 internal/domain/session_event.go）。
	Type string `json:"type"`
	// Time 用 time.Time 直接序列化，与本包既有 DTO 一致（见 conversationTurnResponse.CreatedAt），
	// 由 encoding/json 产出 RFC3339Nano。
	Time time.Time `json:"time"`
	// Data 是事件载荷的 JSON 原文，原样透出：这一层不解载荷，各事件的形状归它们的
	// 生产者与消费者管（与 domain.SessionEvent.Data 同一个立场）。
	Data json.RawMessage `json:"data"`
}

// handleSessionEvents 开出一条会话的原始事件，供轨迹首屏与翻页使用。
//
// 走 ReadFrom 而不是 Load：spec §4.3.1 第 3 条要求 Load 只对「确定没有活跃写入者」的
// 会话调用，而这个端点在任务执行期间也会被前端调用。ReadFrom 只读后缀、不触发崩溃恢复，
// 正是这里要的。
func (s *HTTPServer) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "session store is unavailable")
		return
	}
	sessionID := sessionIDFromSubresourcePath(r.URL.Path, "/events")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session id is required")
		return
	}

	// 会话不存在与「会话存在但没有事件」是两件事，前端要能区分：前者 404，后者
	// 200 加一页空列表。
	if _, ok, err := s.sessions.GetAgentSession(r.Context(), sessionID); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load session %q: %v", sessionID, err))
		return
	} else if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", sessionID))
		return
	}

	fromSeq, err := parseBoundedQueryInt(r, "from_seq", 0, 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// limit 的下界是 1 而不是 0：一页零条事件对调用方毫无用处，写出 limit=0 的人
	// 要的几乎一定是别的东西。把它当成默认值接着跑，会让他以为自己的分页生效了。
	limit, err := parseBoundedQueryInt(r, "limit", 1, defaultSessionEventsLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	events, err := s.sessions.ReadFrom(r.Context(), sessionID, fromSeq)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("read session events for %q: %v", sessionID, err))
		return
	}

	// nextSeq 的两种情形：截断了就指向被截掉的第一条；没截断就指向最后一条的下一格。
	// 空结果时保持调用方给的 fromSeq——「从这里继续等」。
	nextSeq := fromSeq
	if int64(len(events)) > limit {
		events = events[:limit]
	}
	if len(events) > 0 {
		nextSeq = events[len(events)-1].Seq + 1
	}

	// 空列表用 [] 而不是 null：前端不该为了「一条事件都没有」再分一路特判。
	out := sessionEventsResponse{Events: make([]sessionEventDTO, 0, len(events)), NextSeq: nextSeq}
	for _, e := range events {
		out.Events = append(out.Events, sessionEventDTO{
			Seq:  e.Seq,
			Type: string(e.Type),
			Time: e.Time.UTC(),
			Data: e.Data,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// sessionIDFromSubresourcePath 取出 /v1/sessions/{id}<suffix> 里的 {id}。
//
// 与 sessionIDFromPath 分开：那个要求路径里再没有 '/'（它服务的是会话本体），
// 而这里的路径按定义就带一段子资源后缀。id 自身仍不许含 '/'——含了就说明这条路径
// 比 /v1/sessions/{id}<suffix> 更深，不是这个 handler 该认的东西。
func sessionIDFromSubresourcePath(path, suffix string) string {
	trimmed, ok := strings.CutSuffix(strings.TrimPrefix(path, "/v1/sessions/"), suffix)
	if !ok || strings.Contains(trimmed, "/") {
		return ""
	}
	return strings.TrimSpace(trimmed)
}

// parseBoundedQueryInt 读一个有下界的整数查询参数。
//
// 坏值一律报错并**指名是哪个参数**，不悄悄当成默认值：调用方拼错了参数名或传了
// 越界的值，静默用默认值会让它以为自己的分页生效了（fail-loud 铁律，CLAUDE.md §0）。
// 参数缺席是合法的可选，返回 fallback。
func parseBoundedQueryInt(r *http.Request, name string, least int64, fallback int64) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", name, raw)
	}
	if v < least {
		return 0, fmt.Errorf("%s must be at least %d, got %d", name, least, v)
	}
	return v, nil
}
