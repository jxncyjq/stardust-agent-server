# 插件观察点 实施计划（G4a）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 插件第一次能够**参与**工具执行链——工具调用完成后收到 `(call, result)`，只读、改不了任何东西；同时把「扩展点」这一整套授权面立起来，供后面的决策点与提示词段复用。

**Architecture:** 四段。①`plugin.json` 声明 `extensions`、`plugins.json` 的 `grant.extensions` 逐项授权（**声明≠拿到**，与能力同规格但语义是子集）；②`tool.Registry` 增加 `Observer` 接缝，注册返回撤销句柄并入 ledger；③ABI 新增 op 2（`OpObserveToolResult`），宿主→guest 单向通知；④只有**被授权**的插件才注册观察者——未授权的扩展点在宿主侧根本不注册，不是运行时判断。

**Tech Stack:** Go 1.26.0 + wazero；SDK（Rust/Go）同期扩。不引依赖。

**上游依据:** spec `specs/2026-08-29-plugin-extension-points-design.md`（三个决策已拍板：做 ask、fail-closed、进稳定前缀）；本期是分期表里的 **G4a**。

## 前置事实（已在 master）

```go
// internal/tool/registry.go
func (r *Registry) Execute(ctx, agent, call) (domain.ToolResult, error)   // :277
//   … Guardrails.After → sanitizer → appendAudit("tool_executed") → return result, nil
func (r *Registry) RegisterDescriptor(d Descriptor, h Handler) func()      // :100，返回撤销
// internal/tool/owned.go
func RegisterOwned(ledger, owner, r, descriptor, handler) func() error     // :14，撤销入 ledger

// internal/plugin/manifest
type PluginManifest struct{ …; Capabilities []string; Tools []ToolDecl; Requires []string; ConfigSchema json.RawMessage }
type GrantDecl struct{ Capabilities []string; AllowedHosts []string; AllowedPaths []string }
func AssembleSpec(pm, entry, deployLimits) (host.Spec, error)              // 声明 ∩ 授权

// internal/plugin/consent
func ResolveCapabilities(actor string, granted []string, pm) ([]string, error)  // 要求 EXACT 集合

// internal/plugin/abi
const OpManifest int32 = 0; OpCallTool int32 = 1

// internal/plugin/host
type Spec struct{ Name; Wasm; Tools; Registry; MaxInstances; Grant perm.Grant; Deps; MemoryPages }
func contributeTools(ledger, owner, spec, guest, keep)                     // 贡献工具 + gateable + ledger
```

## 关键设计

### 扩展点授权是 grant 的新维度，语义是**子集**不是全等

能力（`capabilities`）要求 grant 与声明**完全相等**，因为部分授权写出的 entry 根本加载不了。扩展点不同：「允许你观察，但不允许你决策」是一句**有意义**的话，所以 `grant.extensions ⊆ plugin.json 的 extensions`，空集合法（谁都不给）。

这条差异必须在代码注释与手册里写明，否则下一个人会「顺手统一」成全等，把这句有意义的话删掉。

### 观察是同步的、有界的

- **同步**：Execute 调用观察者并等它返回。异步会把「撤销一个正在运行的观察者」变成又一个生命周期问题（`plugin/unload_leaked` 那一类），而观察点的价值不值这个复杂度。
- **有界**：每次观察有自己的超时 `min(descriptor.Timeout/4, 200ms)`。它花的是这次工具调用的预算，所以必须小且写明。
- **失败只属于插件自己**：观察者返回的任何东西被丢弃，出错只计入该插件的健康度（G1 的 `OnFault`），**绝不改变这次调用的结果**。

### 什么时候通知

**只在调用产生了结果之后**（含 `success:false` 的失败结果），即 `appendAudit("tool_executed")` 之后、`return result` 之前。

不通知的三种情况，各有理由：**越权/策略拒绝**（调用从未发生，通知一个没发生的事会让观察者以为它发生了）、**护栏拒绝**、**handler 返回 Go error**（那是宿主侧的故障，不是工具的答案）。这条边界要有测试，否则「观察点看得见什么」会随重构漂移。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）。
- 公开标识符必须有 Go doc 注释。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空。
- **每个 task 至少跑一次 `go test ./...`**；涉及并发的额外 `go test -race ./internal/...`。
- 每个 task 做变异验证：把核心机制改坏，确认测试确实 FAIL，输出留在报告里，然后还原并 `git status` 核对。
- **向后兼容**：没有 `extensions` 的插件（今天全部）行为一字不变，有测试钉住。
- 提交只 stage 本 task 的文件（显式路径），永不 `git add -A`。

---

### Task 1: 清单与授权面（`extensions`）

**Files:**
- Modify: `internal/plugin/manifest/manifest.go`（`PluginManifest.Extensions`、`GrantDecl.Extensions`、校验）
- Create: `internal/plugin/perm/extensions.go`（`perm.Extensions` 与解析）
- Modify: `internal/plugin/manifest/assemble.go`（`AssembleSpec` 求交集）
- Test: `internal/plugin/manifest/extensions_test.go`

**Interfaces:**
- Produces：
  - `PluginManifest.Extensions []string`（`json:"extensions"`）
  - `GrantDecl.Extensions []string`（`json:"extensions"`）
  - `type perm.Extensions struct{ Observe bool }`（后面几期再加 Decide/Prompt）
  - `func perm.ParseExtensions(names []string) (Extensions, error)`
  - `host.Spec.Extensions perm.Extensions`

**校验规则**（各一条测试）：

