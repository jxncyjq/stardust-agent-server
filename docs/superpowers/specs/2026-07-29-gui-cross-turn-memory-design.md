---
title: 修 GUI 跨轮会话记忆 — serve 路径注入 session 最近轮
date: 2026-07-29
status: approved
scope: legionAgent internal/runtime（AgentRuntimeResolver）+ serve 装配
---

# 修复：GUI serve 路径注入 session 最近轮

## 目标

修 bug（详见 `docs/superpowers/2026-07-28-bug-gui-no-cross-turn-memory.md`）：GUI serve 路径构建的 per-agent runtime 从不填 `Config.ConversationTurns`，`conversationBlock` 恒空 → GUI 每轮当第一条跑、零跨轮记忆。CLI/TUI 路径有注入，serve 漏了。本设计给 serve 路径补上，镜像 CLI 的 `runTUITask`。

## 根因回顾（已坐实）

- 注入点：`cognitive/core.go:138` `conversationBlock(req.ConversationTurns)` ← `runtime.Config.ConversationTurns`。
- GUI serve 路径：`AgentRuntimeResolver.ResolveTaskRunner` 构建 runtime（`NewRuntime(Config{...})`），**从不设 `ConversationTurns`**，且 resolver 无 session store。
- 联动确认：`conversation_turns.session_id` = `turn.SessionID`（sqlite.go:398）；`task.SessionID` 即会话键（checkpoint.go `sessionKeyForTask`）；serve 经 `recordUserTurn`（http.go:900）把当前用户轮写库（ID=`<taskID>:user`）**在任务入队前**。
- `SessionStore` 接口（server/http.go:54）已有 `ListConversationTurns(ctx, sessionID, limit)`；serve 持 `s.sessions`。

## 架构

### 组件
- `internal/runtime/agent_resolver.go`：
  - 新增最小接口 `type ConversationTurnLister interface { ListConversationTurns(ctx context.Context, sessionID string, limit int) ([]domain.ConversationTurn, error) }`（只依赖这一个方法，解耦，不引整个 SessionStore）。
  - `AgentRuntimeResolverConfig` + `AgentRuntimeResolver` 加字段 `ConversationTurns ConversationTurnLister`（nil = 不注入，合法禁用态）。
  - `ResolveTaskRunner` 里加载并注入：见数据流。
  - `NewRuntime(Config{...})` 加 `ConversationTurns: <加载到的历史>`。
- serve 装配处（`internal/cli/command.go` serve 命令构建 `AgentRuntimeResolverConfig` 的地方）：把已实现 `ListConversationTurns` 的 session store 传入。

### 数据流（一次 GUI 轮）
1. GUI POST 消息 → `recordUserTurn` 写当前用户轮（`<taskID>:user`）→ 任务入队。
2. coordinator 派发 → `ResolveTaskRunner(task)`。
3. `lister != nil && task.SessionID != ""` 时：
   - `turns, err := lister.ListConversationTurns(ctx, task.SessionID, normalizeRecentTurns(DefaultRecentTurns)+1)`（多取 1 条以补偿当前轮）。
   - **过滤当前轮**：丢弃 `turn.TaskID == task.ID`（即那条 `<taskID>:user`）。
   - 每轮 `Content` 按 `Session.MaxTurnChars` 截断（复用现有 truncate 语义/helper）。
   - 取最近 `DefaultRecentTurns` 条 → `Config.ConversationTurns = 结果`。
4. `NewRuntime` → `r.conversationTurns` → cognitive `conversationBlock` 拼「Recent conversation:」→ 模型见前几轮。

## 错误处理（fail-loud）

- `ListConversationTurns` 返回 error → `ResolveTaskRunner` 返回 error（与其现有 error 返回契约一致）。**绝不**静默注入空历史假装无前文（那会把「加载失败」与「真无历史」混淆）。
- `lister == nil`（未装配，如测试/嵌入）或 `task.SessionID == ""`（无会话）→ 合法：不注入，`ConversationTurns` 留空。契约允许的可选，非兜底。
- 过滤/截断纯内存操作，无 error。

## 决策（已确认）

1. 排除当前轮：`ListConversationTurns(limit+1)` 后过滤 `TaskID == task.ID`，取最近 `DefaultRecentTurns` 条。**不猜**。
2. 最小接口 `ConversationTurnLister` 而非整个 SessionStore，保持 resolver 解耦。
3. 加载点 = `ResolveTaskRunner`（每任务一次，天然按 task.SessionID 隔离）。
4. `MaxTurnChars` 截断镜像 CLI/config 意图，防单轮过大。

## 测试（`internal/runtime/agent_resolver_test.go`）

- fake lister 返回 N+1 条（含一条 `TaskID==当前task.ID` 的当前轮）→ 断言构建的 runtime 渲染 context **含**「Recent conversation」+ 历史内容，**排除**当前轮，条数 ≤ N。
- 单轮 `Content` 超 `MaxTurnChars` → 注入内容被截断。
- `task.SessionID == ""` → 不注入（无「Recent conversation」块）。
- lister 返回 error → `ResolveTaskRunner` 返回 error（fail-loud，不静默空注入）。
- `lister == nil` → 不注入、不 panic。
- 门禁：`go build/vet/test ./...` 全绿、`gofmt -l .` 空。

## 非目标

- 不改 CLI/TUI 路径（已正常）。
- 不改 cognitive `conversationBlock` 渲染（正常，只是没数据喂它）。
- 不做跨会话/语义检索（另事）。
- 不动 P1/P2/P3/P4 harness 加固。

## 相关

bug doc：`docs/superpowers/2026-07-28-bug-gui-no-cross-turn-memory.md`。
memory：[[legion-config-resolution-roots]]（session 状态语义）、[[legion-token-multiround-debug-probe]]（探针坐实本 bug）。
