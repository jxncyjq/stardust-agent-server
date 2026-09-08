# 按名字委派给已配置 agent 实施计划（Spec B）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `delegate_task` 能按名字选中一个已配置的 agent，并真正按那个 agent 的配置（role / 工具授权 / 模型 profile / 工作目录 / skills / context files）运行子代理。

**Architecture:** 给 `Runtime` 注入一个委派用的 agent 解析接缝；`agent_id` 正式进 `InputSchema` 并入既有的 `validateSubTaskSpec` 校验；具名委派复用 `AgentRuntimeResolver` 的 per-agent 装配逻辑，但**委派上下文（`depth+1` / `maxSpawnDepth` / `leaf`\|`orchestrator`）由委派侧决定**，子代理绝不以 `depth` 0 出生。

**Tech Stack:** Go；标准 `testing`。

规格：`docs/superpowers/specs/2026-09-08-delegate-to-configured-agent-design.md`

## Global Constraints

- **fail-loud 铁律**（本仓 CLAUDE.md 最高优先级）：不许回落零值、不许吞错误、不许静默跳过、不许「拿不到就当没配置」。错误用 `fmt.Errorf("...: %w", err)` 包装，**错误点必须可定位**。
- **注释是契约**：注释里每条事实陈述必须与代码一致，且**不得出现关于「谁调用它 / 有没有调用方 / 某个东西落没落地」的句子**。凡是提到别的文件 / 常量 / 包行为的句子，**亲自去那个文件确认再写**。**写封闭枚举（"Only …" / "Two …"）之前先把所有出口数一遍**——Spec A 期间在这上面栽过三次。
- **不得引入包级可变量**（包级函数不受此限）。
- **子代理绝不能以 `depth` 0 出生。**
- **两个同名不同义的 role 必须分清**：`AgentConfig.Role`（`developer` 等，管能调什么工具，进 `domain.Agent`）vs `SubTaskSpec.Role`（`leaf`/`orchestrator`，管能不能再委派）。
- **不传 `agent_id` 的默认路径行为逐字不变。**
- **传了 `agent_id` 但没生效 = 缺陷**：resolver 为 nil 时必须硬拒，不许静默回落到克隆父。
- **`agent_id` 的校验并入既有 `validateSubTaskSpec`**，不另起一处判断。
- 全绿判据：`go test ./... -count=1 -p 1 -timeout 900s`；`go test ./internal/runtime/ -race -count=1`；`gofmt -l .` 为空；`go vet ./...` 干净。**测退出码不要接管道**（管道会让 `$?` 取到 `tail` 的状态）。
- 只按**显式路径** `git add`，**不用 `git add -A`**。提交信息正文中文，结尾一行 `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`。

## 本期最该防的那个形状

「**接缝在，但没人测那条接缝**」——上一期复发八次，Spec A 期间又复发多次（batch 出口、async 入口接线、`max_rounds` 话术臂、常量字面值、nil 注册表分支），**每一次都靠变异实证抓出来，静态审查一次都没抓住**。

**每个任务都必须做变异验证**：把它要守的规则在实现里失效掉（**`go vet` 必须干净，不能只造成编译失败**），确认对应用例 FAIL，再改回来确认 PASS。**哪条变异之后测试仍然全绿，就说明那条规则没人守着，必须补测试而不是放过。**

**还原纪律**：每条变异从干净副本重新复制再打；还原后用 `md5sum` **且** `git diff` 确认与提交态一致再跑下一条。**先用一条空变异（把基线原样写回）验证 harness 真会报红**——Spec A 期间有复审者靠这一步揪出了自己脚本的 Windows CRLF bug。

本期尤其要盯这四条接线：
1. resolver 真的被注入到了子代理路径（不是定义了接口没人接）
2. 子代理**不以 `depth` 0 出生**
3. 两个 role **没有被接串**
4. batch 模式**每条各自的 `agent_id`** 都生效（不是只认第一条）

## 既有代码的关键事实（实施者必读）

