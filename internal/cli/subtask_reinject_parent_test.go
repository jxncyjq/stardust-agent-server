package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/storage"
)

// Audit item V18: `parentTaskID, _ := agentruntime.ParentTaskIDForSubTask(...)`
// dropped the ok flag.
//
// ParentTaskIDForSubTask returns (s, false) when the id has no ":sub-" segment.
// A subtask_completed event's TaskID is supposed to be a sub-task id, so a false
// there is a broken state — but the code carried on with the degraded value, in
// which parentTaskID is the sub-task's own id. The result message was then
// reinjected to the sub-task itself, the parent's read_messages never saw it, and
// the delegation chain broke silently: the parent agent simply waits for a reply
// that has already been filed somewhere else.

func TestSubtaskReinjectionFailsLoudOnNonSubTaskID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:      "subtask_completed",
		TaskID:    "task-parent-only", // no ":sub-" segment
		Message:   "child finished",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Publish error = %v, want nil", err)
	}

	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteRepository error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	job := newSubtaskReinjectionJob(events, repo)
	err = job(ctx)
	if err == nil {
		t.Fatal("job(non-sub-task id) error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "task-parent-only") {
		t.Errorf("error = %q, want it to name the offending task id", err.Error())
	}
}

// TestSubtaskReinjectionStillReinjectsRealSubTasks guards the other direction:
// a well-formed sub-task id must still be reinjected to its parent.
func TestSubtaskReinjectionStillReinjectsRealSubTasks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:      "subtask_completed",
		TaskID:    "task-parent:sub-1",
		Message:   "child finished",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Publish error = %v, want nil", err)
	}

	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteRepository error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := newSubtaskReinjectionJob(events, repo)(ctx); err != nil {
		t.Fatalf("job(valid sub-task id) error = %v, want nil", err)
	}
	messages, err := repo.ListAgentMessages(ctx, domain.AgentMessageQuery{TaskID: "task-parent", FromAgentID: "delegate-runtime"})
	if err != nil {
		t.Fatalf("ListAgentMessages error = %v, want nil", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages keyed to the parent = %d, want 1", len(messages))
	}
}
