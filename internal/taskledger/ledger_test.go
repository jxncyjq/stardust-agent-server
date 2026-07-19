package taskledger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLedgerAppendAndRebuild(t *testing.T) {
	root := t.TempDir()
	ledger := newTestLedger(t, root)
	ctx := context.Background()
	base := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{
			EventID:       "evt-1",
			TaskID:        "TASK-20260521-001",
			Type:          EventTaskCreated,
			Title:         "调研任务账本",
			Status:        "planned",
			Owner:         "researcher",
			ActorAgentID:  "researcher",
			Summary:       "建立任务账本",
			CreatedAt:     base,
			SchemaVersion: schemaVersion,
		},
		{
			EventID:        "evt-2",
			TaskID:         "TASK-20260521-001",
			Type:           EventHandoffAppended,
			From:           "researcher",
			To:             "writer",
			ActorAgentID:   "researcher",
			Summary:        "调研完成，证据已写入 docs/research/task-ledger.md",
			Artifact:       "docs/research/task-ledger.md",
			CreatedAt:      base.Add(time.Minute),
			IdempotencyKey: "handoff-1",
			SchemaVersion:  schemaVersion,
		},
	}
	for _, event := range events {
		if _, err := ledger.Append(ctx, event); err != nil {
			t.Fatalf("Ledger.Append(%s) error = %v, want nil", event.EventID, err)
		}
	}
	projection, err := ledger.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Ledger.Rebuild() error = %v, want nil", err)
	}
	task := projection.Tasks["TASK-20260521-001"]
	if task.Title != "调研任务账本" || task.Owner != "researcher" || len(task.Messages) != 1 {
		t.Fatalf("Ledger.Rebuild().Tasks[TASK-20260521-001] = %#v, want title/owner/message", task)
	}
	indexData, err := os.ReadFile(filepath.Join(root, "tasks.md"))
	if err != nil {
		t.Fatalf("ReadFile(tasks.md) error = %v, want nil", err)
	}
	if !strings.Contains(string(indexData), "TASK-20260521-001") || !strings.Contains(string(indexData), "researcher") {
		t.Fatalf("tasks.md = %q, want task id and owner", string(indexData))
	}
	detailData, err := os.ReadFile(filepath.Join(root, "tasks", "TASK-20260521-001.md"))
	if err != nil {
		t.Fatalf("ReadFile(task detail) error = %v, want nil", err)
	}
	if !strings.Contains(string(detailData), "handoff.appended") || !strings.Contains(string(detailData), "docs/research/task-ledger.md") {
		t.Fatalf("task detail = %q, want handoff and artifact", string(detailData))
	}
}

func TestBuildProjectionDeduplicatesEventIDAndIdempotencyKey(t *testing.T) {
	base := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	events := []Event{
		testEvent("evt-1", "key-1", EventMessageAppended, "first", base),
		testEvent("evt-1", "key-2", EventMessageAppended, "duplicate event id", base.Add(time.Second)),
		testEvent("evt-2", "key-1", EventMessageAppended, "duplicate idempotency key", base.Add(2*time.Second)),
		testEvent("evt-3", "key-3", EventMessageAppended, "second", base.Add(3*time.Second)),
	}
	projection := BuildProjection(events, Config{DoneStatuses: []string{"done", "cancelled"}})
	task := projection.Tasks["TASK-20260521-001"]
	if len(task.Messages) != 2 {
		t.Fatalf("BuildProjection(events).Messages len = %d, want 2", len(task.Messages))
	}
	if task.Messages[0].Summary != "first" || task.Messages[1].Summary != "second" {
		t.Fatalf("BuildProjection(events).Messages = %#v, want first and second", task.Messages)
	}
}

