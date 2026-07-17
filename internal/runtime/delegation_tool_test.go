package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/tool"
)

func hasDescriptor(registry *tool.Registry, name string) bool {
	for _, d := range registry.Descriptors() {
		if d.Name == name {
			return true
		}
	}
	return false
}

func TestRegisterDelegateTaskToolOnlyForOrchestrators(t *testing.T) {
	t.Parallel()

	maas := &recordingSubMaas{summary: "ok"}

	orchestrator := NewRuntime(Config{Maas: maas})
	orchRegistry := tool.NewRegistry(nil, nil, nil)
	orchestrator.RegisterDelegateTaskTool(orchRegistry)
	if !hasDescriptor(orchRegistry, "delegate_task") {
		t.Fatalf("orchestrator registry missing delegate_task")
	}

	// A leaf child never gets the tool, so it cannot recurse.
	leaf := NewRuntime(Config{Maas: maas, Role: roleLeaf, Depth: 1})
	leafRegistry := tool.NewRegistry(nil, nil, nil)
	leaf.RegisterDelegateTaskTool(leafRegistry)
	if hasDescriptor(leafRegistry, "delegate_task") {
		t.Fatalf("leaf registry unexpectedly has delegate_task")
	}
}

func TestHandleDelegateTaskSingleMode(t *testing.T) {
	t.Parallel()

	maas := &recordingSubMaas{summary: "single summary"}
	parent := NewRuntime(Config{Maas: maas})
	result, err := parent.handleDelegateTask(context.Background(), domain.ToolCall{
		ID: "call-1", Arguments: map[string]string{"goal": "do a thing"},
	})
	if err != nil {
		t.Fatalf("handleDelegateTask(single) error = %v, want nil", err)
	}
	payload := decodeDelegate(t, result)
	if payload["mode"] != "single" || payload["summary"] != "single summary" {
		t.Fatalf("single payload = %v, want single summary", payload)
	}
}

func TestHandleDelegateTaskBatchMode(t *testing.T) {
	t.Parallel()

	maas := &recordingSubMaas{summary: "batch item"}
	parent := NewRuntime(Config{Maas: maas})
	result, err := parent.handleDelegateTask(context.Background(), domain.ToolCall{
		ID: "call-1", Arguments: map[string]string{"tasks": `[{"goal":"a"},{"goal":"b"}]`},
	})
	if err != nil {
		t.Fatalf("handleDelegateTask(batch) error = %v, want nil", err)
	}
	payload := decodeDelegate(t, result)
	if payload["mode"] != "batch" {
		t.Fatalf("batch payload mode = %v, want batch", payload["mode"])
	}
	results, ok := payload["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("batch results = %v, want two", payload["results"])
	}
}

func TestHandleDelegateTaskInvalidBatchJSONFailsSoft(t *testing.T) {
	t.Parallel()

	maas := &recordingSubMaas{summary: "x"}
	parent := NewRuntime(Config{Maas: maas})
	result, err := parent.handleDelegateTask(context.Background(), domain.ToolCall{
		ID: "call-1", Arguments: map[string]string{"tasks": "{not json"},
	})
	if err != nil {
		t.Fatalf("handleDelegateTask(bad json) error = %v, want nil (tool-level failure)", err)
	}
	if result.Success {
		t.Fatalf("handleDelegateTask(bad json) success = true, want failure result")
	}
}

func TestNewSubRuntimeToolsetsNarrowsChildRegistry(t *testing.T) {
	t.Parallel()

	registry := tool.NewRegistry(nil, nil, nil)
	noop := tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Success: true}, nil
	})
	registry.RegisterDescriptor(tool.Descriptor{Name: "read_file"}, noop)
	registry.RegisterDescriptor(tool.Descriptor{Name: "write_file"}, noop)

	parent := NewRuntime(Config{Maas: &recordingSubMaas{summary: "ok"}, Tools: registry})

	// No toolsets inherits the full parent registry.
	full, err := parent.newSubRuntime(roleLeaf, nil)
	if err != nil {
		t.Fatalf("newSubRuntime(no toolsets) error = %v", err)
	}
	if len(full.tools.Descriptors()) != 2 {
		t.Fatalf("inherited tools = %d, want 2", len(full.tools.Descriptors()))
	}

	// Toolsets narrows the child to the named subset.
	narrowed, err := parent.newSubRuntime(roleLeaf, []string{"read_file"})
	if err != nil {
		t.Fatalf("newSubRuntime(toolsets) error = %v", err)
	}
	descs := narrowed.tools.Descriptors()
	if len(descs) != 1 || descs[0].Name != "read_file" {
		t.Fatalf("narrowed tools = %v, want only read_file", descs)
	}
}

func TestParseToolsetsCSV(t *testing.T) {
	t.Parallel()
	if got := parseToolsetsCSV(""); got != nil {
		t.Fatalf("parseToolsetsCSV(empty) = %v, want nil", got)
	}
	got := parseToolsetsCSV(" read_file , write_file ,")
	if len(got) != 2 || got[0] != "read_file" || got[1] != "write_file" {
		t.Fatalf("parseToolsetsCSV = %v, want [read_file write_file]", got)
	}
}

func decodeDelegate(t *testing.T, result domain.ToolResult) map[string]any {
	t.Helper()
	if !result.Success {
		t.Fatalf("delegate_task result unsuccessful: %q", result.Error)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(result.Output), &payload); err != nil {
		t.Fatalf("decode delegate_task output %q: %v", result.Output, err)
	}
	return payload
}
