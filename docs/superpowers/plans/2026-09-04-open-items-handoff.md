# 未完事项接续（2026-09-04）

**这份文档的用途**：会话事件日志 P1–P5 那一期已经收尾，随后的清账（真机验证、遗留 Minor 分诊、三条拍板项）也做完了。这里只写**还没做的事**，每条都说清「为什么它还在、下一步具体做什么、需不需要人拍板」。给下一个接手的人看。

**三仓 tip**：`stardust-agent-server` `c118723` / `stardust-agent-gui` `4d35a3a` / `docs` `8c9202d`。工作树均干净，**三仓零开放 PR**。

**2026-09-04 二次更新**：§二 的 A（注释订正）与 D（`internal/plugin/fetch` 的 Windows 锁抖动）已做完并合入 master（server#154 / server#155，master CI 双绿）。两条都推翻了这份文档原本的判断，改动记在各自小节里；新长出来的一条待办见 §二 E。

**上两份**：[2026-09-03-session-event-log-handoff.md](2026-09-03-session-event-log-handoff.md)（那一期的完成记录与教训，§七 值得先读）、[2026-08-31-open-items-handoff.md](2026-08-31-open-items-handoff.md)（浏览器/插件那期，§三 的四条拍板项仍未决）。

---

## 一、本轮清账已全部合入

会话事件日志 P1–P5 收尾之后的清账（真机验证、遗留 Minor 分诊、三条拍板项）到此结束，
**没有在飞的分支**。最后合入的 server#153 是三条拍板项：

| 拍板 | 实现 |
|---|---|
| 度量口径 → **新开字段** | `total_tool_call_chars` / `tool_call_chars`，不并进 `total_content_chars`（后者是既有日志口径，并进去新旧日志不可比） |
| `limit` → **连 SQL 下推一起做** | 契约加 `limit`（`<=0` 不限），五处热路径传 0 行为逐字不变；端点上界 5000 **拒绝**而非夹紧 |
| 取消时 `turn/end` → **单独一次** | `closeTurnOnError` 改用 `context.WithoutCancel(ctx)` |

本轮合入的 PR：#141–#153（含 D-1 删表、D-2 熔断边界、T-1 e2e 可诊断化、T-2/T-3 配置
可见性、T-5 schema 降级守卫、遗留 Minor 三条）。

**二次更新补记**：此后又合入 #154（§二 A 的注释订正）与 #155（§二 D 的 Windows 锁争用），
两条都在 master CI 上跑绿；本地在 master 上另跑过 `go test -count=20 ./internal/plugin/fetch/`
（修前必红的那个 count）与 `go test -p 2 ./...`（ok=50 FAIL=0）。

## 二、能直接做，不需要拍板

### A. 注释订正一批（9 条）——**已做完，server#154 合入 `738b955`**

本仓把注释当契约用，而上一期光「注释里的事实陈述不实」就栽了 5 次。下表是当初列的
清单，**逐条核到行之后有三处与这份清单本身不符**，先看这三条再看表：

1. **「`turn.TaskID == task.ID` 今天是空转」不成立。** 「HTTP 层在入队前写 user turn」
   那半条确实是假的（`touchSessionOnSubmit` 自己写着不再写 prompt），但那条 filter
   **不是死代码**：`Coordinator.runResume` 会为同一个 task 重新 `ResolveTaskRunner`、
   因而重读历史，而它上一次运行已经用同一个 `task_id` 落过 `user/message`。当成死代码
   删掉，会让 resume 把上一轮的提问重放给模型。
2. **`Seq` 的 `omitempty` 没有按「1 行」改掉。** 去掉它会让每条生命周期事件凭空长出
   `"seq":0`——正是 `TestTranslateLeavesNonSessionEventsWithoutSessionFields` 在 SSE
   侧明令禁止的形状。改成把代价写进注释：`/v1/runtime-events` 列表会丢 `seq=0`，要 seq
   连续性就读 SSE 帧或 `/events?from_seq=`（两者恒带）。**若要改行为，需单独拍板。**
3. **「示例描述的 `conversation_turns` 表已随 D-1 删除」不成立**——示例里根本没出现过
   表名。截断口径那半条是真的，已改。

