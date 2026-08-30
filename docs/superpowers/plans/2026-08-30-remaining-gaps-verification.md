# 三项遗留的真机验证（2026-08-30）

**状态**：三项全部跑完。**签名 + 远程源**与**提示词段排序/预算**按设计工作，无缺陷；
**浏览器接管的修饰键与错误码**验出四个真问题，**四个已全部修完并合入**：
3.1 修饰键（server #111 / GUI #34 / docs #24），3.2–3.4 错误码语义（见文末「修复记录」）。

夹具与命令在 scratchpad 的 `verify2/`（提示词段）、`verify3/`（签名+远程）、`verify4/`（接管）下，
模型是本地 mockmodel（按请求内容作答，不按调用次数）。

---

## 一、插件签名 + 远程源（无缺陷）

真流程：`plugins keygen` → `plugins sign` → 打成 `tar.gz` 挂在本机 HTTP → `plugins install --digest`
→ `serve` 加载。九个检查点：

| # | 场景 | 结果 |
|---|---|---|
| 1 | digest 不匹配 | 拒绝，报出期望与实际 digest；**字节从不落盘** |
| 2 | 未签名包 + `require_signature: true` | 拒绝于**解包**阶段：`archive is missing required file "plugin.sig"` |
| 3 | 正确签名 + 正确 digest + `--grant log` | 安装、写 manifest（`enabled: true` + digest）、`serve` 后 `state: loaded`，工具已注册 |
| 4 | 源站**关掉**后重启 serve | 照常加载（cache 按 digest 命中；内容寻址无需回源） |
| 5 | 篡改 cache 里的 `plugin.json`（把 capabilities 从 `["log"]` 偷偷改成 `["log","http"]`） | 签名不过 → 该插件 `state: failed`，serve 继续跑；**并把这份 cache 逐出**，理由写进日志 |
| 6 | 逐出后再启动（源站已恢复） | 重新下载、重新校验、加载成功 |
| 7 | `allow_insecure_sources: false` + http 源（**包已在 cache 里**） | **serve 直接拒绝启动**，指名插件与 URL；缓存命中不能绕过明文拒绝 |
| 8 | 换成只信任 `stranger-key` 的 keyring | `key id ...` 不在信任集 → `failed` + 再次逐出 cache |
| 9 | 恢复正确 keyring | 重新拉取、加载成功 |

两点值得记，都不是缺陷但会绊人：

- **远程包必须带 `plugin.sig`，与 `require_signature` 无关**：解包契约是「三个文件，不多不少」，
  少一个就拒。`require_signature: false` 只是**不验**签名，不是**不要**签名文件。
- 打开 `allow_insecure_sources` 后，每次启动都会 WARN 一行说明代价（可被观测/阻断、可被换成
  一个旧的但签名合法的版本），并说明 digest 仍然保证内容。

## 二、多插件提示词段：排序与 8192 rune 总上限（无缺陷）

五个插件（`aaa-brief` 60 runes，`bbb`/`ccc`/`ddd`/`eee` 各 2500 runes），**manifest 里按倒序注册**
（eee→aaa），每个只授 `prompt` 扩展点。拿 mockmodel 收到的真实请求体验证：

- **排序**：围栏出现顺序 `aaa-brief, bbb-bulky, ccc-bulky, ddd-bulky`——按插件名，不按挂载顺序。
- **单段 2048 rune 上限**：三个进入提示词的 bulky 段各带一个截断标记，段尾的 `[名字#END]` 标记
  全部消失、段首 `#START` 都在 → 确实是从头保留、尾部被切。激活期日志四条 `plugin prompt
  segment truncated`（含最终没进提示词的 eee）。
- **总预算 8192 rune**：`eee-bulky` **整段不进提示词**，日志
  `did not fit the total budget ... runes=2127 used=6441 limit=8192 consequence=这段完全不在系统提示词里`。
  不是截断，是整段拒绝并留痕。
- **前缀稳定**：两次不同任务的插件块**逐字节相同**（6896 runes）——这是它待在缓存稳定前缀里的前提。

