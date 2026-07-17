package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestSQLiteAdaptersRecoverEventsAndAuditAfterReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agent.db")
	repo, err := OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) error = %v, want nil", dbPath, err)
	}
	eventBus := NewSQLiteEventBus(repo)
	auditLog := NewSQLiteAuditLog(repo)
	runtimeEvent := domain.RuntimeEvent{
		Type:      "task_started",
		TaskID:    "task-1",
		Message:   "runtime started",
		CreatedAt: time.Date(2026, 5, 12, 14, 0, 0, 0, time.UTC),
	}
	if err := eventBus.Publish(ctx, runtimeEvent); err != nil {
		t.Fatalf("SQLiteEventBus.Publish(%q) error = %v, want nil", runtimeEvent.Type, err)
	}
	audit := domain.AuditEvent{
		ID:          "audit-1",
		RequestID:   "task-1:run",
		SubjectType: "task",
		SubjectID:   "task-1",
		Action:      "task_started",
		Hash:        "hash-1",
		CreatedAt:   runtimeEvent.CreatedAt,
	}
	if err := auditLog.Append(ctx, audit); err != nil {
		t.Fatalf("SQLiteAuditLog.Append(%q) error = %v, want nil", audit.ID, err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	reopened, err := OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) reopen error = %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("Close() reopened error = %v, want nil", err)
		}
	})
	recoveredEvents := NewSQLiteEventBus(reopened).Events()
	if len(recoveredEvents) != 1 || recoveredEvents[0].Type != runtimeEvent.Type || recoveredEvents[0].TaskID != runtimeEvent.TaskID {
		t.Fatalf("SQLiteEventBus.Events() = %#v, want recovered event %#v", recoveredEvents, runtimeEvent)
	}
	recoveredAudit := NewSQLiteAuditLog(reopened).Events()
	if len(recoveredAudit) != 1 || recoveredAudit[0].ID != audit.ID || recoveredAudit[0].Action != audit.Action {
		t.Fatalf("SQLiteAuditLog.Events() = %#v, want recovered audit %#v", recoveredAudit, audit)
	}
}