- `validateSubTaskSpec`（`internal/runtime/delegation.go:90-120`）已存在，是纯函数，由 `RunSubTask` / `RunSubTasks`（整批预检）/ `RunSubTaskAsync` **三处共用**。今天校验 `goal` / `role` / 深度 / `toolsets`（用 `r.tools.Descriptors()` 解析链）。
- `runChild`（`internal/runtime/delegation.go:221`）里 `agent := domain.Agent{ID: agentID, Role: "developer"}`（`:239`）——**写死的 role 就在这里**。
- `delegateTaskArgs.AgentID`（`internal/runtime/delegation_tool.go:20`）与 `handleDelegateTask` 读 `call.Arguments["agent_id"]`（`:96`）都已存在；**缺的是 `InputSchema` 里的 properties 条目**。
- `AgentRuntimeResolver`（`internal/runtime/agent_resolver.go:133`）持有 `registry *agentregistry.Registry`、`gate`、`sessionEvents`、`maasFactory`、`toolGate` 等；`ResolveTaskRunner(ctx, task) (domain.Agent, TaskRunner, bool, error)`（`:222`）按 `task.AgentID` 查 registry，校验 `DisabledTools` 全在 `toolauth.GateableToolNames()`（`internal/toolauth/catalog.go:61`）里，建 `NewRuntime(Config{... Gate: r.gate, SessionEvents: r.sessionEvents ...})`。**它建的 Runtime `depth` 出生即 0。**
- `agentregistry.Registry.Get(name) (AgentConfig, bool)`（`internal/agentregistry/registry.go:66`）、`.Names() []string`（`:71`）。
- `AgentConfig`（`internal/agentregistry/config.go:5`）：`ID` / `Role` / `MaasProfile` / `ContextFiles` / `Workspace` / `Skills` / `DisabledTools`。
- `TaskRunner interface { RunTask(context.Context, domain.Agent, domain.Task) (domain.TaskRun, error) }`（`internal/runtime/coordinator.go:37-39`）——**传不进委派上下文**。
- **接线顺序**：`internal/cli/command.go:3002` 先建 resolver，默认 Runtime 后建 → **resolver 可以直接进 Runtime 的 Config**。resolver 自己建的 per-agent runtime 要能再委派，靠 `ResolveTaskRunner` 内部把 `r` 自己传下去——**没有构造环**。
- 审计事件形状（照抄 `RunSubTaskAsync` 里既有的用法）：`domain.AuditEvent{ID, RequestID, SubjectType, SubjectID, Action, Hash, CreatedAt}`。
- 测试夹具：`recordingSubMaas{summary: "..."}`（`internal/runtime/delegation_test.go:17`，`recorded()` 在 `:33`）、`unchangingReadRegistry(t)`（`internal/runtime/multiturn_test.go`）。构造：`NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t)})`。

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/runtime/delegation_agents.go`（新建） | 委派用的 agent 解析接缝：接口定义 + 具名委派的装配入口 |
| `internal/runtime/runtime.go` | `Config` 与 `Runtime` 加解析器字段；`newSubRuntime` 把它带给子代理 |
| `internal/runtime/delegation.go` | `validateSubTaskSpec` 加 `agent_id` 校验；`runChild` 不再写死 role；具名委派走 per-agent 装配；审计 |
| `internal/runtime/delegation_tool.go` | `agent_id` 进 `InputSchema` |
| `internal/runtime/agent_resolver.go` | 实现新接缝；把自己传给它建的 runtime |
| `internal/cli/command.go` | 把 resolver 接给默认 Runtime |

---

### Task 1: 委派用的 agent 解析接缝与接线

**Files:**
- Create: `internal/runtime/delegation_agents.go`
- Modify: `internal/runtime/runtime.go`（`Config` 与 `Runtime` 加字段；`newSubRuntime` 带下去）
- Modify: `internal/runtime/agent_resolver.go`（实现接口；把自己传给它建的 runtime）
- Modify: `internal/cli/command.go:3002` 一带（把 resolver 接给默认 Runtime）
- Test: `internal/runtime/delegation_agents_test.go`（新建）

**Interfaces:**
- Consumes: 无
- Produces: `type DelegationAgents interface { AgentNames() []string; HasAgent(id string) bool }`；`Config.DelegationAgents DelegationAgents`；`Runtime.delegationAgents DelegationAgents`

- [ ] **Step 1: 写失败的测试**

新建 `internal/runtime/delegation_agents_test.go`：

```go
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run "DelegationAgents" -count=1`
Expected: 编译失败，`unknown field DelegationAgents in struct literal`。

- [ ] **Step 3: 定义接缝**

新建 `internal/runtime/delegation_agents.go`：

```go
package runtime

