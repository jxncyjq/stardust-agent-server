# 会话事件日志 P1–P5 收尾与未决事项（2026-09-03）

**这份文档的用途**：会话事件日志系列（P1 存储层 → P5 的 G3 开关）到此为止，三仓 master 干净、零开放 PR。下面是**没做的事**，每一条都写清「为什么它还在这儿、下一步具体做什么」。给下一个接手的人看。

**三仓 tip**：`stardust-agent-server` `8991eee` / `stardust-agent-gui` `b47cda0` / `docs` `8c9202d`（均 master/main，工作树干净）。

**上一份积压**：[2026-08-31-open-items-handoff.md](2026-08-31-open-items-handoff.md)（浏览器/插件那一期，未清项仍然有效，见本文§五）。

---

## 一、本期完成（已合 master）

| 事项 | PR | 这条真正抓到的东西 |
|---|---|---|
| P1 存储层（`session_events` + 六条不变量） | server#137 | 重复 `call_id` 检查范围过宽，会让健康日志打不开 |
| P2 发射点 + 三道 fail-closed 闸门 | server#138 | 删掉成功分支的 `recordToolResult`，包内**照样绿**（测试跑的是 tool-not-found） |
| P3 投影 + `conversation_turns` 退休 | server#140 | 计划漏了 `columnMigrations` 里 5 条 `ALTER`——会让**每次启动都失败** |
| P4a 事件读取端点 + SSE 帧 | server#142 | `eventbridge` 谓词写错类型；`next_seq` 语义（截断时指向第一条被丢弃的事件） |
| P4b GUI 轨迹视图 | gui#46 | `events not array` 守卫删掉照样绿（坏值不可迭代，抛错路径满足了断言） |
| P5 G3 开关（默认关） | server#145 | 计划用了 session 级 `call_id` map——跨轮复用合法，结果**静默错配**且 `Validate()` 通过 |
| spec §7 订正 | docs#23 | 「EventsTab 退休」被证伪：`ListRuntimeEvents` 读的是全局工作流总线 |
| SSE 吊销时序 flake | server#146 | 不是 flake，是生产缺陷：失效信号在写响应头**之后**取，那一刻的轮换会被整个错过 |
| G3 真机 400 修复 | server#147 | 见下 §二 |
| MIN-4 参数预算 | server#148 | 见下 §二 |
| D-1 老会话历史（拍板删表） | — | 「两张空表」实为 31 个会话 314 行；见 §三 D-1 |
| D-2 重复熔断只数本任务 | server#149 | 「漏检」实为**误报**：正常会话就能触发警告甚至中止；见 §三 D-2 |

## 二、本期两个真机/复审抓出的生产缺陷（已修，记在这里因为它们的形状会再来）

**G3 打开后会话恢复必然 400（server#147）**
历史以 transcript 排在 `message[0]` 之后，而历史最后一条几乎总是上一轮的收尾回答——一条没有 `tool_calls` 的 assistant。请求就此以 assistant 结尾，provider 当成 prefill 续写，thinking 模型要求它带回 `reasoning_content`：

```
The `reasoning_content` in the thinking mode must be passed back to the API.
```

`agent.json` 四个 profile 全指向 deepseek，所以这在**默认部署上是 100% 失败，不是偶发**。代码注释预见了这个风险，但判断只是「模型可能顺着旧话题往下说」。修法是 `appendCurrentInput`：把当前输入复述成末尾一条 user，历史位置与缓存断点都不动。

取证方式值得复用：**受控对照**——同一段 head + 同一批历史，唯一差别是末尾多一条 user。补上就通过并正确答出一个只存在于工具往返里的事实（`metrics: 6060`），不补就 400。

**短参数被换成更长的省略记号（server#148，第一轮复审 I-1）**
额度耗尽时无条件返回记号、不看原文长度：`{"path":"a.md"}` 5 个字符被换成 56 个字符的说明——既没省字节（多 11 倍），又把模型唯一能用的信息丢掉了。

## 三、需要拍板的（两条均已拍板并处置）

### ~~D-1 老会话的历史读不出来了~~ ✅ 已处置（2026-09-03，拍板：删表）

