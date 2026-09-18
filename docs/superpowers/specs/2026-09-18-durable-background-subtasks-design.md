# 后台子任务持久化（Spec C）设计

**目标**：让一条后台子任务在进程退出后仍然**说得清自己停在哪**——按 id 查得到、状态诚实（`interrupted` 而不是永远 `running`）——而**不自动重跑**。

**背景**：`RunSubTaskAsync` 起的子任务今天是进程内的，`SubTaskHandle` 的文档自陈 "process-local and non-durable"；父进程一退出，在飞的子任务连同「它存在过」这件事一起消失。deepseek-harness 的对应物把「会话落盘、运行态不落盘」拆开，本设计取同一条线的浅档。

**已拍板的前提**（本设计不再重新论证）：

1. **写库失败 → 终止任务**。不记日志继续：一个声称可续的任务，状态没落盘就是在撒谎。
2. **后台子任务要有外部可寻址的 id**。
3. **冷恢复取 A 档**：重启后只把状态摆正，不重跑。B（从头重跑）与 C（断点续跑）都卡在同一件事上——工具不幂等，`write_file`、shell、外部请求重放一次就是做第二遍。重跑留给调用方显式发起，让人或模型对副作用负责。
4. **落盘范围取「所有任务」**：`RunTask` 一律落盘，后台子任务只是其中一类。
5. 重启后**全表扫**把 `running` 摆成 `interrupted`；子任务 id 用 **UUID**；结果查询出口改成 **`handleGetTaskResult` 读表**。

---

## 一、今天的事实（设计建立在这些之上）

| 事实 | 位置 |
|---|---|
| `SaveTaskRun` / `ListTaskRuns` 已存在，**没有任何生产调用方** | `internal/storage/sqlite.go:1131`、`:1149` |
| `task_runs` 只有 7 列：`id / task_id / agent_id / started_at / ended_at / result` + 迁移加的 `stop_reason` | `sqlite.go:2176`、`:1941` |
| `domain.TaskRun` 有 12 个字段——`ReasoningSummary`、四个 token 字段、`GeneratedFiles` **没有对应列，写进去就丢** | `internal/domain/types.go:106` |
| 幂等加列走 `columnMigrations`，只容忍 "duplicate column name" | `sqlite.go:1906`、`:1952` |
| `finishRun` 组装 `TaskRun`，**只在成功路径到达**；`RunTask` 主体另有 20 余处 `return domain.TaskRun{}, err` | `internal/runtime/runtime.go:1291`、`:661`–`:1245` |
| `handleGetTaskResult` / `taskResult` 从事件总线捞 `task_completed`，注释自陈 "because TaskRun is not persisted" | `internal/server/http.go:1232`、`:1283` |
| Runtime 今天只持有 `port.AuditLog` 与 `port.EventBus` 两个存储口 | `runtime.go:213`、`:214`；`internal/port/ports.go:138`、`:147` |
| 子任务 id 是 `<父任务>:sub-<进程内计数>`，重启后从 1 重数 | `internal/runtime/delegation.go:527` |
| **有一个消费方靠从 id 里解析父任务**：子任务结果回注 | `internal/cli/subtask_reinject.go:45`、`delegation.go:539` |
| 后台子任务在开 goroutine 前取任务边界票（`r.gate.BeginChild`），由唯一那个闭包归还 | `delegation.go:483` 一带 |

---

## 二、状态是封闭的四值枚举

```go
// RunStatus 是一次任务运行的生命周期状态。空串不是它的取值。
type RunStatus string

const (
    RunStatusRunning     RunStatus = "running"
    RunStatusCompleted   RunStatus = "completed"
    RunStatusFailed      RunStatus = "failed"
    RunStatusInterrupted RunStatus = "interrupted"
)
```

放 `internal/domain`，照 `StopReason` 的写法：带 `String()`，`ParseRunStatus` 遇到未知值**报错**，不设 `default` 兜底分支。

四值的分界：

- `running`：插入时的状态。**它只有两种正当结局**——被同一个进程改写成终态，或被下一次启动扫成 `interrupted`。
- `completed`：`finishRun` 走完。此时 `stop_reason` 必非空（`finishRun` 已有 panic 守着）。
- `failed`：这一轮自己报错结束（模型调用失败、工具循环报错、`ctx` 取消等）。**与 `interrupted` 必须分开**：前者是「跑到了一个失败的结论」，后者是「没人知道它跑到哪」。
- `interrupted`：进程没了。**只由启动扫描写**，运行期任何代码都不得写它。

---

## 三、表与迁移

