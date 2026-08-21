# A4a 插件 Loader 与任务边界生效 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让部署方用一份声明式清单（`plugins.json`）管理插件目标态——启动期自动拉起、运行期 `agent plugins reload` 收敛——且**目标态切换绝不打断进行中的任务**。

**Architecture:** `internal/plugin/manifest` 解析两级清单（插件自带 `plugin.json` + 部署侧 `plugins.json`），做 sha256 校验与「声明 ∩ 授权」求交；`internal/plugin/loader` 的 `Apply` 对比目标态与实际态，逐条卸载/激活，失败回滚到旧实例；`internal/runtime` 新增任务边界闸门，让 `Apply` 只在任务边界之间落地（契约 4）。挂载与卸载复用 P1 已交付的 `host.Activate` 与 `lifecycle.Ledger`，本期不碰 ABI、不碰实例池。

**Tech Stack:** Go 1.26.0，module `github.com/stardust/legion-agent`。**不新增任何第三方依赖**——清单用标准库 `encoding/json`，CLI 用既有 `github.com/spf13/cobra v1.10.2`。

**不含**（各自独立 plan）：三态依赖收敛与级联挂起（A4b）、`plugin.json` 的 `requires:` 字段、文件 watcher、签名与远程分发、GUI 同意流、策略钩子与 prompt 段。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：禁止 fallback、禁止静默吞错、禁止零值假装正常；错误用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装；不变量违反用 `panic`。
- 公开 API 必须有 Go doc 风格注释，以标识符名开头，且不得与代码矛盾。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿，`gofmt -l .` 输出为空。
- 涉及并发的任务额外跑 `go test -race`，**plugin / runtime 包串行跑**（`-p 1`）。
- 错误路径必须有测试断言「确实返回 error」，不得只测 happy path。
- 每个 task 完成后做一次**变异验证**：把该 task 的核心机制改坏，确认测试确实 FAIL，把失败输出留在报告里，然后还原。
- **格式决定：清单一律 JSON**（`plugin.json` / `plugins.json`）。设计文档 §5.4/§6.2 写的是 yaml，但本仓无 YAML 依赖、主配置 `agent.json` 走 `encoding/json`（`internal/config/config.go:271`）。Task 7 负责把设计文档改成 json 口径。
- 设计依据：docs 仓 `docs/design/architecture/legion-plugin-system.md` §5.4（Loader）、§6.2（插件包与清单）、§6.12 契约 4（任务边界）、§8（事件与诊断出口）。

## ⚠️ Fork-bomb 安全规程（血的教训，违反会宕机）

P1 期间一个测试两次把机器打挂：`host.test.exe` 吃到 **170 GB 虚存**（Windows 事件 2004 + Kernel-Power 41），原因是递归测试把**被测功能本身**当唯一终止条件。本期同样涉及「反复激活/卸载插件」，规矩照旧：

1. 任何会递归或反复分配的测试，必须有**独立于被测功能**的硬边界：计数器在 spawn 之前检查、额度小（≤8）、越界**返回 error 而不是在 wasm 帧里 `t.Fatalf`**（`runtime.Goexit` 穿 wazero 帧不安全）。
2. 这类测试**先实现后跑**，用变异证据替代 RED 证据，并在报告里写明这次顺序反转。
3. 每次 `go test` 都带 `-timeout 120s`；plugin / runtime 包用 `-p 1`；递归类测试绝不 `-count>1`。
4. 循环 `Apply` 的测试必须限定轮数（≤5 轮），且每轮断言实例数有上限——不允许「跑到收敛为止」这种没有上界的写法。

## 前置事实（已在 master `4f5d329`，直接用）

