# P1 WASM 插件宿主实施计划（A3）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 legionAgent 能挂载一个 `.wasm` 插件、把它贡献的工具交给模型调用、再干净卸载——所有权、授权、审计、熔断全部落在既有通道上。

**Architecture:** wazero 宿主 + 自研精简 ABI。guest 导出 3 个函数（`plugin_alloc` / `plugin_free` / `plugin_invoke`），host 在模块名 `legion` 下按授权注册 host function（**未授权的函数根本不注册**，链接期缺失而非运行时拒绝）。实例长驻 reactor，池化并发，卸载走 `lifecycle.Ledger` 逆序撤销。

**Tech Stack:** Go 1.26.0，module `github.com/stardust/legion-agent`，新增依赖 `github.com/tetratelabs/wazero v1.12.0`（**当前 go.mod 尚无此依赖**）。

**不含**（各自独立 plan）：`plugin.Loader` 与 `plugins.yaml`、三态依赖收敛、热加载、签名分发、GUI 同意流。本期验收是「手工指定一个 `.wasm` 走通挂载→调用→卸载」。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：禁止 fallback、禁止静默吞错、禁止零值假装正常；错误用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装；不变量违反用 `panic`。
- 公开 API 必须有 Go doc 风格注释，以标识符名开头。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿，`gofmt -l .` 输出为空。
- 涉及并发的任务额外跑 `go test -race`（**逐包串行跑**，并行多包时 storage 会偶发 CGO sqlite 崩溃，与代码无关）。
- 错误路径必须有测试断言「确实返回 error」，不得只测 happy path。
- 每个 task 完成后做一次**变异验证**：把该 task 的核心机制改坏，确认测试确实 FAIL。本系列前一期两次抓出「测试绿但根本没测到东西」。
- 设计依据：Legion docs 仓 `docs/design/architecture/legion-plugin-system.md` §6（WASM 机制）、§6.12（接线契约）。

## 前置事实（已在 master，直接用）

PR #80 已合入 master（`df7c7b9`）。以下 API 可直接调用：

```go
lifecycle.NewLedger() *Ledger
(*Ledger).Add(owner Owner, label string, dispose func() error) func() error
(*Ledger).DisposeOwner(owner Owner) error
(*Ledger).Snapshot() map[Owner][]string

(*tool.Registry).RegisterDescriptor(Descriptor, Handler) func()   // 重名 panic
(*tool.Registry).Replace(Descriptor, Handler) func()
tool.RegisterOwned(ledger, owner, registry, descriptor, handler) func() error

toolauth.Contribute(GateableTool) func()                          // 重名/遮蔽内建 panic
tool.WithCallOrigin(ctx, "plugin:<name>") context.Context
```

## spike 实证的 ABI（**以此为准**）

spike 位于会话临时目录 `…/scratchpad/spike/`（`abi/host/main.go` 205 行，`deps/loader.go` 685 行 + `deps/loader_test.go` 425 行）。**若已被清理，本节足以重建。**

> ⚠️ **docs 仓 handoff 文档在两处写错了，以本节为准**：
> - 导出名是 **`plugin_invoke`**，不是 `plugin_call`
> - 只有 **3 个导出**，没有独立的 `plugin_manifest`——自描述是 `plugin_invoke` 的一个保留 op

**guest 导出**：

```
plugin_alloc(size i32) -> i32          // host 写入前必须先调它拿地址
plugin_free(ptr i32, size i32)
plugin_invoke(op i32, ptr i32, len i32) -> i64
```

**返回值打包**：`i64` 高 32 位 = ptr，低 32 位 = len。`outLen == 0` 表示无返回体（不是错误）。

**四个致命细节**（spike 里逐条踩出来的，写错任何一条都会得到间歇性 bug）：

1. **读出结果后必须立刻 copy**。guest 内存会增长并搬迁，持有 `Memory().Read` 返回的切片跨调用等于悬垂。
2. **`plugin_free` 必须用 `context.WithoutCancel(ctx)`**。ctx 已取消时 free 会连带失败，泄漏 guest 内存。
3. **实例化用 `WithStartFunctions("_initialize")`**。reactor 模块没有 `_start`，用默认配置会失败。
4. **`WithName("")`**。否则同一模块第二次实例化会因名字冲突失败——实例池必然踩到。

