# 委派结果的可判读性（Spec A）

**日期**：2026-09-07
**状态**：设计已确认，待写实施计划

## 一、这是什么

把 deepseek-harness 子代理机制里两条值得抄的做法落到 Legion 的委派路径：

- **A1 终止原因**：让「任务为什么停下来」成为一等信息，从主循环记录、落盘、出到委派结果。
- **A2 委派请求的前置校验**：非法的委派请求在**启动任何子代理之前**硬拒，不再静默降级。

来源对比记录在会话里，不重复搬运。deepseek 侧的对应物是 `SubagentStopReason`（终止原因联合类型）与 `SubagentCapabilities`（能力协商，start 前校验、缺能力抛 `UNSUPPORTED_CAPABILITY`、**绝不接受后忽略**）。

## 二、拆分与本 spec 的边界

三条值得抄的 + 一个已核出的缺口，拆成三个 spec，本次只做 A：

| Spec | 内容 | 状态 |
|---|---|---|
| **A（本文）** | 终止原因 + 委派请求前置校验 | 本次 |
| B | 按名字委派给已配置 agent（委派路径从未碰过 `agentregistry`，`agent_id` 只是标签且不在 `InputSchema` 里） | 未开工，安全相关 |
| C | 后台子任务持久化（今天进程内不落盘，父进程退出即丢） | 未开工，依赖 A 的终止原因 |

**本 spec 明确不做**：
- 不碰 `agent_id` / `agentregistry` / `toolauth`（Spec B）
- 不做后台子任务持久化（Spec C）
- **不建 provider / descriptor 抽象**，不建 `SubagentCapabilities` 四标志位。Legion 今天只有一种传输（进程内克隆父 runtime），为不存在的第二种实现付抽象成本不划算；将来真要接 ACP / Codex 再说
- 不动 `Registry.Without` 的语义

## 三、A1 终止原因

### 3.1 现状

主循环在 `internal/runtime/runtime.go:927`：

```go
for st.round < r.maxToolRounds && len(st.resp.ToolCalls) > 0 {
```

退出有四种原因，**代码里已经是四条独立路径**，却被压成一个 `loopCut bool`（`runtime.go:906`）：

| 原因 | 触发 | 今天的痕迹 |
|---|---|---|
| 正常结束 | `len(st.resp.ToolCalls) == 0` | 无 |
| 轮数耗尽 | `st.round >= r.maxToolRounds` | **完全没有** |
| 单工具熔断 | per-tool call cap（`capHit`，`runtime.go:1064`） | `loopCut = true` |
| 重复调用熔断 | identical call repeated（`runtime.go:1088`） | `loopCut = true` |

`domain.TaskRun`（`internal/domain/types.go:85`）没有任何终止原因字段，信息**从不离开 `RunTask`**。父任务、`/v1` 端点、审计全都答不出「为什么停」。

### 3.2 类型

新增 `domain.StopReason`，底层 `string`，四个常量：

| 常量 | 值 |
|---|---|
| `StopReasonCompleted` | `completed` |
| `StopReasonMaxRounds` | `max_rounds` |
| `StopReasonToolLoopCap` | `tool_loop_cap` |
| `StopReasonRepeatLoopBroken` | `repeat_loop_broken` |

`domain.TaskRun` 增加 `StopReason StopReason` 字段。

**`loopCut bool` 被这个枚举取代，不是并存**——并存就是第二套真相。

### 3.3 唯一出口与零值不变量

成功的 `TaskRun` **只有一个出口**：`finishRun`（`runtime.go:1240`，其注释自己写着 "done, not suspended, and returns the assembled TaskRun"）。挂起走 `ErrSuspended` 错误路径；其余 31 处 `return domain.TaskRun{}` 全是零值 + 错误，不产生成功的 run。

因此：

- **`StopReason` 在 `finishRun` 一处填写**，那是唯一落点。
- **零值（空串）的含义是「没人填」，即编程错误**。`finishRun` 在返回前断言非空，违反即 `panic`（按本仓 CLAUDE.md「不变量断言、绝不该到达的分支」的惯例）。
- **零值绝不许被当作 `completed`。** 这是本仓反复出现的「加了字段但某条路径忘了填」形状的直接防线。

### 3.4 落盘

`task_runs` 表（`internal/storage/sqlite.go:2168`）今天六列：`id / task_id / agent_id / started_at / ended_at / result`。

- 走既有 `columnMigrations`（`sqlite.go:1903`）加一列：
  `ALTER TABLE task_runs ADD COLUMN stop_reason TEXT NOT NULL DEFAULT ''`
