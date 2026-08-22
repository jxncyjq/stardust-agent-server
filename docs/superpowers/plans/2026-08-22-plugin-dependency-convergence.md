# A4b 插件三态依赖收敛 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让依赖不满足的插件从「活着但每次调用都失败」变成**挂起**——贡献物撤出目录、实例与状态保留；依赖恢复即重新贡献，并逐级级联。

**Architecture:** 插件在 `plugin.json` 里用 `requires:` 声明它要调的外部工具名。`host` 把一次激活拆成两个 owner——实例侧（runtime/pool）与贡献侧（工具注册 + gateable）——于是「撤销贡献但保留实例」成为 dispose 一个 owner 的事，`(*Plugin).Suspend`/`Resume` 就是它的门面。`Loader.Apply` 在收敛时算一遍依赖图：不满足的挂起、重新满足的恢复、依赖挂起者的逐级挂起。

**Tech Stack:** Go 1.26.0，module `github.com/stardust/legion-agent`。**不新增任何第三方依赖**。

**不含**（各自独立 plan）：运行期健康度驱动的自动失败与卸载（§6.9 的故障计数）、文件 watcher、签名与远程分发、GUI 同意流、策略钩子与 prompt 段、`plugin/unload_leaked` 的在途泄漏诊断。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：禁止 fallback、禁止静默吞错、禁止零值假装正常；错误用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装；不变量违反用 `panic`。
- 公开 API 必须有 Go doc 风格注释，以标识符名开头，且不得与代码矛盾。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿，`gofmt -l .` 输出为空。
- 涉及并发的任务额外跑 `go test -race`，**plugin / runtime / cli 包串行跑**（`-p 1`）。
- 错误路径必须有测试断言「确实返回 error」，不得只测 happy path。
- 每个 task 完成后做一次**变异验证**：把该 task 的核心机制改坏，确认测试确实 FAIL，把失败输出留在报告里，然后还原。
- 清单一律 JSON（A4a 已定，设计文档已回写）。
- 设计依据：docs 仓 `docs/design/architecture/legion-plugin-system.md` §5.5（三态收敛）、§5.3（不变量 I1/I2）、§8（事件）。

## ⚠️ Fork-bomb 安全规程（本仓已因此宕机两次）

前一期一个测试把被测功能当唯一终止条件，`host.test.exe` 吃到 **170 GB 虚存**（Windows 事件 2004 + Kernel-Power 41）。本期新增**级联传播**——一个天然会写成「传播到不动点为止」的循环——规矩因此更严：

1. 级联传播必须**由图的规模封顶**（不超过插件数量的迭代轮数），而不是「传播到没有变化为止」。依赖成环时必须**报错并点名环上的插件**，绝不自旋。
2. 任何测试的循环边界必须独立于被测功能：轮数写成字面量（≤5），每轮断言实例数上限。
3. 每次 `go test` 带 `-timeout 120s`；plugin / runtime / cli 包用 `-p 1`；递归或级联类测试绝不 `-count>1`。
4. 跑挂或内存上涨立刻杀掉、先加边界再重试。

## 前置事实（已在 master `f53715b`，直接用）

```go
// internal/plugin/host —— 一次激活当前把全部条目挂在同一个 owner 上
func Activate(ctx context.Context, ledger *lifecycle.Ledger, owner lifecycle.Owner, spec Spec) (*Plugin, error)
type Plugin struct { Name string; Manifest Manifest /* pool 未导出 */ }
const ledgerLabelRuntime = "wasm-runtime"
const ledgerLabelPool    = "wasm-instance-pool"
func gateableLabel(toolName string) string  // "gateable:" + name
// contributeTools(ledger, owner, spec, guest, keep) —— 未导出，Activate 内部调用；
// 每个工具登记两条：tool.RegisterOwned(...) 与 ledger.Add(owner, gateableLabel(name), undo)

// internal/plugin/loader
func (l *Loader) Apply(ctx context.Context, dep manifest.Deployment, root string) error
func (l *Loader) Status() []InstanceStatus
type InstanceStatus struct{ Name, Version, State string; Tools []string; LastError string }
// State 目前只有 StateLoaded / StateFailed
type instance struct{ name, version string; owner lifecycle.Owner; spec host.Spec; fingerprint, sha256 string; tools []string; lastError string }
func ownerFor(name, version string) lifecycle.Owner   // "plugin:<name>@<version>"

// internal/lifecycle
func (l *Ledger) Add(owner Owner, label string, dispose func() error) func() error
func (l *Ledger) DisposeOwner(owner Owner) error
func (l *Ledger) Snapshot() map[Owner][]string

// internal/tool
func (r *Registry) Descriptors() []Descriptor
```

