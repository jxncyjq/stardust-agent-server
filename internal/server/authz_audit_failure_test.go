package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/task"
)

// failingAuditLog is an audit backend whose every write fails, standing in for a
// broken SQLite audit store. The access-denied paths must still deny, and must
// not let the write failure disappear.
type failingAuditLog struct {
	err error
}

func (f failingAuditLog) Append(context.Context, domain.AuditEvent) error {
	return f.err
}

func (f failingAuditLog) Events() []domain.AuditEvent {
	return nil
}

func TestHTTPCrossCompanyDenialLogsAuditAppendFailure(t *testing.T) {
	ctx := context.Background()
	scheduler := task.NewScheduler()
	if err := scheduler.Add(ctx, domain.Task{
		ID:        "task-1",
		CompanyID: "company-a",
		AgentID:   "agent-1",
		Status:    domain.TaskPending,
		Input:     "company a task",
		CreatedAt: time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Scheduler.Add(%q) error = %v, want nil", "task-1", err)
	}
	var logs bytes.Buffer
	srv := NewHTTPServer(Config{
		Tasks:      scheduler,
		Audit:      failingAuditLog{err: errors.New("audit backend down")},
		AdminToken: "token",
		Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/task-1", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-b")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/tasks/task-1 status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	got := logs.String()
	for _, want := range []string{"level=ERROR", "audit backend down", "access_denied.cross_company"} {
		if !strings.Contains(got, want) {
			t.Fatalf("logger output = %q, want it to contain %q", got, want)
		}
	}
}

func TestHTTPRBACDenialLogsAuditAppendFailure(t *testing.T) {
	var logs bytes.Buffer
	srv := NewHTTPServer(Config{
		AdminToken: "token",
		Audit:      failingAuditLog{err: errors.New("audit backend down")},
		Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-events", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	req.Header.Set("X-Role", "viewer")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/audit-events viewer status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	got := logs.String()
	for _, want := range []string{"level=ERROR", "audit backend down", "access_denied.rbac"} {
		if !strings.Contains(got, want) {
			t.Fatalf("logger output = %q, want it to contain %q", got, want)
		}
	}
}