func TestBuildProjectionOwnerClaimConflict(t *testing.T) {
	base := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{
			EventID:       "evt-1",
			TaskID:        "TASK-20260521-001",
			Type:          EventTaskClaimed,
			ActorAgentID:  "researcher",
			Owner:         "researcher",
			CreatedAt:     base,
			SchemaVersion: schemaVersion,
		},
		{
			EventID:       "evt-2",
			TaskID:        "TASK-20260521-001",
			Type:          EventTaskClaimed,
			ActorAgentID:  "writer",
			Owner:         "writer",
			Summary:       "writer attempted claim",
			CreatedAt:     base.Add(time.Second),
			SchemaVersion: schemaVersion,
		},
	}
	projection := BuildProjection(events, Config{})
	task := projection.Tasks["TASK-20260521-001"]
	if task.Owner != "researcher" {
		t.Fatalf("BuildProjection(events).Owner = %q, want researcher", task.Owner)
	}
	if len(task.Conflicts) != 1 || task.Conflicts[0].Type != "conflict.owner_claim" {
		t.Fatalf("BuildProjection(events).Conflicts = %#v, want owner claim conflict", task.Conflicts)
	}
	detail := projection.TaskMarkdown["TASK-20260521-001"]
	for _, want := range []string{"conflict.owner_claim", "actor=writer", "owner=writer", "writer attempted claim"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("BuildProjection(events).TaskMarkdown missing %q:\n%s", want, detail)
		}
	}
}

func TestLedgerRebuildArchivesTerminalTasks(t *testing.T) {
	root := t.TempDir()
	ledger := newTestLedger(t, root)
	ctx := context.Background()
	base := time.Date(2026, 5, 23, 9, 0, 0, 0, time.UTC)
	for _, event := range []Event{
		{
			EventID:       "evt-archive-1",
			TaskID:        "TASK-20260523-001",
			Type:          EventTaskCreated,
			Title:         "已完成调研",
			Status:        "planned",
			ActorAgentID:  "researcher",
			Summary:       "需要归档",
			CreatedAt:     base,
			SchemaVersion: schemaVersion,
		},
		{
			EventID:       "evt-archive-2",
			TaskID:        "TASK-20260523-001",
			Type:          EventTaskStatusChanged,
			Status:        "done",
			ActorAgentID:  "writer",
			Summary:       "已完成",
			CreatedAt:     base.Add(time.Minute),
			SchemaVersion: schemaVersion,
		},
	} {
		if _, err := ledger.Append(ctx, event); err != nil {
			t.Fatalf("Ledger.Append(%s) error = %v, want nil", event.EventID, err)
		}
	}
	projection, err := ledger.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Ledger.Rebuild() error = %v, want nil", err)
	}
	if strings.Contains(projection.IndexMarkdown, "TASK-20260523-001") {
		t.Fatalf("Ledger.Rebuild().IndexMarkdown = %q, want terminal task omitted from active index", projection.IndexMarkdown)
	}
	archiveData, err := os.ReadFile(filepath.Join(root, "tasks", "archive", "TASK-20260523-001.md"))
	if err != nil {
		t.Fatalf("ReadFile(archive task detail) error = %v, want nil", err)
	}
	if !strings.Contains(string(archiveData), "| Status | done |") || !strings.Contains(string(archiveData), "已完成") {
		t.Fatalf("archive task detail = %q, want done status and summary", string(archiveData))
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "TASK-20260523-001.md")); !os.IsNotExist(err) {
		t.Fatalf("Stat(active task detail) error = %v, want not exist after archive", err)
	}
}

func TestBuildProjectionAddsSplitHintWhenTaskDetailIsTruncated(t *testing.T) {
	base := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{
			EventID:       "evt-split-0",
			TaskID:        "TASK-20260523-002",
			Type:          EventTaskCreated,
			ActorAgentID:  "researcher",
			Summary:       "长任务",
			CreatedAt:     base,
			SchemaVersion: schemaVersion,
		},
	}
	for i := 1; i <= 12; i++ {
		events = append(events, Event{
			EventID:       "evt-split-" + string(rune('a'+i)),
			TaskID:        "TASK-20260523-002",
			Type:          EventMessageAppended,
			ActorAgentID:  "researcher",
			Summary:       "message line",
			CreatedAt:     base.Add(time.Duration(i) * time.Second),
			SchemaVersion: schemaVersion,
		})
	}
	projection := BuildProjection(events, Config{MaxTaskLines: 12, DoneStatuses: []string{"done", "cancelled"}})
	detail := projection.TaskMarkdown["TASK-20260523-002"]
	for _, want := range []string{"projection exceeded 12 lines", "建议拆分任务或归档长内容到 docs/ / memory/"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("BuildProjection(long task).TaskMarkdown missing %q:\n%s", want, detail)
		}
	}
}

