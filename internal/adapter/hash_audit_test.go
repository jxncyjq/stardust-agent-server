package adapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestHashChainAuditLogAppendsVerifiableChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	log := NewHashChainAuditLog()

	first := auditEvent("audit-1", "task_started")
	second := auditEvent("audit-2", "task_completed")
	if err := log.Append(ctx, first); err != nil {
		t.Fatalf("Append(%q) error = %v, want nil", first.ID, err)
	}
	if err := log.Append(ctx, second); err != nil {
		t.Fatalf("Append(%q) error = %v, want nil", second.ID, err)
	}

	events := log.Events()
	if len(events) != 2 {
		t.Fatalf("Events() len = %d, want 2", len(events))
	}
	if events[0].Hash == "" {
		t.Fatalf("Events()[0].Hash = empty, want chain hash")
	}
	if events[0].Hash == events[1].Hash {
		t.Fatalf("Events() hashes both %q, want changing chain hash", events[0].Hash)
	}
	if err := log.VerifyChain(ctx); err != nil {
		t.Fatalf("VerifyChain() error = %v, want nil", err)
	}
	if err := VerifyAuditEvents(events); err != nil {
		t.Fatalf("VerifyAuditEvents(events) error = %v, want nil", err)
	}
}

func TestHashChainAuditLogIsIdempotentByID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	log := NewHashChainAuditLog()

	event := auditEvent("audit-1", "task_started")
	if err := log.Append(ctx, event); err != nil {
		t.Fatalf("Append(%q) error = %v, want nil", event.ID, err)
	}
	retry := auditEvent("audit-1", "tampered_retry")
	if err := log.Append(ctx, retry); err != nil {
		t.Fatalf("Append(%q retry) error = %v, want nil", retry.ID, err)
	}

	events := log.Events()
	if len(events) != 1 {
		t.Fatalf("Events() len = %d, want 1", len(events))
	}
	if events[0].Action != event.Action {
		t.Fatalf("Events()[0].Action = %q, want original %q", events[0].Action, event.Action)
	}
}

func TestVerifyAuditEventsDetectsTampering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	log := NewHashChainAuditLog()

	first := auditEvent("audit-1", "task_started")
	second := auditEvent("audit-2", "task_completed")
	if err := log.Append(ctx, first); err != nil {
		t.Fatalf("Append(%q) error = %v, want nil", first.ID, err)
	}
	if err := log.Append(ctx, second); err != nil {
		t.Fatalf("Append(%q) error = %v, want nil", second.ID, err)
	}

	events := log.Events()
	events[0].Action = "task_deleted"
	err := VerifyAuditEvents(events)
	if !errors.Is(err, ErrAuditChainInvalid) {
		t.Fatalf("VerifyAuditEvents(tampered) error = %v, want %v", err, ErrAuditChainInvalid)
	}
}

func TestHashChainAuditLogEventsReturnsCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	log := NewHashChainAuditLog()

	event := auditEvent("audit-1", "task_started")
	if err := log.Append(ctx, event); err != nil {
		t.Fatalf("Append(%q) error = %v, want nil", event.ID, err)
	}
	events := log.Events()
	events[0].Action = "changed outside"

	if err := log.VerifyChain(ctx); err != nil {
		t.Fatalf("VerifyChain() after external mutation error = %v, want nil", err)
	}
	got := log.Events()[0]
	if got.Action != event.Action {
		t.Fatalf("Events()[0].Action = %q, want %q", got.Action, event.Action)
	}
}

func auditEvent(id string, action string) domain.AuditEvent {
	return domain.AuditEvent{
		ID:          id,
		RequestID:   "request-1",
		SubjectType: "task",
		SubjectID:   "task-1",
		Action:      action,
		CreatedAt:   time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
	}
}
