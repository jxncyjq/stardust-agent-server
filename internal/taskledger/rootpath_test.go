package taskledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// escapingLedger builds a Ledger whose Root points outside the workspace,
// bypassing New's up-front validation the way a mutated/loaded config or a
// future construction path could.
func escapingLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	workspace := t.TempDir()
	outside := filepath.Join(workspace, "..", "outside-ledger")
	return &Ledger{cfg: Config{
		WorkspaceRoot:    workspace,
		IndexPath:        "tasks.md",
		Root:             outside,
		Now:              time.Now,
		EventIDGenerator: defaultEventID,
	}}, outside
}

func TestLedgerRootPathFailsLoudOutsideWorkspace(t *testing.T) {
	ledger, _ := escapingLedger(t)

	path, err := ledger.rootPath()

	if err == nil {
		t.Fatalf("rootPath() error = nil, want sandbox violation")
	}
	if path != "" {
		t.Fatalf("rootPath() path = %q, want empty on error", path)
	}
	if !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("rootPath() error = %q, want it to name the sandbox violation", err)
	}
}

func TestLedgerEventsRootPropagatesRootPathError(t *testing.T) {
	ledger, _ := escapingLedger(t)

	path, err := ledger.eventsRoot()

	if err == nil {
		t.Fatalf("eventsRoot() error = nil, want sandbox violation")
	}
	if path != "" {
		t.Fatalf("eventsRoot() path = %q, want empty on error", path)
	}
}

func TestLedgerEventPathPropagatesRootPathError(t *testing.T) {
	ledger, _ := escapingLedger(t)

	path, err := ledger.eventPath(time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC))

	if err == nil {
		t.Fatalf("eventPath() error = nil, want sandbox violation")
	}
	if path != "" {
		t.Fatalf("eventPath() path = %q, want empty on error", path)
	}
}

// TestLedgerAppendFailsLoudWhenRootEscapesWorkspace is the end-to-end guard: a
// swallowed rootPath error used to make Append write the lock and event files to
// the process working directory instead of failing. Append fails at acquireLock,
// the first rootPath caller; eventPath's own propagation is covered separately by
// TestLedgerEventPathPropagatesRootPathError.
func TestLedgerAppendFailsLoudWhenRootEscapesWorkspace(t *testing.T) {
	ledger, outside := escapingLedger(t)

	_, err := ledger.Append(context.Background(), Event{
		EventID:   "evt-1",
		Type:      EventTaskCreated,
		TaskID:    "TASK-1",
		CreatedAt: time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC),
	})

	if err == nil {
		t.Fatalf("Append() error = nil, want sandbox violation")
	}
	if !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("Append() error = %q, want it to name the sandbox violation", err)
	}
	// The point of failing loud is that nothing gets written outside the sandbox.
	if _, statErr := os.Stat(outside); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Stat(%q) error = %v, want the escaping root to stay uncreated", outside, statErr)
	}
}

func TestLedgerReadEventsFailsLoudWhenRootEscapesWorkspace(t *testing.T) {
	ledger, _ := escapingLedger(t)

	events, err := ledger.ReadEvents(context.Background())

	if err == nil {
		t.Fatalf("ReadEvents() error = nil, want sandbox violation")
	}
	if events != nil {
		t.Fatalf("ReadEvents() events = %v, want nil on error", events)
	}
}
