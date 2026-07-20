package taskledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newClaimLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	root := t.TempDir()
	ledger, err := New(Config{
		WorkspaceRoot: root,
		IndexPath:     "tasks.md",
		Root:          "tasks",
		ArchiveRoot:   "tasks/archive",
		DoneStatuses:  []string{"done", "cancelled"},
	})
	if err != nil {
		t.Fatalf("New(ledger) error = %v, want nil", err)
	}
	return ledger, root
}

func claimEvent(taskID, owner, eventID string) Event {
	return Event{
		EventID:       eventID,
		TaskID:        taskID,
		Type:          EventTaskClaimed,
		ActorAgentID:  owner,
		Owner:         owner,
		Summary:       "claim",
		CreatedAt:     time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
		SchemaVersion: schemaVersion,
	}
}

func seedTask(t *testing.T, ledger *Ledger, taskID string) {
	t.Helper()
	event := Event{
		EventID:       "evt-create-" + taskID,
		TaskID:        taskID,
		Type:          EventTaskCreated,
		ActorAgentID:  "researcher",
		Status:        "planned",
		Summary:       "seed",
		CreatedAt:     time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC),
		SchemaVersion: schemaVersion,
	}
	if _, err := ledger.Append(context.Background(), event); err != nil {
		t.Fatalf("Append(create %s) error = %v, want nil", taskID, err)
	}
}

// Regression (P1-1): Append never read the current owner, so two agents racing
// to claim the same task both got a successful result and both events landed.
// The conflict was only noted after the fact in the projection, which the
// caller never sees — so both agents proceeded to do the same work.
func TestClaimIsExclusiveUnderConcurrency(t *testing.T) {
	t.Parallel()

	ledger, _ := newClaimLedger(t)
	seedTask(t, ledger, "TASK-20260720-001")

	const claimants = 8
	var succeeded atomic.Int32
	var duplicates atomic.Int32
	var other atomic.Int32
	var wg sync.WaitGroup

	for i := range claimants {
		wg.Go(func() {
			owner := "agent-" + string(rune('a'+i))
			_, err := ledger.Append(context.Background(), claimEvent("TASK-20260720-001", owner, "evt-claim-"+owner))
			switch {
			case err == nil:
				succeeded.Add(1)
			case errors.Is(err, ErrDuplicateClaim):
				duplicates.Add(1)
			default:
				other.Add(1)
				t.Errorf("Append(claim) unexpected error = %v", err)
			}
		})
	}
	wg.Wait()

	if got := succeeded.Load(); got != 1 {
		t.Errorf("successful claims = %d, want exactly 1", got)
	}
	if got := duplicates.Load(); got != claimants-1 {
		t.Errorf("ErrDuplicateClaim results = %d, want %d", got, claimants-1)
	}
	if got := other.Load(); got != 0 {
		t.Errorf("unexpected errors = %d, want 0", got)
	}
}

// Re-claiming a task one already owns is a retry, not a conflict: a caller that
// lost its response must be able to repeat the call safely.
func TestClaimByCurrentOwnerIsIdempotent(t *testing.T) {
	t.Parallel()

	ledger, _ := newClaimLedger(t)
	seedTask(t, ledger, "TASK-20260720-002")
	ctx := context.Background()

	if _, err := ledger.Append(ctx, claimEvent("TASK-20260720-002", "researcher", "evt-claim-1")); err != nil {
		t.Fatalf("Append(first claim) error = %v, want nil", err)
	}
	if _, err := ledger.Append(ctx, claimEvent("TASK-20260720-002", "researcher", "evt-claim-2")); err != nil {
		t.Fatalf("Append(re-claim by same owner) error = %v, want nil", err)
	}
}

// The owner index must survive a restart: it is derived from the event log, so
// a fresh Ledger over the same directory has to reach the same conclusion.
func TestClaimExclusivitySurvivesReopen(t *testing.T) {
	t.Parallel()

	ledger, root := newClaimLedger(t)
	seedTask(t, ledger, "TASK-20260720-003")
	ctx := context.Background()
	if _, err := ledger.Append(ctx, claimEvent("TASK-20260720-003", "researcher", "evt-claim-1")); err != nil {
		t.Fatalf("Append(claim) error = %v, want nil", err)
	}

	reopened, err := New(Config{
		WorkspaceRoot: root,
		IndexPath:     "tasks.md",
		Root:          "tasks",
		ArchiveRoot:   "tasks/archive",
		DoneStatuses:  []string{"done", "cancelled"},
	})
	if err != nil {
		t.Fatalf("New(reopened) error = %v, want nil", err)
	}
	_, err = reopened.Append(ctx, claimEvent("TASK-20260720-003", "writer", "evt-claim-2"))
	if !errors.Is(err, ErrDuplicateClaim) {
		t.Fatalf("Append(claim after reopen) error = %v, want ErrDuplicateClaim", err)
	}
}

// Regression (P1-3): the lock file recorded a timestamp that was never read
// back, and O_EXCL had no retry or recovery. A process killed mid-write left
// .lock behind and every subsequent write failed forever.
func TestStaleLockIsReclaimed(t *testing.T) {
	t.Parallel()

	ledger, root := newClaimLedger(t)
	lockPath := filepath.Join(root, "tasks", ".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v, want nil", err)
	}
	// A lock left by a process that died long ago. Staleness is judged by the
	// file's mtime rather than by its contents: a process killed mid-write can
	// leave a truncated or empty stamp, and mtime is set by the filesystem
	// regardless.
	staleAt := time.Now().Add(-24 * time.Hour)
	if err := os.WriteFile(lockPath, []byte(staleAt.Format(time.RFC3339Nano)), 0o644); err != nil {
		t.Fatalf("WriteFile(stale lock) error = %v, want nil", err)
	}
	if err := os.Chtimes(lockPath, staleAt, staleAt); err != nil {
		t.Fatalf("Chtimes(stale lock) error = %v, want nil", err)
	}

	if _, err := ledger.Append(context.Background(), Event{
		EventID:       "evt-after-stale",
		TaskID:        "TASK-20260720-004",
		Type:          EventTaskCreated,
		ActorAgentID:  "researcher",
		Status:        "planned",
		Summary:       "after stale lock",
		CreatedAt:     time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
		SchemaVersion: schemaVersion,
	}); err != nil {
		t.Fatalf("Append() error = %v, want nil (a stale lock must be reclaimed)", err)
	}
}

// A lock held by a live writer must NOT be stolen — otherwise the reclaim logic
// would trade a deadlock for concurrent writers corrupting the log.
func TestFreshLockIsNotStolen(t *testing.T) {
	t.Parallel()

	ledger, root := newClaimLedger(t)
	lockPath := filepath.Join(root, "tasks", ".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v, want nil", err)
	}
	if err := os.WriteFile(lockPath, []byte(time.Now().Format(time.RFC3339Nano)), 0o644); err != nil {
		t.Fatalf("WriteFile(fresh lock) error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = os.Remove(lockPath) })

	if _, err := ledger.Append(context.Background(), Event{
		EventID:       "evt-blocked",
		TaskID:        "TASK-20260720-005",
		Type:          EventTaskCreated,
		ActorAgentID:  "researcher",
		Status:        "planned",
		Summary:       "should not proceed",
		CreatedAt:     time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
		SchemaVersion: schemaVersion,
	}); err == nil {
		t.Fatalf("Append() error = nil, want a failure while another writer holds the lock")
	}
}
