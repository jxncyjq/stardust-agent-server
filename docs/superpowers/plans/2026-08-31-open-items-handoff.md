# 本期收尾与未决事项（2026-08-31）

**这份文档的用途**：本期到此为止，三个仓 master 干净、零开放 PR。下面是**没做的事**，以及每一条「为什么它还在这儿、下一步具体做什么」。给下一个接手的人（可能还是我）看。

**三仓 tip**：`stardust-agent-server` `7ef4f7b` / `stardust-agent-gui` `90f6051` / `docs` `main`。

---

## 一、本期完成（已合 master）

| 事项 | PR | 这条真正抓到的东西 |
|---|---|---|
| E13 回环不豁免出口策略 | server#124 | **推翻了我先前「这参数测不出效果」的结论**；删掉它会红 |
| V2 出口代理跑真实网络 | server#124 | CONNECT 隧道丢握手开头字节；连不上被报成策略拒绝 |
| E14 半截注入 | server#125 | 语义纠正（回滚给不出）；进度从句子改成字段 |
| P6a 进程池并发压测 | server#126 | **关闭后仍能起 Chromium**（孤儿泄漏） |
| A⑤/D4 macOS 孤儿看门狗 | server#127 | macOS 无 Pdeathsig，缺口实测确认 |
| agent-ci 连红四次 | server#128 | 我合前三个 PR 时只看了 Browser Matrix |
| V1 签名 + 远程源真机 | server#129 | 被拒的包被报成「尚未缓存」→ 界面给出按不动的按钮 |
| V3a 内置 Chromium 整条链 | server#130 / gui#40 | 那个字段**从没人填过**，三处断开 |
| V3b 三平台打包 CI | gui#40 | 仓自己建不起来（跨仓 replace）；Linux 需 `webkit2_41` |
| V3c 按平台的安装脚本 | gui#41 | 四个只在真机上存在的坑 |
| logfmt 标签不进给人看的字段 | server#131 | 三个写点，第一版只守住一个 |
| 两条分叉的取证与结论 | server#132 | 证伪了我自己关于「帧尺寸浪费」的猜想 |
| 按系统去 GitHub 取脚本并执行 | gui#42 | 网络这一跳在信任上没多给什么——写在代码注释里 |

## 二、已决策不做（有取证，不是搁置）

| 事项 | 结论与触发条件 |
|---|---|
| **Windows AppContainer** | 做不成。Crashpad 在命令行开关处理**之前**就要建具名管道，AppContainer 建不了（`CreateNamedPipe 0x5`），四种关 crash reporter 的写法症状一字不变。这与 Chrome 架构一致：它自己**用** AppContainer 关渲染进程，broker 必须在外面。维持 Job Object。 |
| **macOS sandbox-exec 外层沙箱** | 对 Google Chrome 做不成——连「什么都不限、只包一层」的 profile 都起不来（Chromium 开源构建可以，但 macOS 上装的通常是 Chrome）。**不假装有**：`ConfineProcess` 在 macOS 上提供的是孤儿保护，不是隔离。已调通的 SBPL profile 没有留在仓库里（无调用方的安全代码是负资产）。 |
| **Chromium 打进安装包** | 已拍板不做，改安装脚本。 |
| **WebRTC 帧升级** | 回环上买不到东西（实测：稀疏 6.6 KiB/帧、稠密 263 KiB/帧、2.1 MiB/s）。**重开条件**：①远程接管成为产品目标 ②帧率要求远高于 8fps。真咬人时先走更便宜的三条：自适应 quality → 提高 `EveryNthFrame` → 本机 MJPEG 二进制流。详见 `docs/superpowers/specs/2026-08-31-browser-forks-webrtc-and-set-of-marks.md`。 |

---

## 三、未完成：需要拍板的

### D-1 界面上那次询问（触发浏览器安装 + 显示进度）

gui#42 只交付了**机制**：`App.InstallBundledChromium()` 取脚本、校验、执行，输出逐行发 `chromium:install` 事件。**没有让它开机自己跑**——它执行的是从网上取回来的代码。

