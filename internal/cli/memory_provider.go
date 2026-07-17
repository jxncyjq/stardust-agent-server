package cli

import (
	"context"
	"fmt"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/memory"
)

// episodicMemoryProvider adapts an *memory.EpisodicMemoryStore to the
// cognitive.MemoryProvider interface expected by the cognitive Core. The
// episodic store exposes Add/Search over an embedding index; the Core wants a
// SystemPromptBlock plus a per-task Prefetch. Episodic memory carries no
// agent-scoped system block, so SystemPromptBlock is intentionally empty, and
// Prefetch maps to a bounded similarity Search over the task input.
type episodicMemoryProvider struct {
	store *memory.EpisodicMemoryStore
	topK  int
}

func newEpisodicMemoryProvider(store *memory.EpisodicMemoryStore, topK int) *episodicMemoryProvider {
	if topK <= 0 {
		topK = 3
	}
	return &episodicMemoryProvider{store: store, topK: topK}
}

func (p *episodicMemoryProvider) SystemPromptBlock(ctx context.Context, _ domain.Agent) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", nil
}

func (p *episodicMemoryProvider) Prefetch(ctx context.Context, task domain.Task) ([]domain.MemoryEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := p.store.Search(ctx, task.Input, p.topK)
	if err != nil {
		return nil, fmt.Errorf("search episodic memory for task %q: %w", task.ID, err)
	}
	return entries, nil
}
