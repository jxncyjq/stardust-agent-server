# GUI 插件面板真机走查记录

**日期**：2026-08-28
**目标**：交接文档 `2026-08-27-handoff.md` §二.1「GUI 界面的真机走查」——八期以来第一次有人在真界面上把「取回声明」这条链路点完。
**结论**：四个检查点全部通过；另外抓出三个界面缺陷（全在呈现与状态收敛，后端无缺陷）。
**修复**：三个缺陷已修，GUI [#25](https://github.com/jxncyjq/stardust-agent-gui/pull/25)（分支 `fix/plugin-panel-error-and-refresh`，待合），修完在同一套夹具上真机复验过。

---

## 一、夹具（可复现）

夹具目录：`<scratchpad>/rt`（一次性，不入仓）。

| 件 | 做法 |
|---|---|
| 插件包 | `internal/plugin/host/testdata/e2e.wasm` + 手写 `plugin.json`（`capabilities: ["log","tool"]`，`sha256` = wasm 摘要） |
| 签名 | `agent plugins keygen --key-id demo-key` → `keyring.json`；`agent plugins sign <dir>` |
| 源站 | 本地 Go 静态站 `127.0.0.1:18099`，`/good.tar.gz` 直发、`/slow.tar.gz` **每 512 字节停 1.2s**（全长约 13s，卡在 fetch 上）、`/bad.tar.gz` 签名被改坏 |
| 部署 | `plugins.json` 三条：`legion-demo`(好) / `legion-slow`(慢源) / `legion-bad`(坏签名)。前两条走 `agent plugins install`（无 `--grant`），第三条手写——`install` 会验签，坏包装不进去 |
| 「未缓存」 | install 之后删 `cache/sha256/*`，`declared_unresolved_reason` 即回到 `not_cached` |
| GUI | `wails build`（**不带 `-s`**）后 `build/bin/legionAgentGUI.exe <配置>`；交互走查另用 `LEGION_CONFIG=... wails dev` + 浏览器（同一份前端、同一个 in-process serve），因为主显示器 5120px 宽，截图降采样后界面文字不可读 |

## 二、四个检查点

| # | 检查点 | 结果 | 证据 |
|---|---|---|---|
| 1 | 未解析行出现「取回声明」（次要样式），「授权」仍禁用 | ✅ | 三行均 `border-input / text-muted-foreground`（非 `bg-primary`），三个「授权」`disabled=true` |
| 2 | 取回成功后常驻显示「已取回并缓存该插件包」 | ✅ | 文案出现，「取回声明」消失，「授权」转为可用；点「刷新」后仍在 |
| 3 | 签名坏 → 醒目告警且**没有**重试 | ✅ | `text-xs font-semibold text-destructive` + `role="alert"`，该行「取回声明」消失；刷新后服务端把该行降为 `load_failed`，同样不给取回 |
| 4 | 取回期间 Esc / X / 背景 / 切 tab 四扇门都被拦 | ✅ | 在 `slow.tar.gz` 真在传输的 13s 窗口内逐一试过，设置面板均未关闭、tab 未切走；面板显示「正在取回声明，请稍候……」，且**没有**取消按钮 |

副作用核对（每次取回后查磁盘）：

```
cache/sha256/<digest>            ← 只多出取回的那个包
plugins.json                     ← 三条 entry 全无 grant 段，enabled 全 false
```

「取回不是授权」在真机上成立。授权对话框能正确列出声明的 `log` / `tool`（只读），即取回的意义所在。

WebView2 侧另外单独跑了一遍主路径（不经浏览器）：真窗口点「取回声明」→ 源站日志出现请求 → 缓存目录回填 → `plugins.json` 未动。

## 三、抓到的三个缺陷

### F1（呈现）安全告警里塞了整条原始错误链

不可信分支渲染的是 `该插件包未通过信任校验……：{resolveError}`，而 `resolveError` 是 Go 绑定回来的**裸字符串**，内容为：

```
resolve plugin "legion-bad": 插件包不被信任: post /v1/plugins/legion-bad/resolve failed:
status 422: {"error":"resolve plugin \"legion-bad\": plugin consent: resolve \"legion-bad\":
plugin package is not trusted: load plugin package \"C:\\\\Users\\\\...\\\\cache\\\\sha256\\\\0d7ff98...\":
verify plugin.json signature: plugin package is not trusted: verify signature: ..."}
```

单个 `<p>` 里 633 个字符，含 HTTP 状态行、JSON 包体、四层双重转义的 Windows 绝对路径。人要读的那句话被埋在中间，缓存绝对路径也就此进了界面。`load_failed` 那条注记同样是整链直出。

组件测试只断言「包含某子串」，抓不到这个。

### F2（呈现）刷新后同一个失败显示两遍

`load()` 只清 `overrides`，不清 `resolveError`。于是刷新之后：服务端那条 `插件声明解析失败：…` 与客户端那条 422 告警并排出现，同一个签名失败讲两遍，合计约 1200 字符。

### F3（状态收敛）`resolved` 遮蔽服务端真相，且刷新救不回来

`effectivePlugin = resolved[name] ?? plugin`，而 `resolved` **刻意**不被 `load()` 清除（`PluginsPage.tsx:182-188` 有注释说明理由：取回过就别让刷新把已付出的下载代价抹掉）。

问题在注释里那句 "Only the CACHE half stays true forever"——这个不变量只在缓存条目不会消失的前提下成立。实测：取回成功后从磁盘删掉该 digest 的缓存目录，再点「刷新」，界面仍然说「已取回并缓存该插件包」，并且**不再提供「取回声明」按钮**——也就是说，界面在陈述一件假事实，同时收走了唯一能修正它的控件。只有整个应用重启才恢复（页面重载后该行正确回到 `not_cached`）。

触发面：仓内**无 eviction API**，所以现实触发是运维清缓存目录、磁盘清理，或改 `plugins.json` 的 digest 后旧 digest 的 `resolved` 仍在遮蔽（后者未实测，属推断）。

严重度不高，但正是「per-task 评审对接缝失明」那一类：单看取回这个动作，不清 `resolved` 是对的；接上「服务端说它没了」这一侧，就是各说各话。

### 另记（未验）

取回进行中「刷新」按钮仍可点。没试过在飞行途中刷新会不会打乱状态。

## 三之二、三个缺陷的修复（GUI PR #25）

| 缺陷 | 改法 | 真机复核 |
|---|---|---|
| F1 告警塞整条原始错误链 | 新增 `ErrorDetail`：句子单独成行，链折进原生 `<details>`（**留着**，不是删掉——诊断供应链问题要靠它）。四处同形的链统一处理：不可信告警、取回声明失败、插件声明解析失败、重试失败 | 告警本体 33 字符，600 字符的链在「显示详细错误」后面 |
| F2 刷新不清客户端错误 | `load()` 现在清 `overrides` / `retryError` / `resolveError` 三样。`retryError` 顺带修掉一条更隐蔽的：它由 `load()` 会清的 override 把门，所以刷新后隐身，等下一次收敛挂起再冒出来，把旧失败记在没失败的请求上 | 刷新后该行只剩服务端那一份 |
| F3 `resolved` 遮蔽服务端真相 | 新增 `reconcileResolved`：不是清空而是**对账**——服务端仍认账就留着（否则刷新一次重付一次下载代价，那正是它要活过刷新的理由），服务端说未解析或解析失败就丢掉 | 删缓存目录后刷新，假的「已取回并缓存」消失、「取回声明」回来、「授权」重新禁用 |

验证：新增 6 个用例先红后绿（改前 5 红 1 绿，那 1 绿正是「服务端仍认账时别误删」的反向护栏）；变异核对把 `reconcileResolved` 改直通，F3 第一条即失败；全量 49 文件 / 289 测试通过（改前 49 / 283）；`tsc --noEmit` 与 `npm run build` 通过。

## 四、给下一个会话

- 夹具是一次性目录，已随会话结束失效；上面第一节足以在十分钟内重造。
- 交接文档 §二.2「收敛等待期间三扇门」仍未验——本次造的慢速源脚本正是它需要的东西（把 `slow.tar.gz` 的源换给一个未缓存的 `grant` 目标即可让 grant 卡在 fetch 上）。
- 主显示器 5120px 宽，截图工具降采样后界面文字不可读；要读界面文字就用 `wails dev` + 浏览器取 DOM 文本，别指望截图。
