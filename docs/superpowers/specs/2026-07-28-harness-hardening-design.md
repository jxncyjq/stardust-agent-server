---
title: legion harness 加固设计（防循环失控 + 上下文压缩 + 迭代上限 + 工具并发）
date: 2026-07-28
status: approved
scope: legionAgent runtime 执行层 — 4 个独立组件，各自独立 PR，按优先级落地
---

# legion harness 加固设计

## 动机

runtime.debug 探针实测：5 字追问「员工在上海」跑 13 轮 tool-loop、同一保险手册被整篇重读约 5 次、累计 166k 字符 ≈ 95.7k token。三方 harness 对比（legion / hermes-agent / claw-code）显示 legion 在**挂起恢复（mid-turn checkpoint）**与 **fail-loud** 上领先，但在**循环失控防护**与**上下文压缩**上明显落后。本设计补齐 4 个洞，均借鉴 hermes（guardrail signature、iteration budget、细粒度并发）与 claw（边界安全 compaction）的成熟做法。

PR #58（read_file 重读内容层提醒）已补内容层；本设计补**循环层 + 上下文层**。

## 全局约束（4 组件共守）

- **Fail-loud 铁律**：新增组件的错误一律 `%w` 上抛或结构化记录，禁静默吞/兜底零值；LLM 摘要调用失败不得静默丢弃历史。
- **152-incident 硬约束**：多轮 append-only、重复对模型可见的既有语义不破坏；新增的重复守卫只增强、不折叠成字节相同（见 memory `legion-tool-loop-multiturn`）。
- **prompt-cache 稳定前缀**：`basePrompt` 作为 StablePrefixLen 断点的语义必须保留；compaction 改写的是 basePrompt **之后**的对话体，不动稳定前缀（否则每次压缩=全缓存失效）。
- 门禁：`go build/vet/test ./...` 全绿、`gofmt -l .` 空；错误路径有测试断言。
- 4 组件**独立 PR**，互不阻塞，按下方优先级序落地。

## 现状关键 seam（实测确认）

- `internal/runtime/messages.go`：`repeatWarnStreak=3`/`repeatAbortStreak=8`，`repeatedCallStreak` 只数**连续**相同（`callsKey`=name+args 的哈希）。
- `internal/config/config.go:481`：`UnlimitedToolRoundsCap=1000`；`normalizeMaxToolRounds(0)→1000`（serve 路径用户设 0 即得 1000 高帽，非真无限）。
- `internal/runtime/runtime.go`：`runToolLoop` 主循环 `for round<maxToolRounds && ToolCalls>0`；`executeToolCalls` **串行** `for _,call:=range calls`。
- `internal/cognitive`：`Compressor` 接口 + `NoopCompressor`（压的是系统 context 块，**非**对话体——与本设计的 conversation compaction 是两回事，勿混）。

---

## 组件 1（P1，直击爆炸）：非连续重复守卫（ToolCallSignature）

**问题**：`repeatedCallStreak` 只拦**连续**相同调用；A→B→A 交替重读绕过（13 轮任务正是如此）。

**决策（已定）**：先警告后硬停。

**机制**：任务级签名计数器 `repeatGuard`（`map[signature]int`，signature = 复用现有 `callsKey`(name+args)）。每轮对每个 pending call 计其**任务内累计出现次数**（非连续）：
- 达 `repeatWarnCount`（如 4）→ 注入一条警告 user turn（仿 `repeatWarnStreak` 文案，指出「你已第 N 次以相同参数调用 X，结果不会变，改用 search_content 或直接作答」）。
- 达 `repeatAbortCount`（如 6）→ 硬停循环（`loopCut=true`，走现有 generateNoTools 收尾），publish `tool_loop_broken` 事件 + Warn 日志（复用现有断循环路径）。

**落点**：`messages.go` 加 `repeatGuard` 类型 + 常量；`loopState` 持有一个 `*repeatGuard`（任务级）；`runToolLoop` 在现有 streak 检测旁并列调用签名计数，警告/硬停复用现有 `appendUser`/`tool_loop_broken` 分支。

**Fail-loud**：签名计数无外部依赖不产 error；硬停必 publish 事件（现有路径已 fail-loud）。

**测试**：A→B→A→B... 交替同调用达阈值→警告轮出现；继续→硬停 + 事件；不同参数不误计；与现有连续 streak 并存不冲突。

---

## 组件 2（P2，治本）：对话上下文压缩（LLM 摘要 compaction）

**问题**：`NoopCompressor` 是空的；对话体每轮重发，render 只折叠超 16k 的旧 tool 输出，无摘要。多轮累积无根治。

**决策（已定）**：LLM 摘要 compaction，tool-pair 边界安全，保留最近 N 轮。