// DelegationAgents is the set of configured agents a delegation may name. It is
// the narrow half of the agent registry the delegation path needs: enough to
// decide whether a requested agent_id exists and to say which names are
// available when it does not.
//
// A nil DelegationAgents means this deployment configured no agent directory.
// That is a legitimate state, not a fallback: delegation without agent_id keeps
// working. What must never happen is a request that names an agent being served
// as if it had not — see validateSubTaskSpec.
type DelegationAgents interface {
	// AgentNames returns the configured agent ids, sorted, for error messages.
	AgentNames() []string
	// HasAgent reports whether id names a configured agent.
	HasAgent(id string) bool
}
```

- [ ] **Step 4: Runtime 持有它并传给子代理**

在 `internal/runtime/runtime.go` 的 `Config` 里，紧邻 `MaxSpawnDepth` 加：

```go
	// DelegationAgents lets delegate_task name a configured agent. Nil means
	// this deployment has no agent directory; naming one is then refused.
	DelegationAgents DelegationAgents
```

在 `Runtime` 结构体里加：

```go
	delegationAgents DelegationAgents
```

在 `NewRuntime` 里赋值：

```go
		delegationAgents:      cfg.DelegationAgents,
```

在 `newSubRuntime` 构造 `child := &Runtime{...}` 的字面量里加（紧邻 `maxSpawnDepth`）：

```go
		// Carried like tools and the deny-list: a child that lost it could not
		// resolve an agent name its parent could.
		delegationAgents: r.delegationAgents,
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/runtime/ -run "DelegationAgents" -count=1`
Expected: PASS，两个用例都过。

- [ ] **Step 6: 让 `AgentRuntimeResolver` 实现它，并把自己传下去**

在 `internal/runtime/agent_resolver.go` 末尾加：

```go
// AgentNames implements DelegationAgents.
func (r *AgentRuntimeResolver) AgentNames() []string {
	names := r.registry.Names()
	sort.Strings(names)
	return names
}

// HasAgent implements DelegationAgents.
func (r *AgentRuntimeResolver) HasAgent(id string) bool {
	_, ok := r.registry.Get(id)
	return ok
}
```

若该文件尚未 import `sort`，补上。

在 `ResolveTaskRunner` 里 `runner := NewRuntime(Config{...})` 的字面量中加一行，让它建的每个 per-agent runtime 也能按名字委派：

```go
		DelegationAgents:      r,
```

- [ ] **Step 7: 接线到默认 Runtime**

在 `internal/cli/command.go`（`resolver := agentruntime.NewAgentRuntimeResolver(...)` 之后、默认 Runtime 构造处）的 `Config{...}` 里加：

```go
		DelegationAgents:      resolver,
