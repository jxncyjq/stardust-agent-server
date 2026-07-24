---
title: Per-Agent 工具授权（disabled_tools 勾选）
date: 2026-07-24
status: approved
---

# Per-Agent 工具授权设计

## 目标

每个 agent 可在配置里勾选它能调用哪些工具；未勾选的工具该 agent 不可使用（既看不到、也调不到）。元工具（`load_capabilities`/`call_tool`）常驻，不进勾选范围。**只 gate 工具，skills 不在本次改动范围。**

## 背景

现状：`AgentConfig`（`internal/agentregistry/config.go`）没有工具字段；`agent_resolver.go` 给每个 agent 建同一套 `NewFileReadWriteWorkspaceRegistry`，全 agent 工具集相同。唯一的 per-agent 收窄是 Plan 模式（`effectiveTools` → `Subset(SafeToolNames)`）。

需求来自使用中希望按 agent 限定能力面（例：某 agent 只做调研，禁掉 `write_file` 后它就不能创建/写文件）。

## 核心决策（已与用户确认）

1. **元工具常驻**：`load_capabilities`/`call_tool` 永不进勾选列表。当前系统只有这两个元工具（历史 `list_tools` 已删）。
2. **禁用列表编码（deny-list）**：配置存**被禁用**的工具名。缺字段 / `null` / `[]` = 不禁 = 全部可用。将来系统新增工具，现有 agent 自动获得（不在任何禁用列表里）。
3. **强制点唯一**：在 `effectiveTools` 处减掉 disabled_tools，eager schema / lazy 目录 / dispatch 三层自动生效。
4. **未勾 = 硬边界**：不进能力目录 **且** dispatch 权威拒绝（`ErrToolNotFound`），符合 ADR-0002 服务端权威 + fail-loud 铁律。
5. **协议无关**：gating 不依赖 lazy/eager。eager 下 = 决定发哪些原生 schema；lazy 下 = 决定目录里列哪些 + `call_tool` 能调哪些。不强制切换协议。
6. **默认 agent 也纳入**：`agent.json` 的 runtime 段同样支持 `disabled_tools`。
7. **GUI 工具全集口径**：列出可 gate 工具的**全集**（registry Descriptors 减元工具）；禁一个该 agent 本就没有的工具 = 无害 no-op。

## 架构

### 强制点：effectiveTools 一处收窄

`effectiveTools(task)`（`internal/runtime/runtime.go`）是唯一工具集出口，同时喂：
- eager 原生 schema（`inferenceTools`）
- lazy 能力目录（`buildCatalog`）
- dispatch 执行（`Registry.Execute` 找不到即 `ErrToolNotFound`）

在此处再减一层：

```
effectiveTools(task):
  base = r.tools
  if task.Mode == Plan:  base = base.Subset(base.SafeToolNames()...)   // 既有
  if len(disabledTools) > 0:  base = base.Without(disabledTools...)     // 新增
  return base
```

三层强制全部从这一个 registry 派生，无需分别实现「目录里没有」和「dispatch 拒绝」。

**元工具豁免天然成立**：`call_tool`/`load_capabilities` 不在 registry 里（`metaInferenceTools` 单独合成、dispatch `isMetaTool` 特判），`Without` 触不到它们。

### disabled_tools 从哪来

disabled_tools 是 per-agent 配置，需在**构造该 agent 的 Runtime 时**传入，落到 Runtime 上供 `effectiveTools` 读取。两个构造点：

- **Sub-agent**：`agent_resolver.go` 读 `AgentConfig.DisabledTools`，经 `runtime.Config.DisabledTools` 传入。
- **默认 agent**：默认 Runtime 构造点（app.go / command.go serve 装配）读 `config.RuntimeConfig.DisabledTools`。

`runtime.Config` 新增字段 `DisabledTools []string`；`Runtime` 保存它；`effectiveTools` 使用它。

### Registry.Without

```go
// Without returns a new Registry containing every registered tool except the
// named ones. Names not present are ignored (disabling a tool an agent never
// had is a legitimate no-op). It never mutates the receiver, mirroring Subset.
func (r *Registry) Without(names ...string) *Registry
```

实现：`keep = 所有已注册名 − names`，复用现有 `Subset(keep...)`。

## 数据模型

```go
// internal/agentregistry/config.go — AgentConfig
DisabledTools []string `json:"disabled_tools,omitempty"`

// internal/config — RuntimeConfig（agent.json runtime 段）
DisabledTools []string `json:"disabled_tools,omitempty"`
```

