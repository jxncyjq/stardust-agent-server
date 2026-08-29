# 插件签名密钥吊销 实施计划（G7）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 一把泄漏的私钥可以被作废——它签过的包从此验不过，而且**运维改完立刻知道要重启 serve 才生效**，不会以为改了就算数。

**Architecture:** 三段。①`sign` 包的 keyring 增加 `revoked` 列表，吊销**压过** `keys`，并给出专属哨兵 `ErrRevokedKey`；②`manifest` 把它归入 `ErrUntrustedPackage`——被吊销钥匙签的包就是不可信的包，于是 G6 的自动清缓存**顺带生效**；③吊销集进 `SignaturePolicy`，从而复用既有的 reload 守卫：信任集变了就拒绝在旧策略下收敛，并让运维去重启。

**Tech Stack:** Go 1.26.0，标准库（`crypto/ed25519` 已在用）。不引依赖。

**上游依据:** 路线 `plans/2026-08-28-plugin-gap-closure-roadmap.md` 的 G7。

## 前置事实（已在 master）

```go
// internal/plugin/sign —— sign.go
type Keyring struct{ keys map[KeyID]ed25519.PublicKey }              // :69
type rawKeyring struct{ Keys []rawKeyEntry `json:"keys"` }           // :86，DisallowUnknownFields
func ParseKeyring(data []byte) (*Keyring, error)                      // 拒绝空 keys、重复 id
func (k *Keyring) IDs() []KeyID                                       // :169，已排序
func (k *Keyring) Verify(sig Signature, message []byte) error         // :210

// internal/plugin/manifest —— assemble.go
var ErrUntrustedPackage = errors.New("plugin package is not trusted") // :132
// 三条路径挂它：签名缺失、不可解析、验不过；读 plugin.sig 的 I/O 错误刻意不挂

// internal/plugin/loader —— loader.go
type SignaturePolicy struct{ Enforced bool; KeyIDs []sign.KeyID }     // :575
func SignaturePolicyOf(keyring *sign.Keyring) SignaturePolicy         // :595

// internal/cli —— plugins_command.go:779
// reload 比较「配置里的策略」与「本进程在跑的策略」，不等就拒绝收敛并要求重启 serve
```

## 关键设计：吊销靠既有的「信任集冻结 + reload 守卫」生效，不另造机制

信任集在 serve 装配时冻结，运行中换不了（既有设计）。`reload` 已经会把配置里的签名策略与在跑的策略比对，不等就**拒绝收敛**并让运维重启。

把吊销集放进 `SignaturePolicy`，这条既有守卫就自动覆盖了吊销：运维在 keyring 里加一条吊销 → `reload` 拒绝并说「重启 serve」→ 重启后新信任集生效 → 那把钥匙签的包在收敛时验不过、按名字失败。

**不做在线重新验证**（运行中重新读 keyring、逐个重验已挂载的插件）。那要在运行期换掉冻结的信任集，与既有不变量正面冲突；而它买到的只是「不必重启」，代价是一个横跨所有已挂载插件的新可变状态。**代价与收益不成比例。**

**必须诚实写进文档的后果**：吊销**不是即时**的，它在下一次 serve 重启时生效；`reload` 会拒绝并说明这一点，所以运维不会误以为改完就算数——这正是这条守卫存在的价值。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：吊销相关的任何歧义一律拒绝解析，不猜。
- 公开标识符必须有 Go doc 注释。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空。
- **每个 task 至少跑一次 `go test ./...`**。
- 每个 task 做变异验证：把核心机制改坏，确认测试确实 FAIL，输出留在报告里，然后还原并 `git status` 核对。
- **向后兼容**：没有 `revoked` 字段的 keyring（今天所有的）行为一字不变，且要有测试钉住。
- 提交只 stage 本 task 的文件（显式路径），永不 `git add -A`。

---

### Task 1: keyring 接受 `revoked`，吊销压过信任

**Files:**
- Modify: `internal/plugin/sign/sign.go`
- Test: `internal/plugin/sign/revocation_test.go`

**Interfaces:**
- Produces:
  - `var sign.ErrRevokedKey error`
  - `func (k *Keyring) RevokedIDs() []KeyID`（排序）
  - keyring 文档新增 `"revoked": [{"key_id": "...", "revoked_at": "...", "reason": "..."}]`

**语义（写死在测试里）**：

