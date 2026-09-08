package runtime

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/taskgate"
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

	orchestrator := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas})
	orchRegistry := tool.NewRegistry(nil, nil, nil)
	orchestrator.RegisterDelegateTaskTool(orchRegistry)
	if !hasDescriptor(orchRegistry, "delegate_task") {
		t.Fatalf("orchestrator registry missing delegate_task")
	}

	// A leaf child never gets the tool, so it cannot recurse.
	leaf := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Role: roleLeaf, Depth: 1})
	leafRegistry := tool.NewRegistry(nil, nil, nil)
	leaf.RegisterDelegateTaskTool(leafRegistry)
	if hasDescriptor(leafRegistry, "delegate_task") {
		t.Fatalf("leaf registry unexpectedly has delegate_task")
	}
}

func TestHandleDelegateTaskSingleMode(t *testing.T) {
	t.Parallel()

	maas := &recordingSubMaas{summary: "single summary"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas})
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
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas})
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
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas})
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

	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: &recordingSubMaas{summary: "ok"}, Tools: registry})

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

// delegateTaskDescriptorFrom pulls the delegate_task descriptor back out of a
// registry it was registered on. Reading it from the registry rather than
// calling delegateTaskDescriptor directly is the point: what a model sees is
// whatever RegisterDelegateTaskTool chose to build, so a test that called the
// builder itself would still pass if the wiring between the runtime's agent
// list and the descriptor were cut.
func delegateTaskDescriptorFrom(t *testing.T, registry *tool.Registry) tool.Descriptor {
	t.Helper()
	for _, descriptor := range registry.Descriptors() {
		if descriptor.Name == "delegate_task" {
			return descriptor
		}
	}
	t.Fatal("registry exposes no delegate_task descriptor")
	return tool.Descriptor{}
}

// agentIDDescriptionOf returns the agent_id property description a model would
// read off descriptor.
func agentIDDescriptionOf(t *testing.T, descriptor tool.Descriptor) string {
	t.Helper()
	properties, ok := descriptor.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("InputSchema[\"properties\"] = %v (%T), want a map[string]any", descriptor.InputSchema["properties"], descriptor.InputSchema["properties"])
	}
	entry, ok := properties["agent_id"].(map[string]any)
	if !ok {
		t.Fatalf("InputSchema properties[\"agent_id\"] = %v (%T), want a map[string]any", properties["agent_id"], properties["agent_id"])
	}
	description, ok := entry["description"].(string)
	if !ok {
		t.Fatalf("agent_id description = %v (%T), want a string", entry["description"], entry["description"])
	}
	return description
}

// TestDelegateTaskDescriptionListsTheDelegatableAgentNames guards spec §3.1's
// second half. Spec §2 declined a list_agents tool specifically because the
// tool description would carry the names instead, so the tool description is
// the ONLY channel through which a model can learn which ids agent_id accepts.
// Without them it has to guess, and it only ever sees the list inside
// validateSubTaskSpec's refusal -- a wasted round per guess.
func TestDelegateTaskDescriptionListsTheDelegatableAgentNames(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime(Config{
		Gate:             taskgate.NewTaskGate(),
		Maas:             &recordingSubMaas{summary: "ok"},
		DelegationAgents: &fakeDelegationAgents{names: []string{"reviewer", "researcher"}},
	})
	registry := tool.NewRegistry(nil, nil, nil)
	runtime.RegisterDelegateTaskTool(registry)

	description := agentIDDescriptionOf(t, delegateTaskDescriptorFrom(t, registry))
	for _, name := range []string{"researcher", "reviewer"} {
		if !strings.Contains(description, name) {
			t.Errorf("agent_id description = %q, want it to name the configured agent %q: a model that cannot read the list has to guess ids and burn a round on the refusal to see them", description, name)
		}
	}
}

// TestDelegateTaskDescriptionSaysWhenNoAgentsAreConfigured is the other half:
// a deployment with no agent directory is a legitimate state, and the
// description must say so rather than advertise a menu with nothing on it.
// Every agent_id is refused there (validateSubTaskSpec refuses a nil resolver
// outright, and a resolver that knows no names refuses every id), so promising
// the field works would be a lie the model pays a round to discover.
func TestDelegateTaskDescriptionSaysWhenNoAgentsAreConfigured(t *testing.T) {
	t.Parallel()
	runtime := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: &recordingSubMaas{summary: "ok"}})
	registry := tool.NewRegistry(nil, nil, nil)
	runtime.RegisterDelegateTaskTool(registry)

	description := agentIDDescriptionOf(t, delegateTaskDescriptorFrom(t, registry))
	if !strings.Contains(description, "no configured agents") {
		t.Errorf("agent_id description = %q, want it to state that this deployment has no configured agents", description)
	}
	if strings.Contains(description, "may be named here") {
		t.Errorf("agent_id description = %q, want no list of nameable agents when none are configured", description)
	}
}

// TestBatchDelegateTaskToolCallMapsEachEntrysAgentID closes the seam
// TestBatchDelegationHonoursEachEntrysAgentID could not reach: that test hands
// RunSubTasks a hand-built []SubTaskSpec, so the JSON -> SubTaskSpec mapping in
// handleDelegateTask's batch branch is never crossed, and the end-to-end guard
// in internal/cli only exercises single mode. Deleting the per-entry AgentID
// mapping there compiles, vets clean and leaves every other test green while
// every batch entry silently degrades to a clone of the parent.
func TestBatchDelegateTaskToolCallMapsEachEntrysAgentID(t *testing.T) {
	t.Parallel()
	agents := &concurrentAgentRecorder{fakeDelegationAgents: fakeDelegationAgents{names: []string{"researcher", "reviewer"}}}
	parent := NewRuntime(Config{
		Gate:             taskgate.NewTaskGate(),
		Maas:             &recordingSubMaas{summary: "ok"},
		DelegationAgents: agents,
		MaxSpawnDepth:    3,
	})

	result, err := parent.handleDelegateTask(context.Background(), domain.ToolCall{
		ID: "call-batch-agents",
		Arguments: map[string]string{
			"tasks": `[{"goal":"dig","agent_id":"researcher"},{"goal":"check","agent_id":"reviewer"}]`,
		},
	})
	if err != nil {
		t.Fatalf("handleDelegateTask(batch) error = %v, want nil", err)
	}
	if payload := decodeDelegate(t, result); payload["mode"] != "batch" {
		t.Fatalf("payload mode = %v, want batch", payload["mode"])
	}

	got := agents.recorded()
	sort.Strings(got)
	want := []string{"researcher", "reviewer"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ResolveDelegate was called with ids %v, want each batch entry's OWN agent_id %v to survive the JSON -> SubTaskSpec mapping; an empty list means every entry fell back to cloning the parent", got, want)
	}
}
