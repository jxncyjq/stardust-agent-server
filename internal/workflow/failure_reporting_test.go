package workflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/task"
)

// Audit item V9: Execute's failure branch was the only one that dropped its
// publish and audit errors —
//
//	_ = e.publish(ctx, def.ID, "workflow_failed", err.Error())
//	_ = e.appendAudit(ctx, def.ID, "workflow_failed")
//
// while every other branch (waiting_approval, waiting_event, compensated,
// completed) returns them. An inconsistent double standard, and on the one
// branch where it matters most: when the sink is down, the last event an
// observer ever sees is workflow_started, so a failed workflow reads as still
// running. Both sinks are SQLite-backed, so they go together.

// failOnTypeEventBus rejects one event type and accepts the rest. Rejecting
// everything would abort at the workflow_started publish instead, never reaching
// the branch under test.
type failOnTypeEventBus struct {
	mu       sync.Mutex
	failType string
	err      error
	events   []domain.RuntimeEvent
}

func (b *failOnTypeEventBus) Publish(_ context.Context, event domain.RuntimeEvent) error {
	if event.Type == b.failType {
		return b.err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, event)
	return nil
}

func (b *failOnTypeEventBus) Events() ([]domain.RuntimeEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]domain.RuntimeEvent(nil), b.events...), nil
}

type appendFailingAuditLog struct{ err error }

func (a appendFailingAuditLog) Append(context.Context, domain.AuditEvent) error { return a.err }
func (appendFailingAuditLog) Events() ([]domain.AuditEvent, error)              { return nil, nil }

func invalidDefinition() Definition {
	return Definition{ID: "wf-1", Root: Node{ID: "root", Kind: NodeKind("bogus")}}
}

func TestExecuteReportsFailedPublishAlongsideWorkflowError(t *testing.T) {
	t.Parallel()

	events := &failOnTypeEventBus{failType: "workflow_failed", err: errors.New("event bus down")}
	engine := NewEngine(Config{
		Scheduler: task.NewScheduler(),
		Approvals: approval.NewService(),
		Events:    events,
		Audit:     adapter.NewMemoryAuditLog(),
	})

	_, err := engine.Execute(context.Background(), invalidDefinition())
	if err == nil {
		t.Fatal("Execute(invalid node) error = nil, want an error")
	}
	// The workflow's own failure must survive — it is the more important one and
	// callers match on it.
	if !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("error = %v, want it to still wrap ErrInvalidNode", err)
	}
	if !strings.Contains(err.Error(), "event bus down") {
		t.Fatalf("error = %v, want it to also carry the publish failure", err)
	}
}

func TestExecuteReportsFailedAuditAlongsideWorkflowError(t *testing.T) {
	t.Parallel()

	engine := NewEngine(Config{
		Scheduler: task.NewScheduler(),
		Approvals: approval.NewService(),
		Events:    adapter.NewMemoryEventBus(),
		Audit:     appendFailingAuditLog{err: errors.New("audit store down")},
	})

	_, err := engine.Execute(context.Background(), invalidDefinition())
	if err == nil {
		t.Fatal("Execute(invalid node) error = nil, want an error")
	}
	if !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("error = %v, want it to still wrap ErrInvalidNode", err)
	}
	if !strings.Contains(err.Error(), "audit store down") {
		t.Fatalf("error = %v, want it to also carry the audit failure", err)
	}
}

// TestExecuteFailureUnchangedWhenSinksHealthy guards the other direction: with
// working sinks the returned error must stay exactly the workflow's own, not get
// wrapped in anything extra.
func TestExecuteFailureUnchangedWhenSinksHealthy(t *testing.T) {
	t.Parallel()

	engine := NewEngine(Config{
		Scheduler: task.NewScheduler(),
		Approvals: approval.NewService(),
		Events:    adapter.NewMemoryEventBus(),
		Audit:     adapter.NewMemoryAuditLog(),
	})

	_, err := engine.Execute(context.Background(), invalidDefinition())
	if !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("error = %v, want ErrInvalidNode", err)
	}
	if strings.Contains(err.Error(), "publish") || strings.Contains(err.Error(), "audit") {
		t.Errorf("error = %v, want no sink noise when both sinks are healthy", err)
	}
}