## 三、浏览器接管：修饰键契约与错误码语义（四个问题，未修）

真 Chromium（系统 Chrome）、真会话（任务里 `browser_open` 打开的 `sess-1`）、真 HTTP。

### 3.1 修饰键根本无法表达 —— 而 GUI 每次按 Shift 都在发失败请求

`InputEvent` 没有任何修饰键字段，`namedKeys` 白名单里也没有 Control/Shift/Alt/Meta，
`keyToInputKey` 对它们报 `unsupported key`，而校验是**整批拒绝**：

```
POST .../input {"events":[{"type":"keydown","key":"Control"}]}
→ 400 {"error":"ELEMENT_NOT_FOUND: invalid input batch: event 0 (keydown): unsupported key \"Control\""}
```

GUI 侧 `BrowserView.onKeyDown` 的分流是「单字符 → char，其余 → keydown」，所以：

- 按 **Shift** 打大写字母：先发一条必定 400 的 `keydown Shift`，再发 `char "A"`——**大写字母是碰巧
  能用的**，代价是每次一条失败请求；
- **Ctrl+C / Ctrl+V / Ctrl+A 永远不可能生效**：`keydown Control` 被拒，随后的 `c` 作为 `char` 落到页面上，
  于是**「复制」变成往页面里输入一个字母 c**。

这不是实现 bug，是**契约缺口**：接管这个功能的前提是「人能像用浏览器一样用它」，而快捷键是其中一部分。
修法要选：①`InputEvent` 增加 `modifiers`（按 CDP 的 bitmask 传给 go-rod），或 ②白名单收下修饰键并按
press/release 维护按下状态。两者都要同时改 GUI 与后端，并补一条「Ctrl+A 真的全选」的真机断言。

### 3.2 错误码在讲一件不存在的事

`internal/browser/errors.go` 的 Code 是**给 Agent 看的可自恢复语义**（`ELEMENT_NOT_FOUND` = ref 失效，
建议重新 read）。接管这条路把它当成了通用错误码：

| 请求 | 实际返回 |
|---|---|
| `keydown Control` | 400 `ELEMENT_NOT_FOUND: ... unsupported key "Control"` |
| 坐标 1.5 | 400 `ELEMENT_NOT_FOUND: ... coordinate 1.5 out of [0,1]` |
| 空批次 | 400 `ELEMENT_NOT_FOUND: ... input batch is empty` |
| 视口 99999x1 | 400 `ELEMENT_NOT_FOUND: viewport 99999x1 out of range [100,8192]` |

一个校验错误告诉调用方「元素没找到，重新读页面」。人类调用方看不懂，Agent 若照建议做则是白做。

### 3.3 同一个「会话不存在」，三个端点三种状态码

```
POST /nope-9/takeover  → 404 {"error":"CONTEXT_EVICTED: unknown session nope-9"}
POST /nope-9/input     → 400 {"error":"CONTEXT_EVICTED: unknown session nope-9"}
POST /nope-9/viewport  → 400 {"error":"CONTEXT_EVICTED: unknown session nope-9"}
```

同样一件事，客户端要写三套判断。另外 `CONTEXT_EVICTED` 的语义是「Context 被回收，需重建 Session」，
用来回答「你给的 id 根本没存在过」是把两件事混在一起（补救动作恰好相同，所以危害有限）。

### 3.4 `SESSION_UNDER_TAKEOVER` 被用来表示「没有在接管」

```
POST /sess-1/input（takeover 关）
→ 409 {"error":"SESSION_UNDER_TAKEOVER: session sess-1 not under takeover; enable takeover before injecting"}
```

同一个码在别处（`runtime.go` 的 Open/Click/Type）表示**正好相反**的状态——「会话正被人接管，Agent 的写动作
挡下」。一个码承担两个互斥含义，只靠后面的散文区分。

### 3.5 顺带确认为设计而非缺陷的两条

