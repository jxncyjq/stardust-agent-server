# 插件扩展面设计（G4 spec）

**日期**：2026-08-29
**状态**：**已决策（2026-08-29）** —— 三个问题都已拍板，见第 6 节。本文是边界，不是实施计划；各期的实施计划另写。
**上游**：路线 `plans/2026-08-28-plugin-gap-closure-roadmap.md` 的 G4；与 dsh Cordis 的比对结论「装载侧已比 Cordis 完整，差距全在扩展面的宽度」。

---

## 1. 问题

插件今天只能做一件事：**贡献工具**。Cordis 的插件可以挂在工具执行链上（`pre-execute` 决策、`execute` 包装、`post-execute` 改写、`result` 观察），还能贡献 systemPrompt 段。Legion 宿主自己有审批、策略、护栏、提示词装配，但**插件参与不进去**。

这是与 Cordis 差距最大的一块，也是**安全含义最大的一块**：Cordis 的插件是可信的同进程 TS，Legion 的插件是不可信的 wasm。同一个能力，在两边不是同一件事。

## 2. 既有接缝（这次要接进去的地方）

```go
// internal/tool/registry.go —— Registry.Execute 的顺序
resolve → 补 RiskLevel → 校验 input schema
       → PermissionEnforcer.Check      // 越权
       → Policy.Decide                 // allow / deny
       → Guardrails.Before             // 路径守卫等
       → handler.Execute(带 descriptor.Timeout)
       → Guardrails.After
       → 审计

// internal/runtime/runtime.go —— 审批在更上面一层
type ToolGate interface {
    ShouldSuspend(ctx, task, calls, tools) (bool, error) // 本轮要不要挂起等审批
    Resolve(ctx, task, call, tools) (allow bool, err error) // 派发时这一条放不放
}

// internal/cognitive/core.go —— 提示词按命名块拼装
add("catalog", …) add("durable_memory", …) add("context_files", …)
stablePrefixLen := len(prompt)   // ← 稳定前缀边界，前缀缓存靠它
add("header", …) add("capability", …) add("prefetch", …) add("conversation", …)
```

三个事实决定了设计：

1. **拒绝有两个层次**：`Policy.Decide` 只有 allow/deny；「要求人工审批」住在 runtime 的 `ToolGate`，不在 registry 里。
2. **提示词有稳定前缀**：`stablePrefixLen` 之前的内容必须跨任务逐字节一致，否则 MaaS 侧的前缀缓存失效（这是 token 账单问题，不是性能洁癖）。
3. **工具调用有超时**：`descriptor.Timeout` 是这一条调用的预算，任何插入的环节都在花这份预算。

## 3. 提议的三个扩展点

### 3.1 观察点（read-only，先做）

工具调用完成后，把 `(call, result)` 通知给注册了观察点的插件。

- **不能改变任何东西**：返回值被丢弃，只有「有没有出错」被记为插件健康度（G1）。
- **在审计之后、返回之前**触发，且**不进入 result 的关键路径**：观察的失败不影响这次调用的结果。
- 单次通知有独立超时（建议取 `min(descriptor.Timeout/4, 200ms)`），超时=一次故障计数。
- **风险最低**：它读不到别的插件的东西，改不了结果，最坏情况是它自己被卸载。

### 3.2 决策点（只能收紧）

派发前征询插件：`allow` / `deny` / `ask`（要求人工审批）。

**唯一的合成规则：取最严。** `deny > ask > allow`。插件返回 `allow` **不是**授权，只是「我不反对」——它永远无法把宿主已经决定的 deny 或 ask 放宽。

落点有两种，取决于要不要 `ask`（见 §6 的决策 A）：

| 方案 | 落点 | 能表达 | 代价 |
|---|---|---|---|
| **A1 只做 deny** | 包一层 `tool.Policy`，与既有 policy 取交集 | allow / deny | 改动小，不动 runtime |
| **A2 deny + ask** | 还要接 `ToolGate`：插件的 ask 要能让本轮挂起 | allow / deny / ask | 要动 runtime 的挂起/检查点路径，面大 |

**失败语义（这是本设计最重要的一条）**：一次决策调用超时、trap、返回不可解码的东西 —— 怎么办？

- **fail-closed（拒绝这次调用）**：一个坏掉的插件能让 agent 停摆；但它同时计入健康度，连续故障到阈值就被自动卸载（G1 已有），所以停摆是**有界**的。
- fail-open（忽略这次决策）：一个被攻击者搞崩的插件 = 一个被绕过的安全控制。

**拍板：fail-closed。** 坏插件最多让若干次调用被拒，然后被 G1 的连续故障计数自动卸载——停摆是有界的；而 fail-open 会让「安全控制」变成「攻击者把插件搞崩就能关掉的安全控制」。这条「有界」必须写进手册，否则运维遇到批量拒绝时会不知道该等还是该动手。

### 3.3 提示词段

插件贡献一段文本，进系统提示词。

- 按 owner 计入 ledger，**卸载即撤回**（与工具贡献同一套机制）。
- **长度上限**（建议 2 KiB/插件，全部插件合计 8 KiB），超出**截断并记 Warn**——不是静默截断。
- **放在稳定前缀里**（它是部署级事实，不是任务级），代价必须写明：**插件挂载/卸载会让前缀缓存失效一次**。这是可接受的（挂载是低频操作），但要说出来。
- **必须带明确的边界标记**，例如：

