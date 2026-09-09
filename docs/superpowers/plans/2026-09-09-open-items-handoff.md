# 未完事项接续（2026-09-09）

**这份文档的用途**：插件签名分发（S1/S2）与「deepseek-harness 三条值得抄的」里的 A、B 都已合入 master。这里只写**还没做的事**，每条都说清「为什么它还在、下一步具体做什么、需不需要人拍板」。给下一个接手的人看。

**三仓 tip**：`stardust-agent-server` master `7dd05b8` / `stardust-agent-gui` `f6f60bf` / `docs` `8c9202d`。工作树均干净，**三仓零开放 PR**。

**上一份**：[2026-09-07-open-items-handoff.md](2026-09-07-open-items-handoff.md)。那份里 §一/§二/§三/§四/§五 仍然有效，本文接着往下记，**不重复搬运**——但 §〇 那颗雷已经升级，见本文 §〇。

## 本轮合入 master 的三件

| PR | 内容 | squash |
|---|---|---|
| [#158](https://github.com/jxncyjq/stardust-agent-server/pull/158) | S2 插件的分级安装体验 | `f1e92b2` |
| [#159](https://github.com/jxncyjq/stardust-agent-server/pull/159) | Spec A 委派结果的可判读性 | `c5d6661` |
| [#160](https://github.com/jxncyjq/stardust-agent-server/pull/160) | Spec B 按名字委派给已配置 agent | `7dd05b8` |

---

## 〇、先读这两条

### A. 那颗雷还在，而且现在更容易被踩

`Store.Refresh` 失败时的 fallback Trust **仍然不携带撤销记录**（`internal/plugin/trustlist/store.go:256` 一带）。

上一份文档说「今天没有任何调用方把它送进 `Merge`，一旦有人送就是同一条 Critical 换个拼法复活」。**这条今天仍然成立**——S2 修的是 `Current()` 那条路，`Refresh` 那条没动。

推荐修法不变：**在类型上让「不带撤销记录的 Trust」进不了 `Merge`**，而不是再修一次调用点。

### B. `TaskRun` 生产上根本不落盘 —— 这是 Spec C 的前置

`SaveTaskRun` / `ListTaskRuns`（`internal/storage/sqlite.go`）**没有任何生产调用方**，`internal/server/http.go` 的 `handleGetTaskResult` 注释自陈 "TaskRun is not persisted"。

Spec A 给 `task_runs` 加了 `stop_reason` 列并保证「写得进、读得回、老行不报错」，但**没接生产写入路径**——那是新增行为与新的失败模式（写库失败该终止任务还是记日志继续？），当时判定属独立决策。

**Spec C（后台子任务持久化）开工前必须先决定这件事。** 规格 §3.4/§3.7 已订正为如实描述，别把那两句当成已经能用。

---

## 一、Spec C 未开工：后台子任务持久化

三条值得抄的里剩这一条。今天 `RunSubTaskAsync` 起的后台子任务是**进程内的**，父进程退出即丢（`SubTaskHandle` 的 doc 自己写着 "process-local and non-durable"）。

deepseek-harness 的对应物是「可续后台子代理」：**落盘 Session + 进程内 Activation** 两层拆分——Session 活过进程，Activation 不活，冷恢复时重建。那个拆分很干净，是现成的图纸。

**前置**：§〇 B（`TaskRun` 生产写入路径）。
**依赖已满足**：Spec A 的终止原因已合入，落盘时能存「为什么停」。

**需要拍板**：写库失败时终止任务还是记日志继续；冷恢复的边界（恢复到哪一步算恢复）；后台子任务要不要外部可寻址的 id。

---

## 二、Spec A 遗留

- **`RunSubTasks` 的整批预检遇第一条非法即返回、只报一条**。一批里若有 3 条写错，模型要来回试三轮；`errors.Join` 聚合全部非法条目会更省轮次。**非规格要求，纯改进。**
- **batch 模式完全不读 `background`**（既有行为），而 `delegate_task` 的描述符仍向模型广告它；与 single 模式现在的硬拒不一致。
- `internal/runtime/multiturn_test.go:137` 的 `TestRuntimeBreaksRepeatedIdenticalToolCallLoop` 断言偏弱，最终复审分诊为**命名误导而非覆盖空洞**（它确实覆盖了东西，只是名字暗示的路径未必是它实际走的），**可留**。

## 三、Spec B 遗留

- **`EpisodeRecorder` 的写入量级**：具名子代理带 `EpisodeRecorder`（克隆子代理不带），**每次具名委派 = 1 条情景记忆 + 1 次蒸馏 LLM 调用**（上限 2000 字符）。已按裁决保留 + 双向用例 + 注释钉住。最终复审核实 `internal/storage/retention.go:97-100` 清理时**连 FTS 表一起删**，所以历史上那个「情景记忆污染要连 FTS 同删」的陷阱**已不适用**。**残留**：topK 稀释、每次委派的模型开销，值得在真实负载下评估一次。
- **`agent_resolver_test.go:761-777` / `:810-820` 两处既有用例用 `tools.Descriptors()`**。今天它们覆盖的是注册表装配、不是 `DisabledTools`，**不是假阳性**；但将来若被误用来断言 `DisabledTools` 相关行为会重演假阳性——**原因见 §五 那条教训**。
- **真机缺口**：`agent_id + role: orchestrator` 在真 `legion serve` 路径上只验到 unit 与 `internal/cli` 层，真模型 + 真 `agent.json` 下未跑。
- 其余 6 条 Minor 已分诊「可留」，逐条见 `.superpowers/sdd/specB-final-review-round2.md`（描述互相矛盾、未知 `DisabledTools` 未走真实 `ResolveDelegate`、`toolsets` 描述未提静默丢名、batch 的 `agent_id` 未 trim 而 single 有、「calls exactly once」是生产限定断言）。

## 四、更早的遗留（仍然有效，不重复展开）

见 [2026-09-07-open-items-handoff.md](2026-09-07-open-items-handoff.md)：

- **§一** S2 的三条验证缺口（真机 `agent serve` + 真联网清单从没跑过；损坏的 `revoked-ever.json` 是让插件挂不上还是让 serve 起不来未实测；GUI 仓是否真渲染三个 trust 字段跨仓未验）
- **§二** S2 的 11 条 Minor（全部「可留」，其中 Minor-8 是**改规格文字不是改代码**）
- **§三** S1 遗留（`trust/` 等第一位真实开发者——**已拍板，照办即可**；`ErrUntrustedList` 拆哨兵；两条不变量护栏；三条硬约束）
- **§四** S3 开发者申请流程未开工——**需要拍板**：申请走什么通道、审什么、清单怎么发布
- **§五** 机器状态：判据只有 WHEA 计数一直是 7

---

## 五、这一路最该带走的三条教训

### 1. 「规格明写、实现为零」**连续两个 spec 都是最终复审才抓出来的**

- **S2**：规格明写「清单拉不到时撤销仍硬拒」，实现与测试皆为零 —— 删掉 `trustlist.json` 就能让被撤销钥匙签的包照常挂载。
- **Spec B**：规格 §3.1 要求「工具描述里列出可委派的 agent 名」，零代码 —— 而规格**当初正是拿掉 `list_agents` 工具换的这条**，交易做了没交货。

两次的共同点：**基线全绿**（51 个包 ok、race 干净、vet/gofmt 都过），因为那条要求从头到尾没有任何代码，**所以没有任何测试会红**。逐任务复审全都没抓住，因为每个只看自己那块 diff。

**判据：最终复审必须拿规格逐节点名，问「这一条有没有任何代码实现」。不是审 diff，是审覆盖。**

### 2. 计划里的**测试断言**需要与产品代码同等的审查强度

这一路计划写错过三次断言，全靠变异实证当场揭穿：

- Spec A：`MaxToolRounds: 0` —— `NewRuntime` 把 `<=0` 规范成 `defaultMaxToolRounds`(4)，与 `config.Load` 那条「0=无限」**是两套语义**，重复守卫与熔断根本够不着。
- Spec A：docstring 写「It is the single judgment RunSubTask, RunSubTasks and RunSubTaskAsync share」——违反本仓「注释不得出现谁调用它」的硬约束，且一旦有第四个入口忘了接线就变成撒谎的注释。
- Spec B：让用 `tools.Descriptors()` 断言 `DisabledTools` —— **假阳性**。

**最后一条值得单独记住**：`DisabledTools` **从不烘焙进 `r.tools`**，只在 `effectiveTools()` 里现算 `tools.Without(...)`，所以 `Descriptors()` 无论 deny-list 生不生效都返回同一份未过滤列表。**要断言「模型实际拿到了哪些工具」，读 `InferenceRequest.Tools`，不要读 `Descriptors()`。**

同类的第二个坑：**Web 工具关着时，「收窄后 `fetch_url` 缺席」这条断言恒真**。所以那条用例现在带**阳性对照**——先断言不收窄时它确实在，对照不成立就 `t.Fatalf` 明说「下面的断言证明不了任何东西」。

### 3. 同一条不变量被绕过多次 = **守卫位置错了**，不是补丁不够

- **S2**：「撤销了某把钥匙后由它背书的实例就不该再服务」被绕过三次（pass 2 不判已挂载实例 → 判定寄生在磁盘包 `admit` 的成败上 → `restore` 把它挂回来）。终结办法是收口到 `revokedEndorsementOf(inst *instance)`——**签名收 `*instance`，让「问的是这个实例自己的背书」在类型上成立**。前两次的漏本质都是传参传错了对象。
- **Spec B**：`agent_id + role=orchestrator` 的硬拒最初裁在 `ResolveDelegate` 里，**绕过了批量整批预检** → 探针实测 entry 0 已经跑完 entry 1 才被拒，**部分执行**。而这是**静态可判**的条件，本该在 `validateSubTaskSpec`。

**判据：数清所有能到达那个状态的路径，逐条证明都过了同一个判定。** 收口之后要能说出「写入点恰好几处，各自被判过」。

### 附：变异实证的纪律（本轮又新增两条）

- 每条变异**必须 `go vet` 干净**——靠编译失败的变异什么都没证明。
- **哪条变异之后测试仍然全绿，那条规则就没人守着，必须补测试而不是放过。** 本轮靠这条补出的用例：batch 出口、async 入口接线、`max_rounds` 话术臂、四个常量字面值、nil 注册表分支、两处装配点、schema 条目、batch 的 `agent_id` 映射、`toolsets` 对自注册工具族的收窄。
- **用例先绿不等于假阳性**——Spec B Task 6 两条用例首次即绿（前面任务已做对），但变异仍逐条转红，说明它们是真载荷。
- **还原纪律**：每条变异从干净副本重新复制再打，还原后用 `md5sum` **且** `git diff`（或 `git status --porcelain` 逐字比对）确认回到提交态。**先用一条空变异验证 harness 真会报红**——本轮有两位复审者靠这一步各揪出自己脚本的一个 bug（Windows CRLF 翻译、还原不干净）。
- **测退出码不要接管道**：管道会让 `$?` 取到 `tail` 的状态。

### 附二：一条工具收窄的硬知识

`tool.Registry.Subset(names...)` 返回的是 **view**，而 `Registry.resolve` 的规则是「**自己注册的工具绕过 filter，只有继承来的才过 filter**」。

所以 `Subset` 与 `RegisterWebTools` / `RegisterBrowserTools` 等的**先后顺序就是全部**：先注册后 `Subset` → 收窄覆盖得到；先 `Subset` 后注册 → **整族逃过收窄**。当前 `agent_resolver.go` 里的顺序是正确的那个，且现在有用例守着（带阳性对照）。
