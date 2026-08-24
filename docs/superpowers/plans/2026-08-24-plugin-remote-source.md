# A5b 插件远程来源与内容寻址缓存 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让插件不必先由人手工拷到部署方磁盘上——`plugins.json` 写一个 HTTPS 地址与摘要，宿主自己拉、自己校验、自己缓存，且缓存命中时完全不联网。

**Architecture:** 远程条目的 `source` 是一个 `https://` URL，并**必须**同时声明 `digest`。拉取按流校验摘要，不符即丢弃、绝不落盘；通过则以摘要为名进内容寻址缓存（`<cache>/sha256/<digest>/`），解包成与本地包完全相同的目录布局，之后原封不动交给 A5a 已有的验签与加载路径。**两道门各管一段**：`digest` 把守「这些字节该不该要」，签名把守「这个包该不该加载」。

**Tech Stack:** Go 1.26.0，module `github.com/stardust/legion-agent`。**不新增任何第三方依赖**——`net/http`、`archive/tar`、`compress/gzip`、`crypto/sha256` 全在标准库。

**不含**（各自独立 plan）：OCI registry 传输（本期只做 HTTPS 拉 tarball）、`legion plugin install|search` CLI 与 GUI 同意流（A5c）、密钥吊销与透明日志、运行期健康度驱动的自动卸载。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：禁止 fallback、禁止静默吞错、禁止零值假装正常；错误用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装；不变量违反用 `panic`。
- 公开 API 必须有 Go doc 风格注释，以标识符名开头，且不得与代码矛盾。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿，`gofmt -l .` 输出为空。
- 涉及并发的任务额外跑 `go test -race`，**plugin / runtime / cli 包串行跑**（`-p 1`）。
- 错误路径必须有测试断言「确实返回 error」，不得只测 happy path。
- 每个 task 完成后做一次**变异验证**：把该 task 的核心机制改坏，确认测试确实 FAIL，把失败输出留在报告里，然后还原。
- 设计依据：docs 仓 `docs/design/architecture/legion-plugin-system.md` §7（安全模型）、§9 路线图 P3-A5b。

## ⚠️ 本期的攻击面比前几期都大，四条额外规矩

前四期处理的都是**部署方自己磁盘上**的字节。本期第一次把「网络上的字节」引进来，且解压 tar 本身就是一类经典攻击面。

1. **摘要先于落盘**。边下边算 sha256，不符立刻丢弃临时文件；绝不「先存下来再看对不对」。
2. **解包必须防路径穿越**：tar 条目名含 `..`、绝对路径、指向包外的符号链接/硬链接，一律拒绝整个包（不是跳过该条目）。**只接受普通文件**——目录按需创建，其余类型（符号链接、设备、FIFO）一律拒绝。
3. **每一处都要有上限**：响应体字节数、解压后总字节数、条目数、单条目大小、请求超时。没有上限的解压就是 zip bomb 的入口。
4. **测试绝不打真实网络**。全部用 `httptest.Server`。一条会在 CI 里连公网的测试是不可复现的测试。

## ⚠️ Fork-bomb 安全规程（本仓已因此宕机两次）

`host.test.exe` 曾吃到 **170 GB 虚存**（Windows 事件 2004 + Kernel-Power 41），原因是测试把被测功能当唯一终止条件。

1. 任何循环边界必须独立于被测功能：轮数写成字面量（≤5），每轮断言实例数上限。
2. 每次 `go test` 带 `-timeout 120s`；plugin / runtime / cli 包用 `-p 1`。
3. 跑挂或内存上涨立刻杀掉、先加边界再重试。
4. 绝不把变异留在工作区，每次还原后用 `git status` 核对。

## 前置事实（已在 master `5391fdb`，直接用）

