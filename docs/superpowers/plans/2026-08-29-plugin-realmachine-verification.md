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
