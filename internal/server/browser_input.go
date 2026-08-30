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