```go
// internal/plugin/manifest
type Entry struct {
    Name, Source string
    Enabled      bool            // 缺省 true
    Grant        GrantDecl
    Tools        []ToolAccept
    Config       json.RawMessage
}
func ParseDeployment(data []byte) (Deployment, error)   // 拒未知字段、拒尾部残留、拒重复条目名
func LoadPackage(dir string, keyring *sign.Keyring) (PluginManifest, []byte, error)
// 读 dir/plugin.json + dir/plugin.wasm + dir/plugin.sig；验签（keyring 非 nil 时）并逐字节校验 sha256

// internal/plugin/loader
func (l *Loader) Apply(ctx context.Context, dep manifest.Deployment, root string) error
func packageDir(name, root, source string) (string, error)
// 现状：拒绝绝对路径、拒绝走出 root 的 ..；返回 root 下的包目录

// internal/plugin/sign
func ParseKeyring(data []byte) (*Keyring, error)
```

**A5a 定下、本期必须守的合同：**
- 签名覆盖 `plugin.json` 的原始字节，经其中的 sha256 传递性覆盖 `plugin.wasm`。**远程拉取不得绕过这条**：解包后的目录必须原样走 `LoadPackage`。
- `require_signature` 缺省 true；nil keyring 只能来自显式 `false`。
- 安全控制不许因配置坏掉而自动关闭。

**`source` 的现有语义**：相对部署根的包目录，绝对路径与 `..` 一律拒绝，因为「插件的 wasm 从哪读」是一次信任决定。本期扩展它，但那条理由不变——只是信任决定从「哪个目录」变成「哪个 URL + 哪个摘要」。

---

### Task 1: 远程条目的清单形状

**Files:**
- Modify: `internal/plugin/manifest/manifest.go`（`Entry` 增 `Digest`，新增来源判别与校验）
- Test: `internal/plugin/manifest/manifest_test.go`

**Interfaces:**
- Produces:
  - `Entry.Digest string`（JSON 键 `digest`）
  - `func (e Entry) IsRemote() bool`
  - `func (e Entry) RemoteURL() (*url.URL, error)`

**判别规则**（写进 `Entry.Source` 与 `IsRemote` 的 doc）：`source` 以 `https://` 或 `http://` 开头即远程，否则按既有语义当作相对部署根的本地目录。

**`http://` 默认拒绝，但可由部署显式解锁**（调试期要能用本地静态服务器）。解锁开关是 Task 5 的 `plugins.allow_insecure_sources`，缺省 `false`；本 task 只做形状校验与判别，**不读该开关**——清单层不知道策略，策略在装配层。因此本 task 对 `http://` 的产出是「这是一条不安全的远程来源」这个事实，而不是「拒绝」这个动作。

明文传输在有 digest 的前提下**丢的是什么、不丢的是什么**，要写进开关的 doc（Task 5），这里先记清楚：digest 仍然逐字节把守完整性，所以中间人**换不掉**字节；丢的是机密性（谁在拉哪个插件是可观察的）与可用性（连接可被阻断），另外攻击者可以持续投喂一个**旧但合法**的版本。这就是它只该用于调试的理由。

**七条校验规则，逐条要有测试：**

1. 远程条目**必须**有 `digest`，缺失即报错点名该条目。这是本期的信任锚。**`http://` 与 `https://` 一视同仁**——恰恰因为明文通道更弱，digest 在那里更不能免。
2. 本地条目**不得**有 `digest`：本地目录的信任来自签名与部署方对自己磁盘的控制，写一个不会被校验的字段会让读者以为它生效了。
3. `digest` 形状必须是 `sha256:` 加 64 位十六进制，否则报错点名实际值。**只接受 sha256**——多一种算法就多一条「用弱算法签发」的路径，本期不需要。
4. `IsRemote()` 对 `https://` 与 `http://` 都为真；新增 `func (e Entry) IsInsecureSource() bool`，只对 `http://` 为真，供 Task 5 的策略判断。
5. URL 解析失败、或含用户信息（`https://user:pass@host/...`）→ 报错。凭据不该出现在一份会被提交进 git 的部署清单里。**这条对两种 scheme 都适用。**
6. 除 `http`/`https` 之外的 scheme（`file://`、`ftp://`、`ssh://`…）→ 报错点名该 scheme。**不要用「不是 https 就当本地路径」的写法**：那会让 `file:///etc/passwd` 被 `filepath.Join` 当成相对路径去拼，是一条静默的语义错位。
7. 既有的本地路径规则（绝对路径、`..`）保持不变，且要有测试证明扩展没有把它们放松。

