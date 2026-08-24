# A5a 插件签名与验签 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让部署方只加载**可验真**的插件：签名由信任的密钥签发才准挂载，验签失败与未签名一律拒绝并在 `plugins status` 里点名。

**Architecture:** Ed25519（标准库 `crypto/ed25519`），信任公钥写在一份 JSON keyring 里。签名是**分离式**的 `plugin.sig`，签的是 `plugin.json` 的**原始字节**——而 `plugin.json` 里已经带着 `plugin.wasm` 的 sha256 且 `LoadPackage` 会校验它，所以一个签名传递性地覆盖了清单与二进制两样。策略开关在部署侧，默认强制。

**Tech Stack:** Go 1.26.0，module `github.com/stardust/legion-agent`。**不新增任何第三方依赖**——`crypto/ed25519`、`encoding/json`、`encoding/base64` 全在标准库。

**不含**（各自独立 plan）：远程来源拉取（OCI/HTTP，A5b）、`legion plugin install` 与 GUI 授权同意流（A5c）、密钥吊销与透明日志、运行期健康度驱动的自动卸载。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：禁止 fallback、禁止静默吞错、禁止零值假装正常；错误用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装；不变量违反用 `panic`。
- 公开 API 必须有 Go doc 风格注释，以标识符名开头，且不得与代码矛盾。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿，`gofmt -l .` 输出为空。
- 涉及并发的任务额外跑 `go test -race`，**plugin / runtime / cli 包串行跑**（`-p 1`）。
- 错误路径必须有测试断言「确实返回 error」，不得只测 happy path。
- 每个 task 完成后做一次**变异验证**：把该 task 的核心机制改坏，确认测试确实 FAIL，把失败输出留在报告里，然后还原。
- 设计依据：docs 仓 `docs/design/architecture/legion-plugin-system.md` §7（安全模型的供应链一行）、§9 路线图 P3。

## ⚠️ 安全类改动的额外规矩

这是本系列第一次写密码学代码，且它的失败模式是**静默放行**而不是崩溃。除 fork-bomb 规程外另加四条：

1. **绝不自己实现密码学原语**。只调 `crypto/ed25519` 与 `crypto/rand`；不手写常数时间比较（用 `crypto/subtle` 或直接用 `ed25519.Verify` 的返回值）。
2. **私钥绝不进日志、绝不进错误信息、绝不进事件流**。任何 `%v` 一个含私钥的结构体都是缺陷。
3. **每一条「拒绝」都必须有一条测试证明它确实拒绝**——伪造签名、改一个字节、换一把不在 keyring 里的钥匙、签名文件缺失，逐条来。一个只测「合法签名能过」的套件对安全特性毫无意义。
4. **变异验证在本 plan 里是安全断言**：把验签整个删掉如果测试仍然全绿，说明这层保护根本没接上。

## ⚠️ Fork-bomb 安全规程（本仓已因此宕机两次）

`host.test.exe` 曾吃到 **170 GB 虚存**（Windows 事件 2004 + Kernel-Power 41），原因是测试把被测功能当唯一终止条件。

1. 任何循环边界必须独立于被测功能：轮数写成字面量（≤5），每轮断言实例数上限。
2. 每次 `go test` 带 `-timeout 120s`；plugin / runtime / cli 包用 `-p 1`。
3. 跑挂或内存上涨立刻杀掉、先加边界再重试。
4. 绝不把变异留在工作区，每次还原后用 `git status` 核对。

## 前置事实（已在 master `6c9923a`，直接用）

```go
// internal/plugin/manifest
func ParsePlugin(data []byte) (PluginManifest, error)
func LoadPackage(dir string) (PluginManifest, []byte, error)
// LoadPackage 读 dir/plugin.json 与 dir/plugin.wasm，并已校验
// sha256(plugin.wasm) == pm.SHA256，不符即报错（assemble.go:66-72）
type PluginManifest struct {
    Name, Version string
    ABI           int
    SHA256        string        // .wasm 的摘要，形状已校验为 64 位十六进制
    Capabilities  []string
    Limits        Limits
    Network       Network
    Filesystem    Filesystem
    Tools         []ToolDecl
    Requires      []string
}
type Deployment struct{ Plugins []Entry }

// internal/plugin/loader
func New(cfg Config) (*Loader, error)
func (l *Loader) Apply(ctx context.Context, dep manifest.Deployment, root string) error
func (l *Loader) Status() []InstanceStatus
type InstanceStatus struct {
    Name, Version, State string
    Tools, SuspendedBy   []string
    LastError            string
}
// State 取值：StateLoaded / StateFailed / StateSuspended

// internal/config —— plugins 段现有字段
// manifest（清单路径，键不存在 = 不启用插件）、root、limits、apply_wait_ms
```

