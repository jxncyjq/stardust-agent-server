package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/browser"
	"github.com/stardust/legion-agent/internal/storage"
)

func TestSQLiteBrowserStoreBridge(t *testing.T) {
	repo, err := storage.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()
	var store browser.BrowserSessionStore = newSQLiteBrowserStore(repo)

	now := time.Now().UTC().Truncate(time.Second)
	if err := store.Save(browser.SessionRecord{ID: "s1", TaskID: "t", StorageState: `[{"name":"sid"}]`, CreatedAt: now, LastUsedAt: now, Evicted: true}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := store.Get("s1")
	if err != nil || !ok || got.StorageState != `[{"name":"sid"}]` || !got.Evicted {
		t.Fatalf("Get mismatch ok=%v err=%v rec=%+v", ok, err, got)
	}
	if err := store.Touch("s1", "https://x", now.Add(time.Minute)); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if again, _, _ := store.Get("s1"); again.StorageState != `[{"name":"sid"}]` {
		t.Fatalf("Touch clobbered storage state")
	}
}
