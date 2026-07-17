package memory

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
)

func TestEpisodicMemoryStoreSearchUsesEmbeddingTopK(t *testing.T) {
	ctx := context.Background()
	store := NewEpisodicMemoryStore(adapter.KeywordEmbeddingProvider{})
	agent := domain.Agent{ID: "agent-1"}

	_, err := store.Add(ctx, agent, domain.Task{ID: "task-1"}, "approval gate waits for human review")
	if err != nil {
		t.Fatalf("add approval memory: %v", err)
	}
	want, err := store.Add(ctx, agent, domain.Task{ID: "task-2"}, "scheduler reaps stale locks before dispatch")
	if err != nil {
		t.Fatalf("add scheduler memory: %v", err)
	}
	_, err = store.Add(ctx, agent, domain.Task{ID: "task-3"}, "working memory keeps short lived context")
	if err != nil {
		t.Fatalf("add working memory: %v", err)
	}

	got, err := store.Search(ctx, "scheduler lock", 1)
	if err != nil {
		t.Fatalf("search episodic memory: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected one result, got %d", len(got))
	}
	if got[0].ID != want.ID {
		t.Fatalf("expected %q first, got %q", want.ID, got[0].ID)
	}
}

func TestEpisodicMemoryStoreFallsBackWithoutEmbeddingProvider(t *testing.T) {
	ctx := context.Background()
	store := NewEpisodicMemoryStore(nil)
	agent := domain.Agent{ID: "agent-1"}

	want, err := store.Add(ctx, agent, domain.Task{ID: "task-1"}, "approval gate requires explicit decision")
	if err != nil {
		t.Fatalf("add approval memory: %v", err)
	}
	_, err = store.Add(ctx, agent, domain.Task{ID: "task-2"}, "scheduler owns task dispatch")
	if err != nil {
		t.Fatalf("add scheduler memory: %v", err)
	}

	got, err := store.Search(ctx, "approval", 3)
	if err != nil {
		t.Fatalf("search fallback memory: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected one fallback match, got %d", len(got))
	}
	if got[0].ID != want.ID {
		t.Fatalf("expected %q, got %q", want.ID, got[0].ID)
	}
}

func TestEpisodicMemoryStoreSearchHonorsTopK(t *testing.T) {
	ctx := context.Background()
	store := NewEpisodicMemoryStore(adapter.KeywordEmbeddingProvider{})
	agent := domain.Agent{ID: "agent-1"}

	for _, content := range []string{
		"scheduler dispatch policy",
		"scheduler lock cleanup",
		"scheduler event stream",
	} {
		if _, err := store.Add(ctx, agent, domain.Task{ID: content}, content); err != nil {
			t.Fatalf("add memory %q: %v", content, err)
		}
	}

	got, err := store.Search(ctx, "scheduler", 2)
	if err != nil {
		t.Fatalf("search top k: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected top 2 results, got %d", len(got))
	}
}