**A4a 定下、本期必须守的合同：**
- `Activate` 拒绝非空 owner，owner 专属于单次激活。
- `Apply` 的收敛动作整体在一次 `ApplyAtBoundary` 里，中间态不得被任何任务看见。
- `restore` 与 `activate` 一样必须做工具名预检——`RegisterDescriptor`/`toolauth.Contribute` 对重名是 **panic**，operator 数据冲突必须是 error。
- 不变量 I1：`Registry` 的派生视图不得拷贝 handler。不变量 I2：host 侧不得缓存插件返回的对象/闭包。

## 本期的三条裁决（已定，实施时不再讨论）

1. **挂起机制**：`host` 暴露 `Suspend`/`Resume`，内部把一次激活拆成两个 owner。保留 guest 内存状态与实例化开销，正是设计文档给 Suspended 的存在理由。
2. **依赖判定**：`requires:` 里的名字**在收敛时于注册表可解析即满足**。内建工具也算——`requires: ["read_file"]` 永远满足。判定只看「能不能调到」，不看「谁提供的」。
3. **评估时机**：只在 `Apply` 期间。运行期健康度驱动的自动失败留给后续 plan。

---

### Task 1: `requires:` 声明

**Files:**
- Modify: `internal/plugin/manifest/manifest.go`（`PluginManifest` 增字段 + 校验）
- Test: `internal/plugin/manifest/manifest_test.go`

**Interfaces:**
- Produces: `PluginManifest.Requires []string`（JSON 键 `requires`）

**语义**（写进字段 doc）：插件声明它**经 `call_tool` 调用的外部工具名**。缺省（键不存在）= 无外部依赖，这是契约显式声明的可选。它不是能力声明——能力走 `capabilities`，两者互不替代：`requires` 不满足是可挂起的暂时状态，`capabilities` 不获授权是拒绝加载。

- [ ] **Step 1: 写失败测试**

覆盖（每条断言**确实返回 error** 且错误点名出错的值）：
- `requires` 含空串 → 报错
- `requires` 含重复名字 → 报错（与 `tools` 的重名检查同源）
- `requires` 列了本插件自己 `tools` 里的名字 → 报错。自依赖是配置错误：它永远「满足」（自己贡献的名字自己能解析到），却会让依赖图多一条毫无意义的自环，把成环检测的信号污染掉
- happy path：`requires` 缺省解析为空、给了值则逐项断言

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证**：删掉自依赖检查 → 该测试应 FAIL。

- [ ] **Step 4: 提交**

```bash
git commit -m "feat(plugin): let a plugin declare the tools it requires"
```

---

### Task 2: host 拆两个 owner，暴露 `Suspend`/`Resume`

**Files:**
- Modify: `internal/plugin/host/activate.go`（owner 拆分、`Plugin` 增字段与方法）
- Modify: `internal/plugin/host/contribute.go`（贡献登记到贡献侧 owner）
- Test: `internal/plugin/host/suspend_test.go`（新建）
- Test: `internal/plugin/host/activate_test.go`（既有断言随 owner 拆分调整，只增不减）

**Interfaces:**
- Produces:
  - `func ToolsOwner(owner lifecycle.Owner) lifecycle.Owner` —— 由实例 owner 推出贡献侧 owner（`<owner>/tools`）
  - `func (p *Plugin) Suspend(ctx context.Context) error`
  - `func (p *Plugin) Resume(ctx context.Context) error`
  - `func (p *Plugin) Suspended() bool`

**登记结构（本 task 的全部难点）：**

| owner | 条目 |
|---|---|
| `plugin:x@v`（实例侧） | `wasm-runtime`、`wasm-instance-pool`、以及**一条指向贡献侧 owner 的条目**，其 dispose 调用 `ledger.DisposeOwner(ToolsOwner(owner))` |
| `plugin:x@v/tools`（贡献侧） | 每个工具的 `tool:<name>` 与 `gateable:<name>` |

