# GUI 插件授权同意流 设计

**日期**：2026-08-25
**前置**：A5c 安装与授权语义（server PR #86 / `f22c0be`，docs PR #7 / `812bbb7`）
**仓库**：`legionAgent`（server，`stardust-agent-server`）+ `legionAgentGUI`（GUI，`stardust-agent-gui`），两个独立 git 仓库

## 目标

把 A5c 交付的 `unauthorized` / `disabled` / `authorized` 三态搬到图形界面上，让插件的授权与撤销不必再走命令行。授权在 GUI 里点一下就**当场生效**，而不是改完还要人去敲 reload。

## 非目标（各自独立议题）

- **不做安装**：装插件仍走 `agent plugins install`。GUI 里做安装意味着处理下载进度、超时、验签失败一整套长耗时流程，是独立一期的量。
- 不做 `search` / 插件市场（设计文档 §10 明确排除，仓内无 registry 概念）。
- 不做插件状态的实时推送。面板打开时拉一次，操作后重拉。

## 一、约束：GUI 够不到 A5c 的任何代码

A5c 的实现全部在 `internal/plugin/manifest` 与 `internal/cli`。Go 的 internal 规则让 GUI 模块无法 import 它们——即便 GUI 通过 `replace github.com/stardust/legion-agent => ../legionAgent` 直接依赖该模块。

现有的 per-agent 工具授权 UI 是靠 server 仓专门开的一条窄 public seam 绕过的（`serve.GateableTools()`，`app_agents.go:25`）。所以本期必然要在 server 仓开一条通路。

另一个决定形态的事实：**GUI 进程就是 serve 进程**。`serve_manager.go:50` 调 `serve.BuildService()` 在自己进程内跑服务，并已经在用 `ServeResult.Token` + `ServeResult.BaseURL` 对这个 in-process serve 发 HTTP 与 SSE（`command.go:1917-1924` 的 doc 明写这个消费者就是 Wails GUI）。

因此通路选 **HTTP 端点**：逻辑留在 server，GUI 只做 UI，运行中的 loader 句柄不外泄给外部调用方，且与 CLI 共用同一套并发守卫。

## 二、三个端点

加进 `internal/server/http.go` 既有的路由 switch（约 :290-315），天然继承 loopback hardening 与 Bearer token 鉴权。

```
GET  /v1/plugins
POST /v1/plugins/{name}/grant   body: {capabilities[], allowed_hosts[], allowed_paths[]}
POST /v1/plugins/{name}/deny
```

`GET /v1/plugins` 每条目返回：名字、版本、当前状态、贡献的工具、**插件声明的**能力与 hosts/paths、**已授予的**能力与 hosts/paths。声明与已授必须分开返回——同意对话框要拿声明去渲染清单，拿已授去标记当前状态。

状态取值**完全沿用 `plugins status` 已有的一套**，不新增：`unauthorized` / `disabled` / `loaded` / `failed` / `suspended` / `pending`（`mergePluginStatus` 里「已在清单启用但尚未收敛」的那一格）。

**不要造第二套状态机**：第四节的 `pending_convergence` 只是 grant/deny 端点**这一次调用**的即时结果字段，说的是「本次收敛没在超时内完成」；它不是条目的持久状态。收敛超时后再 `GET /v1/plugins`，该条目落在既有的 `pending` 上——磁盘上已授权、运行时尚未挂载，正是 `pending` 本来的含义。

## 三、⚠️ 共享校验必须提取，不能重写

A5c 全期反复抓到同一类缺陷：**两条路径验证同一个概念，然后分道扬镳**。install 与 grant 在重复能力名、allowlist 规则、并发守卫上各错过一次，每一次都是「一条路补了、另一条没补」。

HTTP 端点会是验证这套规则的**第三条路径**，也是 `plugins.json` 的**第四个 writer**。若端点自己重写一遍校验，就是明知故犯地再造一次同样的分歧。

新建内部包（建议 `internal/plugin/consent`），把下列三样从 `internal/cli` 提取进去，CLI 改为调用它：

| 提取项 | 现位置 | 不共用会怎样 |
|---|---|---|
| 能力 set-equality 校验 | `resolveGrantCapabilities` | 端点只查单向就复现 F1：插件声明 `log,http` 而只授 `log`，装得干干净净、`serve` 正常启动、插件静静停在 `failed` |
| hosts/paths 成员校验 + 授 `http`/`fs` 必须给 allowlist | `resolveGrantAllowedHosts` / `resolveGrantAllowedPaths` | 端点漏了就是「看起来授权了、`status` 显示 `loaded`、每个出站调用被拒，且无一处说明原因」 |
| 并发编辑守卫 | `refusePluginDeploymentChanged` | 端点是第四个 writer；一个守卫只守一部分 writer 就不是守卫 |

**代价**：改动 A5c 刚合入的 `plugins_command.go`。这是有意接受的——CLI 的既有测试就是这次提取的回归护体，提取后它们必须全绿且不许改断言。

提取是**纯搬移**：不趁机改语义。验证方式与 A4b 把 `TaskGate` 下沉到 `internal/taskgate` 时相同——搬完 CLI 测试一条不改、全绿。

## 四、半成功必须是一等公民