- [ ] **Step 1: 写失败测试**（上述七条，每条断言**确实返回 error** 且点名字段/值；外加正向：一条 https 远程、一条 http 远程、一条本地条目在同一份清单里各自解析正确且 `IsRemote`/`IsInsecureSource` 取值正确，逐字段断言）

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证（两个）**
  - 把「远程必须有 digest」改成可选 → 该测试应 FAIL
  - 把「非 http/https 的 scheme 报错」改成「当作本地路径」→ `file://` 那条测试应 FAIL

- [ ] **Step 4: 提交**

```bash
git commit -m "feat(plugin): let a deployment entry name a remote source and its digest"
```

---

### Task 2: 拉取——边下边校验、绝不先落盘

**Files:**
- Create: `internal/plugin/fetch/fetch.go`
- Test: `internal/plugin/fetch/fetch_test.go`

**Interfaces:**
- Produces:
  - `type Limits struct{ MaxBytes int64; Timeout time.Duration }`
  - `func Fetch(ctx context.Context, client *http.Client, u *url.URL, digest string, limits Limits) ([]byte, error)`

**这个包只做「把字节安全地拿回来」**，不解包、不落盘、不认识插件——解包是 Task 3，缓存是 Task 4。它的全部职责是：发请求、限量读、算摘要、比对、返回字节或错误。

**七条实现约束，逐条要有测试（全部用 `httptest.Server`，绝不打真实网络）：**

1. **边读边算**：用 `io.TeeReader` 把响应体同时喂给 sha256 与缓冲区，读完比对。摘要不符 → 返回错误，**返回的字节丢弃**。
2. **`MaxBytes` 是硬上限**：用 `io.LimitReader(body, MaxBytes+1)`，读到 `MaxBytes+1` 字节即判定超限报错（`+1` 才能把「正好等于上限」与「超出」分开）。一个声称 10KB 实际无限流的服务器不能拖垮宿主。
3. **非 2xx 响应 → 报错**，错误里带状态码与 URL。
4. **重定向必须限次并保持 HTTPS**：装 `CheckRedirect`，跳数上限 10，任何一跳降级到 `http://` 立即拒绝。（A5a 期评审在 `http_request` 上抓到过同形状的绕过：只校验首跳等于没校验。）
5. **超时来自 `Limits.Timeout`**，且 `ctx` 取消要能真正中断下载（把 ctx 挂到 request 上，别只靠 client.Timeout）。
6. **摘要比对用 `crypto/subtle.ConstantTimeCompare` 或直接比较十六进制串**——这里没有时序攻击面（摘要是公开值），但错误信息**必须同时给出期望与实际**，否则运维无从判断是源变了还是配置写错了。
7. 空响应体（0 字节）不是特例：它照样要过摘要比对，不符即拒。

- [ ] **Step 1: 写失败测试**（上述七条；正向一条：摘要相符时返回的字节与服务器发出的完全一致）

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证（两个）**
  - 把摘要比对整个删掉 → 「摘要不符」的测试应 FAIL
  - 把 `LimitReader` 的上限去掉 → 「超限」测试应 FAIL（**这条测试的服务器必须自己有界**：发送固定的 `MaxBytes+N` 字节，不要写一个无限流的 handler——被测代码失去上限时，无限流会把测试进程吃穿，正是 fork-bomb 规程要防的形状）

- [ ] **Step 4: 提交**

---

### Task 3: 解包——路径穿越与解压炸弹

**Files:**
- Create: `internal/plugin/fetch/unpack.go`
- Test: `internal/plugin/fetch/unpack_test.go`

**Interfaces:**
- Produces:
  - `type UnpackLimits struct{ MaxEntries int; MaxTotalBytes, MaxEntryBytes int64 }`
  - `func Unpack(archive []byte, destDir string, limits UnpackLimits) error`

