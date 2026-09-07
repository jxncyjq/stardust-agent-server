# 插件分级安装体验（S2）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让插件加载路径消费 S1 交付的信任清单——已登记的显示开发者名直接装，未签名的要操作者显式认下，已撤销的硬拒且任何 flag 都救不了。

**Architecture:** `manifest.LoadPackage` 从「过 / 不过」升成返回一个**来源判定**（`Provenance`），只报告不裁决；策略由调用方执行。信任集是**本地 keyring ∪ 联网清单**的并集，且 `loader` 改成**每次挂载动态读**，这样后台刷到的撤销无需重启就生效。安装期的确认绑定到 `plugin.json` 的字节摘要，记进 `plugins.json`。

**Tech Stack:** Go 1.26.0，module `github.com/stardust/legion-agent`。既有包 `internal/plugin/{manifest,sign,loader,trustlist}`、`internal/cli`、`internal/server`。

**Spec:** `docs/superpowers/specs/2026-09-06-plugin-graded-install-design.md`。有歧义时以 spec 为准，spec 没写的以本计划为准。

## Global Constraints

- **fail-loud 铁律**：不许回落零值、不许吞错误、不许「拿不到就当没配置」。错误用 `fmt.Errorf("...: %w", err)` 包装，**错误点必须可定位**。
- **注释是契约**：注释里每条事实陈述必须与代码一致，且**不得出现关于「谁调用它 / 有没有调用方 / 某个东西落没落地」的句子**。S1 在这上面被评审抓到过**七条以上** finding。凡是提到别的文件/常量/包行为的句子，**亲自去那个文件确认再写**。
- **不得引入包级可变量**。S1 里两个任务据此拒绝过方案。
- `internal/plugin/sign` **一行不改**。
- `plugin.json` 的 `sha256` 与 `plugin.wasm` 的逐字节比对**一行不改**。
- 三个状态的确切名字：`ProvenanceRegistered` / `ProvenanceUnsigned` / `ProvenanceRevoked`。
- **撤销压过一切**：任何 flag、任何 `AcceptedUnsigned`、`require_signature: false` 都绕不过 `ProvenanceRevoked`。
- **先合并、后判定**：绝不允许「先用本地 keyring 判一次、拿不到再用清单判一次」。
- 装配合并信任集必须走 `sign.ParseKeyring`，**不得在 `sign.Keyring` 外面自建第二套判断**。
- `gofmt -l .` 为空，`go vet ./...` 干净。
- 全绿判据：`go test ./... -count=1 -p 1 -timeout 900s`；改到 `trustlist` 的任务另跑 `-race -count=5` 与 `-cpu=1 -count=10`。
- 只按显式路径 `git add`，**不用 `git add -A`**。
- 提交信息正文中文，结尾 `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`。

## 本期最该防的那个形状

S1 里「**接缝在，但没人测那条接缝**」复发了**六次**，每一次都是靠**变异实证**跑出来的，静态审查一次都没抓住。最狠的几条：把 `VerifyDocument` 换成 `ParseDocument` 整包全绿；`sign` 报告成功但磁盘上没有 `.sig` 全绿；serve 里那行 `go runTrustlistRefreshLoop(...)` 整行删掉全绿。

**每个任务都必须做变异验证**：把它要守的规则在实现里失效掉（**不能只造成编译失败**），确认对应用例 FAIL，再改回来确认 PASS。**哪条变异之后测试仍然全绿，就说明那条规则没人守着，必须补测试。**

本期尤其要盯这三条接线：
1. `LoadPackage` 返回的三态**真的被 `loader.prepare` 消费了**（不是算出来就扔）
2. `TrustSet` 提供者**真的在每次挂载时被调用**（不是只调一次然后缓存）
3. `install` **真的把摘要写进了 `plugins.json`**

---

## File Structure

| 文件 | 职责 |
|------|------|
| `internal/plugin/manifest/provenance.go`（新） | `ProvenanceState` / `Provenance` / `TrustInput`、`assessProvenance`、两个哨兵 |
| `internal/plugin/manifest/assemble.go`（改） | `LoadPackage` 签名与三态接线；删掉 `verifyManifestSignature` |
| `internal/plugin/manifest/manifest.go`（改） | `Entry.AcceptedUnsigned` + `rawEntry` JSON |
| `internal/plugin/trustlist/merge.go`（新） | `Merge`：本地 keyring ∪ 清单，登记与撤销都取并集 |
| `internal/plugin/loader/loader.go`（改） | `Config.TrustSet` 提供者、`prepare` 消费三态与策略、`SignaturePolicy` 语义收窄 |
| `internal/cli/plugins_command.go`（改） | `install --accept-unsigned`；装配 `TrustSet` 闭包 |
| `internal/cli/plugin_consent_service.go`（改） | 三条路填 `PluginView` 的信任字段 |
| `internal/server/plugins.go`（改） | `PluginView` 增加三个字段 |

---

## Task 1: `manifest` 的来源判定（三态）

**Files:**
- Create: `internal/plugin/manifest/provenance.go`
- Create: `internal/plugin/manifest/provenance_test.go`
- Modify: `internal/plugin/manifest/assemble.go`（`LoadPackage` 签名与调用；删除 `verifyManifestSignature`）

**Interfaces:**
- Consumes: `internal/plugin/sign` 的 `Keyring`（`IDs()` / `Revoked(id)` / `Verify(sig, msg)`）、`KeyID`、`ParseSignature`、`ErrRevokedKey`
- Produces:
  - `type ProvenanceState int`，常量 `ProvenanceUnsigned`（零值）/ `ProvenanceRegistered` / `ProvenanceRevoked`，`func (s ProvenanceState) String() string`
  - `type Provenance struct { State ProvenanceState; KeyID sign.KeyID; Publisher string; Reason string; RevokedAt time.Time }`
  - `type TrustInput struct { Keyring *sign.Keyring; Publishers map[sign.KeyID]string }`
  - `func LoadPackage(dir string, trust TrustInput) (PluginManifest, []byte, Provenance, error)`
  - `var ErrUnsignedNotAccepted = errors.New("plugin package is unsigned and was never accepted")`
  - `var ErrRevokedPublisher = errors.New("plugin package is signed by a revoked key")`

**为什么 `Publishers` 是 `map[sign.KeyID]string` 而不是 `trustlist.Publisher`**：`manifest` 因此完全不 import `trustlist`。它只需要一个显示名，不需要那个包的类型；引进来只会让两个包互相知道对方存在，而这条依赖没有任何人需要。

- [ ] **Step 1: 写失败的测试**

创建 `internal/plugin/manifest/provenance_test.go`：

