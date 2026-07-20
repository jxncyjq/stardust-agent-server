package storage

import (
	"context"
	"path/filepath"
	"testing"
)

// TestSQLiteAdaptersEventsReportReadFailure pins the fail-loud contract of
// port.EventBus.Events / port.AuditLog.Events: when the backing store cannot be
// read, the adapters must return an error. Returning an empty slice with a nil
// error would make a database outage indistinguishable from "nothing has been
// recorded yet", which is exactly the silent fallback the project forbids.
func TestSQLiteAdaptersEventsReportReadFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite(temp agent.db) error = %v, want nil", err)
	}
	// Closing the repository is the cheapest deterministic way to make every
	// subsequent query fail.
	if err := repo.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	events, err := NewSQLiteEventBus(repo).Events()
	if err == nil {
		t.Fatalf("SQLiteEventBus.Events() on closed repo = (%#v, nil), want non-nil error", events)
	}
	if events != nil {
		t.Errorf("SQLiteEventBus.Events() on closed repo returned %#v, want nil slice alongside the error", events)
	}

	audit, err := NewSQLiteAuditLog(repo).Events()
	if err == nil {
		t.Fatalf("SQLiteAuditLog.Events() on closed repo = (%#v, nil), want non-nil error", audit)
	}
	if audit != nil {
		t.Errorf("SQLiteAuditLog.Events() on closed repo returned %#v, want nil slice alongside the error", audit)
	}
}
