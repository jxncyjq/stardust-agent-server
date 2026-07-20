---
id: "adr-curator-001"
title: "ADR: Curator 技能生命周期确定性扫描（零 token）"
aliases: ["Curator ADR", "技能生命周期", "skill curator 决策"]
type: "design"
category: "backend/library/architecture"
tags: ["adr", "legion-agent", "curator", "skill", "lifecycle", "token-optimization"]
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

# ADR: Curator 技能生命周期确定性扫描

<!-- @section: overview -->
## 概述

**状态**：已实现（本轮优化计划 Task 7）。

为技能系统补齐使用统计 + 闲置老化自动化：`skill.Curator` 以**确定性、零 token** 的纯 Go 扫描把闲置的 agent 自建技能依次流转 stale→archived，**从不删除**。参考 [[analysis-hermes-updates-008|Hermes v0.17 新增功能分析]] §curator。
<!-- @end-section -->

<!-- @section: context -->
## 背景与动机

- `internal/skill/lifecycle.go` 有 Enable/Disable/status/risk，gene 有 success_rate/count，但**无使用统计 + stale/archive 自动化**。
- 若引入 agent 自建技能，需要零成本确定性维护，避免技能库膨胀撑爆上下文。
- 既有 `Skill` 结构无 `created_by`/`pinned`/`last_activity_at` 字段，`Source` 只有 `workspace`/`registry`，`Status` 无 stale/archived。
<!-- @end-section -->

<!-- @section: decision -->
## 决策

- **新增状态 + 转换**：`system.go` 加 `StatusStale`/`StatusArchived`；`lifecycle.go` 加 `Archive` 转换（可逆、从不删除、缺失技能 fail-loud）。
- **usage sidecar**：`UsageStore`（并发安全，键为 skill id）记录 `LastActivityAt`/`UseCount`/`Pinned`，与 `Skill` 分离——用一次技能不重写技能内容。
- **确定性扫描**：`Curator.Sweep(ctx, now)` 按 id 排序遍历，`idle >= ArchiveAfter`（默认 90d）→ archived，`>= StaleAfter`（默认 30d）→ stale；仅**状态真变**才持久化 → **幂等**。
- **作用域映射**：只碰 `SourceWorkspace`（对应 hermes 的 agent 自建）；`SourceRegistry`（对应 bundled/hub）免疫；`Pinned` 豁免；无 usage 记录者跳过；**从不删除**。
- **可选 LLM 合并**：`CuratorConfig.Consolidate` 默认 `false`（契约显式可选），开启才调辅助模型；关闭时全程零 token。
<!-- @end-section -->

<!-- @section: consequences -->
## 影响与权衡

- ✅ 技能维护零 token、确定性、幂等、并发安全；从不删除，归档可逆。
- ✅ 与 hermes 字段模型的差异通过「新 sidecar + workspace/registry 映射」faithful 落地，未污染既有 `Skill` 结构。
- ✅ 生产接线已完成：`storage.SQLiteRepository.ListSkills`（+ `MemoryRepository`）满足 `CuratorRepository`，`newSkillCuratorSweepJob` 挂入后台调度器定时 `Sweep`；`skill.System.WithUsage` 在 `SelectForTask` 选中技能时 `UsageStore.Touch` 记录活跃，与 Curator 共享同一 `UsageStore` 实例——活跃度数据链路闭合。
- ✅ `Consolidate` 不再是死开关：改为可插拔 `SkillConsolidator` 接口，`Consolidate=true` 但未注入 consolidator 时 `NewCurator` **构造即 fail-loud**，Sweep 在开启时真实调用并传播其错误。具体 LLM 合并器由调用方注入（当前未接生产合并器）。
<!-- @end-section -->

## 相关文档

- [[analysis-hermes-updates-008|Hermes v0.17 新增功能分析]] — Curator 参考来源
- [[adr-delegation-001|ADR: delegate_task 子任务委派]]
- [[adr-moa-001|ADR: Mixture-of-Agents 多模型协作]]