func TestBuildProjectionAddsArchiveHintWhenIndexIsTruncated(t *testing.T) {
	base := time.Date(2026, 5, 23, 11, 0, 0, 0, time.UTC)
	var events []Event
	for i := range 8 {
		events = append(events, Event{
			EventID:       "evt-index-" + string(rune('a'+i)),
			TaskID:        "TASK-20260523-10" + string(rune('0'+i)),
			Type:          EventTaskCreated,
			ActorAgentID:  "researcher",
			Status:        "planned",
			Summary:       "active task",
			CreatedAt:     base.Add(time.Duration(i) * time.Second),
			SchemaVersion: schemaVersion,
		})
	}
	projection := BuildProjection(events, Config{
		MaxIndexLines: 8,
		DoneStatuses:  []string{"done", "cancelled"},
	})
	for _, want := range []string{"tasks.md exceeded 8 lines", "建议归档 done/cancelled 任务或拆分活跃任务摘要"} {
		if !strings.Contains(projection.IndexMarkdown, want) {
			t.Fatalf("BuildProjection(many tasks).IndexMarkdown missing %q:\n%s", want, projection.IndexMarkdown)
		}
	}
}

func TestLedgerConcurrentAppendKeepsAllEvents(t *testing.T) {
	root := t.TempDir()
	ledger := newTestLedger(t, root)
	ctx := context.Background()
	base := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	const total = 12
	var wg sync.WaitGroup
	errs := make(chan error, total)
	for i := range total {
		wg.Go(func() {
			_, err := ledger.Append(ctx, Event{
				EventID:       "evt-concurrent-" + string(rune('a'+i)),
				TaskID:        "TASK-20260521-001",
				Type:          EventMessageAppended,
				ActorAgentID:  "researcher",
				Summary:       "message",
				CreatedAt:     base.Add(time.Duration(i) * time.Second),
				SchemaVersion: schemaVersion,
			})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Ledger.Append(concurrent) error = %v, want nil", err)
		}
	}
	events, err := ledger.ReadEvents(ctx)
	if err != nil {
		t.Fatalf("Ledger.ReadEvents() error = %v, want nil", err)
	}
	if len(events) != total {
		t.Fatalf("Ledger.ReadEvents() len = %d, want %d", len(events), total)
	}
}

func TestLedgerRejectsUnknownAgentAndOutsideArtifact(t *testing.T) {
	ledger := newTestLedger(t, t.TempDir())
	ctx := context.Background()
	_, err := ledger.Append(ctx, Event{
		EventID:       "evt-1",
		TaskID:        "TASK-20260521-001",
		Type:          EventMessageAppended,
		ActorAgentID:  "unknown",
		Summary:       "bad actor",
		CreatedAt:     time.Now(),
		SchemaVersion: schemaVersion,
	})
	if err == nil {
		t.Fatalf("Ledger.Append(unknown agent) error = nil, want error")
	}
	_, err = ledger.Append(ctx, Event{
		EventID:       "evt-2",
		TaskID:        "TASK-20260521-001",
		Type:          EventMessageAppended,
		ActorAgentID:  "researcher",
		Artifact:      filepath.Join("..", "outside.md"),
		Summary:       "bad artifact",
		CreatedAt:     time.Now(),
		SchemaVersion: schemaVersion,
	})
	if err == nil {
		t.Fatalf("Ledger.Append(outside artifact) error = nil, want error")
	}
}

func newTestLedger(t *testing.T, root string) *Ledger {
	t.Helper()
	ledger, err := New(Config{
		WorkspaceRoot:   root,
		IndexPath:       "tasks.md",
		Root:            "tasks",
		ArchiveRoot:     "tasks/archive",
		MaxIndexLines:   500,
		MaxTaskLines:    300,
		MaxMessageChars: 300,
		AllowedAgentIDs: []string{"researcher", "writer"},
	})
	if err != nil {
		t.Fatalf("New(Config) error = %v, want nil", err)
	}
	return ledger
}

func testEvent(eventID, key, eventType, summary string, createdAt time.Time) Event {
	return Event{
		EventID:        eventID,
		TaskID:         "TASK-20260521-001",
		Type:           eventType,
		ActorAgentID:   "researcher",
		IdempotencyKey: key,
		Summary:        summary,
		CreatedAt:      createdAt,
		SchemaVersion:  schemaVersion,
	}
}
