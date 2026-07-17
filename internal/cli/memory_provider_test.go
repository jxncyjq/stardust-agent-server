package cli

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/memory"
)

// Compile-time assertions that the serve-path memory wiring satisfies the
// cognitive Core provider interfaces. The capability store is used directly;
// the episodic store is wrapped by an adapter.
var (
	_ cognitive.MemoryProvider           = (*episodicMemoryProvider)(nil)
	_ cognitive.CapabilityMemoryProvider = (*memory.CapabilityMemoryStore)(nil)
)

// TestEpisodicMemoryProviderPrefetchRecallsStored verifies the L4 memory
// wiring: an entry added to the episodic store is recalled by Prefetch for a
// related task input, going through the embedding-backed Search.
func TestEpisodicMemoryProviderPrefetchRecallsStored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	store := memory.NewEpisodicMemoryStore(adapter.KeywordEmbeddingProvider{})
	agent := domain.Agent{ID: "default-agent"}
	if _, err := store.Add(ctx, agent, domain.Task{ID: "t1"}, "the scheduler lock was released after the audit"); err != nil {
		t.Fatalf("seed episodic memory: %v", err)
	}

	provider := newEpisodicMemoryProvider(store, 3)
	entries, err := provider.Prefetch(ctx, domain.Task{ID: "t2", Input: "scheduler lock audit"})
	if err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("Prefetch for related input = 0 entries, want the stored memory recalled")
	}
}

// TestEpisodicMemoryProviderPrefetchFailLoud asserts that a cancelled context
// surfaces as an error rather than an empty-result fallback.
func TestEpisodicMemoryProviderPrefetchFailLoud(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	provider := newEpisodicMemoryProvider(memory.NewEpisodicMemoryStore(adapter.KeywordEmbeddingProvider{}), 3)
	if _, err := provider.Prefetch(ctx, domain.Task{ID: "t3", Input: "anything"}); err == nil {
		t.Fatal("Prefetch with cancelled context = nil error, want fail-loud error")
	}
}