- **接管开关本身工作正常**：开→注入 200，关→注入 409。
- **批次校验先于注入**：`[char "Z", keydown Control]` 整批 400，`Z` 没有被输入。注意这只覆盖**校验**失败；
  若失败发生在 `injectOne`（第 i 条注入时），前 i-1 条已经生效，此时**没有回滚**——这是既有取舍，不是本次新发现。
- **浏览器打不开内网/回环地址**：`browser.RuntimeConfig.AllowPrivateHosts` 在配置里**没有对应的键**，
  serve 装配也不传，所以恒为 false（代码注释写着「仅测试放开」）。本次验证因此改用公网页面。
  这是 SSRF 基线的自觉选择，但意味着**用内置浏览器访问自家内网 staging 是不可能的**，值得单独决策。

---

## 结论

- 签名 + 远程源：**可以按手册直接用**，正反路径都对，失败都响亮且可恢复。
- 提示词段：排序、单段截断、总预算、前缀稳定四条全部与设计一致。
- 浏览器接管：**功能通、契约缺**。3.1 是用户能直接撞上的（Ctrl+C 变成输入 c），3.2–3.4 是接口语义债。
  四条都需要改对外契约，未擅自动手。

---

## 修复记录（2026-08-30）

### 3.1 修饰键（已合）

`InputEvent.Modifiers`（ctrl|shift|alt|meta）随**每一条**事件走：注入前按下、注入后（含出错路径）释放，
不做跨请求的按下状态——注入是一串互不相干的 HTTP 请求，丢一条 keyup 就把浏览器永久留在 Ctrl 按住的状态里。
`char` 只接受 shift（它是 InsertText，收下 ctrl 等于把复制变成输入一个 c）；把修饰键当键发仍拒，但错误改成
指路。真机与 `-tags chromium` 双重证据：ctrl+c 到达页面且输入框仍为空。

### 3.2–3.4 错误码语义（已修）

三个问题同源：这套码是**给调用方的建议**，却被当成通用错误码用。

- 新增 `INVALID_INPUT`：空批次、越界坐标、认不出的键名、越界视口。此前一律 `ELEMENT_NOT_FOUND`——
  在建议「重新 read 页面」，而页面没有任何问题，重试同一个请求也永远不会成功。
- 新增 `SESSION_NOT_FOUND`，与 `CONTEXT_EVICTED` 分开：后者是「你**曾经**有的那个会话，上下文被回收了」，
  对一个从未存在过的 id 说这句话是在编造历史。两者补救动作相同，但排查方向相反。
- 新增 `TAKEOVER_REQUIRED`：注入被拒是因为**没有**在接管。此前回的是 `SESSION_UNDER_TAKEOVER`——
  字面意思正是拒绝理由的反面，同一个码承担两个互斥含义。
- HTTP 状态码不再由每个 handler 各挑一个，而是由**语义码**推出（`httpStatusForBrowserCode`）：
  400 请求本身错 / 403 部署拒绝这个目标 / 404 没有这个会话 / 409 会话在错误的状态（页面没了、接管开关不对、
  ref 失效）/ 500 没有语义码的错误（那是我们接线的缺口，不是调用方的错，回 400 会让调用方背锅并躲开 5xx 看板）。

真机复核（同一套部署）：

| 请求 | 之前 | 现在 |
|---|---|---|
| 未接管时注入（页面活着） | 409 `SESSION_UNDER_TAKEOVER` | 409 `TAKEOVER_REQUIRED: ... is not under takeover` |
| 会话不存在（takeover / input / viewport） | 404 / 400 / 400，均 `CONTEXT_EVICTED` | **三者一致 404** `SESSION_NOT_FOUND` |
| 会话在但页面没了 | 400 `CONTEXT_EVICTED` | 409 `CONTEXT_EVICTED` |
| 空批次 / 越界坐标 / 越界视口 | 400 `ELEMENT_NOT_FOUND` | 400 `INVALID_INPUT` |
| 把修饰键当键发 | 400 `ELEMENT_NOT_FOUND: unsupported key` | 400 `INVALID_INPUT: ... put "ctrl" in this event's "modifiers"` |
| ctrl+c / ctrl+click（接管中） | 400（不可能表达） | 200 |

