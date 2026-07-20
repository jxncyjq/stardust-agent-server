---
id: "adr-delegation-001"
title: "ADR: delegate_task 子任务委派（batch/background/leaf-orchestrator）"
aliases: ["delegation ADR", "delegate_task", "子代理委派决策"]
type: "design"
category: "backend/library/architecture"
tags: ["adr", "legion-agent", "delegation", "runtime", "token-optimization"]
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
  - id: "adr-moa-001"
    relation: "related_to"
    path: "./adr-moa.md"
  - id: "adr-session-search-001"
    relation: "related_to"
    path: "./adr-session-search.md"
---

# ADR: delegate_task 子任务委派

<!-- @section: overview -->
## 概述

**状态**：已实现（本轮 Legion×Hermes 优化计划 Task 5）。

为 legionAgent 运行时补齐「让模型主动派生子任务」的能力。子 Agent 拥有**独立上下文**、只回传最终摘要，从而把子任务中间过程挡在父上下文之外——既是能力增强，也是 token 优化手段。参考 hermes-agent v0.17 的 delegation 机制（见 [[analysis-hermes-updates-008|Hermes v0.17 新增功能分析]] §delegation）。
<!-- @end-section -->

<!-- @section: context -->
## 背景与动机

- 既有 `agent_resolver.go` 能解析子 agent 配置、`tool/agent_message.go` 有 agent 间通信，但**没有**让模型在一次任务内主动派生并回收子任务的工具。
- 长任务把所有中间推理/工具结果堆进单一上下文，input token 随轮数膨胀。
- 需要并行执行独立子任务、后台异步子任务，以及防止无界递归派生。
<!-- @end-section -->

<!-- @section: decision -->
## 决策

在 `internal/runtime` 内实现委派核心（`delegation.go`），并以 `delegate_task` 工具暴露给模型：

- **子 Runtime 独立上下文**：`RunSubTask` 经 `newSubRuntime` 克隆父的 maas/tools/contextBuilder/事件汇，但**清空会话历史**，跑 `RunTask` 后只取 `TaskRun.Result`（摘要）回传。
- **batch 并行**：`RunSubTasks` 用 `sync.WaitGroup` + 信号量限 `MaxConcurrent`（默认 3），结果按输入顺序稳定；单个子任务失败**逐条回报**（`SubTaskResult.Err`）不炸整批，仅**调度层**失败（禁止委派）才 fail-loud。
- **background**：`RunSubTaskAsync` 立即返回 `SubTaskHandle`，在 detached context（`context.WithoutCancel`）上执行，完成经 `EventBus` 发 `subtask_completed` 事件回灌；进程内、非 durable。
- **leaf / orchestrator + 深度限制**：root 默认 orchestrator，子默认 leaf；`canDelegate()` 要求 orchestrator 且 `depth < MaxSpawnDepth`（默认 2），超限 fail-loud。
- **工具注册方位**：因 `tool` 包不能反向 import `runtime`（会构成 import 循环），`delegate_task` 工具经 `Runtime.RegisterDelegateTaskTool` **从 runtime 侧**注册。leaf 子 Runtime 不满足 `canDelegate()` → 不注册该工具 → **天然禁止递归派生**。
<!-- @end-section -->

<!-- @section: consequences -->
## 影响与权衡

- ✅ 子任务上下文隔离，父上下文只增加摘要，显著抑制 input token 膨胀。
- ✅ fail-loud：深度/并发/派生失败均响亮报错；后台完成/发布失败在 goroutine 边界经审计日志兜底记录，不静默吞。
- ⚠️ 背景子任务非 durable：父进程退出即丢失（已在工具描述与代码注释标注）。
- ⚠️ `delegate_task` 为 high-risk 工具，已加入 `developer` 角色白名单与 auto-allow；生产若需人审可移出 auto-allow。
- ✅ 子 Agent `toolsets` 范围裁剪已落地（`SubTaskSpec.Toolsets` → `Registry.Subset`，空则继承父全集；`delegate_task` 支持 `toolsets` 参数）。
- ✅ 背景子任务完成回灌已接线：`newSubtaskReinjectionJob` 后台消费 `subtask_completed` 事件，经 `ParentTaskIDForSubTask` 还原父 id，转成一条 `AgentMessage`（type=result，`task_id`=父任务），父 Agent 下一回合经 `read_messages(task_id=<父>)` 即见；消息 id 由子任务 id 派生 → upsert 幂等。
<!-- @end-section -->

## 相关文档

- [[analysis-hermes-updates-008|Hermes v0.17 新增功能分析]] — delegation 参考来源
- [[adr-moa-001|ADR: Mixture-of-Agents 多模型协作]]
- [[adr-session-search-001|ADR: FTS5 session_search]]
- [[adr-curator-001|ADR: Curator 技能生命周期]]
