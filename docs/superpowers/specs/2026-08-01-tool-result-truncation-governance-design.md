---
title: 工具结果截断 / 重试治理设计
date: 2026-08-01
status: draft
tags: [runtime, truncation, tool-loop, token, fail-loud]
related:
  - "[[2026-07-31-web-search-tools-design]]"
---

# 工具结果截断 / 重试治理设计

## 1. 背景（事故取证）

会话 `session-1785390610840955100` 的 task `gui-task-1785563819850553400`（提问"查历史走势"）实测：

- 耗时 **291s**，输入 **831.7k** token（缓存 752.8k / 命中 90.5%），输出 24.6k
- 291s 窗口内 **60 次工具调用**：`fetch_url`×24、`list_files`×10、`read_file`×9、`web_extract`×9、`search_content`×8；`model_inference_completed` 汇总 1 条
- 最终**失败**，agent 自述："系统反复压缩了工具输出，我已多次尝试不同的 K 线数量参数（5/8/10/.../120 日）和不同工具（fetch_url / web_extract），返回内容都被压缩截断，无法逐日解析完整 K 线明细"

取证方法：`conversation_turns` 表只存最终对账（每 task 2 turns），中间 tool-loop 轮次不落此表；真相在 `audit_events` 表按时间窗聚合（`tool_executed` / `model_inference_completed`）。db = `legionAgent/agent.db`。

## 2. 根因

`runtime.go:894 truncateText(text, maxChars=4000)` 把工具结果**无声硬截到 4000 rune**，截断文本仅追加 `…[truncated N chars]`：

1. 抓 K 线大数据 → runtime 硬截 4000 rune
2. 截断文本**不说明这是硬截（非数据/参数问题）、不给全文位置、不给取回方式**
3. agent 误判为"自己要的数据太多" → 换 K 线数 / 换工具**盲目重试**
4. 循环 **60 次无熔断拦截**（`MaxToolRounds=4` 默认值未对此生效）
5. 每轮重发整个 conversation（多轮累积）→ input 累积到 831.7k → 全失败

**结论**：非 token 编码问题（缓存命中 90.5% 健康），是**截断无自我描述 → agent 盲目重试 → 循环无熔断**三重缺陷叠加。

参考方案：hermes-agent（详见 `scratchpad/hermes-truncation-retry.md` 调研）——截断即落盘 + footer 明确取法、三层防御、压缩尝试上限、工具循环熔断（same-tool warn@3/halt@8 + loop cap 50/turn）。

## 3. 目标与范围

在 **runtime 统一截断层**集中治理，分三阶段（单 spec，可独立交付）：

- **P0** 截断自我描述：截断文本明确"硬截非错误 + 原量 + 全文位置 + read_file 取法"
- **P1** 通用大结果落盘分页：超阈值落盘、in-context 留 preview + footer、read_file 取回、防 persist→read 环、web_extract 重构并入
- **P2** 循环熔断 + 预算缩放：same-tool 失败熔断、每轮 loop cap、预算随上下文窗口缩放

### 不做（YAGNI）
- 不做 LLM 摘要压缩（hermes 的 context_compressor 那套）——落盘分页已足够，摘要引入不确定性
- 不做针对结构化数据（JSON）的语义感知截断——字符级 head 截断 + 全文落盘即可
- 不引入 per-URL 落盘粒度（web_extract 重构后按整个 ToolResult 落盘）

## 4. 架构

治理集中在 runtime 层（`internal/runtime`）：

| 组件 | 位置 | 职责 |
|------|------|------|
| 截断+落盘逻辑 | `runtime.go` / `messages.go appendToolResults` | 超 preview 阈值 → 落盘 + preview + 自我描述 footer |
| 落盘 writer | 新增 `internal/runtime/toolcache.go`（或 tool 包复用） | 落 `toolRoot/.stardust/tool_results/`，过 `guard.Check` |
| toolRoot 注入 | Runtime 结构体 + conversation | Runtime 持有 sandbox root，传入截断逻辑 |
| 循环熔断控制器 | 新增 `internal/runtime/toolguard.go` | 纯函数式：跟踪 same-tool 失败 / loop cap 计数，返回决策 |
| web_extract 重构 | `internal/tool/webextract.go` | 移除工具层落盘，返回完整内容交 runtime 统一处理 |

