package memory

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestNoopProviderReturnsEmptyMemory(t *testing.T) {
	t.Parallel()

	provider := NoopProvider{}
	block, err := provider.SystemPromptBlock(context.Background(), domain.Agent{ID: "agent-1"})
	if err != nil {
		t.Fatalf("SystemPromptBlock() error = %v, want nil", err)
	}
	if block != "" {
		t.Errorf("SystemPromptBlock() = %q, want empty", block)
	}

	entries, err := provider.Prefetch(context.Background(), domain.Task{ID: "task-1"})
	if err != nil {
		t.Fatalf("Prefetch() error = %v, want nil", err)
	}
	if len(entries) != 0 {
		t.Errorf("Prefetch() entries = %d, want 0", len(entries))
	}
}

func TestProviderPrefetchesTaskRelevantMemory(t *testing.T) {
	t.Parallel()

	provider := NewProvider()
	if err := provider.SyncAfterTurn(context.Background(), domain.Agent{ID: "agent-1"}, domain.Task{ID: "task-1"}, "scheduler learned"); err != nil {
		t.Fatalf("SyncAfterTurn() error = %v, want nil", err)
	}

	entries, err := provider.Prefetch(context.Background(), domain.Task{ID: "task-2", Input: "scheduler"})
	if err != nil {
		t.Fatalf("Prefetch() error = %v, want nil", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Prefetch() entries = %d, want 1", len(entries))
	}
	if entries[0].Content != "scheduler learned" {
		t.Errorf("Prefetch() content = %q, want %q", entries[0].Content, "scheduler learned")
	}
}
