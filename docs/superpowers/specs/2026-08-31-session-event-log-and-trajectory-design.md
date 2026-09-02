# 会话事件日志与轨迹视图 —— 设计

**日期**：2026-08-31
**状态**：设计已确认，待实现计划
**来源**：轨迹视图照搬 deepseek-harness（`packages/client/ui-trajectory` + `packages/session/session-persistence*`）的做法。

---

## 1. 要解决什么

现在的「事件」页是 `runtime_events` 的平铺列表：一行 `type` + 一句截断的 `message`。它答不了「这个任务到底做了什么」——因为**工具调用与结果从来没有被记录过**：

| 存储 | 有什么 | 缺什么 |
|---|---|---|
| `conversation_turns` | user/assistant 的 content、token、生成文件 | 没有 tool 角色，没有调用参数/结果 |
| `audit_events` | 动作名、时间、origin、token | 没有载荷——只记「发生过」 |
| `runtime_events` | type / task_id / 一句 message | 现在的「事件」页 |

目标是做出截图里那种**轨迹**：按 turn 分组，USER / ASSISTANT / TOOL 逐条铺开，TOOL 行显示 `命令 + 参数 → 结果预览`，顶部有 Duration/Turns/Calls 与密度时间带，可搜索。

## 2. 借鉴对象的形状

deepseek-harness 的轨迹**不是独立存储的东西，而是同一条事件流的另一种投影**。

- 事件源：一条只追加、带单调 `seq` 与 `time` 的 `SessionEvent` 日志。词表：`turn/start` · `step/start` · `user/message` · `assistant/chunk` · `assistant/message` · `tool/call` · `tool/result` · `step/end` · `turn/end`。
- 存储（SQLite 后端）：一行一事件 `(session_id, seq, type, time, data, source_event_seqs, surface_op)`，`data` 是载荷 JSON 原文。
- 大输出不进事件：`spill` 家族把超限的工具输出落到会话作用域的文件，事件里只留**有界预览 + 检索定位符**。
- 崩溃恢复：不截断。保留半个 turn 的真实事件，**补合成关闭事件**，让重建出的历史仍是合法的 provider transcript。
- checkpoint 策略（与存储分离）：模型请求前、有外部副作用的顶层工具体执行前、每个 step 边界刷盘，**fail-closed**。
- 投影：纯 `init/apply/view` 单元，框架驱动，`stateVersion` 作缓存失效锚。

聊天视图与轨迹视图是同一条日志的两个投影——它的轨迹「免费」，因为事件流本来就长这样。

## 3. 已定的取舍

| 编号 | 决定 | 理由 |
|---|---|---|
| A | 新增 append-only 会话事件日志 | 唯一能同时喂聊天与轨迹、且事后可完整回放的形态；顺带补上「工具调用从不落盘」这个既有窟窿 |
| A2 | 事件日志是**唯一真相源**，`conversation_turns` 退役 | 不留两个真相源 |
| B3 | 不迁移历史数据（用户会清库重置） | 无历史包袱 |
| C1 | **服务端**投影 | 模型侧消费者在服务端，投影必须在那儿存在；再在前端写一份就会漂移 |
| D | 大载荷走 spill，事件只存预览 + 定位符 | 与 harness 一致；Legion 已有同构实现（`internal/runtime/toolcache.go`） |
| E1 | 抄契约，不抄机制 | 不变量与 checkpoint 才值钱；注册表是为它的插件生态服务的，Legion 没有那个生态 |
| F1 | 委派子任务写**自己的**会话日志，父日志只留那一次 `tool/call`/`tool/result` | 层级维持可读性；深委派拍平会把父轨迹淹掉 |
| G3 | 模型上下文含历史工具往返 = **可配置，默认关** | 它改的是每次请求的体积，不该在做轨迹的顺路上悄悄打开 |
| H1 | `search_session` 改为对事件建 FTS5 索引 | 单一真相源；顺带让工具调用与结果可搜 |
| I1 | 轨迹是**对话栏上方的标签页**，与对话互斥 | 轨迹是「回头看整件事」的专注动作，且需要横向空间放 `命令 → 结果` |

