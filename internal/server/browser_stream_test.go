package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/browser"
)

// fakeStreamer 满足 BrowserStreamer：Subscribe 返回一个我们手工喂数据的通道。
type fakeStreamer struct {
	ch      chan browser.StreamEvent
	replay  []browser.StreamEvent
	lastReq uint64
}

func (f *fakeStreamer) Subscribe(sessionID string) (<-chan browser.StreamEvent, func(), error) {
	return f.ch, func() {}, nil
}
func (f *fakeStreamer) ReplaySince(sessionID string, lastID uint64) []browser.StreamEvent {
	f.lastReq = lastID
	return f.replay
}

func TestBrowserStreamWritesSSEEvents(t *testing.T) {
	f := &fakeStreamer{ch: make(chan browser.StreamEvent, 4)}
	srv := &HTTPServer{browser: f}

	f.ch <- browser.StreamEvent{Type: browser.EventProgress, Seq: 7, Data: map[string]any{"action": "click"}}

	req := httptest.NewRequest(http.MethodGet, "/v1/browser/sessions/sess-1/stream", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	srv.handleBrowserStream(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(body, "event: progress") || !strings.Contains(body, "id: 7") || !strings.Contains(body, `"action":"click"`) {
		t.Fatalf("SSE wire missing pieces:\n%s", body)
	}
}

func TestBrowserStreamReplaysOnLastEventID(t *testing.T) {
	f := &fakeStreamer{
		ch:     make(chan browser.StreamEvent),
		replay: []browser.StreamEvent{{Type: browser.EventObservation, Seq: 5}},
	}
	srv := &HTTPServer{browser: f}

	req := httptest.NewRequest(http.MethodGet, "/v1/browser/sessions/sess-1/stream", nil)
	req.Header.Set("Last-Event-ID", "4")
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	srv.handleBrowserStream(rec, req)

	if f.lastReq != 4 {
		t.Fatalf("ReplaySince got lastID=%d, want 4", f.lastReq)
	}
	if !strings.Contains(rec.Body.String(), "id: 5") {
		t.Fatalf("expected replayed obs seq=5:\n%s", rec.Body.String())
	}
}