```go
// internal/plugin/host
func Activate(ctx context.Context, ledger *lifecycle.Ledger, owner lifecycle.Owner, spec Spec) (*Plugin, error)

type Spec struct {
    Name         string
    Wasm         []byte
    Tools        []tool.Descriptor  // 非空；每项 Name/Description/Group/Timeout 必填
    Grant        perm.Grant
    Deps         Deps
    MemoryPages  uint32             // >=1
    MaxInstances int                // >=1
}

type Manifest struct {                 // guest 自描述（plugin_invoke(OpManifest)）
    Name     string   `json:"name"`
    Version  string   `json:"version"`
    Provides []string `json:"provides"`
}

type Deps struct {
    PluginName string
    Logger     *slog.Logger
    Config     json.RawMessage
    KV         KVStore
    HTTP       *http.Client
    FS         port.WorkspacePathGuard
    Events     port.EventBus
    Tools      *tool.Registry
    Agent      domain.Agent
}

type Plugin struct { Name string; Manifest Manifest /* pool 不导出 */ }

func EventHasCategory(event domain.RuntimeEvent, category string) bool

// internal/plugin/perm
type Grant struct {
    Log, Config, KV, HTTP, FS, Tool bool
    AllowedHosts, AllowedPaths      []string
}

// internal/lifecycle
func NewLedger() *Ledger
func (l *Ledger) Add(owner Owner, label string, dispose func() error) func() error
func (l *Ledger) DisposeOwner(owner Owner) error
func (l *Ledger) Snapshot() map[Owner][]string

// internal/tool
type Descriptor struct {
    Name, Description string
    InputSchema       map[string]any
    RiskLevel         string
    Timeout           time.Duration
    Group             string   // 必填，否则进不了能力目录
    Sensitive         bool
}
```

**Activate 的两条硬合同（P1 定的，本期必须守）**：

- `owner` **专属于单次激活**：`Activate` 入口拒绝非空 owner。重载同一插件必须先 `DisposeOwner` 旧 owner，再激活新的。
- 激活失败走句柄级逆序回滚，只撤销本次激活登记的条目，dispose 错误 `errors.Join` 到激活错误上。

**运行时现状（契约 4 的战场）**：`Runtime.effectiveTools(task)`（`internal/runtime/runtime.go:258`）返回的是 `*tool.Registry` 的**视图**，调用时才解析（不变量 I1：派生视图不得拷贝 handler）；`buildCatalog(effTools)`（`runtime.go:281`）每任务构建能力目录。所以目录已经是「每任务一份」，缺的是**让 Loader 的增删只发生在任务之间**。

---

### Task 1: 两级清单的解析与校验

**Files:**
- Create: `internal/plugin/manifest/manifest.go`
- Test: `internal/plugin/manifest/manifest_test.go`
- Test fixture: `internal/plugin/manifest/testdata/`（几份合法与非法清单）

**Interfaces:**
- Produces:
  - `type PluginManifest struct{ Name, Version string; ABI int; SHA256 string; Capabilities []string; Limits Limits; Network Network; Filesystem Filesystem; Tools []ToolDecl }`
  - `type Limits struct{ TimeoutMs int; MaxMemoryPages uint32; MaxInstances int }`
  - `type Network struct{ AllowedHosts []string }`、`type Filesystem struct{ AllowedPaths []string }`
  - `type ToolDecl struct{ Name, Description, Group, RiskLevel string; InputSchema map[string]any; TimeoutMs int; Sensitive bool }`
  - `type Deployment struct{ Plugins []Entry }`
  - `type Entry struct{ Name, Source string; Enabled bool; Grant GrantDecl; Tools []ToolAccept; Config json.RawMessage }`
  - `type GrantDecl struct{ Capabilities, AllowedHosts, AllowedPaths []string }`
  - `type ToolAccept struct{ Name, RiskLevel string; Sensitive *bool }`
  - `func ParsePlugin(data []byte) (PluginManifest, error)`
  - `func ParseDeployment(data []byte) (Deployment, error)`

**两级清单各自的职责**（写进包 doc，别让后来者搞混）：

- `plugin.json` 跟着 `.wasm` 走，是**插件作者的声明**：我叫什么、要哪些能力、贡献哪些工具、想要多少资源。
- `plugins.json` 是**部署方的目标态与授权**：装哪些、开不开、给哪些能力、接受哪些工具。
- 交集与覆盖规则在 Task 2；本 task 只做「读进来、校验形状、非法就响亮报错」。

- [ ] **Step 1: 写失败测试**

