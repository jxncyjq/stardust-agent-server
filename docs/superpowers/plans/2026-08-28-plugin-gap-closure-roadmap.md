# 插件系统补齐差距 · 分期路线

**日期**：2026-08-28
**上游依据**：设计文档 `docs/design/architecture/legion-plugin-system.md`（docs 仓）§6.9 / §8 / §9；与 dsh Cordis 的逐项比对（2026-08-28）
**当前基线**：P0 / P0.5 / P1 / P2-A4a / P2-A4b / P3-A5a / P3-A5b / P3-A5c / GUI 同意流 **全部已交付并合入 master**；`plugin_example/` 与参考手册已交付。本路线图的 **G1 / G2 / G3 / G5 / G6 / G7 已交付并合入 master**；**G4 拆成 a-d 四步，G4a/G4b/G4c 已交付**，G4d（提示词段）待做（见下）

这份文档只做一件事：把「与 Cordis 比对后确认还缺的东西」排成可执行的期次。**每一期开工时另写自己的 TDD 实施计划**（仓内惯例，见 `plans/2026-08-2x-*.md`），本文件只定范围、顺序、验收与边界。

---

## 一、比对结论（决定顺序的依据）

Cordis 是**进程内 TS 插件内核**——dsh 的一切（shell / llm / session / fs）都是插件，彼此 `inject`，全部可信同权限。Legion 的插件面是**沙箱化的第三方扩展点**：宿主自身能力仍是原生 Go，插件是不可信代码。

因此比对不是「把 Cordis 搬过来」，而是看**同一个问题两边各解到什么程度**：

| 维度 | Cordis | Legion | 结论 |
|---|---|---|---|
| 可回卷效果 / 卸载 | `ctx.effect()` | `lifecycle.Ledger` + owner | 已对齐 |
| 目标态加载 | `cordis.yml` + loader | `plugins.json` + `Apply` | 已对齐（无嵌套 group，够用） |
| 依赖收敛 | `inject` 等待服务 | `requires` + suspended/resumed 级联 | 已对齐（简化三态） |
| 激活失败回滚 | fiber 回滚 | 分步激活 + 回滚旧实例 | 已对齐 |
| 隔离与准入 | 无（可信代码） | WASM 沙箱 + 链接期能力门 + 签名 + 摘要 + 装授分离 + GUI 同意流 | **Legion 领先** |
| 扩展面宽度 | 拦截点 4 个 + guard + prompt 段 + service 注册 | **只能贡献 tool** | **最大差距** |
| 作者体验 | 一个 TS 模块 + zod Config | 手写 ABI，无 SDK，config 无 schema | 差距 |
| 运行期健康 | fiber 报错即停 | 无故障计数、无自动卸载、泄漏无事件 | 差距 |

## 二、期次

顺序原则：**先补「没有它就没人写插件」和「没有它坏了没人知道」，再做最大的扩展面**。G1 与 G2 互不依赖，可并行。

| 期 | 内容 | 依赖 | 估工 | 产品决策 |
|---|---|---|---|---|
| **G1 运行期健康度** | 调用失败分类、故障计数、超阈值自动卸载、`plugin/unload_leaked` | 无 | 2-3 天 | 否 |
| **G2 Guest SDK** | `pkg/legionplugin`（Go/wasip1）+ `sdk/rust` crate | 无 | 3-5 天 | 否 |
| **G3 插件配置 schema** | 清单声明 `config_schema`，加载期校验 | 无 | 2-3 天 | 否 |
| **G4 扩展面：拦截点与 prompt 段** | 插件参与工具执行管线与提示词装配 | G1（分类）、G2（SDK 要同步扩） | 1-2 周 | **是**（先写 spec） |
| **G5 状态实时推送** | `GET /v1/plugins` 之外加事件流，GUI 免手动刷新 | 无 | 2 天 | 否 |
| **G6 缓存治理** | eviction API + 不可信包落缓存后的清理 | 无 | 1-2 天 | 否 |
| **G7 密钥吊销** | keyring 吊销列表 + 装配期与 reload 期校验 | 无 | 3-4 天 | 否 |
| **待决策 D1** | 服务接缝（插件提供可被别的插件消费的抽象能力） | — | 大 | **是** |
| **待决策 D2** | scope 遮蔽 / restriction（per-agent 同名工具替换） | — | 中 | **是** |

---

### G1 运行期健康度

**为什么**：设计文档 §6.9 定义了五类失败与「连续故障超阈值自动卸载」，一条都没实现；§8 的五个事件里 `plugin/unload_leaked` 也没发。今天一个反复 trap 的插件会一直挂着，一次泄漏的 wasm runtime 悄无声息。

**范围**：
- guest 调用失败按 `timeout` / `trap` / `abi` 分类（`denied` 已有，单独计数不计入故障）；
- 每个已挂载插件维护**连续**故障计数，成功即清零；
- 超阈值 → 自动卸载并发 `plugin/unloaded`（reason=`health`），`plugins status` / `GET /v1/plugins` 说明是健康度卸载；
- drain 超时留下在途调用 → 发 `plugin/unload_leaked`（含在途数与等待上限）。

