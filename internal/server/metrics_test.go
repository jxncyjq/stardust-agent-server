package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/task"
)

func TestMetricsEndpointReturnsSnapshot(t *testing.T) {
	t.Parallel()
	metrics := observability.NewMetricsRecorder(nil)
	srv := NewHTTPServer(Config{
		Tasks:               task.NewScheduler(),
		AdminToken:          "secret-token",
		PublicHealthEnabled: true,
		RequestIDHeader:     "X-Request-ID",
		Metrics:             metrics,
	})

	unauthorized := httptest.NewRecorder()
	srv.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("GET /metrics without token status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics with token status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var snapshot observability.MetricsSnapshot
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(/metrics) error = %v, want nil", err)
	}
	if snapshot.HTTPStatus["401"] != 1 || snapshot.HTTPStatus["200"] != 1 {
		t.Fatalf("GET /metrics HTTPStatus = %#v, want 401=1 200=1", snapshot.HTTPStatus)
	}
}

func TestMetricsPrometheusFormat(t *testing.T) {
	t.Parallel()
	metrics := observability.NewMetricsRecorder(nil)
	metrics.IncTaskStatus("completed")
	metrics.IncModelCall("ok")
	metrics.IncApproval("approved")
	metrics.IncWorkflowRun("completed")
	srv := NewHTTPServer(Config{
		Tasks:               task.NewScheduler(),
		AdminToken:          "secret-token",
		PublicHealthEnabled: true,
		RequestIDHeader:     "X-Request-ID",
		Metrics:             metrics,
	})
	req := httptest.NewRequest(http.MethodGet, "/metrics?format=prometheus", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics?format=prometheus status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	contentType := rec.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") {
		t.Fatalf("GET /metrics?format=prometheus Content-Type = %q, want text/plain", contentType)
	}
	body := rec.Body.String()
	required := []string{
		"legion_agent_http_requests_total",
		"legion_agent_tasks_total",
		"legion_agent_model_calls_total",
		"legion_agent_approvals_total",
		"legion_agent_workflows_total",
	}
	for _, metric := range required {
		if !strings.Contains(body, metric) {
			t.Errorf("GET /metrics?format=prometheus body missing %q: %s", metric, body)
		}
	}
	if strings.Contains(body, "prompt") || strings.Contains(body, "secret") || strings.Contains(body, "api_key") {
		t.Fatalf("GET /metrics?format=prometheus leaked sensitive field: %s", body)
	}
}