另外顺带记下一个**只写进注释、没改行为**的形状缺陷：G3 打开 + resume 时，transcript
会重放这个 task 上一轮的 user 消息，`appendCurrentInput` 又把同样的输入贴一次，模型
因此看到自己的问题两遍。`RecentTurnsForTask` 那条路按 TaskID 过滤掉了；transcript 这条
路的投影里没有 `task_id`，过滤不了。

清单原文（均已订正）：

| 位置 | 问题 |
|---|---|
| `internal/runtime/session_turns.go:357` | 说「HTTP 层在任务入队前先写 user turn」——**全仓没有这个写入方**。真正写 `user/message` 的只有 `runtime.go:726` 与 `cli/command.go:1344`，`http.go:1137` 自己的注释就写着改成了「recordUserMessage when the task actually runs」。于是 `:386` 那条 `if turn.TaskID == task.ID { continue }` 今天是空转。**要害是同文件 `:90-97` 已经把真相写对了，两段注释直接矛盾** |
| `internal/runtime/runtime.go` 取舍注释 | 一个字没提「历史第一条几乎总是 user，而 `messages[0]` 也是 user，于是常规形状里有连续两条 user」 |
| `internal/tool/builtin.go:40/45` | 把截断归给 `appendToolResults`，那段逻辑已经搬走 |
| `internal/eventbridge/eventbridge.go:110-112` | 描述了一个「按 subject 过滤」的能力，而 `events.go:40` 只有 `?type=` |
| `internal/runtime/eventlog.go:448-449` | 回退链写成「回退到 `ContextFiles.Root`」，真实链是 `app/app.go:386-392` 的 `WorkingDir → ToolRoot → "."`，对 server 顶层任务不准 |
| `internal/server/http.go:567-570` | `sessionIDFromPath` 没说它现在是 PATCH/DELETE 的路由判据（`:352/354` 白名单）——**放宽它等于删库**，这个前提必须写下来 |
| `internal/server/openapi_coverage_test.go:158-170` | `negated` 恒为空、成了死代码，注释还指着 P4a 已换掉的 `!HasSuffix("/turns")` |
| `domain.RuntimeEvent.Seq` 的 `omitempty` | 让 `/v1/runtime-events` 丢掉 `seq=0`（1 行） |
| `configs/agent.full.example.json:78` | 「存储前截断」错（`cli/command.go:1409` 明写存全文、截断是读侧），且它描述的 `conversation_turns` 表已随 D-1 删除；「追加 `[truncated]` 标记」也只在一条路上成立 |

前两条最该先做——**它们是两段互相矛盾的注释**，下一个人读到哪条全看运气。

### B. GUI 插件面板人眼走查（上一期 T-2）

`wails dev` 起 GUI，人眼过一遍插件面板。上一期记的原因仍成立。

### C. `wails dev` 手动验证（上一期 T-3，累积）

GUI 侧几期改动累积下来的手动验证，一直没做。

### D. `internal/plugin/fetch` 的 Windows 文件锁抖动 ——**已做完，server#155 合入 `c118723`**

**不是抖动，是生产代码的平台语义漏判，约 2% 复现率。**

`lockDigestDir` 用 `O_CREATE|O_EXCL` 创建锁文件，并把「不是 `ErrExist`」的错误一律当致命。
Windows 上这条判据是错的：删除一个文件不一定立刻解除名字占用——只要还有句柄未关，名字处于
**delete-pending** 状态，此时对它的 `CreateFile` 返回 `ERROR_ACCESS_DENIED`（errno 5），
而不是「已存在」。一个 writer 释放锁的过程因此就是一个窗口，另一个 writer 的创建会在窗口里
看到 errno 5，而那把锁只是**被人持有**。

取证（12 goroutine 争用同一路径的独立探针，不碰被测代码）：
`ErrExist 28160 / acquired 4189 / errno=5 611`——约 2% 的争用创建走这条假失败路径。

**复现条件比这份文档原本记的宽得多**：不需要全量并发，`go test -count=20 ./internal/plugin/fetch/`
单包必现，命中 `TestCache_ConcurrentPut_OverAnIncompleteEntry_NeverDeletesAPublishedPackage`。
原记录「单独重跑 3 次没复现」是**采样不足**，不是证据。

修法：`lockCreateIsContended`（`cache_lock.go`）+ build-tag 两份平台文件。Windows 把
`ERROR_ACCESS_DENIED` 并入等待；POSIX **一条不加**——那边 `EACCES` 就是权限问题，等满 30 秒
再报等于把权限问题变成挂起。代价（真打不开的锁文件要等满 `lockWait` 才报）由超时消息带上
最后一次创建错误来兜，否则权限故障会被报成一个不存在的锁持有者。

