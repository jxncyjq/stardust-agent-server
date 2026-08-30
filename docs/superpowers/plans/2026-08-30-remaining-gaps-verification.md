# 三项遗留的真机验证（2026-08-30）

**状态**：三项全部跑完。**签名 + 远程源**与**提示词段排序/预算**按设计工作，无缺陷；
**浏览器接管的修饰键与错误码**验出四个真问题，全部**未修**（改的是对外契约，先记录再决定）。

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
