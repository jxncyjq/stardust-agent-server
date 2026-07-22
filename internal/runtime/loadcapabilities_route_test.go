package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

// newGroupedLazyRegistry returns a lazy-protocol registry whose single tool
// declares a catalog group, so a capability catalog can be built from it.
func newGroupedLazyRegistry(audit port.AuditLog) *tool.Registry {
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"lookup"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "lookup",
		Group:       "files",
		Description: "lookup test data",
		InputSchema: map[string]any{
			"required":   []string{"query"},
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
		},
	}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: call.ID, Success: true, Output: "cache is implemented by map"}, nil
	}))
	return registry
}

// loadCapabilitiesMaas drives the lazy protocol: round 1 calls the
// load_capabilities meta tool for "lookup"; round 2 answers in text. It records
// the prompt it received each round so the test can assert the loaded schema was
// pinned into the prompt (not surfaced as an "unhandled meta tool" failure).
type loadCapabilitiesMaas struct {
	prompts []string
}

func (m *loadCapabilitiesMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompts = append(m.prompts, req.Prompt)
	if len(m.prompts) == 1 {
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "meta-load",
			Name:      metaToolLoadCapabilities,
			Arguments: map[string]string{"names": "lookup"},
		}}}, nil
	}
	return port.InferenceResponse{Text: "answered from the loaded schema"}, nil
}

// TestRuntimeRoutesLoadCapabilities asserts load_capabilities is dispatched to
// dispatchLoadCapabilities (not the default "unhandled meta tool" branch) and
// that the requested capability's full definition is pinned into the loaded
// block, so it reaches the model on the next round.
func TestRuntimeRoutesLoadCapabilities(t *testing.T) {
	t.Parallel()

	maas := &loadCapabilitiesMaas{}
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	runner := NewRuntime(Config{
		Maas:      maas,
		Audit:     audit,
		Events:    events,
		Tools:     newGroupedLazyRegistry(audit),
		LazyTools: true,
	})

	run, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.Task{
		ID:    "task-load",
		Input: "look something up",
	})
	if err != nil {
		t.Fatalf("RunTask(load_capabilities) error = %v, want nil", err)
	}
	if run.Result != "answered from the loaded schema" {
		t.Fatalf("RunTask().Result = %q, want the final text answer", run.Result)
	}
	if len(maas.prompts) != 2 {
		t.Fatalf("maas prompts = %d, want 2 (load call, then answer)", len(maas.prompts))
	}

	round2 := maas.prompts[1]
	// The routing regression: an unrouted load_capabilities falls through to the
	// default branch and comes back as an "unhandled meta tool" failure instead
	// of pinning the schema.
	if strings.Contains(round2, "unhandled meta tool") {
		t.Fatalf("load_capabilities hit the unhandled-meta-tool branch:\n%s", round2)
	}
	// The pinned loaded block must carry the tool's real definition.
	if !strings.Contains(round2, "Loaded capabilities:") {
		t.Fatalf("round 2 prompt missing the loaded capabilities block:\n%s", round2)
	}
	if !strings.Contains(round2, "input_schema") {
		t.Fatalf("round 2 prompt missing the loaded tool schema detail:\n%s", round2)
	}
}