### E. `internal/taskledger` 有同形状的缺陷（D 的副产品，**未做**）

`internal/taskledger/ledger.go:495` 用同样方式取锁、同样「非 `ErrExist` 即致命」，在 Windows 上
有同样的暴露面。没有在 server#155 里一并改，因为它的重试策略不同（最多两次 + 按 mtime 判陈旧），
**errno 5 不是关于锁年龄的证据**，不能直接喂进陈旧判定，要单独设计。照 `internal/plugin/fetch`
的三个文件抄形状即可，但争用锤测要按它自己的策略重写。

---

## 三、需要拍板

### 上一期 §三 的四条，仍未决

见 [2026-08-31-open-items-handoff.md](2026-08-31-open-items-handoff.md)：

- **D-1** 界面上那次询问（触发浏览器安装 + 显示进度）
- **D-2** 签名清单：真正的「不发新版也能换脚本」
- **D-3** 代码签名 / macOS 公证 —— 需要证书，不是纯技术决定
- **D-4** 工具结果的图像通道 → set-of-marks

### 本轮新增

- **`restore_latest` / `Append` 的触碰面** —— 设计待议，一直没展开
- **P4b 的 Minor 项未分诊** —— 在 GUI 仓，记的是「14 条」，但 P5/P4a 的数字都不准（见下），这个数同样待核

---

## 四、spec 门控（要先改 spec 才能做）

投影缓存、虚拟滚动、`assistant/chunk` 持久化、EventsTab 按类型收窄，以及「一个 assistant 的 `tool_calls` 内重复 `call_id`」是否要在 spec 层面收紧（代码侧已按现行 spec 补了 fail-loud，见 server#152）。

---

## 五、已分诊、判定为「不该修」的（16 条，不要重复捡起来）

P5/P4a 两份分诊里判为不该修的条目，理由多为「有意的设计」「spec 门控」「改判据会让路由 case 变复杂」「代价大于收益」。**下次看到它们时先查这一节，不要重新分析一遍。** 完整理由在两个 subagent 的分诊结论里，关键几条：

- `Blocks` 多一条 `{conversation 0}` —— 有意设计，`core.go` 注释明写 always listed，零 size 正是让人看出走了哪条路
- `[附图 N 张]` 标记位置 —— `eventlog.go:632` 有完整注释说明为何在此产生
- 深层路径 GET 回 400 / PATCH/DELETE 回 404 —— 改判据会让路由 case 变复杂
- 500 回显内部错误原文 —— `internal/server/http.go` 20+ 处同形状，单点修造成一文件两风格
- 失败结果全文带 `"failed: "` 而事件 preview 不带 —— 行为正确，且提醒目的已随 gui#46 过期

---

## 六、分诊纠正过的记录错误（写下来，因为这类偏差反复出现）

这一轮**每一次核实都推翻了至少一条既有记录**：

| 记录 | 实际 |
|---|---|
| 「老库里两张空表」 | **7 张表、31 个会话 314 行**，且不是空的 |
| D-2「漏检，偏差方向安全」 | **是误报**——正常会话就能触发熔断警告甚至中止 |
| T-1「判定是首帧更慢还是窗口太紧」 | **两条都不是**——测试丢弃了 `Read`/`Click` 的错误，红了三次查不下去正因为此 |
| P5 Minor「5+ 条」 | **18 条** |
| P4a Minor「16 条」 | **21 条**——只抄了清单 A，漏掉最终复审自己新提的 N1–N5 |
| 「原始 review 在 `.superpowers/sdd/`」 | 对 P4a **不成立**：`task-1/2/3-review.md` 已被 P5 同名文件覆盖。**归档要带阶段前缀** |
| P4a T2-M5「帧顺序没有常驻测试」 | 不成立，`eventlog_session_frame_test.go:89-95` 就是按序逐条比对、乱序即红 |
| 我自己写的「迁移会造出假日志」 | 不成立——老表字段完整，`task_id`/role/content 异常各 0 行 |
| A「`turn.TaskID == task.ID` 今天是空转」 | **不成立**——`runResume` 会为同一个 task 重读历史，filter 在那条路上会命中 |
| A「`Seq` 的 `omitempty` 是 1 行修」 | 改了会引入**相反**的缺陷（每条事件长出 `"seq":0`），已改为写清代价 |
| A「示例描述了已删的 `conversation_turns` 表」 | 示例里根本没出现过表名 |
| D「全量并发才复现，单独重跑 3 次没复现」 | **单包 `-count=20` 必现**；「重跑 3 次」是采样不足，不是证据 |
| D「与 T-1 同类但根因不同，还没查」 | 根因是生产代码的平台语义漏判（Windows delete-pending → errno 5），与测试无关 |