- 补进 `sqlite.go:1133` 的 `INSERT INTO task_runs (...)` 列表与读取侧（`sqlite.go:1151` 的 `SELECT ... FROM task_runs`）。
- 老数据 `stop_reason` 为空串，**读出来是空值，不报错**。只有新任务有值。空值在**读取**侧是合法的历史状态，与 3.3 里「写入侧零值即 bug」不冲突：写入侧的断言拦的是新写入，读取侧要容忍旧行。

**注意这条落盘今天在生产上到不了**：`SaveTaskRun` / `ListTaskRuns`（`internal/storage/sqlite.go`）**没有任何生产调用方**，`internal/server/http.go` 的 `handleGetTaskResult` 注释自陈 "TaskRun is not persisted"。本 spec 只保证「这一列存在、写得进去、读得回来、老行不报错」，**不接生产写入路径**——让 `finishRun` 去调 `SaveTaskRun` 是新增行为与新的失败模式（写库失败该终止任务还是记日志继续？），属独立决策，留给 Spec C 作为前置。

### 3.5 出口

- `runtime.SubTaskResult`（`internal/runtime/delegation.go:41`）增加 `StopReason`。
- `delegate_task` 的 JSON 输出带上它：single 模式与 batch 模式**每条结果**都带（`delegation_tool.go` 的 `delegateJSON` / `delegateResultsView`）。

父于是能分清「子代理答完了」与「子代理撞了熔断、输出可能是残缺的」。

### 3.6 收尾话术

`runtime.go:1148-1155` 今天只有两句收尾提示，四个原因共用，且**存在一处既有不准确**：`runtime.go:1064`（单工具熔断）与 `:1088`（重复调用熔断）**都**置 `loopCut = true`，于是撞了单工具上限的任务，被告知的却是「检测到你在重复同一个工具调用」。

改为按 `StopReason` 分派收尾话术，**并改正这处不准确**：撞单工具上限就说撞了单工具上限。

这是**模型可见的 prompt 变更**，不是纯重构：话术变了，模型的收尾回答可能随之变。因此它要有**自己的用例**钉住「哪个原因配哪句话」，且**不与「原因判定」共用一个用例**——否则一个变异同时打两处规则，分不清是哪条在守。

### 3.7 受益面与风险

改的是所有任务共用的主循环，所以终止原因对**所有任务**都被记录下来，不只子代理。今天它的实际出口是 `SubTaskResult` 与 `delegate_task` 的工具输出（3.5）——父 agent 由此能分清子代理是答完了还是被截断了。

**`/v1` 与审计今天还答不出「为什么停」**：那需要先把 `TaskRun` 的生产写入路径接上（见 3.4 的注意事项），不在本 spec 范围内。

风险缓解：`StopReason` 只做**新增记录**，**不改变任何一条既有控制流分支**——哪条 `break`、什么时候 `break`，一律不动。唯一的行为变更是 3.6 那句收尾话术，且单独有用例。

## 四、A2 委派请求的前置校验

### 4.1 现状：两处静默降级

**（1）`toolsets` 传了不存在的工具名，被静默丢弃。**

`Registry.Subset`（`internal/tool/registry.go:240-246`）只是建一个 allow-map，未知名字无声无息。它与 `Without`（`:252`）不同：`Without` 的注释明写「禁用一个 agent 本来就没有的工具是合法 no-op」——那是**放宽**，无所谓；`Subset` 是**收窄**，名字拼错意味着子代理拿到的工具比调用方以为的少，**极端情况一个都没有**，而模型只会看到一个能力莫名其妙的子代理。

**（2）`background` 非法值静默当 false。**

`parseDelegateBool`（`delegation_tool.go`）只认 `1/true/yes/y`，其余一律 false。拼成 `"ture"` 就悄悄跑成前台阻塞调用。

**今天已经是 fail-loud、本次不动的三条**：`role` 非法值与深度超限（`delegation.go:71-78` 返回 error）、`tasks` JSON 解析失败（tool 层返回 `Success: false`）。

### 4.2 一个共用的纯校验函数

抽一个**纯校验函数**（不产生任何副作用、不创建任何 runtime），`RunSubTask` / `RunSubTasks` / `RunSubTaskAsync` **三处共用同一个**，不是各写一遍。

理由与 S2 里对撤销判定的收口裁决同源：**守卫要放在收口上；各写一遍就是第二套真相**。同一条不变量在 S2 里被三条不同通道各绕过一次，最后靠收口到一个共用判定才终结。

`newSubRuntime` 自己既有的深度与角色断言**保留**，作为最后一道防线。

### 4.3 校验项

只校验 Legion 真有的请求项：

| 项 | 规则 | 今天 |
|---|---|---|
| `goal` | 非空 | 已有，收进共用函数 |
| `role` | ∈ {`leaf`, `orchestrator`}（空串按 `leaf`） | 已有，收进共用函数 |
| 深度 | `depth + 1 <= maxSpawnDepth` | 已有，收进共用函数 |
| `toolsets` | **每个名字都必须存在于父的有效注册表**，用 `Registry.Descriptors()`（`registry.go:311`，沿 parent 链解析并按视图过滤）；**不能用 `Registry.HasTool`**，它只认本层自己注册的名字，会把继承来的插件工具与 `Subset` 视图里真实可调用的工具误拒 | **新增** |
| `background` | 必须是可识别的布尔字面量 | **新增** |