```

**先读那处 `Config{` 字面量确认字段风格与缩进再落笔。**

- [ ] **Step 8: 跑全量确认没碰坏别处**

Run: `go build ./... && go test ./internal/runtime/ ./internal/cli/ -count=1`
Expected: PASS。

- [ ] **Step 9: 变异验证**

每条：改坏 → `go vet ./internal/runtime/`（必须干净）→ 跑测试（必须 FAIL）→ 还原 → 确认 PASS。

1. `newSubRuntime` 里删掉 `delegationAgents: r.delegationAgents,` 那一行 → `TestSubRuntimeInheritsTheDelegationAgents` 必红
2. `NewRuntime` 里把 `delegationAgents: cfg.DelegationAgents` 改成不赋值 → 同一条必红

- [ ] **Step 10: 提交**

```bash
git add internal/runtime/delegation_agents.go internal/runtime/delegation_agents_test.go internal/runtime/runtime.go internal/runtime/agent_resolver.go internal/cli/command.go
git commit -m "feat(runtime): 委派路径接入已配置 agent 的解析接缝"
```

---

### Task 2: `agent_id` 进 schema 并入既有校验

**Files:**
- Modify: `internal/runtime/delegation_tool.go`（`InputSchema` 加 `agent_id`）
- Modify: `internal/runtime/delegation.go:90-120`（`validateSubTaskSpec`）
- Test: `internal/runtime/delegation_agents_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `Runtime.delegationAgents`
- Produces: 无新符号（校验并入既有函数）

- [ ] **Step 1: 写失败的测试**

追加到 `internal/runtime/delegation_agents_test.go`：

```go
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
```

把 `"strings"` 加进该文件的 import 块。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run "ValidateRefuses|ValidateAccepts|ValidateStill" -count=1`
Expected: FAIL——今天 `agent_id` 不参与任何校验，四条里前三条不返回错误。

- [ ] **Step 3: 校验并入 `validateSubTaskSpec`**

在 `internal/runtime/delegation.go` 的 `validateSubTaskSpec` 里，`toolsets` 那段**之后**、`return nil` **之前**加：

```go
	// Naming an agent is a request to run as THAT agent's configuration. A
	// deployment with no agent directory cannot honour it, and an unknown name
	// cannot either; both refuse rather than quietly running the parent's clone
	// under someone else's label.
	if spec.AgentID != "" {
		if r.delegationAgents == nil {
			return fmt.Errorf("validate sub task: agent_id %q was requested but this deployment has no configured agents", spec.AgentID)
		}
		if !r.delegationAgents.HasAgent(spec.AgentID) {
			return fmt.Errorf("validate sub task: agent_id %q is not a configured agent; configured agents are %v",
				spec.AgentID, r.delegationAgents.AgentNames())
		}
	}
```

- [ ] **Step 4: `agent_id` 进 `InputSchema`**

在 `internal/runtime/delegation_tool.go` 的 `delegateTaskDescriptor()` 的 `InputSchema.properties` 里，`toolsets` 之后加：

```go
				"agent_id": map[string]any{"type": "string", "description": "Optional id of a configured agent to run this sub-task as; it then runs with that agent's own role, tool authorisation, model profile and workspace. Omit to run as a plain clone of this agent."},
```

并在 `tasks` 那条的描述里，把 `{goal, context, role, toolsets}` 改成 `{goal, context, role, toolsets, agent_id}`。

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/runtime/ -count=1`
Expected: PASS，`internal/runtime` 全绿。

- [ ] **Step 6: 变异验证**

1. 删掉 `r.delegationAgents == nil` 那一支 → `TestValidateRefusesAnAgentIDWhenNoRegistryIsWired` 必红。**这条专抓「传了但没生效」**
2. 把 `!r.delegationAgents.HasAgent(...)` 改成 `false`（永不拒）→ `TestValidateRefusesAnUnknownAgentIDAndListsTheKnownOnes` 必红
3. 错误信息里去掉 `r.delegationAgents.AgentNames()` → 同一条必红（它断言了列出可用名字）
4. **从 `InputSchema` 里删掉 `agent_id` 条目** → 若测试仍全绿，说明 schema 这条接线没人守着，**补一条断言 `delegateTaskDescriptor().InputSchema` 里有 `agent_id` 的用例**，不要放过

- [ ] **Step 7: 提交**

```bash
git add internal/runtime/delegation.go internal/runtime/delegation_tool.go internal/runtime/delegation_agents_test.go
git commit -m "feat(runtime): agent_id 进 schema 并入既有委派校验"
```

---

### Task 3: 具名委派走 per-agent 装配

**Files:**
- Modify: `internal/runtime/delegation_agents.go`（加带委派上下文的解析接缝）
- Modify: `internal/runtime/agent_resolver.go`（实现它）
- Modify: `internal/runtime/delegation.go`（`RunSubTask` / `RunSubTaskAsync` 走具名路径；`runChild` 不再写死 role）
- Test: `internal/runtime/delegation_agents_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `DelegationAgents`
- Produces: `DelegationAgents` 接口新增方法
  `ResolveDelegate(ctx context.Context, id string, dc DelegationContext) (domain.Agent, *Runtime, error)`；
  `type DelegationContext struct { Depth int; MaxSpawnDepth int; Role string; Toolsets []string }`

**这是本计划最大的一块。** 两条硬约束：子代理**不以 `depth` 0 出生**；两个 role **不接串**。

- [ ] **Step 1: 写失败的测试**

追加到 `internal/runtime/delegation_agents_test.go`：

```go
// 具名委派的子代理必须带着父的委派上下文出生 —— 尤其 depth。
// 用 ResolveTaskRunner 造出来的 runtime 是给顶层任务用的，depth 出生即 0；
// 直接拿来当子代理，递归深度防护就没了。
func TestNamedDelegationChildIsBornBelowItsParent(t *testing.T) {
	t.Parallel()
	agents := &recordingDelegationAgents{names: []string{"researcher"}}
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
	agents := &recordingDelegationAgents{names: []string{"researcher"}}
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
	agents := &recordingDelegationAgents{names: []string{"researcher"}, resolveErr: errors.New("maas profile boom")}
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
```

在同一文件里，把 `fakeDelegationAgents` 扩成也能记录与实现新方法（**保持零值即旧行为**，Task 1 那两条用例不受影响）：

```go
type recordingDelegationAgents struct {
	fakeDelegationAgents
	resolveErr  error
	lastContext DelegationContext
}

func (r *recordingDelegationAgents) ResolveDelegate(ctx context.Context, id string, dc DelegationContext) (domain.Agent, *Runtime, error) {
	r.lastContext = dc
	if r.resolveErr != nil {
		return domain.Agent{}, nil, r.resolveErr
	}
	child := &Runtime{
		maas:          &recordingSubMaas{summary: "ok"},
		gate:          taskgate.NewTaskGate(),
		logger:        slog.Default(),
		role:          dc.Role,
		depth:         dc.Depth,
		maxSpawnDepth: dc.MaxSpawnDepth,
	}
	return domain.Agent{ID: id, Role: "researcher-role"}, child, nil
}
```

并给 `fakeDelegationAgents` 补一个同签名的方法，使它仍满足接口（Task 1 的两条用例不调用它）：

```go
func (f *fakeDelegationAgents) ResolveDelegate(context.Context, string, DelegationContext) (domain.Agent, *Runtime, error) {
	return domain.Agent{}, nil, errors.New("fakeDelegationAgents does not resolve")
}
```

补齐 import：`context`、`errors`、`log/slog`、`github.com/stardust/legion-agent/internal/domain`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run "NamedDelegation" -count=1`
Expected: 编译失败，`undefined: DelegationContext`。

- [ ] **Step 3: 扩接口**

在 `internal/runtime/delegation_agents.go` 里加：

```go
// DelegationContext is what the delegating side decides about a child, as
// opposed to what the named agent's own configuration decides. A resolver must
// apply these verbatim: a child built at depth 0 would sit outside the spawn
// ceiling its parent is already inside.
type DelegationContext struct {
	// Depth is the child's depth: the parent's depth plus one.
	Depth int
	// MaxSpawnDepth is the ceiling the parent runs under, carried down unchanged.
	MaxSpawnDepth int
	// Role is the DELEGATION role (roleLeaf or roleOrchestrator): whether this
	// child may delegate further. It is not the agent's tool-permission role,
	// which comes from that agent's own configuration.
	Role string
	// Toolsets, when non-empty, narrows the child further on top of whatever
	// its own configuration already denies.
	Toolsets []string
}
```

并在 `DelegationAgents` 接口里加：

```go
	// ResolveDelegate builds a child runtime for the named agent, applying that
	// agent's own configuration for role, tool authorisation, model profile and
	// workspace, and applying dc for everything the delegating side decides.
	ResolveDelegate(ctx context.Context, id string, dc DelegationContext) (domain.Agent, *Runtime, error)
```

补齐 `context` 与 `domain` 的 import。

- [ ] **Step 4: `RunSubTask` / `RunSubTaskAsync` 走具名路径**

在 `internal/runtime/delegation.go` 里加一个私有 helper（两个入口共用，**不要各写一遍**）：

```go
// childFor builds the runtime that will run one sub-task: the named agent's own
// runtime when spec names one, otherwise a clone of this runtime. It returns the
// domain.Agent to run as alongside it, because a named child runs as that
// agent's configured role rather than the fixed one an unnamed child uses.
func (r *Runtime) childFor(ctx context.Context, spec SubTaskSpec, subTaskID string) (domain.Agent, *Runtime, error) {
	if spec.AgentID == "" {
		child, err := r.newSubRuntime(spec.Role, spec.Toolsets)
		if err != nil {
			return domain.Agent{}, nil, err
		}
		return domain.Agent{ID: subTaskID, Role: "developer"}, child, nil
	}
	agent, child, err := r.delegationAgents.ResolveDelegate(ctx, spec.AgentID, DelegationContext{
		Depth:         r.depth + 1,
		MaxSpawnDepth: r.maxSpawnDepth,
		Role:          spec.Role,
		Toolsets:      spec.Toolsets,
	})
	if err != nil {
		return domain.Agent{}, nil, fmt.Errorf("resolve delegate agent %q: %w", spec.AgentID, err)
	}
	return agent, child, nil
}
```

`RunSubTask` 与 `RunSubTaskAsync` 里原本调 `r.newSubRuntime(spec.Role, spec.Toolsets)` 的两处，改成调 `r.childFor(ctx, spec, subTaskID)`，并把拿到的 `domain.Agent` 一路传给 `runChild`。

**`runChild` 的签名增加一个 `agent domain.Agent` 参数**，函数体里删掉 `agent := domain.Agent{ID: agentID, Role: "developer"}` 那一行（`delegation.go:239`），改用传进来的。

- [ ] **Step 5: `AgentRuntimeResolver` 实现 `ResolveDelegate`**

在 `internal/runtime/agent_resolver.go` 里加。**它必须复用 `ResolveTaskRunner` 已有的 per-agent 装配逻辑**（查 registry、校验 `DisabledTools` 全在 `toolauth.GateableToolNames()` 里、建 maas runner、加载 context files），**不要另写一条**。把那段装配抽成一个共用的私有函数，由 `ResolveTaskRunner` 与 `ResolveDelegate` 各自带自己的上下文调用：`ResolveTaskRunner` 传 `depth 0`、角色留空；`ResolveDelegate` 传 `dc`。

**实现时先读 `ResolveTaskRunner` 全文**，把「哪些来自 agentCfg、哪些来自调用方」逐项分清再动手。

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/runtime/ -count=1`
Expected: PASS。

- [ ] **Step 7: 变异验证**

1. `childFor` 里把 `Depth: r.depth + 1` 改成 `Depth: 0` → `TestNamedDelegationChildIsBornBelowItsParent` 必红。**这条守递归防护**
2. 把 `Role: spec.Role` 改成传 agentCfg 的 role（模拟两个 role 接串）→ `TestNamedDelegationPassesTheDelegationRoleNotTheAgentRole` 必红
3. `childFor` 里把 `ResolveDelegate` 的错误吞掉、回落到 `newSubRuntime` → `TestNamedDelegationSurfacesAResolveFailure` 必红。**这条守「不静默回落」**
4. `runChild` 里把传进来的 agent 换回写死的 `domain.Agent{ID: agentID, Role: "developer"}` → 若测试仍全绿，**补一条断言具名子代理的 `domain.Agent.Role` 来自目标配置的用例**

- [ ] **Step 8: 提交**

```bash
git add internal/runtime/delegation_agents.go internal/runtime/delegation.go internal/runtime/agent_resolver.go internal/runtime/delegation_agents_test.go
git commit -m "feat(runtime): 具名委派走目标 agent 的 per-agent 装配"
```

---

### Task 4: 工具集取交集

**Files:**
- Modify: `internal/runtime/agent_resolver.go`（`ResolveDelegate` 的工具装配）
- Test: `internal/runtime/delegation_agents_test.go`（追加）

**Interfaces:**
- Consumes: Task 3 的 `DelegationContext.Toolsets`
- Produces: 无新符号

**规则**（spec §4.4）：父传的 `toolsets` 收窄与目标 agent 的 `DisabledTools` **两者都生效、取交集**。`toolsets` 是调用方对这一次委派的收窄意图，`DisabledTools` 是目标 agent 的固有限制，谁都不该抹掉谁。

- [ ] **Step 1: 写失败的测试**

这条要走真实的 `AgentRuntimeResolver`，而不是假 resolver。构造照抄 `internal/runtime/agent_resolver_test.go:33-41`：

```go
resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
	Gate:     taskgate.NewTaskGate(),
	Registry: agentregistry.New(map[string]agentregistry.AgentConfig{ /* 目标 agent 的配置 */ }),
	Audit:    adapter.NewMemoryAuditLog(),
	MaasFactory: func(string) (MaasRunnerFactoryResult, error) {
		return MaasRunnerFactoryResult{Client: &resolverCaptureMaas{response: "ok"}}, nil
	},
})
```

（`resolverCaptureMaas` 是同文件里的既有夹具。）然后写两条：

- `TestResolveDelegateKeepsTheAgentsOwnDenyList`：目标 agent 的 `DisabledTools` 含某工具，父没传 `toolsets` → 子代理**调不动**该工具。
- `TestResolveDelegateAlsoAppliesTheCallersNarrowing`：目标 agent 不禁用任何工具，父传 `Toolsets: []string{"read_file"}` → 子代理**只有** `read_file`。

断言用子 runtime 的 `tools.Descriptors()` 里有没有该名字。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run ResolveDelegate -count=1`
Expected: FAIL——今天 `ResolveDelegate` 还没处理 `Toolsets`。