## 4. 事件模型与存储

### 4.1 词表

| 事件 | `data` 载荷 | 何时发 |
|---|---|---|
| `turn/start` | `{turn}` | 收到一条用户输入 |
| `user/message` | `{content, images?}` | 同上 |
| `step/start` | `{turn, step}` | 每次准备发模型请求前 |
| `assistant/message` | `{turn, step, content, tool_calls[], usage{prompt,completion,cached,total}, model_profile}` | 模型响应装配完成 |
| `tool/call` | `{turn, step, call_id, name, arguments}` | 派发前 |
| `tool/result` | `{turn, step, call_id, preview, spill_locator?, is_error, duration_ms}` | 工具返回 |
| `step/end` | `{turn, step, reason}` | 每步结束（含失败/取消，`finally` 里发） |
| `turn/end` | `{turn, reason}` | 轮次结束；恢复补出的那条带 `{interrupted:true}` |

**取值约定**（写在这里，免得实现时各猜各的）：

- `turn` 从 0 起、**每会话**单调递增，不随任务或进程重置；`step` 从 0 起、**每 turn** 内重置。
- `step/end` 的 `reason` ∈ `completed | failed | cancelled | max_tokens`；`turn/end` 的 `reason` ∈ `completed | failed | cancelled | interrupted`，其中 `interrupted` **只**由崩溃恢复补出。
- `spill_locator` 是**工具根相对路径**（与 `read_file` 同源，见 `toolcache.go`），不是绝对路径——绝对路径会随安装位置漂移，且泄漏本机目录结构。事件里不存全文长度之外的任何全文内容。
- `preview` 的截断上限复用现有截断治理的配置，不新引入一个旋钮。

**不抄 `assistant/chunk`**：harness 存它是为逐 token 回放；Legion 的流式已走 SSE，事后逐 token 回放价值存疑，而它把写入频率抬高一个量级。词表可扩展，将来要加不必改表。

### 4.2 表

```sql
CREATE TABLE session_events (
  session_id TEXT    NOT NULL,
  seq        INTEGER NOT NULL,   -- 每会话单调、连续
  type       TEXT    NOT NULL,
  time       INTEGER NOT NULL,   -- unix millis
  data       TEXT    NOT NULL,   -- 载荷 JSON 原文
  PRIMARY KEY (session_id, seq)
);
```

去掉 harness 的 `source_event_seqs` / `surface_op`（服务于它的 surface 机制，Legion 没有）。会话元数据继续用现有 `agent_sessions`，不另起 sessions 行。

### 4.3 六条不变量

1. **只追加、seq 连续**：`Append` 的首个 seq 必须等于库里的 next-seq；整批在**一个事务**里，中途失败整批回滚。
2. **崩掉的 turn 补齐而非截断**：`Load` 保留半个 turn 的真实事件，为每个未答的 `tool/call` 补一条 `is_error` 的 `tool/result`，再补 `step/end` + `turn/end{interrupted:true}`。**重建出的历史必须是合法的 provider transcript**，否则下一次请求会带着「有调用没结果」的消息数组发给模型。
3. **中间断裂 = 损坏，拒绝**：seq 有洞或某行解析不了就报错，不静默跳过。只有从未写完的**尾部残片**可以丢。
4. **未知事件类型 = 拒绝**，并指名类型与会话。
5. **懒物化**：没有事件的会话不在事件表里留痕。
6. **大载荷不进事件**：`tool/result` 只存预览 + spill 定位符。事件表的增长与**调用次数**成正比，不与输出体积成正比。

### 4.3.1 补出来的事件排在尾部——这给 P2/P3 留下四条硬约束

追加式日志**不可能插入**：`Load` 只能把合成的关闭事件追加到末尾。当一个未答调用属于**已经收尾**的
step/turn 时（它的 `step/end`/`turn/end` 已在日志里），补出的 `tool/result` 就排在自己那次调用的
收尾事件**之后**。P1 的实现与测试已确认这一点（见 Task 5 复审 I3）。

于是四条约束，不写下来就会在后面两期重现（第 3 条讲的是「什么时候可以调 `Load`」，第 4 条是
final-review-2.md C-1 修复时补的）：

