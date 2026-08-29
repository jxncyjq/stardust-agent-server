# 让模型够得到插件贡献的工具

**日期**：2026-08-29
**上游**：G4 spec §9.3 的第一条遗留——「四个 seam 目前对 agent 自己的工具调用不触发」。
**为什么现在做**：G4a–G4d 把 observe / decide / ask / prompt 四个扩展点做齐了，但插件挂在 `newPluginLoader` 建的**独立**工具注册表上，模型的调用不经过它。在这条缝合上之前，那四期的能力在生产里是死的。

---

## 一、现状（读代码得到的三个事实）

1. `newPluginLoader` 建一个自己的工作区注册表，插件的工具、观察者、决策者都注册在**那一个**上。
2. 每个任务的注册表是**另建**的：`agent_resolver.go` 按 agent 建，`app.go` 的一次性路径也各建一个。两者都不认识插件注册表。
3. `tool.Registry` **已经有** parent 链：`resolve` / `visible` / `notifyObservers` / `consultDeciders` 全都走 parent。缺的只是「把插件注册表挂成 parent」这一步——不是缺机制。

## 二、做法

给工作区注册表加一个构造选项：**继承插件注册表**。

```
per-agent registry (policy / enforcer / guardrails / audit 都是自己的)
        └── parent: plugin registry (插件的工具 + observe/decide seam)
```

- 解析回落，**不共享策略**：插件的工具在**本 agent 的**策略、护栏与审计下执行。这与 `Subset`/`Without` 视图相反（那两个共享父的策略），所以是新的构造选项而不是复用 `view`。
- parent 是**引用**：插件运行期挂载/卸载立刻对所有任务注册表生效，不需要重建谁。
- G4 的四个 seam 因此自动生效：`consultDeciders` 与 `notifyObservers` 本来就走 parent。

## 三、权限：插件工具凭什么能被调用

`BatchRolePermissionEnforcer` 是白名单，插件工具名是运行期才知道的，永远不在里面。给它一个**动态来源**：一个 `func(toolName string) bool`，由装配处接到插件注册表的「这个名字是不是我贡献的」。

- 不是「插件工具对所有人放行」：它与内置工具走**同一条**判定路径，`disabled_tools`（toolauth 那套）照旧能禁掉它——插件工具在 gateable 目录里，这是 P2 就立下的规矩。
- 动态来源为 nil 时行为与今天逐字节相同：没有插件的部署不受影响。

## 四、任务

| # | 任务 | 关键测试 |
|---|---|---|
| T1 | `tool`：继承选项 + `Registry.HasTool` + enforcer 动态来源 | 子注册表能解析并执行父的工具；父卸载后立刻不可见；子的 policy/guardrails/audit 仍是**子自己的**；未知工具仍被拒 |
| T2 | 装配：resolver 与一次性路径都继承插件注册表 | resolver 建的注册表看得见插件工具；没有插件时逐字节不变 |
| T3 | 端到端：插件的 decide/observe 对 agent 的调用真的触发 | 这是本期存在的理由，必须有一个测试直说 |
| T4 | 文档：手册、spec §9.3、路线图 | — |

## 五、明确不做

- 让插件工具**遮蔽**同名内置工具：`resolve` 里自身注册优先，插件的被遮蔽。本期保持这个方向（内置赢），并在文档里写明；反过来会让一个插件悄悄替换 `write_file`。
- 把插件注册表变成所有注册表的**唯一**根（那会让插件的 policy/审计接管一切）。
- 让插件工具绕过 `disabled_tools`。

## 六、验收

`go build/vet/test ./...` 全绿、`gofmt -l` 空、`-race ./internal/tool/... ./internal/runtime/... ./internal/plugin/...`；每任务变异核对。
