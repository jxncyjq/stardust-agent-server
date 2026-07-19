package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSQLiteBinderBindResolvePersists(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")

	binder, err := OpenSQLiteBinder(ctx, path)
	if err != nil {
		t.Fatalf("OpenSQLiteBinder() error = %v, want nil", err)
	}
	// Miss before bind.
	if _, _, ok, err := binder.Resolve(ctx, "telegram:42"); err != nil || ok {
		t.Fatalf("Resolve(before) = ok %v err %v, want miss", ok, err)
	}
	if err := binder.Bind(ctx, "telegram:42", "session-1", "42"); err != nil {
		t.Fatalf("Bind() error = %v, want nil", err)
	}
	sid, raw, ok, err := binder.Resolve(ctx, "telegram:42")
	if err != nil || !ok || sid != "session-1" || raw != "42" {
		t.Fatalf("Resolve(after) = %q %q %v %v, want session-1/42/true", sid, raw, ok, err)
	}
	if err := binder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	// Reopen: binding survives (persistence).
	reopened, err := OpenSQLiteBinder(ctx, path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	sid, _, ok, err = reopened.Resolve(ctx, "telegram:42")
	if err != nil || !ok || sid != "session-1" {
		t.Fatalf("Resolve(reopened) = %q %v %v, want session-1/true", sid, ok, err)
	}
}

// TestSQLiteBinderConcurrentBindsNoBusy drives many goroutines binding and
// resolving at once. Before OpenSQLiteBinder set a busy_timeout and capped the
// pool at one connection, concurrent binds intermittently returned SQLITE_BUSY
// ("database is locked"). The test asserts every bind succeeds, no error is a
// lock-contention error, and every binding resolves back.
func TestSQLiteBinderConcurrentBindsNoBusy(t *testing.T) {
	ctx := context.Background()
	binder, err := OpenSQLiteBinder(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteBinder() error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = binder.Close() })

	const writers = 16
	const perWriter = 25
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				key := fmt.Sprintf("telegram:%d-%d", w, i)
				if err := binder.Bind(ctx, key, fmt.Sprintf("session-%d-%d", w, i), fmt.Sprintf("%d", i)); err != nil {
					errs <- fmt.Errorf("bind %q: %w", key, err)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked") {
			t.Errorf("concurrent bind hit lock contention: %v", err)
			continue
		}
		t.Errorf("concurrent bind failed: %v", err)
	}

	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			key := fmt.Sprintf("telegram:%d-%d", w, i)
			sid, _, ok, err := binder.Resolve(ctx, key)
			if err != nil || !ok || sid != fmt.Sprintf("session-%d-%d", w, i) {
				t.Errorf("Resolve(%q) = %q %v %v, want session-%d-%d/true", key, sid, ok, err, w, i)
			}
		}
	}
}
