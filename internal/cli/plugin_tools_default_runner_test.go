package cli

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/tool"
)

// The default task runner serves every task whose agent_id is not in the agent
// registry — the GUI's own path, and every default-agent task. A real-machine
// run found plugins reachable from the per-agent resolver and NOT from here,
// which reads to an operator as "plugins do not work".
func TestTheDefaultRunnerBuildsRegistriesThatReachPluginTools(t *testing.T) {
	plugins := tool.NewRegistry(
		tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	)
	plugins.RegisterDescriptor(
		tool.Descriptor{Name: "hello_echo", Description: "greet", Group: "plugins", RiskLevel: "low"},
		tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{Success: true, Output: "hello, legion!"}, nil
		}))

	runner := &defaultTaskRunner{
		contextRoot: t.TempDir(),
		audit:       adapter.NewMemoryAuditLog(),
		pluginTools: plugins,
	}
	tools := runner.buildTaskTools(domain.Task{ID: "t1"})

	result, err := tools.Execute(context.Background(),
		domain.Agent{ID: "a1", Role: "developer"},
		domain.ToolCall{ID: "c1", Name: "hello_echo", Arguments: map[string]string{"name": "legion"}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Output != "hello, legion!" {
		t.Errorf("result = %+v, want the plugin tool's output", result)
	}
}

// TestTheDefaultRunnerBuildsRegistriesThatCanResolveAnApprovedAsk: a plugin's
// "ask" is only half-wired without the arbiter — the round boundary opens the
// ticket, and dispatch has to be able to read the human's answer back. A real
// machine showed the symptom this test now prevents: the operator approved,
// and the call was refused anyway.
func TestTheDefaultRunnerBuildsRegistriesThatCanResolveAnApprovedAsk(t *testing.T) {
	plugins := tool.NewRegistry(
		tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	)
	plugins.RegisterDescriptor(
		tool.Descriptor{Name: "guarded_tool", Group: "plugins", RiskLevel: "low"},
		tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{Success: true, Output: "ran"}, nil
		}))
	plugins.AddDecider("plugin:legion-hello", tool.DeciderFunc(
		func(context.Context, domain.ToolCall) tool.Verdict {
			return tool.Verdict{Decision: tool.DecisionAsk, Reason: "a human should look"}
		}))

	runner := &defaultTaskRunner{
		contextRoot: t.TempDir(),
		audit:       adapter.NewMemoryAuditLog(),
		pluginTools: plugins,
		askArbiter: tool.AskArbiterFunc(func(context.Context, domain.ToolCall) (bool, error) {
			return true, nil // the human approved
		}),
	}
	tools := runner.buildTaskTools(domain.Task{ID: "t1"})

	result, err := tools.Execute(context.Background(),
		domain.Agent{ID: "a1", Role: "developer"},
		domain.ToolCall{ID: "c1", Name: "guarded_tool"})
	if err != nil {
		t.Fatalf("Execute with an approved ask = %v, want the call to run", err)
	}
	if result.Output != "ran" {
		t.Errorf("result = %+v, want the tool to have run", result)
	}
}
