# Per-Agent 工具授权 · 后端（PR1）实施计划

> **For agentic workers:** 用 superpowers:subagent-driven-development 或 superpowers:executing-plans 逐任务执行。步骤用 `- [ ]` 复选框跟踪。

**Goal:** 给 legionAgent 后端加 per-agent `disabled_tools`（deny-list）：配置里被禁的工具不进 eager schema / lazy 能力目录 / dispatch，元工具永不受影响；并提供一个 canonical「可 gate 工具全集」供 GUI 拉取与配置校验共用。

**Architecture:** 唯一收窄点是 `runtime.effectiveTools`——它已是 Plan 模式 `Subset(SafeToolNames)` 的收窄处，且同时喂原生 schema、能力目录、dispatch。加 `Registry.Without` 在此再减一层，三层强制自动生效。canonical 全集做成一处显式数据 + 漂移守卫测试。

**Tech Stack:** Go 1.26；标准库 testing。

## 事故/需求背景

见 spec `docs/superpowers/specs/2026-07-24-per-agent-tool-authorization-design.md`。要点：每 agent 勾选可用工具，未勾 = 硬边界（看不到 + 调不到）；deny-list 编码（存被禁的，缺字段 = 全可用）；只 gate 工具不动 skills；默认 agent 纳入。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：未知工具名 fail-loud；不得静默兜底。错误用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装。
- 门禁：`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空。
- `-race` 在 Windows 跑不了。用 WSL Ubuntu-22.04：`export PATH=$HOME/sdk/go/bin:$PATH && GOOS=linux GOARCH=amd64 CGO_ENABLED=1 CC=gcc go test -race ...`。
- 公开 API 必须有以标识符名开头的 Go doc 注释。
- deny-list 语义：缺字段 / `nil` / `[]` = 不禁 = 全可用（契约声明的可选默认，非兜底）。
- 元工具 `load_capabilities`/`call_tool` 永不受 gate；它们不在 registry 里，`Without` 天然触不到。

## 可 gate 工具全集（canonical）

跨两包：tool 包注册 `read_file`/`search_content`/`list_files`/`write_file`/task ledger 系列/agent message 系列/`fetch_url`；runtime 包注册 `delegate_task`/`moa_consult`/`session_search`。因此 canonical 集做成**显式数据**（不靠单包 registry 枚举），配一个漂移守卫测试用生产 registry 兜底。

## File Structure

| 文件 | 责任 | 动作 |
|---|---|---|
| `internal/tool/registry.go` | `Registry.Without` | 修改 |
| `internal/toolauth/catalog.go` | canonical 可 gate 全集 `GateableTools()` / `GateableToolNames()` | 创建 |
| `internal/toolauth/catalog_test.go` | canonical 集单测 | 创建 |
| `internal/agentregistry/config.go` | `AgentConfig.DisabledTools` | 修改 |
| `internal/config/config.go` | `RuntimeConfig.DisabledTools` | 修改 |
| `internal/runtime/runtime.go` | `Config.DisabledTools` + `Runtime.disabledTools` + `effectiveTools` 收窄 | 修改 |
| `internal/runtime/agent_resolver.go` | 传 `AgentConfig.DisabledTools` + 装配期校验 | 修改 |
| `internal/app/app.go` | `RunTaskOptions.DisabledTools` → `runtime.Config` | 修改 |
| `internal/cli/command.go` | 各 RunTaskOptions 调用点传 `cfg.Runtime.DisabledTools` | 修改 |
| `internal/app/app_agents.go` | `ListGateableTools` Wails 绑定 | 修改 |
| `internal/runtime/toolauth_drift_test.go` | 漂移守卫：生产 registry 非元工具 ⊆ canonical | 创建 |

---

### Task 1: Registry.Without

**Files:**
- Modify: `internal/tool/registry.go`
- Test: `internal/tool/registry_test.go`

**Interfaces:**
- Produces: `func (r *Registry) Without(names ...string) *Registry`
- Consumes: 既有 `Subset`

- [ ] **Step 1: 写失败测试**

```go
func TestRegistryWithoutRemovesNamedTools(t *testing.T) {
	base := NewFileReadWriteWorkspaceRegistry(t.TempDir(), nil)
	got := base.Without("write_file")

	names := map[string]bool{}
	for _, d := range got.Descriptors() {
		names[d.Name] = true
	}
	if names["write_file"] {
		t.Fatal("Without(write_file) still exposes write_file")
	}
	if !names["read_file"] {
		t.Fatal("Without(write_file) dropped an unrelated tool")
	}
}

