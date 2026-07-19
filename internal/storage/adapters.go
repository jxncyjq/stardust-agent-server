package storage

import (
	"context"
	"fmt"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

var (
	_ port.EventBus = (*SQLiteEventBus)(nil)
	_ port.AuditLog = (*SQLiteAuditLog)(nil)
)

type SQLiteEventBus struct {
	repo *SQLiteRepository
}

func NewSQLiteEventBus(repo *SQLiteRepository) *SQLiteEventBus {
	return &SQLiteEventBus{repo: repo}
}

func (b *SQLiteEventBus) Publish(ctx context.Context, event domain.RuntimeEvent) error {
	return b.repo.AppendRuntimeEvent(ctx, event)
}

func (b *SQLiteEventBus) Events() ([]domain.RuntimeEvent, error) {
	events, err := b.repo.ListRuntimeEvents(context.Background())
	if err != nil {
		return nil, fmt.Errorf("list runtime events: %w", err)
	}
	return events, nil
}

type SQLiteAuditLog struct {
	repo *SQLiteRepository
}

func NewSQLiteAuditLog(repo *SQLiteRepository) *SQLiteAuditLog {
	return &SQLiteAuditLog{repo: repo}
}

func (l *SQLiteAuditLog) Append(ctx context.Context, event domain.AuditEvent) error {
	return l.repo.AppendAuditEvent(ctx, event)
}

func (l *SQLiteAuditLog) Events() ([]domain.AuditEvent, error) {
	events, err := l.repo.ListAuditEvents(context.Background())
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	return events, nil
}
