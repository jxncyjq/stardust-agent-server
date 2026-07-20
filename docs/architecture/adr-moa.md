---
id: "adr-moa-001"
title: "ADR: Mixture-of-Agents 多模型协作协调器"
aliases: ["MoA ADR", "Mixture of Agents", "多模型协作决策"]
type: "design"
category: "backend/library/architecture"
tags: ["adr", "legion-agent", "moa", "runtime", "multi-model"]
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

# ADR: Mixture-of-Agents 多模型协作协调器

<!-- @section: overview -->
## 概述

**状态**：已实现（本轮优化计划 Task 6）。

引入 `runtime.MoACoordinator`：N 个**参考模型**并行对同一任务生成，再由**聚合器模型**综合出单一更优答复。one-shot 触发，避免每轮多模型导致 token 爆炸。参考 [[analysis-hermes-updates-008|Hermes v0.17 新增功能分析]] §moa。
<!-- @end-section -->

<!-- @section: context -->
## 背景与动机

- `MaasInferenceClient` 主循环是**单模型**调用；`06` 洞察已提出「多模型协作 + 投票」方向。
- 需要质量感知的多模型裁决（如研究用一档、代码用另一档，聚合器综合），但**不能**让每个工具轮都跑 N 个模型（token 成本线性放大）。
<!-- @end-section -->

<!-- @section: decision -->
## 决策

`internal/runtime/moa.go` 实现 `MoACoordinator`：

- `NewMoACoordinator(references, aggregator)`：校验至少 1 个参考模型 + 非空聚合器，否则构造失败（编程错误 fail-loud）。
- `Aggregate(ctx, task)`：用 `sync.WaitGroup` **并行**调用所有参考模型的 `Generate`；把每个成功输出以 `[参考回答 <label>]` 分块拼进聚合器 prompt，聚合器综合出 `MoAResult.Text`。
- **失败策略**：单个参考模型报错/空结果 → 记 `Warnings` 并**丢弃该路**；**全部失败/全空** → fail-loud（拒绝拿空输入喂聚合器，聚合器不被调用），杜绝「用空结果冒充答复」。
- **触发定位**：one-shot（供工具/斜杠/配置开关调用），**不**挂进 `RunTask` 每轮默认路径。
<!-- @end-section -->

<!-- @section: consequences -->
## 影响与权衡

- ✅ 一次性多模型综合，质量提升的同时把成本限制在单轮。
- ✅ 结算环节 fail-loud：不以空/失败结果冒充有效答复。
- ✅ **触发点已接线**：`moa_consult` 工具（`RegisterMoAConsultTool` + `ModelResolver`），参数 `task`/`reference_profiles`/`aggregator_profile`，经 MaaS profile 解析构建 `ModelRef`；high-risk，已入 developer 白名单。coordinator 本身仍可被其它入口复用。
- ⚠️ token 成本仍是「N 参考 + 1 聚合」次调用，需由触发方按场景决定是否值得。
<!-- @end-section -->

## 相关文档

- [[analysis-hermes-updates-008|Hermes v0.17 新增功能分析]] — MoA 参考来源
- [[adr-delegation-001|ADR: delegate_task 子任务委派]]
- [[adr-curator-001|ADR: Curator 技能生命周期]]