```
--- plugin "legion-jira" (untrusted, provided by a deployment-installed plugin) ---
<插件文本>
--- end plugin "legion-jira" ---
```

理由：这是**不可信文本进入系统提示词**。模型必须能分辨哪些指令来自宿主、哪些来自一个被装上的插件；没有边界标记，一个插件就能写「忽略先前的指令」。

## 4. 授权模型：新的 grant 维度，不是新的 host 函数

既有能力（log/config/kv/http/fs/tool）门的是 **guest 调用宿主**。扩展点是反方向：**宿主调用 guest**。同一把锁开不了这扇门。

因此：

```jsonc
// plugin.json —— 插件声明它想挂哪些扩展点
"extensions": ["observe", "decide", "prompt"]

// plugins.json —— 部署逐项授权，与 capabilities 同规格
"grant": { "capabilities": ["log"], "extensions": ["observe"] }
```

规则与能力一致：**声明了不等于拿到**；`grant.extensions` 必须是声明集合的子集；未授权的扩展点**根本不会被调用**（宿主侧不注册，不是运行时判断）。GUI 同意流按扩展点逐项展示——「这个插件将能否决你的工具调用」是运维必须看见的一句话。

## 5. 明确不做

| 不做 | 为什么 |
|---|---|
| `execute` around 包装（Cordis 有） | 它能替换 `exec.signal` 与真实派发。把这个交给不可信代码，等于把工具调用的控制权交出去 |
| 放宽型决策（插件把 deny 变 allow） | 装一个插件就能松掉审批，是这套授权模型的反面 |
| 插件改写别的插件/宿主工具的结果 | 结果的改写权属于宿主；一个能改写别人结果的插件可以静默伪造工具输出 |
| 插件注册**服务**供别的插件消费（Cordis 的 Provider） | D1 待决策项，安全含义远大于本期 |

## 6. 需要拍板的三件事

### 决策 A：`ask`（要求人工审批）本期做不做？ —— **做（A2）**

拍板：**deny + ask 一起做**（推翻了本文原先「A1 先行」的建议）。

理由是产品侧的：只能 `deny` 的插件在真实场景里会被迫做二选一——要么放行一个它觉得可疑的调用，要么把它硬挡掉。「让人看一眼再决定」正是这类插件最该有的表达方式，砍掉它等于逼插件作者在两个都不对的选项之间挑一个。

因此 `ask` 要接进 runtime 的 `ToolGate`：插件的 `ask` 必须能让本轮挂起、进审批队列、并在审批通过后继续。实施计划里要单独处理的风险：

- 那条挂起/检查点路径上出过并发缺陷（`plugin/suspended` 那一轮），必须 `-race` 覆盖；
- 一次挂起要能说清**是谁要求的**（哪个插件、为什么），否则运维在审批界面上看到一个没有出处的待办；
- 插件的 `ask` 与宿主自身的 `Sensitive` 审批是同一条队列，不能变成两套并行的挂起来源。

### 决策 B：决策点失败时 fail-closed 还是 fail-open？

**建议 fail-closed**（拒绝该次调用 + 计入健康度 + 到阈值自动卸载）。反对意见是「一个坏插件能停摆 agent」——G1 的自动卸载把它变成有界的；而 fail-open 让「安全控制」变成「攻击者可以关掉的安全控制」。

### 决策 C：提示词段进不进稳定前缀？

**拍板：进稳定前缀。** 它是部署级事实，跨任务不变；代价是插件挂载/卸载各让前缀缓存失效一次（低频操作）。不进的代价更大：每个任务重发一遍，token 成本长期更高。

实施时必须做的一件事：把「插件段变化会使前缀缓存失效一次」写进手册的 token 一节，否则运维会把挂载后那一次缓存未命中当成 bug 查。

## 7. 分期建议

按 2026-08-29 的决策（A=做 ask、B=fail-closed、C=进稳定前缀）重排：

| 期 | 内容 | 依赖 | 估工 |
|---|---|---|---|
| **G4a 观察点** | ABI 新 op + `extensions` 声明/授权 + ledger 撤回 + GUI 展示 | 无 | 3-4 天 |
| **G4b 决策点 deny** | 包一层 `tool.Policy`，取最严，fail-closed | G4a 的授权面 | 4-5 天 |
| **G4c 决策点 ask** | 接 `ToolGate`：插件的 ask 让本轮挂起、进同一条审批队列、说清出处 | G4b；`-race` 必跑 | 1 周 |
| **G4d 提示词段** | 稳定前缀里的命名块 + 上限 + 边界标记 + token 文档 | G4a | 3-4 天 |

`ask` 排在 deny 之后而不是与之合并：deny 的合成规则（取最严）与授权面先立住，`ask` 才只需要解决「挂起来源」这一个新问题，而不是同时解决两件事。

每期照例：先写实施计划，再 TDD 执行，每个 task 变异核对。SDK（Rust + Go）与手册同期更新——一个没有 SDK 支持的扩展点等于没有。

## 8. 验收（对 G4a-c 共通）

- 每个扩展点都有「未授权则根本不会被调用」的测试；
- 决策点有「插件不能放宽既有 deny」的测试；
- 提示词段有「卸载即撤回」「超长截断并 Warn」「边界标记存在」的测试；
- 观察点有「返回值被丢弃、失败不影响结果」的测试；
- 每期至少一次 `go test ./...` 与 `-race ./internal/...`。
