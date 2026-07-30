package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/memory"
	"github.com/stardust/legion-agent/internal/storage"
)

// TestPersistentEpisodicStoreRoundTripSQLite proves that
// memory.PersistentEpisodicStore, wired by BuildServeService onto a real
// *storage.SQLiteRepository, actually persists episodic entries across a
// repository reopen rather than only within one process's lifetime.
func TestPersistentEpisodicStoreRoundTripSQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "epi.db")

	repo, err := storage.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	store := memory.NewPersistentEpisodicStore(repo)
	if _, err := store.Add(ctx, domain.Agent{ID: "a1"}, domain.Task{ID: "t1"}, "团体保险 保全服务办理"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen: proves the entry survived on disk, not just in the closed repo's
	// in-memory state.
	repo2, err := storage.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer repo2.Close()

	hits, err := memory.NewPersistentEpisodicStore(repo2).Search(ctx, "保全", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].TaskID != "t1" {
		t.Fatalf("expected persisted entry after reopen, got %+v", hits)
	}
}