**单位约定**：所有阈值以 **rune**（`[]rune` 长度）计，与现有 `truncateText` 一致。

## 5. P0 — 截断自我描述

改 `runtime.go:894 truncateText`（无 P1 落盘时的纯自我描述版）：

```
──────── [输出被硬截断] ────────
这是硬截断（上下文预算限制），非数据/参数问题——换参数或换工具重试不会有帮助。
显示 <preview> / 共 <total> 字符。
```

P1 落盘启用后，footer 追加全文位置 + `read_file` 命令（见 §6）。核心：消除 agent"以为参数错→重试"的动机。

## 6. P1 — 通用落盘分页

- **触发**：`appendToolResults` 中工具结果 `len([]rune(content))` > `previewChars`（默认 **3000**）→ 落盘全文，in-context 只留 preview（前 3000 rune）+ footer。
- **落盘位置**：`toolRoot/.stardust/tool_results/{tool}-{sha256(callID)[:10]}.md`（通用命名，取代 web_extract 的 web_cache）。
- **toolRoot 解析**：= sandbox root（`task.WorkingDir` 或 `ContextFiles.Root`，非 home），Runtime 持有并传入截断逻辑。落盘路径过 `port.WorkspacePathGuard.Check`（**guard.Check 先于 MkdirAll**，复用 web_extract 修复 F1 的顺序）。
- **footer**（复用 web_extract 已验证的读回契约：相对 toolRoot、forward slash）：
  ```
  ──────── [输出被硬截断] ────────
  这是硬截断，非数据/参数问题——重试不会有帮助。全文已完整保存。
  显示 <preview> / 共 <total> 字符。
  取回剩余：read_file path="<rel>" offset=<preview>
  ```
- **防环（关键）**：`read_file` 工具自身的结果**豁免落盘/截断**（否则读回落盘内容又落盘 = 无限环）。判定 `call.Name == "read_file"`（对标 hermes read_file 阈值 pin=inf）。
- **落盘上限**：单文件 cap 2MB（复用 `webExtractCacheFileMax`），rune 安全切割。
- **web_extract 重构**：移除 `truncateAndCache` / `writeExtractCache` / `sanitizeSlug` 落盘逻辑与 `char_limit` 参数；`extractOne` 返回完整渲染内容（**保留** `stripBase64Images` + URL 密钥阻断 + SSRF 校验），交 runtime 统一层落盘。相关测试同步调整。

## 7. P2 — 循环熔断 + 预算缩放

新增 `internal/runtime/toolguard.go`（纯函数式控制器，跟踪计数返回决策，副作用由 runtime 接线），在 `registry.Execute` 前后接线（或 runtime tool-loop 内）：

- **同工具反复失败**：`same_tool_failure_warn_after=3` / `halt_after=8`。**默认只 warn**（`hard_stop_enabled=false`），halt 需 config 显式开。warn 文本行动导向："这像循环，先看最近错误、验证假设，别原样重试"。
- **每轮 loop cap**：单 agent-loop 内单工具总调用上限 `loop_cap=30`（那次事故 fetch_url 调 24 次，30 留余量）。达上限返回合成结果："本轮已调用 <tool> 30 次，像失控循环，用现有结果作答。"loop cap **无视 hard_stop_enabled 一律生效**（这是硬顶）。
- **预算随上下文窗口缩放**（P2 子项）：`previewChars` / 落盘阈值按模型 context 窗口比例缩放，带 floor（如 window<64k 时按比例缩小，floor 1500 rune）。
- **计数器每 agent-loop 重置**（per-loop 非全会话累计）。

## 8. 配置项（`RuntimeConfig` 扩展）

