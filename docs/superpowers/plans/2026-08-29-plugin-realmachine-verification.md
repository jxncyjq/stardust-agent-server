# 真机验证记录：插件四个扩展点 + 工具可达性

**日期**：2026-08-29
**为什么记**：G4a–G4d 与「插件工具可达性」此前**全部只有测试证据**。这次是第一次有真插件在真 `agent serve` 里被真派发路径调用。

## 一、怎么搭的（可复现）

- **真的**：`agent serve` 进程、真 wasm 插件（`plugin_example` 的 guest，只改了 decider 的判据）、真 HTTP API、真审批落盘。
- **假的**：模型。一个本地 OpenAI 兼容端点（127.0.0.1:18099），按**请求内容**决定回什么（不是按调用次数——运行时还会为情景记忆蒸馏调同一个端点，按次数排的脚本会整体错位一位）。
- 部署：`plugins.json` 直接授权（`capabilities:[log]`、`extensions:[observe,decide,prompt]`），`require_signature:false`，端口 18098 与用户自己的 serve 隔离。
- 假模型把每次请求体落盘——**系统提示词与工具结果都在里面**，这是最省事的取证面。

## 二、五个检查点，全过

| # | 检查点 | 证据 |
|---|---|---|
| 1 | 插件在真机上挂载 | `GET /v1/plugins` → `state:"loaded"`，`granted_extensions:["observe","decide","prompt"]` |
| 2 | **模型看得见也调得动**插件的工具 | 请求体的 `tools` 里有 `hello_echo`；工具结果 `hello, legion!` 回到模型 |
| 3 | `observe` 生效 | 日志 `observed tool=hello_echo success=true`（插件自己经 log 能力写的） |
| 4 | `decide` 能拒**agent 自己的内置调用** | 工具结果：`permission denied: plugin:legion-hello refused this call: reads are frozen during the incident`（被拒的是 `read_file`） |
| 5 | `prompt` 段进系统提示词 | 请求体里有 `--- plugin "legion-hello" (untrusted, …) ---` 围栏与段落原文 |
| 6 | `ask` 全闭环 | 任务 `suspended` → `GET /v1/approvals` 有票（`requested_by: plugin:legion-hello`、`reason` 是插件的话）→ 批准 → 任务 `done`，`read_file` 真跑，结果 `note file` |

## 三、抓到两个真 bug（这才是真机验证的价值）

两次都是同一形状：**per-agent resolver 接好了，默认任务路径没接**。默认路径服务的是「agent_id 不在 agent 注册表里的每个任务」——GUI 自己的路径、每个 default-agent 任务，也就是大多数任务。

1. **漏 `pluginTools`**：模型的 tools 列表里没有插件工具，调用返回 `tool not found`。单元测试全绿，因为它们测的是 resolver 那条路径。
2. **漏 `askArbiter`**：更难查——插件的 ask 能挂起任务、能开票、能批准，**恢复之后照样被拒**（「这个部署没有审批通道」）。前半段全对，只有最后一步错。

修法与防复发：把注册表构建抽成 `defaultTaskRunner.buildTaskTools`，于是「一个任务的注册表够得到什么」可以被直接测试，不必立起整个 runtime。两个测试各钉一半，变异验证各自复现了真机症状。

## 四、教训（下次接线时先问这句）

**「这条接线，有几条任务路径？」** 这个仓有两条（per-agent resolver 与默认 runner），此前的 browser、prompt 段、ask 仲裁者都只在其中一条上被验证过。只测一条 = 对大多数任务不生效，而症状是「插件根本不工作」，不是一个显眼的报错。

## 五、仍未验的

- GUI 界面上的审批卡片（本次全程走 HTTP API）。
- 签名/远程源的真机路径（本次用本地目录源 + `require_signature:false`）。
- 多插件并存时的顺序与总量上限（提示词段的 8192 rune 合计上限没有在真机上被撞到）。

---

## 六、GUI 界面走查（2026-08-30）

第一次在 `wails dev` 里用真插件走完审批闭环。此前的真机验证全程走 HTTP API，界面这一段是空白。

### 走通的

发消息 → 插件的 `ask` 让任务挂起 → 审批卡片出现 → 点批准 → 任务恢复、工具执行、气泡显示结果。

### 抓到三个缺陷（都已修）