`columnMigrations` 追加（每条都是 `ALTER TABLE task_runs ADD COLUMN ... NOT NULL DEFAULT ...`）：

| 列 | 类型 / 默认 | 说明 |
|---|---|---|
| `status` | TEXT, `'completed'` | 老行都带着 `ended_at`，本来就是已结束的运行；默认成 `running` 会让第一次启动扫描把历史数据全标成中断 |
| `parent_task_id` | TEXT, `''` | 空 = 不是子任务。父子关系从此**存在列里，不再编码进 id** |
| `background` | INTEGER, `0` | 是不是 `RunSubTaskAsync` 起的 |
| `goal` | TEXT, `''` | 子任务的 `SubTaskSpec.Goal`；A 档不重跑，所以**不存完整 spec**，只存这一句让人看得懂它在干什么 |
| `error` | TEXT, `''` | `failed` 时的错误摘要 |
| `reasoning_summary` | TEXT, `''` | 补 `TaskRun` 已有字段的缺口 |
| `prompt_tokens` / `completion_tokens` / `cached_tokens` / `total_tokens` | INTEGER, `0` | 同上 |
| `generated_files` | TEXT, `''` | JSON 数组，与 `conversation_turns` 的同名列同形（`internal/storage/project_turns.go:237`） |

`SaveTaskRun` / `ListTaskRuns` 的列清单同步补全——**这是本设计的第一条不变量：`TaskRun` 的每个字段都要有列、都要往返得回来**。今天缺的 6 个字段是静默丢失，不是设计。

---

## 四、新增存储端口

```go
// TaskRunStore 是任务运行记录的落点。
type TaskRunStore interface {
    // StartTaskRun 插入一条 running 记录。
    StartTaskRun(ctx context.Context, run domain.TaskRun) error
    // FinishTaskRun 按 id 字段级写回终态，不整行覆盖。
    FinishTaskRun(ctx context.Context, run domain.TaskRun) error
    // SweepRunning 把所有 running 记录改成 interrupted，返回条数。
    SweepRunning(ctx context.Context, at time.Time) (int, error)
    // TaskRun 按 run id 取一条；不存在时 found 为 false。
    TaskRun(ctx context.Context, runID string) (run domain.TaskRun, found bool, err error)
    ListTaskRuns(ctx context.Context, taskID string) ([]domain.TaskRun, error)
}
```

装配位置与 `audit` / `events` 相同（`internal/cli/command.go` 的 serve 装配），由同一个 `SQLiteRepository` 实现。

**字段级写回，不整行 UPSERT**：这仓吃过这个亏（任务状态落盘那次）。`FinishTaskRun` 只更新终态相关的列，`started_at`、`goal`、`parent_task_id` 不参与——它们在插入时就已定型，让一次结束写去重申它们，等于给「结束时手里那份不全的快照」一个覆盖开头的机会。

---

## 五、写入点，以及「数清所有出口」

### 5.1 开始

`RunTask` 在取得任务边界票之后、任何模型调用之前插一行 `running`。插失败 → **直接返回错误，任务不开始**。

理由是顺序而非偏好：先落盘后干活，这样「跑过」这件事不会因为一次写库失败而消失；反过来先跑后记，崩在中间就什么都不剩。

### 5.2 结束

- 成功：`finishRun` 里，在它已经组装好 `TaskRun` 之后写 `completed`。
- 失败：`RunTask` 每一条错误出口都要写 `failed`。

**这一条是本设计最容易坏的地方**，也是这仓反复复发的缺陷形状（「接缝在，但有一条路径没接上」）：`RunTask` 主体在 `runtime.go:661`–`:1245` 之间有 **20 余处** `return domain.TaskRun{}, err`。实现时必须逐条数清、逐条证明都经过同一个终态写入，而不是在看起来主要的那几条上各加一句。

**推荐做法**：在 `RunTask` 顶部用一个具名返回值 + `defer` 收口，让「无论怎么出去都写一次终态」在结构上成立，而不是靠 20 处各自记得。收口函数自身必须是幂等的（成功路径已经写过 `completed` 时不再覆盖）。

**写终态失败怎么办**：任务本来就要以错误结束，这时再叠一次写库失败——两条错误都要保住（`errors.Join`），不得只报后一条。任务成功但终态写失败时，按拍板 1：**任务整体报错**，不得返回一个「结果有了但没人知道」的成功。

### 5.3 后台子任务

`RunSubTaskAsync` 在 `go` 之前**同步**插行：

