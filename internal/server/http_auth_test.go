package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/task"
)

func TestHTTPAdminTokenAndRequestID(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{
		Tasks:               task.NewScheduler(),
		AdminToken:          "secret-token",
		PublicHealthEnabled: true,
		RequestIDHeader:     "X-Request-ID",
	})

	health := httptest.NewRecorder()
	srv.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", health.Code, http.StatusOK)
	}
	if health.Header().Get("X-Request-ID") == "" {
		t.Fatalf("GET /healthz X-Request-ID = %q, want generated request id", health.Header().Get("X-Request-ID"))
	}

	unauthorized := httptest.NewRecorder()
	unauthorizedReq := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewBufferString(`{
		"id": "task-auth-1",
		"input": "blocked"
	}`))
	srv.ServeHTTP(unauthorized, unauthorizedReq)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("POST /v1/tasks without token status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	created := httptest.NewRecorder()
	createdReq := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewBufferString(`{
		"id": "task-auth-2",
		"input": "allowed"
	}`))
	createdReq.Header.Set("Authorization", "Bearer secret-token")
	createdReq.Header.Set("X-Request-ID", "request-from-client")
	srv.ServeHTTP(created, createdReq)
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /v1/tasks with token status = %d, want %d body=%s", created.Code, http.StatusCreated, created.Body.String())
	}
	if created.Header().Get("X-Request-ID") != "request-from-client" {
		t.Fatalf("POST /v1/tasks X-Request-ID = %q, want request-from-client", created.Header().Get("X-Request-ID"))
	}
}

func TestHTTPHealthzCanRequireAdminToken(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{
		Tasks:               task.NewScheduler(),
		AdminToken:          "secret-token",
		PublicHealthEnabled: false,
		RequestIDHeader:     "X-Request-ID",
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /healthz with public health disabled status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHTTPServerWritesStructuredRequestLogs(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger, err := observability.NewLogger(&logs, observability.LoggerConfig{})
	if err != nil {
		t.Fatalf("NewLogger(default) error = %v, want nil", err)
	}
	srv := NewHTTPServer(Config{
		Tasks:           task.NewScheduler(),
		RequestIDHeader: "X-Request-ID",
		Logger:          logger,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewBufferString(`{
		"id": "task-log-1",
		"input": "allowed"
	}`))
	req.Header.Set("X-Request-ID", "req-log-1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/tasks status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	entries := decodeLogEntries(t, logs.Bytes())
	assertLogEntry(t, entries, "http request handled", map[string]any{
		"component":  "server",
		"request_id": "req-log-1",
		"method":     http.MethodPost,
		"path":       "/v1/tasks",
		"status":     float64(http.StatusCreated),
	})
	assertLogEntry(t, entries, "task submitted", map[string]any{
		"component":  "server",
		"request_id": "req-log-1",
		"task_id":    "task-log-1",
	})
}

func decodeLogEntries(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	var entries []map[string]any
	for decoder.More() {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatalf("Decode(log entry) error = %v, want nil; logs=%s", err, string(data))
		}
		entries = append(entries, entry)
	}
	return entries
}

func assertLogEntry(t *testing.T, entries []map[string]any, msg string, fields map[string]any) {
	t.Helper()
	for _, entry := range entries {
		if entry["msg"] != msg {
			continue
		}
		for key, want := range fields {
			if got := entry[key]; got != want {
				t.Fatalf("log entry %q field %q = %#v, want %#v; entry=%#v", msg, key, got, want, entry)
			}
		}
		if entry["level"] != slog.LevelInfo.String() {
			t.Fatalf("log entry %q level = %#v, want %s", msg, entry["level"], slog.LevelInfo.String())
		}
		return
	}
	t.Fatalf("log entries missing msg %q; entries=%#v", msg, entries)
}
