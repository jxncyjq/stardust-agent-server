package memory

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

type fakePersister struct {
	added  []domain.MemoryEntry
	search func(query string, topK int) ([]domain.MemoryEntry, error)
}

func (f *fakePersister) AddEpisodicMemory(_ context.Context, e domain.MemoryEntry) error {
	f.added = append(f.added, e)
	return nil
}
func (f *fakePersister) SearchEpisodicMemory(_ context.Context, query string, topK int) ([]domain.MemoryEntry, error) {
	if f.search != nil {
		return f.search(query, topK)
	}
	return nil, nil
}

func TestPersistentEpisodicStoreAddGeneratesEntryAndPersists(t *testing.T) {
	fp := &fakePersister{}
	store := NewPersistentEpisodicStore(fp)
	entry, err := store.Add(context.Background(),
		domain.Agent{ID: "a1"}, domain.Task{ID: "t1"}, "团体保险 保全服务")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if entry.ID == "" || entry.AgentID != "a1" || entry.TaskID != "t1" || entry.Content != "团体保险 保全服务" {
		t.Fatalf("entry not populated: %+v", entry)
	}
	if entry.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not set")
	}
	if len(fp.added) != 1 || fp.added[0].ID != entry.ID {
		t.Fatalf("entry not persisted: %+v", fp.added)
	}
}

func TestPersistentEpisodicStoreSearchDelegates(t *testing.T) {
	want := []domain.MemoryEntry{{ID: "e1", Content: "hit"}}
	fp := &fakePersister{search: func(q string, k int) ([]domain.MemoryEntry, error) {
		if q != "保全" || k != 5 {
			t.Fatalf("unexpected search args: %q %d", q, k)
		}
		return want, nil
	}}
	got, err := NewPersistentEpisodicStore(fp).Search(context.Background(), "保全", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].ID != "e1" {
		t.Fatalf("search not delegated: %+v", got)
	}
}

// 编译期断言：两种实现都满足 EpisodicStore
var _ EpisodicStore = (*EpisodicMemoryStore)(nil)
var _ EpisodicStore = (*PersistentEpisodicStore)(nil)