- [ ] **Step 3: 实现交集**

在 `ResolveDelegate` 的工具装配处：先按目标 agent 的 `DisabledTools` 走既有的 deny 逻辑（`ResolveTaskRunner` 已有），**再**在其上套一层 `Subset(dc.Toolsets...)`（`dc.Toolsets` 非空时）。注释写明为什么是叠加而非二选一。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ -count=1`
Expected: PASS。

- [ ] **Step 5: 变异验证**

1. 去掉 `Subset(dc.Toolsets...)` 那一层 → `TestResolveDelegateAlsoAppliesTheCallersNarrowing` 必红
2. 去掉目标的 `DisabledTools` 处理 → `TestResolveDelegateKeepsTheAgentsOwnDenyList` 必红

**两侧各有一条变异**，缺一条就说明那一侧没人守。

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/agent_resolver.go internal/runtime/delegation_agents_test.go
git commit -m "feat(runtime): 具名委派的工具集取父收窄与目标禁用的交集"
```

---

### Task 5: 具名委派落审计

**Files:**
- Modify: `internal/runtime/delegation.go`（`childFor` 的具名分支）
- Test: `internal/runtime/delegation_agents_test.go`（追加）

**Interfaces:**
- Consumes: Task 3 的 `childFor`
- Produces: 审计动作名 `subtask_delegated_to_agent`

