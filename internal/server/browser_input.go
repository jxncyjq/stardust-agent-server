package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/stardust/legion-agent/internal/browser"
)

// parseBrowserActionID 从 /v1/browser/sessions/{id}/{action} 抽 id 与 action。
func parseBrowserActionID(path string) (id, action string, ok bool) {
	const prefix = "/v1/browser/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	slash := strings.LastIndex(rest, "/")
	if slash <= 0 || slash == len(rest)-1 {
		return "", "", false
	}
	id, action = rest[:slash], rest[slash+1:]
	if id == "" || strings.Contains(id, "/") {
		return "", "", false
	}
	return id, action, true
}

// writeBrowserError answers a browser runtime error with the status its
// SEMANTIC CODE implies, and nothing else decides it.
//
// Each handler used to pick a status by hand, and they disagreed: the same
// "unknown session" was 404 from takeover and 400 from input and viewport, so a
// client had to write three checks for one condition — and at least two of the
// three were wrong on the other endpoints. Deriving it here means adding a code
// is one edit, not four, and forgetting one endpoint is no longer possible.
func writeBrowserError(w http.ResponseWriter, err error) {
	var be *browser.BrowserError
	if !errors.As(err, &be) {
		// No semantic code is a gap in OUR wiring, not the caller's mistake:
		// answering 400 would blame them for something they cannot fix, and
		// hide it from every dashboard that watches 5xx.
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeError(w, httpStatusForBrowserCode(be.Code), err.Error())
}

// httpStatusForBrowserCode maps one semantic code onto the status that carries
// the same advice.
//
//   - 400: the request itself is wrong; retrying it unchanged can never work.
//   - 403: the deployment refuses this target, whatever the caller does.
//   - 404: there is no such session.
//   - 409: the session exists but is in the wrong state for this call — its
//     page is gone, or takeover is (not) on. The same request works once the
//     state changes.
//   - 500: everything else, including a code added without a mapping. It is
//     deliberately noisy rather than a plausible-looking 400.
func httpStatusForBrowserCode(code browser.Code) int {
	switch code {
	case browser.CodeInvalidInput:
		return http.StatusBadRequest
	case browser.CodeProtocolBlocked, browser.CodePrivateHostBlocked:
		return http.StatusForbidden
	case browser.CodeSessionNotFound:
		return http.StatusNotFound
	case browser.CodeContextEvicted, browser.CodeTakeover, browser.CodeTakeoverRequired,
		browser.CodeElementNotFound:
		// ELEMENT_NOT_FOUND belongs here rather than with the malformed
		// requests: the request was well-formed, the page moved underneath it.
		// Re-reading and asking again is exactly what 409 tells a client.
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

type takeoverRequest struct {
	Enabled bool `json:"enabled"`
}

type inputRequest struct {
	Events []browser.InputEvent `json:"events"`
}

// handleBrowserNavigate 是人工导航：地址栏、后退/前进/刷新。
//
// 它存在是因为浏览器视图此前只是一个能点击的录像——用户想回上一页都得让 Agent
// 去做。校验（接管中、URL 策略、动作白名单）全在 runtime 里，这里只做搬运，免得
// 两处各写一份策略然后慢慢分叉。
func (s *HTTPServer) handleBrowserNavigate(w http.ResponseWriter, r *http.Request) {
	if s.browser == nil {
		writeError(w, http.StatusServiceUnavailable, "browser runtime is unavailable")
		return
	}
	id, _, ok := parseBrowserActionID(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "bad browser navigate path")
		return
	}
	var req browser.NavigateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid navigate request")
		return
	}
	if err := s.browser.NavigateTakeover(id, req); err != nil {
		writeBrowserError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "url": req.URL, "action": req.Action})
}

// handleBrowserSessionInfo 回一个会话的当前状态：地址、接管标志、页面是否还在。
//
// 地址栏据它渲染。此前没有任何地方能回答「现在在哪」——观测事件里没有 URL，会话
// 状态也没有出口，于是界面只能显示一个 session id。
func (s *HTTPServer) handleBrowserSessionInfo(w http.ResponseWriter, r *http.Request) {
	if s.browser == nil {
		writeError(w, http.StatusServiceUnavailable, "browser runtime is unavailable")
		return
	}
	// parseBrowserActionID（按最后一段切）而不是 parseBrowserSessionID（后者只认
	// /stream 后缀）：这里的最后一段是 /info。
	id, _, ok := parseBrowserActionID(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "bad browser session path")
		return
	}
	info, err := s.browser.SessionInfo(id)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleBrowserSessions 列出当前的浏览器会话，供界面渲染标签条。
//
// ?chat_session_id= 按对话过滤。不过滤是默认，因为运维（curl）想看的是全部；而
// 界面总是带上它——视图跟着当前对话走，把别的对话的会话摆进标签条只会让人点进一个
// 与眼前工作无关的页面。
func (s *HTTPServer) handleBrowserSessions(w http.ResponseWriter, r *http.Request) {
	if s.browser == nil {
		writeError(w, http.StatusServiceUnavailable, "browser runtime is unavailable")
		return
	}
	sessions := s.browser.ListSessions(strings.TrimSpace(r.URL.Query().Get("chat_session_id")))
	// 空列表要回 [] 而不是 null：前端 map 一个 null 会炸，而「一个会话都没有」是
	// 最常见的正常状态（Agent 还没浏览过任何东西）。
	if sessions == nil {
		sessions = []browser.SessionInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

type viewportRequest struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// handleBrowserViewport 把会话视口设为请求的 width×height，使帧填满 GUI 面板（消除
// letterbox）。auth 由 HTTPServer.authorized 统一守。范围/无会话/无活跃页 → 400。
func (s *HTTPServer) handleBrowserViewport(w http.ResponseWriter, r *http.Request) {
	if s.browser == nil {
		writeError(w, http.StatusServiceUnavailable, "browser runtime is unavailable")
		return
	}
	id, _, ok := parseBrowserActionID(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "bad browser viewport path")
		return
	}
	var req viewportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid viewport request")
		return
	}
	if err := s.browser.SetViewport(id, req.Width, req.Height); err != nil {
		writeBrowserError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "width": req.Width, "height": req.Height})
}

// handleBrowserTakeover 置/清会话接管标志。auth 由 HTTPServer.authorized 统一守。
func (s *HTTPServer) handleBrowserTakeover(w http.ResponseWriter, r *http.Request) {
	if s.browser == nil {
		writeError(w, http.StatusServiceUnavailable, "browser runtime is unavailable")
		return
	}
	id, _, ok := parseBrowserActionID(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "bad browser takeover path")
		return
	}
	var req takeoverRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid takeover request")
		return
	}
	if err := s.browser.SetTakeover(id, req.Enabled); err != nil {
		writeBrowserError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "takeover": req.Enabled})
}

// handleBrowserInput 注入一批输入事件；要求该会话已进入接管（InjectInput 内校验后拒非接管）。
func (s *HTTPServer) handleBrowserInput(w http.ResponseWriter, r *http.Request) {
	if s.browser == nil {
		writeError(w, http.StatusServiceUnavailable, "browser runtime is unavailable")
		return
	}
	id, _, ok := parseBrowserActionID(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "bad browser input path")
		return
	}
	var req inputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid input request")
		return
	}
	if err := s.browser.InjectInput(id, req.Events); err != nil {
		writeBrowserError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "injected": len(req.Events)})
}
