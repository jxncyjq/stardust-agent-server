package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/stardust/legion-agent/internal/domain"
)

var ErrAuditChainInvalid = errors.New("audit chain invalid")

type HashChainAuditLog struct {
	mu      sync.Mutex
	events  []domain.AuditEvent
	seenIDs map[string]struct{}
}

func NewHashChainAuditLog() *HashChainAuditLog {
	return &HashChainAuditLog{seenIDs: make(map[string]struct{})}
}

func (l *HashChainAuditLog) Append(ctx context.Context, event domain.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.seenIDs[event.ID]; ok {
		return nil
	}
	var previousHash string
	if len(l.events) > 0 {
		previousHash = l.events[len(l.events)-1].Hash
	}
	event.Hash = auditChainHash(previousHash, event)
	l.events = append(l.events, event)
	l.seenIDs[event.ID] = struct{}{}
	return nil
}

func (l *HashChainAuditLog) Events() []domain.AuditEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.AuditEvent(nil), l.events...)
}

func (l *HashChainAuditLog) VerifyChain(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return VerifyAuditEvents(l.events)
}

func VerifyAuditEvents(events []domain.AuditEvent) error {
	var previousHash string
	for _, event := range events {
		expected := auditChainHash(previousHash, event)
		if event.Hash != expected {
			return fmt.Errorf("%w: event %q hash mismatch", ErrAuditChainInvalid, event.ID)
		}
		previousHash = event.Hash
	}
	return nil
}

func auditChainHash(previousHash string, event domain.AuditEvent) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		previousHash,
		event.ID,
		event.RequestID,
		event.SubjectType,
		event.SubjectID,
		event.Action,
		event.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}
