---
id: "design-token-usage-persistence-001"
title: "token 消耗持久化技术 spec（audit_events + conversation_turns 落盘 token）"
aliases: ["token 落盘", "token usage persistence", "token 消耗记录"]
type: "design"
category: "superpowers/specs"
tags: ["legionagent", "sqlite", "audit", "conversation-turns", "token", "usage", "spec"]
version: "1.0.0"
created: "2026-08-10"
updated: "2026-08-10"
author: "jxncyjq"
status: "draft"
parent: null
children: []
---

# token 消耗持久化技术 spec

> legionAgent 目前不把 token 消耗量落盘：`audit_events` 和 `conversation_turns` 表无 token 列，`TaskRun`（携带 token）是纯内存、不持久化——token 只在 GUI 靠 `GetTaskResult` 实时显示。本 spec 给两表各加 `prompt_tokens/completion_tokens/cached_tokens/total_tokens` 四个 typed 列，把 `finishRun` 落盘点已就绪的 `st.*Tokens` 写进去，使 token 消耗可事后查询、对账。**纯 legionAgent 后端**：schema 迁移 + 写读接线，无新系统、无 API 破坏。

<!-- @section: overview -->
## 概述

### 背景（取证）
排查会话 token 消耗时发现：全库无任何 `token/usage/cost` 列；`audit_events.model_inference_completed` 只记「发生了推理」不记量；`conversation_turns` 只存文本。token 数在 `internal/runtime/runtime.go` 的 `finishRun` 处已算好（`st.promptTokens/completionTokens/cachedTokens/totalTokens`，见该函数写入 `domain.TaskRun` 字段），但 `TaskRun` 无对应表——落盘缺口就在此。

### 目标
token 消耗量持久化到 `audit_events`（权威审计链，per-inference）与 `conversation_turns`（per-turn，便于随内容一起查/展示）。

### 非目标
- ❌ 新增 `task_runs` 表
- ❌ 成本（货币 $）换算
- ❌ 跨会话/跨 agent 的聚合统计报表
- ❌ 改动 HTTP API 契约（现有 `taskResultResponse` 已含 token 字段，不变）

<!-- @section: architecture -->
## 架构

token 数在两个写点均已就绪，只是没写进表。给两表各加 4 个 typed 列，把已有数写入。

```
runtime.finishRun ──(st.*Tokens)──> audit.Append(model_inference_completed)  ← 加 token
                                     domain.TaskRun (内存, 不落盘)

server.taskResult ──(usage from task_completed event)──> recordAssistantTurn  ← 加 token
                                     AppendConversationTurnIfAbsent            ← INSERT 加列
```

**语义**：token 列只对 `model_inference_completed` audit 行、assistant turn 有意义；其余行（user turn、`task_completed`/`intent`/`mutate` 等 audit）= 0。存的是与 GUI 实时显示、`Checkpoint`、`TaskRun` 一致的**同一权威数**（多轮 tool-loop 则为该 loop 的累计值，不另造）。

<!-- @section: schema -->
## Schema 变更（internal/storage/sqlite.go）

给两表各加 4 列，**双写**以覆盖新库与老库：
1. **CREATE TABLE**（新库）：在 `audit_events`、`conversation_turns` 的建表语句里加列。
2. **迁移列表**（老库）：在既有幂等 `ALTER TABLE ... ADD COLUMN` 迁移列表（sqlite.go 约 1629 起，每次打开跑）追加 8 条。

```sql
-- audit_events + conversation_turns 各加：
prompt_tokens     INTEGER NOT NULL DEFAULT 0
completion_tokens INTEGER NOT NULL DEFAULT 0
cached_tokens     INTEGER NOT NULL DEFAULT 0
total_tokens      INTEGER NOT NULL DEFAULT 0
```

老库既有行 → 默认 0（历史数据无 token 记录，属正当可选）。迁移幂等：ADD COLUMN 对已加过的库会报「duplicate column」——沿用现有迁移列表的忽略/跳过机制（与现有 `agent_sessions ADD COLUMN project` 等同款处理）。