**布局契约**：tarball 解开后必须**直接**是 `plugin.json` / `plugin.wasm` / `plugin.sig` 三个文件（允许一层同名顶层目录，若有则剥掉）。多余文件一律拒绝——一个插件包里不该有别的东西，而「拒绝多余」比「忽略多余」少一条把恶意内容带进缓存目录的路。

**九条安全规则，逐条要有测试：**

1. 条目名含 `..`、以 `/` 开头、或经 `filepath.Clean` 后走出 `destDir` → **拒绝整个包**，不是跳过该条目。
2. 非普通文件（符号链接、硬链接、设备、FIFO、目录以外的一切）→ 拒绝整个包。符号链接是路径穿越最常见的载体。
3. 条目数超 `MaxEntries` → 拒绝。
4. 单条目解压后超 `MaxEntryBytes` → 拒绝。
5. 解压后总字节超 `MaxTotalBytes` → 拒绝。**这三条上限缺一不可**：只限总量挡不住一亿个空文件，只限条目数挡不住一个 10GB 的文件。
6. 出现契约之外的文件名 → 拒绝并点名该文件。
7. 三个必需文件缺任何一个 → 拒绝并点名缺哪个。
8. gzip 解压同样要走 `io.LimitReader`——压缩比可以做到极高，**限的必须是解压后的字节数，不是压缩包大小**。
9. 写文件权限 0600，目录 0700：缓存目录里放的是即将被执行的 wasm 与验证它的签名。

- [ ] **Step 1: 写失败测试**

夹具在测试里现造（`archive/tar` + `compress/gzip` 写进 `bytes.Buffer`），**不要提交任何恶意 tarball 进仓库**——一个仓库里躺着的路径穿越样本，迟早会被某个扫描器或某个手快的人当成真问题。

**解压炸弹用例必须自己有界**：造一个解压后约 `MaxTotalBytes + 1KB` 的包（例如高度可压缩的重复字节），断言被拒。绝不造一个「真正的」几 GB 炸弹——被测代码失去上限时它会吃穿测试机。

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证（三个）**
  - 去掉 `..` 检查 → 路径穿越测试应 FAIL
  - 把「非普通文件拒绝」改成「跳过」 → 符号链接测试应 FAIL
  - 去掉 `MaxTotalBytes` → 解压炸弹测试应 FAIL

- [ ] **Step 4: 提交**

---

### Task 4: 内容寻址缓存

**Files:**
- Create: `internal/plugin/fetch/cache.go`
- Test: `internal/plugin/fetch/cache_test.go`

**Interfaces:**
- Produces:
  - `type Cache struct{ /* unexported */ }`
  - `func NewCache(root string) (*Cache, error)`
  - `func (c *Cache) Dir(digest string) string`
  - `func (c *Cache) Has(digest string) (bool, error)`
  - `func (c *Cache) Put(digest string, archive []byte, limits UnpackLimits) (string, error)`

**布局**：`<root>/sha256/<64 位十六进制>/`，里面就是解开的三个文件。**摘要即身份**：同一个摘要必然是同一份字节，所以没有「过期」这回事，命中即可用、无需联网。

**四条不变量，逐条要有测试：**

1. **`Put` 是原子的**：先解包进同目录下的临时目录，成功后 `os.Rename` 就位。一次中断的解包绝不能留下一个「看起来命中」的半包——那会让下一次启动加载半个插件，且因为摘要目录已存在而永远不再重拉。
2. **`Put` 已存在的摘要是幂等的**：直接成功返回，不重解包（并发的两个 `Put` 也不能互相破坏）。
3. `Has` 只认「目录存在且三个文件齐全」。只看目录在不在，会把上一条的半包判成命中。
4. 摘要形状非法 → 报错，绝不据此拼路径。**这是路径穿越的最后一道**：`Dir("../../etc")` 必须报错而不是返回一个逃出缓存根的路径。