变异验证两处：把 `SESSION_NOT_FOUND` 映射改回 400 → 三端点一致性测试红；把校验失败的码改回
`ELEMENT_NOT_FOUND` → 语义测试红。

---

## 四、出口代理的真实网络验证（E13 + V2，2026-08-30）

### E13：`--proxy-bypass-list=<-loopback>` 现在有覆盖了，而且之前那条结论是**错的**

早先写过「那个参数的效果测不出来，变异不红」。**结论错了**，错在量错了东西：当时的代理放行一切，于是「绕没绕过代理」在结果上看不出差别。

换成「让代理拒绝回环、让 runtime 放行，再看那台回环服务器有没有被碰到」之后：删掉该参数 → 红（服务器真的被直连到）；让代理不再拒绝回环 → 也红。也就是说，**这个 Chromium 确实会绕过回环**，那个参数一直在真正起作用。

### V2：四条真实网络探针（`go test -tags network`，默认不跑）

| 探针 | 结果 |
|---|---|
| https 走 CONNECT 隧道（example.com） | 通，页面内容正确 |
| 跨协议重定向 http→https 逐跳受检 | 通（3.1s） |
| 10MB 下载穿隧道 | 通（26s，字节数精确） |
| 云元数据 169.254.169.254 | 403，且是代理自己的拒绝文案 |

**它抓出两个本地测试造不出来的真问题**：

1. **CONNECT 隧道丢掉了握手的开头字节。** 客户端常常不等 `200 Connection Established` 就把 TLS ClientHello 跟在 CONNECT 后面发出来，那几个字节此刻已经在 net/http 的 `bufio.Reader` 里；旧实现从 Hijack 返回的**裸 net.Conn** 读，就再也拿不到它们——对端一直等，客户端最后报 `unexpected EOF`。本地测试永远碰不到，因为 Go 的 http.Transport 总是先等 200 再发。修法是接着用 Hijack 给的 bufio 读。
2. **连不上被报成了「策略拒绝」。** 拨号失败回 403，于是排查的人（我）先去查了一圈策略，而真相是 TCP 超时。现在拆开：403 = 策略拒绝，502 = 目标允许但连不上。

### 一次写错又改对的修法（值得记）

第一版为了补那几个字节，写成「`Peek` 出来先发一遍，再照常拷贝」。**Peek 不消费缓冲**，于是同一段字节被发了两遍——8KB 的早发数据一下把重复暴露出来（回显里带重复前缀）。正确做法只有一件事：**从 bufio 读，而不是从裸连接读**。

回归测试也踩了同一类坑：第一版用十几个字节做早发数据，两个变异都不红——那些字节其实还留在 socket 里，怎么改都能拿到。改成 8KB（大于 net/http 的读缓冲）之后，「从裸连接读」这个变异立刻红。

### 一条环境事实，不是缺陷

开发这台机器到 `github.com:443` 的 TCP **本身不稳**（直连三次一通两超时），重定向探针因此换成了 cloudflare.com。这件事是靠上面那条 502/403 的区分在几秒内看清的——在此之前它伪装成「策略拒绝」。

## 五、E14：半途失败的一批注入（2026-08-30）

清单上写的是「`injectOne` 中途失败不回滚」。**回滚这条给不出**，也不该假装给得出：已经发生的点击、已经输入的字符，浏览器里没有撤销这回事。把语义改成两条能兑现的承诺：

1. **不留半按状态。** 批次里按下过、还没轮到抬起的鼠标键与普通键，失败时放掉。留一个按住的左键，用户看到的是「浏览器坏了」——之后每一次移动都成拖拽，每一次点击都落在选区上。
2. **说清做到第几条。** 调用方唯一能做的判断是「哪些已经生效」：重发整批会把前面几条再做一遍（又点一次「提交」）。