### 4.4 批量整批预检

**任一条目非法 → 整批拒绝，一个子代理都不启动。**

今天的批量在每个 goroutine 里才校验（`RunSubTasks` 内部调 `RunSubTask`），等发现第 5 条非法时，**前 4 个已经带着副作用跑起来了**。子代理有它自己的副作用（`delegate_task` 的描述符里写着 `Sensitive: true, // spawns sub-agents with side effects of their own`），所以这不是洁癖。

这与本仓插件那期的既有裁决同形（「key 校验期预检违反整批拒」），照搬。

**注意**：整批拒是**调度层**的失败，按 `RunSubTasks` 既有契约返回**整体 error**，而不是把校验失败塞进每条的 `Err` 字段。既有契约里 `Err` 是「这条子任务跑了但失败了」，不能与「这条根本没资格启动」混为一谈。

### 4.5 错误信息必须可定位

拒绝信息要点出：批量里**第几条**、**哪个字段**、**什么值**。未知工具名要**列出是哪个名字**（不是笼统一句「有工具名不认识」）。

## 五、测试

本仓最该防的形状是「**接缝在，但没人测那条接缝**」——上一期已复发八次，**每一次都靠变异实证抓出来，静态审查一次都没抓住**。

**每条规则都要变异实证**：把它在实现里失效掉（**`go vet` 必须干净，不能靠编译失败**），确认对应用例 FAIL，还原确认 PASS。**哪条变异之后测试仍然全绿，那条规则就没人守着，必须补测试而不是放过。**

必须有用例守的接缝：

1. 四个 `StopReason` 各自的判定 —— 尤其 `max_rounds`，它今天**在代码里没有任何痕迹**，是纯新增
2. **零值断言**：构造一条不填 `StopReason` 的成功路径 → 必须炸，**不许被当作 `completed`**
3. 迁移幂等：跑两次 `columnMigrations` 不出错
4. 老行读出来是空值而非报错（3.4 的读写不对称）
5. **写得进去且读得回来** —— 本仓的经典缺口形状（「写得出但读不回」/「字段填了但没人读」）
6. 收尾话术按原因分派，**单独用例**，与规则 1 的用例分开
7. **批量整批拒**：5 条里第 3 条 `toolsets` 名字不认识 → **一个子代理都没起**。断言副作用数为 0，**不是只断言返回了 error**
8. 未知工具名的拒绝信息点出**是哪个名字**
9. `background` 非法字面量硬拒
10. **三个入口共用同一个校验函数** —— 变异：只改坏其中一处的调用，三处的用例都得红

## 六、全局约束

- **fail-loud 铁律**（本仓 CLAUDE.md 最高优先级）：不许回落零值、不许吞错误、不许静默跳过、不许「拿不到就当没配置」。错误用 `fmt.Errorf("...: %w", err)` 包装，**错误点必须可定位**；错误被吞咽 / 终止的边界必须结构化记录。
- **注释是契约**：注释里每条事实陈述必须与代码一致，且**不得出现关于「谁调用它 / 有没有调用方 / 某个东西落没落地」的句子**。凡是提到别的文件 / 常量 / 包行为的句子，**亲自去那个文件确认再写**。上一期在这上面被抓到七条以上 finding。
- **不得引入包级可变量**。
- **不改变任何一条既有控制流分支**（3.7）。
- 全绿判据：`go test ./... -count=1 -p 1 -timeout 900s`；`gofmt -l .` 为空；`go vet ./...` 干净。**测退出码不要接管道**（管道会让 `$?` 取到 `tail` 的状态）。
- 只按**显式路径** `git add`，**不用 `git add -A`**。提交信息正文中文，结尾一行 `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`。

## 七、涉及文件

| 文件 | 改动 |
|---|---|
| `internal/domain/types.go` | 新增 `StopReason` 类型与四常量；`TaskRun` 加字段 |
| `internal/runtime/runtime.go` | 主循环记录终止原因；`finishRun` 填写并断言非空；`loopCut` 被枚举取代；收尾话术按原因分派 |
| `internal/storage/sqlite.go` | `columnMigrations` 加一条；`task_runs` 的 INSERT 与 SELECT 补列 |
| `internal/runtime/delegation.go` | `SubTaskResult` 加 `StopReason`；新增共用校验函数；`RunSubTasks` 整批预检 |
| `internal/runtime/delegation_tool.go` | `background` 严格解析；输出带 `stop_reason` |