- [ ] **Step 1: 写失败测试**（上述四条；并发用例：10 个 goroutine 同时 `Put` 同一个摘要，全部成功且目录内容正确，`-race` 干净。**goroutine 数与轮数都写字面量**）

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证（两个）**
  - 把 `Put` 的「临时目录 + rename」改成直接解进目标目录 → 「中断的解包不会被判成命中」的测试应 FAIL
  - 去掉 `Dir` 的摘要形状校验 → 路径穿越测试应 FAIL

- [ ] **Step 4: `-race`**

```bash
go test ./internal/plugin/fetch/ -race -count=1 -p 1 -timeout 120s
```

- [ ] **Step 5: 提交**

---

### Task 5: 接进 Loader 与配置

**Files:**
- Modify: `internal/plugin/loader/loader.go`（`prepare` 里远程条目先取包目录）
- Modify: `internal/config/config.go`（`plugins` 段新增 `cache`、`fetch` 上限）
- Modify: `internal/cli/plugins_command.go`（装配处建 Cache 与 HTTP client）
- Test: `internal/plugin/loader/remote_test.go`（新建）、`internal/config/config_test.go`、`internal/cli/plugins_command_test.go`

**配置形状**（跟随既有 snake_case 风格）：

```json
"plugins": {
  "manifest": "./configs/plugins.json",
  "root": "./plugins",
  "cache": "./var/plugin-cache",
  "keyring": "./configs/plugin-keyring.json",
  "require_signature": true,
  "allow_insecure_sources": false,
  "fetch": { "timeout_ms": 30000, "max_bytes": 33554432 },
  "limits": { "timeout_ms": 10000, "max_memory_pages": 256, "max_instances": 4 },
  "apply_wait_ms": 60000
}
```

**接线顺序**（`prepare` 里，远程条目走这条，本地条目一字不改）：

1. `cache.Has(digest)` 命中 → 直接用缓存目录，**完全不联网**。
2. 未命中 → `Fetch`（摘要把守）→ `cache.Put`（解包 + 原子就位）→ 用缓存目录。
3. 拿到目录之后，**原样交给既有的 `LoadPackage(dir, keyring)`**——验签、sha256 校验一步不少。远程与本地在这一行之后完全同路。

**八条策略规则，逐条要有测试：**

1. **有远程条目但 `cache` 未配 → serve 装配失败**，错误说明远程来源需要缓存目录。不许临时目录兜底：缓存位置是部署决定，静默选一个地方写文件是另一种降级。
2. **拉取失败不阻断其余条目**：走既有失败通道，`Apply` 返回合并错误，`plugins status` 里该条目 `State=failed` 且 `LastError` 说明是拉取/摘要问题。
3. **缓存命中路径必须证明没有联网**：测试里给一个「一旦被请求就让测试失败」的 handler，断言命中时它没被碰过。
4. `fetch.timeout_ms` / `max_bytes` 缺省值写进 config 的字段 doc；**缺省必须是有限值**，不许 0 表示无限。
5. 既有的本地条目行为一字不变——要有测试证明。
6. **`allow_insecure_sources` 缺省 `false`**，用 `*bool` 解析后归一（与 `require_signature` 同一手法、同一方向：安全开关的缺省在安全那一侧）。缺省下遇到 `http://` 条目 → **serve 装配失败**，错误点名该条目与其 URL，并说明这是调试用途、要用必须显式开。
7. **开关为 `true` 时**：`http://` 条目允许拉取，但每一条都要在装配期打一条 **Warn**，点名条目与 URL，并说明代价（明文可被观察与阻断、可被投喂旧但合法的版本；digest 仍然保证字节完整性）。一条不打 Warn 就悄悄用明文的部署，是这条开关最容易被滥用的形态。
8. **开关只影响 scheme，不影响其它任何一道门**：`http://` 条目的 digest 仍然必填、仍然逐字节校验，签名仍然照验。要有测试证明——开着开关时把 digest 改错，该条目仍然 failed。