覆盖（每条都要断言**确实返回 error** 且错误信息点名字段）：

- `plugin.json` 缺 `name` / 缺 `version` / `abi` 不是 1 / `sha256` 不是 64 位十六进制 → 报错
- `tools` 为空数组 → 报错（插件不贡献工具就没有存在意义，与 `host.Spec.Tools` 非空要求同源）
- 某个 tool 缺 `group` 或 `timeout_ms <= 0` → 报错点名是哪个工具的哪个字段（`host.validateSpec` 会再拦一次，但错误在清单层报更可读）
- `capabilities` 出现未知能力名（不在 log / config / kv / http / fs / tool 六个里）→ 报错点名该值
- `limits.max_memory_pages == 0` 或 `max_instances < 1` → 报错
- `plugins.json` 条目缺 `name` / 缺 `source` → 报错
- 同一份 `plugins.json` 里出现两个同名条目 → 报错（目标态必须无歧义）
- **未知字段一律报错**：两个解析函数都用 `json.Decoder` + `DisallowUnknownFields()`。理由：清单里写错一个键名却被静默忽略，等于配置没生效而无人知晓——正是 fail-loud 铁律要防的
- happy path：一份完整清单解析出的每个字段都逐项断言，不能只断言 `err == nil`

- [ ] **Step 2: 实现** `manifest.go`

`Enabled` 用 `*bool` 解析后归一：缺省视为 `true`（条目写进目标态就是要装），显式 `false` 才是禁用。这属于契约显式声明的可选，不算兜底——在字段 doc 里写明。

- [ ] **Step 3: 变异验证**：把 `DisallowUnknownFields()` 去掉 → 「未知字段报错」的测试必须 FAIL。

- [ ] **Step 4: 提交**

```bash
git commit -m "feat(plugin): parse the plugin and deployment manifests"
```

---

### Task 2: 装配——sha256 校验、能力求交、生成 host.Spec

**Files:**
- Create: `internal/plugin/manifest/assemble.go`
- Test: `internal/plugin/manifest/assemble_test.go`
- Test fixture: `internal/plugin/manifest/testdata/pkg/`（一个插件目录：`plugin.json` + 一份真 `.wasm`，从 `internal/plugin/host/testdata/plugin.wasm` 拷一份）

**Interfaces:**
- Consumes: Task 1 的 `PluginManifest` / `Entry` / `Limits`
- Produces:
  - `func LoadPackage(dir string) (PluginManifest, []byte, error)`——读目录里的 `plugin.json` 与 `plugin.wasm`，**校验 sha256**，不匹配即报错并同时给出期望值与实际值
  - `func AssembleSpec(pm PluginManifest, entry Entry, deployLimits Limits) (host.Spec, error)`

**三条求交规则（本 task 的全部难点，逐条要有测试）：**

1. **能力：声明 ⊄ 授权 = 拒绝加载**。插件声明 `[log, http, kv]`、部署只授 `[log, http]` → 报错，信息形如 `plugin "legion-jira" requires capability "kv" which the deployment does not grant`。**反过来不对称**：部署授了插件没声明的能力，直接忽略不授（多授无意义，且扩大攻击面）。
2. **资源上限取 min**：`min(插件申请, 部署上限)`。`deployLimits` 由调用方给（Task 6 从 `agent.json` 取，本 task 只管算）。任一侧为 0 视为未申报——**除了 `MaxMemoryPages`：两侧都为 0 报错**，因为 `host.NewRuntime` 对 0 页是 panic。
3. **工具：部署必须显式接受**。`entry.Tools` 里没列到的插件工具**不注册**；`entry.Tools` 里列了插件没声明的名字 → 报错（写错名字比少装一个工具更该响亮）。`ToolAccept.RiskLevel` / `Sensitive` 非空时覆盖插件的声明，且**只能收紧不能放松**：部署可以把插件声明的 `low` 改成 `high`、把 `sensitive:false` 改成 `true`，反向则报错。

`AllowedHosts` / `AllowedPaths` 同样取交集（插件声明 ∩ 部署授权），空集就是空集——`perm.Grant` 已经定义「HTTP 授权但 allowlist 为空 = 一个域名都到不了」。