```go
package manifest

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// signedPackage 在 t.TempDir() 里造一个完整的插件包，并按需签名。
//
// 返回包目录、以及签它用的那把钥匙的 id。signWith 为 nil 时不写 plugin.sig
// ——那正是「未签名」这一态要考的输入。
func signedPackage(t *testing.T, signWith ed25519.PrivateKey, keyID sign.KeyID) string {
	t.Helper()
	dir := t.TempDir()
	wasm := []byte("\x00asm\x01\x00\x00\x00")
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), wasm, 0o600); err != nil {
		t.Fatalf("write plugin.wasm: %v", err)
	}
	pm := []byte(`{"name":"probe","version":"1.0.0","sha256":"` + sha256Hex(wasm) + `",` +
		`"capabilities":[],"tools":[{"name":"t","description":"d","input_schema":{"type":"object"}}]}`)
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), pm, 0o600); err != nil {
		t.Fatalf("write plugin.json: %v", err)
	}
	if signWith != nil {
		sig, err := sign.Sign(signWith, keyID, pm)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		data, err := sign.MarshalSignature(sig)
		if err != nil {
			t.Fatalf("MarshalSignature: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "plugin.sig"), data, 0o600); err != nil {
			t.Fatalf("write plugin.sig: %v", err)
		}
	}
	return dir
}

// keyringWith 造一个信任集：ids 里的每一把都在 keys 里，revoke 里的每一把
// **同时**留在 keys 里并进 revoked——sign 包刻意保留被撤销的公钥，正是为了
// 让拒绝理由能说出「这把钥匙曾被信任」，测试不能把这条前提改掉。
func keyringWith(t *testing.T, keys map[sign.KeyID]ed25519.PublicKey, revoke []sign.KeyID) *sign.Keyring {
	t.Helper()
	entries := make([]string, 0, len(keys))
	for id, pub := range keys {
		b, err := sign.MarshalKeyEntry(id, pub)
		if err != nil {
			t.Fatalf("MarshalKeyEntry: %v", err)
		}
		entries = append(entries, string(b))
	}
	doc := `{"keys":[` + join(entries, ",") + `]`
	if len(revoke) > 0 {
		rs := make([]string, 0, len(revoke))
		for _, id := range revoke {
			rs = append(rs, `{"key_id":"`+string(id)+`","revoked_at":"2026-08-29T10:00:00Z","reason":"私钥泄漏"}`)
		}
		doc += `,"revoked":[` + join(rs, ",") + `]`
	}
	doc += `}`
	kr, err := sign.ParseKeyring([]byte(doc))
	if err != nil {
		t.Fatalf("ParseKeyring(%s): %v", doc, err)
	}
	return kr
}

func TestProvenanceRegistered(t *testing.T) {
	pub, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := signedPackage(t, priv, "dev-abc")
	kr := keyringWith(t, map[sign.KeyID]ed25519.PublicKey{"dev-abc": pub}, nil)

	_, _, prov, err := LoadPackage(dir, TrustInput{
		Keyring:    kr,
		Publishers: map[sign.KeyID]string{"dev-abc": "张三"},
	})
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if prov.State != ProvenanceRegistered {
		t.Errorf("State = %v, want ProvenanceRegistered", prov.State)
	}
	if prov.KeyID != "dev-abc" {
		t.Errorf("KeyID = %q, want dev-abc", prov.KeyID)
	}
	if prov.Publisher != "张三" {
		t.Errorf("Publisher = %q, want 张三", prov.Publisher)
	}
}

// TestProvenanceUnsignedWhenThereIsNoSignature：没有 plugin.sig 不是错误，
// 是一种**判定**。这与改动前的行为相反（那时非 nil keyring + 缺签名 = error），
// 而这正是三态存在的理由：放不放行是调用方的策略，不是这里的。
func TestProvenanceUnsignedWhenThereIsNoSignature(t *testing.T) {
	pub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := signedPackage(t, nil, "")
	kr := keyringWith(t, map[sign.KeyID]ed25519.PublicKey{"dev-abc": pub}, nil)

	_, _, prov, err := LoadPackage(dir, TrustInput{Keyring: kr})
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if prov.State != ProvenanceUnsigned {
		t.Errorf("State = %v, want ProvenanceUnsigned", prov.State)
	}
	if prov.KeyID != "" {
		t.Errorf("KeyID = %q, want 空——没有签名就没有 key 可报", prov.KeyID)
	}
}

// TestProvenanceUnsignedWhenTheKeyIsUnknown：签了，但签它的 key 这台机器不认识。
// 对用户的意义与「压根没签」完全相同：没有任何已登记的开发者为这份字节背书。
//
// KeyID 必须留空：把一把这台机器不认识的 key id 显示出来，只会让操作者以为它有意义。
func TestProvenanceUnsignedWhenTheKeyIsUnknown(t *testing.T) {
	known, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	_, stranger, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := signedPackage(t, stranger, "dev-stranger")
	kr := keyringWith(t, map[sign.KeyID]ed25519.PublicKey{"dev-abc": known}, nil)

	_, _, prov, err := LoadPackage(dir, TrustInput{Keyring: kr})
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if prov.State != ProvenanceUnsigned {
		t.Errorf("State = %v, want ProvenanceUnsigned", prov.State)
	}
	if prov.KeyID != "" {
		t.Errorf("KeyID = %q, want 空", prov.KeyID)
	}
}

// TestProvenanceRevoked：撤销的判定必须带上当初写下的时间与理由——
// sign.Keyring 保留它们正是为了让拒绝能说明自己，丢掉就退化成「未知钥匙」。
func TestProvenanceRevoked(t *testing.T) {
	pub, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := signedPackage(t, priv, "dev-gone")
	kr := keyringWith(t,
		map[sign.KeyID]ed25519.PublicKey{"dev-gone": pub, "dev-live": pub},
		[]sign.KeyID{"dev-gone"})

	_, _, prov, err := LoadPackage(dir, TrustInput{Keyring: kr})
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if prov.State != ProvenanceRevoked {
		t.Fatalf("State = %v, want ProvenanceRevoked", prov.State)
	}
	if prov.KeyID != "dev-gone" {
		t.Errorf("KeyID = %q, want dev-gone", prov.KeyID)
	}
	if prov.Reason != "私钥泄漏" {
		t.Errorf("Reason = %q, want 私钥泄漏", prov.Reason)
	}
	if prov.RevokedAt.IsZero() {
		t.Error("RevokedAt 是零值——撤销时间丢了")
	}
}

// TestATamperedSignatureIsAnErrorNotAState：签名对不上**不是**一种来源判定，
// 是完整性失败。三态说的是「谁为它背书」，篡改说的是「它被改过」，两件事不能混：
// 前者是策略，后者不是——把篡改说成 Unsigned，就等于让一个 --accept-unsigned
// 放行一份被改过的包。
func TestATamperedSignatureIsAnErrorNotAState(t *testing.T) {
	pub, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := signedPackage(t, priv, "dev-abc")
	// 改一个字节：签名覆盖的是 plugin.json 的确切字节。
	pmPath := filepath.Join(dir, "plugin.json")
	data, err := os.ReadFile(pmPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	data = []byte(string(data[:len(data)-1]) + " }")
	if err := os.WriteFile(pmPath, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	kr := keyringWith(t, map[sign.KeyID]ed25519.PublicKey{"dev-abc": pub}, nil)

	_, _, _, err = LoadPackage(dir, TrustInput{Keyring: kr})
	if err == nil {
		t.Fatal("被篡改的包没有报错")
	}
	if !errors.Is(err, ErrUntrustedPackage) {
		t.Errorf("错误没裹 ErrUntrustedPackage：%v", err)
	}
}

// TestNoTrustSetMeansUnsignedNotUnchecked：没有任何信任集时，一切都是 Unsigned，
// **不是**「不验签所以都放行」。判定与放行是两件事。
func TestNoTrustSetMeansUnsignedNotUnchecked(t *testing.T) {
	_, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := signedPackage(t, priv, "dev-abc")

	_, _, prov, err := LoadPackage(dir, TrustInput{})
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if prov.State != ProvenanceUnsigned {
		t.Errorf("State = %v, want ProvenanceUnsigned", prov.State)
	}
}
```

再在同文件末尾加两个小工具（`sha256Hex` 与 `join`），避免引入新依赖：

```go
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func join(parts []string, sep string) string { return strings.Join(parts, sep) }
```

（对应 import：`crypto/sha256`、`encoding/hex`、`strings`。）

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/plugin/manifest/ -run TestProvenance -count=1 -timeout 120s
```

预期：编译失败，`undefined: ProvenanceRegistered` 等。

- [ ] **Step 3: 实现 `provenance.go`**

创建 `internal/plugin/manifest/provenance.go`：

```go
package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// ErrUnsignedNotAccepted marks a package that carries no endorsement from any
// registered publisher and that nobody has accepted at install time.
//
// It is a sentinel because it is the one trust outcome an operator can undo:
// re-running the install with the flag that records an acceptance makes the
// same bytes loadable. Callers that offer that remedy have to be able to tell
// this case from ErrRevokedPublisher, which no flag can undo.
var ErrUnsignedNotAccepted = errors.New("plugin package is unsigned and was never accepted")

// ErrRevokedPublisher marks a package signed by a key the deployment has
// withdrawn trust from.
//
// Nothing overrides it: not an install-time acceptance, not a flag, not a
// deployment that has declared it does not require signatures. Those all say
// "I do not demand an endorsement"; a revocation says "this key was trusted
// and is not any more", which is a decision already made rather than a
// requirement not yet imposed.
var ErrRevokedPublisher = errors.New("plugin package is signed by a revoked key")

// ProvenanceState is what a package's signature says about who stands behind
// its bytes, on this machine, right now.
//
// The name is deliberately not Trust: internal/plugin/trustlist already has a
// Trust, and it means something else entirely (how fresh the fetched list is).
// Two different concepts under one name in the same import block is the shape
// that gets misread.
type ProvenanceState int

const (
	// ProvenanceUnsigned covers both "there is no plugin.sig" and "there is
	// one, but the key that made it is not in this machine's trust set".
	//
	// They are one state because they mean the same thing to whoever has to
	// decide: no registered publisher stands behind these bytes. Splitting
	// them would add a row to every policy table without adding a decision.
	ProvenanceUnsigned ProvenanceState = iota
	ProvenanceRegistered
	ProvenanceRevoked
)

func (s ProvenanceState) String() string {
	switch s {
	case ProvenanceUnsigned:
		return "unsigned"
	case ProvenanceRegistered:
		return "registered"
	case ProvenanceRevoked:
		return "revoked"
	default:
		return fmt.Sprintf("ProvenanceState(%d)", int(s))
	}
}

// Provenance is LoadPackage's verdict about one package's origin. It reports;
// it does not decide. Whether an unsigned package may load is the caller's
// policy, the same boundary LoadPackage has always drawn around its keyring
// argument.
type Provenance struct {
	State ProvenanceState

	// KeyID names the key that signed the package, and is set only when this
	// machine recognises that key — that is, for Registered and Revoked.
	//
	// It stays empty for Unsigned even when a plugin.sig was present, because
	// the id in that file names a key this machine knows nothing about;
	// showing it would invite the reader to treat it as meaningful.
	KeyID sign.KeyID

	// Publisher is the display name the trust list records for KeyID. It is
	// empty when the trust set carries no name for that key.
	Publisher string

	// Reason and RevokedAt are what the operator wrote down when the key was
	// revoked, carried so that a refusal can explain itself. Both are optional
	// in a revocation record, so both may be zero even for Revoked.
	Reason    string
	RevokedAt time.Time
}

// TrustInput is everything LoadPackage needs to judge a package's provenance:
// the merged trust set, and the display names that go with the keys in it.
//
// A zero TrustInput (no keyring) is a deployment with no trust set at all.
// That makes every package Unsigned — NOT unchecked. The difference matters:
// "unchecked" would be a verdict of "fine", and this type has no way to say
// that.
type TrustInput struct {
	Keyring *sign.Keyring

	// Publishers maps a key id to the display name a human reads. It is a
	// plain string map rather than a trustlist type so that this package need
	// not know internal/plugin/trustlist exists; a display name is all it uses.
	Publishers map[sign.KeyID]string
}