那条跨 owner 的条目是关键：**`DisposeOwner(实例 owner)` 必须仍然拆干净一切**，A4a 的 loader、CLI 排空、`ServeResult.Close` 全都只认这一个 owner。逆序 dispose 决定它必须在两个 wasm 条目**之后**登记，这样拆除顺序是「先撤工具、再关池、最后关 runtime」。

**四条不变量，逐条要有测试：**
1. `Suspend` 后：工具从 `Registry.Descriptors()` 消失、`toolauth.IsGateable` 转 false、`Snapshot()` 里贡献侧 owner 为空，而实例侧 owner **仍有** runtime 与 pool 两条。
2. `Resume` 后：工具回到注册表与 gateable 目录，且**用的是同一个池**（拿 guest 内可观测的跨调用状态断言，不是只看工具名回来了）。
3. `Suspend` 两次 / `Resume` 未挂起者 → 返回 error 点名当前状态。状态机不允许含糊。
4. 挂起中 `DisposeOwner(实例 owner)` → 干净拆除，不因贡献侧已空而报错。

**`Resume` 的重名预检**：恢复时那些工具名可能已被别的贡献者占用（`RegisterDescriptor`/`toolauth.Contribute` 对重名是 panic）。`Resume` 必须先检查再贡献，冲突返回 error 点名冲突的工具名。**这与 A4a 终审抓到的 `restore` panic 是同一个坑，别再踩一次。**

- [ ] **Step 1: 写失败测试**（上述四条不变量 + `Resume` 重名预检）

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证（三个）**
  - 删掉实例侧那条指向贡献侧 owner 的条目 → 「DisposeOwner 拆干净」的测试应 FAIL（工具仍在注册表）
  - `Resume` 去掉重名预检 → 重名测试应从 error 变成 panic，测试应 FAIL
  - `Suspend` 改成整个 `DisposeOwner(实例 owner)` → 「实例侧仍有 runtime 与 pool」的测试应 FAIL

- [ ] **Step 4: `-race`**

```bash
go test ./internal/plugin/host/ -race -count=1 -p 1 -timeout 120s
```

- [ ] **Step 5: 提交**

---

### Task 3: 依赖图与成环检测

**Files:**
- Create: `internal/plugin/loader/depgraph.go`
- Test: `internal/plugin/loader/depgraph_test.go`

**Interfaces:**
- Produces:
  - `func resolveStates(entries []depNode, resolvable func(toolName string) bool) (map[string]depState, error)`
  - `type depNode struct{ Name string; Provides, Requires []string }`
  - `type depState int`，取值 `depActive` / `depSuspended`

**纯函数，不碰 ledger、不碰 host**——这是本 task 与 Task 4 的边界，也是它能被穷尽测试的原因。

**算法**（写进函数 doc）：
1. 初始全部 `depActive`。
2. 迭代：某节点的 `Requires` 里存在一个名字，既不被 `resolvable` 认可、也不由当前仍 `depActive` 的节点提供 → 该节点转 `depSuspended`。
3. **迭代轮数上限 = 节点数**：每轮至少有一个节点转挂起，否则收敛完成。轮数用完仍在变 → 这是编程错误，`panic` 点明不变量（而不是自旋）。
4. **成环**：`Requires` 与 `Provides` 构成的图有环 → 返回 error 点名环上全部插件。环是 operator 数据，必须是 error 不是 panic。

- [ ] **Step 1: 写失败测试**

覆盖：
- 无依赖 → 全 `depActive`
- A 提供 T、B 依赖 T，两者都在 → 都 `depActive`
- B 依赖 T，无人提供且 `resolvable` 说不 → B `depSuspended`
- B 依赖 T，`resolvable` 说可以（内建工具）→ B `depActive`，**即使没有任何插件提供 T**
- **三级级联**：A 提供 T1；B 依赖 T1 且提供 T2；C 依赖 T2。A 缺席 → B 与 C 都 `depSuspended`
- 级联的**部分传播**：上面的图里 A 在、B 缺席 → C 挂起而 A 仍 `depActive`
- **成环**：A 依赖 B 的工具、B 依赖 A 的工具 → 返回 error，错误里两个名字都出现
- 自环由 Task 1 在清单层拦掉，这里补一条断言说明该情形不会到达（若到达则报错）

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证（两个）**
  - 把轮数上限改成无上限的 `for {}` → 成环测试应挂死（**用 `-timeout 20s` 单跑这一条**，确认它确实是被超时打断的，然后立刻还原；这一步是本 plan 唯一允许观察到挂死的地方，别在包级 suite 上做）
  - 删掉级联传播（只挂起直接不满足者）→ 三级级联测试应 FAIL