<!-- @section: domain -->
## Domain 变更（internal/domain/types.go）

```go
// AuditEvent（约 112 行）+ 4 字段
type AuditEvent struct {
    // ...现有字段...
    PromptTokens     int `json:"prompt_tokens,omitempty"`
    CompletionTokens int `json:"completion_tokens,omitempty"`
    CachedTokens     int `json:"cached_tokens,omitempty"`
    TotalTokens      int `json:"total_tokens,omitempty"`
}

// ConversationTurn（约 170 行）+ 同样 4 字段
```

<!-- @section: write-paths -->
## 写路径

### 1. audit（internal/runtime/runtime.go, finishRun）
`model_inference_completed` 的 `domain.AuditEvent` 字面量填 `st.promptTokens/completionTokens/cachedTokens/totalTokens`（同函数内已在作用域，与写 `TaskRun` 同源）。

### 2. audit INSERT（internal/storage/sqlite.go, audit.Append 实现）
audit 写库 INSERT 语句加 4 列 + 占位符 + 传值。

### 3. assistant turn（internal/server/http.go, recordAssistantTurn）
`recordAssistantTurn` 现签名 `(ctx, task, result)`，不接 token。改为接收 token（加参或传一个 usage 结构）；其调用者已持有 `usage`（来自 `taskResult` 读 task_completed 事件，见 http.go ~1090 注释「TaskRun 不持久化，task_completed 事件是唯一暴露处」）。构造 `ConversationTurn` 时填 4 值。

### 4. turn INSERT（internal/storage/sqlite.go）
`AppendConversationTurn`（约 400）与 `AppendConversationTurnIfAbsent`（约 435）两个 INSERT 语句各加 4 列 + 占位符 + 传值。

<!-- @section: read-paths -->
## 读路径

`ListAuditEvents` / `ListConversationTurns`（及 GUI 侧 `app.go` 的 `ListAuditEvents`）的 SELECT 带出新 4 列并映射回 domain 结构，使 token 可查询（GUI 可展示/对账）。SELECT 显式列出列名的地方补上；`SELECT *` 处随 scan 目标结构同步。

<!-- @section: failloud -->
## fail-loud（守 legionAgent CLAUDE.md 铁律）

- **不伪造**：usage 缺失时存真实的 0，不猜一个值。user turn / 非 model audit 行的 0 是**契约允许的可选**（token 语义由 role/action 决定），非兜底。
- **不静默吞错**：迁移/INSERT/SELECT 的 error 一律 `fmt.Errorf("<动作> <标识>: %w", err)` 包装返回，沿用现有写法。
- **权威一致**：写入的 token 与 `TaskRun`/`Checkpoint`/GUI 实时值同源，不新引入一条可能漂移的计算路径。

<!-- @section: testing -->
## 测试

- **迁移**：新库 CREATE 后两表含 4 新列；老库（不带列的建表 + 跑迁移）ALTER 后含 4 列且幂等（重复跑不报错）。
- **turn 往返**：`AppendConversationTurn` / `AppendConversationTurnIfAbsent` 写入带 token 的 turn → `ListConversationTurns` 读回 4 值一致；user turn（无 token）读回 0。
- **audit 往返**：写带 token 的 `model_inference_completed` → 读回一致；`task_completed` 等行 token=0。
- **finishRun**：断言写出的 `model_inference_completed` audit 携带非零 token（happy path）。
- **recordAssistantTurn**：传入 usage → 落盘 turn 带对应 token。
- **回归**：现有 turns/audit/runtime 测试仍绿；`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 空。

<!-- @section: open-questions -->
## 待确认 / 后续
- `taskResult` 的 usage 来自内存事件总线；若某任务的 task_completed 事件已被消费/过期导致 usage 拿不到，assistant turn 会落 0——是否需要改由 runtime 在写 turn 时直接携带 token（更可靠）留作后续优化。
- 跨会话 token 聚合、成本换算另开 spec。
