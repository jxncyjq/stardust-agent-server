package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

var errEventStoreUnavailable = errors.New("event store unavailable")

// failingAuditLog / failingEventBus model a backing store whose reads fail (a
// closed SQLite handle, a broken connection). They exist to pin the handler
// behaviour on that path.
type failingAuditLog struct{}

func (failingAuditLog) Append(context.Context, domain.AuditEvent) error { return nil }

func (failingAuditLog) Events() ([]domain.AuditEvent, error) {
	return nil, errEventStoreUnavailable
}

type failingEventBus struct{}

func (failingEventBus) Publish(context.Context, domain.RuntimeEvent) error { return nil }

func (failingEventBus) Events() ([]domain.RuntimeEvent, error) {
	return nil, errEventStoreUnavailable
}

var (
	_ port.AuditLog = failingAuditLog{}
	_ port.EventBus = failingEventBus{}
)

// TestAuditEventsReportStoreReadFailure pins the fail-loud contract of
// /v1/audit-events: an unreadable audit store must surface as 500, never as a
// 200 with an empty list. An audit trail that reads as empty because the query
// failed is worse than one that reads as broken.
func TestAuditEventsReportStoreReadFailure(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{AdminToken: "token", Audit: failingAuditLog{}})

	req := httptest.NewRequest(http.MethodGet, "/v1/audit-events", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	req.Header.Set("X-Role", "admin")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /v1/audit-events with failing store status = %d, want %d body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if body := rec.Body.String(); body == "" || body == "[]\n" {
		t.Fatalf("GET /v1/audit-events with failing store body = %q, want an error payload rather than an empty list", body)
	}
}

// TestRuntimeEventsReportStoreReadFailure pins the same contract for
// /v1/runtime-events: the status panel must see a failure, not a silently empty
// stream, when the event bus cannot be read.
func TestRuntimeEventsReportStoreReadFailure(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{AdminToken: "token", WorkflowEvents: failingEventBus{}})

	req := httptest.NewRequest(http.MethodGet, "/v1/runtime-events", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	req.Header.Set("X-Role", "admin")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /v1/runtime-events with failing bus status = %d, want %d body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if body := rec.Body.String(); body == "" || body == "[]\n" {
		t.Fatalf("GET /v1/runtime-events with failing bus body = %q, want an error payload rather than an empty list", body)
	}
}
