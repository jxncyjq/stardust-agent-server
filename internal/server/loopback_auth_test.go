package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateLoopbackTokenUnique(t *testing.T) {
	a, err := GenerateLoopbackToken()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	b, err := GenerateLoopbackToken()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	if a == "" || len(a) < 32 {
		t.Fatalf("token too short: %q", a)
	}
	if a == b {
		t.Fatal("tokens not unique across calls")
	}
}

func TestHandshakeJSON(t *testing.T) {
	h := Handshake{BaseURL: "http://127.0.0.1:54321", Token: "abc"}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"baseURL"`) || !strings.Contains(string(b), `"token"`) {
		t.Fatalf("handshake json shape: %s", b)
	}
}

func TestLoopbackOriginMiddleware(t *testing.T) {
	const allowed = "http://127.0.0.1:54321"
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mw := LoopbackOriginGuard(next, allowed)

	// Legal: no Origin header (same-origin fetch/EventSource and non-browser
	// clients often omit it) → pass.
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("no-Origin should pass, got %d", rec.Code)
	}

	// Legal: Origin matching the server's base URL → pass.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("Origin", allowed)
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("matching Origin should pass, got %d", rec.Code)
	}

	// Illegal: cross-site Origin → 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin should be 403, got %d", rec.Code)
	}
}