- [ ] **Step 1: 写失败测试**

覆盖：上面三条规则的正反面各一条；sha256 不匹配 → 报错且**错误里同时有期望与实际**；`plugin.wasm` 缺失 → 报错；`plugin.json` 缺失 → 报错；happy path 断言产出的 `host.Spec` 逐字段正确（`Tools` 顺序、`Timeout` 由 `timeout_ms` 换算、`MemoryPages` / `MaxInstances` 取到 min、`Grant` 的六个 bool 与两个 allowlist）。

**这条测试要有意义，必须断言 `Spec.Grant` 的每个 bool**，而不是只断言没报错——只测「没报错」的话，一个把所有能力都授上的实现同样能过。

- [ ] **Step 2: 实现**

`AssembleSpec` 不填 `Spec.Deps`：依赖注入是 Loader 的活（Task 3），清单包不该知道 `*http.Client` 或 `*tool.Registry` 的存在。这个边界写进函数 doc。

- [ ] **Step 3: 变异验证（两个）**
  - 把「声明 ⊄ 授权」改成「取交集后继续」→ 缺能力的测试应 FAIL
  - 把 sha256 校验整段删掉 → 摘要不符的测试应 FAIL

- [ ] **Step 4: 提交**

---

### Task 3: `Loader.Apply`——目标态收敛与失败回滚

**Files:**
- Create: `internal/plugin/loader/loader.go`
- Test: `internal/plugin/loader/loader_test.go`

**Interfaces:**
- Consumes: Task 2 的 `LoadPackage` / `AssembleSpec`；`host.Activate`；`lifecycle.Ledger`
- Produces:
  - `type Loader struct{ /* unexported */ }`
  - `type Config struct{ Ledger *lifecycle.Ledger; Deps func(name string, cfg json.RawMessage) host.Deps; Events port.EventBus; Logger *slog.Logger; DeployLimits manifest.Limits }`
  - `func New(cfg Config) (*Loader, error)`
  - `func (l *Loader) Apply(ctx context.Context, dep manifest.Deployment, root string) error`
  - `func (l *Loader) Status() []InstanceStatus`
  - `type InstanceStatus struct{ Name, Version, State string; Tools []string; LastError string }`

**收敛语义（逐条有测试）：**

| 目标态 vs 实际态 | 动作 |
|---|---|
| 新增条目（`enabled` 非 false） | 激活 |
| 条目消失 | 卸载 |
| 条目 `enabled: false` | 卸载。**与「条目被删除」行为完全一致** |
| 条目内容变了（sha256 / grant / tools / config 任一变） | 先卸载旧的，再激活新的 |
| 条目没变 | 不动。**这条必须有测试**：不动的插件不能被重启（重启会丢 guest 内存状态，也白白付一次实例化开销） |

**owner 命名**：`lifecycle.Owner("plugin:" + name + "@" + version)`。版本进 owner 是为了让「换版本」天然是两个不同 owner，与 `Activate` 拒绝复用 owner 的合同对齐。

**失败必须回滚到旧实例**（设计文档 §5.4 的硬要求）：替换一个正在跑的插件时，新实例激活失败 → 把旧实例重新激活回来；旧实例也拉不起来 → 两个错误 `errors.Join` 一起报，并且发 `plugin/activation_failed` 事件写明**没能恢复旧实例**。`Apply` 永远返回「已发生的全部失败」的合并错误，绝不静默留下半套目标态。

**事件**（经 `Config.Events` 发，类型名照设计文档 §8）：`plugin/loaded`、`plugin/unloaded`（reason: `manifest-removed` / `disabled` / `replaced`）、`plugin/activation_failed`（含失败步骤、已回滚条目数、是否恢复旧实例）。

- [ ] **Step 1: 写失败测试**

用 `internal/plugin/host/testdata/plugin.wasm` 做真插件，`adapter.NewMemoryEventBus()` 与 `adapter.NewMemoryAuditLog()` 断言事件。覆盖：