**host function 之间不得互相调用**：wazero 已知问题，一个 host function 调另一个会丢失 memory 访问。公共逻辑抽成普通 Go 函数。

**关键配置**：

```go
wazero.NewRuntimeConfig().
    WithCloseOnContextDone(true).   // 唯一能打断纯计算死循环的手段（wazero 无 fuel）
    WithMemoryLimitPages(pages)     // Rust guest 64 页(4MiB)够；标准 Go guest 需 128 页(8MiB)起
```

⚠️ **`WithCloseOnContextDone` 的代价**：超时会 close 整个模块，**实例报废不可复用**。实例池必须标记 dead 并新建，绝不放回池子。这是 Task 5 的核心不变量。

---

### Task 0: 前置——导出 `guardedToolName`

**Files:**
- Modify: `internal/runtime/lazytools.go`（`guardedToolName` → 导出或移位）
- Modify: `internal/runtime/runtime.go`（两处调用点）
- Test: `internal/runtime/toolguard_lazy_test.go`（既有，断言不变）

**理由：** 契约 1 要求插件发起的调用与模型发起的**共用** `toolNameGuard` 计数器。`guardedToolName` 现在是 `internal/runtime` 包内未导出函数，`internal/plugin/*` 调不到。

**在插件侧另写一份是错的**——两份实现分叉就等于两个计数器，正是契约 1 要防的事。

- [ ] **Step 1**：决定落点。二选一，推荐 A：
  - **A（推荐）**：移进 `internal/domain`，改名 `domain.GuardedToolName(call ToolCall) string`。理由：它是纯函数，只依赖 `domain.ToolCall`，且 `domain` 已被两侧 import，不引入新依赖方向。
  - B：在 `internal/runtime` 原地导出为 `runtime.GuardedToolName`。理由：改动最小。代价：`internal/plugin` 要 import `internal/runtime`，而 runtime 将来要 import plugin，成环。**选 A 可避免这个环。**

- [ ] **Step 2**：搬移 + 导出，更新 `runtime.go` 两处调用点（`toolNameGuard.record` 与 `nameByID`）。

- [ ] **Step 3**：跑既有测试确认行为不变：

```bash
go test ./internal/runtime/ -run "TestLoopCap|TestSameToolFail" -count=1
```

预期：全 PASS（这些测试在 PR #80 里已咬住 wrapper 语义，搬移不应改变任何行为）。

- [ ] **Step 4**：提交

```bash
git commit -m "refactor(domain): export GuardedToolName so plugins share the model's counters"
```

---

### Task 1: wazero 依赖与 ABI 编解码

**Files:**
- Modify: `go.mod` / `go.sum`
- Create: `internal/plugin/abi/abi.go`
- Test: `internal/plugin/abi/abi_test.go`

**Interfaces:**
- Produces:
  - `const (ExportAlloc/ExportFree/ExportInvoke string)`
  - `const HostModuleName = "legion"`
  - `func PackResult(ptr, length uint32) uint64`
  - `func UnpackResult(packed uint64) (ptr, length uint32)`
  - `const (OpManifest, OpCallTool, … int32)`

- [ ] **Step 1: 加依赖**

```bash
go get github.com/tetratelabs/wazero@v1.12.0
```

- [ ] **Step 2: 写失败测试** `internal/plugin/abi/abi_test.go`

覆盖：
- `UnpackResult(PackResult(p, l))` 对 `(0,0)` / `(1,1)` / `(math.MaxUint32, math.MaxUint32)` / 随机若干组均往返一致
- `PackResult(0, 0) == 0`（`outLen == 0` 是「无返回体」的约定，必须是零值）
- 高低位不串：`UnpackResult(PackResult(0xDEADBEEF, 0x12345678))` 精确等于两个原值

**这个测试要有意义，必须断言具体位布局**（`packed>>32 == ptr && uint32(packed) == length`），而不是只测往返——只测往返的话，把高低位对调的实现同样能通过。

- [ ] **Step 3: 实现** `abi.go`

