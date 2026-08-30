package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stardust/legion-agent/internal/browser"
)

// fakeBrowser 满足扩展后的 BrowserStreamer；记录调用供断言。
type fakeBrowser struct {
	takeoverCalls []struct {
		id      string
		enabled bool
	}
	takeoverErr error
	injected    [][]browser.InputEvent
	injectErr   error
	viewport    struct {
		id     string
		width  int
		height int
	}
	viewportErr error
}

func (f *fakeBrowser) Subscribe(string) (<-chan browser.StreamEvent, func(), error) {
	ch := make(chan browser.StreamEvent)
	return ch, func() {}, nil
}
func (f *fakeBrowser) ReplaySince(string, uint64) []browser.StreamEvent { return nil }
func (f *fakeBrowser) SetTakeover(id string, enabled bool) error {
	if f.takeoverErr != nil {
		return f.takeoverErr
	}
	f.takeoverCalls = append(f.takeoverCalls, struct {
		id      string
		enabled bool
	}{id, enabled})
	return nil
}
func (f *fakeBrowser) InjectInput(id string, events []browser.InputEvent) error {
	if f.injectErr != nil {
		return f.injectErr
	}
	f.injected = append(f.injected, events)
	return nil
}
func (f *fakeBrowser) SetViewport(id string, width, height int) error {
	if f.viewportErr != nil {
		return f.viewportErr
	}
	f.viewport = struct {
		id     string
		width  int
		height int
	}{id, width, height}
	return nil
}

// errPlain is an error with no semantic code at all.
var errPlain = errors.New("something went wrong inside the runtime")

func newBrowserTestServer(fb *fakeBrowser) *HTTPServer {
	return NewHTTPServer(Config{Browser: fb})
}