**插件包的当前布局**：一个目录，`plugin.json` + `plugin.wasm`。本期新增第三个文件 `plugin.sig`。

---

### Task 1: 签名原语与 keyring

**Files:**
- Create: `internal/plugin/sign/sign.go`
- Test: `internal/plugin/sign/sign_test.go`

**Interfaces:**
- Produces:
  - `type KeyID string`
  - `type Keyring struct{ /* unexported */ }`
  - `func ParseKeyring(data []byte) (*Keyring, error)`
  - `func (k *Keyring) Verify(sig Signature, message []byte) error`
  - `func (k *Keyring) IDs() []KeyID`
  - `type Signature struct{ KeyID KeyID; Algorithm string; Value []byte }`
  - `func ParseSignature(data []byte) (Signature, error)`
  - `func Sign(priv ed25519.PrivateKey, id KeyID, message []byte) (Signature, error)`
  - `func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error)`

**这个包只做纯计算，不碰文件系统**——`ParseKeyring` / `ParseSignature` 收字节，`Verify` 收字节。读文件是 Task 2 的事。理由与 `internal/plugin/manifest` 当初把解析与装配分开一样：纯函数能被穷尽测试，掺了 I/O 就不能。

**keyring 的 JSON 形状**（写进 `ParseKeyring` 的 doc）：

```json
{
  "keys": [
    { "id": "ops-2026", "algorithm": "ed25519", "public_key": "<base64 32 字节>" }
  ]
}
```

**签名文件 `plugin.sig` 的 JSON 形状**：

```json
{ "key_id": "ops-2026", "algorithm": "ed25519", "signature": "<base64 64 字节>" }
```

**七条校验规则，逐条要有测试：**

1. 两个解析函数都用 `json.Decoder` + `DisallowUnknownFields()`（与 `manifest` 包同源的理由：写错一个键名却被静默忽略，等于保护没生效而无人知晓）。
2. `algorithm` 不是 `"ed25519"` → 报错点名实际值。**不接受空串当默认**：一个没写算法的签名文件是坏文件，不是「用默认算法」。
3. 公钥长度不是 `ed25519.PublicKeySize`（32）→ 报错。签名长度不是 `ed25519.SignatureSize`（64）→ 报错。
4. keyring 里 `id` 重复 → 报错（信任集合必须无歧义）。
5. keyring 为空（`keys: []`）→ 报错。一个空的信任集合配上「默认强制签名」等于谁都装不上，与其在挂载时逐个报错，不如在解析时就说清楚。
6. `Verify` 的 `sig.KeyID` 不在 keyring 里 → 报错点名该 id **与 keyring 里已有的 id 列表**。不要遍历所有钥匙去试：签名自己声明了用哪把，试遍所有钥匙会让「用错钥匙签的」与「被篡改的」两种失败无法区分。
7. `Verify` 通过 `ed25519.Verify` 的返回值判定，失败即报错。

- [ ] **Step 1: 写失败测试**

覆盖上面七条（每条断言**确实返回 error**），外加正向：`Sign` 出来的签名 `Verify` 得过；同一 message 换一把钥匙签则不过；message 改一个字节则不过。

- [ ] **Step 2: 实现**

私钥只出现在 `Sign` 与 `GenerateKey` 的参数/返回值里。`Keyring` 只持公钥。**任何类型都不要给私钥写 `String()`**。

- [ ] **Step 3: 变异验证（两个）**
  - 把 `Verify` 改成永远返回 nil → 「改一个字节则不过」的测试应 FAIL
  - 把未知 key id 的分支改成「遍历所有公钥挨个试」→ 未知 id 的测试应 FAIL（它现在会因为碰巧有钥匙能验过而放行）

- [ ] **Step 4: 提交**

```bash
git commit -m "feat(plugin): add ed25519 signature primitives and a trust keyring"
```

---

### Task 2: 包级验签

**Files:**
- Modify: `internal/plugin/manifest/assemble.go`（`LoadPackage` 增加验签）
- Test: `internal/plugin/manifest/assemble_test.go`
- Test fixture: `internal/plugin/manifest/testdata/`（一份 keyring、一个已签名的包、一个签名被篡改的包）

**Interfaces:**
- Consumes: Task 1 的 `sign.Keyring` / `sign.ParseSignature`
- Produces: `func LoadPackage(dir string, keyring *sign.Keyring) (PluginManifest, []byte, error)`（签名从 `dir/plugin.sig` 读）

**签什么、为什么够**（写进 `LoadPackage` 的 doc，这是本 plan 的核心论证）：