契约：
- 缺字段 / `null` / `[]` = 不禁 = 全部工具可用（向后兼容，现有配置升级零变化）。这是**契约声明的可选默认**，非兜底。
- 列表内每个名字**必须是已知可 gate 工具名**。装配期校验：出现未知名 → fail-loud（返回 error 阻止该 agent 装配，附上未知名与合法名集合），防拼写错误静默失效。校验的合法集 = 可 gate 工具全集（见下）。

## 可 gate 工具全集

= 规范工作区 registry 的 Descriptors 减去元工具。当前含（随注册演进）：
`read_file`、`search_content`、`list_files`、`write_file`、task ledger 系列、agent message 系列、`fetch_url`（web）、以及默认 runtime 才有的 `delegate_task`/`session_search`/`moa_consult`（orchestrator-only，worker 不注册）。

GUI 列**全集**。worker 本就没有的工具被列出并可勾掉 = 对该 worker 的 no-op（`Without` 对不存在名无副作用）。装配期校验的合法集也用这个全集，故勾掉 orchestrator-only 工具不会触发 fail-loud。

新增 Go 绑定供 GUI 拉取全集：

```go
// ListGateableTools returns every tool a per-agent config may disable, each with
// its name and one-line description, sorted by name. Meta-tools are excluded.
func (a *App) ListGateableTools() []GateableTool   // {Name, Description}
```

后端提供一个可复用函数（如 `tool.GateableToolNames()` / 构造规范 registry 后取 Descriptors 减元工具），绑定与装配期校验共用同一来源，避免两处清单漂移。

## GUI

`AgentConfigPage`（声明式 `AGENT_SECTIONS` + `FieldRenderer`）新增一段「工具授权」，新 field 类型 `tool-checklist`：

- 挂载时经 `ListGateableTools` 拉全集
- 每工具一行复选框，**默认全勾**；`disabled_tools` 里的名字取消勾
- 存回：收集**未勾**的名字 → 写 `disabled_tools`（deny-list）
- 走既有草稿 store + 「保存并重启」一次落盘并重启 serve

默认 agent：主设置页（非 sub-agent 抽屉）同样出一个 `tool-checklist`，绑定 agent.json runtime 段的 `disabled_tools`。

## 错误处理

- 未知工具名（配置里写了不存在的工具）→ 装配期 fail-loud，返回 error 阻止装配，消息含未知名 + 合法名集合。
- `ListGateableTools` 构造 registry 失败 → 返回 error 给前端（前端显示加载失败，不静默给空列表）。
- `Without` 对不存在名 no-op，是契约声明的合法行为，非兜错。

## 测试

**后端**：
- `Registry.Without`：正确剔除；剔除不存在名 = no-op；传入元工具名无影响；不改原 registry。
- `effectiveTools`：仅 disabled；Plan ∩ disabled 组合；无 disabled 时与原行为一致。
- 端到端：eager 下被禁工具不出现在 `inferenceTools`；lazy 下不出现在能力目录；dispatch 调被禁工具 → `ErrToolNotFound`。
- 装配校验：未知工具名 → fail-loud（断言确实返回 error，含名字）。
- `GateableToolNames`/`ListGateableTools`：含预期工具、排除元工具、排序稳定。

**GUI**（vitest）：
- `tool-checklist` 默认全勾；`disabled_tools` 里的项取消勾。
- 取消勾一项 → 草稿 `disabled_tools` 含该名。
- 往返：加载 disabled → 渲染 → 保存，disabled 集一致。
- `ListGateableTools` 失败 → 显示加载失败，不渲染空列表冒充成功。

## 交付拆分

- **PR 1（server 仓库 jxncyjq/stardust-agent-server）**：`AgentConfig.DisabledTools` + `RuntimeConfig.DisabledTools` + `Registry.Without` + `effectiveTools` 接线 + 两个构造点传参 + 装配校验 + `GateableToolNames`/`ListGateableTools` 绑定 + 全部后端测试。
- **PR 2（GUI 仓库 jxncyjq/stardust-agent-gui）**：`tool-checklist` field + AgentConfigPage/主设置页接线 + vitest。依赖 PR 1 的 `ListGateableTools` 绑定，**PR 1 先合**。

## 非目标（本次不做）

- skills 的 per-agent 授权（工具是工具、skills 是 skills，skills 侧不动）。
- 运行时动态改授权（改配置须经「保存并重启」，与现有配置流一致）。
- 新增元工具（如规划中未做的 `search` 能力搜索）。
- lazy/eager 协议切换（gating 协议无关，不触碰 `lazy_tools` 开关）。