**为什么要有**（spec §4.2）：具名委派开出一条通道——受限 agent 可以委派给不受限 agent。这条通道的宽度等于运维配了什么，**但做选择的从人变成了模型**。审计不是为了拦，是为了事后答得出「谁把活派给了谁」。

- [ ] **Step 1: 写失败的测试**

追加一条：用既有的 `adapter.NewMemoryAuditLog()` 作为 `Config.Audit`，跑一次具名委派，再用既有 helper `mustAuditEvents(t, log)`（`internal/runtime/runtime_test.go:777`）把事件读出来，断言其中有一条 `Action == "subtask_delegated_to_agent"`，且它带得出父 agent、目标 agent、goal。**不要新建假审计类型。**

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run DelegationAudit -count=1`
Expected: FAIL，没有该审计事件。

- [ ] **Step 3: 落审计**

在 `childFor` 的具名分支里，**解析成功之后**追加一条审计（形状照抄 `RunSubTaskAsync` 里既有的 `domain.AuditEvent{ID, RequestID, SubjectType, SubjectID, Action, Hash, CreatedAt}` 用法）。

**审计写入失败按本仓惯例处理**：不静默吞掉——记结构化日志，并让委派继续（审计失败不该毙掉一次合法委派）。**这条取舍要写进注释。**

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ -count=1`
Expected: PASS。