签名覆盖 `plugin.json` 的**原始字节**。`plugin.json` 里带着 `plugin.wasm` 的 sha256，而 `LoadPackage` 本来就会校验二进制确实哈希成那个值——所以改动 `.wasm` 会让 sha256 对不上，改动 sha256 会让签名对不上。**一个签名，两样都覆盖。**

对**原始字节**而非解析后的结构体签名同样是有意的：任何「重新序列化再签」的方案都要求签验两侧的 JSON 编码逐字节一致，那是一类经典的可利用歧义。

**`keyring == nil` 的语义**：表示本部署不要求签名（策略由 Task 3 决定，不在这里判断），此时跳过验签但**不跳过 sha256 校验**。这是契约显式声明的可选，写进参数 doc。

- [ ] **Step 1: 写失败测试**

覆盖（每条断言**确实返回 error**，且错误点名插件与失败原因）：
- 合法签名 → 加载成功（正向；断言返回的清单字段正确，不能只断言没报错）
- `plugin.sig` 缺失而 keyring 非 nil → 报错
- 签名被改一个字节 → 报错
- 用一把不在 keyring 里的钥匙签 → 报错点名那个 key id
- **`plugin.json` 被改一个字节（签名不动）→ 报错**。这条是整个机制的存在理由
- **`plugin.wasm` 被改（sha256 与签名都不动）→ 报错**，且错误来自 sha256 校验。这条证明「一个签名覆盖两样」的论证成立
- `keyring == nil` → 跳过验签但 sha256 校验仍然生效（改坏 .wasm 仍报错）

**夹具怎么造**：用 Task 1 的 `GenerateKey` + `Sign` 在**测试里**现场生成密钥与签名，写进 `t.TempDir()`。不要把私钥提交进仓库——即便是测试用的私钥，提交一把私钥进 git 也会训练出错误的肌肉记忆。

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证（两个）**
  - 把验签调用整个删掉 → 「plugin.json 被改」与「伪造签名」两条测试应 FAIL
  - 把 `keyring == nil` 的分支改成「也跳过 sha256 校验」→ 最后一条测试应 FAIL

- [ ] **Step 4: 提交**

---

### Task 3: 部署策略与接线

**Files:**
- Modify: `internal/config/config.go`（`plugins` 段新增 `keyring` 与 `require_signature`）
- Modify: `internal/plugin/loader/loader.go`（`Config` 增 `Keyring`，`prepare` 传给 `LoadPackage`）
- Modify: `internal/cli/plugins_command.go`（装配处读 keyring）
- Test: `internal/config/config_test.go`、`internal/plugin/loader/signature_test.go`（新建）、`internal/cli/plugins_command_test.go`

**配置形状**（跟随既有 snake_case 风格）：

```json
"plugins": {
  "manifest": "./configs/plugins.json",
  "root": "./plugins",
  "keyring": "./configs/plugin-keyring.json",
  "require_signature": true,
  "limits": { "timeout_ms": 10000, "max_memory_pages": 256, "max_instances": 4 },
  "apply_wait_ms": 60000
}
```

**四条策略规则，逐条要有测试：**

1. **`require_signature` 缺省为 `true`**。用 `*bool` 解析后归一——一个安全开关的缺省值必须是安全的那一侧。这与 `Entry.Enabled` 缺省为 true 是同一手法，但方向相反：那里缺省是「装」，这里缺省是「严」。
2. `require_signature: true` 而 `keyring` 未配 → **serve 装配失败**，错误说明要么配 keyring 要么显式关掉。绝不「要求签名却没有信任集合」地跑起来。
3. `require_signature: false` → `LoadPackage` 收到 nil keyring，跳过验签。这条必须有测试，否则「显式关掉」这条路径无人守门。
4. 验签失败的插件走既有的失败通道：不阻断其余条目、`Apply` 返回合并错误、`plugins status` 里 `State=failed` 且 `LastError` 说明是签名问题。**不新增状态**——验签失败就是激活失败的一种。

- [ ] **Step 1: 写失败测试**

覆盖上面四条，外加：keyring 路径配了但文件读不到 → 装配失败（与 `manifest` 路径同源的「配了就是要用」）。

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证**：把 `require_signature` 的缺省从 true 改成 false → 缺省强制的测试应 FAIL。

- [ ] **Step 4: 提交**

---

### Task 4: `agent plugins keygen|sign`

**Files:**
- Modify: `internal/cli/plugins_command.go`（两个子命令）
- Test: `internal/cli/plugins_command_test.go`

**为什么在本期而不是留给 A5c 的 CLI**：一个只能验签、却没有任何办法产出签名的交付物，在真机上是无法使用的——部署方拿不到可签名的工具，就只能把 `require_signature` 关掉，于是这一整期等于没做。`install` 那类命令留给 A5c，签名与产钥这两条是验签的对偶，必须同期。