func TestRegistryWithoutIgnoresUnknownNames(t *testing.T) {
	base := NewFileReadWriteWorkspaceRegistry(t.TempDir(), nil)
	before := len(base.Descriptors())

	got := base.Without("no_such_tool")

	if len(got.Descriptors()) != before {
		t.Fatalf("Without(unknown) changed the tool set: %d -> %d", before, len(got.Descriptors()))
	}
}

func TestRegistryWithoutDoesNotMutateReceiver(t *testing.T) {
	base := NewFileReadWriteWorkspaceRegistry(t.TempDir(), nil)
	before := len(base.Descriptors())

	base.Without("write_file")

	if len(base.Descriptors()) != before {
		t.Fatal("Without mutated the receiver registry")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

`go test ./internal/tool/ -run TestRegistryWithout -v` → 编译失败（`Without` 未定义）。

- [ ] **Step 3: 最小实现**

```go
// Without returns a new registry exposing every registered tool except the
// named ones. It shares this registry's policy, enforcer, guardrails, audit log
// and sanitizer (like Subset). Names with no matching tool are ignored:
// disabling a tool an agent never had is a legitimate no-op, not an error. It
// never mutates the receiver.
func (r *Registry) Without(names ...string) *Registry {
	remove := make(map[string]bool, len(names))
	for _, name := range names {
		remove[name] = true
	}
	keep := make([]string, 0, len(r.describes))
	for name := range r.describes {
		if !remove[name] {
			keep = append(keep, name)
		}
	}
	return r.Subset(keep...)
}
```

- [ ] **Step 4: 跑测试确认通过**

`go test ./internal/tool/ -run TestRegistryWithout -v`

- [ ] **Step 5: 提交**

```bash
git add internal/tool/registry.go internal/tool/registry_test.go
git commit -m "feat(tool): Registry.Without 剔除指定工具，返回新 registry 不改原"
```

---

### Task 2: canonical 可 gate 全集

**Files:**
- Create: `internal/toolauth/catalog.go`
- Test: `internal/toolauth/catalog_test.go`

**Interfaces:**
- Produces:
  - `type GateableTool struct { Name string; Description string }`
  - `func GateableTools() []GateableTool`（按 Name 排序，含全集，不含元工具）
  - `func GateableToolNames() map[string]bool`
  - `func IsGateable(name string) bool`

- [ ] **Step 1: 写失败测试**

```go
func TestGateableToolsIncludesKnownToolsAndExcludesMeta(t *testing.T) {
	names := toolauth.GateableToolNames()
	for _, want := range []string{
		"read_file", "search_content", "list_files", "write_file",
		"fetch_url", "delegate_task", "moa_consult", "session_search",
	} {
		if !names[want] {
			t.Errorf("GateableToolNames() missing %q", want)
		}
	}
	for _, meta := range []string{"call_tool", "load_capabilities"} {
		if names[meta] {
			t.Errorf("GateableToolNames() must not list meta-tool %q", meta)
		}
	}
}

func TestGateableToolsAreSortedAndDescribed(t *testing.T) {
	tools := toolauth.GateableTools()
	if len(tools) == 0 {
		t.Fatal("GateableTools() is empty")
	}
	for i, tl := range tools {
		if tl.Description == "" {
			t.Errorf("GateableTools()[%d] %q has no description", i, tl.Name)
		}
		if i > 0 && tools[i-1].Name >= tl.Name {
			t.Errorf("GateableTools() not sorted at %d: %q >= %q", i, tools[i-1].Name, tl.Name)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

`go test ./internal/toolauth/ -v`（包不存在 → 编译失败）。

- [ ] **Step 3: 最小实现**

```go
// Package toolauth defines which tools a per-agent config may disable.
package toolauth

import "sort"

// GateableTool is one tool a per-agent config may allow or disable, with a
// one-line description for the config UI.
type GateableTool struct {
	Name        string
	Description string
}

// gateable is the canonical set of tools a per-agent disabled_tools list may
// name. It is explicit data rather than a registry enumeration because the tools
// are registered across two packages (internal/tool and internal/runtime); a
// drift-guard test (internal/runtime) asserts every real production tool appears
// here, so a newly added tool that is not listed fails loudly.
//
// Meta-tools (call_tool, load_capabilities) are deliberately absent: they are
// always resident and never gated.
var gateable = []GateableTool{
	{"append_task_message", "向任务追加一条消息"},
	{"claim_task", "认领一个任务"},
	{"create_task", "创建新任务"},
	{"delegate_task", "把子任务委派给其他 agent（仅编排者）"},
	{"fetch_url", "抓取一个 URL 的内容"},
	{"list_files", "列出目录下的文件"},
	{"moa_consult", "向多个模型发起 MoA 咨询（仅编排者）"},
	{"read_file", "读取一个文件的内容"},
	{"read_messages", "读取 agent 间消息"},
	{"read_task", "读取一个任务的详情"},
	{"rebuild_tasks", "重建任务台账索引"},
	{"search_content", "在文件内容中搜索"},
	{"send_message", "向其他 agent 发送消息"},
	{"session_search", "跨会话检索历史（仅编排者）"},
	{"update_task", "更新一个任务的状态"},
	{"write_file", "写入/创建一个文件"},
}

// GateableTools returns the canonical gateable tools, sorted by name.
func GateableTools() []GateableTool {
	out := make([]GateableTool, len(gateable))
	copy(out, gateable)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GateableToolNames returns the gateable tool names as a set, for validating a
// disabled_tools list.
func GateableToolNames() map[string]bool {
	names := make(map[string]bool, len(gateable))
	for _, t := range gateable {
		names[t.Name] = true
	}
	return names
}

// IsGateable reports whether name is a tool a config may disable.
func IsGateable(name string) bool {
	return GateableToolNames()[name]
}
```

- [ ] **Step 4: 跑测试确认通过**

`go test ./internal/toolauth/ -v`

- [ ] **Step 5: 提交**

```bash
git add internal/toolauth/
git commit -m "feat(toolauth): canonical 可 gate 工具全集（排除元工具）"
```

---

### Task 3: 配置字段

**Files:**
- Modify: `internal/agentregistry/config.go`
- Modify: `internal/config/config.go`（`RuntimeConfig`）
- Test: `internal/agentregistry/registry_test.go`

**Interfaces:**
- Produces: `AgentConfig.DisabledTools []string`；`config.RuntimeConfig.DisabledTools []string`

- [ ] **Step 1: 写失败测试**

```go
func TestAgentConfigCarriesDisabledTools(t *testing.T) {
	raw := []byte(`{"id":"a1","role":"researcher","disabled_tools":["write_file"]}`)
	var cfg agentregistry.AgentConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal error = %v, want nil", err)
	}
	if len(cfg.DisabledTools) != 1 || cfg.DisabledTools[0] != "write_file" {
		t.Fatalf("DisabledTools = %#v, want [write_file]", cfg.DisabledTools)
	}
}

func TestAgentConfigOmitsDisabledToolsWhenAbsent(t *testing.T) {
	var cfg agentregistry.AgentConfig
	if err := json.Unmarshal([]byte(`{"id":"a1","role":"r"}`), &cfg); err != nil {
		t.Fatalf("Unmarshal error = %v, want nil", err)
	}
	if cfg.DisabledTools != nil {
		t.Fatalf("DisabledTools = %#v, want nil when absent", cfg.DisabledTools)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

`go test ./internal/agentregistry/ -run TestAgentConfigCarriesDisabledTools -v`

- [ ] **Step 3: 最小实现**

`AgentConfig` 加：

```go
	// DisabledTools names the tools this agent may not use (deny-list). Absent /
	// null / empty means no tool is disabled — every tool is available. Each name
	// must be a known gateable tool (validated at agent assembly); meta-tools are
	// never listed here and cannot be disabled.
	DisabledTools []string `json:"disabled_tools,omitempty"`
```

`config.RuntimeConfig` 加同样字段与文档注释（默认 agent 用）。

- [ ] **Step 4: 跑测试确认通过**

`go test ./internal/agentregistry/ ./internal/config/ -v`

- [ ] **Step 5: 提交**

```bash
git add internal/agentregistry/config.go internal/config/config.go internal/agentregistry/registry_test.go
git commit -m "feat(config): AgentConfig 与 RuntimeConfig 增 disabled_tools（deny-list）"
```

---

### Task 4: runtime effectiveTools 收窄

**Files:**
- Modify: `internal/runtime/runtime.go`（`Config`、`Runtime`、`NewRuntime`、`effectiveTools`）
- Test: `internal/runtime/runtime_test.go`（或新 `toolauth_test.go`）

**Interfaces:**
- Consumes: Task 1 `Registry.Without`
- Produces: `runtime.Config.DisabledTools []string`；`effectiveTools` 在 Plan 收窄后再 `Without(disabledTools...)`

- [ ] **Step 1: 写失败测试**

```go
// A disabled tool must not appear in the offered native schema (eager) — the
// single effectiveTools choke point covers offer, catalog and dispatch at once.
func TestEffectiveToolsRemovesDisabledTool(t *testing.T) {
	maas := &recordingRoundsMaas{responses: []port.InferenceResponse{{Text: "done"}}}
	rt := NewRuntime(Config{
		Maas:          maas,
		Tools:         unchangingReadRegistry(t), // has read_file
		DisabledTools: []string{"read_file"},
	})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "go"}); err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}
	for _, tl := range maas.requests[0].Tools {
		if tl.Name == "read_file" {
			t.Fatal("disabled read_file was still offered to the model")
		}
	}
}

// Dispatch is authoritative: even if the model names a disabled tool, executing
// it must fail loud rather than run.
func TestDispatchRejectsDisabledTool(t *testing.T) {
	maas := &recordingRoundsMaas{responses: []port.InferenceResponse{
		{ToolCalls: []domain.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": "x"}}}},
		{Text: "done"},
	}}
	rt := NewRuntime(Config{
		Maas:          maas,
		Tools:         unchangingReadRegistry(t),
		DisabledTools: []string{"read_file"},
	})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "go"}); err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}
	// The tool round's result must be a failure carrying ErrToolNotFound, fed
	// back to the model as a tool turn (not executed).
	var sawRejection bool
	for _, req := range maas.requests {
		for _, msg := range req.Messages {
			if msg.Role == port.RoleTool && strings.Contains(msg.Content, "not found") {
				sawRejection = true
			}
		}
	}
	if !sawRejection {
		t.Fatal("dispatching a disabled tool did not surface a not-found rejection")
	}
}

func TestEffectiveToolsUnaffectedWhenNoDisabled(t *testing.T) {
	maas := &recordingRoundsMaas{responses: []port.InferenceResponse{{Text: "done"}}}
	rt := NewRuntime(Config{Maas: maas, Tools: unchangingReadRegistry(t)})
	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "go"}); err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}
	var sawRead bool
	for _, tl := range maas.requests[0].Tools {
		if tl.Name == "read_file" {
			sawRead = true
		}
	}
	if !sawRead {
		t.Fatal("read_file missing with no disabled_tools set")
	}
}
```

（`recordingRoundsMaas`/`unchangingReadRegistry` 已存在于 `internal/runtime/multiturn_test.go`。）

- [ ] **Step 2: 跑测试确认失败**

`go test ./internal/runtime/ -run 'TestEffectiveToolsRemovesDisabledTool|TestDispatchRejectsDisabledTool' -v`

- [ ] **Step 3: 最小实现**

`Config` 加：

```go
	// DisabledTools names tools this runtime's agent may not use (deny-list).
	// effectiveTools removes them from the registry that drives the offered
	// schema, the lazy capability catalog and dispatch at once. Meta-tools are
	// never in the registry, so they are unaffected. Empty disables nothing.
	DisabledTools []string
```

`Runtime` 加字段 `disabledTools []string`；`NewRuntime` 赋值 `disabledTools: cfg.DisabledTools`。

`effectiveTools`：

```go
func (r *Runtime) effectiveTools(task domain.Task) *tool.Registry {
	tools := r.tools
	if tools != nil && task.Mode == domain.ModePlan {
		tools = tools.Subset(tools.SafeToolNames()...)
	}
	if tools != nil && len(r.disabledTools) > 0 {
		tools = tools.Without(r.disabledTools...)
	}
	return tools
}
```

- [ ] **Step 4: 跑测试确认通过**

`go test ./internal/runtime/ -v`

- [ ] **Step 5: 提交**

```bash
git add internal/runtime/runtime.go internal/runtime/*_test.go
git commit -m "feat(runtime): effectiveTools 按 disabled_tools 收窄，三层强制一处生效"
```

---

### Task 5: 接线两个构造点 + 装配期校验

**Files:**
- Modify: `internal/runtime/agent_resolver.go`（sub-agent：传 DisabledTools + 校验）
- Modify: `internal/runtime/delegation.go`（`newSubRuntime` 拷 disabledTools —— Task 4 review 发现的跨任务缺口）
- Modify: `internal/app/app.go`（`RunTaskOptions.DisabledTools` → `runtime.Config`）
- Modify: `internal/cli/command.go`（各默认 RunTaskOptions 调用点传 `cfg.Runtime.DisabledTools`）
- Test: `internal/runtime/agent_resolver_test.go`、`internal/runtime/delegation_test.go`

**Interfaces:**
- Consumes: Task 2 `toolauth.GateableToolNames`、Task 3 配置字段、Task 4 `runtime.Config.DisabledTools`
- Produces: `app.RunTaskOptions.DisabledTools []string`；未知工具名 → fail-loud

**关键缺口（Task 4 review）：** `newSubRuntime`（delegation.go:71）手工构造子 Runtime 结构体、拷了 `tools`/`lazyTools` 等但**没拷 `disabledTools`** → 一个禁了 `write_file` 的 agent 委派子任务后，子 runtime 的 `disabledTools` 为 nil，被禁工具复活。安全属性端到端不完整。必须在 child 结构体字面量加 `disabledTools: r.disabledTools,`。**效率注意（Task 2 review Minor）：** 循环校验 disabled_tools 时把 `toolauth.GateableToolNames()` 提到循环外调一次，别每项都重建 map。

- [ ] **Step 1: 写失败测试**

```go
// A disabled_tools entry that is not a known gateable tool is a config error
// (a typo silently disabling nothing is exactly what fail-loud forbids).
func TestResolverRejectsUnknownDisabledTool(t *testing.T) {
	// ... build resolver with an AgentConfig whose DisabledTools = ["writ_file"]
	_, _, _, err := resolver.Resolve(ctx, taskForAgent("a1"))
	if err == nil {
		t.Fatal("resolver accepted an unknown disabled tool name")
	}
	if !strings.Contains(err.Error(), "writ_file") {
		t.Fatalf("error = %v, want it to name the unknown tool", err)
	}
}

// A valid disabled_tools reaches the runtime and takes effect.
func TestResolverAppliesDisabledTools(t *testing.T) {
	// ... AgentConfig DisabledTools = ["write_file"], run a task, assert the
	// captured request's Tools omit write_file.
}
```

```go
// Task 4 review gap: a disabled tool must stay disabled after delegation. The
// child runtime is built by hand (newSubRuntime), so it must carry the parent's
// deny-list or a delegating agent silently regains the tool it was denied.
func TestSubRuntimeInheritsDisabledTools(t *testing.T) {
	parent := NewRuntime(Config{
		Maas:          &recordingRoundsMaas{responses: []port.InferenceResponse{{Text: "done"}}},
		Tools:         unchangingReadRegistry(t), // has read_file
		MaxSpawnDepth: 2,
		DisabledTools: []string{"read_file"},
	})
	child, err := parent.newSubRuntime("leaf", nil) // no toolsets → inherits parent's full registry
	if err != nil {
		t.Fatalf("newSubRuntime error = %v, want nil", err)
	}
	if _, err := child.RunTask(context.Background(), domain.Agent{ID: "c"}, domain.Task{ID: "t1", Input: "go"}); err != nil {
		t.Fatalf("child RunTask error = %v, want nil", err)
	}
	req := child.maas.(*recordingRoundsMaas).requests[0]
	for _, tl := range req.Tools {
		if tl.Name == "read_file" {
			t.Fatal("delegated child still offers the parent's disabled read_file")
		}
	}
}
```

（`agent_resolver` 的两个用例按 `agent_resolver_test.go` 现有 resolver 构造与 `resolverCaptureMaas` 写；未知名断言是判别性用例。`newSubRuntime` 是包内私有，委派测试须放 `package runtime`（`delegation_test.go` 已是）。）

- [ ] **Step 2: 跑测试确认失败**

`go test ./internal/runtime/ -run 'TestResolverRejectsUnknownDisabledTool|TestResolverAppliesDisabledTools' -v`

- [ ] **Step 3: 最小实现**

`agent_resolver.go` 在建 runtime 前校验并传参：

```go
	for _, name := range agentCfg.DisabledTools {
		if !toolauth.IsGateable(name) {
			return domain.Agent{}, nil, false, fmt.Errorf(
				"agent %q disabled_tools names unknown tool %q (gateable: %v)",
				agentCfg.ID, name, sortedKeys(toolauth.GateableToolNames()))
		}
	}
	runner := NewRuntime(Config{
		// ... 既有字段
		DisabledTools: agentCfg.DisabledTools,
	})
```

`app.go`：`RunTaskOptions` 加 `DisabledTools []string`；构造 `runtime.Config` 处传 `DisabledTools: opts.DisabledTools`。默认 agent 同样在此校验（未知名 → 返回 error）：

```go
	for _, name := range opts.DisabledTools {
		if !toolauth.IsGateable(name) {
			return domain.TaskRun{}, fmt.Errorf("disabled_tools names unknown tool %q", name)
		}
	}
```

`command.go` 各默认 RunTaskOptions 调用点（run/TUI/mentioned-TUI/defaultTaskRunner，约 line 134/533/620/1902）加 `DisabledTools: cfg.Runtime.DisabledTools`（TUI 路径用对应的 `cfg.Config.Runtime.DisabledTools` / `runtimeSettings.DisabledTools`，按该点既有 MaxToolRounds 的取值来源对齐）。

`delegation.go` 的 `newSubRuntime` child 结构体字面量加一行（在 `tools: tools,` 附近）：

```go
		// The deny-list must survive delegation: a child built by hand here
		// would otherwise regain a tool its parent was denied. Carry it over
		// explicitly, like tools/logger above.
		disabledTools: r.disabledTools,
```

- [ ] **Step 4: 跑测试确认通过**

`go test ./internal/runtime/ ./internal/app/ ./internal/cli/ -v`

- [ ] **Step 5: 提交**

```bash
git add internal/runtime/agent_resolver.go internal/app/app.go internal/cli/command.go internal/runtime/agent_resolver_test.go
git commit -m "feat(runtime): 接线 disabled_tools 两个构造点 + 未知工具名装配期 fail-loud"
```

---

### Task 6: 漂移守卫测试

**Files:**
- Create: `internal/runtime/toolauth_drift_test.go`

**Interfaces:**
- Consumes: Task 2 `toolauth.GateableToolNames`

- [ ] **Step 1: 写测试（本任务只加保护网，允许一次即绿）**

```go
// A production runtime registry (default runtime path) must contain no non-meta
// tool that toolauth cannot gate: a newly added tool that nobody listed in the
// gateable catalog would silently be un-disableable, which is exactly the
// drift this guard forbids. Adding a tool now requires adding it to
// toolauth.gateable, or this fails.
func TestEveryProductionToolIsGateable(t *testing.T) {
	registry := productionToolRegistryForTest(t) // 复用 resolver/app 构造出的全量 registry
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
```

`productionToolRegistryForTest` 按 `agent_resolver.go` 的构造序列建：`NewFileReadWriteWorkspaceRegistry` + `RegisterTaskLedgerTools` + `RegisterAgentMessageTools` + `RegisterWebTools` + runtime 侧 `delegate_task`/`moa_consult`/`session_search` 的注册（照 agent_resolver 实际注册序列复制；这些工具的注册点以代码现状为准）。

- [ ] **Step 2: 跑测试**

`go test ./internal/runtime/ -run TestEveryProductionToolIsGateable -v` → 应绿（Task 2 已把全集列全）。若红，说明漏列，补进 `toolauth.gateable`。

- [ ] **Step 3: 提交**

```bash
git add internal/runtime/toolauth_drift_test.go
git commit -m "test(runtime): 漂移守卫——生产工具全部可 gate，新增漏登记即红"
```

---

### Task 7: ListGateableTools Wails 绑定

**Files:**
- Modify: `internal/app/app_agents.go`
- Test: `internal/app/app_test.go`

**Interfaces:**
- Consumes: Task 2 `toolauth.GateableTools`
- Produces: `func (a *App) ListGateableTools() []GateableToolDTO`（`{Name, Description}`），供 GUI PR2 拉取

- [ ] **Step 1: 写失败测试**

```go
func TestListGateableToolsReturnsSortedNamedTools(t *testing.T) {
	got := app.New().ListGateableTools()
	if len(got) == 0 {
		t.Fatal("ListGateableTools() returned nothing")
	}
	var sawWrite bool
	for i, tl := range got {
		if tl.Name == "" || tl.Description == "" {
			t.Errorf("ListGateableTools()[%d] missing name/description: %#v", i, tl)
		}
		if tl.Name == "write_file" {
			sawWrite = true
		}
		if tl.Name == "call_tool" || tl.Name == "load_capabilities" {
			t.Errorf("ListGateableTools() leaked meta-tool %q", tl.Name)
		}
	}
	if !sawWrite {
		t.Error("ListGateableTools() missing write_file")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

`go test ./internal/app/ -run TestListGateableTools -v`

- [ ] **Step 3: 最小实现**

```go
// GateableToolDTO is one tool the per-agent config UI can allow or disable.
type GateableToolDTO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListGateableTools returns every tool a per-agent config may disable, each with
// a one-line description, sorted by name. It is the source the tool-authorization
// checklist in the config UI renders. Meta-tools are excluded — they are always
// resident and cannot be disabled.
func (a *App) ListGateableTools() []GateableToolDTO {
	tools := toolauth.GateableTools()
	out := make([]GateableToolDTO, 0, len(tools))
	for _, t := range tools {
		out = append(out, GateableToolDTO{Name: t.Name, Description: t.Description})
	}
	return out
}
```

- [ ] **Step 4: 跑测试确认通过**

`go test ./internal/app/ -run TestListGateableTools -v`

- [ ] **Step 5: 全量门禁 + WSL race + 提交并开 PR**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l .
```

WSL race：

```bash
wsl -d Ubuntu-22.04 -- bash -lc 'cd /mnt/f/source/stardust/Legion/legion/legionAgent && export PATH=$HOME/sdk/go/bin:$PATH && GOOS=linux GOARCH=amd64 CGO_ENABLED=1 CC=gcc go test -race ./internal/runtime/ ./internal/tool/ ./internal/toolauth/ ./internal/app/'
```

```bash
git add internal/app/app_agents.go internal/app/app_test.go
git commit -m "feat(app): ListGateableTools 绑定，供 GUI 工具授权 checklist 拉取"
```

开 PR（base master），标题「feat: per-agent 工具授权后端（disabled_tools deny-list）」，正文引用 spec + 说明 GUI PR2 依赖 ListGateableTools。

---

## 人工/后续

- **PR2（GUI）** 另出计划：`tool-checklist` field 类型 + AgentConfigPage/主设置页接线 + vitest。依赖本 PR 的 `ListGateableTools`，本 PR 先合。
- 真机验证（本 PR 合 + GUI PR 合 + `run.bat build` 后）：给某 agent 勾掉 `write_file`，发写文件任务，确认 Audit 面板无 `write_file`，模型收到 not-found 反馈或改用其他方式。

## 已知取舍

- canonical 全集是显式数据，新增工具须同时登记进 `toolauth.gateable`，靠 Task 6 漂移守卫兜底（红了就补）。
- 默认 agent 与 sub-agent 校验分处两个构造点（app.go 与 agent_resolver.go），共用 `toolauth.IsGateable` 单一判据。
- 禁用某 agent 本就没有的工具（如给 worker 禁 `delegate_task`）= `Without` no-op，合法无害。