| 情形 | 行为 |
|---|---|
| `revoked` 里的 id 同时在 `keys` 里 | **吊销赢**：`Verify` 返回 `ErrRevokedKey`，错误里带吊销时间与理由（若有）。**保留公钥是刻意的**：它让错误能说「这把钥匙曾经可信、现在不可信」，比「未知的 key id」有用得多 |
| `revoked` 里的 id 不在 `keys` 里 | 合法：一把早已从 keys 里删掉的钥匙仍然可以被记录为吊销 |
| 同一个 id 在 `revoked` 里出现两次 | **拒绝解析**：两条记录哪条算数？没有答案就不要猜 |
| `key_id` 为空 | 拒绝解析 |
| `keys` 里每一把都被吊销 | **拒绝解析**：等价于空信任集（`ParseKeyring` 本来就拒绝空 keys），配上强制验签就是「拒绝一切插件」，在解析期说出来比在每次挂载时说要好 |
| 没有 `revoked` 字段 | 与今天完全一致 |

`revoked_at` 与 `reason` 是可选的、只用于错误信息；`revoked_at` 若出现必须是 RFC 3339，**格式错就拒绝解析**（一个解析不了的时间戳会原样出现在错误里误导人）。

- [ ] **Step 1: 写失败测试**

新建 `internal/plugin/sign/revocation_test.go`，夹具沿用 `sign_test.go` 现有的写法（**先读**，那里已有造密钥对与 keyring JSON 的助手）：

```go
func TestParseKeyringAcceptsARevocationList(t *testing.T) { … }
func TestVerifyRefusesARevokedKeyThatIsStillListed(t *testing.T) {
	// 同一个 id 既在 keys 又在 revoked：签名本身是对的，仍须拒绝，
	// 且 errors.Is(err, ErrRevokedKey) 为真，错误里出现吊销时间。
}
func TestVerifyStillAcceptsAKeyThatWasNotRevoked(t *testing.T) { … }
func TestParseKeyringRefusesADuplicateRevocation(t *testing.T) { … }
func TestParseKeyringRefusesAnEmptyRevokedKeyID(t *testing.T) { … }
func TestParseKeyringRefusesWhenEveryKeyIsRevoked(t *testing.T) {
	// 等价于空信任集。
}
func TestParseKeyringRefusesAMalformedRevokedAt(t *testing.T) { … }
func TestParseKeyringWithoutARevokedListIsUnchanged(t *testing.T) {
	// 向后兼容。
}
func TestRevokedIDsAreSorted(t *testing.T) { … }
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/plugin/sign/ -run "Revok" -v`
Expected: FAIL，`unknown field "revoked"`

- [ ] **Step 3: 实现**

`rawKeyring` 加 `Revoked []rawRevocation`；`Keyring` 加 `revoked map[KeyID]Revocation`；`Verify` 在查到公钥**之前**先查吊销表（顺序重要：一把被吊销的钥匙即使还留着公钥，也不该走到签名比对那一步）。

- [ ] **Step 4-6: 绿 → 全量 → 变异 → 提交**

变异：把 `Verify` 里的吊销检查挪到公钥查找**之后**且只在查不到时才做，确认
`TestVerifyRefusesARevokedKeyThatIsStillListed` FAIL；还原。

```bash
git add internal/plugin/sign/sign.go internal/plugin/sign/revocation_test.go
git commit -m "feat(plugin): keyring 支持密钥吊销，吊销压过信任"
```

---

### Task 2: 被吊销钥匙签的包 = 不可信的包

**Files:**
- Modify: `internal/plugin/manifest/assemble.go`（`verifyManifestSignature` 的错误归类）
- Test: `internal/plugin/manifest/assemble_test.go`

**Interfaces:**
- Consumes: `sign.ErrRevokedKey`（Task 1）

`Verify` 返回吊销错误时，必须与「验不过」走同一条归类：包上 `ErrUntrustedPackage`。这不是顺手：

- HTTP 侧的 422 与 GUI 的「不给重试」都挂在这个哨兵上——重试一把被吊销的钥匙，一万次也还是被吊销；
- G6 的**自动清缓存**也挂在它上面，于是被吊销钥匙签的包会立刻被清出缓存，而这正是吊销想要的效果。

- [ ] **Step 1: 写失败测试**

```go
func TestLoadPackageTreatsARevokedKeyAsUntrusted(t *testing.T) {
	// 用一把随后被吊销的钥匙签包 → LoadPackage 失败 →
	// errors.Is(err, manifest.ErrUntrustedPackage) 为真，
	// errors.Is(err, sign.ErrRevokedKey) 也为真（分类没被吃掉），
	// 且错误文本里出现 key id。
}
```