契约 4 规定插件变更只在任务边界落地。GUI 点授权时若有任务在跑，收敛会等到 `apply_wait_ms`。于是有三种结果，端点必须分得清：

| 情况 | 端点返回 | UI 表现 |
|---|---|---|
| 写盘失败 | `4xx/5xx`，磁盘一字未变 | 报错，状态行不变 |
| 写盘成功 + 收敛成功 | `200`，`state: authorized`，插件已挂载 | 状态行变已授权 |
| 写盘成功 + **收敛没发生**（等待超时／被取消／另一个 apply 正在跑） | `200`，`state: pending_convergence` | 「已授权，待收敛」+ 一个「稍后重试生效」按钮 |
| 写盘成功 + 收敛发生了但**这个插件激活失败**（包损坏等） | `200`，`state: failed` + loader 的失败原因 | 状态行变失败并显示原因 |

**顺序决定了这四种的存在**：端点先写盘、再触发收敛，所以除第一种外**磁盘都已经改了**。`ApplyAtBoundary` 的超时路径（`taskgate.go:327`）wrap 的是 `waitCtx.Err()` 且消息明写 "nothing was applied"——据此区分第三种；能走到收敛却让这一条目失败的，是第四种。把第四种误报成 `pending` 会让人一直等一个永远不会来的收敛。

**第二种绝不能报成纯成功。** 授权确实写进磁盘了，但插件此刻还没在跑——把这个状态说成「已生效」，就是 A5c 全期最痛恨的那种「报告成功但其实没生效」，且这一次是在安全边界上。

UI 在等待期间显示**还在等几个任务**与**等待上限**（`apply_wait_ms`）——这两项都能从 `ApplyAtBoundary` 超时错误的消息里拿到（`taskgate.go:327` 带 `%d task(s) still running` 与已等时长）。

**不提供取消按钮。** 阻塞的是 server 端的 Apply，而 Wails 绑定没有 abort 语义：前端放弃等待也拦不住 server 把 `apply_wait_ms` 等完。放一个实际取消不了任何东西的按钮，正是本期在防的那类谎。等待上限是配置里的有限值，如实显示它就够。

## 五、同意对话框的形状

A5c 的规则造成一个不对称，UI 必须如实反映：

- **能力只能整体接受**：授权集必须**等于**插件声明集（`reconcileCapabilities` 拒绝严格子集，多余的它本来就忽略）。所以能力渲染成**只读清单 + 一个确认按钮**，不是禁用的勾选框——禁用的勾选框会让人误以为是自己权限不足，而实际原因是「插件的能力声明不是菜单」。对话框要用一句话说清这点。
- **hosts/paths 可以收窄**：交集语义，允许严格子集。渲染成勾选框，默认全选，可取消。
- **授 `http`/`fs` 却把 allowlist 取消光会被拒**（除非插件声明的 allowlist 本身为空）。UI 应在提交前就阻止这个组合并说明原因，而不是让人提交后吃一个端点错误。

## 六、GUI 侧

| 文件 | 内容 |
|---|---|
| `app_plugins.go`（新） | Wails binding：`ListPlugins` / `GrantPlugin` / `DenyPlugin`，用 `ServeResult` 的 Token + BaseURL 调端点，与 SSE bridge 同一套基础设施 |
| `frontend/src/components/settings/PluginsPage.tsx`（新） | 插件列表 + 状态徽章，放进 `SettingsModal`，与 `AgentConfigPage` 平级 |
| 同意对话框 | 复用既有 `ConfirmDialog` / `ApprovalPrompt` 的样式语言 |

前端不直接 fetch——走 Wails binding 到 Go，Go 再发 HTTP。这是仓内既定架构（避 CORS）。

## 七、测试

**server**：端点单测，至少覆盖——收敛超时返回 `pending_convergence`（而非成功）、并发编辑冲突被拒、能力严格子集被拒、授 `http` 但 allowlist 为空被拒、`deny` 后 `source`/`digest` 原样保留。

**提取的回归护体**：CLI 既有测试一条不改、全绿。这是「纯搬移」的判据。

**GUI**：binding 的 Go 测试 + 组件 vitest。**vitest 必须在 `frontend/` 目录跑**——父目录另有一个无 jsdom 的 v4，在那里跑会 `document undefined` 假失败。

## 八、风险与取舍

- **提取会动刚合入的代码**。已接受；CLI 测试是护体，且提取是纯搬移不改语义。
- **「立即生效」意味着 GUI 授权可能阻塞**在任务边界闸门上。已用 `pending_convergence` + 取消按钮处理，不假装同步。
- **端点是第四个 writer**，lost-update 窗口与 A5c 一样是「变窄不是关上」（read-compare-write 之间仍有间隙）。不在本期扩大治理范围，但端点必须接入既有守卫。
- 本期不做实时推送，面板状态靠拉取。插件状态若被 CLI 在别处改动，需重开面板或手动刷新才看得到。

## 九、未包含

GUI 里安装插件、`search` 与插件市场、OCI registry 传输、插件状态实时推送、密钥吊销与透明日志。

**真机验证**：六期下来从没有第三方插件在真机上挂载过。本期同样不承诺真机验证，证据仍来自单测、端到端测试与真 wasm 夹具。
