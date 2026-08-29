package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/manualgate"
	"github.com/stardust/legion-agent/internal/prompt"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/tool"
)

// The two halves of a plugin's "ask" meet here: the gate opened a ticket at
// the round boundary, and dispatch has to find the human's answer. It finds it
// through the approval scope the runtime marks the context with — without
// that marking the arbiter has no task to look a ticket up under, and every
// approved call would be refused.

func askingGateSetup(t *testing.T) (*Runtime, *manualgate.ManualToolGate, *approval.ToolGateStore, *tool.Registry, *bool) {
	t.Helper()

	store := approval.NewToolGateStore(t.TempDir())
	gate := manualgate.New(store)
	ran := false
	registry := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{})
	registry.RegisterDescriptor(tool.Descriptor{Name: "read_file"}, tool.HandlerFunc(
		func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			ran = true
			return domain.ToolResult{Success: true, Output: "contents"}, nil
		}))
	registry.AddDecider("plugin:legion-gatekeeper", tool.DeciderFunc(
		func(context.Context, domain.ToolCall) tool.Verdict {
			return tool.Verdict{Decision: tool.DecisionAsk, Reason: "reads are reviewed during the incident"}
		}))
	registry.SetAskArbiter(gate)

	r := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: &oneToolThenTextMaas{}, Tools: registry, ToolGate: gate})
	return r, gate, store, registry, &ran
}

func TestAnApprovedPluginAskRunsAtDispatch(t *testing.T) {
	r, gate, store, registry, ran := askingGateSetup(t)
	task := domain.Task{ID: "t1", SessionID: "s1", Mode: domain.ModeAuto}
	call := domain.ToolCall{ID: "c1", Name: "read_file"}

	if _, err := gate.ShouldSuspend(context.Background(), task, []domain.ToolCall{call}, registry); err != nil {
		t.Fatalf("ShouldSuspend: %v", err)
	}
	if _, err := store.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalApproved, ""); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	res, err := r.dispatchToolCall(context.Background(), domain.Agent{ID: "a1"}, task, call,
		&loopState{tools: registry, toolNameGuard: newSharedToolBudget()})
	if err != nil {
		t.Fatalf("dispatchToolCall: %v", err)
	}
	if !res.Success {
		t.Fatalf("result = %+v, want the approved call to run", res)
	}
	if !*ran {
		t.Error("the handler did not run for a call a human approved")
	}
}

// TestAnUndecidedPluginAskIsRefusedAtDispatch is the fail-closed half: the
// ticket exists, nobody answered it, and the call must not run. (Reaching
// dispatch in that state means the round-boundary suspend was bypassed —
// exactly the situation this backstop is for.)
func TestAnUndecidedPluginAskIsRefusedAtDispatch(t *testing.T) {
	r, gate, _, registry, ran := askingGateSetup(t)
	task := domain.Task{ID: "t1", SessionID: "s1", Mode: domain.ModeAuto}
	call := domain.ToolCall{ID: "c1", Name: "read_file"}

	if _, err := gate.ShouldSuspend(context.Background(), task, []domain.ToolCall{call}, registry); err != nil {
		t.Fatalf("ShouldSuspend: %v", err)
	}

	_, err := r.dispatchToolCall(context.Background(), domain.Agent{ID: "a1"}, task, call,
		&loopState{tools: registry, toolNameGuard: newSharedToolBudget()})
	if err == nil {
		t.Fatal("dispatchToolCall = nil error, want the undecided ask to refuse the call")
	}
	if !strings.Contains(err.Error(), "legion-gatekeeper") {
		t.Errorf("error = %v, want it to name the plugin that asked", err)
	}
	if *ran {
		t.Error("the handler ran for a call nobody approved")
	}
}

// TestAnApprovedPluginAskRunsThroughTheLazyProtocol: under the lazy protocol
// what reaches the registry is the INNER call, with an id of its own. The
// ticket the gate opened is keyed to that id, and dispatch has to line up with
// it — this is the pair that breaks first if either side changes how the inner
// id is built.
func TestAnApprovedPluginAskRunsThroughTheLazyProtocol(t *testing.T) {
	r, gate, store, registry, ran := askingGateSetup(t)
	r.lazyTools = true
	task := domain.Task{ID: "t1", SessionID: "s1", Mode: domain.ModeAuto}
	meta := domain.ToolCall{ID: "c1", Name: "call_tool", Arguments: map[string]string{
		"tool_name":      "read_file",
		"arguments_json": `{"path":"/tmp/x"}`,
	}}

	if _, err := gate.ShouldSuspend(context.Background(), task, []domain.ToolCall{meta}, registry); err != nil {
		t.Fatalf("ShouldSuspend: %v", err)
	}
	innerID := tool.LazyInnerCallID("c1", "read_file")
	if _, err := store.Decide("s1", approval.TicketID("t1", innerID), approval.ApprovalApproved, ""); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	res, err := r.dispatchToolCall(context.Background(), domain.Agent{ID: "a1"}, task, meta,
		&loopState{tools: registry, toolNameGuard: newSharedToolBudget()})
	if err != nil {
		t.Fatalf("dispatchToolCall: %v", err)
	}
	if !res.Success {
		t.Fatalf("result = %+v, want the approved lazy call to run", res)
	}
	if !*ran {
		t.Error("the handler did not run for an approved lazy call")
	}
}