- [ ] **Step 2-6: 红 → 实现 → 绿 → 全量 → 变异 → 提交**

变异：把吊销错误改成不包 `ErrUntrustedPackage`，确认新测试 FAIL；还原。

```bash
git add internal/plugin/manifest/assemble.go internal/plugin/manifest/assemble_test.go
git commit -m "feat(plugin): 被吊销钥匙签的包归入不可信"
```

---

### Task 3: 吊销集进 SignaturePolicy，reload 据此拒绝

**Files:**
- Modify: `internal/plugin/loader/loader.go`（`SignaturePolicy`、`SignaturePolicyOf`、`Equal`、`String`）
- Test: `internal/plugin/loader/loader_test.go`（或既有 signature policy 测试文件）
- Test: `internal/cli/plugins_command_test.go`（reload 拒绝）

**Interfaces:**
- Produces: `SignaturePolicy.RevokedIDs []sign.KeyID`

没有这一步，整件事就是**假的**：运维在 keyring 里加一条吊销、跑 `reload`，`KeyIDs` 没变所以守卫放行，而进程里跑的还是旧信任集——那把钥匙签的包继续挂载，屏幕上却显示 reload 成功。

有了这一步，既有守卫直接覆盖：策略不等 → 拒绝收敛 → 明确要求重启 serve。

- [ ] **Step 1: 写失败测试**

```go
func TestSignaturePolicyOfCarriesRevokedIDs(t *testing.T) { … }
func TestSignaturePolicyEqualDistinguishesARevocation(t *testing.T) {
	// 同样的 KeyIDs、不同的 RevokedIDs 必须不相等——这正是守卫要看见的差别。
}
func TestSignaturePolicyStringNamesRevocations(t *testing.T) {
	// 错误信息要说清差在哪，否则运维只看到"策略不同"却不知道差什么。
}
func TestPluginsReloadRefusesAfterAKeyIsRevoked(t *testing.T) {
	// CLI 侧：装配后往 keyring 加一条吊销 → reload 报错，
	// 文本里出现被吊销的 key id 与"restart serve"。
}
```

- [ ] **Step 2-6: 红 → 实现 → 绿 → 全量 → 变异 → 提交**

变异：把 `Equal` 里的吊销比较去掉，确认
`TestSignaturePolicyEqualDistinguishesARevocation` 与 CLI 那条 FAIL；还原。

```bash
git add internal/plugin/loader/loader.go internal/plugin/loader/*_test.go internal/cli/plugins_command_test.go
git commit -m "feat(plugin): 吊销进签名策略，reload 据此拒绝在旧信任集下收敛"
```

---

### Task 4: 文档

**Files:**
- Modify: docs 仓 `agents/reference/reference-legion-agent-plugins-001.md`（§5.2 信任集、§9 排错）
- Modify: docs 仓 `design/architecture/legion-plugin-system.md`（路线表 G7）

- [ ] **Step 1: 手册 §5.2**

写 keyring 的 `revoked` 形状与三条语义（吊销压过 keys、保留公钥是为了给出更好的错误、全部吊销=拒绝解析），以及**吊销何时生效**：不是即时，下次 serve 重启生效；`reload` 会拒绝并说明——这句必须显眼，否则运维会以为改完就算数。

- [ ] **Step 2: §9 排错**

加两行：`signed by a revoked key` 怎么办（换钥匙重签，重试没用；那个包会被自动清出缓存）；`reload` 报「策略不同」时怎么办（重启 serve）。

- [ ] **Step 3: 路线表 G7 标记已交付**，并写明**不做**在线吊销查询与透明日志、以及「吊销在重启时生效」这条取舍。

---

## 自检

**范围覆盖**：G7 的三件事——keyring 的 `revoked` 列表（Task 1）、装配期与 reload 期都校验（Task 1 的 Verify + Task 3 的策略守卫）、被吊销钥匙签的插件在下一次收敛时失败并点名（Task 2 的归类 + 重启后的收敛）——各有任务；文档在 Task 4。

**类型一致性**：`ErrRevokedKey` 在 Task 1 定义、Task 2 判定；`RevokedIDs()` 在 Task 1 定义、Task 3 消费；`SignaturePolicy.RevokedIDs` 在 Task 3 内自洽。

**刻意不做**：在线吊销查询（OCSP 式）、透明日志、运行期热换信任集（理由见「关键设计」）。