- 空实际态 + 两条目标态 → 两个插件都激活，`ledger.Snapshot()` 有两个 owner，工具出现在 registry
- 再 `Apply` 一次同样的目标态 → **实例没有被重建**（用「同一实例才有的可观测量」断言，例如激活计数器，而不是只看工具还在）
- 条目改成 `enabled: false` → 卸载，工具消失，`toolauth.IsGateable` 转 false，owner 从 ledger 消失
- 条目从目标态删掉 → 与上一条行为一致
- 换 sha256 / 版本 → 旧的先卸载、新的再激活，且旧 owner 不残留
- **替换时新实例激活失败 → 旧实例被恢复**，`Apply` 返回错误，事件里写明已恢复
- **新旧实例都拉不起来 → 两个错误都在返回值里**（各自可辨），事件写明未恢复
- 一条失败不阻断其余条目：三条目标态中间一条坏掉，另外两条仍收敛到位，返回的合并错误只提到坏掉的那条

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证（两个）**
  - 把「内容没变就不动」改成「每次都重建」→ 幂等测试应 FAIL
  - 把回滚到旧实例的分支删掉 → 「替换失败要恢复旧实例」的测试应 FAIL

- [ ] **Step 4: `-race` 验证**

```bash
go test ./internal/plugin/... -race -count=1 -p 1 -timeout 120s
```

- [ ] **Step 5: 提交**

---

### Task 4: 任务边界闸门（契约 4 的机制）

**Files:**
- Create: `internal/runtime/taskgate.go`
- Test: `internal/runtime/taskgate_test.go`

**Interfaces:**
- Produces:
  - `type TaskGate struct{ /* unexported */ }`
  - `func NewTaskGate() *TaskGate`
  - `func (g *TaskGate) Begin() (end func(), err error)`
  - `func (g *TaskGate) ApplyAtBoundary(ctx context.Context, wait time.Duration, fn func() error) error`

**为什么需要它**：能力目录已经是每任务一份，但工具**注册表**是全局共享的。Loader 在任务半途撤销一个工具，正在跑的任务会拿到 `ErrToolNotFound`，而它的 prompt 里还在广告这个工具——契约 4 要求「运行中的 task 沿用它启动时的目录，新目录只对之后启动的 task 生效」。

**机制（不做引用计数，复用实例池已验证过的 drain 写法）：**

1. `Begin()` 让在途任务计数 +1，返回的 `end` 让它 -1；**有 pending apply 时 `Begin()` 直接返回错误**，让新任务稍后重试而不是挤进正在切换的目录。
2. `ApplyAtBoundary` 置 pending 标志 → 等在途归零（上限 `wait`）→ 跑 `fn()` → 清标志。
3. **等待超时是响亮失败**：不执行 `fn`、清掉 pending 标志、返回的错误里点名还有几个任务在跑。绝不「等不到就直接改」——那正是契约 4 要禁止的。

选「`Begin()` 拒绝」而不是「`Begin()` 阻塞」：阻塞会把插件重载变成一次隐形的服务暂停，调用方看到的只是任务莫名变慢；返回错误则调用方可以自己决定重试还是报错。这个取舍写进 doc。

- [ ] **Step 1: 写失败测试**

覆盖：

- 无在途任务时 `ApplyAtBoundary` 立刻执行 `fn`
- **在途任务未结束时 `fn` 不被执行**；任务结束后才执行（用 channel 做确定性交接，**不要用 `time.Sleep` 猜时机**）
- pending 期间 `Begin()` 返回错误（不是阻塞到超时）
- 等待超时 → `fn` **一次都没被调用**、返回错误点名剩余任务数、且此后 `Begin()` 恢复正常（标志被清掉，不能把闸门永久卡死）
- `fn` 返回错误时原样透出，且 pending 标志照样清掉
- 并发：50 goroutine 反复 `Begin`/`end` 与一次 `ApplyAtBoundary` 并行，`-race` 无竞争，且断言**确实发生了竞争窗口**（例如 apply 期间至少有一次 `Begin` 被拒），否则这条测试是空跑

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证**：把「等待超时就不执行 fn」改成「超时后照样执行」→ 超时测试应 FAIL。

