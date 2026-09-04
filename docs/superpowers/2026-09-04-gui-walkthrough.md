# GUI 真机走查记录（B/C：插件面板 + `wails dev` 累积改动）

**日期**：2026-09-04
**目标**：接续文档 [plans/2026-09-04-open-items-handoff.md](plans/2026-09-04-open-items-handoff.md) §二 的 B（GUI 插件面板人眼走查，上一期 T-2）与 C（`wails dev` 手动验证，上一期 T-3，累积 23 个提交）。
**结论**：检查点全过，抓出 **2 个真问题**（一个阻断级、一个界面缺陷），两条都已修并开 PR。
**修复**：GUI [#48](https://github.com/jxncyjq/stardust-agent-gui/pull/48)（错误链重复渲染）、GUI [#49](https://github.com/jxncyjq/stardust-agent-gui/pull/49)（`wails dev` 起不来 + 守卫）。

上一次同类记录：[2026-08-28-gui-plugin-walkthrough.md](2026-08-28-gui-plugin-walkthrough.md)。

---

## 一、抓到的两个真问题

### P1（阻断级）按 README 起 GUI 起不来

`wails dev` 默认先跑 `go mod tidy`，而 **`go mod tidy` 不认工作区的 replace**——它按单模块
模式解析，于是去公网拉从未发布的 `github.com/stardust/legion-agent@v0.0.0`，报
`Repository not found` 后退出。必须 `wails dev -m`。

这条知识**仓里本来就有**：CI（`package.yml`）与 `run.bat` 都用 `-m`，CI 的注释还把原因写得
很完整。唯独 README 从没改过——至今是 Wails 模板原文——而新人只读 README。走查第一步就撞在
这里，损失约二十分钟。

修法（#49）：README 重写为本仓实际说明（含代价：legion-agent 不发布就跑不了 tidy，`go.mod`
依赖要手工维护）、`run.bat` 的 `-m` 补原因注释、新增 `TestEveryWailsCommandLineSkipsModTidy`
钉住三个文件里**真正的命令行**。守卫第一版把散文里的提及也算成命令（9 条里 7 条假阳性），
收紧为「只认行首是 `wails` 的行」——逼人在句子里写 `-m` 的守卫一周内就会被绕过去。

### P2（界面）折叠了错误链，紧接着又铺开一遍

签名不受信任的插件行：人话「插件声明解析失败。」+ `<details>` 折叠链（有意设计），**紧接着
又用 `原因：{detail}` 把同一条链原样铺在下面**——305 字符、含 Windows 绝对路径、不可折叠。

两处渲染各自成立，凑在一起才是缺陷：服务端对这种包 `declared_error` 与 `detail` **都设**，
内容是同一条链（前者只多一个 `plugin consent: load declared manifest for …` 前缀）。

**测试为什么一直绿**：既有 F1 用例用 `state: 'unauthorized'` 构造 load_failed 行，而真服务端
返回的是 **`state: 'failed'`**（真机取证 `/v1/plugins`），那条裸 `detail` 段只在 `failed`
分支渲染。夹具用了一个服务端不会产生的状态。

修法（#48）：`chainAlreadyDisclosed(detail, declared_error)` 用**包含**判定去重（两者只差前缀；
真是两个不同的失败时仍要各说各的），且 `failed` 的 `detail` 自身也走折叠。

这是 2026-08-28 F1/F2 的第三次同类再现，通道不同。

## 二、检查点结果

| # | 检查点 | 结果 | 证据 |
|---|---|---|---|
| B1 | 不受信任行：人话 + 折叠链 | ✅（另见 P2） | `<details open=false>`，summary「显示详细错误」 |
| B2 | 该行「重新授权」不可点 | ✅ | `disabled=true`；同一行「撤销授权」可点是对的（点了弹确认框，不是直接执行） |
| B3 | 不受信任行没有取回按钮 | ✅ | `load_failed` 不给取回（重取只会读到同样的坏字节） |
| C1 | 同意流逐项授权 | ✅ | 能力只读且明说「不是菜单」；扩展点默认全不授予并解释方向与能力相反 |
| C2 | 授权落盘 | ✅ | `plugins.json` 写入 `grant.capabilities=[log,tool]`；`legion-bad` 始终授权不了 |
| C3 | 面板按事件自动刷新 | ✅ | 授权后失败原因当场从「缺 log 能力」变成「deployment accepts no tools」 |
| C4 | 模型徽标 | ✅ | `fake-model · context 未设` |
| C5 | 流式对话渲染 | ✅ | 真 SSE，assistant 气泡按 delta 出现 |
| C6 | **轨迹视图（P4b）** | ✅ | 按 turn 分组、组内按 seq 有序、空正文标「（无正文）」、搜索按内容过滤（"probe two" 只留两条命中） |

未覆盖：浏览器视图（要 Chromium）、文件卡片导出（要先产出文件）、chromium 安装脚本（会真下载，
本轮跳过）。

## 三、夹具（可复现）

夹具目录：`<scratchpad>/rt`（一次性，不入仓）。

| 件 | 做法 |
|---|---|
| 插件包 | `internal/plugin/host/testdata/e2e.wasm` + 手写 `plugin.json`（`abi:1`、`capabilities:[log,tool]`、`sha256` = wasm 摘要、一个 tool 声明） |
| 签名 | `agent plugins keygen --key-id demo-key --private-key demo.key` → 公钥填进 `keyring.json`；`agent plugins sign <dir> --private-key demo.key` |
| 坏包 | 先签名，**再**改 `plugin.json`（version 0.1.0→0.1.1）——签名覆盖的是 plugin.json 的原始字节 |
| 配置 | `agent.json` 全用**绝对路径**：plugins 的 manifest/root/cache/keyring 按**进程 cwd** 解析，而 `wails dev` 的 cwd 是 GUI 仓 |
| 假模型 | 本地 `127.0.0.1:18091` OpenAI 兼容端点，**必须支持 `stream=true` 的 SSE** |
| GUI | `LEGION_CONFIG=<rt>/agent.json wails dev -m -browser`，浏览器驱动 http://localhost:34115 |

### 假模型必须发 SSE

GUI 的对话路径走 `generateOpenAIChatStream`（`Stream: true`）。非流式 JSON 响应会让
`assistant/message` 的 content 落成**空串**、usage 全 0，界面显示「任务状态: done，暂无结果。
输入 0k 输出 0k」——**长得和产品缺陷一模一样**。补上 SSE（`data: {...chunk}` + 末帧 usage +
`data: [DONE]`）后，回答正常渲染。

假模型按**请求内容**作答，不按调用次数：按次数作答会让「页面重发了同一个请求」看起来像有进展。

## 四、三次自我纠正（都差点写成缺陷）

这一轮**每一条初判都是错的**，写下来是因为三次的形状不同：

| 初判 | 实际 | 怎么发现的 |
|---|---|---|
| 「Enter 不发送」「徽标 fake-model 却跑 recording」 | **旧实例没杀干净**，端口被占（新实例 bind 34115 失败仍继续），我连的是旧进程 | 日志里那行 `bind: Only one usage…` 被我先前当噪音跳过 |
| 「发送被静默丢弃」 | **`computer type` 打不进这个 React 受控 textarea**，`value` 一直是空 | 打完字不按回车先读 `textarea.value` → 空 |
| 「assistant 内容为空 = 缺陷」 | 假模型不支持流式（见上） | 查 `http_maas.go` 确认 `Stream: true` |

另外两条取证教训：

- **`window.go.main.App.*` 上装探针无效**：wailsjs 生成的模块在加载时就持有了绑定引用，替换
  `window.go` 影响不到它。看似「没有任何绑定调用」是假象。
- **console 会被轮询刷掉**：`ListSessions`/`ServeStatus`/`ListRuntimeEvents` 每秒都在发，
  想找的那条早被冲出窗口。硬证据要落到**后端**：`agent.db` 的 `tasks` / `session_events`、
  假模型进程的请求日志。

## 五、方法论（下次直接照做）

1. **先杀干净再开始**：`taskkill` 之后用 `netstat -ano | grep :34115` 确认端口真的空了；
   看到 `bind:` 失败就当场停下，不要继续走查。
2. **界面走查一律用 ref，不用固定坐标**（沿用上一期教训），但要知道 ref 点击也可能因为
   受控组件不生效——**改动输入框用 native setter + `input` 事件**：
   ```js
   const set = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set
   set.call(ta, '文本'); ta.dispatchEvent(new Event('input',{bubbles:true}))
   ```
3. **每个「缺陷」先做排除自己的对照实验**，再落笔。本轮 3 次初判全错。
4. **取证落到后端**，不要只信界面与 console。