```yaml
runtime:
  # 现有
  max_tool_rounds: 4
  # 新增
  tool_result_preview_chars: 3000      # P1 preview 阈值(rune)，超此落盘
  tool_result_cache_dir: ".stardust/tool_results"  # 相对 toolRoot
  tool_loop_cap: 30                    # P2 单工具每轮硬上限，0=禁用
  tool_same_failure_warn_after: 3      # P2
  tool_same_failure_halt_after: 8      # P2
  tool_hard_stop_enabled: false        # P2 halt 默认关(只 warn)
  tool_budget_scale_by_window: true    # P2 预算窗口缩放
```

## 9. 错误处理（fail-loud 铁律）

- 落盘失败 → **不静默**：`slog.Warn` 记录（tool/path/error）+ footer 降级说明（"全文无法落盘，请缩小请求或分步获取"）；sandbox 逃逸（`errors.Is(err, port.ErrPathOutsideWorkspace)`）→ 硬失败（复用 web_extract F2 模式）。
- 熔断/loop cap 的计数与决策边界走 fail-loud，合成结果明确标注是熔断产物。
- 截断本身不是错误（正常预算行为），但 preview+footer 必须完整（footer 被二次截断会重演事故——footer 长度须计入预算，preview 留足空间，对标 `builtin.go readFilePageBudget` 的做法）。

## 10. 测试计划

`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 空。

- **P0**：`truncateText` footer 含"硬截断"声明 + preview/total 字符数；footer 不被自身预算挤掉。
- **P1**：大结果落盘 + 同 registry `read_file(rel, offset)` 端到端读回（那个坑的断言）；`read_file` 结果**不被二次落盘**（防环断言）；落盘失败 → Warn + 降级；sandbox 逃逸 → 硬失败；web_extract 重构后仍返完整内容且经 runtime 落盘、base64/密钥处理保留。
- **P2**：same-tool 失败计数触发 warn（默认）/ halt（开 hard_stop 时）；loop cap 达 30 返回合成结果；计数器 per-loop 重置；预算窗口缩放。
- 回归：现有 `TestConversationTruncatesOversizedToolResult`、`messages_test.go`、web_extract 全测适配新行为。

## 11. 关键约束速查（踩坑点）

1. footer 必须自我描述（硬截声明 + 全文位置 + read_file 命令），这是防盲目重试的第一性原理。
2. 落盘必须在 sandbox root 内（toolRoot），`guard.Check` 先于 MkdirAll，read_file 才能读回。
3. `read_file` 结果豁免落盘/截断，防 persist→read→persist 无限环。
4. footer 长度计入预算，preview 留足空间，否则 footer 被二次截断重演事故。
5. loop cap 无视 hard_stop 一律生效；same-tool halt 默认关（只 warn）。
6. web_extract 重构后按整个 ToolResult 落盘（丢 per-URL 粒度，已接受）。
7. 单位统一 rune；落盘文件 rune 安全切割（复用 web_extract F6 修复）。

## 12. 实现锚点

- `runtime.go:894` `truncateText`；`messages.go:63` `appendToolResults(calls, results, maxResultChars)`
- toolRoot：`agent_resolver.go:316 agentToolRoot` / `command.go:1985 defaultTaskRunner root`；Runtime 需新增 sandbox root 字段并传入 conversation
- web_extract 重构点：`webextract.go` `extractOne` / `truncateAndCache` / `writeExtractCache` / `sanitizeSlug`
- 落盘 guard：`port.NewWorkspacePathGuard` / `.Check`（复用 web_extract 模式）
- config：`internal/config/config.go RuntimeConfig` + 默认值 `defaultConfig()`
- 现有回归测试：`internal/runtime/messages_test.go TestConversationTruncatesOversizedToolResult`

## 13. 开放问题

无（设计决策已确认：范围 P0+P1+P2、runtime 统一层、web_extract 重构并入、preview 3000 rune、loop cap 30、halt 默认 warn）。
