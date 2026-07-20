package taskledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newValidationLedger(t *testing.T) (*Ledger, string) {
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

func validEvent(taskID string) Event {
	return Event{
		EventID:       "evt-" + strings.NewReplacer("/", "_", "\\", "_", ".", "_", " ", "_").Replace(taskID),
		TaskID:        taskID,
		Type:          EventTaskCreated,
		ActorAgentID:  "researcher",
		Status:        "planned",
		Summary:       "seed",
		CreatedAt:     time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
		SchemaVersion: schemaVersion,
	}
}

// Regression (critical): validateEvent only required task_id to be non-empty.
// A task_id containing a path separator therefore landed in the event log, and
// only the projection stage rejected it — permanently, because every later
// Rebuild replays the same bad event and fails again, with no tool able to
// delete it. The check has to happen before the event is durable.
func TestAppendRejectsUnsafeTaskIDBeforePersisting(t *testing.T) {
	t.Parallel()

	for _, taskID := range []string{"../evil", "a/b", `a\b`, "..", "  ", "\ttab"} {
		ledger, root := newValidationLedger(t)
		if _, err := ledger.Append(context.Background(), validEvent(taskID)); err == nil {
			t.Errorf("Append(task_id=%q) error = nil, want a rejection", taskID)
			continue
		}

		// The event must not be on disk: a rejected write that still persists is
		// exactly the poison-pill this prevents.
		eventsDir := filepath.Join(root, "tasks", "events")
		entries, err := os.ReadDir(eventsDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ReadDir(events) error = %v, want nil or not-exist", err)
		}
		for _, entry := range entries {
			data, readErr := os.ReadFile(filepath.Join(eventsDir, entry.Name()))
			if readErr != nil {
				t.Fatalf("ReadFile(%s) error = %v, want nil", entry.Name(), readErr)
			}
			if len(strings.TrimSpace(string(data))) > 0 {
				t.Errorf("Append(task_id=%q) was rejected but still wrote an event: %s", taskID, data)
			}
		}
	}
}

// The existing id format must keep working — the fix is a safety gate, not a
// tightening of the id scheme.
func TestAppendAcceptsConventionalTaskID(t *testing.T) {
	t.Parallel()

	ledger, _ := newValidationLedger(t)
	if _, err := ledger.Append(context.Background(), validEvent("TASK-20260523-101")); err != nil {
		t.Fatalf("Append(conventional task_id) error = %v, want nil", err)
	}
}

// ValidateTaskID is the single gate both the write path and the projection path
// go through; if they ever diverge, an event becomes writable but not
// rebuildable, which is unrecoverable.
func TestValidateTaskIDMatchesProjectionPath(t *testing.T) {
	t.Parallel()

	ledger, _ := newValidationLedger(t)
	for _, taskID := range []string{"../evil", "a/b", `a\b`, "..", " "} {
		if err := ValidateTaskID(taskID); err == nil {
			t.Errorf("ValidateTaskID(%q) = nil, want an error", taskID)
		}
		if _, err := ledger.taskPath(taskID); err == nil {
			t.Errorf("taskPath(%q) = nil error, want the same rejection as ValidateTaskID", taskID)
		}
	}
	if err := ValidateTaskID("TASK-20260523-101"); err != nil {
		t.Errorf("ValidateTaskID(conventional) = %v, want nil", err)
	}
}

// status drives archival (isTerminal compares against DoneStatuses). A typo
// like "donee" silently produced a task that could never be archived.
func TestAppendRejectsUnknownStatus(t *testing.T) {
	t.Parallel()

	ledger, _ := newValidationLedger(t)
	event := validEvent("TASK-20260523-102")
	event.Status = "donee"
	if _, err := ledger.Append(context.Background(), event); err == nil {
		t.Fatalf("Append(status=donee) error = nil, want a rejection")
	}
}

// A bad event that is already on disk (written before the gate existed, or by
// hand) must not brick the ledger: Rebuild skips it and says so, rather than
// failing forever with no way to remove it.
func TestRebuildSkipsAndReportsUnprojectableEvent(t *testing.T) {
	t.Parallel()

	ledger, root := newValidationLedger(t)
	ctx := context.Background()
	if _, err := ledger.Append(ctx, validEvent("TASK-20260523-103")); err != nil {
		t.Fatalf("Append(good) error = %v, want nil", err)
	}

	// Bypass Append to simulate a pre-existing poisoned event.
	eventsDir := filepath.Join(root, "tasks", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		t.Fatalf("ReadDir(events) error = %v, want nil", err)
	}
	if len(entries) == 0 {
		t.Fatalf("no event file was written")
	}
	poisoned := filepath.Join(eventsDir, entries[0].Name())
	line := `{"event_id":"evt-bad","task_id":"../evil","type":"task.created","actor_agent_id":"researcher","status":"planned","summary":"bad","created_at":"2026-07-20T10:00:00Z","schema_version":` + itoa(schemaVersion) + "}\n"
	file, err := os.OpenFile(poisoned, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile(events) error = %v, want nil", err)
	}
	if _, err := file.WriteString(line); err != nil {
		t.Fatalf("WriteString(poison) error = %v, want nil", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close(events) error = %v, want nil", err)
	}

	projection, err := ledger.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Rebuild() error = %v, want nil (a single bad event must not brick the ledger)", err)
	}
	if _, ok := projection.Tasks["../evil"]; ok {
		t.Errorf("the unsafe task_id was projected instead of skipped")
	}
	if _, ok := projection.Tasks["TASK-20260523-103"]; !ok {
		t.Errorf("the good task disappeared along with the bad one")
	}

	// "Skipped" must be visible, not merely recorded in a field nobody reads.
	joined := strings.Join(projection.Diagnostics, "\n")
	if !strings.Contains(joined, "../evil") {
		t.Errorf("Diagnostics does not mention the skipped event: %#v", projection.Diagnostics)
	}
	index, err := os.ReadFile(filepath.Join(root, "tasks.md"))
	if err != nil {
		t.Fatalf("ReadFile(tasks.md) error = %v, want nil", err)
	}
	if !strings.Contains(string(index), "../evil") {
		t.Errorf("tasks.md does not surface the skipped event:\n%s", index)
	}
}

// Regression: ReadEvents swallowed walk errors and returned a partial event
// set, which Rebuild then used to atomically overwrite tasks.md and every
// tasks/*.md — silently erasing history.
func TestReadEventsFailsLoudOnUnreadableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unreadable directories are not reproducible on Windows")
	}
	t.Parallel()

	ledger, root := newValidationLedger(t)
	ctx := context.Background()
	if _, err := ledger.Append(ctx, validEvent("TASK-20260523-104")); err != nil {
		t.Fatalf("Append() error = %v, want nil", err)
	}

	blocked := filepath.Join(root, "tasks", "events", "blocked")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatalf("MkdirAll(blocked) error = %v, want nil", err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatalf("Chmod(blocked) error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	if _, err := ledger.ReadEvents(ctx); err == nil {
		t.Fatalf("ReadEvents() error = nil, want the walk failure surfaced")
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}