// assessProvenance reads dir/plugin.sig, if any, and judges what it says about
// manifestData's origin.
//
// It returns an error only for failures that are NOT verdicts:
//
//   - plugin.sig exists but cannot be read (a permission fault, or it is not a
//     regular file) — an environment problem, not a statement about trust, so
//     it is not wrapped in ErrUntrustedPackage;
//   - plugin.sig is malformed, or its signature does not verify against a key
//     this machine trusts — the bytes and the signature disagree, which is a
//     tampering report rather than an absence of endorsement. Both wrap
//     ErrUntrustedPackage.
//
// Everything else is a Provenance. In particular a MISSING plugin.sig is
// Unsigned rather than an error, which is the change this whole type exists
// for: whether that is allowed is the caller's policy.
func assessProvenance(dir string, manifestData []byte, trust TrustInput) (Provenance, error) {
	if trust.Keyring == nil {
		// No trust set: nothing can be verified against anything, so no key
		// can be recognised. plugin.sig is not even read — with no keys, its
		// contents could not change the verdict.
		return Provenance{State: ProvenanceUnsigned}, nil
	}

	sigPath := filepath.Join(dir, "plugin.sig")
	sigData, err := os.ReadFile(sigPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Provenance{State: ProvenanceUnsigned}, nil
		}
		// NOT ErrUntrustedPackage: see that sentinel's doc comment.
		return Provenance{}, fmt.Errorf("read plugin.sig: %w", err)
	}
	sig, err := sign.ParseSignature(sigData)
	if err != nil {
		return Provenance{}, fmt.Errorf("parse plugin.sig: %w: %w", ErrUntrustedPackage, err)
	}

	// Revocation is checked BEFORE membership, and that order is the point: a
	// revoked key stays listed among the keys (sign keeps the public half so
	// refusals can name a time and a reason), so asking "is it known?" first
	// would answer yes and hand a revoked key the Registered verdict.
	if revocation, gone := trust.Keyring.Revoked(sig.KeyID); gone {
		return Provenance{
			State:     ProvenanceRevoked,
			KeyID:     sig.KeyID,
			Publisher: trust.Publishers[sig.KeyID],
			Reason:    revocation.Reason,
			RevokedAt: revocation.At,
		}, nil
	}
	if !slices.Contains(trust.Keyring.IDs(), sig.KeyID) {
		// Signed by a key this machine does not know. Same verdict as no
		// signature at all, and KeyID stays empty — see Provenance.KeyID.
		return Provenance{State: ProvenanceUnsigned}, nil
	}
	if err := trust.Keyring.Verify(sig, manifestData); err != nil {
		return Provenance{}, fmt.Errorf("verify plugin.json signature: %w: %w", ErrUntrustedPackage, err)
	}
	return Provenance{
		State:     ProvenanceRegistered,
		KeyID:     sig.KeyID,
		Publisher: trust.Publishers[sig.KeyID],
	}, nil
}
```

- [ ] **Step 4: 改 `assemble.go`**

把 `LoadPackage` 的签名与函数体改成（**删掉整个 `verifyManifestSignature`**，它的职责已经由 `assessProvenance` 承担）：

```go
func LoadPackage(dir string, trust TrustInput) (PluginManifest, []byte, Provenance, error) {
	manifestPath := filepath.Join(dir, "plugin.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return PluginManifest{}, nil, Provenance{}, fmt.Errorf("load plugin package %q: read plugin.json: %w", dir, err)
	}
	prov, err := assessProvenance(dir, manifestData, trust)
	if err != nil {
		return PluginManifest{}, nil, Provenance{}, fmt.Errorf("load plugin package %q: %w", dir, err)
	}
	pm, err := ParsePlugin(manifestData)
	if err != nil {
		return PluginManifest{}, nil, Provenance{}, fmt.Errorf("load plugin package %q: %w", dir, err)
	}

	wasmPath := filepath.Join(dir, "plugin.wasm")
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		return PluginManifest{}, nil, Provenance{}, fmt.Errorf("load plugin package %q: read plugin.wasm: %w", dir, err)
	}

	sum := sha256.Sum256(wasm)
	actual := hex.EncodeToString(sum[:])
	if !strings.EqualFold(actual, pm.SHA256) {
		return PluginManifest{}, nil, Provenance{}, fmt.Errorf(
			"load plugin package %q: plugin %q sha256 mismatch: plugin.json declares %s, plugin.wasm actually hashes to %s",
			dir, pm.Name, pm.SHA256, actual,
		)
	}

	return pm, wasm, prov, nil
}
```

同时把 `LoadPackage` 的文档注释改写：它现在**总是**判定来源，不再有「keyring 为 nil 就完全不验」这回事；那句「Deciding when a deployment may pass nil is the caller's policy call」要改成描述三态与调用方策略的关系。**逐句核对你写的每一句**。

- [ ] **Step 5: 让其余 6 个调用点先编译过**

`LoadPackage` 有 7 个调用点（`grep -rn "LoadPackage(" --include=*.go . | grep -v _test.go`）。本任务只需让它们**编译通过**，语义接线在 Task 4/5/6 做。改法：把 `keyring` 参数包成 `manifest.TrustInput{Keyring: keyring}`，新增的 `Provenance` 返回值先接成 `_`。

**在每一处 `_` 旁边写一行注释**，标明它由哪个 Task 接线（例如 `// 三态在 Task 4 接线`）。这不是占位符：它让下一个任务的实施者能 grep 到自己该改哪里，而 review 也能看出哪些接缝还没接。

- [ ] **Step 6: 跑测试确认通过**

```bash
go build ./... && go test ./internal/plugin/manifest/ -count=1 -timeout 180s
```

预期：构建成功，manifest 包全绿（含既有用例）。

- [ ] **Step 7: 变异验证（三条，逐条做）**

**(a) 撤销检查排在成员检查之后**：把 `assessProvenance` 里 `Revoked` 那一段挪到 `slices.Contains` 之后。
```bash
go test ./internal/plugin/manifest/ -run TestProvenanceRevoked -count=1 -timeout 120s
```
预期 **FAIL**（会判成 Registered）。改回来确认 PASS。

**(b) 未知 key 的 KeyID 不留空**：把那一支改成 `return Provenance{State: ProvenanceUnsigned, KeyID: sig.KeyID}, nil`。
```bash
go test ./internal/plugin/manifest/ -run TestProvenanceUnsignedWhenTheKeyIsUnknown -count=1 -timeout 120s
```
预期 **FAIL**。改回来确认 PASS。

**(c) 篡改被当成一种状态**：把 `Verify` 失败那一支改成 `return Provenance{State: ProvenanceUnsigned}, nil`。
```bash
go test ./internal/plugin/manifest/ -run TestATamperedSignatureIsAnErrorNotAState -count=1 -timeout 120s
```
预期 **FAIL**。改回来确认 PASS。

**绝不把变异留在树里**，改回来后 `git status --porcelain` 确认。

- [ ] **Step 8: 提交**

```bash
gofmt -l . && go vet ./internal/plugin/manifest/
git add internal/plugin/manifest/provenance.go internal/plugin/manifest/provenance_test.go internal/plugin/manifest/assemble.go
# 以及 Step 5 改过的那 6 个文件，按实际路径显式列出
git commit -m "$(cat <<'EOF'
feat(manifest): LoadPackage 从「过/不过」升成三态来源判定

已登记 / 未签名 / 已撤销。「签了但 key 不在信任集」归到未签名——它对
要做决定的人意义完全相同：没有任何已登记的开发者为这份字节背书；多一态
只会让每张策略表多一行而不多一个决定。

缺 plugin.sig 从此**不是错误**，是一种判定：放不放行是调用方的策略。但
篡改仍然是错误——三态说的是「谁为它背书」，篡改说的是「它被改过」，把后者
说成 Unsigned 就等于让一个 --accept-unsigned 放行一份被改过的包。

撤销检查排在成员检查之前：被撤销的 key 仍然留在 keys 里（sign 保留公钥正是
为了让拒绝能说出时间与理由），先问「认不认识」会把它判成已登记。

没有任何信任集时一切都是 Unsigned，不是「不验签所以都放行」——Provenance
没有办法表达「没问题」这个判决。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: `Entry.AcceptedUnsigned`

**Files:**
- Modify: `internal/plugin/manifest/manifest.go`（`Entry` 加字段、`rawEntry` 加 JSON、解析与校验）
- Test: `internal/plugin/manifest/manifest_test.go`（追加用例）

**Interfaces:**
- Produces: `Entry.AcceptedUnsigned string`，JSON 键 `accepted_unsigned`，格式 `"sha256:" + 64 位十六进制`

- [ ] **Step 1: 写失败的测试**

在 `internal/plugin/manifest/manifest_test.go` 末尾追加：

```go
// TestEntryAcceptedUnsignedRoundTrips：这个字段是操作者在安装期认下的那份
// 字节的摘要，它必须原样往返——写进去什么，读出来就是什么。
func TestEntryAcceptedUnsignedRoundTrips(t *testing.T) {
	const digest = "sha256:" +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	doc := `{"plugins":[{"name":"p","source":"./p","enabled":true,` +
		`"accepted_unsigned":"` + digest + `"}]}`

	dep, err := ParseDeployment([]byte(doc))
	if err != nil {
		t.Fatalf("ParseDeployment: %v", err)
	}
	if got := dep.Plugins[0].AcceptedUnsigned; got != digest {
		t.Errorf("AcceptedUnsigned = %q, want %q", got, digest)
	}
}

// TestEntryAcceptedUnsignedIsOptional：绝大多数条目（已登记的包）根本不需要
// 它，缺省必须合法，且读出来是空串——空串的意思是「从没人认过」。
func TestEntryAcceptedUnsignedIsOptional(t *testing.T) {
	dep, err := ParseDeployment([]byte(`{"plugins":[{"name":"p","source":"./p","enabled":true}]}`))
	if err != nil {
		t.Fatalf("ParseDeployment: %v", err)
	}
	if got := dep.Plugins[0].AcceptedUnsigned; got != "" {
		t.Errorf("AcceptedUnsigned = %q, want 空串", got)
	}
}

