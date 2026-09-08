package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
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

// fakeDelegationAgents 只回答「有没有这个名字」，不负责真的造子运行时；上面那两条
// 用例走的是 validateSubTaskSpec，够不到这条路径。
func (f *fakeDelegationAgents) ResolveDelegate(context.Context, string, DelegationContext) (domain.Agent, *Runtime, error) {
	return domain.Agent{}, nil, errors.New("fakeDelegationAgents does not resolve")
}

// recordingDelegationAgents 记下委派侧递过来的上下文，好让下面的用例断言「委派侧
// 决定的那几项」原样到达解析器。
type recordingDelegationAgents struct {
	fakeDelegationAgents
	resolveErr  error
	lastContext DelegationContext
	// lastEpisodes 挂在最近一次造出来的子运行时上，用来看清「子任务到底以谁的身份
	// 跑起来的」——RunTask 把 domain.Agent 原样交给 EpisodeRecorder。
	lastEpisodes *capturingEpisodeRecorder
}

// capturingEpisodeRecorder 只记下 RunTask 传进来的身份。
type capturingEpisodeRecorder struct {
	agent domain.Agent
}

func (c *capturingEpisodeRecorder) RecordEpisode(agent domain.Agent, _ domain.Task, _ string, _ string) {
	c.agent = agent
}

func (r *recordingDelegationAgents) ResolveDelegate(ctx context.Context, id string, dc DelegationContext) (domain.Agent, *Runtime, error) {
	r.lastContext = dc
	if r.resolveErr != nil {
		return domain.Agent{}, nil, r.resolveErr
	}
	// 结构体字面量绕过了 NewRuntime，所以 NewRuntime 会兜住的那几个 sink（audit /
	// events / logger）必须在这里自己填，否则子任务一跑就 nil 解引用。
	r.lastEpisodes = &capturingEpisodeRecorder{}
	child := &Runtime{
		maas:            &recordingSubMaas{summary: "ok"},
		audit:           noopAuditLog{},
		events:          noopEventBus{},
		gate:            taskgate.NewTaskGate(),
		logger:          slog.Default(),
		role:            dc.Role,
		depth:           dc.Depth,
		maxSpawnDepth:   dc.MaxSpawnDepth,
		episodeRecorder: r.lastEpisodes,
	}
	return domain.Agent{ID: id, Role: "researcher-role"}, child, nil
}

// 具名委派的子代理必须带着父的委派上下文出生 —— 尤其 depth。
// 用 ResolveTaskRunner 造出来的 runtime 是给顶层任务用的，depth 出生即 0；
// 直接拿来当子代理，递归深度防护就没了。
func TestNamedDelegationChildIsBornBelowItsParent(t *testing.T) {
	t.Parallel()
	agents := &recordingDelegationAgents{fakeDelegationAgents: fakeDelegationAgents{names: []string{"researcher"}}}
	parent := NewRuntime(Config{
		Gate:             taskgate.NewTaskGate(),
		Maas:             &recordingSubMaas{summary: "ok"},
		DelegationAgents: agents,
		MaxSpawnDepth:    3,
	})

	if _, err := parent.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "dig",
		AgentID:      "researcher",
	}); err != nil {
		t.Fatalf("RunSubTask() error = %v, want nil", err)
	}

	if agents.lastContext.Depth != parent.depth+1 {
		t.Errorf("resolved child Depth = %d, want %d (parent depth + 1): a child born at depth 0 defeats the recursion guard",
			agents.lastContext.Depth, parent.depth+1)
	}
	if agents.lastContext.MaxSpawnDepth != parent.maxSpawnDepth {
		t.Errorf("resolved child MaxSpawnDepth = %d, want the parent's %d",
			agents.lastContext.MaxSpawnDepth, parent.maxSpawnDepth)
	}
}

// 两个 role 同名不同义：SubTaskSpec.Role 管「能不能再委派」，
// AgentConfig.Role 管「能调什么工具」。传下去的必须是前者。
func TestNamedDelegationPassesTheDelegationRoleNotTheAgentRole(t *testing.T) {
	t.Parallel()
	agents := &recordingDelegationAgents{fakeDelegationAgents: fakeDelegationAgents{names: []string{"researcher"}}}
	parent := NewRuntime(Config{
		Gate:             taskgate.NewTaskGate(),
		Maas:             &recordingSubMaas{summary: "ok"},
		DelegationAgents: agents,
		MaxSpawnDepth:    3,
	})

	if _, err := parent.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "dig",
		AgentID:      "researcher",
		Role:         roleOrchestrator,
	}); err != nil {
		t.Fatalf("RunSubTask() error = %v, want nil", err)
	}

	if agents.lastContext.Role != roleOrchestrator {
		t.Errorf("resolved child Role = %q, want %q: this field carries the DELEGATION role (leaf/orchestrator), not the agent's tool-permission role",
			agents.lastContext.Role, roleOrchestrator)
	}
}

