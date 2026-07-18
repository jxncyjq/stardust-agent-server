package runtime

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

type planProbeMaas struct {
	offeredTools []string
	calls        int
}

func (m *planProbeMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.calls++
	if m.calls == 1 {
		for _, tl := range req.Tools {
			m.offeredTools = append(m.offeredTools, tl.Name)
		}
	}
	return port.InferenceResponse{Text: "plan text"}, nil
}

func planRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"read_x", "write_x"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	)
	reg.RegisterDescriptor(tool.Descriptor{Name: "read_x", Sensitive: false}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Success: true}, nil
	}))
	reg.RegisterDescriptor(tool.Descriptor{Name: "write_x", Sensitive: true}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Success: true}, nil
	}))
	return reg
}

func TestPlanModeOffersOnlySafeTools(t *testing.T) {
	maas := &planProbeMaas{}
	runner := NewRuntime(Config{
		Maas: maas, Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
		Tools: planRegistry(t), LazyTools: false,
	})
	_, err := runner.RunTask(context.Background(), domain.Agent{ID: "a1"}, domain.Task{
		ID: "t1", AgentID: "a1", Status: domain.TaskRunning, Input: "plan it", Mode: domain.ModePlan,
	})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	for _, name := range maas.offeredTools {
		if name == "write_x" {
			t.Errorf("plan mode offered sensitive tool write_x; offered=%v", maas.offeredTools)
		}
	}
	found := false
	for _, name := range maas.offeredTools {
		if name == "read_x" {
			found = true
		}
	}
	if !found {
		t.Errorf("plan mode did not offer safe tool read_x; offered=%v", maas.offeredTools)
	}
}

// planLazyCallToolMaas drives the lazy meta-tool protocol inside a Plan-mode
// run: round 1 tries to invoke the sensitive write_x tool via call_tool,
// every later round answers in text so the loop terminates.
type planLazyCallToolMaas struct {
	calls int
}

func (m *planLazyCallToolMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.calls++
	if m.calls == 1 {
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "meta-call",
			Name:      metaToolCallTool,
			Arguments: map[string]string{"tool_name": "write_x", "arguments_json": "{}"},
		}}}, nil
	}
	return port.InferenceResponse{Text: "plan done"}, nil
}

// TestPlanModeLazyCallToolCannotReachSensitiveTool guards the security
// invariant that under the lazy tool protocol a Plan-mode run cannot reach a
// sensitive tool through the call_tool meta-tool: the sensitive tool is
// excluded from the safe subset registry actually dispatched against, so
// Execute fails loud with ErrToolNotFound (surfaced as an unsuccessful
// ToolResult, not a task-killing error) and the sensitive handler never runs.
func TestPlanModeLazyCallToolCannotReachSensitiveTool(t *testing.T) {
	var writeXCalled bool
	reg := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"read_x", "write_x"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	)
	reg.RegisterDescriptor(tool.Descriptor{Name: "read_x", Sensitive: false}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Success: true}, nil
	}))
	reg.RegisterDescriptor(tool.Descriptor{Name: "write_x", Sensitive: true}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		writeXCalled = true
		return domain.ToolResult{Success: true}, nil
	}))

	maas := &planLazyCallToolMaas{}
	runner := NewRuntime(Config{
		Maas: maas, Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
		Tools: reg, LazyTools: true,
	})
	run, err := runner.RunTask(context.Background(), domain.Agent{ID: "a1"}, domain.Task{
		ID: "t3", AgentID: "a1", Status: domain.TaskRunning, Input: "try to write", Mode: domain.ModePlan,
	})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if writeXCalled {
		t.Fatalf("sensitive tool write_x was invoked via lazy call_tool in Plan mode; run=%#v", run)
	}
	if run.Result != "plan done" {
		t.Fatalf("RunTask.Result = %q, want final answer after rejected call_tool", run.Result)
	}
}

func TestAutoModeOffersAllTools(t *testing.T) {
	maas := &planProbeMaas{}
	runner := NewRuntime(Config{
		Maas: maas, Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
		Tools: planRegistry(t), LazyTools: false,
	})
	_, err := runner.RunTask(context.Background(), domain.Agent{ID: "a1"}, domain.Task{
		ID: "t2", AgentID: "a1", Status: domain.TaskRunning, Input: "do it", Mode: domain.ModeAuto,
	})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	has := func(n string) bool {
		for _, x := range maas.offeredTools {
			if x == n {
				return true
			}
		}
		return false
	}
	if !has("write_x") || !has("read_x") {
		t.Errorf("auto mode should offer all tools; offered=%v", maas.offeredTools)
	}
}
