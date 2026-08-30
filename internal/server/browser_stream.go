package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/stardust/legion-agent/internal/browser"
)

// BrowserStreamer 是 SSE handler 依赖的最小接口：Subscribe（RuntimeAPI 也有）
// 外加 ReplaySince（仅具体 *browser.Runtime 有，用于 Last-Event-ID 补发）。
// 故 *browser.Runtime 满足它，而 RuntimeAPI 接口本身不满足——这是有意的，
// 避免给三个 RuntimeAPI fake 增加 ReplaySince 负担。
type BrowserStreamer interface {
	Subscribe(sessionID string) (<-chan browser.StreamEvent, func(), error)
	ReplaySince(sessionID string, lastID uint64) []browser.StreamEvent
	// SetTakeover 置/清会话人工接管标志；InjectInput 注入一批归一化输入事件。
	// 二者仅具体 *browser.Runtime 满足（同 ReplaySince），不进 browser.RuntimeAPI。
	SetTakeover(sessionID string, enabled bool) error
	InjectInput(sessionID string, events []browser.InputEvent) error
	// SetViewport 把会话视口设为 width×height CSS px，使帧填满 GUI 面板。
	SetViewport(sessionID string, width, height int) error
}

// parseBrowserSessionID 从 /v1/browser/sessions/{id}/stream 抽 id。
func parseBrowserSessionID(path string) (string, bool) {
	const prefix = "/v1/browser/sessions/"
	const suffix = "/stream"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// handleBrowserStream 把一个浏览器会话的流事件经 SSE 长连接推给前端：先按
// Last-Event-ID 补发缓冲的 status 事件（帧不补），再进入实时循环。镜像
// handleEvents 的头/flush/select 惯例，客户端断开时经 r.Context().Done() 收尾，
// 保证 Subscribe 的 cancel 被调用、不泄漏 goroutine。
func (s *HTTPServer) handleBrowserStream(w http.ResponseWriter, r *http.Request) {
	if s.browser == nil {
		writeError(w, http.StatusServiceUnavailable, "browser runtime is unavailable")
		return
	}
	sessionID, ok := parseBrowserSessionID(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "bad browser stream path")
		return
	}
	ch, cancel, err := s.browser.Subscribe(sessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	flush()

	// Last-Event-ID 补发缓冲的 status 事件（帧不补）。
	if lastID := parseLastEventID(r); lastID > 0 {
		for _, ev := range s.browser.ReplaySince(sessionID, lastID) {
			if err := writeBrowserSSE(w, ev); err != nil {
				return
			}
		}
		flush()
	}

	// 这一代凭证的失效信号，在进入循环前取一次。screencast 是本服务里活得最久的
	// 一条流（一个接管中的会话可以挂几个小时），它继续送帧就等于被吊销的凭证仍在
	// 看着用户的屏幕。
	revoked := s.tokens.Changed()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := writeBrowserSSE(w, ev); err != nil {
				return
			}
			flush()
		case <-revoked:
			writeReauth(w)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// parseLastEventID 读断线重连游标：优先 Last-Event-ID 头，回退 ?lastEventId=。
// 解析失败按 0 处理（不补发）。
func parseLastEventID(r *http.Request) uint64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("lastEventId")
	}
	n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// writeBrowserSSE 把一条 StreamEvent 写成 SSE 线格式：event:<Type> id:<Seq> data:<json>。
func writeBrowserSSE(w http.ResponseWriter, ev browser.StreamEvent) error {
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", ev.Type); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\n", ev.Seq); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}