边界同样重要，而且方向相反：**成功的批次不许替它抬起**。GUI 把 mousedown 与 mouseup 发在不同批次里，拖拽正是这么来的；一刀切地收尾会把每一次拖拽都掐断在起点。四条单测各自钉住一个方向，四个变异（不收尾 / 成功也收尾 / 抬起不记账 / 错误不带进度）逐个验红。

### 为此动的结构

`injectOne`/`holdModifiers` 从 `*rod.Page` 改到 `inputTarget` 接口（`pageTarget` 是唯一的生产实现）。原因不是抽象洁癖：真机上很难让第 i 条事件稳定失败，而这里要钉的**恰恰是失败之后的那段收尾**。

### 进度必须是字段，不是句子

第一版只把「applied 2 of 5」写进错误句子里。那样客户端要拿到这个数就得去解析措辞——把一个**会影响副作用**的判断建在字符串格式上。改成 `browser.PartialInjection{Applied, Total, Failed}`，HTTP 层用 `errors.As` 取出来，失败响应带 `injected` / `total` 字段，状态码仍走包在外层的 `BrowserError`（这里是 409）。两个变异（退回纯文本 / 状态码写死 500）验红。

GUI 侧不需要改：`browserPost` 已经把非 2xx 的响应体原样带进错误里，这两个数就在其中。

## 六、P6a：进程池的负载测试（2026-08-30）

池此前只被**顺序**地测过：一条 goroutine 依次拿、依次还。而它在生产里的形状恰恰相反——多个任务同时开会话、reaper 与用户的关闭同时落下。**上限只在无人竞争时成立，等于没有上限。**

四条并发测试（全部配 `-race`）：

| 测试 | 钉住的东西 | 验红的变异 |
|---|---|---|
| 同时抢时上限仍是上限 | 200 并发只发出 32 个（4×8），其余一律 RESOURCE_EXHAUSTED；无重复 context | 起进程时放开池锁（那个「别在冷启动时持锁」的常见优化）→ 进程数越界 |
| 取还交替后账是平的 | 每个进程 contexts 归零、owners 清空 | Release 不清 owners |
| 关闭与获取撞在一起不漏进程 | 关闭后创建的进程数为 0，所有进程都被关 | Close 不留关闭标记 / Acquire 不看它 |
| 内存门槛并发不被绕过 | 64 并发全部被拒，且逐次读数（不缓存） | 门槛判断恒真 |

### 抓到一个真的泄漏

**关闭之后还能起 Chromium。** `Close()` 把 `instances` 清空就完事，没留任何标记；随后（哪怕晚一微秒）到来的 `Acquire` 看到空池，于是**起一个新进程**并挂进那个已经废弃的 slice。那个进程不属于任何池：没人再会去关它，它带着自己的出口代理和一串 renderer 留在机器上，直到用户自己去任务管理器清。

这不是编出来的时序——serve 退出、GUI 关窗时，正有任务在开浏览器会话。整套 confinement（Job Object / Pdeathsig / 进程组）就是为了不让孤儿进程存在，而从这个口子漏出去的一样是孤儿。

修法：池上加 `closed`，`Acquire` 关后报错（**不给语义码**：这不是「稍后再试」，是进程正在退出）。`adopt` 同理，且关后收到的实例**当场关掉再报错**——留着它就是把进程丢在机器上。

### 一条自己写的测试差点白写

「关闭与获取撞在一起」第一版全靠调度：变异跑五次绿了两次。**一条半数会绿的测试等于没有测试**。改成两波——第一波与 Close 并发（给 -race 看交错），第二波在 `Close()` 确实返回之后才开始，于是「关后不许起进程」这条是确定性的。同一变异现在 5/5 红。

### 顺带写清一件事

内存门槛（`MinFreeMemoryBytes`）是**逐次的安全余量，不是配额**：并发放行多少个由池决定，门槛本身不做在途记账。测试断言的是「低于门槛时并发一个都过不去」与「逐次读数、不缓存」，没有假装它是配额。