// TestEntryAcceptedUnsignedRefusesAMalformedDigest：形状不对的值必须在解析期
// 就被拒。放它进来的后果是运行期拿它去比对时永远不相等，而报出来的会是
// 「这个包变了」——把一个配置错误伪装成一次篡改警报，排查方向完全错。
func TestEntryAcceptedUnsignedRefusesAMalformedDigest(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"缺前缀", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"长度不足", "sha256:aaaa"},
		{"非十六进制", "sha256:" + "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{"别的算法", "sha512:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := `{"plugins":[{"name":"p","source":"./p","enabled":true,` +
				`"accepted_unsigned":"` + tc.value + `"}]}`
			_, err := ParseDeployment([]byte(doc))
			if err == nil {
				t.Fatalf("%s 被接受了", tc.name)
			}
			if !strings.Contains(err.Error(), "accepted_unsigned") {
				t.Errorf("错误没点名是哪个字段：%v", err)
			}
		})
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/plugin/manifest/ -run TestEntryAcceptedUnsigned -count=1 -timeout 120s
```

预期：`AcceptedUnsigned` 未定义，编译失败。

- [ ] **Step 3: 实现**

在 `Entry` 结构体里（`GrantStated` 之后）加：

```go
	// AcceptedUnsigned is the digest of the exact plugin.json bytes an
	// operator accepted at install time for a package no registered publisher
	// endorses. Format: "sha256:" followed by 64 hex digits. Empty means
	// nobody has ever accepted this package.
	//
	// It is bound to the bytes, not to the plugin's name: an acceptance says
	// "I trust THIS build", and a name-scoped one would hand every later build
	// — including one somebody else swapped in — the same permission. Pinning
	// plugin.json is enough to pin the code, because plugin.json carries the
	// sha256 of plugin.wasm and LoadPackage compares it on every load.
	AcceptedUnsigned string
```

在 `rawEntry` 里加 `AcceptedUnsigned string \`json:"accepted_unsigned,omitempty"\``，并在把 `rawEntry` 转成 `Entry` 的地方搬运它。

在 entry 的校验函数里（与既有的 `Digest` 校验放在一起）加：

```go
	if e.AcceptedUnsigned != "" && !digestPattern.MatchString(e.AcceptedUnsigned) {
		return fmt.Errorf("plugin %q accepted_unsigned %q is not \"sha256:\" followed by 64 hex digits; "+
			"a malformed value can never match the package it is meant to pin, and the mismatch would be "+
			"reported as the package having changed — a configuration mistake wearing a tampering alarm's "+
			"clothes", e.Name, e.AcceptedUnsigned)
	}
```

（`digestPattern` 是 `manifest.go` 里既有的 `^sha256:[0-9a-fA-F]{64}$`。**先去确认它确实是这个**，别照抄本计划。）

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/plugin/manifest/ -count=1 -timeout 180s
```

- [ ] **Step 5: 变异验证**

把校验那一段的条件改成恒假（`if false && ...`，保持能编译）：
```bash
go test ./internal/plugin/manifest/ -run TestEntryAcceptedUnsignedRefusesAMalformedDigest -count=1 -timeout 120s
```
预期 **FAIL**（四个子用例）。改回来确认 PASS。

另做一条：把 `rawEntry` → `Entry` 的搬运删掉。
```bash
go test ./internal/plugin/manifest/ -run TestEntryAcceptedUnsignedRoundTrips -count=1 -timeout 120s
```
预期 **FAIL**。改回来确认 PASS。

- [ ] **Step 6: 提交**

```bash
gofmt -l . && go vet ./internal/plugin/manifest/
git add internal/plugin/manifest/manifest.go internal/plugin/manifest/manifest_test.go
git commit -m "$(cat <<'EOF'
feat(manifest): Entry 记录安装期认下的那份未签名字节

绑定到 plugin.json 的确切字节而不是插件名：一次确认说的是「我信任这一份
构建」，绑到名字上就等于把同一张通行证发给了此后每一份构建——包括别人换
进来的那一份。钉住 plugin.json 就等于钉住了代码，因为它带着 plugin.wasm
的 sha256 而 LoadPackage 每次加载都比对。

形状不对的值在解析期就拒：放进来的后果是运行期永远比不上，而报出来的会是
「这个包变了」——一个配置错误穿着篡改警报的外衣。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: 合并信任集（本地 keyring ∪ 清单）

**Files:**
- Create: `internal/plugin/trustlist/merge.go`
- Create: `internal/plugin/trustlist/merge_test.go`

**Interfaces:**
- Consumes: 本包既有的 `Trust`（`Keyring`/`Publishers`/`Status`）、`Publisher`
- Produces:
  - `func Merge(local *sign.Keyring, t Trust) (*sign.Keyring, map[sign.KeyID]string, error)`

**这个函数放在 `trustlist` 而不是 `cli`**：合并两个 keyring 是**机制**不是部署策略，而本包已经有 `assembleKeyring` 在做同形状的事（拼一份 keyring 文档再交给 `sign.ParseKeyring`）。放在 `cli` 会让这段逻辑离它唯一的同类实现很远，而两处各写一遍正是 S1 反复吃亏的地方。

- [ ] **Step 1: 写失败的测试**

创建 `internal/plugin/trustlist/merge_test.go`：

```go
package trustlist

import (
	"crypto/ed25519"
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// localKeyring 造一份「本地 keyring 文件」那一侧的信任集。
func localKeyring(t *testing.T, keys map[sign.KeyID]ed25519.PublicKey, revoked []sign.KeyID) *sign.Keyring {
	t.Helper()
	return parseKeyringDoc(t, keys, revoked)
}

// TestMergeTakesTheUnionOfBothSides：两侧登记的钥匙都要能用。
// 内网自己签的插件与公开生态的插件，是同一台机器上并存的两类东西。
func TestMergeTakesTheUnionOfBothSides(t *testing.T) {
	localPub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	listPub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	local := localKeyring(t, map[sign.KeyID]ed25519.PublicKey{"ops-local": localPub}, nil)
	listTrust := trustFrom(t, map[sign.KeyID]ed25519.PublicKey{"dev-abc": listPub}, nil,
		map[sign.KeyID]string{"dev-abc": "张三"})

	merged, publishers, err := Merge(local, listTrust)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	ids := merged.IDs()
	if len(ids) != 2 {
		t.Fatalf("合并后有 %d 把钥匙，want 2：%v", len(ids), ids)
	}
	if publishers["dev-abc"] != "张三" {
		t.Errorf("publishers[dev-abc] = %q, want 张三", publishers["dev-abc"])
	}
}

// TestMergeTakesTheUnionOfRevocations 是这个函数最重要的性质，两个方向都要守：
// 任何一边说撤销了就是撤销了。撤销单调这条不能因为来源多了就打折。
func TestMergeTakesTheUnionOfRevocations(t *testing.T) {
	pubA, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pubB, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	t.Run("只有本地撤销", func(t *testing.T) {
		local := localKeyring(t,
			map[sign.KeyID]ed25519.PublicKey{"k": pubA, "live": pubB}, []sign.KeyID{"k"})
		listTrust := trustFrom(t, map[sign.KeyID]ed25519.PublicKey{"k": pubA}, nil, nil)
		merged, _, err := Merge(local, listTrust)
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if _, gone := merged.Revoked("k"); !gone {
			t.Error("本地撤销的 k 在合并后不再是撤销状态——被清单那一半覆盖掉了")
		}
	})

	t.Run("只有清单撤销", func(t *testing.T) {
		local := localKeyring(t,
			map[sign.KeyID]ed25519.PublicKey{"k": pubA, "live": pubB}, nil)
		listTrust := trustFrom(t,
			map[sign.KeyID]ed25519.PublicKey{"k": pubA}, []sign.KeyID{"k"}, nil)
		merged, _, err := Merge(local, listTrust)
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if _, gone := merged.Revoked("k"); !gone {
			t.Error("清单撤销的 k 在合并后不再是撤销状态——本地那一半盖过了撤销")
		}
	})
}

// TestMergeSurvivesEitherSideBeingAbsent：断网（清单不可用）与没配本地 keyring
// 都是正常状态，各自都要能单独工作。
func TestMergeSurvivesEitherSideBeingAbsent(t *testing.T) {
	pub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	t.Run("只有本地", func(t *testing.T) {
		local := localKeyring(t, map[sign.KeyID]ed25519.PublicKey{"ops": pub}, nil)
		merged, _, err := Merge(local, Trust{Status: StatusUnavailable})
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if len(merged.IDs()) != 1 {
			t.Errorf("IDs = %v, want 只有 ops", merged.IDs())
		}
	})

	t.Run("只有清单", func(t *testing.T) {
		listTrust := trustFrom(t, map[sign.KeyID]ed25519.PublicKey{"dev": pub}, nil, nil)
		merged, _, err := Merge(nil, listTrust)
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if len(merged.IDs()) != 1 {
			t.Errorf("IDs = %v, want 只有 dev", merged.IDs())
		}
	})

	t.Run("两边都没有", func(t *testing.T) {
		merged, publishers, err := Merge(nil, Trust{Status: StatusUnavailable})
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if merged != nil {
			t.Errorf("两边都没有时 merged = %v, want nil——nil 的意思是「没有信任集」", merged.IDs())
		}
		if len(publishers) != 0 {
			t.Errorf("publishers = %v, want 空", publishers)
		}
	})
}
```

