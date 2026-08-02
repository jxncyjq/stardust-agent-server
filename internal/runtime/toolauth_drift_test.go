package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/taskledger"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/toolauth"
)

// A production runtime registry (default runtime path) must contain no non-meta
// tool that toolauth cannot gate: a newly added tool that nobody listed in the
// gateable catalog would silently be un-disableable, which is exactly the
// drift this guard forbids. Adding a tool now requires adding it to
// toolauth.gateable, or this fails.
func TestEveryProductionToolIsGateable(t *testing.T) {
	registry := productionToolRegistryForTest(t)
	gateable := toolauth.GateableToolNames()
	for _, d := range registry.Descriptors() {
		if d.Name == "call_tool" || d.Name == "load_capabilities" {
			continue
		}
		if !gateable[d.Name] {
			t.Errorf("production tool %q is not in toolauth.GateableTools() — add it", d.Name)
		}
	}
}

// productionToolRegistryForTest builds the full production tool registry,
// mirroring cli.defaultTaskRunner.RunTask (internal/cli/command.go) — the
// default runtime's construction sequence, which is a strict superset of the
// per-agent (worker) registry built by AgentRuntimeResolver.ResolveTaskRunner
// (internal/runtime/agent_resolver.go): it additionally carries the
// orchestrator-tier session_search, moa_consult and delegate_task tools (see
// the rationale comment on ResolveTaskRunner and
// TestResolverOmitsOrchestratorOnlyTools, which lock that asymmetry). Using
// the default runtime's sequence here, rather than the resolver's, is what
// makes this guard see every production tool, not just the worker subset.
func productionToolRegistryForTest(t *testing.T) *tool.Registry {
	t.Helper()

	tools := tool.NewFileReadWriteWorkspaceRegistry(t.TempDir(), nil)

	ledger, err := taskledger.New(taskledger.Config{WorkspaceRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("taskledger.New() error = %v, want nil", err)
	}
	tool.RegisterTaskLedgerTools(tools, ledger)
	tool.RegisterAgentMessageTools(tools, driftGuardMessageStore{})
	tool.RegisterWebTools(tools, tool.WebToolOptions{Enabled: true})
	tool.RegisterSessionSearchTool(tools, driftGuardMessageSearcher{})
	RegisterMoAConsultTool(tools, mapResolver{})

	// delegate_task is registered by the runtime itself (Runtime.RegisterDelegateTaskTool),
	// not by a package-level tool.RegisterXxx function — mirror
	// defaultTaskRunner.RunTask exactly: build the runtime with this registry,
	// then let it add delegate_task on top. A default-constructed runtime
	// (Depth 0) resolves to role "orchestrator" (see NewRuntime), so
	// canDelegate() is true and delegate_task actually registers.
	rt := NewRuntime(Config{Tools: tools})
	rt.RegisterDelegateTaskTool(tools)

	return tools
}

// driftGuardMessageStore is a registration-only tool.AgentMessageStore double:
// RegisterAgentMessageTools only needs a non-nil store to register its
// descriptors, and this test never executes a tool handler, so the methods
// are never actually invoked.
type driftGuardMessageStore struct{}

func (driftGuardMessageStore) SaveAgentMessage(context.Context, domain.AgentMessage) error {
	return nil
}

func (driftGuardMessageStore) ListAgentMessages(context.Context, domain.AgentMessageQuery) ([]domain.AgentMessage, error) {
	return nil, nil
}

func (driftGuardMessageStore) MarkAgentMessageRead(context.Context, string, time.Time) error {
	return nil
}

// driftGuardMessageSearcher is a registration-only tool.MessageSearcher
// double, analogous to driftGuardMessageStore above.
type driftGuardMessageSearcher struct{}

func (driftGuardMessageSearcher) SearchMessages(context.Context, string, int) ([]domain.ConversationTurn, error) {
	return nil, nil
}

func (driftGuardMessageSearcher) ScrollMessages(context.Context, string, string, int) ([]domain.ConversationTurn, error) {
	return nil, nil
}

func (driftGuardMessageSearcher) BrowseSessions(context.Context, int) ([]domain.AgentSession, error) {
	return nil, nil
}
