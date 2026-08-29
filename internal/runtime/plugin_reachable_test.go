package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/tool"
)

// This file is the point of the whole change: until the per-agent registry
// inherited the plugin registry, a plugin's tools lived where no model could
// see them and its observe/decide seams were never consulted for an agent's
// own calls. Every test here asserts one half of "they are now".

func pluginRegistryWith(t *testing.T) *tool.Registry {
	t.Helper()

	plugins := tool.NewRegistry(
		tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	)
	plugins.RegisterDescriptor(
		tool.Descriptor{Name: "jira_search", Description: "search Jira", Group: "plugins", RiskLevel: "low"},
		tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{Success: true, Output: "issues"}, nil
		}))
	return plugins
}

func resolverWithPlugins(t *testing.T, plugins *tool.Registry) *AgentRuntimeResolver {
	t.Helper()

	return NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Gate: taskgate.NewTaskGate(),
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {ID: "agent-researcher", Role: "researcher", MaasProfile: "deep"},
		}),
		RootConfig: config.Config{
			ContextFiles: config.ContextFilesConfig{Root: t.TempDir()},
			Runtime:      config.RuntimeConfig{MaxToolRounds: 1},
		},
		Audit:       adapter.NewMemoryAuditLog(),
		Events:      adapter.NewMemoryEventBus(),
		PluginTools: plugins,
		MaasFactory: func(string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: &resolverCaptureMaas{response: "ok"}}, nil
		},
	})
}

func resolvedRegistry(t *testing.T, resolver *AgentRuntimeResolver) *tool.Registry {
	t.Helper()

	_, runner, ok, err := resolver.ResolveTaskRunner(context.Background(), domain.Task{
		ID: "task-1", AgentID: "researcher",
	})
	if err != nil || !ok {
		t.Fatalf("ResolveTaskRunner = (%v, %t), want a runner", err, ok)
	}
	rt, isRuntime := runner.(*Runtime)
	if !isRuntime {
		t.Fatalf("runner type = %T, want *Runtime", runner)
	}
	return rt.tools
}

func researcher() domain.Agent { return domain.Agent{ID: "a1", Role: "researcher"} }

// TestAnAgentCanSeeAndCallAPluginTool is the headline: the model's own
// registry lists the tool and running it reaches the plugin's handler.
func TestAnAgentCanSeeAndCallAPluginTool(t *testing.T) {
	t.Parallel()

	registry := resolvedRegistry(t, resolverWithPlugins(t, pluginRegistryWith(t)))

	var names []string
	for _, descriptor := range registry.Descriptors() {
		names = append(names, descriptor.Name)
	}
	found := false
	for _, name := range names {
		if name == "jira_search" {
			found = true
		}
	}
	if !found {
		t.Fatalf("descriptors = %v, want the plugin's tool among them", names)
	}

	result, err := registry.Execute(context.Background(), researcher(),
		domain.ToolCall{ID: "c1", Name: "jira_search"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Output != "issues" {
		t.Errorf("result = %+v, want the plugin handler's output", result)
	}
}

// TestAPluginMountedAfterATaskStartedIsStillReachable: plugins mount and
// unload while the process runs. The inheritance is a reference, so a
// `plugins reload` reaches registries that already exist.
func TestAPluginMountedAfterATaskStartedIsStillReachable(t *testing.T) {
	t.Parallel()

	plugins := pluginRegistryWith(t)
	registry := resolvedRegistry(t, resolverWithPlugins(t, plugins))

	plugins.RegisterDescriptor(
		tool.Descriptor{Name: "jira_comment", Group: "plugins", RiskLevel: "low"},
		tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{Success: true, Output: "commented"}, nil
		}))

	result, err := registry.Execute(context.Background(), researcher(),
		domain.ToolCall{ID: "c1", Name: "jira_comment"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Output != "commented" {
		t.Errorf("result = %+v, want the newly mounted tool to have run", result)
	}
}

// TestAPluginsDeciderRefusesAnAgentsOwnToolCall is the G4b half of the point:
// the decider is registered on the PLUGIN registry, and the call is made on
// the AGENT's. Before this change it was never consulted.
func TestAPluginsDeciderRefusesAnAgentsOwnToolCall(t *testing.T) {
	t.Parallel()

	plugins := pluginRegistryWith(t)
	plugins.AddDecider("plugin:legion-gatekeeper", tool.DeciderFunc(
		func(_ context.Context, call domain.ToolCall) tool.Verdict {
			if call.Name != "read_file" {
				return tool.Verdict{Decision: tool.DecisionAllow}
			}
			return tool.Verdict{Decision: tool.DecisionDeny, Reason: "reads are frozen during the incident"}
		}))
	registry := resolvedRegistry(t, resolverWithPlugins(t, plugins))

	_, err := registry.Execute(context.Background(), researcher(),
		domain.ToolCall{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": "nothing.txt"}})
	if !errors.Is(err, tool.ErrPermissionDenied) {
		t.Fatalf("Execute = %v, want the plugin's decider to refuse a BUILTIN tool call", err)
	}
	if !strings.Contains(err.Error(), "reads are frozen during the incident") {
		t.Errorf("error = %v, want the plugin's own reason", err)
	}
}

// TestAPluginsObserverSeesAnAgentsOwnToolCall is the G4a half.
func TestAPluginsObserverSeesAnAgentsOwnToolCall(t *testing.T) {
	t.Parallel()

	plugins := pluginRegistryWith(t)
	var seen []string
	plugins.AddObserver("plugin:legion-audit", tool.ObserverFunc(
		func(_ context.Context, call domain.ToolCall, _ domain.ToolResult) {
			seen = append(seen, call.Name)
		}))
	registry := resolvedRegistry(t, resolverWithPlugins(t, plugins))

	if _, err := registry.Execute(context.Background(), researcher(),
		domain.ToolCall{ID: "c1", Name: "jira_search"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(seen) != 1 || seen[0] != "jira_search" {
		t.Errorf("observer saw %v, want [jira_search]", seen)
	}
}

// TestADeploymentWithNoPluginsIsUnchanged: the whole seam must cost nothing
// when nothing is mounted.
func TestADeploymentWithNoPluginsIsUnchanged(t *testing.T) {
	t.Parallel()

	registry := resolvedRegistry(t, resolverWithPlugins(t, nil))

	if _, err := registry.Execute(context.Background(), researcher(),
		domain.ToolCall{ID: "c1", Name: "jira_search"}); !errors.Is(err, tool.ErrToolNotFound) {
		t.Errorf("Execute = %v, want ErrToolNotFound with no plugin registry", err)
	}
}