**教训**：接手时不要把 handoff 里的数字和判断当事实用，先核到代码。这份文档里的每一条也一样。

---

## 七、环境与工具链

- **`go test ./...` 默认并发会触发 Go 工具链自身崩溃**（这台机器上每次崩在不同包：`internal/task` → 标准库 `net`/`go/ast` → `internal/browser`），逐包单跑与 `-p 2` 全绿。**全量一律用 `go test -p 2 ./...`**
- 浏览器 e2e 带 `chromium` 构建标签隔离，跑它要 `go test -tags chromium ./internal/server/`
- GUI 的 vitest **必须在 `frontend/` 目录跑**，仓根有另一个没配 jsdom 的 vitest
- 改配置文件做实验时：`agent.json` 的 `agents` 段是相对仓库根的路径，挪到别处要改绝对路径；`storage` 段是 `{"driver":"sqlite","path":...}`；工具读写根来自**建会话时**的 `working_dir`；任务完成状态是 **`done`** 不是 `completed`

---

## 八、这一轮反复出现的错（比上一期新增两类）

上一期 §七 记的三类（「绿得不是地方」「接缝在但没人调用它」「注释里的事实陈述不实」）全部**再次出现**，另加两类：

### 「声称修了但没落盘」

改动脚本把多处修改放在同一次 `write` 里，中间的 `assert` 失败让脚本退出，`write` 从未执行；我只补了其中一处却断定另一处「已写入」，而另一个文件的注释已按修完的样子描述了那个不存在的修复。复审那句话是要害：**`go test` 全绿不构成证据**——当时没有任何测试断言过那个校验是否存在。

两条规程：
1. 每次提交前**逐项 grep 确认落盘**（函数存在、调用点计数、关键字符串在位），不只看 build/test 绿；
2. 变异清单要覆盖**「注释声称的事实」**。

改动脚本若要改多处，**要么每处单独 write，要么先把全部锚点 assert 完再动手**。按大括号配对找结构体收尾行来插入是脆的——同一期它把四个测试文件一次写坏。

另：验证命令里**绝不能有改变工作区状态的操作**。本轮一条「验证没破坏其他内容」的命令里放了 `git stash --keep-index`，把三个文件的改动全 stash 掉了，随后测试报「环境变量没生效」才发现。

### 「夹具比真 store 宽松，缺陷藏在那道缝里」

`captureEventStore.Append` 的签名写 `_ context.Context` 把 ctx 丢了，而真 store 走 `db.ExecContext(ctx, ...)` 会因取消失败——于是「运行被取消时收尾还能不能落盘」这个缺陷**在测试里根本不成立**。先把夹具对齐真 store，测试才变红。

同一个夹具此前已因 seq 校验栽过一次（那段注释写着「与真 store**同款**，不是可选的严格模式」）。**fake 的严格程度必须对齐真 store**，这是同一条规矩的第二次。

### 「重跑几次没复现」不是证据（二次更新新增）

D 原本记着「单独重跑 3 次、两次后续全量都没复现」，据此判成环境抖动挂了起来。真实复现率
约 2%：3 次不复现的概率是 94%，这个采样量对 2% 的事件**什么都证明不了**。

正确的做法是按缺陷的**形状**写探针而不是重跑被测程序：这次是 12 goroutine 争用同一路径、
只统计 create 的原始 errno，几秒钟就把 `ErrExist 28160 / errno=5 611` 摆出来，根因当场落定。
探针不碰被测代码，所以它的结论不依赖对被测代码的任何假设。

推论：判「疑似抖动」之前先算一下——如果真实复现率是 p，我这几次不复现的概率是多少。

### 「加了守卫却不写测试」

`appendHistory` 的 fail-loud 守卫、`Load` 的 `task_start` 校验，都是加完之后变异不红才发现没有断言盯着。守卫本身就是一条不变量，按本仓规矩必须有断言。