- [ ] **Step 4: `-race`**

```bash
go test ./internal/runtime/ -race -count=1 -p 1 -timeout 120s
```

- [ ] **Step 5: 提交**

---

### Task 5: 接线——RunTask 走闸门，Loader 经闸门落地

**Files:**
- Modify: `internal/runtime/runtime.go`（`RunTask` 开头 `Begin()`，`defer end()`）
- Modify: `internal/plugin/loader/loader.go`（`Apply` 经闸门）
- Test: `internal/runtime/taskgate_wiring_test.go`
- Test: `internal/plugin/loader/boundary_test.go`

**Interfaces:**
- Consumes: Task 3 的 `Loader.Apply`、Task 4 的 `TaskGate`
- Produces: `loader.Config` 增加 `Gate *runtime.TaskGate` 与 `ApplyWait time.Duration`；`runtime.Config` 增加 `Gate *TaskGate`

**两处接线，三个不变量：**

1. `RunTask` 一进来就 `Begin()`，`defer end()`。`Begin()` 报错时 `RunTask` **直接返回错误**（此刻正在切换插件目录，起新任务会拿到半新半旧的目录）。错误要可辨识，调用方能据此重试。
2. `Loader.Apply` 的**收敛动作整体**放进 `ApplyAtBoundary` 的 `fn` 里——不是每条目标态一次闸门。理由：一次 `Apply` 可能卸载 A 再装 B，中间态不该被任何任务看见。
3. **`Gate` 为 nil 是接线错误，不是「不用闸门」**：`loader.New` 与 `runtime.NewRuntime` 都要在 `Gate == nil` 时 fail-loud。给它一个「没配就不管」的默认，等于把契约 4 变成一条随时会被静默关掉的保护。

- [ ] **Step 1: 写失败测试**

覆盖：

- 一个任务在途时调 `Apply`，插件工具**在该任务眼里不变**；任务结束后 `Apply` 才落地
- `Apply` 期间起新任务 → `RunTask` 返回可辨识的错误，不是拿到半套目录
- 超时路径：在途任务超过 `ApplyWait` 不结束 → `Apply` 返回错误，且**目标态一条都没落地**（用 `ledger.Snapshot()` 与 registry 同时断言）
- `Gate == nil` → `loader.New` / `NewRuntime` 报错

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证**：把 `Apply` 的闸门去掉（直接跑收敛）→ 「在途任务眼里不变」的测试应 FAIL。**这条是契约 4 唯一的守门测试；如果它在变异下仍然过，说明它测的不是契约 4，回 Step 1 重设计。**

- [ ] **Step 4: 提交**

---

### Task 6: 装配与 CLI——启动期 Apply、`agent plugins status|reload`

**Files:**
- Create: `internal/cli/plugins_command.go`
- Test: `internal/cli/plugins_command_test.go`
- Modify: `internal/app/app.go`（持有 `*loader.Loader`，暴露 `Plugins()`）
- Modify: `internal/cli/command.go`（`serve` 装配处创建 Loader 并跑启动期 `Apply`；根命令挂 `plugins` 子命令）
- Modify: `internal/config/config.go`（新增 `plugins` 段）

**Interfaces:**
- Consumes: Task 3 的 `Loader`、Task 5 的接线
- Produces: `func (a *App) Plugins() *loader.Loader`；`agent plugins status`；`agent plugins reload`

**配置段**（`agent.json`，字段名跟随既有 snake_case 风格）：

```json
"plugins": {
  "manifest": "./configs/plugins.json",
  "root": "./plugins",
  "limits": { "timeout_ms": 10000, "max_memory_pages": 256, "max_instances": 4 },
  "apply_wait_ms": 60000
}
```

**`manifest` 缺省（键不存在）= 不启用插件**——这是契约显式声明的可选，不算兜底，在字段 doc 里写明。但**配了路径却读不到文件 = 启动失败**：配了就是要用。

**启动期 Apply 的失败处理**：任何一条插件激活失败，`serve` **不退出**，但必须 Error 级记录并让 `plugins status` 长期可见（设计文档 §8 要求「插件没生效」的三种原因可区分）。理由：一个坏插件不该让整个 agent 起不来，但它绝不能安静地不存在。