- [ ] **Step 4: 变异验证**：把 `PackResult` 的高低位对调，确认位布局断言 FAIL。

- [ ] **Step 5: 提交**

---

### Task 2: 实例——编译、实例化、调用

**Files:**
- Create: `internal/plugin/host/instance.go`
- Test: `internal/plugin/host/instance_test.go`
- Test fixture: `internal/plugin/host/testdata/`（一个最小 guest 的 `.wasm`）

**Interfaces:**
- Produces:
  - `type Instance struct{…}`
  - `func (i *Instance) Invoke(ctx context.Context, op int32, in []byte) ([]byte, error)`
  - `func (i *Instance) Dead() bool`
  - `func (i *Instance) Close(ctx context.Context) error`

**测试夹具怎么来**：guest 用 Rust 编译最省事（68KB / 编译 17ms），但会给仓库引入 Rust 工具链依赖。**推荐：把编译好的 `.wasm` 提交进 `testdata/`，并在同目录放 guest 源码与一行编译命令的 README。** 理由：CI 不需要 Rust，且 `.wasm` 只有几十 KB。若走标准 Go guest 则是 3.26MB，太大——**这是选 Rust 做夹具的实际理由**。

- [ ] **Step 1: 写失败测试**

覆盖：
- JSON 往返：`Invoke(OpEcho, {...})` 返回等价 JSON
- 未知 op **不 crash**，返回可读错误或空结果（按 guest 契约）
- 同一实例连续 2000 次调用不泄漏（对比首尾 `mod.Memory().Size()`，增长应有界）
- `Invoke` 传 `nil` 输入不调 `alloc`（走 `len(in) == 0` 分支）
- **ctx 取消后 `Invoke` 返回 error 且 `Dead()` 为 true**
- `Close` 幂等

- [ ] **Step 2: 实现**，逐条落实上文「四个致命细节」。参考 spike `abi/host/main.go:27-61` 的 `call` 方法——它是唯一实证过的实现。

- [ ] **Step 3: 变异验证**（三个，逐个做）：
  - 去掉结果 copy，直接返回 `Memory().Read` 的切片 → 2000 次调用测试应 FAIL 或数据错乱
  - `plugin_free` 改回用原 ctx → ctx 取消测试应暴露 free 失败
  - 去掉 `WithStartFunctions("_initialize")` → 实例化应直接失败

- [ ] **Step 4: 提交**

---

### Task 3: host function 能力白名单

**Files:**
- Create: `internal/plugin/perm/perm.go`（**别叫 `capability`**——`internal/capability` 已被能力目录占用，同名两个包 import 必须起别名且极易看错）
- Create: `internal/plugin/host/hostfunc.go`
- Test: `internal/plugin/host/hostfunc_test.go`

**Interfaces:**
- Produces:
  - `type Grant struct { Log, Config, KV, HTTP, FS, Tool bool; AllowedHosts, AllowedPaths []string }`
  - `func BuildHostModule(ctx, rt wazero.Runtime, g Grant, deps Deps) (api.Module, error)`

**核心不变量：未授权的能力不注册进模块。** 是链接期缺失（guest 实例化时报 import 找不到），不是运行时返回 DENIED。

- [ ] **Step 1: 写失败测试**

覆盖：
- `Grant{Log:true}` 构建的模块，`ExportedFunctionDefinitions()` **只有** `log`，没有 `http_request` / `read_file` / `call_tool`
- 一个 import 了 `http_request` 的 guest，在未授 HTTP 时**实例化就失败**，错误信息人类可读（点名缺哪个函数），而不是 wazero 的原始 link error
- **授权了 HTTP 但域名不在 `AllowedHosts` → `http_request` 内部返回 DENIED**（第二道检查，Step 2 的要点 1）
- 授权了 FS 但路径越界 → 拒绝，且**必须复用 `port.WorkspacePathGuard`**，不要另写一套路径检查（它刚修掉 Windows 设备名与 ADS 两类绕过）

- [ ] **Step 2: 实现**。两条实现约束：
  1. **能力检查在 host 侧再做一次**。「没授权就不注册」只挡住能力维度，挡不住参数维度：授权了 `http` 不等于可以访问任意域名。
  2. **host function 之间不得互相调用**（wazero memory 访问会丢）。公共逻辑抽成普通 Go 函数。