- **要决定的**：首次运行发现没有内置浏览器时，是弹一次询问（推荐），还是静默安装。
- **下一步**：前端在 `BundledChromiumPath()` 为空时展示一次提示，接 `chromium:install` 事件显示进度。
- **代价**：小，纯前端 + 一个 store。

### D-2 签名清单：真正的「不发新版也能换脚本」

现在的形态是**钉住 commit + 编译进二进制的摘要**。诚实地说：摘要既然在二进制里，取回来的就只能等于随包那份——**网络这一跳在信任上什么也没多给**，只是省了把 4KB 脚本塞进二进制。

- **要决定的**：是否让仓库发布一份**签名清单**（URL + 摘要），App 用内嵌公钥验签。那样才能不发新版就换脚本，同时仍然可验。
- **可复用**：`internal/plugin/sign`（Ed25519 密钥环、撤销、`agent plugins keygen/sign`）已经在用，且本期做过真机验证（8 条负向对照）。
- **代价**：中。需要发布流程配一把签名钥匙，并决定钥匙谁保管。
- **不做的后果**：换脚本必须发新版——对目前的节奏未必是问题，写下来是为了别在某天「顺手改成指向分支」（那等于把执行权交给任何能推该分支的人）。

### D-3 代码签名 / macOS 公证

三平台能打出包，但**没有签名**：用户下载会被 Gatekeeper / SmartScreen 拦。

- **要决定的**：现在做还是发布前再做。
- **阻塞**：需要 Apple 开发者账号与 Windows 代码签名证书；**凭据我拿不到**，只能把 CI 接线与流程写好，由你注入 secrets。
- **代价**：中（CI 改动小，证书采买与保管是主要成本）。

### D-4 工具结果的图像通道 → set-of-marks

set-of-marks 的唯一前提。链条断在：`domain.ToolResult` 只有 `Output string`；`port.Message.Images` 明确**只在 user 消息上**。今天做出来的标注图没有任何路径能交给模型。

- **最小路径（按依赖顺序，不可跳）**：①工具结果的图像通道（跨 `domain`/`runtime`/`port`，**与浏览器无关**，是多模态管线本身，且要确认在用的模型端点支持视觉输入）②`Element` 加几何（与 ref 同一次遍历，避免二次 DOM 往返与 ref 漂移）③标注与 `mark → ref` 回指（ref 语义不变是它能安全接进来的前提）。
- **代价**：①中偏大，②③小。
- **提醒**：**不要先做 ②③**。这个月已经栽过两次「造了没人消费的东西」（内置 Chromium 那一级、macOS 那份没有调用方的 profile）。

---

## 四、未完成：不需要拍板，做就是了

### ~~T-1 Windows 上 `TestBrowserStreamE2EObservationProgressFrame` 抖动约 40%~~ ✅ 已处置（2026-09-03，server#151）

**这里原先给的取证方向被推翻了。** 原话是「先判定是『Windows 上首帧确实更慢』还是『测试的等待窗口写死得太紧』——前者要改产品，后者改测试」，做法是三平台打点比较分布。

两条都不是。问题在测试自己身上：**它把诊断需要的信息全丢了**。

```go
read, _ := rt.Read(...)     // 错误丢弃
for ... { if 名字含「按钮」 { ref = e.Ref } }
_, _ = rt.Click(...)        // 错误丢弃，ref 可能是空串
```

Read 失败或页面里找不到那个按钮时，ref 是空串、Click 必然失败、progress 永远不来，而测试只报一句 `missing events: progress=false`。**红了三次都查不下去，正是因为这两个被丢掉的错误**（顺带违反 fail-loud 铁律）。

另两处一并修了：

- `time.Sleep(200ms)` 等订阅建立是多余的——`handleBrowserStream` 先 `Subscribe` 再写响应头，`Do()` 返回时订阅已经在了。去掉后本机耗时 1.4–1.6s → 1.3s。
- `for sc.Scan() && time.Now().Before(deadline)` 里的 deadline 形同虚设：`Scan()` 阻塞，时间只在每次 Scan 返回之后才检查，流里迟迟没有新行时它根本不生效。

现在失败信息能区分三种病：动作本身失败了（带上读到的元素名）、动作成功但帧没到、动作到此刻还没返回。

