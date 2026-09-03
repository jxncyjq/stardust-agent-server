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

## 三、未完成：需要拍板的

### D-1 老会话的历史读不出来了（P3 没做数据迁移）

**这是本次整理新查到的，优先级最高。**

本机 `agent.db` 实测：

| 表 | 会话数 | 行数 |
|---|---|---|
| `conversation_turns`（P3 已退休） | 31 | 314 |
| `session_events`（新的唯一真相源） | 1 | 53 |

**31 个老会话在事件日志里一条记录都没有**，最近的一个停在 `2026-08-11T12:49`。P3 把 `ListConversationTurns` 改成 `ReadFrom` + 投影之后，这些会话的历史**再也读不出来**——GUI 打开老会话会是空的。数据没丢（老表还在），但没有任何读路径能到达它。

老表连 FTS5 影子表一共 **7 张**（`conversation_turns` / `_fts` / `_fts_data` / `_fts_idx` / `_fts_content` / `_fts_docsize` / `_fts_config`），不是先前记的「两张空表」。

**要拍的板**：

1. **写一次性迁移**：把 `conversation_turns` 投影回 `session_events`。难点是老表只有 (task_id, role) 折叠后的结果，没有 step/tool 事件——补出来的事件日志会缺工具往返，且 `seq` 要凭空构造。
2. **明确接受**：宣布「P3 之前的会话历史不再可读」，把 7 张表 drop 掉，在 CHANGELOG 里写明。
3. **保留只读旁路**：让 `ListConversationTurns` 在事件日志为空时回落到老表——**但这违反 fail-loud 铁律**（CLAUDE.md §0：出错就响亮地错），且会让「事件日志是唯一真相源」这条不变量失效。不推荐，列在这里是因为它一定会被想到。

我的建议是 2：老会话已经三周没动，而 1 造出来的是一份**假的**事件日志（缺工具往返、seq 是编的），比没有更糟——将来没人分得清哪些 seq 是真的。

### D-2 I-3：被裁的历史参数让循环检测漏检

`appendHistory` 把历史塞进 `convo.messages` → `repeatedCallStreak` → `callsKey` → `dedupKey` 按 `name|k=v` **逐字**比对。历史里被裁过的参数与模型这一轮真实发出的参数不等，streak 断在那里，跨会话的重复调用漏检。

**不是 MIN-4 引入的**：写入侧本就把 arguments 截到 `maxEventPreviewRunes`(2000)，超过的改动前同样匹配不上。MIN-4 把不匹配的边界从「超过 2000」拉到「超过剩余额度」，范围扩大了。

**偏差方向安全**：只会漏检（熔断少触发），不会误报中断任务。已用 `TestTrimmedHistoryArgumentsNoLongerMatchTheLiveCallVerbatim` + 两处注释钉住——注意那是 **notice 不是 guard**：两 key 相等时它只 `t.Log` 然后 return，谁把 `dedupKey` 改成对裁剪不敏感，它不会红。

**要拍的板**：跨会话的同一组调用算不算连续重复？两条便宜的旁路（给 `repeatedCallStreak` 传 history 偏移量跳过历史消息、或让 `dedupKey` 对记号不敏感）都是在**替这个问题作答**，不是绕开它。

## 四、未完成：不需要拍板，做就是了

### T-1 G3 端到端真机验证（本期唯一有实质风险的一条）

这次真机验证是**探针直连 provider**：从 `agent.db` 投影出 transcript → `port.InferenceRequest.Validate()` → 用项目自己的 adapter 发给 deepseek。三个验证点都过了（形状被接受、模型确实看得见工具往返、`_truncated_arguments` 读得懂），400 那个 bug 也是这么抓到的。

**但没有起过真实 `agent serve` 跑完整链路**。差的是：`session.tool_transcript_enabled` 从配置读进来 → `SessionHistoryForTask` 选路 → runtime 装配 → 真实工具循环 → 落盘 → 下一轮恢复。

启动坑（P3 走查时踩过）：`agent.json` 的 `agents` 段用的是相对仓库根的路径，配置文件复制到别处要改成绝对路径，否则 `--config` 看起来像被忽略了。

### T-2 `configs/agent.complete.example.json` 缺 `tool_transcript_enabled`

已核实：全仓 `configs/` 与 `agent.json` 都搜不到这个 key。不看代码的人不知道有这个开关。加一行，附一句「打开后请求体积可能涨数倍」。

### T-3 没有环境变量覆盖

`config.go:479` 只有 json tag。容器部署里改一个开关要重新打配置文件。

### T-4 per-task review 的 Minor 项

P4a 16 条 / P4b 14 条 / P5 5+ 条。**数字来自当时各轮 review 的记录，本次没有逐条复核**；原始 review 文件在 `.superpowers/sdd/`（git-ignored，`git clean -fdx` 会清掉）。

### T-5 零散的已知项

| 事项 | 说明 |
|---|---|
| 一个 assistant 的 `tool_calls` 内重复 `call_id` 静默允许 | spec §4.3.1 只禁「同 step 跨消息复用」 |
| `Blocks` 多一条 `{conversation 0}` | G3 打开时历史离开 prompt，该条 Chars=0 |
| `CurrentSchemaVersion` 无守卫 | |
| `restore_latest` / `Append` 的触碰面 | 设计待议 |
| `[附图 N 张]` 标记位置 | |
| GUI 注释引用了另一个仓的 spec 路径 | 跨仓引用，那边一改就悬空 |
| `internal/plugin/fetch` 的 Windows 文件锁抖动 | 全量并发跑时 `Access is denied`；单独重跑 3 次、两次后续全量都没复现。与上一期 T-1 的 `TestBrowserStreamE2EObservationProgressFrame` 抖动同类，可一起处理 |

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

**「接缝在但没人调用它」——本期栽了 3 次。**

P5 最严重：删掉任一处 `HistoryTranscript` 赋值，`go test ./...` 全绿；重新枚举发现有 **7 处**调用点而不是以为的 2 处。

MIN-4 这次**没**栽，因为落点选在 `SessionTranscript` 这个唯一收口处。判据是：请求装配路径只有一处 `newConversation`、一处 `appendHistory`——先数清楚「有几条路径」，再决定改哪里。

**注释里的事实陈述会过时，而本仓把注释当契约用。**

MIN-4 两轮复审各抓到一次：第一轮「总量仍然受控」不实（实际是 `maxTurnChars + footer + K×记号`）；第二轮「保底前缀是 8」不实——我为修 I-1 加的守卫把有效地板抬到了 `peek + len(记号)` = 294，而常量 doc、上限表述、测试上界三处都还按 8 写。**复审是用我自己的测试当证据的**：`TestAMediumArgumentIsNotReplacedByALongerNote` 里 budget=10 而一个 50 字符参数整条通过——单个参数就是配置上限的 5 倍，那条测试却是绿的。