**实测确认了问题**：会话列表返回 32 个（GUI 会全列出来），点开老会话 **0 条 turn**——用户看到的是 31 个空白会话。

**先前写在这里的推荐（方案 2 删表）当时的理由是错的**，记下来因为这类误判会再来：我说「迁移会造出假的事件日志（缺工具往返、seq 凭空构造）」。实测后三条都不成立——

- 老表字段是**完整的**：`task_id / agent_id / model_profile / role / content / created_at / 四个 token 列 / generated_files`，正好是 `projectTurns` 产出的 `ConversationTurn` 全部字段；
- 数据质量干净：`task_id` 为空 0 行、role 非 user/assistant 0 行、content 为空 0 行；
- 「缺工具往返」是老表**本来就没存**，如实反映而非伪造；`seq` 按 `created_at` 排序是确定的，不是编的。

我据此在真实库的副本上把迁移写出来并跑通了：31 个会话 314 行全部转换成功，读出内容正确，逐会话核对 turn 数与老表按 `(task_id, role)` 折叠后完全一致，且**幂等**（第二次跑 0 个待迁移）、**不碰**已有事件日志的会话（那个新会话 53 条一条未动）。

**用户拍板：不迁移，删表。** 我提出实测表明这会删掉真实有价值的对话（浏览器任务等），用户重申了这个决定——按其决定执行。

**已执行**：

1. 备份 `agent-before-d1-20260903.db.bak`（2088960 bytes，`.gitignore:24` 的 `*.bak` 覆盖）
2. `drop table conversation_turns_fts` + `drop table conversation_turns`——FTS 主表带走 5 张影子表，7 张全清
3. `vacuum`：2.09MB → 1.11MB
4. 起 serve 验证：正常启动（建表/迁移逻辑不因缺表而失败）、0 error/panic、新会话读写不受影响
5. 迁移工具 `cmd/zzmigrate` 已按拍板删除，仓库无痕

**遗留后果（未处理，需要时另行决定）**：那 31 个会话在 `agent_sessions` 里仍在，GUI 会列出它们、点开永远空白。要消掉这个体验，得另外清 `agent_sessions` 里对应的 31 行——那是第二次删数据，没有一并做。

**备份里还留着那 314 行**，改主意还来得及；备份一旦删掉就真没了。

### ~~D-2 I-3：被裁的历史参数让循环检测漏检~~ ✅ 已处置（2026-09-03，server#149）

**原先写在这里的表述是错的**：我把它记成「漏检 + 偏差方向安全 + 只钉不修」。探针一测就推翻了——**不是漏检，是误报**。

`repeatedCallStreak` 数的是「连续多少**轮**请求了完全相同的工具调用」，「轮」是这一个任务内工具循环的轮次。G3 打开后历史排进同一个 `conversation`，从后往前的扫描一路扫进上一个会话：

| 场景 | 改动前 streak |
|---|---|
| 历史里一次相同调用（短参数，未被裁） | 2 |
| 历史里连续 3 轮相同调用 | **4**（≥ `repeatWarnStreak` 3） |
| 历史参数被 MIN-4 裁过 | 1 |

用户连问三次同样的问题（完全正常的用法）→ 下一个任务的**第一次**模型请求 streak 就是 4，模型平白收到重复调用警告，而它一次工具都还没调；历史里连续 7 轮 → streak=8 撞上 `repeatAbortStreak`，**任务直接被中止**。

熔断可以少管（漏检只是没救到），不能错杀。原先记的「偏差方向安全」只在「漏检」这个错误前提下成立。MIN-4 的参数裁剪意外缓解了它（第三行），但只对超过预算的长参数有效——路径、开关这类短参数照样原样进历史。

**修法**：`conversation` 记 `taskStart`，streak 只扫本任务的消息。`taskStart` 是个下标，所以三条会让它失效的路都堵了——压缩后重算、检查点存取（`Checkpoint.TaskStart`）、`appendHistory` 的注入时机加 fail-loud 守卫。检查点的内容校验抽成 `validateCheckpoint`，两条反序列化路径（`Load`/`ListSuspendedIn`）共用。