// 解析失败必须原样传出来，不能被委派层吞成「那就克隆父吧」。
func TestNamedDelegationSurfacesAResolveFailure(t *testing.T) {
	t.Parallel()
	agents := &recordingDelegationAgents{fakeDelegationAgents: fakeDelegationAgents{names: []string{"researcher"}}, resolveErr: errors.New("maas profile boom")}
	parent := NewRuntime(Config{
		Gate:             taskgate.NewTaskGate(),
		Maas:             &recordingSubMaas{summary: "ok"},
		DelegationAgents: agents,
		MaxSpawnDepth:    3,
	})

	_, err := parent.RunSubTask(context.Background(), SubTaskSpec{ParentTaskID: "t1", Goal: "dig", AgentID: "researcher"})
	if err == nil {
		t.Fatal("RunSubTask() error = nil when resolving the agent failed, want the failure surfaced")
	}
	if !strings.Contains(err.Error(), "maas profile boom") {
		t.Errorf("error = %v, want it to wrap the resolver's own failure", err)
	}
}

// 具名子代理必须以解析出来的那个 agent 的身份跑，而不是一个写死的身份。
// 上面三条都只看「递给解析器的是什么」，把 runChild 里那句身份换回写死的
// domain.Agent{Role: "developer"} 它们照样全绿——那正好抹掉本任务的成果：
// 解析出了目标 agent 的角色，却拿别人的身份去跑。
func TestNamedDelegationRunsAsTheResolvedAgent(t *testing.T) {
	t.Parallel()
	agents := &recordingDelegationAgents{fakeDelegationAgents: fakeDelegationAgents{names: []string{"researcher"}}}
	parent := NewRuntime(Config{
		Gate:             taskgate.NewTaskGate(),
		Maas:             &recordingSubMaas{summary: "ok"},
		DelegationAgents: agents,
		MaxSpawnDepth:    3,
	})

	if _, err := parent.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "dig",
		AgentID:      "researcher",
	}); err != nil {
		t.Fatalf("RunSubTask() error = %v, want nil", err)
	}

	if agents.lastEpisodes == nil {
		t.Fatal("no child runtime was built: the named delegation never reached the resolver")
	}
	got := agents.lastEpisodes.agent
	if got.Role != "researcher-role" {
		t.Errorf("child ran as domain.Agent.Role = %q, want the resolved agent's %q: a named child that runs under a fixed role is not running as the agent that was named",
			got.Role, "researcher-role")
	}
	if got.ID != "researcher" {
		t.Errorf("child ran as domain.Agent.ID = %q, want the resolved agent's %q", got.ID, "researcher")
	}
}

// 不指名道姓的那条路逐字不变：仍是父的克隆，身份仍是派生 id + "developer"。
// 身份是从子任务自己的提示词里读出来的（cognitive.Core 把 Agent/Role 写进去），
// 这样读到的就是 RunTask 真正收到的那个 domain.Agent。
func TestUnnamedDelegationStillRunsAsTheDerivedDeveloperIdentity(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "ok"}
	parent := NewRuntime(Config{
		Gate:           taskgate.NewTaskGate(),
		Maas:           maas,
		ContextBuilder: cognitive.NewCore(cognitive.NoopCompressor{}),
		MaxSpawnDepth:  3,
	})

	res, err := parent.RunSubTask(context.Background(), SubTaskSpec{ParentTaskID: "t1", Goal: "dig"})
	if err != nil {
		t.Fatalf("RunSubTask() error = %v, want nil", err)
	}
	prompts := maas.recorded()
	if len(prompts) == 0 {
		t.Fatal("the child never issued an inference: nothing to read its identity from")
	}
	joined := strings.Join(prompts, "\n")
	if !strings.Contains(joined, "Agent: "+res.TaskID) {
		t.Errorf("child prompt does not carry Agent: %s, want the derived sub-task id:\n%s", res.TaskID, joined)
	}
	if !strings.Contains(joined, "Role: developer") {
		t.Errorf("child prompt does not carry Role: developer, want the unnamed path unchanged:\n%s", joined)
	}
}