// TestResolverGivesEachAgentRegistryTheAskArbiter: the model's calls run in a
// registry the resolver builds per agent, so that is where an ask has to be
// resolvable. Without this wiring every plugin ask would be refused at
// dispatch even after a human approved it — the approval would exist and
// nothing would read it.
func TestResolverGivesEachAgentRegistryTheAskArbiter(t *testing.T) {
	t.Parallel()

	gate := manualgate.New(approval.NewToolGateStore(t.TempDir()))
	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Gate: taskgate.NewTaskGate(),
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {ID: "agent-researcher", Role: "researcher", MaasProfile: "deep"},
		}),
		RootConfig: config.Config{
			ContextFiles: config.ContextFilesConfig{Root: t.TempDir()},
			Runtime:      config.RuntimeConfig{MaxToolRounds: 1},
		},
		Audit:    adapter.NewMemoryAuditLog(),
		Events:   adapter.NewMemoryEventBus(),
		ToolGate: gate,
		MaasFactory: func(string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: &resolverCaptureMaas{response: "ok"}}, nil
		},
	})

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
	// Prove it behaviourally: a decider that asks, and an unapproved call that
	// is therefore refused NAMING the arbiter's answer rather than the
	// "no approval channel" wording a missing arbiter produces.
	rt.tools.AddDecider("plugin:probe", tool.DeciderFunc(
		func(context.Context, domain.ToolCall) tool.Verdict {
			return tool.Verdict{Decision: tool.DecisionAsk, Reason: "probe"}
		}))

	// read_file, because it is a tool this agent's role is actually permitted:
	// an unpermitted name would be refused by the enforcer BEFORE any decider
	// is consulted, and this test would pass without the wiring it exists to
	// check.
	_, execErr := rt.tools.Execute(context.Background(), domain.Agent{ID: "a1", Role: "researcher"},
		domain.ToolCall{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": "nothing.txt"}})
	if execErr == nil {
		t.Fatal("Execute = nil error, want the unapproved ask to be refused")
	}
	if !strings.Contains(execErr.Error(), "plugin:probe") {
		t.Fatalf("error = %v, want the decider to have been consulted at all", execErr)
	}
	if strings.Contains(execErr.Error(), "no approval channel") {
		t.Errorf("error = %v; the registry has no arbiter, so a granted approval could never be honoured", execErr)
	}
}

// TestResolverGivesEachAgentTheSharedPromptSegments: a plugin mounted after
// serve started must still reach every agent's prompt. That works only if the
// resolver hands each context builder the SAME store the plugin host writes
// to, rather than a snapshot of its contents.
func TestResolverGivesEachAgentTheSharedPromptSegments(t *testing.T) {
	t.Parallel()

	segments := prompt.NewSegments(nil)
	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Gate: taskgate.NewTaskGate(),
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {ID: "agent-researcher", Role: "researcher", MaasProfile: "deep"},
		}),
		RootConfig: config.Config{
			ContextFiles: config.ContextFilesConfig{Root: t.TempDir()},
			Runtime:      config.RuntimeConfig{MaxToolRounds: 1},
		},
		Audit:          adapter.NewMemoryAuditLog(),
		Events:         adapter.NewMemoryEventBus(),
		PluginSegments: segments,
		MaasFactory: func(string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: &resolverCaptureMaas{response: "ok"}}, nil
		},
	})

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

	// Mounted AFTER the runtime was built, exactly as a `plugins reload` does.
	segments.Add("legion-jira", "Prefer ticket links.")

	built, err := rt.contextBuilder.BuildContext(context.Background(), cognitive.Request{
		Agent: domain.Agent{ID: "a1", Role: "researcher"},
		Task:  domain.Task{ID: "task-1", Input: "go"},
	})
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if !strings.Contains(built.Prompt, "Prefer ticket links.") {
		t.Errorf("prompt = %q, want the segment a plugin added after wiring", built.Prompt)
	}
}
