# G4c 实施计划：决策点的 `ask`（要求人工审批）

**日期**：2026-08-29
**上游**：spec `specs/2026-08-29-plugin-extension-points-design.md` §3.2 + 决策 A（「做，需要 ask」）；G4b（deny）已交付合入 master。
**范围**：插件的决策可以是第三种答案 `ask` —— 让这一轮挂起、进**既有**审批队列、人批了才继续。

---

## 一、落点：为什么不是一个地方，而是两个

宿主自己的审批就是两处，本期照抄它，而不是发明第三种形状：

```
round 边界   ToolGate.ShouldSuspend  → 开票 + 挂起（checkpoint 落盘，run 结束）
   ↓（人审）
resume → dispatch
派发时       Registry.Execute        → 读既有决定，放行或拒绝
```

- **`deny` 留在 registry**（G4b 已实现）：那是唯一必经点，绕过 runtime 的路径（插件自己的 `call_tool`、CLI、子代理）也必须被拒。
- **`ask` 的挂起只能在 round 边界**：挂起 = 落 checkpoint + 结束本次 run，registry 里同步阻塞等人审批会把这套 checkpoint/resume 模型换成一个「进程崩了就丢」的模型。

因此插件在**一轮里被问两次**，两次的语义不同，且都必须是纯判断（决策者不得有副作用）：

| 时机 | 问谁 | 答 `ask` 的意思 |
|---|---|---|
| round 边界（ToolGate） | 插件 | 开一张票并挂起 |
| 派发时（Registry） | 插件 | **必须已有一张批准的票**，否则拒 |

派发时那一次是 fail-closed 的兜底：没有审批设施（没注入仲裁者）、票不存在、票被拒、读票出错 —— 全部拒绝。**绕过 runtime 的调用因此天然拿不到 ask 的放行**，这不是缺陷，是这条路径本来就没有人在旁边看着。

## 二、票必须说清是谁要求的

`approval.ToolApproval` 加两个字段：

- `RequestedBy`：`host:sensitive` 或 `plugin:<name>`；
- `Reason`：插件给的理由（宿主自身的 Sensitive 票没有理由，留空）。

老票没有这两个字段，读出来是空串 —— 按 `host:sensitive` 呈现，不是错误。

**同一条队列**：插件的 ask 与宿主的 Sensitive 审批共用 `ToolGateStore`、同一票据形状、同一 SSE 事件、同一审批界面。两套并行的挂起来源是这一期最该避免的结果。

## 三、模式

宿主的 Sensitive 审批只在 `Manual` 模式生效。**插件的 ask 不看模式**：一个装了守门插件的 Auto 部署，其意图正是「这几类调用要人看一眼」，按模式忽略它等于把 ask 静默降级成 allow。代价（Auto 任务会挂起等人）必须写进手册。

## 四、任务拆解

| # | 任务 | 关键测试 |
|---|---|---|
| T1 | `tool.DecisionAsk` + 严格度排序（deny > ask > allow）+ `AskArbiter` 注入 | 多插件里 deny 压过 ask；无仲裁者时 ask = 拒绝且理由说清；仲裁者报错 = 拒绝 |
| T2 | `approval.ToolApproval` 的 `RequestedBy` / `Reason` | 老票（无字段）读作 `host:sensitive`；两种来源的票在同一个 store 里并存 |
| T3 | `Registry.ConsultDeciders` 导出 + manualgate 在 round 边界征询插件、任意模式下 ask 开票挂起 | Auto 模式下插件 ask 也挂起；宿主 Sensitive 与插件 ask 同队列；已批准的票不再重复挂起 |
| T4 | manualgate 提供 `AskArbiter`（读票），装配处注入 | 批准的票放行；被拒/无票/读票出错一律拒 |
| T5 | 两个 SDK 的 `Ask(reason)` + 示例插件 + e2e | op 3 能答 `ask`；宿主认得这个词（G4b 里它是 ABI 错误） |
| T6 | GUI：审批卡片显示出处与理由 | 一张插件票在界面上能看出是哪个插件、为什么 |
| T7 | 文档：手册 §3.4/§7/§9、两份 SDK README、示例、路线图 | — |

## 五、明确不做

- registry 里同步阻塞等审批（见 §一）。
- 插件**批准**别人要求的审批（ask 只能提出要求，不能代替人回答）。
- 让插件看到票的内容或别的插件的票。

## 六、验收

`go build/vet/test ./...` 全绿、`gofmt -l` 空、`cargo test` 全绿、**`-race ./internal/runtime/... ./internal/manualgate/... ./internal/tool/... ./internal/plugin/...`**（spec 点名：这条挂起/检查点路径出过并发缺陷）；每个任务做变异核对。
