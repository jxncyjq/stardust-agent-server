# D1b 实施计划：`call_tool` 的服务名解析

**日期**：2026-08-29
**上游**：spec `specs/2026-08-29-plugin-service-seam-design.md`（决策：命名服务 / 先到先得 / 唯一提供者）；D1a 已交付——声明、绑定与生命周期收敛都在了，**只是还没有人能按服务名发起调用**。

---

## 一、要解决的那一件事

D1a 之后，消费者插件仍然只能这样调用提供者：

```jsonc
call_tool { "tool": "jira_search" }   // 写死了某个提供者的工具名
```

这让「按能力依赖」只兑现了一半：生命周期按服务走，调用还按工具名走。换提供者仍要改消费者代码。D1b 补上另一半：

```jsonc
call_tool { "tool": "service:issue-tracker/search" }
```

## 二、能力名从哪来：提供者显式映射

服务的**能力名**（`search`）与提供者的**工具名**（`jira_search`）必须分开，否则换提供者就得让两家用同一个工具名——那正是要消除的耦合。提供者在 `plugin.json` 里显式声明映射：

```jsonc
"provides_services": ["issue-tracker"],
"service_capabilities": {
  "issue-tracker": { "search": "jira_search", "comment": "jira_comment" }
}
```

校验（解析期，fail-loud）：键必须出现在 `provides_services` 里；能力名与工具名都不得为空；工具名必须是**本插件自己贡献的**工具（映射到别人的工具等于替别人做主）。

`service_capabilities` 是**契约声明的可选**：只声明 `provides_services` 而不给映射，是一个「占住这个能力名、但还没有可调用面」的合法状态——D1a 的收敛只关心谁在。

## 三、解析发生在哪，以及绝不发生在哪

- **只在 guest 的 `call_tool` 里解析。** 模型的工具调用路径**不认**服务名：模型从不看见服务名（D1a 立的规矩），一个能按服务名调用的模型会让「模型看到的工具清单」与「它能调用的东西」不再一致。
- 解析出的**真实工具名**才是后续一切的输入：共享预算按它计数（`domain.GuardedToolName`），注册表按它派发，审计按它记录。调用来源仍是 `plugin:<消费者>`。
- 解析**不绕过任何检查**：解析只把名字换掉，之后照旧走 `Registry.Execute` 的权限、策略、护栏、超时、清洗与审计。

## 四、失败都是错误，没有一个回落

| 情形 | 结果 |
|---|---|
| `service:` 后面格式不对（缺 `/`、任一段为空） | `CodeInvalidRequest`，说明期望的形状 |
| 这个部署没有接服务解析器 | 错误（不是「当作普通工具名」）——把服务名当工具名去查，只会得到一句无关的 tool not found |
| 服务无人提供 | 错误点名服务（消费者通常已被挂起，这是兜底） |
| 提供者在，但没声明这个能力 | 错误点名服务、能力与提供者 |

## 五、任务

| # | 任务 | 关键测试 |
|---|---|---|
| T1 | manifest 的 `service_capabilities` 与校验 | 键不在 provides_services / 空名 / 映射到别人的工具，三条各自被拒 |
| T2 | `host.Deps.Services` 解析器接口 + `callTool` 的解析（含四种失败） | 解析后按真实工具名计预算与派发；解析失败不消耗预算 |
| T3 | loader 实现解析器并注入（服务→提供者→工具），随挂载/卸载即时变化 | 换提供者后同一个 `service:` 名字解析到新工具；提供者挂起时解析失败 |
| T4 | Rust SDK 的 `host::invoke_service`（**Go SDK 本期不做**，见下） | 发出的 tool 名是 `service:<名>/<能力>` |
| T5 | 文档：手册 §三点五/§4/§9、spec、路线图 | — |

### T4 的范围调整（实施中发现）

- **Go SDK 不加**：它的 `tool` 能力绑定整体还没落地（`host_wasip1.go` 目前只有 log），单独为服务名加一个便捷函数会造出一个没有底座的 API。等 Go SDK 补上 call_tool 时一并加。
- **不新增第二个示例包**：提供者 + 消费者要两个包，会让 `plugin_example/` 的结构翻倍，而机制已由 loader 的换提供者测试与 host 的接线测试钉住。两侧的完整片段写进手册与 Rust README。

## 六、明确不做

- 让模型按服务名调用（见 §三）。
- 一个能力名映射到别的插件的工具。
- 服务名进 gateable 目录：被禁用的是**工具**，服务只是到达它的一条路。

## 七、验收

`go build/vet/test ./...` 全绿、`gofmt -l` 空、`-race ./internal/plugin/...`；每任务变异核对。