func TestHandleTakeoverSetsFlag(t *testing.T) {
	fb := &fakeBrowser{}
	s := newBrowserTestServer(fb)
	body, _ := json.Marshal(map[string]bool{"enabled": true})
	req := httptest.NewRequest(http.MethodPost, "/v1/browser/sessions/sess-1/takeover", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(fb.takeoverCalls) != 1 || fb.takeoverCalls[0].id != "sess-1" || !fb.takeoverCalls[0].enabled {
		t.Fatalf("SetTakeover not called as expected: %+v", fb.takeoverCalls)
	}
}

func TestHandleInputInjects(t *testing.T) {
	fb := &fakeBrowser{}
	s := newBrowserTestServer(fb)
	body, _ := json.Marshal(map[string]any{
		"events": []browser.InputEvent{{Type: "click", X: 0.5, Y: 0.5, Button: "left"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/browser/sessions/sess-1/input", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(fb.injected) != 1 || len(fb.injected[0]) != 1 || fb.injected[0][0].Type != "click" {
		t.Fatalf("InjectInput not called as expected: %+v", fb.injected)
	}
}

func TestHandleInputNilBrowser503(t *testing.T) {
	s := NewHTTPServer(Config{}) // Browser nil
	req := httptest.NewRequest(http.MethodPost, "/v1/browser/sessions/sess-1/input", bytes.NewReader([]byte(`{"events":[]}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandleTakeoverBearer(t *testing.T) {
	fb := &fakeBrowser{}
	s := NewHTTPServer(Config{Browser: fb, AdminToken: "secret"})
	req := httptest.NewRequest(http.MethodPost, "/v1/browser/sessions/sess-1/takeover", bytes.NewReader([]byte(`{"enabled":true}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req) // 无 Authorization 头
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without bearer", rec.Code)
	}
}

func TestHandleInputNotUnderTakeover409(t *testing.T) {
	fb := &fakeBrowser{
		injectErr: browser.NewBrowserError(browser.CodeTakeover, "session sess-1 not under takeover"),
	}
	s := newBrowserTestServer(fb)
	body, _ := json.Marshal(map[string]any{
		"events": []browser.InputEvent{{Type: "click", X: 0.5, Y: 0.5, Button: "left"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/browser/sessions/sess-1/input", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// 这条测试的 fixture 曾经是 ELEMENT_NOT_FOUND——它复制了那个缺陷本身：一个校验
// 失败在告诉调用方「元素没找到，重新 read 页面」。现在 runtime 对校验失败返回
// INVALID_INPUT，这里跟着改，断言不变（仍是 400）。
func TestHandleInputOtherError400(t *testing.T) {
	fb := &fakeBrowser{
		injectErr: browser.NewBrowserError(browser.CodeInvalidInput, "invalid input batch"),
	}
	s := newBrowserTestServer(fb)
	body, _ := json.Marshal(map[string]any{
		"events": []browser.InputEvent{{Type: "click", X: 0.5, Y: 0.5, Button: "left"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/browser/sessions/sess-1/input", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleViewportSetsSize(t *testing.T) {
	fb := &fakeBrowser{}
	s := newBrowserTestServer(fb)
	body, _ := json.Marshal(map[string]int{"width": 620, "height": 800})
	req := httptest.NewRequest(http.MethodPost, "/v1/browser/sessions/sess-1/viewport", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if fb.viewport.id != "sess-1" || fb.viewport.width != 620 || fb.viewport.height != 800 {
		t.Fatalf("SetViewport not called as expected: %+v", fb.viewport)
	}
}

func TestHandleViewportErrorMaps400(t *testing.T) {
	fb := &fakeBrowser{viewportErr: browser.NewBrowserError(browser.CodeInvalidInput, "viewport out of range")}
	s := newBrowserTestServer(fb)
	req := httptest.NewRequest(http.MethodPost, "/v1/browser/sessions/sess-1/viewport", bytes.NewReader([]byte(`{"width":1,"height":1}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleViewportNilBrowser503(t *testing.T) {
	s := NewHTTPServer(Config{}) // Browser nil
	req := httptest.NewRequest(http.MethodPost, "/v1/browser/sessions/sess-1/viewport", bytes.NewReader([]byte(`{"width":620,"height":800}`)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// 三个端点、同一件事，必须给同一个答案。此前同一个「会话不存在」在 takeover 回
// 404、在 input 与 viewport 回 400：客户端要为一件事写三套判断，而三种写法里
// 至少两种在别的端点上是错的。
//
// 状态码从**语义码**推出（httpStatusForBrowserCode），不再由每个 handler 各自
// 挑一个：新增一个码时忘了改某个 handler，是这种分歧唯一的来源。
func TestTheSameConditionGetsTheSameStatusEverywhere(t *testing.T) {
	notFound := browser.NewBrowserError(browser.CodeSessionNotFound, "unknown session nope")
	fb := &fakeBrowser{takeoverErr: notFound, injectErr: notFound, viewportErr: notFound}
	srv := newBrowserTestServer(fb)

	for _, tc := range []struct {
		action string
		body   string
	}{
		{"takeover", `{"enabled":true}`},
		{"input", `{"events":[{"type":"char","text":"a"}]}`},
		{"viewport", `{"width":800,"height":600}`},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/browser/sessions/nope/"+tc.action,
			bytes.NewBufferString(tc.body))
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s with an unknown session = %d, want 404: %s", tc.action, rec.Code, rec.Body.String())
		}
	}
}

// TestAMalformedRequestIsFourHundredAndAMissingPageIsNot: the two are told
// apart by the code the runtime returned, not by which handler ran. A 400 says
// "fix the request"; retrying it unchanged can never work. A 409 says the
// session's page is gone — same request, rebuilt session, and it works.
func TestAMalformedRequestIsFourHundredAndAMissingPageIsNot(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"malformed batch", browser.NewBrowserError(browser.CodeInvalidInput, "input batch is empty"), http.StatusBadRequest},
		{"page gone", browser.NewBrowserError(browser.CodeContextEvicted, "session s has no active page"), http.StatusConflict},
		{"not under takeover", browser.NewBrowserError(browser.CodeTakeoverRequired, "not under takeover"), http.StatusConflict},
		{"blocked host", browser.NewBrowserError(browser.CodePrivateHostBlocked, "private ip"), http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newBrowserTestServer(&fakeBrowser{injectErr: tc.err})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/browser/sessions/s/input",
				bytes.NewBufferString(`{"events":[{"type":"char","text":"a"}]}`))
			srv.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestAnErrorWithNoSemanticCodeIsNotSilentlyCalledABadRequest: an error the
// runtime returned without a BrowserError is a gap in OUR wiring, not the
// caller's mistake. Answering 400 would blame the caller for a bug they cannot
// fix and hide it from every dashboard that watches 5xx.
func TestAnErrorWithNoSemanticCodeIsNotSilentlyCalledABadRequest(t *testing.T) {
	srv := newBrowserTestServer(&fakeBrowser{injectErr: errPlain})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/browser/sessions/s/input",
		bytes.NewBufferString(`{"events":[{"type":"char","text":"a"}]}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for an uncoded error: %s", rec.Code, rec.Body.String())
	}
}
