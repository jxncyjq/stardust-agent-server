package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/task"
)

func TestReadyzReportsStorageAvailability(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{
		Tasks:               task.NewScheduler(),
		PublicHealthEnabled: true,
		Readiness:           okReadinessChecker{},
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body readinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("Decode(/readyz) error = %v, want nil", err)
	}
	if body.Status != "ok" || body.Checks["storage"] != "ok" {
		t.Fatalf("GET /readyz body = %#v, want ok storage", body)
	}
}

func TestReadyzReportsStorageUnavailable(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{
		Tasks:               task.NewScheduler(),
		PublicHealthEnabled: true,
		Readiness:           failingReadinessChecker{},
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	var body readinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("Decode(/readyz unavailable) error = %v, want nil", err)
	}
	if body.Status != "unavailable" || body.Reason != "storage_unavailable" || body.Checks["storage"] != "unavailable" {
		t.Fatalf("GET /readyz unavailable body = %#v, want storage_unavailable", body)
	}
}

func TestDiagnosticsEndpointRedactsSecrets(t *testing.T) {
	t.Parallel()
	diagnostics := observability.NewDiagnostics(observability.DiagnosticsConfig{
		Version:             "dev",
		StartedAt:           time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
		Now:                 func() time.Time { return time.Date(2026, 5, 14, 9, 0, 5, 0, time.UTC) },
		MaasAPIKey:          "maas-secret",
		AdminToken:          "admin-secret",
		RuntimeDemoResponse: "full prompt should not leak",
	})
	srv := NewHTTPServer(Config{
		Tasks:               task.NewScheduler(),
		AdminToken:          "admin-secret",
		PublicHealthEnabled: true,
		Diagnostics:         diagnostics,
	})

	unauthorized := httptest.NewRecorder()
	srv.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/debug/diagnostics", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("GET /debug/diagnostics without token status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/diagnostics", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /debug/diagnostics status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{"maas-secret", "admin-secret", "full prompt should not leak"} {
		if strings.Contains(body, secret) {
			t.Fatalf("GET /debug/diagnostics body contains secret %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, "[redacted]") {
		t.Fatalf("GET /debug/diagnostics body = %s, want redacted markers", body)
	}
}

type okReadinessChecker struct{}

func (okReadinessChecker) Ping(context.Context) error {
	return nil
}

type failingReadinessChecker struct{}

func (failingReadinessChecker) Ping(context.Context) error {
	return errors.New("storage down")
}