同文件里补两个辅助（`parseKeyringDoc` 与 `trustFrom`）；`trustFrom` 造一个本包的 `Trust`（`Keyring` + `Publishers` + `Status: StatusFresh`），`parseKeyringDoc` 拼 keyring 文档再 `sign.ParseKeyring`。**这两个辅助不得使用本包 `document_test.go` 里的 `newSigner`**——那个会换掉内嵌 root，本任务用不着，引进来只会让用例之间产生看不见的耦合。

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/plugin/trustlist/ -run TestMerge -count=1 -timeout 120s
```

预期：`undefined: Merge`。

- [ ] **Step 3: 实现**

创建 `internal/plugin/trustlist/merge.go`：

```go
package trustlist

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// Merge combines the deployment's local keyring with the fetched trust list
// into the single trust set a package is judged against, and returns the
// display names that go with it.
//
// Both halves answer the same question — which signing keys does this
// deployment trust — from different places: the local file serves an intranet
// or an offline install, the list serves the public ecosystem. So registrations
// are unioned: both kinds of plugin have to be loadable on one machine.
//
// Revocations are unioned too, and that is the half that matters: either side
// saying a key is revoked makes it revoked. Letting one side's silence cancel
// the other's revocation would make revocation depend on where it was recorded,
// and revocation is the one thing in this design that must not weaken as more
// sources appear.
//
// A nil result means this deployment has no trust set at all. That is not
// "signatures are not checked" — it is "no key is recognised", and every
// package judged against it comes out unsigned.
//
// The merged set is assembled by writing a keyring document and handing it to
// sign.ParseKeyring, never by reasoning about trust outside sign.Keyring: one
// rule implemented twice drifts, and the direction it drifts is always some
// path forgetting to consult the revocations.
func Merge(local *sign.Keyring, t Trust) (*sign.Keyring, map[sign.KeyID]string, error) {
	type keyEntry struct {
		ID        sign.KeyID `json:"id"`
		Algorithm string     `json:"algorithm"`
		PublicKey string     `json:"public_key"`
	}
	type revEntry struct {
		KeyID     sign.KeyID `json:"key_id"`
		RevokedAt string     `json:"revoked_at,omitempty"`
		Reason    string     `json:"reason,omitempty"`
	}

	keys := map[sign.KeyID]keyEntry{}
	revoked := map[sign.KeyID]revEntry{}

	absorb := func(kr *sign.Keyring) error {
		if kr == nil {
			return nil
		}
		for _, id := range kr.IDs() {
			// A keyring hands back its keys only as a document, so round-trip
			// one entry at a time through the encoder sign itself provides.
			// Reaching into the type would mean this package owning a second
			// idea of what a key entry is.
			data, err := sign.MarshalKeyEntry(id, mustPublicKey(kr, id))
			if err != nil {
				return fmt.Errorf("merge trust sets: re-encode key %q: %w", id, err)
			}
			var e keyEntry
			if err := json.Unmarshal(data, &e); err != nil {
				return fmt.Errorf("merge trust sets: re-read key %q: %w", id, err)
			}
			if _, dup := keys[id]; !dup {
				keys[id] = e
			}
		}
		for _, id := range kr.RevokedIDs() {
			rec, _ := kr.Revoked(id)
			if _, dup := revoked[id]; dup {
				continue
			}
			entry := revEntry{KeyID: id, Reason: rec.Reason}
			if !rec.At.IsZero() {
				entry.RevokedAt = rec.At.Format(timeFormatRFC3339)
			}
			revoked[id] = entry
		}
		return nil
	}

	if err := absorb(local); err != nil {
		return nil, nil, err
	}
	if err := absorb(t.Keyring); err != nil {
		return nil, nil, err
	}
	if len(keys) == 0 {
		return nil, map[sign.KeyID]string{}, nil
	}

	keyList := make([]keyEntry, 0, len(keys))
	for _, e := range keys {
		keyList = append(keyList, e)
	}
	sort.Slice(keyList, func(i, j int) bool { return keyList[i].ID < keyList[j].ID })
	revList := make([]revEntry, 0, len(revoked))
	for _, e := range revoked {
		revList = append(revList, e)
	}
	sort.Slice(revList, func(i, j int) bool { return revList[i].KeyID < revList[j].KeyID })

	doc := struct {
		Keys    []keyEntry `json:"keys"`
		Revoked []revEntry `json:"revoked,omitempty"`
	}{Keys: keyList, Revoked: revList}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, fmt.Errorf("merge trust sets: %w", err)
	}
	merged, err := sign.ParseKeyring(data)
	if err != nil {
		return nil, nil, fmt.Errorf("merge trust sets: %w", err)
	}

	names := make(map[sign.KeyID]string, len(t.Publishers))
	for id, p := range t.Publishers {
		names[id] = p.DisplayName
	}
	return merged, names, nil
}
```

**两处需要你自己解决的实现细节**（本计划刻意不替你决定，因为它们取决于 `sign` 包的实际导出面）：

1. `mustPublicKey(kr, id)` —— `sign.Keyring` 有没有导出「按 id 取公钥」的方法？**去 `internal/plugin/sign/sign.go` 看**。如果没有，你不能加（`sign` 一行不改），那就换一条路：让 `Merge` 接收**两份 keyring 文档的原始字节**而不是两个 `*sign.Keyring`。这条路更直接、也更符合「原样搬运字节」的既有做法。**选哪条都要在注释里说明理由。**
2. `timeFormatRFC3339` —— 用 `time.RFC3339`，别自己写格式串。

**如果你选了「接收原始字节」那条路，签名与调用方随之改变，请在报告里写清新签名**，Task 4 会用到。

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/plugin/trustlist/ -count=1 -timeout 180s
go test ./internal/plugin/trustlist/ -race -count=5 -timeout 600s
```

- [ ] **Step 5: 变异验证（两条）**

**(a) 撤销不取并集**：把 `absorb` 里处理 `RevokedIDs()` 的那段循环整个跳过（`if false { ... }`）。
```bash
go test ./internal/plugin/trustlist/ -run TestMergeTakesTheUnionOfRevocations -count=1 -timeout 120s
```
预期 **FAIL**（两个子用例都要红）。改回来确认 PASS。

**(b) 登记不取并集**：让 `absorb(t.Keyring)` 不被调用。
```bash
go test ./internal/plugin/trustlist/ -run TestMergeTakesTheUnionOfBothSides -count=1 -timeout 120s
```
预期 **FAIL**。改回来确认 PASS。

- [ ] **Step 6: 提交**

```bash
gofmt -l . && go vet ./internal/plugin/trustlist/
git add internal/plugin/trustlist/merge.go internal/plugin/trustlist/merge_test.go
git commit -m "$(cat <<'EOF'
feat(trustlist): 本地 keyring 与联网清单合并成一个信任集

登记取并集：内网自己签的插件与公开生态的插件是同一台机器上并存的两类东西。
撤销也取并集，而这一半才是要害——任何一边说撤销了就是撤销了。让一边的沉默
去抵消另一边的撤销，等于让撤销的效力取决于它被记在哪里，而撤销恰恰是这套
设计里唯一不能因为来源变多而变弱的东西。

nil 结果的意思是「这个部署没有任何信任集」，不是「不验签」——那会让每个包
都判成未签名，而不是判成没问题。

装配走 sign.ParseKeyring，不在 sign.Keyring 外面自建第二套判断：一条规则
写两遍必然分家，而分家的方向总是某条路忘了查撤销。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `loader` 动态读信任集，并执行策略

**Files:**
- Modify: `internal/plugin/loader/loader.go`（`Config.Keyring` → `Config.TrustSet`；`prepare` 消费三态；`SignaturePolicy` 语义收窄）
- Test: `internal/plugin/loader/signature_test.go`（改造既有用例）+ 新增用例

**Interfaces:**
- Consumes: `manifest.LoadPackage(dir, manifest.TrustInput) (PluginManifest, []byte, Provenance, error)`、`manifest.ProvenanceRegistered/Unsigned/Revoked`、`manifest.ErrUnsignedNotAccepted`、`manifest.ErrRevokedPublisher`、`Entry.AcceptedUnsigned`、Task 3 的合并函数
- Produces:
  - `type TrustSet func() (manifest.TrustInput, error)`
  - `Config.TrustSet TrustSet`（取代 `Config.Keyring`）
  - `Config.RequireSignature bool`
  - `SignaturePolicy` 语义收窄为「本地 keyring 配置」

- [ ] **Step 1: 写失败的测试**

在 `internal/plugin/loader/signature_test.go` 追加（既有用例需要跟着改签名，一并处理）：

```go
// TestUnsignedWithoutAcceptanceDoesNotMount：未签名且没人认过 → 不挂载。
func TestUnsignedWithoutAcceptanceDoesNotMount(t *testing.T) { /* 见下方说明 */ }

// TestUnsignedWithMatchingAcceptanceMounts：认过、且摘要对得上 → 挂载。
func TestUnsignedWithMatchingAcceptanceMounts(t *testing.T) { /* ... */ }

// TestUnsignedWithStaleAcceptanceIsReportedAsAChangedPackage：认过，但摘要
// 对不上。错误必须与「从未认过」**可区分**——前者的意思是这个包变了，后者是
// 没人认过它，操作者要做的事完全不同。
func TestUnsignedWithStaleAcceptanceIsReportedAsAChangedPackage(t *testing.T) { /* ... */ }

// TestRevokedIsRefusedEvenWithAnAcceptance：撤销压过一切确认。
func TestRevokedIsRefusedEvenWithAnAcceptance(t *testing.T) { /* ... */ }

// TestRequireSignatureFalseMountsUnsigned：显式声明不要求签名的既有部署，
// 升级后不能突然掉光插件。
func TestRequireSignatureFalseMountsUnsigned(t *testing.T) { /* ... */ }

// TestRequireSignatureFalseStillRefusesRevoked：那个开关管不到撤销。
func TestRequireSignatureFalseStillRefusesRevoked(t *testing.T) { /* ... */ }

// TestTrustSetIsReadOnEveryMount 是本任务最重要的接线守卫：
// 挂载一次之后把撤销集变大，**不重启**，下一次挂载必须拒绝。
//
// 做法：TrustSet 闭包持有一个计数器与一个可变的信任集（**在测试里**，不是
// 生产代码里的包级变量）；第一次返回不含撤销的集合，第二次返回含撤销的。
// 断言：第一次挂载成功、第二次被拒、且闭包被调用了两次。
func TestTrustSetIsReadOnEveryMount(t *testing.T) { /* ... */ }
```

**这些用例的夹具要照 `internal/plugin/loader` 既有测试的搭法写**（`loader_test.go` / `signature_test.go` 里已有构造 Loader 与临时插件目录的辅助）。**先读它们，用既有夹具，不要另起一套**——这个包的测试夹具已经很重，第二套只会让下一个人不知道该用哪个。

**不得使用 `t.Fatal` 占位**。计划在这里不给完整代码，是因为夹具形状取决于既有测试；但**每条用例都必须是真实断言**，写不出来就先读夹具再写，不要留桩。

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/plugin/loader/ -run "TestUnsigned|TestRevoked|TestRequireSignature|TestTrustSet" -count=1 -p 1 -timeout 300s
```