**机制**（借鉴 claw `compact_session` + hermes ContextEngine）：
- 新增 `conversationCompactor`（runtime 层，独立于 cognitive.Compressor）。触发：`runToolLoop` 累计 `promptTokens` 超阈值（如 60k，可配 `runtime.compact_token_threshold`，0=关）。
- 压缩动作：把 `basePrompt` **之后**、最近保留窗口（`preserveRecentTurns`，如 6）**之前**的对话消息，经一次 LLM 摘要成一条 `RoleUser` 的 `[对话摘要]` 消息，替换原多条。**保留 basePrompt 稳定前缀不动**（护 prompt-cache）。
- **边界安全**：摘要切点不得落在 assistant tool_calls 与其对应 tool result 之间（否则 provider 400）——walk-back 到完整配对边界（claw 同款）。
- 摘要 LLM 调用复用 `r.maas`（或单独轻量 profile）；每任务压缩次数上限（如 3）防抖。

**Fail-loud**：摘要 LLM 调用失败 → **不静默丢历史**：返回 error 中止本次压缩、保留原始对话继续（记 Warn），绝不把「压缩失败」当成「历史清空」。切点计算失败同理 fail-loud。

**测试**：超阈值触发一次压缩、消息数下降但语义保留（摘要含关键事实）；切点不切断 tool 配对（构造 tool_use/result 相邻场景断言边界 walk-back）；basePrompt 未被动；LLM 失败→保留原对话+返回/记录 error 不清空；压缩次数上限生效。

---

## 组件 3（P3，收口开关）：迭代上限有限化 + 临近警告

**问题**：`normalizeMaxToolRounds(0)→UnlimitedToolRoundsCap=1000`，serve 路径用户设 0 = 1000 高帽，形同放任。

**决策（已定）**：有限默认 + 临近警告；显式大值仍可放宽。

**机制**：
- `config.normalizeMaxToolRounds`：`0 → 合理有限默认`（如 `DefaultToolRoundsWhenUnset=12`，新常量）而非 1000。**保留显式放宽**：用户想要更多轮就**显式**写大数字（如 `max_tool_rounds: 100`），0 不再等于「几乎无限」。更新注释与 example 说明「0=默认 12，非无限；要放宽写具体数」。
- **临近警告**：`runToolLoop` 当 `round >= maxToolRounds - warnMargin`（如剩 2 轮）时注入一条提示「工具轮数将达上限，请尽快收尾作答」，让模型主动收敛而非被 generateNoTools 硬截。

**Fail-loud**：纯计数，无吞错。注意与组件 1 的硬停解耦（两条独立终止路径，都走 generateNoTools 收尾）。

**测试**：`normalizeMaxToolRounds(0)==12`、`(100)==100`、`(-1)` 按既有语义；临近上限注入提示轮；example 配置注释更新。

---

## 组件 4（P4，最低优先，只提速）：只读工具轮内并发

**问题**：`executeToolCalls` 串行；读多份文档时逐个等。**只降延迟不省 token**，故最低优先。

**决策（已定）**：纳入 spec，列最低优先（可最后做/可选）。

**机制**（借鉴 hermes 分段规划）：
- 并发白名单 `parallelSafeTools`（只读：`read_file`/`search_content`/`list_files`）。同一轮的 pending calls 分段：全白名单子集 → `errgroup` 并发（bounded，如 `min(len, 4)`）；含写/shell/未知工具 → 该段串行；**保序**：结果按原 call 顺序回填（tool result 顺序=call 顺序，护协议）。
- 写类工具（write_file）路径重叠则退串行（本期可简化为「任何写/非白名单一律串行」，路径重叠判定作为后续增强，YAGNI）。

**Fail-loud**：并发中任一 call error → 收集并 `%w` 上抛（现有 executeToolCalls 错误契约不变）；ctx 取消传播。审批门控（ToolGate）语义不变——并发只作用于已放行的只读工具。

**测试**：多只读 call 并发执行（结果保序）；含写 call 的批退串行；并发中一个 error 正确上抛；ToolGate/审批不被并发绕过。

---

## 交付顺序与拆分

| 优先级 | 组件 | 独立 PR | 依赖 |
|---|---|---|---|
| P1 | 非连续重复守卫 | PR-A | 无 |
| P2 | 对话 LLM compaction | PR-B | 无（与 P1 独立） |
| P3 | 迭代上限有限化 | PR-C | 无 |
| P4 | 只读工具并发 | PR-D | 无 |

四者互不阻塞，可任意序；建议按 P1→P2→P3→P4（爆炸根治优先，提速最后）。每组件一个 spec→plan→PR 循环，或本 spec 拆 4 个 plan。

## 非目标

- 不改挂起/恢复（mid-turn checkpoint）与 fail-loud 既有强项。
- 不引入 hermes 式完整 IterationBudget（refund/父子独立预算）——P3 只做有限默认，YAGNI。
- P4 不做 hermes 的完整路径重叠判定（写类一律串行即可，后续再增强）。
- 不动 cognitive.Compressor（系统 context 块压缩是另一回事，本设计不碰）。

## 相关

memory：[[legion-token-multiround-debug-probe]]（爆炸取证 + 成本结构）、[[legion-tool-loop-multiturn]]（152 事故 = 组件 1/2 的硬约束）、[[legion-config-resolution-roots]]。
参考库对标：hermes-agent（guardrail signature/iteration budget/分段并发）、claw-code（compaction tool-pair 边界安全）。