| 规则 | 理由 |
|---|---|
| 只认已实现的扩展点名（本期：`observe`） | 与能力同一立场：不认识的名字**拒绝**而不是忽略，否则作者以为挂上了 |
| `grant.extensions` 必须是声明的**子集** | 「允许观察不允许决策」是有意义的一句话 |
| 授权了未声明的扩展点 → 拒绝 | 与能力一致：给插件它没要过的东西是配置错误 |
| 重复名字 → 拒绝 | 歧义不猜 |
| 都不写 → 与今天一字不变 | 向后兼容 |

- [ ] Step 1-6：红 → 实现 → 绿 → 全量 → 变异（把子集校验去掉，确认「授权了未声明的扩展点」那条 FAIL）→ 提交

```bash
git commit -m "feat(plugin): plugin.json/plugins.json 增加 extensions 授权维度"
```

---

### Task 2: `tool.Registry` 的观察者接缝

**Files:**
- Create: `internal/tool/observer.go`
- Modify: `internal/tool/registry.go`（Execute 末尾通知）
- Modify: `internal/tool/owned.go`（`ObserveOwned`）
- Test: `internal/tool/observer_test.go`

**Interfaces:**
- Produces：
  - `type Observer interface{ Observe(ctx context.Context, call domain.ToolCall, result domain.ToolResult) }`
  - `func (r *Registry) AddObserver(label string, o Observer) func()`
  - `func ObserveOwned(ledger *lifecycle.Ledger, owner lifecycle.Owner, r *Registry, label string, o Observer) func() error`

要点：注册表持有观察者列表并加锁；通知在 `appendAudit("tool_executed")` 之后；**通知期间不持锁**（一个慢观察者不该挡住注册/撤销）——先在锁内拷一份切片再逐个调用。

- [ ] Step 1-6：红 → 实现 → 绿 → 全量 + `-race` → 变异（把「拷贝后再调用」改成持锁调用，用一个在 Observe 里调 AddObserver 的测试确认死锁/FAIL）→ 提交

```bash
git commit -m "feat(tool): 注册表增加只读观察者接缝"
```

---

### Task 3: ABI op 2 与 guest 侧通知

**Files:**
- Modify: `internal/plugin/abi/abi.go`（`OpObserveToolResult int32 = 2`）
- Modify: `internal/plugin/host/contribute.go`（观察者实现：编码 → `guest.call(op2)` → 丢弃返回值）
- Modify: `internal/plugin/host/activate.go`（授权了 observe 才注册，撤销入 ledger）
- Test: `internal/plugin/host/observe_test.go`

**Interfaces:**
- Produces：`guestToolObservation{call_id, tool, arguments, success, output, error}`（宿主→guest 的 JSON）

要点：

- **未授权就不注册**：`spec.Extensions.Observe == false` 时连观察者都不建，宿主侧没有任何东西会去调 guest；
- 每次通知带自己的超时（见「关键设计」），超时/trap/ABI 违规都走 `ClassifyCallFault` → `OnFault`（G1）；
- 返回值**丢弃**，且这条要有测试（guest 返回一个"改过的结果"，断言宿主返回的仍是原结果）。

- [ ] Step 1-6：红 → 实现 → 绿 → 全量 + `-race` → 变异（把「未授权不注册」改成总是注册，确认对应测试 FAIL）→ 提交

```bash
git commit -m "feat(plugin): ABI op 2，宿主把工具结果只读通知给已授权的插件"
```

---

### Task 4: CLI 与 consent 的 extensions 授权

**Files:**
- Modify: `internal/plugin/consent/consent.go`（`ResolveExtensions`，子集语义）
- Modify: `internal/cli/plugins_command.go`（`grant --extensions`；`install --grant` 的处理）
- Modify: `internal/server/plugins.go`（`PluginView` 增加 declared/granted extensions）
- Test: 各自包内

要点：`ResolveCapabilities` 是**全等**，`ResolveExtensions` 是**子集**——两者放在同一个文件里，注释必须写清为什么不同，否则下一个人会统一它们。

- [ ] Step 1-6：红 → 实现 → 绿 → 全量 → 变异 → 提交

```bash
git commit -m "feat(plugin): grant --extensions 与 /v1/plugins 的扩展点字段"
```

---

### Task 5: SDK 与文档

**Files:**
- Modify: `sdk/rust/legion-plugin/src/lib.rs`（`declare_plugin!` 支持 `observe = fn`）、`README.md`
- Modify: `pkg/legionplugin/plugin.go`（`Observe(handler)`）、`README.md`
- Modify: `plugin_example/`（可选：加一个观察点示例）
- Modify: docs 仓手册（§4 清单字段、§6.2 grant、§7 授权、§9 排错）+ 路线表 G4a

**没有 SDK 支持的扩展点等于没有**：作者要能一行注册观察者，否则这一期只是给宿主加了个没人用的接口。

- [ ] Step 1-4：SDK 两侧 + 示例 + 文档，各自的测试同步

---

## 自检

**范围覆盖**：spec §3.1（观察点）与 §4（授权模型）由 Task 1-4 覆盖；§8 验收里属于本期的三条——「未授权则根本不会被调用」（Task 3）、「返回值被丢弃、失败不影响结果」（Task 3）、「卸载即撤回」（Task 2 的 ledger + Task 3 的注册）——各有测试。

**类型一致性**：`perm.Extensions` 在 Task 1 定义、Task 3 消费；`tool.Observer` 在 Task 2 定义、Task 3 实现；`OpObserveToolResult` 只在 Task 3 出现。

**刻意不做**：决策点（G4b/G4c）、提示词段（G4d）、`execute` around 包装（spec §5 永久不做）。