- [ ] **Step 3: 实现 `TrustSet` 与策略**

`Config` 里把 `Keyring *sign.Keyring` 换成：

```go
	// TrustSet reports the trust set to judge a package against, read afresh
	// on every mount.
	//
	// It is a function rather than a value because the fetched trust list
	// refreshes in the background while this process runs: a set captured at
	// assembly would leave a revocation that arrived after startup with no way
	// to reach the plugins it was published to stop. Reading it per mount
	// costs one merge and buys "an emergency revocation lands without a
	// restart".
	//
	// Returning a zero TrustInput (no keyring) is a deployment with no trust
	// set. See manifest.TrustInput for why that is not the same as unchecked.
	TrustSet TrustSet

	// RequireSignature is whether a package no registered publisher endorses
	// needs an install-time acceptance before it may mount.
	//
	// False is a deployment's explicit statement that it does not require an
	// endorsement, and it is honoured for exactly that: unsigned packages
	// mount, with a warning on every mount. It does NOT extend to revoked
	// keys — "I do not require an endorsement" and "I am willing to run code
	// that was withdrawn" are different sentences, and only the first one was
	// said.
	RequireSignature bool
```

`prepare` 里替换 `manifest.LoadPackage(dir, l.keyring)`：

```go
	trust, err := l.trustSet()
	if err != nil {
		return nil, l.fail(ctx, entry.Name, "", stepLoadPackage,
			fmt.Errorf("read trust set: %w", err), nil)
	}
	pm, wasm, prov, err := manifest.LoadPackage(dir, trust)
	if err != nil {
		// ...既有的 EvictUntrusted 逻辑一字不改...
		return nil, l.fail(ctx, entry.Name, "", stepLoadPackage, err, nil)
	}
	if err := l.admit(entry, dir, prov); err != nil {
		return nil, l.fail(ctx, entry.Name, "", stepLoadPackage, err, nil)
	}
```

新增 `admit`（策略在这里，且只在这里）：

```go
// admit applies this deployment's policy to a package's provenance.
//
// The three outcomes are not symmetric. A registered package loads. An
// unsigned one loads only if an operator accepted these exact bytes, or if the
// deployment has said it does not require an endorsement. A revoked one never
// loads, whatever else is configured.
func (l *Loader) admit(entry manifest.Entry, dir string, prov manifest.Provenance) error {
	switch prov.State {
	case manifest.ProvenanceRevoked:
		return fmt.Errorf("plugin %q is signed by key %q, which this deployment revoked%s: %w",
			entry.Name, prov.KeyID, describeRevocation(prov), manifest.ErrRevokedPublisher)

	case manifest.ProvenanceRegistered:
		l.logger.Info("plugin package is endorsed by a registered publisher",
			"plugin", entry.Name, "key_id", prov.KeyID, "publisher", prov.Publisher)
		return nil

	case manifest.ProvenanceUnsigned:
		digest, err := manifestDigest(dir)
		if err != nil {
			return err
		}
		switch {
		case entry.AcceptedUnsigned == digest:
			l.logger.Info("plugin package is unsigned and mounts on an install-time acceptance",
				"plugin", entry.Name, "accepted_digest", digest)
			return nil
		case entry.AcceptedUnsigned == "" && !l.requireSignature:
			l.logger.Warn("plugin package mounts with no endorsement because this deployment does not require one",
				"plugin", entry.Name, "digest", digest)
			return nil
		case entry.AcceptedUnsigned == "":
			return fmt.Errorf("plugin %q has no endorsement from a registered publisher and nobody has "+
				"accepted it; its plugin.json hashes to %s: %w",
				entry.Name, digest, manifest.ErrUnsignedNotAccepted)
		default:
			return fmt.Errorf("plugin %q changed since it was accepted: the acceptance on record covers %s, "+
				"the package on disk hashes to %s. This is not the same bytes anyone approved: %w",
				entry.Name, entry.AcceptedUnsigned, digest, manifest.ErrUnsignedNotAccepted)
		}

	default:
		// A state this function does not know how to judge must not be
		// admitted by falling through: an unhandled enum value is a
		// programming error, and treating it as "fine" is the exact shape of
		// silent trust escalation.
		return fmt.Errorf("plugin %q has provenance state %v, which this deployment does not know how to "+
			"judge: %w", entry.Name, prov.State, manifest.ErrUnsignedNotAccepted)
	}
}
```

**注意 `require_signature: false` + 有 `AcceptedUnsigned` 但对不上的情况**：上面的 `switch` 会落到 `default` 分支（报「这个包变了」），**即使 `requireSignature` 为 false**。这是有意的：一份对不上的确认记录说明**这个包变了**，而那件事与「要不要背书」无关，值得让操作者看见。**把这条写进 `admit` 的注释。**

`manifestDigest(dir)` 读 `dir/plugin.json` 算 `"sha256:" + hex`。它与 `Entry.AcceptedUnsigned` 的格式必须完全一致，**且与 Task 5 的 `install` 用同一个函数** —— 两处各算一遍，是这一期最容易出现的「写得出但读不回」。把它放在 `manifest` 包并导出，让两边共用。

- [ ] **Step 4: `SignaturePolicy` 语义收窄**

`SignaturePolicyOf` 改成只描述**本地 keyring 配置**。同时**必须改注释**：现有那段论证（「新清单在旧信任集下生效」「被撤销的 key 照样验得过，屏幕上还写着 reload 成功」）对本地 keyring 仍然成立，**逐字保留**；但要补一句说明它**不再覆盖清单那一半**，以及为什么不需要——清单侧是每次挂载动态读的，根本不存在「在旧信任集下收敛」这回事。

**再补一句反面理由**：把会变的清单放进相等性比较，会让 `plugins reload` 在清单刚好刷新过的时候随机拒绝收敛，而**一道随机失败的守卫比没有守卫更糟**——人会学会忽略它。

- [ ] **Step 5: 跑测试确认通过**

```bash
go build ./... && go test ./internal/plugin/loader/ -count=1 -p 1 -timeout 600s
```

- [ ] **Step 6: 变异验证（五条，逐条做）**

| # | 变异 | 预期红的用例 |
|---|------|-------------|
| a | `admit` 里 `ProvenanceRevoked` 那一支改成 `return nil` | `TestRevokedIsRefusedEvenWithAnAcceptance`、`TestRequireSignatureFalseStillRefusesRevoked` |
| b | `entry.AcceptedUnsigned == digest` 改成 `entry.AcceptedUnsigned != ""` | `TestUnsignedWithStaleAcceptanceIsReportedAsAChangedPackage` |
| c | 「摘要对不上」与「从未认过」合并成同一句错误 | 同上（断言的是**可区分**） |
| d | `l.trustSet()` 的结果缓存起来只调一次 | `TestTrustSetIsReadOnEveryMount` |
| e | **`admit` 整个调用删掉**（`prepare` 里不调它） | 上面所有策略用例 |

**(e) 是本任务的接线守卫**——`LoadPackage` 算出三态却没人消费，正是 S1 六次复发的那个形状。

- [ ] **Step 7: 提交**

```bash
gofmt -l . && go vet ./...
git add internal/plugin/loader/loader.go internal/plugin/loader/signature_test.go
git commit -m "$(cat <<'EOF'
feat(loader): 信任集改成每次挂载动态读，并执行分级策略

冻结的信任集会让后台刷到的紧急撤销完全够不着已经在跑的进程——那条通路
正是整套机制存在的理由。改成提供者之后，撤销在下一次挂载即生效。

策略集中在 admit 一个地方：已登记直接挂；未签名要么有一份摘要对得上的
安装期确认、要么这个部署显式声明不要求背书；已撤销无论如何都不挂。

require_signature: false 管的是「要不要背书」，管不到撤销——「我不要求
背书」与「我愿意运行被吊销的代码」是两句不同的话，只说了第一句。

「摘要对不上」与「从未认过」分开报，且前者即使在 require_signature: false
下也照报：一份对不上的确认说明这个包变了，那件事与要不要背书无关。

未知的枚举值走 default 并拒绝，不 fall through：把没见过的状态当成「没问题」
正是静默信任升级的形状。

SignaturePolicy 收窄成只守本地 keyring 配置。把会变的清单放进相等性比较，
会让 reload 在清单刚刷新过时随机拒绝收敛，而一道随机失败的守卫比没有守卫
更糟——人会学会忽略它。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: `install --accept-unsigned`

**Files:**
- Modify: `internal/cli/plugins_command.go`（install 的 flag、`runPluginsInstall`、装配 `TrustSet` 闭包）
- Test: `internal/cli/plugins_command_test.go`

**Interfaces:**
- Consumes: Task 1–4 的全部产出；`manifest.ManifestDigest(dir)`（Task 4 导出的那个）
- Produces: `--accept-unsigned` flag；写入 `Entry.AcceptedUnsigned`

- [ ] **Step 1: 写失败的测试**

```go
// TestInstallRefusesAnUnsignedPackageWithoutTheFlag：错误必须说清是什么问题、
// 并给出要加的 flag——一条只说「不行」的错误会让人去猜。
func TestInstallRefusesAnUnsignedPackageWithoutTheFlag(t *testing.T) { /* 用既有 install 测试夹具 */ }

// TestInstallRecordsTheAcceptedDigest 是本任务的接线守卫：
// 传了 flag 之后，plugins.json 里必须真的出现那份摘要，而且它必须等于
// manifest.ManifestDigest 对同一个目录算出来的值——两处各算一遍是这一期
// 最容易出现的「写得出但读不回」。
func TestInstallRecordsTheAcceptedDigest(t *testing.T) { /* ... */ }

// TestInstallRefusesARevokedPackageEvenWithTheFlag：flag 的名字里是 unsigned，
// 撤销不是未签名。
func TestInstallRefusesARevokedPackageEvenWithTheFlag(t *testing.T) { /* ... */ }

