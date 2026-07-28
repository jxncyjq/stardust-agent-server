---
title: BUG — GUI serve 路径无跨轮会话记忆（conversationBlock 永不注入）
date: 2026-07-28
status: open / 待修
severity: high（影响所有 GUI 多轮对话，模型看不到任何前几轮）
area: legionAgent internal/runtime（AgentRuntimeResolver）+ serve 装配
---

# BUG：GUI 多轮对话无跨轮记忆

## 症状（用户报告）

GUI 里连续 4 轮对话，聊到第 3 轮时模型已「没有第一轮的上下文」。实际经排查是**从头到尾零跨轮记忆**——GUI 每个用户轮都被当成第一条消息跑，模型看不到任何前面轮次。

复现会话：`session-1784994524186778200`，round 1 = 「帮我看下长鑫国际今天的股票情况」，到 round 4 模型完全不知道 round 1 问的是什么。

## 根因（已 100% 坐实）

**GUI serve 路径构建的 per-agent runtime 从不注入 session 最近轮，`conversationBlock` 恒空。**

证据链：
1. **历史确实存着**：`agent.db` 的 `conversation_turns` 里该 session 有 8 轮（4 轮对话）都在，内容都小（15–540 字符）。
2. **配置本应注入**：本地 `agent.json` `session.default_recent_turns=6`、`max_turn_chars=6000`。
3. **注入点在 cognitive**：`internal/cognitive/core.go:138` `conversationBlock(req.ConversationTurns)` 把「Recent conversation:」块拼进 prompt；`req.ConversationTurns` 来自 `runtime.Config.ConversationTurns`（runtime.go:77 → :880 传给 cognitive.Request）。
4. **GUI 路径从不填 ConversationTurns**：`internal/runtime/agent_resolver.go` 的 `AgentRuntimeResolver` / `AgentRuntimeResolverConfig` **没有任何 session store 字段，也从不设 `Config.ConversationTurns`**（grep `ConversationTurns` 在该文件 0 命中）。它构建 runtime 时（`NewRuntime(Config{...})` 约 agent_resolver.go:184）根本没这一项。
5. **日志实锤**：开 `runtime.debug` 后，最近 500 条 `debug inference message` 里「Recent conversation」块出现 **0 次**（GUI 任务）。
6. **CLI/TUI 路径正常（对照）**：`internal/cli/command.go:509-513` 的 `runTUITask` 会 `cfg.Session.RecentTurns(ctx) → cfg.ConversationTurns = turns`，再传进 runtime（command.go:552/642）。**只有 CLI 走了这段；GUI serve 走 AgentRuntimeResolver，漏了。**

> 一句话：会话历史有存、CLI 会注入、**GUI serve 路径漏注入**。`http.go:403` 的 `ListConversationTurns` 只服务 GET 展示端点，不喂任务 runtime。

## 涉及代码

- `internal/runtime/agent_resolver.go`
  - `type AgentRuntimeResolver struct{...}` / `type AgentRuntimeResolverConfig struct{...}` —— **缺 session store 字段**。
  - `ResolveTaskRunner(...)` 里 `NewRuntime(Config{...})`（约 :184）—— **缺 `ConversationTurns`**。
- `internal/runtime/runtime.go:77`（`Config.ConversationTurns`）→ `:284`（存 `r.conversationTurns`）→ `:880`（传 cognitive.Request.ConversationTurns）。
- `internal/cognitive/core.go:138,188`（`conversationBlock`，正常，只是没数据喂它）。
- 正常对照：`internal/cli/command.go:509-513`（`RecentTurns()→ConversationTurns`）。
- session store 接口：`ListConversationTurns(ctx, sessionID, limit)`（http.go:56 / command.go:939）；CLI 用的是 `cfg.Session.RecentTurns`（command.go:1099 `tuiSessionController.RecentTurns` → `store.ListConversationTurns(currentID, recentTurns)`）。

## 修复方向

给 `AgentRuntimeResolver` 一个 session store（新增字段 + Config 传入），在 `ResolveTaskRunner` 里按 `task` 的 session key 加载最近 `DefaultRecentTurns` 轮 → 设 `Config.ConversationTurns`，镜像 CLI 的 `runTUITask` 那段。

要点/坑：
1. **session key**：用与 checkpoint 一致的 `sessionKeyForTask(task)`（runtime 里已有此函数），或 `task.SessionID`——需确认 GUI 任务上带的 session 标识与 `conversation_turns.session_id` 对得上（排查时 session-1784994524186778200 的 turns 是按 session_id 存的）。
2. **limit**：用 `rootConfig.Session.DefaultRecentTurns`（=6），经 `normalizeRecentTurns` 归一。
3. **排除当前轮**：注入的是**历史**轮，不含正在处理的这条 Input（避免重复）。CLI 那段的时序可参考。
4. **fail-loud**：加载失败返回 error（别静默空注入假装无历史）——契约上「无历史」与「加载失败」要分清。
5. **装配**：serve 命令构建 `AgentRuntimeResolver` 处（cli/command.go 里 serve 装配）把已有的 session store 传进 resolver config。
6. **max_turn_chars 截断**：注入前每轮按 `session.max_turn_chars`（6000）截断（CLI 侧是否已做需核对，保持一致）。

## 验证

- 单测：给 resolver 注入一个 fake session store（返回 N 条历史 turn），断言 `ResolveTaskRunner` 建出的 runtime 其 prompt 含「Recent conversation:」+ 历史内容。
- 真机：GUI 4 轮对话，第 4 轮问「第一轮我问的什么」，模型应能答出 round 1 主题。开 `runtime.debug` 看 prompt 里出现「Recent conversation」块。

## 关联

- 与 harness 加固（PR #59 P1 等）无关，是独立的会话记忆注入缺口。
- memory：[[legion-config-resolution-roots]]（working_dir/session 状态语义）、[[legion-token-multiround-debug-probe]]（debug 探针用法——本 bug 正是靠它坐实）。
- 探针实测会话：`session-1784994524186778200`（agent.db）。