**四轮复审，每轮都抓到东西**：

| 轮次 | 抓到的 |
|---|---|
| 1 | **Critical** 压缩后边界越界 panic；`restoreConversation` 用字面量构造、`taskStart` 拿零值——「接缝在但没人调用它」第 4 次 |
| 2 | 损坏检查点不该 panic（与 `sessionstate` 包自己的契约冲突）；**注释事实错误**；压缩测试证不出平移公式 |
| 3 | **Critical：上一轮声称修好的 `Load` 校验根本没落盘** |
| 4 | 「空 `Messages` 算损坏」写在注释里但代码实际放行 |

十四个变异全部被抓住。加空 `Messages` 校验时 13 处既有夹具变红（都图省事没给 Messages），补完后重跑变异确认不是「把测试改到通过」。

## 四、未完成：不需要拍板，做就是了

### ~~T-1 G3 端到端真机验证~~ ✅ 已完成（2026-09-03）

起了两个真实 `agent serve`（同一份配置，只拨 `session.tool_transcript_enabled`，各自独立端口与库），跑同一组两轮对话。

**实验设计**：第一轮让 agent 用 `read_file` 读一份端口表，并要求它「不要复述任何具体端口号」——模型照做了，所以 `shadow-indexer=52984` 这个事实**只存在于 `tool/result` 事件里**。第二轮追问这个数字，并明令「不要再读文件、不要调用任何工具」。

| | G3 开 | G3 关（对照） |
|---|---|---|
| 第二轮回答 | **52984** ✅ | 「我不知道」 |
| 第二轮工具调用 | **0 次**（查事件日志确认） | — |
| 第二轮 prompt tokens | 1752 | 1470 |

对照组自己解释了原因：「之前的对话记录只说明了 ports.md 的内容性质，并未透露具体端口值」——正是 G3 要治的失忆。

**验到的东西**：

1. 完整链路通了：配置读入 → `SessionHistoryForTask` 选路 → runtime 装配 → 真实工具循环 → 事件落盘（第一轮 16 条，含两次 `tool/call`+`tool/result`）→ 下一轮恢复时模型真能看见。
2. **server#147 的修复在真机上生效**。第二轮正是「带历史 transcript 恢复会话」的场景——若那个修复不在，它必然 400（`reasoning_content` must be passed back）。它成功了。
3. **G3 的真实代价：+282 tokens（+19%）**，远小于 P5 单测合成夹具的 3.00x——因为这个会话的工具输出不大。真实部署的倍数取决于工具输出体积，单测那个数字不能当预期值用。
4. 两个 serve 的日志里 0 处 error/panic/400。

**踩到的坑（下次照做/避开）**：

- `agent.json` 的 `storage` 段是 `{"driver":"sqlite","path":"..."}`。我改配置时用 `list(keys())[0]` 猜 key，把 `driver` 覆盖成了路径，启动直接被 fail-loud 拦下：`storage driver "<路径>" provides no task sink`。**报错信息把错误指得很准**——这是 fail-loud 铁律在真机上兑现的一次。
- `agents` 段是相对仓库根的路径，配置挪到别处必须改绝对路径（P3 走查时踩过，这次照做了，没再踩）。
- 工具的读写根来自**建会话时**的 `working_dir`（`POST /v1/sessions`），不是配置里的 `workspace`。
- 任务完成状态是 **`done`**，不是 `completed`。我最初的轮询判据写的是 `completed`，前几轮是靠循环跑满后恰好已落盘才拿到结果——判据错了却"看起来能用"。

### ~~T-2 示例配置缺字段~~ / ~~T-3 没有环境变量覆盖~~ ✅ 已完成（2026-09-03，server#150）

两个示例的 `session` 段都只列了 4 个字段，而 `SessionConfig` 有 6 个——`cache_enabled`、`cache_max_entries`、`tool_transcript_enabled` 一直缺着。三个都补上了。

顺手加了一条**结构性**断言 `TestTheExampleConfigsListEverySessionField`：用反射取 `SessionConfig` 的每个 json tag，逐个检查两个示例里都有。示例与结构体的同步靠人记是记不住的（这次就漏了三个），让它变成会红的测试。