// TestInstallShowsThePublisherForARegisteredPackage
func TestInstallShowsThePublisherForARegisteredPackage(t *testing.T) { /* ... */ }
```

**同样：读既有 install 测试夹具再写，不留 `t.Fatal` 桩。**

- [ ] **Step 2–4: 跑红 → 实现 → 跑绿**

flag 定义（放在既有 `--digest` / `--grant` 旁边）：

```go
	cmd.Flags().BoolVar(&acceptUnsigned, "accept-unsigned", false,
		"install a package that no registered publisher endorses, recording an acceptance of these exact bytes")
```

`runPluginsInstall` 里，拿到 `prov` 之后：

```go
	switch prov.State {
	case manifest.ProvenanceRevoked:
		return fmt.Errorf("plugins install: %s is signed by key %q, which this deployment revoked%s. "+
			"--accept-unsigned does not apply: that flag accepts a package nobody has endorsed, while this "+
			"one carries an endorsement that was withdrawn: %w",
			sourceArg, prov.KeyID, describeRevocation(prov), manifest.ErrRevokedPublisher)
	case manifest.ProvenanceUnsigned:
		if !acceptUnsigned {
			return fmt.Errorf("plugins install: %s carries no endorsement from any registered publisher; "+
				"its plugin.json hashes to %s. Re-run with --accept-unsigned to record that you accept "+
				"these exact bytes: %w", sourceArg, digest, manifest.ErrUnsignedNotAccepted)
		}
		entry.AcceptedUnsigned = digest
	case manifest.ProvenanceRegistered:
		fmt.Fprintf(out, "来自 %s (%s)\n", prov.Publisher, prov.KeyID)
	}
```

（`describeRevocation` 与 Task 4 共用同一个——**放在一处导出，不要各写一遍**。）

- [ ] **Step 5: 装配 `TrustSet` 闭包**

在 serve 装配处（`resolvePluginKeyring` / `resolvePluginTrustlist` 旁边）把两者合成一个 `loader.TrustSet`：

```go
// pluginTrustSet builds the provider a Loader reads its trust set from on every
// mount: the local keyring merged with whatever the trust list currently holds.
//
// The local keyring is resolved once, at assembly, because it is configuration
// — changing it is a config change, which `agent plugins reload` compares
// through SignaturePolicy. The list half is read per call, because it refreshes
// underneath this process.
func pluginTrustSet(local *sign.Keyring, store *trustlist.Store) loader.TrustSet {
	return func() (manifest.TrustInput, error) {
		var listed trustlist.Trust
		if store != nil {
			t, err := store.Current()
			if err != nil && !errors.Is(err, /* 「还没有缓存」那个哨兵 */ nil) {
				// 见下方说明
			}
			listed = t
		}
		merged, names, err := trustlist.Merge(local, listed)
		if err != nil {
			return manifest.TrustInput{}, err
		}
		return manifest.TrustInput{Keyring: merged, Publishers: names}, nil
	}
}
```

**这里有一个你必须自己决定并写明理由的点**：`store.Current()` 在缓存缺失或损坏时返回 `(Trust{Status: StatusUnavailable}, error)` —— **两者都返回**。这个提供者该怎么处理那个 error？

- 把它往上抛 → 每次挂载都因为「还没拉到清单」而失败，**首次启动的机器永远起不来**；
- 完全忽略 → 缓存损坏被静默吞掉，正是 fail-loud 铁律禁止的；
- 折中：**用返回的 `Trust`（unavailable，即清单那一半为空），但把 error 记进日志**。

**我倾向第三条**，理由是 S1 已经把「unavailable 时 Keyring 为 nil」设计成一个可以安全使用的状态，而丢掉日志才是真正的静默。但**你要自己核一遍 `Store.Current` 的实际契约再定**，并把选择与理由写进注释和报告。

- [ ] **Step 6: 变异验证（三条）**

| # | 变异 | 预期红 |
|---|------|--------|
| a | 传了 flag 但不写 `entry.AcceptedUnsigned` | `TestInstallRecordsTheAcceptedDigest` |
| b | Revoked 那一支改成走 Unsigned 的逻辑 | `TestInstallRefusesARevokedPackageEvenWithTheFlag` |
| c | `install` 与 `loader` 各自算摘要（把 install 改成算 `plugin.wasm` 的） | `TestInstallRecordsTheAcceptedDigest`（断言两边相等） |

- [ ] **Step 7: 提交**（信息略，照前面的风格写）

---

## Task 6: `PluginView` 显示信任状态

**Files:**
- Modify: `internal/server/plugins.go`（`PluginView` 加三个字段）
- Modify: `internal/cli/plugin_consent_service.go`（`List` / `Grant` / `Resolve` 三处填充）
- Test: `internal/server/plugins_test.go`、`internal/cli/plugin_consent_service_test.go`

**Interfaces:**
- Produces：`PluginView.TrustState string` / `TrustPublisher string` / `TrustDetail string`，JSON 键 `trust_state` / `trust_publisher` / `trust_detail`

- [ ] **Step 1: 写失败的测试**

```go
// TestPluginViewCarriesTheTrustState：三条路（List / Grant / Resolve）都要填，
// 缺一条就会出现「同一个插件在两个界面上显示不同」。
func TestPluginViewCarriesTheTrustState(t *testing.T) { /* 三条路各一个子用例 */ }

// TestGrantResponseShowsTheTrustState：授权响应也带它。
// 本设计刻意不在 grant 里做信任校验（那会把安装期的策略复制到第二个地方），
// 代价是 GUI 可能在授权一个未认过的包——所以它至少要**看得见**这件事。
func TestGrantResponseShowsTheTrustState(t *testing.T) { /* ... */ }
```

- [ ] **Step 2–4: 跑红 → 实现 → 跑绿**

`PluginView` 加：

```go
	// TrustState is what this machine's trust set says about who stands behind
	// the package's bytes: "registered", "unsigned" or "revoked".
	TrustState string `json:"trust_state"`
	// TrustPublisher is the registered publisher's display name, empty unless
	// TrustState is "registered".
	TrustPublisher string `json:"trust_publisher,omitempty"`
	// TrustDetail carries what a refusal would say — for a revoked key, when it
	// was revoked and why.
	TrustDetail string `json:"trust_detail,omitempty"`
```

三处调用点把 `prov` 映射进去。**`prov.State.String()` 已经产出 "registered"/"unsigned"/"revoked"，直接用它，不要在这里另写一张映射表** —— 两处各写一遍，迟早有一处漏掉新加的状态。

- [ ] **Step 5: 变异验证**

把 `Resolve` 那一处的填充删掉（其余两处保留）：预期 `TestPluginViewCarriesTheTrustState` 的 Resolve 子用例红，另两个仍绿。这条证明**三条路各自被守着**，而不是一条绿了就全绿。

- [ ] **Step 6: 提交**

---

## Task 7: 端到端验证

这一步是**验收条件**，不是可选项。理由与 S1 相同：本期的策略散落在 `manifest` / `loader` / `cli` / `server` 四个包，而单元测试各自只看自己那一段。S1 里六次「接缝在但没人测那条接缝」全部是跨包的。

- [ ] **Step 1: 造三个包**

在临时目录造三个插件包：一个由已登记的钥匙签、一个未签名、一个由**已撤销**的钥匙签。用 `agent plugins keygen` 生成钥匙，用 `agent plugins sign` 签包。

- [ ] **Step 2: 逐个 install，记录实际输出**

```bash
go run ./cmd/agent plugins install <registered> --config <tmp>/agent.json   # 应显示「来自 …」
go run ./cmd/agent plugins install <unsigned>   --config <tmp>/agent.json   # 应拒绝并给出 flag
go run ./cmd/agent plugins install <unsigned> --accept-unsigned --config <tmp>/agent.json
go run ./cmd/agent plugins install <revoked> --accept-unsigned --config <tmp>/agent.json  # 应硬拒
```

- [ ] **Step 3: 核 `plugins.json`**

确认未签名那条的 `accepted_unsigned` 真的写进去了，且**等于**对同一目录算出的摘要。

- [ ] **Step 4: 起 serve，看挂载日志**

三个包的挂载结果与日志文本逐条抄进报告。

- [ ] **Step 5: 改一个字节，再起一次**

改未签名那个包的 `plugin.json` 一个字节（同时更新它内部的 `sha256` 使 wasm 校验仍然通过），重启 serve。预期：**拒绝挂载，且错误说的是「这个包变了」而不是「没人认过它」**。

- [ ] **Step 6: 撤销一把正在用的钥匙**

在信任清单里撤销那把已登记的钥匙，等刷新，**不重启 serve**，触发一次收敛。预期：那个插件被拒绝挂载，日志带撤销时间与理由。**这条验证的是 Task 4 的动态读**，也是本期唯一只有端到端才能验的一条。

- [ ] **Step 7: 把实际输出写进计划末尾**

新开一节「端到端验证记录」，包含**每一步的实际输出**，以及任何**没有**按预期发生的事。S1 的教训：真机验证只有在记录了实际输出时才算数，「跑过了」不是证据。

---

## Self-Review

**Spec 覆盖核对：**

| spec 节 | 任务 |
|---|---|
| §2 三态判定、KeyID 留空、篡改仍是错误 | Task 1 |
| §3 并集、三条不可违反规则、两边都没有时 | Task 3 + Task 4（策略侧） |
| §3 `require_signature` 兼容性 | Task 4 |
| §4.1 `AcceptedUnsigned` 字段与绑定对象 | Task 2 |
| §4.2 运行期判定表 | Task 4 |
| §4.3 拒绝粒度与每次都记日志 | Task 4（`l.fail` 已保证其余条目继续收敛） |
| §5 动态读、`SignaturePolicy` 收窄 | Task 4 |
| §6.1 install 四种行为 | Task 5 |
| §6.2 `PluginView` 三字段 | Task 6 |
| §6.3 grant 不做信任校验但带上状态 | Task 6 |
| §7 两个哨兵与日志措辞 | Task 1（哨兵）+ Task 4（措辞） |
| §8 测试清单 | 分布在 Task 1–6，端到端在 Task 7 |
| §9 不做的六件事 | 不产生任务（正确） |

**两处本计划有意留给实施者判断、且要求写明理由的点**（不是占位符——它们取决于本计划无法替他读到的既有代码）：
1. Task 3 的 `Merge` 到底收 `*sign.Keyring` 还是原始字节，取决于 `sign.Keyring` 有没有导出按 id 取公钥的方法；
2. Task 5 里 `store.Current()` 同时返回 `Trust` 和 `error` 时提供者怎么处理，我给了倾向与理由，但要求他自己核 `Store.Current` 的实际契约再定。

**类型一致性**：`Provenance` / `ProvenanceState` / `TrustInput` / `TrustSet` / `AcceptedUnsigned` / `ManifestDigest` / `describeRevocation` 在各任务间的名字逐处对过，一致。`describeRevocation` 与 `ManifestDigest` 都明确要求**放在一处共用**，因为它们各写一遍正是本期最容易出现的缺陷。

**已知的一处粗糙**：Task 4 与 Task 5 的测试代码没有给出完整实现，因为它们的夹具形状取决于 `internal/plugin/loader` 与 `internal/cli` 既有测试的搭法。计划里明确要求**先读既有夹具再写、不得留 `t.Fatal` 桩**，并逐条给出了每个用例要断言什么。这是本计划里唯一没有完整代码的地方，实施时如果发现夹具不足以表达某条断言，**先停下来问控制者**，不要降低断言强度。

---

## 执行方式

Plan complete and saved to `docs/superpowers/plans/2026-09-06-plugin-graded-install.md`. 两种执行方式：

**1. Subagent-Driven（推荐）** —— 每个任务派一个新的 subagent，任务之间做 review，迭代快

**2. Inline Execution** —— 在当前会话里按 executing-plans 批量执行，带检查点

---

## 端到端验证记录

**执行时间**：2026-09-07。**起点 commit**：`0fa5437`。**二进制**：`go build -o <tmp>/bin/agent.exe ./cmd/agent`（真实构建产物，未用 `go test` 夹具替代）。完整记录（含每条命令的完整输出）在 `.superpowers/sdd/task-7-report.md`。

### 夹具

5 把真钥匙（`agent plugins keygen`）：`dev-registered`（清单里已登记，显示名 `Stardust Studio`）、`dev-revoked`（清单里已撤销）、`dev-unknown`（不在任何信任集 → 判定 `unsigned`）、`local-only`（本地 keyring 唯一那把）、`spare-2026`（备用；撤销 `dev-registered` 后若清单里钥匙全被撤销，`sign.ParseKeyring` 会整份拒绝）。三个包分别用真实 `agent plugins sign` 签，打 tar.gz 经 `http://127.0.0.1:18410` 提供（配置开 `allow_insecure_sources`）。`require_signature` 不写 → 默认 true。