- [ ] **Step 3: 变异验证**：把「未授权不注册」改成「注册但运行时返回 DENIED」→ 「实例化就失败」的测试应 FAIL。

- [ ] **Step 4: 提交**

---

### Task 4: 清单交叉校验与分步激活回滚

**Files:**
- Create: `internal/plugin/host/activate.go`
- Test: `internal/plugin/host/activate_test.go`

**Interfaces:**
- Produces:
  - `func Activate(ctx, ledger *lifecycle.Ledger, owner lifecycle.Owner, spec Spec) (*Plugin, error)`

**激活是多步的**：编译 → 实例化 → 读自描述清单 → 交叉校验 → 注册工具 → 登记 gateable。任一步失败都必须逆序撤销已完成的步骤。

> ⚠️ **这个 task 的测试极易假绿。** spike 里 `TestActivationFailureRollsBack` 一开始是假绿的：`activate()` 的所有失败点都在第一次 `ledger.Add` **之前**，回滚路径是死代码，把 `DisposeOwner` 整个删掉测试照样过。
>
> **修法**：把清单交叉校验放在**实例已入册之后**——这样「清单不符」成为一个在有东西可回滚时才发生的失败，回滚路径才真正被执行。参考 spike `deps/loader.go:540-560`。

- [ ] **Step 1: 写失败测试**

覆盖：
- happy path：激活后 `ledger.Snapshot()[owner]` 含预期条目
- **清单不符**（host spec 声明 `provides: [a]`，guest 自描述 `provides: [b]`）→ 返回 error，且 `ledger.Snapshot()` 中该 owner **为空**（回滚干净）
- 实例化失败 → 同样干净
- 错误信息点名「哪一步失败、host 说什么、guest 说什么」

- [ ] **Step 2: 实现**。自描述经 `plugin_invoke(OpManifest, nil)` 取，返回 `{"name","version","provides":[…]}` JSON。

- [ ] **Step 3: 变异验证（必做）**：删掉失败路径里的 `ledger.DisposeOwner(owner)` → 清单不符的测试必须 FAIL。**如果它仍然通过，说明测试是假绿的，回到 Step 1 重新设计失败点位置。**

- [ ] **Step 4: 提交**

---

### Task 5: 实例池与在途收敛

**Files:**
- Create: `internal/plugin/host/pool.go`
- Test: `internal/plugin/host/pool_test.go`

**Interfaces:**
- Produces:
  - `func newPool(size int, factory func() (*Instance, error)) *pool`
  - `func (p *pool) acquire(ctx) (*Instance, error)` / `func (p *pool) release(i *Instance)`
  - `func (p *pool) drain(ctx) error`

**两个核心不变量：**

1. **dead 实例绝不回池**。`WithCloseOnContextDone` 打断超时调用的方式是 close 整个模块——实例已报废。`release` 必须检查 `Dead()`，dead 的丢弃并按需新建。
2. **`drain` 等在途归零才返回**。用 `inflight sync.WaitGroup` + `closing atomic.Bool`：置位后 `acquire` 直接拒绝，`drain` 等 WaitGroup。

- [ ] **Step 1: 写失败测试**

覆盖：
- 并发 `acquire`/`release` 无竞争（`-race`，50 goroutine × 若干轮）
- **超时打断一次调用后，该实例不再被 `acquire` 返回**
- `drain` 期间的 `acquire` 返回错误，不是阻塞到超时
- `drain` 在有在途调用时**阻塞**，调用结束后返回
- 池满时 `acquire` 排队，ctx 取消时返回 ctx 错误

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证**：去掉 `release` 里的 `Dead()` 检查 → 「dead 不回池」测试应 FAIL。

- [ ] **Step 4: `-race` 验证**

```bash
go test ./internal/plugin/... -race -count=1
```

- [ ] **Step 5: 提交**

---

### Task 6: 工具贡献三件套

**Files:**
- Create: `internal/plugin/host/contribute.go`
- Test: `internal/plugin/host/contribute_test.go`

**插件注册一个工具是三件事，必须挂在同一个 `lifecycle.Owner` 上：**

