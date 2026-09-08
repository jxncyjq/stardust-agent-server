package runtime

import (
	"sort"
	"testing"

	"github.com/stardust/legion-agent/internal/taskgate"
)

// fakeDelegationAgents is a stand-in for the real agent registry, so these
// tests do not need a configured deployment on disk.
type fakeDelegationAgents struct {
	names []string
}

func (f *fakeDelegationAgents) AgentNames() []string {
	out := append([]string(nil), f.names...)
	sort.Strings(out)
	return out
}

func (f *fakeDelegationAgents) HasAgent(id string) bool {
	for _, name := range f.names {
		if name == id {
			return true
		}
	}
	return false
}

// 注入的解析器必须被子代理继承 —— 否则「定义了接口但没人接」。
func TestSubRuntimeInheritsTheDelegationAgents(t *testing.T) {
	t.Parallel()
	agents := &fakeDelegationAgents{names: []string{"researcher"}}
	parent := NewRuntime(Config{
		Gate:             taskgate.NewTaskGate(),
		Maas:             &recordingSubMaas{summary: "ok"},
		DelegationAgents: agents,
	})

	child, err := parent.newSubRuntime(roleLeaf, nil)
	if err != nil {
		t.Fatalf("newSubRuntime() error = %v, want nil", err)
	}
	if child.delegationAgents == nil {
		t.Fatal("child.delegationAgents is nil: a child that cannot resolve agent names can never delegate by name")
	}
	if !child.delegationAgents.HasAgent("researcher") {
		t.Error("child.delegationAgents does not see the configured agent the parent was given")
	}
}

// 没注入时是 nil —— 这是合法状态（部署可以没有 agent 目录），
// 由 Task 2 决定「传了 agent_id 却没有解析器」时硬拒。
func TestRuntimeWithoutDelegationAgentsHasNilResolver(t *testing.T) {
	t.Parallel()
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: &recordingSubMaas{summary: "ok"}})
	if parent.delegationAgents != nil {
		t.Error("delegationAgents is non-nil without Config.DelegationAgents; a runtime must not invent an agent registry")
	}
}