环境变量 `LEGION_AGENT_TOOL_TRANSCRIPT` 走 `REQUIRE_IDENTITY` 那一档而非便利开关的 `== "true" || == "1"`：这个开关改的是每次请求的体积，写 `=yes` 的人是想打开它，静默落回 false 会让他拿到零效果加零警告，而「体积没变」最容易被读成「这开关没用」。不可解析的值 fail-loud。

### T-4 per-task review 的 Minor 项 —— 已分诊（2026-09-03），修掉 3 条，其余分类待办

原先记的「P4a 16 条 / P4b 14 条 / P5 5+ 条」**三个数都不准**，两个 subagent 逐条核实到代码后：

| 原记 | 实际 |
|---|---|
| P5 「5+ 条」 | **18 条**（去重后） |
| P4a「16 条」 | **21 条** —— 只抄了 `archive-p4a-final-review.md` §4 清单 A 的 16 条，漏掉 §6 最终复审自己新提的 N1–N5 |
| P4b「14 条」 | 未分诊（在 GUI 仓） |

**「原始 review 文件在 `.superpowers/sdd/`」这句对 P4a 不成立**：`task-1/2/3-review.md` 已被 **P5** 的同名文件覆盖，P4a 那批的唯一幸存记录是 `archive-p4a-final-review.md` 的 roll-up 表。以后归档要带阶段前缀。

分诊还核出一条**记错的**：P4a 的 T2-M5「帧顺序=seq 顺序没有常驻测试」不成立——`internal/runtime/eventlog_session_frame_test.go:89-95` 就是按序逐条比对、乱序即红，辅助函数不排序、还显式钉了绝对值 `0,1,2`。

#### 已修（server#152）

- **P5 T3-1-M4 检查点丢 `StablePrefixLen`** —— 唯一有真实运行时影响的一条：恢复路径不经 `pinCachePrefix`，续跑的每一次请求都失去 prompt cache，而 G3 把历史排在 `messages[0]` 之后的整个取舍就是为了保住这个断点。修复前测试直接红（0 != 34）。
- **P5 MIN-5 宣告侧缺撞键 fail-loud** —— 三处同性质只有它放行。
- **P4a T3-M1/N4 `appendToolResults` 的 panic 从未被执行过** —— 16 处调用全传对得上的 map，改成 `continue` 也不会有任何测试变红。

#### 值得修但需拍板（3 条）

| 项 | 要拍的板 |
|---|---|
| **P5 T4-顾虑2** `totalChars` 不计 `ToolCalls` | #148 之后「度量口径与预算口径都只看 Content 所以一致」这条立论**已破**：arguments 被预算裁了却量不到，`runtime.debug` 的数会系统性低估 G3 代价（T-1 记的 +19% 正是这个口径）。**并进 `total_content_chars`（破坏与历史日志可比性）还是新开 `total_tool_call_chars`？** |
| **P4a T1-M4** `limit` 无上界 | `session_events.go:76` 只有下界 1，SQL 无 LIMIT 下推。修正一处描述：全量读**不是端点特有的**，`ReadFrom(ctx,id,0)` 已在五处热路径（`runtime.go:553` 每次 `newTaskRecorder` 为算 turn 号把整条日志 JSON 解一遍等），端点独有的只是响应体不设限。**加上界（6 行）还是连 SQL 下推一起做？** |
| **P4a T2-M1** 取消时 `turn/end` 不落盘 | `runtime.go:602-608` 的 `closeTurnOnError` 直接把任务 ctx 交给 `rec.flush`；全仓 11 处 `WithoutCancel` 这条路径一处都没有。取消时 `Append` 先失败，日志留下永远开着的 turn。**需要新测试与取消路径推演，建议单独一次。** |

#### 注释订正一批（9 条，纯文字，可一次提交）