1. **P2（发射点）**：每一条记录过的 `tool/call` 都必须有对应的结果事件——工具失败、取消、被拒绝
   同样要发 `tool/result{is_error:true}`。只有**进程硬崩**才允许留下未答调用，而那种情况下 `step/end`
   也不会发出，合成事件追加到尾部恰好是对的位置。
2. **P3（投影）**：`projectTurns` 必须**按 `call_id` 配对** assistant 的 tool_calls 与 tool 消息，
   **不得按 seq 线性排布**。按位置投影会在上面那种日志上产出非法 transcript（尾随的 tool 消息前面
   没有相邻的 assistant tool_calls；或两次调用与两次结果交错），provider 会直接 400。
3. **P2（会话生命周期）**：`Load` **只可对「确定没有活跃写入者」的会话调用**——进程启动时的崩溃恢复，
   或一个已经结束的会话。存储层看得见的只有事件本身，而「崩掉的半个 turn」与「正在跑、还没收尾的
   turn」在数据上完全等价，它没有任何办法区分：对一个活着的会话调 `Load`，会往那个进行中的 turn 里
   注入 `tool/result{is_error}` + `step/end` + `turn/end{interrupted}`，把一段还在正常进行的历史
   记成中断。判断「会话是否在跑」的信息只存在于 P2 手里，所以这条约束必须由调用方保证。
   （P1 已把同会话的「读-规划-追加」放进同一把 per-session 写锁，所以**并发 `Load` 之间**是安全的；
   这条约束管的是 `Load` 与该会话**正在进行中的写入**并发，那是 P1 层面修不掉的。）
4. **P2（发射点）**：**同一个 step 内、还未被应答的 `tool/call` 不得复用 `call_id`**。原因是 provider
   的 tool call id 只保证**单次响应内唯一**，跨一整条长会话唯一并不是所有 provider/本地模型都保证
   （按序号生成 `call_1`、`tooluse_0` 的实现是存在的）。`Load` 的恢复检查只拦「会真的产出冲突」的那
   种复用——**两个同时未被应答**的 `tool/call` 共用一个 `call_id`，因为那时补出的两条 `tool/result`
   会带着同一个 `call_id`，破坏第 2 条的按 `call_id` 配对。**跨 turn/step 的复用是允许的**：只要前一次
   调用已经应答（有自己的 `tool/result`），它就不再进恢复要补的未答集合，天然不会与后一次复用产生
   合成冲突。这条约束只由 P2 保证——存储层看不到「这个 id 是不是同一个 provider 响应里发出的」，只
   能在事后按「是否被应答」判断，判断依据见 `internal/storage/session_events.go` 的 `planRecovery`。

### 4.4 接口

```go
type SessionEventStore interface {
    Append(ctx context.Context, sessionID string, events []SessionEvent) error
    ReadFrom(ctx context.Context, sessionID string, fromSeq int64) ([]SessionEvent, error)
    Load(ctx context.Context, sessionID string) ([]SessionEvent, error)
}
```

`Load` **会改库**（补恢复事件）；`ReadFrom` 不改库、只读后缀，供轨迹分页与增量拉取。

**seq 分配**：每会话一个串行写入器（per-session 写锁 + 内存 next-seq 游标，游标只在锁下推进）。同会话并发写（工具并行返回、审批恢复与新消息相撞）必须经同一条串行链。

## 5. 发射点与屏障

```
提交任务
  ├─ turn/start + user/message            ← 收到用户输入
  └─ for round < maxToolRounds:
       ├─ step/start                       ← 准备发请求
       ├─ 【屏障 1】flush                   ← 请求前
       ├─ 模型推理
       ├─ assistant/message                ← 响应装配完（含 usage）
       ├─ for each call:
       │    ├─ tool/call                   ← 派发前
       │    ├─ 【屏障 2】flush              ← 进工具体之前
       │    ├─ 执行工具
       │    └─ tool/result                 ← 返回（预览 + spill 定位符）
       ├─ step/end   （defer，失败/取消也发）
       └─ 【屏障 3】flush                   ← 下一轮请求派生前
     turn/end
```