**不做**：自动重试（重试一个 trap 的插件通常只是重复 trap，设计文档已写明）、自动重新加载、按时间窗的滑动计数（YAGNI，先做连续计数）。

**落点**：`internal/plugin/host/instance.go`（错误分类）、`contribute.go`（事件）、`internal/plugin/loader/loader.go`（计数与卸载）、`internal/plugin/host/pool.go`（在途计数）、`internal/config/config.go`（阈值）。

**验收**：`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 空；四类失败各有测试；阈值边界（阈值-1 不卸载、阈值卸载）有测试；`denied` 不计入有测试。

**实施计划**：`plans/2026-08-28-plugin-runtime-health.md`（本次已写）。

---

### G2 Guest SDK

**为什么**：设计文档 §6.10 承诺的 `pkg/legionplugin` 至今不存在，`pkg/` 目录都没有。今天写插件要自己搞 alloc/free、指针打包、host 函数声明、JSON——`plugin_example/` 就是这份手工活的完整证据。作者体验直接决定「有没有人写插件」。

**范围**：
- `pkg/legionplugin`（标准 Go，`GOOS=wasip1 GOARCH=wasm` 可编译）：导出四件套的默认实现、op 分发、`RegisterTool(name, handler)`、七个能力的 typed 包装、`ToolResult` 构造；
- `sdk/rust`（crate `legion-plugin`）：同一套 API 的 Rust 版，`plugin_example/guest` 改为依赖它（示例从 200 行手工降到几十行）；
- 两侧都提供「能力未授权时不 import」的编译期开关（Go: build tag；Rust: feature），与 `plugin.json` 的 `capabilities` 一一对应。

**不做**：其它语言的 SDK；SDK 内置 JSON schema 生成；包管理与版本协商。

**验收**：两个 SDK 各有一个最小插件示例，能通过 `plugin_example` 那套真宿主测试；`plugin_example/guest` 迁移到 Rust SDK 后闭环仍通（install → grant → serve → `state:"loaded"`）。

---

### G3 插件配置 schema

**为什么**：Cordis 的 `Config` 是 zod schema，既是类型也是运行时校验，坏配置在加载期就报错。Legion 的 `config` 是 `json.RawMessage` 原文直传 guest，宿主一个字都不校验——配置写错要等到运行时在 guest 里炸，而 guest 那边报错能力最弱。

**范围**：`plugin.json` 增加可选 `config_schema`（JSON Schema 子集）；`manifest.ParsePlugin` 校验 schema 本身合法；加载期用它校验 `plugins.json` 里该 entry 的 `config`，不通过则该条 `failed` 并点名字段；`GET /v1/plugins` 与 GUI 呈现校验错误。

**不做**：schema 的默认值填充与迁移；跨版本 schema 演进策略。

**验收**：坏配置在**加载期**被拒并点名字段；无 `config_schema` 的插件行为完全不变（向后兼容有测试）。

---

### G4 扩展面：拦截点与 prompt 段

**为什么**：这是与 Cordis 差距最大的一块，也是「插件能不能做横切能力」的分水岭。Cordis 插件可以挂 `tools/pre-execute`（waterfall 策略）、`tools/execute`（around 包装）、`tools/post-execute`（改写结果）、`tools/result`（只读通知），外加 `tools.guard()` 单调守护，还能贡献 systemPrompt 段。Legion 宿主自己有审批与模式策略，但**插件参与不进去**：今天插件只能贡献 tool。

**拆期与进度**（spec 见 `2026-08-29-plugin-observe-extension.md`）：

| 步 | 内容 | 状态 |
|---|---|---|
| G4a | 只读观察点（`observe` 扩展点）：`perm.Extensions`、`tool.Observer` seam、op 2、grant/HTTP/视图、两个 SDK、激活期交叉校验 | **已交付** |
| G4b | 决策点 deny：注册表决策者接缝（放在 enforcer/policy 之后 = 只能收紧），多插件取最严，失败 fail-closed，预算 min(超时/4,200ms) | **已交付** |
| G4c | 决策点 `ask`：接进 `runtime.ToolGate` 的既有审批队列，必须说清是哪个插件要的；`-race` 必跑 | 待做 |
| G4d | prompt 段：稳定前缀块、长度上限、`--- plugin "x" (untrusted) ---` 边界标记、token 说明 | 待做 |

G4a 落下来的三条不可违反的事实：**未授权 = 不存在的注册**（不是运行期 if）；**授权是子集**（能力才是全等）；**授权了而 guest 没实现 = 激活期拒绝**（否则宿主会在每次工具调用上回调一个只会答 unsupported op 的 guest，静默且永远）。

**范围**（需先写 spec，因为每一条都有安全含义）：
- 插件可注册**只读观察点**（对应 `tools/result`）——最安全，先做；
- 插件可注册**决策点**（对应 `pre-execute`）：返回允许/拒绝/要求审批，多个插件的决策取**最严**（不可放宽），杜绝「装一个插件把审批放松掉」；
- 插件可贡献 **prompt 段**，按 owner 计入 ledger，卸载即撤回；段有长度上限并计入 token 预算；
- ABI 扩展：新增 op（`OpOnEvent` 之类）与对应的 guest 侧回调注册，SDK 同步扩。

**不做**（安全底线）：`execute` around 包装（能替换 signal 与真实派发，不可信代码不给）；放宽型决策；插件改写别的插件的结果。

**依赖**：G1（失败分类要能覆盖新的调用路径）、G2（SDK 必须同步，否则没人写得出来）。

**验收**：新增扩展点的每一条都有「插件不能放宽既有策略」的测试；prompt 段有长度上限与卸载即撤回的测试。

---

### G5 状态实时推送

**为什么**：GUI 今天只在打开面板时取一次 `GET /v1/plugins`，之后靠手点刷新。多人运维或后台收敛时，屏幕上是旧状态。

**范围**：把 `plugin/loaded|unloaded|suspended|resumed|activation_failed|unload_leaked` 六个事件接到既有的 SSE 桥（`legionAgentGUI/sse_bridge.go` 已有一套，浏览器视图在用），GUI 插件面板订阅并就地更新行。

**不做**：轮询；WebSocket 另起一套；事件回放/补偿（面板打开时先取一次全量即可）。

**验收**：GUI 不点刷新也能看到 grant 后收敛完成的状态变化；断线重连后先全量再增量的测试。

---

### G6 缓存治理

**为什么**：仓内**无任何 eviction API**。422 之后不可信的包永久留在 `cache/sha256/<digest>/`，运维只能手删目录——这既是磁盘问题也是安全问题（坏包一直躺在可读位置）。

**范围**：`agent plugins cache list|remove <digest>|prune`；不可信包在验签失败后立即从缓存移除（或标记隔离）；缓存容量上限与最久未用淘汰。

**不做**：跨主机共享缓存；缓存的完整性周期性重扫。

**验收**：422 之后再 `List` 不会再报 `load_failed`（因为坏包已移除）；`prune` 不会删掉清单仍在引用的 digest（有测试）。

---

### G7 密钥吊销

**为什么**：签名体系今天只有「信任集里有没有这把钥匙」，没有「这把钥匙曾经可信、现在不可信」。一把泄漏的私钥签过的包会继续通过验签。

**范围**：keyring 增加 `revoked` 列表（key id + 吊销时间）；装配期与 `reload` 期都校验；已挂载但由被吊销钥匙签发的插件在下一次收敛时卸载并点名。

**不做**：透明日志（Sigstore 那一套）；在线吊销查询（OCSP 式）。

**验收**：吊销后 `reload` 会卸载对应插件并给出可操作的错误；吊销列表本身的解析错误 fail-loud（不降级为「不吊销」）。

---

### 待决策 D1：服务接缝

Cordis 的三角色契约（Service Definition / Provider / Consumer）让「换一个 provider 换掉整条行为」成为可能。Legion 的插件之间只有 `call_tool` 一条水平通道，插件**不能提供被别的插件消费的抽象能力**。

要不要做，取决于一个产品问题：**是否允许第三方插件成为宿主能力的实现方**（例如让插件提供 `ctx.web` 的一种实现）。允许则需要一整套「谁能替换什么」的授权模型，安全含义远大于工具贡献。**在回答这个问题之前不要动手。**

### 待决策 D2：scope 遮蔽 / restriction

Legion 今天有 per-agent `disabled_tools`（禁用清单），但没有 Cordis 的「同名工具在某 agent 内被替换」。要不要做，取决于是否需要 per-agent 的工具变体与人格化。技术上不难，难在语义：遮蔽会让「模型看到的工具」与「清单里的工具」不再一一对应，排查成本上升。

---

## 三、明确不做（沿用设计文档 §10）

Fiber 树 / Context 层级覆盖、文件 watcher 式 HMR、OCI registry 传输、插件市场与 `search`、用 `inject` 声明充当安全隔离。这些都不是「还没做」，是**决定不做**。

## 四、每期通用验收门槛

- `go build ./... && go vet ./... && go test ./...` 全绿，`gofmt -l .` 为空；并发相关任务额外 `go test -race`；
- 每个 task 至少跑一次 `go test ./...`（不是包子集）——`TestOpenAPIGolden` 住在 `internal/compat` 却覆盖 `internal/server`，按包跑会漏；
- 错误路径必须有测试断言「确实返回 error」，不得只测 happy path；
- 每个 task 做变异验证：把核心机制改坏，确认测试确实 FAIL，失败输出留在报告里，然后还原并用 `git status` 核对；
- 改动触及 `plugin_example/` 的包时，`plugin_example` 的四个测试必须仍绿（它们钉住清单与 wasm 的配套关系）。