- 插失败 → 不起这个子任务，直接返回错误。这正好和已有的边界票逻辑同侧：票在 `go` 之前取，行也在 `go` 之前插，两者都在「父任务还确凿在飞」的那一刻完成。
- goroutine 结束时写终态，与它发 `subtask_completed` 事件在同一段收尾里。

---

## 六、启动扫描

serve 装配时（拿到 store、开始接任务之前）调 `SweepRunning`：

- 所有 `running` → `interrupted`，`ended_at` 记为扫描时刻。
- 记一条审计事件（`action` 例如 `task_runs_swept`）与一行日志，**带条数**。零条也要记：一个「从来没扫到过东西」的扫描和一个根本没跑的扫描，日志里必须分得开。
- 扫描失败 → serve 起不来。它是这份记录可信的前提：扫不动就意味着接下来所有 `running` 行的含义都是不确定的。

**已知取舍（写进实现注释）**：同一个 `agent.db` 被两个 serve 进程共用时，后启动的那个会把前一个正在跑的任务误标成 `interrupted`。本设计按「一个库一个 serve」的既有假设走；要支持共用，得给每行加 `instance_id` 并判断实例存活，那是另一件事。

---

## 七、id 与父子关系

- 子任务 id 改用 UUID（`github.com/google/uuid`，已在 `go.mod`）。
- 父子关系存 `parent_task_id` 列。
- `SubTaskHandle` 带上这个 id 并对外暴露——这就是拍板 2 的「外部可寻址」。

### 7.1 一个必须一起改的消费方

`internal/cli/subtask_reinject.go:45` 靠 `ParentTaskIDForSubTask` 从 id 字符串里解析父任务，把子任务结果回注给父任务。id 变成 UUID 之后，这条路直接断——**而且它断掉的方式是「父任务永远等不到回复」**，不是一个响亮的错误。

改法：`domain.RuntimeEvent` 加 `ParentTaskID` 字段，`subtask_completed` 事件发布时填上；回注改读这个字段。

**老事件怎么办**：`runtime_events` 是落盘的，真实库里存着改造之前发的事件，它们没有这个字段。所以回注按这个顺序判：

1. `ParentTaskID` 非空 → 用它。
2. 为空且 id 是老形态 `<父>:sub-<n>` → 走 `ParentTaskIDForSubTask`，这是**显式的老数据兼容，不是兜底**。
3. 两者都不成立 → 报错（保持今天的行为）。

`ParentTaskIDForSubTask` 因此**保留**，但降级成只服务老数据的解析器，文档写明这一点。

---

## 八、查询出口

`handleGetTaskResult`：先查 `task_runs`，查不到再回落到 `task_completed` 事件（同样是显式的老数据兼容）。落盘的记录**优先**——它是唯一能报出 `interrupted` 的来源，事件总线里根本没有「中断」这种事件。

返回体里增加 `status`。那句 "because TaskRun is not persisted" 注释删掉。GUI 不用改（新增字段，不改既有字段语义）。

---

## 九、测试要覆盖什么

1. **迁移幂等**：连跑两次 `applyColumnMigrations` 不报错；老行的 `status` 落成 `completed`。
2. **字段往返**：写一条字段全满的 `TaskRun`，读回来逐字段相等——钉住第三节那条不变量，防止再出现「有字段没有列」。
3. **开始写入失败 → 任务不开始**：注入一个必败的 store，断言 `RunTask` 报错，且**模型一次都没被调用**。
4. **结束写入失败 → 任务报错**：注入只在结束时失败的 store，断言成功路径也变成错误，且原始结果与写库错误两条链都在。
5. **每条出口都落终态**：对 `RunTask` 的错误出口做覆盖，断言库里那行不是 `running`。**这里必须做变异实证**：短路掉终态写入之后，必须有用例转红。
6. **扫描**：插两行 `running`、一行 `completed`，扫完断言前两行变 `interrupted`、第三行不动，返回条数为 2；再扫一次返回 0。
7. **后台子任务**：起一条、不等它结束就模拟重启（新 store 实例 + 扫描），按 id 查得到 `interrupted`。
8. **回注三条分支**：新事件走 `ParentTaskID`；老事件走老解析；两者都不成立时报错。**第三条要有阳性对照**——前两条都能正常回注，才证明第三条挡的是真东西。
9. **查询出口**：表里有记录时读表（含 `interrupted`）；表里没有、事件里有时回落。

---

## 十、明确不做

- **不重跑**，不论自动还是「重启后问一句」。
- **不落每轮对话**（那是 C 档）。
- **不做实例存活判定**（见第六节取舍）。
- **不动 `conversation_turns`**：它记的是对话轮次，与任务运行记录是两件事。