1. `tool.RegisterOwned(ledger, owner, reg, desc, handler)` — 工具进注册表
2. `ledger.Add(owner, "gateable:"+name, …)` 包住 `toolauth.Contribute(...)` 的撤销
3. handler 内 `ctx = tool.WithCallOrigin(ctx, "plugin:"+name)` — 审计归因

**漏掉第 2 步 = per-agent `disabled_tools` 够不到这个工具 = 授权绕过。** 这个坑踩过一次（PR #80 的 C3）。

- [ ] **Step 1: 写失败测试**

覆盖：
- 贡献后：`Registry.Execute` 能调到、`toolauth.IsGateable(name)` 为 true
- `DisposeOwner` 后：`Execute` 返回 `ErrToolNotFound`、`IsGateable` 转 **false**、`ledger.Snapshot()` 为空
- **审计事件的 `Origin` 是 `plugin:<name>`**（用 `adapter.NewMemoryAuditLog()` 断言）
- 两个插件贡献同名工具 → 第二个 panic（注册表与 gateable 都是 fail-loud）

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证**：去掉第 2 步的 gateable 登记 → `IsGateable` 断言应 FAIL。

- [ ] **Step 4: 提交**

---

### Task 7: `call_tool` 双计数器（契约 1）

**Files:**
- Modify: `internal/plugin/host/hostfunc.go`（`call_tool` 实现）
- Test: `internal/plugin/host/calltool_test.go`

**契约 1**：插件发起的工具调用必须同时撞两个计数器。

| 计数器 | 归属 | 上限 | 越限行为 |
|---|---|---|---|
| per-plugin 递归深度 | 一次 `call_tool` 的调用链 | 3 | 立刻 DENIED |
| per-task 总预算 | **与模型共用** `toolNameGuard` | `toolLoopCap`(30) | 与模型撞顶同路径 |

**共用计数器是关键**：分开计数等于给插件开了一条绕过 agent 总预算的通道。用 Task 0 导出的 `domain.GuardedToolName` 保证两侧算的是同一个名字。

- [ ] **Step 1: 写失败测试**

覆盖：
- 插件调 `call_tool` → 该工具在 `toolNameGuard` 上**确实被计数**（模型此后调同名工具，剩余额度相应减少）
- 递归深度 4 → DENIED，且发 `plugin/call_failed{category:denied}` 事件
- **插件发起的调用照常经 `tool.Registry.Execute`**：权限被拒时插件同样拿到拒绝（不给插件开后门，I6）
- 审计里这些调用的 `Origin` 是 `plugin:<name>`，与模型发起的可区分

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证**：让 `call_tool` 用独立计数器 → 「共用额度」测试应 FAIL。

- [ ] **Step 4: 提交**

---

### Task 8: 端到端验收

**Files:**
- Test: `internal/plugin/host/e2e_test.go`

- [ ] **Step 1**：一个 `.wasm` 走完整生命周期：

```
Activate → 工具出现在 Registry.Descriptors() → 模型调用成功
  → DisposeOwner → 工具消失、ErrToolNotFound、gateable 转 false
  → ledger.Snapshot() 为空、池已 drain、模块已 Close
```

- [ ] **Step 2**：资源限制：
  - 纯计算死循环被 deadline 打断，实例标记 dead 不回池
  - 内存炸弹被 `WithMemoryLimitPages` trap（**用宽松超时**，否则 deadline 会冒充成「上限生效」——spike 里踩过，见 `abi/host/main.go:189-203`）

- [ ] **Step 3: 全量回归**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l .
```

再逐包串行跑 `-race`。

- [ ] **Step 4: 提交并开 PR**

---

## 交付后状态

- 手工指定的 `.wasm` 可挂载、被模型调用、干净卸载
- 未授权能力物理不可达（链接期）
- 插件发起的调用与模型共用熔断额度，审计可区分
- 失控插件被 deadline 与内存上限挡住，不拖死宿主

**尚未包含**：`plugin.Loader` 与 `plugins.yaml`（A4）、三态依赖收敛与热加载（A4）、任务边界生效（A4，即契约 4）、签名与分发与 GUI 同意流（A5）、多语言模板与 dev 模式（A6）。
