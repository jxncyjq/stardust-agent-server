package memory

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

type NoopProvider struct{}

func (NoopProvider) SystemPromptBlock(ctx context.Context, _ domain.Agent) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", nil
}

func (NoopProvider) Prefetch(ctx context.Context, _ domain.Task) ([]domain.MemoryEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, nil
}

func (NoopProvider) SyncAfterTurn(ctx context.Context, _ domain.Agent, _ domain.Task, _ string) error {
	return ctx.Err()
}

type Provider struct {
	mu      sync.Mutex
	nextID  int
	entries []domain.MemoryEntry
}

func NewProvider() *Provider {
	return &Provider{}
}

func (p *Provider) SystemPromptBlock(ctx context.Context, agent domain.Agent) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var parts []string
	for _, entry := range p.entries {
		if entry.AgentID == agent.ID {
			parts = append(parts, entry.Content)
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "Memory:\n- " + strings.Join(parts, "\n- "), nil
}

func (p *Provider) Prefetch(ctx context.Context, task domain.Task) ([]domain.MemoryEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var matches []domain.MemoryEntry
	query := strings.ToLower(task.Input)
	for _, entry := range p.entries {
		if query == "" || strings.Contains(strings.ToLower(entry.Content), query) {
			matches = append(matches, entry)
		}
	}
	return append([]domain.MemoryEntry(nil), matches...), nil
}

func (p *Provider) SyncAfterTurn(ctx context.Context, agent domain.Agent, task domain.Task, content string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	p.entries = append(p.entries, domain.MemoryEntry{
		ID:        "memory-" + strconv.Itoa(p.nextID),
		AgentID:   agent.ID,
		TaskID:    task.ID,
		Content:   content,
		CreatedAt: time.Now(),
	})
	return nil
}