**`agent plugins keygen`**：生成 Ed25519 密钥对。
- 私钥写文件，**权限 0600**，文件已存在时**拒绝覆盖**并报错（覆盖一把私钥是不可逆的破坏）。
- 公钥以 keyring 条目的形状打印到 stdout，方便直接粘进 keyring。
- **私钥内容绝不打印到 stdout/stderr、绝不进日志**。

**`agent plugins sign <包目录>`**：用私钥对 `<包目录>/plugin.json` 的原始字节签名，写出 `<包目录>/plugin.sig`。
- 私钥路径经 flag 传入。
- 覆盖已有的 `plugin.sig` 是允许的（重新签名是正常操作），但要在输出里说明覆盖了。
- 签完**立刻自验一次**（用刚才的公钥）再落盘：一个产不出可验签名的签名工具，比不签更糟。

- [ ] **Step 1: 写失败测试**

- `keygen` 在目标文件已存在时报错且**不改动原文件**（断言原内容一字未变）
- `keygen` 产出的私钥文件权限是 0600（Windows 上该断言可能不适用——若如此，在测试里明确 skip 并写明原因，不要静默放过）
- `keygen` 的输出里**不含私钥的任何编码形式**（拿产出的私钥字节做子串检索）
- `sign` 产出的 `plugin.sig` 能被 Task 1 的 `Verify` 验过
- `sign` 指向一个没有 `plugin.json` 的目录 → 报错
- `sign` 用一个损坏的私钥文件 → 报错

- [ ] **Step 2: 实现**

- [ ] **Step 3: 变异验证**：去掉 `keygen` 的「拒绝覆盖」→ 该测试应 FAIL。

- [ ] **Step 4: 提交**

---

### Task 5: 端到端验收与文档回写

**Files:**
- Test: `internal/plugin/loader/e2e_test.go`（在既有验收文件里追加）
- Modify: docs 仓 `docs/design/architecture/legion-plugin-system.md`（§7 供应链一行；§9 路线图 P3 拆成 A5a/A5b/A5c 并标 A5a 交付）

**⚠️ 本 task 会反复挂载真 wasm 实例，Fork-bomb 规程逐条适用**：轮数写死 ≤5，每轮断言实例数上限，`-timeout 120s`，`-p 1`。

- [ ] **Step 1: 签名闭环**

```text
keygen 产钥 → 写 keyring → sign 给包签名 → plugins.json 指向它
  → 启动期 Apply → 插件挂载、工具进注册表
  → 篡改 plugin.wasm 一个字节 → reload → 该插件 failed，LastError 说明摘要不符，其余插件不受影响
  → 恢复 .wasm、改 plugin.json 一个字节 → reload → failed，LastError 说明签名不符
  → 换一把不在 keyring 里的钥匙重签 → reload → failed，LastError 点名那个 key id
```

- [ ] **Step 2: 策略路径**

- `require_signature: true` 而包没有 `plugin.sig` → 拒绝加载，`status` 可见原因
- `require_signature: false` → 同一个未签名的包能挂载（证明开关确实是开关）
- `require_signature: true` 而 `keyring` 未配 → serve 装配失败

- [ ] **Step 3: 全量回归**

```bash
go build ./... && go vet ./... && go test ./... -count=1 -timeout 300s && gofmt -l .
```

再逐包串行跑 `-race`（至少 `internal/plugin/...`、`internal/cli`）。

- [ ] **Step 4: 文档回写**

§7 的供应链一行从「v1 只加载本地清单指定的 `.wasm`，记录 sha256；签名与远程分发列为后续议题」改成实际状态：Ed25519 分离签名覆盖 `plugin.json`，经其中的 sha256 传递性覆盖 `.wasm`；信任集合是本地 keyring；默认强制。§9 把 P3 拆成 A5a（本期）/ A5b（远程来源）/ A5c（`legion plugin` CLI 与 GUI 同意流），A5a 标交付。

- [ ] **Step 5: 提交并开 PR**

---

## 交付后状态

- 插件必须由信任密钥签发才准挂载，默认强制，显式才能关
- 一个签名同时锁住清单与二进制：改任一样都会被拒
- 验签失败落在既有的失败通道上，`plugins status` 里可见原因
- 部署方有 `keygen` 与 `sign` 两条命令，能真正把这套用起来

**尚未包含**：远程来源拉取（OCI/HTTP，A5b）、`legion plugin install` 与 GUI 同意流（A5c）、密钥吊销与透明日志、密钥轮换流程、运行期健康度驱动的自动卸载。
