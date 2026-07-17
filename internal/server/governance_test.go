package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/storage"
)

func TestHTTPAuditEventsRequireAdminRole(t *testing.T) {
	t.Parallel()
	audit := adapter.NewMemoryAuditLog()
	if err := audit.Append(t.Context(), domain.AuditEvent{
		ID:          "audit-1",
		RequestID:   "req-1",
		SubjectType: "task",
		SubjectID:   "task-1",
		Action:      "task.created",
		Hash:        "hash",
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("Append(audit-1) error = %v, want nil", err)
	}
	srv := NewHTTPServer(Config{AdminToken: "token", Audit: audit})

	req := httptest.NewRequest(http.MethodGet, "/v1/audit-events", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	req.Header.Set("X-Role", "viewer")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/audit-events viewer status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/audit-events", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	req.Header.Set("X-Role", "admin")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/audit-events admin status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "task.created") {
		t.Fatalf("GET /v1/audit-events body = %s, want audit action", rec.Body.String())
	}
}

func TestHTTPQualityEvalsAllowViewerRole(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := openServerSQLiteRepository(t)
	if err := repo.AppendQualityEvalRun(ctx, quality.EvalRunRecord{
		ID:        "eval-1",
		AgentID:   "agent-1",
		TaskID:    "task-1",
		Component: "planner",
		Status:    quality.EvalNormal,
		Score:     1,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AppendQualityEvalRun(eval-1) error = %v, want nil", err)
	}
	srv := NewHTTPServer(Config{AdminToken: "token", QualityEvals: repo})

	req := httptest.NewRequest(http.MethodGet, "/v1/quality/evals?agent_id=agent-1", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	req.Header.Set("X-Role", "viewer")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/quality/evals viewer status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "eval-1") {
		t.Fatalf("GET /v1/quality/evals body = %s, want eval record", rec.Body.String())
	}
}

func openServerSQLiteRepository(t *testing.T) *storage.SQLiteRepository {
	t.Helper()
	repo, err := storage.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite(temp agent.db) error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close(SQLiteRepository) error = %v, want nil", err)
		}
	})
	return repo
}
