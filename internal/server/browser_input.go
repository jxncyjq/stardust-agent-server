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
		writeError(w, http.StatusBadRequest, err.Error())
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
		writeError(w, http.StatusNotFound, err.Error())
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
		// 未接管即注入 → InjectInput 返回 *browser.BrowserError{Code: CodeTakeover}；
		// 用 errors.As 而非字符串匹配做判别，健壮穿透错误链，映射 409（须先进接管）。
		// 其它（校验失败/无活跃页）→ 400。
		var be *browser.BrowserError
		if errors.As(err, &be) && be.Code == browser.CodeTakeover {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "injected": len(req.Events)})
}
