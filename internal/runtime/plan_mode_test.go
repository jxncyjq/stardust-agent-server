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
