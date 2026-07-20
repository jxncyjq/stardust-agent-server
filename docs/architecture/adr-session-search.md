---
id: "adr-session-search-001"
title: "ADR: FTS5 会话检索 session_search（discovery/scroll/browse）"
aliases: ["session_search ADR", "FTS5 检索", "会话检索决策"]
type: "design"
category: "backend/library/architecture"
tags: ["adr", "legion-agent", "session-search", "fts5", "storage", "token-optimization"]
version: "1.0.0"
created: "2026-07-02"
updated: "2026-07-02"
author: "jxncyjq"
status: "published"
parent: null
children: []
related_docs:
  - id: "analysis-hermes-updates-008"
    relation: "implements"
    path: "../../../../docs/design/analysis/hermes/08-hermes-v017-updates.md"
  - id: "adr-delegation-001"
    relation: "related_to"
    path: "./adr-delegation.md"
---

# ADR: FTS5 会话检索 session_search

<!-- @section: overview -->
## 概述

**状态**：已实现（本轮优化计划 Task 4）。

为 SQLite 存储加 FTS5 全文索引，暴露一等工具 `session_search`，实现「按需检索替代长历史堆叠」——零 aux-LLM、几乎零 token。参考 [[analysis-hermes-updates-008|Hermes v0.17 新增功能分析]] §session-search。
<!-- @end-section -->

<!-- @section: context -->
## 背景与动机

- `internal/storage/sqlite.go` 原本**无 FTS5**；会话历史靠堆叠进 prompt，重复 input token 高。
- 驱动 `modernc.org/sqlite` 默认内建 FTS5，无需额外 build tag。
- 需要三种检索姿态：关键词发现、围绕某消息滚动上下文、浏览近期会话。
<!-- @end-section -->

<!-- @section: decision -->
## 决策

- **schema v3→v4**：新增 `conversation_turns_fts` FTS5 虚拟表（`content` 索引 + `turn_id/session_id/...` UNINDEXED）。
- **写入同步**：`AppendConversationTurn` / `AppendConversationTurnIfAbsent` 重构为**事务**，源行 + FTS 索引 + session touch 一起提交，索引永不漂移；旧 turn 不回填（文档标注，v4 起覆盖）。
- **查询方法**（全 fail-loud）：`SearchMessages`（`MATCH ... ORDER BY rank`）、`ScrollMessages`（锚点 ±window，锚点缺失响亮报错）、`BrowseSessions`（近期会话）。
- **工具**：`internal/tool/session_search.go` 经窄接口 `MessageSearcher`（accept-interface）桥接存储；模式按参数**确定性推断**——`query`→discovery、`session_id`+`around_message_id`→scroll、无参→browse，半个 scroll 参数返回失败提示。
- **接线**：`command.go` 在 repo 可用时 `RegisterSessionSearchTool(defaultTools, repo)`；`builtin.go` 两 registry 加 `session_search` 白名单。
<!-- @end-section -->

<!-- @section: consequences -->
## 影响与权衡

- ✅ 用检索替代历史堆叠，直接削减重复 input token；检索本身不耗推理 token。
- ✅ 索引与源表事务同步，杜绝漂移；查询与工具全链路 fail-loud。
- ✅ v4 之前的历史 turn 已可回填：`BackfillConversationTurnsFTS`（`NOT EXISTS` 幂等）在 `<v4→v4` 升级时自动跑一次，亦可手动调用修复索引。
- ⚠️ FTS5 `MATCH` 语法直接透传，异常查询按 fail-loud 报错而非静默空结果。
<!-- @end-section -->

## 相关文档

- [[analysis-hermes-updates-008|Hermes v0.17 新增功能分析]] — session_search 参考来源
- [[adr-delegation-001|ADR: delegate_task 子任务委派]]
- [[adr-curator-001|ADR: Curator 技能生命周期]]