落点在 `internal/runtime`（`runToolLoop` 及其上下游），**一处都不在工具实现里**。

**三个屏障 fail-closed**：刷不动就不发请求、不进工具体、不开下一步。这是行为改变——数据库写不动时任务会失败而不是照跑。理由在屏障 2：`tool/call` 必须先落盘，否则崩在工具体里就成了「工具真的执行过、但日志里没有这次调用」，恢复时补不出合成结果，不变量 2 失效。工具是有外部副作用的那一端；「先记录再执行」保证任何真发生过的副作用在日志里都有它的调用。

**必须先回答：这条接线有几条任务路径？** 本期栽过一次——插件工具与审批仲裁者只接了 per-agent resolver，**默认任务路径没接**，而默认路径服务大多数任务。事件发射点同理：`runToolLoop` 之外还有审批挂起后的 resume 路径、委派子任务路径。实现第一步是把路径数点清楚，每条各有一个「事件确实发出来了」的测试。

## 6. 投影与退役

```go
func projectTurns(events []SessionEvent) []domain.ConversationTurn
```

`user/message` → user turn；`assistant/message` → assistant turn（content + usage 四件套 + model_profile）。**返回类型不变**，于是五个模型侧消费者（`runtime` 多轮 messages、`cognitive` 提示词段、`sessioncache`、`agent_resolver`、`/turns` handler）一行都不用改，改的只是它们拿到的东西是怎么算出来的。

**退役**：删 `conversation_turns` 表与其写入方；`/v1/sessions/{id}/turns` 改为「读事件 → 投影 → 返回」。

**投影缓存先不做**：harness 的 `(sessionId, key, ver, seq, val)` 缓存服务于多投影单元的注册表；我们只有一个投影、会话长度有界，先每次读时算，测出慢再加。

**G3 开关**：`projectTurns` 可选产出含 `tool/call`/`tool/result` 的完整 transcript，配置项默认**关**。打开时的消息形状是 provider transcript 的标准形状——assistant 消息带 `tool_calls`，其后跟与之 `call_id` 对应的 tool 消息，内容是**预览**（不是全文，全文仍靠 `read_file` 按定位符取）。打开后模型在会话恢复时能看见历史工具往返（不再失忆），代价是每次请求体积可能涨数倍。它是一次单独的、可度量的决定。

**搜索（H1）**：`search_session` 改为对事件建 FTS5 索引（该仓已用 FTS5 做情景记忆）。搜的是事件载荷，因此工具调用与结果一并可搜。

## 7. API 与前端

| 口子 | 用途 |
|---|---|
| `GET /v1/sessions/{id}/events?from_seq=&limit=` | 拉原始事件（走 `ReadFrom`，只读后缀、不触发恢复）。轨迹首屏与翻页 |
| 现有 `/v1/events` SSE 增加 `session_event` 帧 | 实时追加。帧带 `session_id` 与 `seq`；前端按 seq 连续性判断是否漏帧，**漏了从断点补拉**而不是猜 |

不新开流：GUI 已有 SSE 桥（`sse_bridge.go` → Wails 事件），再加一条通道等于多一套重连/鉴权/断线语义。

**详情读取**：`tool/result` 只有预览 + 定位符，展开时按定位符取全文。Legion 已有根限定的 `/v1/files`；spill 落在 `toolRoot/cacheDir` 下，**能否直接被它服务需在实现时确认——根不同源就要补**。这正是本期反复栽的接缝类问题，写进计划逐条验。

> **P4a 的答案（2026-09-02，已用端到端测试钉住）**：**同源，不用补**——但有一个前提。
> 任务的 `WorkingDir` 继承自会话（`server/http.go:1070`），而 `/v1/files` 的根就是
> `session.WorkingDir`（`http.go:744`），所以定位符可以原样交给
> `/v1/files?session_id=<sid>&path=<locator>`。
>
> **前提是会话绑定了 `working_dir`。** 未绑定时 `toolRoot` 回退到 `ContextFiles.Root`，
> spill 落在那里并产出一个非空定位符，而 `handleServeFile` 对空 `WorkingDir` 直接 404——
> 那个定位符**取不回来**。这是有意不修的：让它在不可服务时返回空串等于「有全文却说没有」，
> 是另一种不诚实。**P4b 必须把 404 当成「全文不可得」的合法结果渲染，不是错误弹窗。**

