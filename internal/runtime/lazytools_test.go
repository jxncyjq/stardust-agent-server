package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

// newLazyTestRegistry returns a registry with a single auto-allowed "lookup"
// tool, used to exercise the lazy meta-tool protocol.
func newLazyTestRegistry(audit port.AuditLog) *tool.Registry {
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"lookup"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "lookup",
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

func TestInferenceToolsLazyOffersOnlyMetaTools(t *testing.T) {
	t.Parallel()

	runner := NewRuntime(Config{
		Maas:      &captureMaas{response: "done"},
		Tools:     newLazyTestRegistry(adapter.NewMemoryAuditLog()),
		LazyTools: true,
	})
	tools := runner.inferenceTools(runner.tools)
	if len(tools) != 2 {
		t.Fatalf("lazy inferenceTools() = %d tools, want 2 meta tools: %#v", len(tools), tools)
	}
	names := map[string]bool{tools[0].Name: true, tools[1].Name: true}
	if !names[metaToolListTools] || !names[metaToolCallTool] {
		t.Fatalf("lazy inferenceTools() names = %v, want list_tools and call_tool", names)
	}
	for _, want := range []string{metaToolListTools, metaToolCallTool} {
		if names[want] && strings.Contains(strings.ToLower(want), "lookup") {
			t.Fatalf("lazy inferenceTools() leaked real tool name %q", want)
		}
	}
}

func TestInferenceToolsEagerOffersFullSchema(t *testing.T) {
	t.Parallel()

	runner := NewRuntime(Config{
		Maas:      &captureMaas{response: "done"},
		Tools:     newLazyTestRegistry(adapter.NewMemoryAuditLog()),
		LazyTools: false,
	})
	tools := runner.inferenceTools(runner.tools)
	if len(tools) != 1 || tools[0].Name != "lookup" {
		t.Fatalf("eager inferenceTools() = %#v, want full native lookup descriptor", tools)
	}
}

// lazyToolCallingMaas drives the lazy protocol: round 1 lists tools, round 2
// calls the real lookup tool via call_tool, round 3 answers in text. It records
// the offered tools per round so tests can assert only meta tools were exposed.
type lazyToolCallingMaas struct {
	prompts []string
	tools   [][]port.InferenceTool
}

func (m *lazyToolCallingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompts = append(m.prompts, req.Prompt)
	m.tools = append(m.tools, append([]port.InferenceTool(nil), req.Tools...))
	switch len(m.prompts) {
	case 1:
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "meta-list",
			Name:      metaToolListTools,
			Arguments: map[string]string{},
		}}}, nil
	case 2:
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "meta-call",
			Name:      metaToolCallTool,
			Arguments: map[string]string{"tool_name": "lookup", "arguments_json": `{"query":"cache"}`},
		}}}, nil
	default:
		return port.InferenceResponse{Text: "cache uses map"}, nil
	}
}

func TestRuntimeLazyListToolsAndCallToolDispatch(t *testing.T) {
	t.Parallel()

	maas := &lazyToolCallingMaas{}
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	runner := NewRuntime(Config{
		Maas:      maas,
		Audit:     audit,
		Events:    events,
		Tools:     newLazyTestRegistry(audit),
		LazyTools: true,
	})

	run, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1", Role: "developer"}, domain.Task{
		ID:    "task-lazy",
		Input: "how does cache work",
	})
	if err != nil {
		t.Fatalf("RunTask(lazy) error = %v, want nil", err)
	}
	if run.Result != "cache uses map" {
		t.Fatalf("RunTask(lazy).Result = %q, want final answer", run.Result)
	}
	if len(maas.prompts) != 3 {
		t.Fatalf("lazy maas prompts = %d, want 3 (list, call, answer)", len(maas.prompts))
	}
	// Every inference must have offered only the two meta tools.
	for round, tools := range maas.tools {
		if len(tools) != 2 {
			t.Fatalf("round %d offered %d tools, want 2 meta tools: %#v", round, len(tools), tools)
		}
	}
	// The list_tools result fed into round 2 must contain the real tool catalog.
	if !strings.Contains(maas.prompts[1], "lookup") {
		t.Fatalf("round 2 prompt missing list_tools catalog with real tool name:\n%s", maas.prompts[1])
	}
	// The call_tool result fed into round 3 must contain the real tool output.
	if !strings.Contains(maas.prompts[2], "cache is implemented by map") {
		t.Fatalf("round 3 prompt missing real tool output:\n%s", maas.prompts[2])
	}
	if !hasRuntimeEvent(events.Events(), "tool_executed") {
		t.Fatalf("runtime events missing tool_executed for real tool: %#v", events.Events())
	}
}