// offeredToolNames runs task on rt and collects the tool names actually
// offered to the model in the first inference request -- the same
// black-box check TestResolverAppliesDisabledTools and
// TestEffectiveToolsRemovesDisabledTool use, because DisabledTools is never
// baked into Runtime.tools itself: it is applied fresh on every call by
// Runtime.effectiveTools (tools.Without(r.disabledTools...)), so inspecting
// the stored tools field would miss it entirely. Asking what the model was
// actually offered exercises both DisabledTools and any Toolsets narrowing
// baked into tools at construction, together, exactly as a real dispatch
// would see them.
func offeredToolNames(t *testing.T, rt *Runtime, agent domain.Agent, maas *recordingRoundsMaas) map[string]bool {
	t.Helper()
	task := domain.Task{ID: "sub-task-tools", Input: "go"}
	if _, err := rt.RunTask(context.Background(), agent, task); err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if len(maas.requests) == 0 {
		t.Fatal("no inference request captured; nothing to read the offered tools from")
	}
	names := make(map[string]bool)
	for _, tl := range maas.requests[0].Tools {
		names[tl.Name] = true
	}
	return names
}

// newToolsetIntersectionResolver builds a resolver with one configured agent
// ("researcher"), wired to maas so the caller can inspect what tools a
// ResolveDelegate-built runtime actually offers the model. disabledTools is
// the target agent's own agentregistry.AgentConfig.DisabledTools.
func newToolsetIntersectionResolver(t *testing.T, disabledTools []string, maas *recordingRoundsMaas) *AgentRuntimeResolver {
	t.Helper()
	return NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Gate: taskgate.NewTaskGate(),
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {ID: "agent-researcher", Role: "researcher", MaasProfile: "deep", DisabledTools: disabledTools},
		}),
		RootConfig: config.Config{
			ContextFiles: config.ContextFilesConfig{Root: t.TempDir()},
			Runtime:      config.RuntimeConfig{MaxToolRounds: 1},
		},
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
		MaasFactory: func(string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: maas}, nil
		},
	})
}

// TestResolveDelegateKeepsTheAgentsOwnDenyList guards Task 4 half 1: a named
// agent's own agentCfg.DisabledTools must still apply to a delegated child
// even now that ResolveDelegate accepts a non-empty dc.Toolsets for a named
// agent -- accepting Toolsets must not come at the cost of silently dropping
// the target's own standing deny-list.
func TestResolveDelegateKeepsTheAgentsOwnDenyList(t *testing.T) {
	t.Parallel()
	maas := &recordingRoundsMaas{responses: []port.InferenceResponse{{Text: "done"}}}
	resolver := newToolsetIntersectionResolver(t, []string{"write_file"}, maas)

	agent, child, err := resolver.ResolveDelegate(context.Background(), "researcher", DelegationContext{
		Depth: 1, MaxSpawnDepth: 3,
	})
	if err != nil {
		t.Fatalf("ResolveDelegate() error = %v, want nil", err)
	}

	names := offeredToolNames(t, child, agent, maas)
	if names["write_file"] {
		t.Errorf("offered tools = %v, want write_file absent: the target agent's own DisabledTools must still deny it", names)
	}
	if !names["read_file"] {
		t.Errorf("offered tools = %v, want read_file present: the deny-list must remove only the named tool, not everything", names)
	}
}

// TestResolveDelegateAlsoAppliesTheCallersNarrowing guards Task 4 half 2: a
// parent's dc.Toolsets must narrow a named delegate's tools down to exactly
// that subset when the target agent disables nothing of its own -- this is
// the behaviour Task 3 hard-refused and Task 4 is meant to turn into a real
// intersection.
func TestResolveDelegateAlsoAppliesTheCallersNarrowing(t *testing.T) {
	t.Parallel()
	maas := &recordingRoundsMaas{responses: []port.InferenceResponse{{Text: "done"}}}
	resolver := newToolsetIntersectionResolver(t, nil, maas)

	agent, child, err := resolver.ResolveDelegate(context.Background(), "researcher", DelegationContext{
		Depth: 1, MaxSpawnDepth: 3, Toolsets: []string{"read_file"},
	})
	if err != nil {
		t.Fatalf("ResolveDelegate() error = %v, want nil", err)
	}

	names := offeredToolNames(t, child, agent, maas)
	if !names["read_file"] {
		t.Errorf("offered tools = %v, want read_file present: it is the one name the caller asked for", names)
	}
	for _, absent := range []string{"write_file", "search_content", "list_files"} {
		if names[absent] {
			t.Errorf("offered tools = %v, want %q absent: the caller's toolsets narrowing must exclude everything not named", names, absent)
		}
	}
}
