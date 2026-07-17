package tool

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/taskledger"
)

func TestRegistryTaskLedgerToolsCreateAppendReadAndRebuild(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ledger, err := taskledger.New(taskledger.Config{
		WorkspaceRoot:   root,
		IndexPath:       "tasks.md",
		Root:            "tasks",
		ArchiveRoot:     filepath.Join("tasks", "archive"),
		AllowedAgentIDs: []string{"root", "researcher", "writer"},
	})
	if err != nil {
		t.Fatalf("taskledger.New() error = %v, want nil", err)
	}
	registry := NewWorkspaceRegistry(root, nil)
	RegisterTaskLedgerTools(registry, ledger)
	agent := domain.Agent{ID: "root", Role: "developer"}

	for _, call := range []domain.ToolCall{
		{
			ID:   "create-1",
			Name: "create_task",
			Arguments: map[string]string{
				"task_id": "TASK-20260522-001",
				"title":   "整理 tasks 工具",
				"summary": "准备交接给 writer",
				"owner":   "researcher",
			},
		},
		{
			ID:   "handoff-1",
			Name: "append_task_message",
			Arguments: map[string]string{
				"task_id":  "TASK-20260522-001",
				"type":     "handoff",
				"from":     "researcher",
				"to":       "writer",
				"summary":  "工具设计已完成",
				"artifact": "docs/research/tasks-tools.md",
			},
		},
	} {
		result, execErr := registry.Execute(context.Background(), agent, call)
		if execErr != nil {
			t.Fatalf("Registry.Execute(%s) error = %v, want nil", call.Name, execErr)
		}
		if !result.Success {
			t.Fatalf("Registry.Execute(%s).Success = false, want true", call.Name)
		}
	}

	read, err := registry.Execute(context.Background(), agent, domain.ToolCall{
		ID:        "read-1",
		Name:      "read_task",
		Arguments: map[string]string{"task_id": "TASK-20260522-001"},
	})
	if err != nil {
		t.Fatalf("Registry.Execute(read_task) error = %v, want nil", err)
	}
	for _, want := range []string{"TASK-20260522-001", "整理 tasks 工具", "handoff.appended", "工具设计已完成"} {
		if !strings.Contains(read.Output, want) {
			t.Fatalf("Registry.Execute(read_task).Output missing %q:\n%s", want, read.Output)
		}
	}

	rebuild, err := registry.Execute(context.Background(), agent, domain.ToolCall{
		ID:        "rebuild-1",
		Name:      "rebuild_tasks",
		Arguments: map[string]string{},
	})
	if err != nil {
		t.Fatalf("Registry.Execute(rebuild_tasks) error = %v, want nil", err)
	}
	if !strings.Contains(rebuild.Output, "rebuilt 1 task") {
		t.Fatalf("Registry.Execute(rebuild_tasks).Output = %q, want task count", rebuild.Output)
	}
}

func TestRegistryTaskLedgerToolsClaimAndUpdateTask(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ledger, err := taskledger.New(taskledger.Config{WorkspaceRoot: root, AllowedAgentIDs: []string{"writer"}})
	if err != nil {
		t.Fatalf("taskledger.New() error = %v, want nil", err)
	}
	registry := NewWorkspaceRegistry(root, nil)
	RegisterTaskLedgerTools(registry, ledger)
	agent := domain.Agent{ID: "writer", Role: "developer"}
	_, err = registry.Execute(context.Background(), agent, domain.ToolCall{
		ID: "create-1", Name: "create_task",
		Arguments: map[string]string{"task_id": "TASK-20260522-002", "title": "写说明"},
	})
	if err != nil {
		t.Fatalf("Registry.Execute(create_task) error = %v, want nil", err)
	}
	_, err = registry.Execute(context.Background(), agent, domain.ToolCall{
		ID: "claim-1", Name: "claim_task",
		Arguments: map[string]string{"task_id": "TASK-20260522-002", "owner": "writer"},
	})
	if err != nil {
		t.Fatalf("Registry.Execute(claim_task) error = %v, want nil", err)
	}
	_, err = registry.Execute(context.Background(), agent, domain.ToolCall{
		ID: "status-1", Name: "update_task",
		Arguments: map[string]string{"task_id": "TASK-20260522-002", "status": "in_progress", "summary": "正在整理"},
	})
	if err != nil {
		t.Fatalf("Registry.Execute(update_task) error = %v, want nil", err)
	}
	read, err := registry.Execute(context.Background(), agent, domain.ToolCall{
		ID: "read-1", Name: "read_task", Arguments: map[string]string{"task_id": "TASK-20260522-002"},
	})
	if err != nil {
		t.Fatalf("Registry.Execute(read_task) error = %v, want nil", err)
	}
	if !strings.Contains(read.Output, "| Status | in_progress |") || !strings.Contains(read.Output, "| Owner | writer |") {
		t.Fatalf("Registry.Execute(read_task).Output = %q, want owner and updated status", read.Output)
	}
}

func TestWorkspaceRegistryTaskLedgerToolSchemasAreOpenAICompatibleObjects(t *testing.T) {
	t.Parallel()

	ledger, err := taskledger.New(taskledger.Config{WorkspaceRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("taskledger.New() error = %v, want nil", err)
	}
	registry := NewWorkspaceRegistry(t.TempDir(), nil)
	RegisterTaskLedgerTools(registry, ledger)

	want := map[string]bool{
		"create_task":         false,
		"claim_task":          false,
		"update_task":         false,
		"append_task_message": false,
		"read_task":           false,
		"rebuild_tasks":       false,
	}
	for _, descriptor := range registry.Descriptors() {
		if _, ok := want[descriptor.Name]; !ok {
			continue
		}
		want[descriptor.Name] = true
		if got, _ := descriptor.InputSchema["type"].(string); got != "object" {
			t.Fatalf("Descriptor(%s).InputSchema[type] = %q, want object", descriptor.Name, got)
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("Registry.Descriptors() missing %s", name)
		}
	}
}
