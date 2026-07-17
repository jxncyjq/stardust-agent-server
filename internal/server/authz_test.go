package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/storage"
	"github.com/stardust/legion-agent/internal/task"
	"github.com/stardust/legion-agent/internal/workflow"
)

func TestHTTPRejectsCrossCompanyTaskAccess(t *testing.T) {
	ctx := context.Background()
	scheduler := task.NewScheduler()
	audit := adapter.NewMemoryAuditLog()
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
	srv := NewHTTPServer(Config{
		Tasks:      scheduler,
		Audit:      audit,
		AdminToken: "token",
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/task-1", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-b")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/tasks/task-1 status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	events := audit.Events()
	if len(events) != 1 {
		t.Fatalf("Audit.Events() len = %d, want 1", len(events))
	}
	if events[0].Action != "access_denied.cross_company" {
		t.Fatalf("Audit.Events()[0].Action = %q, want %q", events[0].Action, "access_denied.cross_company")
	}
}

func TestHTTPAllowsSameCompanyTaskAccess(t *testing.T) {
	ctx := context.Background()
	scheduler := task.NewScheduler()
	if err := scheduler.Add(ctx, domain.Task{
		ID:        "task-1",
		CompanyID: "company-a",
		Status:    domain.TaskPending,
		Input:     "company a task",
		CreatedAt: time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Scheduler.Add(%q) error = %v, want nil", "task-1", err)
	}
	srv := NewHTTPServer(Config{
		Tasks:      scheduler,
		AdminToken: "token",
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

func TestHTTPRejectsCrossCompanyWorkflowAccess(t *testing.T) {
	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("SQLiteRepository.Close() error = %v, want nil", err)
		}
	})
	def := workflow.Definition{
		ID:        "workflow-1",
		CompanyID: "company-a",
		Root: workflow.Node{
			ID:        "wait-node",
			Kind:      workflow.NodeWaitEvent,
			EventType: "external.ready",
		},
	}
	result := workflow.Result{
		WorkflowID: def.ID,
		Status:     workflow.StatusWaitingEvent,
		Nodes:      []workflow.NodeResult{{NodeID: "wait-node", Status: workflow.StatusWaitingEvent}},
	}
	if err := repo.SaveWorkflowState(ctx, def, result); err != nil {
		t.Fatalf("SaveWorkflowState(%q) error = %v, want nil", def.ID, err)
	}
	srv := NewHTTPServer(Config{
		WorkflowStates: repo,
		AdminToken:     "token",
		Audit:          storage.NewSQLiteAuditLog(repo),
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/workflows/workflow-1", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-b")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/workflows/workflow-1 status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	audits, err := repo.ListAuditEvents(ctx)
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v, want nil", err)
	}
	if len(audits) != 1 {
		t.Fatalf("ListAuditEvents() len = %d, want 1", len(audits))
	}
	if audits[0].Action != "access_denied.cross_company" {
		t.Fatalf("ListAuditEvents()[0].Action = %q, want %q", audits[0].Action, "access_denied.cross_company")
	}
}
