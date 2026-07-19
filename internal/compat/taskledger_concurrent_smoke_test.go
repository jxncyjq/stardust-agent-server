package compat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/taskledger"
)

func TestTaskLedgerConcurrentCollaborationGolden(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	ledger, err := taskledger.New(taskledger.Config{
		WorkspaceRoot:   root,
		IndexPath:       "tasks.md",
		Root:            "tasks",
		ArchiveRoot:     "tasks/archive",
		MaxIndexLines:   80,
		MaxTaskLines:    120,
		MaxMessageChars: 120,
		AllowedAgentIDs: []string{"cli-agent", "researcher", "writer", "reviewer"},
	})
	if err != nil {
		t.Fatalf("taskledger.New() error = %v, want nil", err)
	}
	base := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	if _, err := ledger.Append(ctx, taskledger.Event{
		EventID:       "evt-task-created",
		TaskID:        "TASK-20260523-900",
		Type:          taskledger.EventTaskCreated,
		Title:         "并发协作 smoke",
		Status:        "in_progress",
		Owner:         "researcher",
		ActorAgentID:  "cli-agent",
		Summary:       "验证并发追加不丢消息",
		CreatedAt:     base,
		SchemaVersion: 1,
	}); err != nil {
		t.Fatalf("Ledger.Append(task.created) error = %v, want nil", err)
	}

	events := []taskledger.Event{
		{
			EventID:        "evt-researcher",
			TaskID:         "TASK-20260523-900",
			Type:           taskledger.EventResultAppended,
			From:           "researcher",
			To:             "writer",
			ActorAgentID:   "researcher",
			Summary:        "research findings are in docs/research/cache.md",
			Artifact:       "docs/research/cache.md",
			CreatedAt:      base.Add(1 * time.Second),
			IdempotencyKey: "researcher-result",
			SchemaVersion:  1,
		},
		{
			EventID:        "evt-writer",
			TaskID:         "TASK-20260523-900",
			Type:           taskledger.EventHandoffAppended,
			From:           "writer",
			To:             "reviewer",
			ActorAgentID:   "writer",
			Summary:        "writer summary ready for review",
			Artifact:       "docs/agents/cache-summary.md",
			CreatedAt:      base.Add(2 * time.Second),
			IdempotencyKey: "writer-handoff",
			SchemaVersion:  1,
		},
		{
			EventID:        "evt-reviewer",
			TaskID:         "TASK-20260523-900",
			Type:           taskledger.EventReviewAppended,
			From:           "reviewer",
			To:             "writer",
			ActorAgentID:   "reviewer",
			Summary:        "review accepted with minor wording notes",
			CreatedAt:      base.Add(3 * time.Second),
			IdempotencyKey: "reviewer-review",
			SchemaVersion:  1,
		},
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, event := range events {
		wg.Go(func() {
			_, err := ledger.Append(ctx, event)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("Ledger.Append(concurrent) errors = %v, want none", errs)
	}

	projection, err := ledger.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Ledger.Rebuild() error = %v, want nil", err)
	}
	task := projection.Tasks["TASK-20260523-900"]
	if len(task.Messages) != 3 {
		t.Fatalf("TaskLedger messages len = %d, want 3: %#v", len(task.Messages), task.Messages)
	}
	eventsAfterReplay, err := ledger.ReadEvents(ctx)
	if err != nil {
		t.Fatalf("Ledger.ReadEvents() error = %v, want nil", err)
	}
	replayed := taskledger.BuildProjection(eventsAfterReplay, taskledger.Config{
		MaxIndexLines:   80,
		MaxTaskLines:    120,
		MaxMessageChars: 120,
		DoneStatuses:    []string{"done", "cancelled"},
	})
	if projection.IndexMarkdown != replayed.IndexMarkdown || projection.TaskMarkdown["TASK-20260523-900"] != replayed.TaskMarkdown["TASK-20260523-900"] {
		t.Fatalf("TaskLedger replay is not deterministic")
	}
	if strings.Contains(projection.IndexMarkdown, "research findings are in") || strings.Contains(projection.IndexMarkdown, "writer summary ready") {
		t.Fatalf("IndexMarkdown leaked message history:\n%s", projection.IndexMarkdown)
	}
	assertGoldenFile(t, "testdata/taskledger-concurrent-index.golden.md", projection.IndexMarkdown)
	assertGoldenFile(t, "testdata/taskledger-concurrent-task.golden.md", projection.TaskMarkdown["TASK-20260523-900"])
}

func assertGoldenFile(t *testing.T, path string, got string) {
	t.Helper()
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v, want nil", path, err)
	}
	normalizedGot := strings.ReplaceAll(got, "\r\n", "\n")
	normalizedWant := strings.ReplaceAll(string(want), "\r\n", "\n")
	if normalizedGot != normalizedWant {
		t.Fatalf("%s mismatch\nwant:\n%s\ngot:\n%s", filepath.Base(path), normalizedWant, normalizedGot)
	}
}
