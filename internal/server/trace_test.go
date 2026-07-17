package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/observability"
)

func TestHTTPTracesRequireAdminTokenAndRedactSecrets(t *testing.T) {
	t.Parallel()
	traces := observability.NewTraceRecorder(observability.TraceConfig{MaxSpans: 10})
	traces.Record(observability.Span{
		TraceID:   "trace-1",
		SpanID:    "span-1",
		Name:      "model.generate",
		StartedAt: time.Now(),
		EndedAt:   time.Now(),
		Attributes: map[string]string{
			"api_key":   "secret-key",
			"component": "runtime",
		},
	})
	srv := NewHTTPServer(Config{AdminToken: "token", Traces: traces})

	unauthorized := httptest.NewRecorder()
	srv.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/debug/traces", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("GET /debug/traces without token status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	req := httptest.NewRequest(http.MethodGet, "/debug/traces", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /debug/traces status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-key") {
		t.Fatalf("GET /debug/traces body leaked secret: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "runtime") {
		t.Fatalf("GET /debug/traces body = %s, want non-sensitive component", rec.Body.String())
	}
}