- [ ] **Step 5: 变异验证**

1. 删掉整条审计写入 → 新用例必红
2. 把审计写入失败改成静默忽略（连日志都不记）→ 若测试仍全绿，**补一条断言「审计失败被记了日志」的用例**

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/delegation.go internal/runtime/delegation_agents_test.go
git commit -m "feat(runtime): 具名委派落审计，答得出谁把活派给了谁"
```

---

### Task 6: batch 每条各自的 `agent_id` 与默认路径回归

**Files:**
- Test: `internal/runtime/delegation_agents_test.go`（追加）
- Modify: 视测试结果而定（若 batch 路径确有缺陷）

**Interfaces:**
- Consumes: Task 2/3 的成果
- Produces: 无新符号

**为什么单独一个任务**：Spec A 期间，batch 出口漏测过一次——计划给的两条用例只覆盖 single，`delegateResultsView` 那条路靠变异当场抓出来才补上。**同一个形状不要再犯第二次。**

- [ ] **Step 1: 写测试**

追加两条：

- `TestBatchDelegationHonoursEachEntrysAgentID`：`tasks` 里两条，分别指定不同的 `agent_id`，断言**两条各自解析到了自己指定的那个**（不是都用第一条的，也不是都用默认路径）。用记录型假 resolver 记下每次 `ResolveDelegate` 的 `id`。
- `TestDelegationWithoutAgentIDStillClonesTheParent`：不传 `agent_id` 的完整一趟（single 与 batch 各一），断言**行为与改动前一致**——`domain.Agent.Role` 仍是 `"developer"`、resolver **一次都没被调用**。

- [ ] **Step 2: 跑测试**

Run: `go test ./internal/runtime/ -run "BatchDelegationHonours|DelegationWithoutAgentID" -count=1`
Expected: 若两条都 PASS，说明前面的任务已经把 batch 与默认路径都做对了——**记录这个结果，进 Step 4**。若有 FAIL，进 Step 3 修。

- [ ] **Step 3: 按失败情况修**

若 batch 没把每条的 `agent_id` 传下去，去 `RunSubTasks` 的 goroutine 里核 `spec` 是不是被闭包捕获错了；若默认路径调了 resolver，去 `childFor` 核 `spec.AgentID == ""` 那一支。**改完重跑 Step 2。**

- [ ] **Step 4: 变异验证**

1. `childFor` 里把 `spec.AgentID == ""` 改成 `false`（永远走具名路径）→ `TestDelegationWithoutAgentIDStillClonesTheParent` 必红
2. 在 `RunSubTasks` 里把每条的 `spec.AgentID` 换成第一条的 → `TestBatchDelegationHonoursEachEntrysAgentID` 必红

- [ ] **Step 5: 全量与提交**

```bash
go test ./... -count=1 -p 1 -timeout 900s
```
退出码单独用 `echo "EXIT=$?"` 取，**不要接管道**。Expected: `EXIT=0`，0 个 FAIL。

```bash
go test ./internal/runtime/ -race -count=1
gofmt -l .
go vet ./...
```
Expected: race 绿；后两者无输出。

```bash
git add internal/runtime/delegation_agents_test.go internal/runtime/delegation.go
git commit -m "feat(runtime): batch 每条各自的 agent_id 生效，默认路径回归钉住"
```

---

## 自检（写计划时已跑）

**规格覆盖**：§3.1 `agent_id` 进 schema→Task 2 Step 4；§3.2 解析规则四种情形→Task 2（不存在/nil/不传）+ Task 3（查得到）；§4.1 目标说了算→Task 3 Step 5 + Task 4；§4.2 那条通道的三条要求→只能选已配置（Task 2）、落审计（Task 5）、`DisabledTools` 校验照跑（Task 3 Step 5 复用 `ResolveTaskRunner` 的既有校验）；§4.4 三层叠加→Task 4；§5.1 两个 role→Task 3 变异 2；§5.2 委派上下文→Task 3 Step 3/4 + 变异 1；§5.3 复用而非另起→Task 3 Step 5 明写「抽成共用私有函数」；§6 四条失败模式→Task 2（前两条）+ Task 3 Step 7 变异 3（错误原样传出）+ Task 3（maas 失败同路径）；§7 的 11 条接缝分别落在 Task 1(接线)、2(2,3,4)、3(6,7,10)、4(5,8)、5(9)、6(1,11)。**无遗漏。**

**类型一致性**：`DelegationAgents` 在 Task 1 定义、Task 3 扩展；`DelegationContext` 在 Task 3 定义、Task 4 消费其 `Toolsets`；`childFor` 在 Task 3 定义、Task 5/6 消费；`runChild` 的签名变更只在 Task 3，其两个调用点同任务内改完。

**夹具全部用既有真名，无占位**：`recordingSubMaas` 与 `recorded()`（`internal/runtime/delegation_test.go:17,33`）、`unchangingReadRegistry`（`internal/runtime/multiturn_test.go`）、`resolverCaptureMaas` 与 `agentregistry.New` 构造（`internal/runtime/agent_resolver_test.go:33-41`）、`adapter.NewMemoryAuditLog()`、`mustAuditEvents`（`internal/runtime/runtime_test.go:777`）。