- [ ] **Step 4: 提交**

---

### Task 4: 收敛时挂起与恢复

**Files:**
- Modify: `internal/plugin/loader/loader.go`（`converge` 末尾接依赖收敛；`instance` 记状态）
- Test: `internal/plugin/loader/suspend_test.go`（新建）

**Interfaces:**
- Produces: `InstanceStatus.State` 新增取值 `StateSuspended`；`InstanceStatus` 增 `SuspendedBy []string`（点名是哪些未满足的工具名导致的）

**接线顺序（在一次 `Apply` 内，全部仍在同一个 `ApplyAtBoundary` 里）：**
1. 先完成 A4a 既有的收敛（卸载 → 激活），得到当前挂载集合。
2. 用挂载集合 + 注册表可解析性构图，调 `resolveStates`。
3. 对比当前状态与目标状态：转挂起的调 `Plugin.Suspend`，转活跃的调 `Plugin.Resume`。
4. `resolveStates` 报错（成环）→ 整个 `Apply` 返回错误，**已挂载的插件保持原状不动**：成环是清单错误，不该顺带把好插件拆了。

**两条边界，逐条要有测试：**
- **挂起的插件不参与「提供」**：`resolveStates` 的输入里，已挂起插件的 `Provides` 必须被排除——否则 B 挂起后 C 会误以为 B 提供的工具还在。
- **恢复的顺序**：多个插件同时可恢复时，必须按依赖顺序恢复（被依赖者先），否则先恢复的那个下一轮又被算成不满足。

**事件**（照设计文档 §8 的命名风格）：`plugin/suspended`（含未满足的工具名、是否级联）、`plugin/resumed`。

- [ ] **Step 1: 写失败测试**

用真 wasm 夹具与真注册表覆盖：
- 装 B（依赖 A 的工具）而不装 A → B 挂起、工具不在注册表、`Status()` 里 `State=suspended` 且 `SuspendedBy` 点名那个工具
- 之后把 A 加进清单再 `Apply` → B 自动恢复，工具回来，且**同一个池**（跨调用状态可见）
- 移除 A → B 再次挂起
- 三级级联：A→B→C，移除 A 后 B、C 都挂起；恢复 A 后**一次 `Apply`** 内两者都恢复
- 成环的清单 → `Apply` 报错，且已挂载插件一个都没被动过（`Snapshot()` 与注册表同时断言）
- **循环轮数**：所有 `Apply` 循环写死 ≤5 轮，每轮断言实例数上限

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证（两个）**
  - 让已挂起插件仍然参与「提供」→ 三级级联测试应 FAIL
  - 恢复不按依赖顺序（逆序恢复）→ 「一次 Apply 内两者都恢复」应 FAIL

- [ ] **Step 4: `-race`（串行）**

- [ ] **Step 5: 提交**

---

### Task 5: `plugins status` 呈现挂起态

**Files:**
- Modify: `internal/cli/plugins_command.go`
- Test: `internal/cli/plugins_command_test.go`

A4a 的 `status` 能区分三种「没生效」（禁用 / 激活失败 / 在途未收敛）。本期加第四种：**挂起**——且必须能一眼看出**卡在哪个工具上**，否则 operator 面对一个空目录无从下手。

- [ ] **Step 1: 写失败测试**

- 挂起的条目在 `status` 里状态是 `suspended`，且行内点名未满足的工具名
- 级联挂起与直接挂起可区分（级联的要指出它依赖的那个插件也挂了）
- 四种状态同屏时各自成行，互不吞并

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证**：把 `SuspendedBy` 从输出里删掉 → 「点名未满足工具」的测试应 FAIL。

- [ ] **Step 4: 提交**

---

### Task 6: `TaskGate` 下沉到中立包

**Files:**
- Create: `internal/taskgate/taskgate.go`（由 `internal/runtime/taskgate.go` 移入）
- Modify: `internal/runtime/runtime.go`、`internal/plugin/loader/loader.go` 及全部构造点
- Test: 既有 `taskgate_test.go` 随之移动，断言只增不减

