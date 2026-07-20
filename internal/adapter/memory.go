package adapter

import (
	"context"
	"sync"

	"github.com/stardust/legion-agent/internal/domain"
)

type MemoryAuditLog struct {
	mu     sync.Mutex
	events []domain.AuditEvent
}

func NewMemoryAuditLog() *MemoryAuditLog {
	return &MemoryAuditLog{}
}

func (l *MemoryAuditLog) Append(ctx context.Context, event domain.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
	return nil
}

// Events returns a copy of the recorded audit events. The in-memory store has
// no failure mode, so the error is always nil; it exists to satisfy the
// port.AuditLog contract that persistent implementations do need.
func (l *MemoryAuditLog) Events() ([]domain.AuditEvent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.AuditEvent(nil), l.events...), nil
}

type MemoryEventBus struct {
	mu     sync.Mutex
	events []domain.RuntimeEvent
}

func NewMemoryEventBus() *MemoryEventBus {
	return &MemoryEventBus{}
}

func (b *MemoryEventBus) Publish(ctx context.Context, event domain.RuntimeEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, event)
	return nil
}

// Events returns a copy of the published runtime events. The in-memory bus has
// no failure mode, so the error is always nil.
func (b *MemoryEventBus) Events() ([]domain.RuntimeEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]domain.RuntimeEvent(nil), b.events...), nil
}