`plugins status` 的输出要能区分三种「没生效」：条目禁用 / 激活失败（附失败步骤与错误）/ 在途未收敛。

- [ ] **Step 1: 写失败测试**

覆盖：

- `plugins status` 在无插件时输出可读的空态，退出码 0
- 装了两个插件时，输出含名字、版本、状态、贡献的工具名
- 一个插件激活失败时，`status` 里该条目状态是失败且**带失败原因**，其余条目仍正常
- `plugins reload` 会真的重读清单并收敛（改 fixture 里的 `enabled` 后重载，工具消失）
- `plugins reload` 在有任务在途且超时的情况下 → **非零退出码**且错误信息说明是任务边界没等到
- 配了 `manifest` 路径但文件不存在 → `serve` 装配报错

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证**：把启动期 Apply 的失败从「记录并继续」改成「静默忽略」→ 「失败条目在 status 里可见」的测试应 FAIL。

- [ ] **Step 4: 提交**

---

### Task 7: 端到端验收与文档回写

**Files:**
- Test: `internal/plugin/loader/e2e_test.go`
- Modify: docs 仓 `docs/design/architecture/legion-plugin-system.md`（§5.4 / §6.2 的 yaml → json；§9 路线图 P2 拆成 A4a / A4b）

**⚠️ 本 task 的测试会反复激活/卸载真 wasm 实例，Fork-bomb 规程逐条适用**：轮数写死 ≤5，每轮断言实例数有上限，`-timeout 120s`，`-p 1`。

- [ ] **Step 1: 全生命周期**

```text
plugins.json 两条目 → 启动期 Apply → 两个插件的工具都出现在 Registry 与 gateable 目录
  → 模型形状的 Registry.Execute 调用成功
  → 改清单：一条 enabled:false、一条换版本 → reload
  → 前者的工具消失（IsGateable 转 false、ErrToolNotFound）、后者换成新 owner 且旧 owner 不残留
  → 全部条目移除 → reload → ledger.Snapshot() 为空
```

- [ ] **Step 2: 任务边界的端到端证明**

一个任务在途时 reload，断言：该任务**跑完**且用的是它启动时的工具集；reload 在它结束后才落地；新起的任务看到新目录。

- [ ] **Step 3: 拒绝路径**

- sha256 被改坏 → 该条目不激活、旧实例（若有）保持不变、`status` 里可见失败原因
- 插件声明了部署没授的能力 → 拒绝加载，错误点名缺哪个能力
- 部署 `tools` 列了插件没声明的名字 → 拒绝加载

- [ ] **Step 4: 全量回归**

```bash
go build ./... && go vet ./... && go test ./... -count=1 -timeout 300s && gofmt -l .
```

再逐包串行跑 `-race`（至少 `internal/plugin/...` 与 `internal/runtime`）。

- [ ] **Step 5: 文档回写**

设计文档三处要改：§5.4 与 §6.2 的清单示例改成 JSON 并说明为什么（本仓无 YAML 依赖）；§9 路线图把 P2 拆成 A4a（本期）与 A4b（三态收敛），A4a 标注交付的 PR 号。

- [ ] **Step 6: 提交并开 PR**

---

## 交付后状态

- 部署方改一份 `plugins.json` 就能增删插件：启动期自动收敛，运行期 `agent plugins reload` 收敛
- 挂载失败回滚到旧实例，失败原因（哪一步、回滚了几项、有没有恢复旧实例）进事件流与 `plugins status`
- sha256 与「声明 ∩ 授权」在加载期拦住摘要不符与越权声明
- **插件目标态的切换只发生在任务边界**：进行中的任务用完它启动时的目录，缓存不被中途打掉

**尚未包含**：三态依赖收敛与级联挂起（A4b，需要 `plugin.json` 增加 `requires:` 列工具名）、文件 watcher、签名与远程分发、GUI 同意流、策略钩子与 prompt 段、`plugin/unload_leaked` 的在途泄漏诊断。