- [ ] **Step 1: 写失败测试**（上述八条；全部用 `httptest.Server`，`Apply` 循环轮数写字面量 ≤5）

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证（四个）**
  - 让缓存命中时也去拉一次 → 「命中不联网」测试应 FAIL
  - 跳过 `LoadPackage`、直接信任拉下来的包 → A5a 的验签测试应 FAIL（**这条最要紧**：它证明远程路径没有绕过签名这道门）
  - 把 `allow_insecure_sources` 的缺省从 `false` 改成 `true` → 「缺省拒绝 http」的测试应 FAIL
  - 让 `allow_insecure_sources: true` 顺带跳过 digest 校验 → 规则 8 的测试应 FAIL

- [ ] **Step 4: `-race`（串行）**

- [ ] **Step 5: 提交**

---

### Task 6: 端到端验收与文档回写

**Files:**
- Test: `internal/plugin/loader/e2e_test.go`（在既有验收文件里追加）
- Modify: docs 仓 `docs/design/architecture/legion-plugin-system.md`（§7 供应链一行补远程来源；§9 路线图 A5b 标交付）

**⚠️ 本 task 反复拉取、解包、挂载真 wasm 实例，两套规程逐条适用**：轮数写死 ≤5，每轮断言实例数上限，全部用 `httptest.Server`，`-timeout 120s`，`-p 1`。

- [ ] **Step 1: 远程闭环**

```text
keygen 产钥 → sign 给包签名 → 打成 tar.gz → 起 httptest.Server 提供它
  → plugins.json 写 https URL + 正确 digest → 启动期 Apply
  → 插件挂载、工具进注册表、缓存目录下出现 sha256/<digest>/ 三个文件
  → 停掉服务器（handler 改成一被请求就让测试失败）→ reload
  → 仍然挂载成功（命中缓存、完全不联网）
```

- [ ] **Step 2: 拒绝路径**

- digest 与实际字节不符 → 该条目 failed、`status` 里可见原因、**缓存目录里不留任何东西**
- 包里 `plugin.json` 被改（签名未同步）→ 拉取成功但 `LoadPackage` 验签失败 → failed。**这条证明两道门各自独立**：digest 过了不等于签名过了
- tarball 里含 `../evil` → 拒绝，缓存目录外无任何文件被创建
- `http://` 的 source 且 `allow_insecure_sources` 缺省（未写）→ 装配期就拒绝，错误点名该条目
- **同一个 `http://` 条目，`allow_insecure_sources: true` → 挂载成功**，且装配期有一条点名它的 Warn。这条是调试通道的正向验收
- 承上，开着开关但把 digest 改错 → 该条目 failed。**证明开关只放开了 scheme，没放开任何一道门**

- [ ] **Step 3: 全量回归**

```bash
go build ./... && go vet ./... && go test ./... -count=1 -timeout 300s && gofmt -l .
```

再逐包串行跑 `-race`（至少 `internal/plugin/...`、`internal/cli`）。

- [ ] **Step 4: 文档回写**

§7 供应链一行补上：远程来源为 HTTPS tarball，条目必须声明 sha256 摘要，拉取按流校验、不符不落盘；缓存内容寻址，命中不联网；解包拒绝路径穿越、非普通文件与解压炸弹；**digest 与签名是两道独立的门**。同时写明**明文 `http://` 仅在 `plugins.allow_insecure_sources: true` 下可用，缺省拒绝，仅供调试**，并写清它丢的是机密性与可用性、不丢完整性（digest 仍逐字节把守）。§9 路线图 A5b 标交付，并写明 OCI 传输仍未做。

- [ ] **Step 5: 提交并开 PR**

---

## 交付后状态

- `plugins.json` 写一个 HTTPS 地址与摘要，宿主自己拉、校验、缓存、加载
- 摘要在字节落盘前把关，签名在加载前把关，两道门互不替代
- 缓存内容寻址：命中即用、不联网，重启与离线部署不依赖网络
- 解包拒绝路径穿越、符号链接与解压炸弹，且拒绝的是整个包而不是单个条目
- 调试可用明文 `http://`：需显式 `allow_insecure_sources: true`，每条都打 Warn，且 digest 与签名两道门一道不少

**尚未包含**：OCI registry 传输、`legion plugin install|search` 与 GUI 同意流（A5c）、缓存清理与容量上限、镜像与代理配置、密钥吊销与透明日志。
