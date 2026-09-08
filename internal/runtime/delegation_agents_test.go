package runtime

import (
	"sort"
	"strings"
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

// 名字不存在要硬拒，并且说出有哪些可选 —— 光说「不认识」等于让模型盲猜。
func TestValidateRefusesAnUnknownAgentIDAndListsTheKnownOnes(t *testing.T) {
	t.Parallel()
	parent := NewRuntime(Config{
		Gate:             taskgate.NewTaskGate(),
		Maas:             &recordingSubMaas{summary: "ok"},
		DelegationAgents: &fakeDelegationAgents{names: []string{"researcher", "reviewer"}},
	})

	err := parent.validateSubTaskSpec(SubTaskSpec{Goal: "dig", AgentID: "resercher"})
	if err == nil {
		t.Fatal("validateSubTaskSpec() error = nil for an unknown agent_id, want a refusal")
	}
	if !strings.Contains(err.Error(), "resercher") {
		t.Errorf("error = %v, want it to name the agent it does not recognise", err)
	}
	for _, known := range []string{"researcher", "reviewer"} {
		if !strings.Contains(err.Error(), known) {
			t.Errorf("error = %v, want it to list the configured agent %q", err, known)
		}
	}
}

// 名字存在就放行。
func TestValidateAcceptsAConfiguredAgentID(t *testing.T) {
	t.Parallel()
	parent := NewRuntime(Config{
		Gate:             taskgate.NewTaskGate(),
		Maas:             &recordingSubMaas{summary: "ok"},
		DelegationAgents: &fakeDelegationAgents{names: []string{"researcher"}},
	})
	if err := parent.validateSubTaskSpec(SubTaskSpec{Goal: "dig", AgentID: "researcher"}); err != nil {
		t.Errorf("validateSubTaskSpec() error = %v, want nil for a configured agent", err)
	}
}

// 没有解析器却指名道姓 —— 必须拒，绝不能悄悄当成「克隆父」跑掉。
// 那会让调用方以为选中了某个 agent，实际跑的是别的东西。
func TestValidateRefusesAnAgentIDWhenNoRegistryIsWired(t *testing.T) {
	t.Parallel()
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: &recordingSubMaas{summary: "ok"}})

	err := parent.validateSubTaskSpec(SubTaskSpec{Goal: "dig", AgentID: "researcher"})
	if err == nil {
		t.Fatal("validateSubTaskSpec() error = nil with no agent registry, want a refusal rather than a silent clone of the parent")
	}
	if !strings.Contains(err.Error(), "researcher") {
		t.Errorf("error = %v, want it to name the agent that could not be resolved", err)
	}
}

// 不指名道姓的既有路径不受影响。
func TestValidateStillAcceptsADelegationWithoutAnAgentID(t *testing.T) {
	t.Parallel()
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: &recordingSubMaas{summary: "ok"}})
	if err := parent.validateSubTaskSpec(SubTaskSpec{Goal: "dig"}); err != nil {
		t.Errorf("validateSubTaskSpec() error = %v, want nil: an unnamed delegation must keep working", err)
	}
}

// TestDelegateTaskDescriptorAdvertisesAgentID 守住 schema 那半条接线：模型只看
// InputSchema 决定它能不能传 agent_id，validateSubTaskSpec 校验得再严，schema 里没有
// 这个字段，模型就永远不会知道可以传它。删掉 InputSchema 里的 agent_id 条目不会让
// go build/go vet/go test 变红——上面那几条测试直接构造 SubTaskSpec{AgentID: ...}，
// 完全绕过了 schema——所以这条必须单独断言 schema 本身。
func TestDelegateTaskDescriptorAdvertisesAgentID(t *testing.T) {
	t.Parallel()
	schema := delegateTaskDescriptor().InputSchema
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("delegateTaskDescriptor().InputSchema[\"properties\"] = %v (%T), want a map[string]any", schema["properties"], schema["properties"])
	}
	if _, ok := properties["agent_id"]; !ok {
		t.Error(`delegateTaskDescriptor().InputSchema["properties"] has no "agent_id" entry: ` +
			"a model reading only the schema can never discover the field, no matter how strict validateSubTaskSpec is")
	}
}
