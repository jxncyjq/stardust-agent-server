package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestRepo(t *testing.T) *SQLiteRepository {
	t.Helper()
	repo, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestBrowserSessionSaveGet(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	rec := BrowserSessionRecord{
		ID: "sess-1", TaskID: "t1", ActiveURL: "https://example.com",
		StorageState: `[{"name":"sid","value":"abc"}]`,
		CreatedAt:    now, LastUsedAt: now, Evicted: true,
	}
	if err := repo.SaveBrowserSession(ctx, rec); err != nil {
		t.Fatalf("SaveBrowserSession: %v", err)
	}
	got, ok, err := repo.GetBrowserSession(ctx, "sess-1")
	if err != nil || !ok {
		t.Fatalf("GetBrowserSession ok=%v err=%v", ok, err)
	}
	if got.TaskID != "t1" || got.ActiveURL != "https://example.com" || got.StorageState != rec.StorageState || !got.Evicted {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

// 字段级写穿：更新 last_used_at/active_url 不应清空已存的 storage_state。
func TestBrowserSessionTouchPreservesStorageState(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	_ = repo.SaveBrowserSession(ctx, BrowserSessionRecord{
		ID: "sess-1", TaskID: "t1", StorageState: `[{"name":"sid"}]`, CreatedAt: now, LastUsedAt: now,
	})
	later := now.Add(time.Minute)
	if err := repo.TouchBrowserSession(ctx, "sess-1", "https://x.com", later); err != nil {
		t.Fatalf("TouchBrowserSession: %v", err)
	}
	got, _, _ := repo.GetBrowserSession(ctx, "sess-1")
	if got.StorageState != `[{"name":"sid"}]` {
		t.Fatalf("touch clobbered storage_state: %q", got.StorageState)
	}
	if got.ActiveURL != "https://x.com" || !got.LastUsedAt.Equal(later) {
		t.Fatalf("touch didn't update url/time: %+v", got)
	}
}

func TestBrowserSessionListAndDelete(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	_ = repo.SaveBrowserSession(ctx, BrowserSessionRecord{ID: "a", CreatedAt: now, LastUsedAt: now})
	_ = repo.SaveBrowserSession(ctx, BrowserSessionRecord{ID: "b", CreatedAt: now, LastUsedAt: now})
	list, err := repo.ListBrowserSessions(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list len=%d err=%v", len(list), err)
	}
	if err := repo.DeleteBrowserSession(ctx, "a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := repo.GetBrowserSession(ctx, "a"); ok {
		t.Fatal("expected a deleted")
	}
}
