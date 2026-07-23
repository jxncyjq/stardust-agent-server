package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/testsupport"
	"github.com/stardust/legion-agent/internal/tool"
)

// TestPlanModeCatalogExcludesSensitiveTools pins the boundary that keeps the
// meta tools from becoming a way around Plan mode. effectiveTools already
// drops the side-effecting tools; if the catalog were built from the full
// registry instead, the model could load a sensitive tool's schema and call it.
func TestPlanModeCatalogExcludesSensitiveTools(t *testing.T) {
	t.Parallel()
	registry := tool.NewRegistry(nil, nil, nil)
	noop := tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, nil
	})
	registry.RegisterDescriptor(tool.Descriptor{
		Name: "read_file", Group: "files", Description: "Read.", InputSchema: map[string]any{"type": "object"},
	}, noop)
	registry.RegisterDescriptor(tool.Descriptor{
		Name: "write_file", Group: "files", Description: "Write.", Sensitive: true, InputSchema: map[string]any{"type": "object"},
	}, noop)

	rt := NewRuntime(Config{Tools: registry})
	// This is the same effective-registry scoping RunTask applies at Plan-mode
	// entry (buildCatalog(r.effectiveTools(task))); building the catalog
	// directly from it here, rather than from the raw registry, is the
	// invariant this test exists to pin.
	effective := rt.effectiveTools(domain.Task{ID: "t1", Mode: domain.ModePlan})
	catalog := capability.NewCatalog(capability.NewToolProvider(effective))

	entries, err := catalog.Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries() error = %v, want nil", err)
	}
	for _, e := range entries {
		if e.Name == "write_file" {
			t.Fatal("Plan-mode catalog lists a sensitive tool: load+call would bypass the read-only restriction")
		}
	}

	st := &loopState{}
	result, err := rt.dispatchLoadCapabilities(context.Background(), st, loadCall("write_file"), catalog)
	if err != nil {
		t.Fatalf("dispatchLoadCapabilities() error = %v", err)
	}
	if result.Success {
		t.Fatal("loading a tool outside the task's effective registry succeeded")
	}
	if !strings.Contains(result.Error, "write_file") {
		t.Errorf("error = %q, want it to name the refused tool", result.Error)
	}
}

// childCapturingMaas drives a two-round tool loop (a real tool call, then a
// finishing text answer) and records every prompt it is handed, so a test can
// inspect exactly what context a run — parent or child — was actually given.
type childCapturingMaas struct {
	prompts []string
}

func (m *childCapturingMaas) Generate(_ context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	m.prompts = append(m.prompts, testsupport.RequestText(req))
	if len(m.prompts) == 1 {
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID: "c1", Name: "lookup", Arguments: map[string]string{"query": "x"},
		}}}, nil
	}
	return port.InferenceResponse{Text: "done"}, nil
}

// TestSubRuntimeStartsWithEmptyLoadedSet pins that a delegated child never
// inherits the parent's loaded-capability block. "loaded" lives on loopState,
// which every RunTask call (including the child's own) builds fresh; Runtime
// itself carries no such field for newSubRuntime to copy. This test proves the
// consequence end to end: even though the parent has already pinned a
// capability's full definition into its own in-flight loop state, the child's
// second tool-round prompt — the only place a loaded block is ever rendered
// (composePrompt) — never mentions it, because the child never itself called
// load_capabilities.
func TestSubRuntimeStartsWithEmptyLoadedSet(t *testing.T) {
	t.Parallel()

	registry := tool.NewRegistry(nil, nil, nil)
	registry.RegisterDescriptor(tool.Descriptor{
		Name: "lookup", Group: "files", Description: "PARENT-ONLY-CAPABILITY-MARKER",
		InputSchema: map[string]any{"type": "object"},
	}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Success: true, Output: "ok"}, nil
	}))

	parent := NewRuntime(Config{Maas: &childCapturingMaas{}, Tools: registry, LazyTools: true})

	// Simulate the parent's in-flight loop state carrying a loaded capability,
	// the way dispatchLoadCapabilities does mid-run. This is exactly the state
	// a leaking newSubRuntime would need somewhere to copy from.
	parentCatalog := capability.NewCatalog(capability.NewToolProvider(registry))
	parentState := &loopState{catalog: parentCatalog}
	if _, err := parent.dispatchLoadCapabilities(context.Background(), parentState, loadCall("lookup"), parentCatalog); err != nil {
		t.Fatalf("setup: dispatchLoadCapabilities() error = %v", err)
	}
	if len(parentState.loaded) == 0 || parentState.loaded[0].detail == "" {
		t.Fatal("setup: parent did not actually load a capability, so this test would prove nothing")
	}

	child, err := parent.newSubRuntime(roleLeaf, nil)
	if err != nil {
		t.Fatalf("newSubRuntime() error = %v", err)
	}
	if child == nil {
		t.Fatal("newSubRuntime() = nil")
	}

	childMaas := &childCapturingMaas{}
	child.maas = childMaas

	run, err := child.RunTask(context.Background(), domain.Agent{ID: "child-agent", Role: "developer"}, domain.Task{
		ID: "child-task", Input: "do work",
	})
	if err != nil {
		t.Fatalf("child.RunTask() error = %v, want nil", err)
	}
	if run.Result != "done" {
		t.Fatalf("run.Result = %q, want %q", run.Result, "done")
	}
	if len(childMaas.prompts) != 2 {
		t.Fatalf("child prompts = %d, want 2 (initial round + after tool round)", len(childMaas.prompts))
	}
	// prompts[1] is built via composePrompt(st.basePrompt, st.loaded, ...): the
	// only place a loaded block is ever rendered. If a sub runtime (or RunTask's
	// fresh loop-state construction) ever started propagating the parent's
	// loaded capabilities, the parent's marker would show up here even though
	// the child never called load_capabilities itself.
	if strings.Contains(childMaas.prompts[1], "PARENT-ONLY-CAPABILITY-MARKER") {
		t.Fatal("child's second-round prompt contains the parent's loaded capability detail: sub runtime leaked the parent's loaded block")
	}
	if strings.Contains(childMaas.prompts[1], "Loaded capabilities:") {
		t.Fatal("child rendered a loaded-capabilities block despite never calling load_capabilities itself")
	}
}