`session_turns.go:357` 描述了一个不存在的 HTTP 前置写入方（且与同文件 `:90-97` 已写对的那段**直接矛盾**）、`runtime.go` 取舍注释没记「连续两条 user」、`builtin.go:40/45` 把截断归给已搬走的函数、`eventbridge.go:110` 描述了不存在的按 subject 过滤能力、`eventlog.go:448` 的回退链对 server 顶层任务不准、`http.go:567` 路由判据的隐形前提没写、`openapi_coverage_test.go:158` 的 `negated` 成死代码且注释指着已换掉的代码、`RuntimeEvent.Seq` 的 `omitempty` 让 `/v1/runtime-events` 丢 `seq=0`、`agent.full.example.json:78` 两处不实。

#### 不该修（16 条）

多数是「有意的设计」「spec 门控」「改判据会让路由 case 变复杂」这类；P4a T3-M2 的提醒目的已随 gui#46 过期。详见两份分诊。

### T-5 零散的已知项

| 事项 | 状态 |
|---|---|
| ~~`CurrentSchemaVersion` 无守卫~~ | ✅ server#150。核实出的缺陷比记录具体：`migrate` 只 `INSERT OR IGNORE`、**从不读回**，于是旧二进制打开新库不被拒，会一路跑到某个查询撞上不存在的列才炸，期间的写入还可能污染库。现在建表前读回 `max(version)`，高于当前值就拒绝启动 |
| ~~GUI 注释引用另一个仓的 spec 路径~~ | ✅ gui#47。核实后比记录更糟：那份文档**从未提交**，两仓皆无、git 历史也没有。改成自包含注释 |
| ~~`Blocks` 多一条 `{conversation 0}`~~ | ✅ 核实为**有意设计**，不是缺陷——`core.go` 注释明写「always listed, named for the route it took」，零 size 正是让读者看出走了哪条路。**这条是我原先记错了** |
| ~~`[附图 N 张]` 标记位置~~ | ✅ 核实无缺陷。`eventlog.go:632` 的 `userMessageContent` 有完整注释说明为什么在这里产生（P3 退休 `conversation_turns` 后，注解必须与 user/message 事件同源，否则丢失） |
| 一个 assistant 的 `tool_calls` 内重复 `call_id` 静默允许 | **spec 门控**：spec §4.3.1 只禁「同 step 跨消息复用」，是否收紧要先改 spec |
| `restore_latest` / `Append` 的触碰面 | **需拍板**：设计待议 |
| `internal/plugin/fetch` 的 Windows 文件锁抖动 | 未处理。与上一期 T-1 同类但根因不同（T-1 是测试丢弃错误，见下）|
| **新增：`go test ./...` 默认并发会触发 Go 工具链自身崩溃** | 这台机器上每次崩在不同包（`internal/task` → 标准库 `net`/`go/ast` → `internal/browser`），逐包单跑与 `-p 2` 全绿。是环境问题，但会干扰全量验证——**以后全量一律用 `go test -p 2 ./...`** |

## 五、spec 门控（要先改 spec 才能做）

投影缓存、虚拟滚动、`assistant/chunk` 持久化、EventsTab 按类型收窄。

## 六、上一期的积压仍然有效

[2026-08-31-open-items-handoff.md](2026-08-31-open-items-handoff.md) §三 需要拍板（D-1 界面上那次询问 / D-2 签名清单 / D-3 代码签名与 macOS 公证 / D-4 工具结果图像通道 → set-of-marks）、§四 做就是了（T-1 Windows 抖动 / T-2 GUI 插件面板走查 / T-3 `wails dev` 手动验证），以及 §五维护约束、§六两类反复出现的错——**改动前先看那两节**。

## 七、本期反复出现的错（写下来，因为它们还会来）

**「绿得不是地方」——本期又栽了 10 次以上。**

七次在 P1–P5 各任务里（最典型：删掉成功分支的 `recordToolResult`，包内照样绿，因为测试跑的是 tool-not-found 那条路）。MIN-4 一个 PR 里就再栽三次，每次都是**变异验证**抓出来的、而不是读代码：