## 七、A⑤：macOS 沙箱与孤儿缺口 / Windows AppContainer（2026-08-30）

按 bubblewrap 那次的教训**先探再写**：上次在 Linux 上一路写完才发现 CI 起不来，返工四轮。这次先花八轮 CI 把事实问清楚，再动手。

### Windows：AppContainer 走不通（本机实测，结论是否定的）

用 `CreateAppContainerProfile` + `PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES` 真起了一次 Chrome。profile 建得出来、ACL 也授权了，浏览器仍然当场死在：

```
FATAL:crashpad_client_win.cc:323 Check failed: CreateNamedPipe: 拒绝访问 (0x5)
```

Crashpad 在 Chrome **早于命令行开关处理**的阶段就要建具名管道，而 AppContainer 进程建不了全局命名空间里的管道。`--disable-breakpad`、`--disable-crash-reporter`、`--no-crashpad`、`--disable-features=Crashpad` 四种写法逐个试过，症状一字不变。

这与 Chrome 的架构一致：它自己**用** AppContainer 关它的渲染进程，broker（浏览器进程）必须在外面。所以 Windows 侧维持现状——Job Object（kill-on-close + 内存上限）已经在管进程生死与资源，AppContainer 加不上去，不是没做，是做不成。

### macOS：sandbox-exec 可行，profile 收到了「只有自己能写」

| 探针 | 结果 |
|---|---|
| `/usr/bin/sandbox-exec` 存在 | ✅（Apple 标 deprecated 多年，仍随系统发） |
| Chrome 在 profile 下宣告 DevTools 地址 | ✅ |
| **保留 Chrome 自带沙箱**（不加 --no-sandbox） | ✅ 两层可叠 |
| 只放行回环出网 | ✅ 浏览器照常起来 |
| profile 之外的写被拒 | ✅ |
| 父进程被 SIGKILL 后子进程存活 | ✅ 缺口确认 |
| 看门狗收掉孤儿 | ✅ |

出货的 profile：整盘可读、**只有 user-data-dir 与本用户的 T/C 可写**、出网只留回环。

三个只有真机才问得出来的事实：

1. **Chromium 不认 TMPDIR 的重定向。** 把 TMPDIR 指进 profile 目录之后它照样起不来；放行本用户的 `T` 与 `C` 之后立刻就起来了。
2. **SBPL 的 subpath 按解析过软链的真实路径匹配。** macOS 上 `/var`、`/tmp` 都指向 `/private/…`；写未解析的路径进去，内核对不上，于是**连自己的 profile 目录都写不了**，浏览器一个字都说不出来就退出。
3. **`(with report)` 不能加在 deny 上。** `sandbox-exec: report modifier does not apply to deny action` —— 整份 profile 编译失败，症状是所有变体一起红，看上去像 Chrome 起不来。

孤儿缺口（D4）由 `ConfineProcess` 起的看门狗补：盯着 agent 自己的 pid，一没就杀浏览器的**进程组**。它不占浏览器进程的位置（pid 仍是 Chromium 的，内存采样与进程池照旧），代价是每个浏览器进程多一个在睡觉的 sh。

### 这一轮里我自己写坏又改对的三处

1. **第一版 profile 什么都没挡**：整片放行了 `/private/var/folders`，而 macOS 上每个 app 的临时目录都在它底下。更糟的是探针当时报绿——它挑的「外面」（`t.TempDir()`）恰好也在那片里，测的是自己挖的洞。
2. **「非零退出」被当成了「被沙箱拒绝」**：profile 编译不过也是非零退出。那条断言因此以错误的理由绿过一轮。现在要求输出里确实有 `Operation not permitted`。
3. **一条只在别处生效的测试**：守「路径要解析」的那条在 Windows 上跳过，于是对应变异在我实际跑测试的机器上是绿的。补了一条不依赖软链的：profile 里每个路径再解析一次必须还是它自己（macOS 上 `/var→/private/var` 露馅，Windows 上 8.3 短名同样露馅）。