func TestRuntimeListToolsCatalogExcludesMetaToolsAndFilters(t *testing.T) {
	t.Parallel()

	runner := NewRuntime(Config{
		Maas:      &captureMaas{response: "done"},
		Tools:     newLazyTestRegistry(adapter.NewMemoryAuditLog()),
		LazyTools: true,
	})
	catalog, err := runner.listToolsCatalog("", runner.tools)
	if err != nil {
		t.Fatalf("listToolsCatalog() error = %v, want nil", err)
	}
	var decoded struct {
		Tools []toolCatalogEntry `json:"tools"`
	}
	if err := json.Unmarshal([]byte(catalog), &decoded); err != nil {
		t.Fatalf("listToolsCatalog() returned invalid JSON: %v\n%s", err, catalog)
	}
	if len(decoded.Tools) != 1 || decoded.Tools[0].Name != "lookup" {
		t.Fatalf("listToolsCatalog() = %#v, want only the real lookup tool", decoded.Tools)
	}
	for _, entry := range decoded.Tools {
		if isMetaTool(entry.Name) {
			t.Fatalf("listToolsCatalog() leaked meta tool %q", entry.Name)
		}
	}
	// A non-matching query filters everything out.
	filtered, err := runner.listToolsCatalog("nonexistent", runner.tools)
	if err != nil {
		t.Fatalf("listToolsCatalog(query) error = %v, want nil", err)
	}
	if err := json.Unmarshal([]byte(filtered), &decoded); err != nil {
		t.Fatalf("filtered catalog invalid JSON: %v", err)
	}
	if len(decoded.Tools) != 0 {
		t.Fatalf("listToolsCatalog(\"nonexistent\") = %#v, want empty", decoded.Tools)
	}
}

func TestRuntimeCallToolFailLoudOnBadInput(t *testing.T) {
	t.Parallel()

	runner := NewRuntime(Config{
		Maas:      &captureMaas{response: "done"},
		Audit:     adapter.NewMemoryAuditLog(),
		Events:    adapter.NewMemoryEventBus(),
		Tools:     newLazyTestRegistry(adapter.NewMemoryAuditLog()),
		LazyTools: true,
	})
	agent := domain.Agent{ID: "agent-1", Role: "developer"}
	task := domain.Task{ID: "task-fail-loud"}

	cases := []struct {
		name string
		args map[string]string
		want string
	}{
		{
			name: "missing tool_name",
			args: map[string]string{"tool_name": "", "arguments_json": "{}"},
			want: "non-empty tool_name",
		},
		{
			name: "malformed arguments_json",
			args: map[string]string{"tool_name": "lookup", "arguments_json": "{not json"},
			want: "valid JSON object",
		},
		{
			name: "unknown tool",
			args: map[string]string{"tool_name": "does_not_exist", "arguments_json": "{}"},
			want: "tool not found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := domain.ToolCall{ID: "meta-call", Name: metaToolCallTool, Arguments: tc.args}
			result, err := runner.dispatchToolCall(context.Background(), agent, task, call, runner.tools)
			if err != nil {
				t.Fatalf("dispatchToolCall(%s) returned Go error = %v, want fail-loud ToolResult", tc.name, err)
			}
			if result.Success {
				t.Fatalf("dispatchToolCall(%s).Success = true, want false", tc.name)
			}
			if !strings.Contains(result.Error, tc.want) {
				t.Fatalf("dispatchToolCall(%s).Error = %q, want substring %q", tc.name, result.Error, tc.want)
			}
		})
	}
}

func TestRuntimeCallToolCoercesNonStringArguments(t *testing.T) {
	t.Parallel()

	args, err := parseCallToolArguments(`{"path":"README.md","limit":5,"flag":true}`)
	if err != nil {
		t.Fatalf("parseCallToolArguments() error = %v, want nil", err)
	}
	if args["path"] != "README.md" {
		t.Fatalf("args[path] = %q, want README.md", args["path"])
	}
	if args["limit"] != "5" {
		t.Fatalf("args[limit] = %q, want 5", args["limit"])
	}
	if args["flag"] != "true" {
		t.Fatalf("args[flag] = %q, want true", args["flag"])
	}
}