**前端组件**（照抄 harness 的切分，不抄它的框架）：

```
TrajectoryView
├─ TrajectoryToolbar    Duration / Turns / Calls + 搜索框
├─ TrajectoryTimeline   顶部三条密度带（Input / Model / Tools）
└─ TrajectoryTable
   └─ TrajectoryTurn（按 turn 分组，左侧 "Turn N" 标记）
      └─ TrajectoryCell   USER / ASSISTANT / TOOL 行：徽章 + 摘要 +（TOOL）→ 结果预览
```

- **位置**：对话栏上方的标签页（「对话 / 轨迹」），与对话互斥。轨迹落地后，右侧状态栏现有的「事件」标签退休——它是同一批数据的贫瘠版本。
- **搜索**：客户端在已加载事件里搜，与 harness 一致。与服务端 FTS5 不共用：用户搜的是「我刚看到的这些」，模型搜的是「整个历史」。
- **虚拟滚动先不做**：靠 `limit` 分页压住每屏事件数；测出卡顿再加（harness 有 `trajectory-virtual-rows.ts` 可参考）。

## 8. 测试与验收

| 断言 | 变异掉什么会红 |
|---|---|
| 首 seq ≠ next-seq 时拒绝 | 去掉连续性检查 |
| 批中途失败整批回滚 | 去掉事务 |
| 中间 seq 有洞 → 报损坏 | 改成静默跳过 |
| 未知事件类型 → 拒绝并指名 | 改成忽略 |
| **崩溃恢复补出合法 transcript** | 去掉合成关闭事件 |
| 三个屏障 fail-closed | 让 `Append` 失败，断言**模型没被调用、工具体没进过**（不是只看返回了 error） |
| 并发写 seq 仍连续 | 去掉串行链；配 `-race` |
| `tool/result` 存的是预览不是全文 | 关掉 spill |
| 每条任务路径都真的发了事件 | 只接一条路径 |

**最要紧的是崩溃恢复**：整套设计里唯一「平时看不出、出事才致命」的部分。测法是写半个 turn（有 `tool/call` 没 `tool/result`）→ `Load` → 断言补出的序列能原样喂给模型而不报「调用无结果」。

**真机验收**：跑一个真任务 → 打开轨迹 → 看到按 turn 分组的 USER/ASSISTANT/TOOL 行，TOOL 行有 `命令 → 结果预览`，展开能取到全文。

## 9. 分期

| 期 | 内容 | 判据 |
|---|---|---|
| P1 | 事件表 + Store + 六条不变量 + 崩溃恢复 | 单测全绿，变异逐条验红 |
| P2 | 发射点接入（**枚举所有任务路径**）+ 三个屏障 fail-closed | 跑一个真任务，库里事件序列完整且平衡 |
| P3 | 投影 + `/turns` 改读事件 + 删 `conversation_turns` + FTS5 搜索 | 五个模型侧消费者行为不变；`search_session` 能搜到工具调用 |
| P4 | `/events` 口子 + SSE 帧 + 轨迹前端 | 界面上看到截图那样的轨迹 |
| P5 | G3 开关（默认关） | 打开后 token 体积变化可度量 |

**P1–P3 不可跳**：P3 删表之前，P2 必须已在真机上写出完整事件流，否则删掉的是唯一能用的那份数据。

## 10. 明确不做（都留了口子与触发条件）

| 不做 | 什么时候重新考虑 |
|---|---|
| `assistant/chunk` 入库 | 需要事后逐 token 回放流式过程时 |
| 投影缓存表 | 投影在真实会话长度上测出慢时 |
| 前端虚拟滚动 | 分页之后仍卡顿时 |
| 投影注册表 / 变更 feed | 出现第三个投影消费者时——那时它是有依据的抽象，现在是猜的 |
| 历史数据迁移 | 不做。用户清库重置（B3） |
