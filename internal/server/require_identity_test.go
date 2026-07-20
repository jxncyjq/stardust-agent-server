package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/task"
)

func requireIdentityScheduler(t *testing.T) *task.Scheduler {
	t.Helper()
	scheduler := task.NewScheduler()
	if err := scheduler.Add(context.Background(), domain.Task{
		ID:        "task-1",
		CompanyID: "company-a",
		AgentID:   "agent-1",
		Status:    domain.TaskPending,
		Input:     "company a task",
		CreatedAt: time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Scheduler.Add(%q) error = %v, want nil", "task-1", err)
	}
	return scheduler
}

// TestHTTPRequireIdentityRejectsMissingCompanyHeader is the tenant half of the
// switch: with identity required, a request carrying no X-Company-ID must be
// denied and the denial must reach the audit store.
func TestHTTPRequireIdentityRejectsMissingCompanyHeader(t *testing.T) {
	t.Parallel()
	audit := adapter.NewMemoryAuditLog()
	srv := NewHTTPServer(Config{
		Tasks:           requireIdentityScheduler(t),
		Audit:           audit,
		AdminToken:      "token",
		RequireIdentity: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/task-1", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/tasks/task-1 status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "company access denied") {
		t.Fatalf("GET /v1/tasks/task-1 body = %s, want %q", rec.Body.String(), "company access denied")
	}
	events := audit.Events()
	if len(events) != 1 {
		t.Fatalf("Audit.Events() len = %d, want 1", len(events))
	}
	if events[0].Action != "access_denied.cross_company" {
		t.Fatalf("Audit.Events()[0].Action = %q, want %q", events[0].Action, "access_denied.cross_company")
	}
}

// TestHTTPRequireIdentityRejectsCrossCompanyHeader keeps the pre-existing
// cross-tenant rejection intact once the switch is on.
func TestHTTPRequireIdentityRejectsCrossCompanyHeader(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{
		Tasks:           requireIdentityScheduler(t),
		Audit:           adapter.NewMemoryAuditLog(),
		AdminToken:      "token",
		RequireIdentity: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/task-1", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-b")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/tasks/task-1 status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

// TestHTTPRequireIdentityAllowsMatchingCompanyHeader proves the switch denies
// only anonymous and cross-tenant callers, not legitimate ones.
func TestHTTPRequireIdentityAllowsMatchingCompanyHeader(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{
		Tasks:           requireIdentityScheduler(t),
		AdminToken:      "token",
		RequireIdentity: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/task-1", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-a")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks/task-1 status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestHTTPWithoutRequireIdentityAllowsMissingCompanyHeader pins the default
// (master-compatible) behaviour: no headers still means full access.
func TestHTTPWithoutRequireIdentityAllowsMissingCompanyHeader(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{
		Tasks:      requireIdentityScheduler(t),
		AdminToken: "token",
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/task-1", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks/task-1 status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestHTTPRequireIdentityRejectsMissingRoleHeader is the RBAC half: an absent
// X-Role must stop being an implicit admin grant, and the denial must be
// audited.
func TestHTTPRequireIdentityRejectsMissingRoleHeader(t *testing.T) {
	t.Parallel()
	audit := adapter.NewMemoryAuditLog()
	srv := NewHTTPServer(Config{
		AdminToken:      "token",
		Audit:           audit,
		RequireIdentity: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-events", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/audit-events status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "audit access denied") {
		t.Fatalf("GET /v1/audit-events body = %s, want %q", rec.Body.String(), "audit access denied")
	}
	events := audit.Events()
	if len(events) != 1 {
		t.Fatalf("Audit.Events() len = %d, want 1", len(events))
	}
	if events[0].Action != "access_denied.rbac" {
		t.Fatalf("Audit.Events()[0].Action = %q, want %q", events[0].Action, "access_denied.rbac")
	}
}

// TestHTTPRequireIdentityRejectsMissingRoleOnQuality covers the second RBAC
// call site (quality evals), which viewers may read but anonymous callers may
// not once identity is required.
func TestHTTPRequireIdentityRejectsMissingRoleOnQuality(t *testing.T) {
	t.Parallel()
	audit := adapter.NewMemoryAuditLog()
	srv := NewHTTPServer(Config{
		AdminToken:      "token",
		Audit:           audit,
		QualityEvals:    openServerSQLiteRepository(t),
		RequireIdentity: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/quality/evals", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/quality/evals status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	events := audit.Events()
	if len(events) != 1 {
		t.Fatalf("Audit.Events() len = %d, want 1", len(events))
	}
	if events[0].Action != "access_denied.rbac" {
		t.Fatalf("Audit.Events()[0].Action = %q, want %q", events[0].Action, "access_denied.rbac")
	}
}

// TestHTTPRequireIdentityAllowsAdminRoleHeader shows a valid role still passes.
func TestHTTPRequireIdentityAllowsAdminRoleHeader(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{
		AdminToken:      "token",
		Audit:           adapter.NewMemoryAuditLog(),
		RequireIdentity: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-events", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	req.Header.Set("X-Role", "admin")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/audit-events status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestHTTPRequireIdentityRejectsViewerRoleOnAudit shows an over-reaching but
// non-empty role is still rejected with the switch on.
func TestHTTPRequireIdentityRejectsViewerRoleOnAudit(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{
		AdminToken:      "token",
		Audit:           adapter.NewMemoryAuditLog(),
		RequireIdentity: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-events", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	req.Header.Set("X-Role", "viewer")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/audit-events status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

// TestHTTPWithoutRequireIdentityAllowsMissingRoleHeader pins the default
// behaviour on the RBAC side: no X-Role still reads audit events.
func TestHTTPWithoutRequireIdentityAllowsMissingRoleHeader(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{
		AdminToken: "token",
		Audit:      adapter.NewMemoryAuditLog(),
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-events", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/audit-events status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestNewHTTPServerAnnouncesOptionalIdentityContract makes the optional-identity
// contract visible at assembly time. Without this line the exemption would be
// an invisible default, which is exactly what the fail-loud rule forbids.
func TestNewHTTPServerAnnouncesOptionalIdentityContract(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	NewHTTPServer(Config{
		AdminToken: "token",
		Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	})
	got := logs.String()
	for _, want := range []string{"level=INFO", "identity verification disabled", "server.require_identity=true", "X-Role", "X-Company-ID"} {
		if !strings.Contains(got, want) {
			t.Fatalf("logger output = %q, want it to contain %q", got, want)
		}
	}
}

// TestNewHTTPServerStaysQuietWhenIdentityRequired: the notice exists to flag the
// permissive mode, so it must not fire in the strict one.
func TestNewHTTPServerStaysQuietWhenIdentityRequired(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	NewHTTPServer(Config{
		AdminToken:      "token",
		RequireIdentity: true,
		Logger:          slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if got := logs.String(); strings.Contains(got, "identity verification disabled") {
		t.Fatalf("logger output = %q, want no identity-disabled notice", got)
	}
}
