package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestAuditOriginColumnExistsAfterInit(t *testing.T) {
	if !columnExists(t, openTestRepo(t), "audit_events", "origin") {
		t.Fatal("missing column audit_events.origin")
	}
}

func TestAuditEventOriginRoundTrips(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	events := []domain.AuditEvent{
		{ID: "e1", RequestID: "c1", SubjectType: "tool", SubjectID: "read_file",
			Action: "tool_executed", Hash: "a1", CreatedAt: now, Origin: "plugin:jira"},
		{ID: "e2", RequestID: "c2", SubjectType: "tool", SubjectID: "read_file",
			Action: "tool_executed", Hash: "a1", CreatedAt: now.Add(time.Second),
			Origin: domain.DelegateOrigin(1)},
		// An unset origin must land as the agent default, never as a blank a
		// forensic query would have to guess at.
		{ID: "e3", RequestID: "c3", SubjectType: "tool", SubjectID: "read_file",
			Action: "tool_executed", Hash: "a1", CreatedAt: now.Add(2 * time.Second)},
	}
	for _, ev := range events {
		if err := repo.AppendAuditEvent(ctx, ev); err != nil {
			t.Fatalf("AppendAuditEvent %s: %v", ev.ID, err)
		}
	}

	stored, err := repo.ListAuditEvents(ctx)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	got := make(map[string]string, len(stored))
	for _, ev := range stored {
		got[ev.ID] = ev.Origin
	}
	for id, want := range map[string]string{
		"e1": "plugin:jira",
		"e2": "delegate:depth-1",
		"e3": domain.OriginAgent,
	} {
		if got[id] != want {
			t.Errorf("event %s origin = %q, want %q", id, got[id], want)
		}
	}
}

// Rows written before attribution existed were all agent-initiated: the
// migration must backfill them as such rather than leaving a blank.
func TestAuditOriginMigrationBackfillsExistingRows(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()

	if _, err := repo.db.ExecContext(ctx, `ALTER TABLE audit_events DROP COLUMN origin`); err != nil {
		t.Fatalf("drop origin to simulate an older database: %v", err)
	}
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO audit_events (id, request_id, subject_type, subject_id, action, hash, created_at)
		VALUES ('old', 'c0', 'tool', 'read_file', 'tool_executed', 'a1', ?)
	`, formatTime(time.Now().UTC())); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	if err := repo.applyColumnMigrations(ctx); err != nil {
		t.Fatalf("applyColumnMigrations: %v", err)
	}

	stored, err := repo.ListAuditEvents(ctx)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	for _, ev := range stored {
		if ev.ID == "old" {
			if ev.Origin != domain.OriginAgent {
				t.Fatalf("legacy row origin = %q, want %q", ev.Origin, domain.OriginAgent)
			}
			return
		}
	}
	t.Fatal("legacy row missing after migration")
}