| 缺口 | 为什么假绿 |
|---|---|
| 短参数守卫 | 用 4 字符参数，它在更早的分支就返回了，走不到那个守卫 |
| 裁剪路径扣额度 | 用 500 字符参数，全落到保底前缀上，扣不扣结果相同 |
| 确定性 | **改了一个常量（peek 8→48）就把原本有效的断言变成了恒真** |

最后一条是本期最值得记的：**改常量会让既有断言失效**。所以每次改边界常量之后要重跑全部变异，不能只跑测试。

变异验证脚本本身也要设两道门，否则「脚本错」会被读成「变异没抓住」——本期真的发生过一次：
1. `grep` 确认变异**真的应用了**（sed 里 `...` 没转义、python 锚点没匹配，都会静默不改）
2. 确认它**不是只造成编译错误**（编译不过不算变异验证）
3. 判据用 `go test -v` 的 `^--- FAIL`——非 verbose 模式**不输出这一行**

**「声称修了但没落盘」——D-2 第三轮复审抓到的 Critical，本期最该记的一条。**

改动脚本把两处修改放在同一次 `write` 里，中间的 `assert` 失败让脚本退出，`write` 从未执行——两处一起丢了。我随后只补了其中一处，却断定另一处「已写入」，没有回头确认。更糟的是另一个文件的注释已经写着「校验归 `Store.Load`」，**文档按修完的样子描述了一个不存在的修复**，会主动劝退下一轮核实。

复审那句话是要害：**`go build` / `go test` 全绿、十个变异全被抓住，都不构成证据**——当时没有任何测试断言过 `Load` 会不会校验，而缺失的校验不会让任何既有断言变红。

两条规程因此定下来：

1. 每次提交前**逐项 grep 确认落盘**（函数存在、调用点计数、关键字符串在位），不只看 build/test 绿；
2. 变异清单要覆盖**「注释声称的事实」**——注释在本仓当契约用，一条不实的注释和一个缺失的校验一样危险。

另外：改动脚本若要改多处，要么每处单独 write，要么先把全部锚点 assert 完再动手。按大括号配对去找结构体收尾行来插入是脆的——同一期里它把四个测试文件一次写坏（编译不过，`git checkout` 回滚重来）。

**「注释里的事实陈述会过时/不实」——本期栽了 5 次。**

MIN-4 两轮各一次（「总量受控」、「保底前缀是 8」），D-2 两轮各一次（「老检查点不带历史段」被自己的 diff 证伪、「空 Messages 算损坏」而代码放行），加上 D-1 handoff 里那条「迁移会造出假日志」的错误推荐。共同点：**都是我在解释「为什么这样做」时写下的因果，而不是对代码行为的直接描述**。这类句子最容易在后续改动中失效，也最容易误导排查。

**「加了守卫却不写测试」——本期 2 次。**

`appendHistory` 的 fail-loud 守卫、`Load` 的 `task_start` 校验，都是加完之后变异不红才发现没有断言盯着。守卫本身就是一条不变量，按本仓规矩必须有断言。

**「接缝在但没人调用它」——本期栽了 4 次。**

第 4 次是 D-2 的 `restoreConversation`（用结构体字面量构造，新增字段拿零值）。P5 最严重：删掉任一处 `HistoryTranscript` 赋值，`go test ./...` 全绿；重新枚举发现有 **7 处**调用点而不是以为的 2 处。

MIN-4 这次**没**栽，因为落点选在 `SessionTranscript` 这个唯一收口处。判据是：请求装配路径只有一处 `newConversation`、一处 `appendHistory`——先数清楚「有几条路径」，再决定改哪里。

**注释里的事实陈述会过时，而本仓把注释当契约用。**

MIN-4 两轮复审各抓到一次：第一轮「总量仍然受控」不实（实际是 `maxTurnChars + footer + K×记号`）；第二轮「保底前缀是 8」不实——我为修 I-1 加的守卫把有效地板抬到了 `peek + len(记号)` = 294，而常量 doc、上限表述、测试上界三处都还按 8 写。**复审是用我自己的测试当证据的**：`TestAMediumArgumentIsNotReplacedByALongerNote` 里 budget=10 而一个 50 字符参数整条通过——单个参数就是配置上限的 5 倍，那条测试却是绿的。