**一处与「完全真机」的偏差**：清单的**取回**没走网络。`trustlist.fetchBytes` 硬性要求 https，本机没有 Go 的 Windows 平台校验器会接受的 https 站点，而把自签 CA 装进系统根存储属于修改系统安全设置，未做。替代做法是**直接写缓存目录的三个文件**（`trustlist.json` / `trustlist.sig` / `revoked-ever.json`）——那正是 `Store.Current()` 每次读的三个文件，而 `cache.read` 对它们走的是与网络路径**完全相同**的 `VerifyDocument`（内嵌 root 公钥验签）。清单本身用真实 root 私钥经 `agent plugins trustlist sign` 签，并用 `agent plugins trustlist show --cache` 复核被本仓验签路径接受。所以「信任集每次挂载动态读」这一段是真验的；没验的是 fetch/serial 比较那一段（S1 已验）。

### 七步结果

| 步骤 | 结果 |
|---|---|
| 1 造三个包 | 如预期（+发现 A） |
| 2 逐个 install（四种行为） | **四条全中**（+发现 B） |
| 3 核 `plugins.json` | 如预期 |
| 4 起 serve 看挂载日志 | 如预期 |
| 5 改一个字节再起一次 | 如预期 |
| 6 撤销一把正在用的钥匙，不重启 serve | **部分如预期**（+发现 C，本次唯一的实现缺陷） |
| 7 写进计划 | 本节 |

**Step 2** 四条实际输出的要点：已登记的包报 `endorsed by "Stardust Studio" (key "dev-registered")`（显示名只可能来自清单那一半，所以这一行同时证明并集信任集与 publishers 名录都接上了）；未签名不带 flag 被拒且给出 `--accept-unsigned` 与摘要 `sha256:e42d35cc…`；带 flag 装上并说明记了哪份摘要；被撤销的钥匙签的包硬拒，带撤销时间 `2026-09-01T08:00:00Z` 与理由，并明说「`--accept-unsigned` 不适用」。

**Step 3**：`accepted_unsigned` 确实写进了未签名那条，且**逐字等于**对同一目录 `plugin.json` 独立复算的 `sha256:e42d35cc…`；已登记那条**没有**这个字段。「读得回但写不出」没有复发。

**Step 4**：三条日志齐全 —— `plugin package is endorsed by a registered publisher`（带 key_id 与 publisher）、`plugin package is unendorsed and is admitted on an install-time acceptance`（带 accepted_digest）、`plugin activation failed … signed by key "dev-revoked", which this deployment revoked at … : plugin package is signed by a revoked key`。拒绝粒度按 §4.3：一个条目失败，另两个照常挂上。`GET /v1/plugins` 三个信任字段全部有值，**`trust_publisher` 不是死字段**（`"Stardust Studio"`）。

**Step 5**：改缓存里 `plugin.json` 的一个字符（长度不变，`sha256` 字段未动，wasm 校验仍通过），重启后错误主句是 `changed since it was accepted: the acceptance on record covers sha256:e42d35cc…, and the package in … hashes to sha256:758c339e… These are not the bytes anyone approved` —— **说的是「这个包变了」，两个摘要都印出来了**，不是「没人认过它」。

**Step 6 —— 怎么在不重启 serve 的前提下触发收敛**：`agent plugins reload` 需要**本进程**的 loader（`requirePluginLoader`），另起一个进程只会得到 "no plugin loader in this process"，不能用来收敛已在跑的 serve。可用触发点是 HTTP 侧：`POST /v1/plugins/{name}/grant` 会走 `PluginConsentService.applyAndReport` → `Loader.Apply`，即一次完整收敛；而 `prepare` 里 `trustSet()` 与 `admit()` 都在指纹比较**之前**，所以未变更的条目也会被重新判定。触发对象选**另一个**插件（`legion-e2e-plugin`），观察对象是 `legion-test-plugin`，两者分开。先做了**对照实验**（清单不变时同样的 grant 不改变任何判定）排除触发动作本身。serve 全程同一 pid。

### 三条计划外发现

- **A（跨包接缝，中）**：**「真·未签名」的包经 `plugins install` 不可达**。`install` 只装远端包，而 `fetch.Unpack` 的 layout 契约要求归档里恰好有 `plugin.json`/`plugin.wasm`/`plugin.sig` 三个文件，缺 `plugin.sig` 在**解包**就被拒（实测 `archive is missing required file "plugin.sig"`），走不到三态判定。于是 `ProvenanceUnsigned` 文档里说的两种输入，经 install 只有「签了但钥匙不在信任集」这一种可达。又因为 `AcceptedUnsigned` 只有 `install` 会写，一个真正没签名的**本地**包在 `require_signature: true` 下永远拿不到接受记录、永远挂不上。不是 S2 引入的缺陷，但 install 帮助文本与 spec §二 的「未签名」措辞盖住了一个不可达的形状。**只记录，未改**，待判断：改措辞 / 放宽 unpack 契约 / 记为已知边界。
- **B（次要）**：**被硬拒的包，字节仍留在缓存里**。`install` 的顺序是 `Fetch` → `Cache.Put` → `LoadPackage` → `admitInstalledProvenance`，撤销判定在落盘之后且没有清理；`loader.prepare` 侧的 `fetch.EvictUntrusted` 只覆盖 `ErrUntrustedPackage`，同样不覆盖 `ErrRevokedPublisher`。没有任何路径会挂载它（`plugins.json` 里没有条目），但被撤销的代码留在了一个部署会去读的目录里。**只记录，未改**。
- **C（实现缺陷，本次最值钱的一条）**：**撤销到达时，已经挂载的实例不会下线**。Step 6 里 `legion-test-plugin` 的判定确实变成了 `revoked`（日志带撤销时间与理由，动态读成立），但 `GET /v1/plugins` 仍然是 `state: "loaded"`、`tools: ["echo_tool"]` —— `Loader.Status()` 的 `StateLoaded` 唯一来源是 `l.instances`，所以那个 wasm 实例真的还在，工具还在注册表里。根因在 `converge` pass 2：`prepare` 失败的条目不产生 plan，`planFor[name] == nil` 且该条目仍是 `desired`，于是命中 `case desired[name]: continue`，既不替换也不卸载。`prepare` 的注释把这条解释成「新包坏了不该拆掉正在跑的旧实例」——那个理由对「包坏了」成立，对「信任被撤回了」不成立。与本 spec 直接冲突两处：§4.3 写「它自己的工具消失」（实测没消失）；§5.1 写撤销「无需重启 serve」即生效（对未挂载的成立，对**已经在跑的**不成立，实际要等 serve 重启——而紧急撤销要救的恰恰是正在跑的那个）。§九「明确不做」里没有列这一条。附带症状：`PluginView` 自相矛盾（`state: loaded` 同时挂着一句解释为什么被拒绝的 `detail`），GUI 上会同时显示「运行中」与「已撤销」。**按任务约束只记录未修**：修法有分歧点（是否打断正在用该工具的任务、复用 `suspend` 还是硬 unload、`unload` 失败怎么办），需先拍板。