A4a 终审记下的分层倒置：`internal/plugin/loader` 为了拿 `TaskGate` 而 import `internal/runtime`，插件层依赖运行时层。`TaskGate` 是个纯同步原语，不该绑在 runtime 包里。

**这是纯搬移，不改语义**：`Begin`/`BeginChild`/`ApplyAtBoundary`/`ErrApplyPending` 的行为一字不动。搬完后 `internal/runtime` 与 `internal/plugin/loader` 都 import `internal/taskgate`，两者之间不再有依赖。

- [ ] **Step 1: 搬移并更新全部构造点**

- [ ] **Step 2: 跑既有测试确认行为不变**

```bash
go test ./internal/taskgate/ ./internal/runtime/ ./internal/plugin/... ./internal/cli/ -count=1 -p 1 -timeout 180s
```

预期：全 PASS。搬移不应改变任何行为。

- [ ] **Step 3: 确认依赖方向**

```bash
go list -deps ./internal/plugin/loader | grep -c "legion-agent/internal/runtime"
```

预期：`0`。

- [ ] **Step 4: 提交**

```bash
git commit -m "refactor(taskgate): move the boundary gate out of the runtime package"
```

---

### Task 7: 端到端验收与文档回写

**Files:**
- Test: `internal/plugin/loader/e2e_test.go`（在 A4a 的验收文件里追加，不新建）
- Modify: docs 仓 `docs/design/architecture/legion-plugin-system.md`（§5.5 标注已实现并写明判定口径与成环行为；§9 路线图 A4b 标交付）

**⚠️ 本 task 反复激活/挂起/恢复真 wasm 实例，Fork-bomb 规程逐条适用**：轮数写死 ≤5，每轮断言实例数上限，`-timeout 120s`，`-p 1`。

- [ ] **Step 1: 三态全生命周期**

```text
plugins.json 三条目 A→B→C（A 提供 T1，B 依赖 T1 提供 T2，C 依赖 T2）
  → Apply → 三者皆 Active，三个工具都在注册表与 gateable 目录
  → 移除 A → reload → B、C 转 suspended，T2 与 C 的工具从目录消失，A 的 owner 不在 ledger
  → 但 B、C 的实例侧 owner 仍在（runtime + pool 各一条）
  → 加回 A → reload → 一次收敛内三者全部 Active，工具全回来
```

- [ ] **Step 2: 挂起态的可观测性**

`plugins status` 在上述每一步的输出：挂起原因点名到具体工具，级联与直接挂起可区分。

- [ ] **Step 3: 拒绝路径**

- 成环清单 → `Apply` 报错点名环上插件，已挂载的一个都没动
- 恢复时工具名已被别的插件占用 → `Resume` 返回 error（不是 panic），该插件停在 suspended 并在 status 里可见原因

- [ ] **Step 4: 全量回归**

```bash
go build ./... && go vet ./... && go test ./... -count=1 -timeout 300s && gofmt -l .
```

再逐包串行跑 `-race`（至少 `internal/plugin/...`、`internal/runtime`、`internal/taskgate`）。

- [ ] **Step 5: 文档回写**

§5.5 从「结论：要做」改成「已实现」，并补两条实现口径：判定看**注册表可解析性**（内建工具也算满足）、成环是 error 且已挂载插件不受影响。§9 路线图 A4b 标交付。

- [ ] **Step 6: 提交并开 PR**

---

## 交付后状态

- 依赖不满足的插件**挂起**而非半残：工具撤出目录，模型看不见它，实例与 guest 状态保留
- 依赖恢复后**重新贡献**而非重新初始化，一次 `Apply` 内完成整条链的收敛
- 依赖成环被当作 operator 数据错误报出来，且不牵连已挂载的插件
- `plugins status` 四种「没生效」各自可辨，挂起能点名卡在哪个工具
- `internal/plugin/loader` 不再 import `internal/runtime`，分层倒置消除

**尚未包含**：运行期健康度驱动的自动失败与卸载（§6.9 故障计数）、文件 watcher、签名与远程分发、GUI 同意流、策略钩子与 prompt 段、`plugin/unload_leaked` 在途泄漏诊断。
