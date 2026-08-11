//go:build chromium

package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSetViewportChangesInnerSize drives real Chromium: after SetViewport the
// page's window.innerWidth/innerHeight must reflect the requested device-metrics
// size. This guards the go-rod SetViewport call (proto.EmulationSetDeviceMetrics
// Override) and the screencast restart path.
func TestSetViewportChangesInnerSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><h1>vp</h1></body></html>`))
	}))
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	open, err := rt.Open(context.Background(), OpenReq{URL: srv.URL, TaskID: "vp"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := rt.SetViewport(open.SessionID, 480, 640); err != nil {
		t.Fatalf("SetViewport: %v", err)
	}

	_, page, err := rt.activePage(open.SessionID)
	if err != nil {
		t.Fatalf("activePage: %v", err)
	}
	w, h, err := viewportSize(page)
	if err != nil {
		t.Fatalf("viewportSize: %v", err)
	}
	if int(w) != 480 || int(h) != 640 {
		t.Fatalf("viewport = %vx%v, want 480x640", w, h)
	}
}

// TestSetViewportRejectsOutOfRange asserts the bounds check is fail-loud.
func TestSetViewportRejectsOutOfRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body>ok</body></html>`))
	}))
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})
	open, err := rt.Open(context.Background(), OpenReq{URL: srv.URL, TaskID: "vp"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := rt.SetViewport(open.SessionID, 10, 640); err == nil {
		t.Fatal("SetViewport(10x640) = nil, want out-of-range error")
	}
}
