package gateway

import (
	"context"
	"path/filepath"
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