1. **GUI 的每个写操作都不带 loopback token**（server 在 hardening 模式下会铸一个）。读路径带了；六个写路径各自用裸 `client.Post/Do`，一个都没带。症状是「界面能看、什么都做不了」——第一次发消息就 401。**修在传输层**，不再让第七个调用点去记。
2. **审批卡片把插件的票显示成宿主的**：卡片是从 SSE `approval_pending` 事件渲染的，而那个事件不带 `requested_by`/`reason`；前端按契约把缺失的来源读作宿主。`GET /v1/approvals` 早就带这两个字段（G4c 加的）——**同一份数据两条通路，只补了一条**，单测与接口全绿，错的只有屏幕上那张卡片。
3. **挂起等待审批被当成任务结束**：`TERMINAL_STATUSES` 含 `suspended`，气泡冻结在「任务状态: suspended，暂无结果」；人批准、工具执行、任务 done，屏幕再也不动。对用户就是「批准之后什么也没发生」。

### 第四个缺陷：加固在它唯一的目标场景里从未生效（已修）

**自动 loopback hardening 在 GUI 场景永远不触发**：`serve` 只在 `addr == ""` 时判定为「这是嵌入方」并铸 token，而 GUI 显式传 `127.0.0.1:0`——它要一个随机 loopback 端口，并且说了出来。于是这条加固在它唯一的目标场景里没生效：**每一个出厂的 GUI 都在跑一个谁都不问的 agent**，机器上任何进程都能列会话、经 `/v1/files` 读工作区、提交任务。缺陷 1（写路径不带 token）也因此是休眠的——它只在别人手动打开 `loopback_hardening` 时才炸。

修法是**让嵌入方明说，而不是从一个无关字段的缺席里去猜**：

- `ServeOptions.LoopbackHardening`（`serve.Options` 是它的别名，GUI 拿得到），与 `cfg.Server.LoopbackHardening` 并列；`addr == ""` 这条自动路径保留，但只服务真的什么都没传的调用方。
- `ServeManager.Start` 传 `LoopbackHardening: true`。
- 仍然**不**对任意 loopback `--addr` 自动加固：`serve --addr 127.0.0.1:8080` 的既有客户端手里没有 token，替他们决定就是把他们踢下线。

证据是行为而不是字段：铸没铸 token 说明不了什么，一个中间件没装好的构建照样能让「`Token != ""`」通过。两侧的测试都打真的 HTTP——server 侧「无 token → 401、带 token → 不是 401」，GUI 侧走 `postJSON`（**写**路径，不带自己的 header，完全依赖传输层；`apiGet` 自己设 header，用读路径测等于什么都没测）。变异验证：去掉 `opts.LoopbackHardening` 那一项，未加固的 serve 对无 token 的 `GET /v1/sessions` 回 **200**——这就是缺陷本身的取证。

**真机复核（2026-08-30，同一套 mock 部署，配置里没有 `loopback_hardening` 键）**：GUI 起的 serve 端口 1819，无 token 的 `GET /v1/sessions` 回 **401**，带 handshake.json 里的 token 回 200；GUI 窗口本身照常——会话列表、`plugin/loaded` 事件流（SSE 带 token）、发消息新建会话（写路径）、审批卡片、点批准后 `tool_executed read_file` → `tool_result` → `task_completed`、气泡显示 done。加固打开而 GUI 没有任何一处被锁在外面。

**一个已知后果，已决定并落地**：文件卡片的「复制链接」复制出的 `http://127.0.0.1:<port>/v1/files?...` 在外部浏览器里现在是 401。这正是加固要堵的洞（一个无鉴权的本地文件服务在对整台机器发工作区文件），但对用户是个功能回退——一个只在应用内有效、却长得像可分享链接的东西。把 token 一起放进剪贴板不是解法：那等于把整个 agent 的钥匙交给下一个读剪贴板的程序。

**决定：改导出**（GUI PR `jxncyjq/stardust-agent-gui#33`）。相对 URL（loopback 默认，就是 401 那种）不再提供「复制链接」，原「下载」改名「导出」；绝对 URL（部署配了 `server.file_base_url`）保留「复制链接」——那是部署方明确对外发布、自带鉴权的地址，且复制原样，不再拿 loopback base 去拼。

### 一次自我纠正（值得记）

走查中途看到「票在 4-5 秒内自动变成 approved」，一度当成后端自动批准的重大缺陷。用**对照实验**（GUI 窗口开着但不做任何交互 + curl 发任务）确认：票稳定保持 `pending`、任务保持 `suspended`。真相是**我自己用固定坐标点击发送按钮，而审批卡片出现后布局上移，那个坐标落在了「批准」上**。教训有两条：界面走查里点击一律用元素引用而不是坐标；把「疑似产品缺陷」写进报告之前，先做一次能把自己排除掉的对照实验。