**没有声称「修好了 flake」**：本机带 `-tags chromium` 连跑 5 次全绿，复现不了 CI 的抖动。这次改的是**下次它再红时能不能查下去**。合并时三平台 Browser e2e 全绿（含 windows-latest）。

变异验证：把页面按钮的 `aria-label` 改掉，测试报「触发动作本身就失败了：页面里没有名字含「按钮」的元素，读到的是 ["别的东西"]」，而不是从前那句看不出原因的话。

### T-2 GUI 插件面板人眼走查

V1 全程走 HTTP API。面板的渲染分支有前端测试守着，但「屏幕上确实不给那个按钮」没有人看过。server#129 修的正是那条会让面板给出**按不动的按钮**的数据。

**下一步**：`wails dev` 起来，造一个签名不受信任的包，确认面板显示的是「人话 + 折叠错误链」且**没有获取按钮**。

### T-3 `wails dev` 手动验证（累积）

本期 GUI 侧改动不少（浏览器视图独立栏、安装脚本、`chromium:install` 绑定），只有自动化测试与三平台打包 CI，没有人真开着界面走一遍。

---

## 五、维护约束（下次改动前先看这几条）

1. **改 `scripts/install-chromium.*` 要动两处**：`internal/chromium/install.go` 里的摘要，以及 `scriptRef` 指向的 commit（且那个 commit 必须先推上去）。忘前者 → `TestTheEmbeddedDigestsMatchTheScriptsInThisRepo` 立刻红；忘后者 → **只有 `-tags network` 那条会红**，而常规 CI 不跑它，症状是用户装不上、报错却指向网络。
2. **`scriptRef` 永远是 commit SHA，不能是分支名**。有测试守着（认 40 位十六进制），但要知道它守的是什么：指向分支 = 谁能推该分支，谁就能在每台用户机器上执行代码。
3. **改浏览器配置字段要过那条键覆盖断言**（`TestEveryBrowserConfigKeyReachesTheRuntime`，现 15 键）。少接一个字段不会让任何东西报错——浏览器照常起来，那个开关只是永远是零值。
4. **`-tags network` 与 `-tags sandboxprobe` 的东西不进常规 CI**：一条依赖外网的红线会变成没人看的红线。它们是「人在真机上跑一遍」的验证：
   - `go test -tags network ./internal/browser/`（出口代理走真实网络）
   - `go test -tags network ./internal/chromium/`（GUI 仓：真 GitHub 上的脚本与随包摘要相符）
   - `go test -tags chromium ./internal/browser/ -run TestMeasureTheScreencastChannel -v`（帧通道度量；重开 WebRTC 话题前先跑它拿新数字）
   - `go test -tags sandboxprobe ./internal/browser/`（macOS 沙箱探针；下次有人想在 macOS 上加外层沙箱，先跑这个）
5. **macOS 安装脚本要的是 `.app/Contents/MacOS`**，不是 `.app` 所在目录。给错的后果是浏览器装到 `.app` 旁边——脚本一路成功、App 起来找不到。脚本会拒绝并打印该给的路径。

---

## 六、本期反复出现的两类错（写下来，因为它们还会来）

**一、「接缝在，但没人调用它」。** 本期三次：内置 Chromium 那一级优先级（配置键、`RuntimeConfig` 字段、装配那一跳，三处断开）；`allow_private_hosts`（字段有、键没有）；加固开关（服务端做好了、宿主没说要，于是每个出货的 GUI 都跑着一个谁都能连的 serve）。**共同症状是没有症状**——功能照常工作，只是走的是另一条路。防法是给每一跳单独一条断言，并**逐个做变异验证**：本期两次出现「只加一条断言，另外两处的变异都不红」。

**二、「绿得不是地方」。** 变异脚本按双空格替换而 gofmt 是单空格，于是变异根本没生效，我差点把「测试没守住」当成结论；探针八轮探的不是生产会用的那个浏览器（`CHROME_PATH` 的 Chromium vs `/Applications` 的 Google Chrome），八轮结论全部作废；「非零退出」被当成「被沙箱拒绝」，而一份编译不过的 profile 也是非零退出。**共同点是：绿/红本身没错，错在它证明的不是我以为的那件事。**
