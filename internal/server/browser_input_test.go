package server

import (
	"bytes"
	"encoding/json"
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
	injected  [][]browser.InputEvent
	injectErr error
}

func (f *fakeBrowser) Subscribe(string) (<-chan browser.StreamEvent, func(), error) {
	ch := make(chan browser.StreamEvent)
	return ch, func() {}, nil
}
func (f *fakeBrowser) ReplaySince(string, uint64) []browser.StreamEvent { return nil }
func (f *fakeBrowser) SetTakeover(id string, enabled bool) error {
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

func TestHandleInputOtherError400(t *testing.T) {
	fb := &fakeBrowser{
		injectErr: browser.NewBrowserError(browser.CodeElementNotFound, "invalid input batch"),
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
