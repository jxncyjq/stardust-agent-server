# 插件信任清单联网分发（S1）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把一份由 root 私钥签名、防回滚、撤销永不遗忘的插件信任清单送到用户机器上，并解析成内存里的 `*sign.Keyring` + 发布者名录。

**Architecture:** 新包 `internal/plugin/trustlist`，它只认识 `internal/plugin/sign`、`net/http` 和文件系统，**不 import `manifest`/`loader`/`host`**。清单文档是一个信封，把现有 keyring 文档以 `json.RawMessage` 原样嵌在里面——验签验的字节与解析信任集的字节是同一段，`internal/plugin/sign` 一行不改。root 公钥用 `go:embed` 嵌进二进制；清单本身放 GitHub 仓库文件，可变，靠 root 签名保护。

**Tech Stack:** Go 1.26.0，module `github.com/stardust/legion-agent`。crypto/ed25519（经 `internal/plugin/sign`）、`net/http`、`embed`、cobra（CLI）、`net/http/httptest`（测试）。

**Spec:** `docs/superpowers/specs/2026-09-05-plugin-trustlist-distribution-design.md`。有歧义时以 spec 为准，spec 没写的以本计划为准。

## Global Constraints

- **fail-loud 是本仓的铁律**：不许有回落到零值的分支、不许吞错误、不许「拿不到就当没配置」。安全开关的未声明值取安全的那一侧。
- **注释是契约**：注释里的事实陈述必须与代码一致。上一期光「注释里的事实陈述不实」就栽了 5 次。写「因为 X 所以 Y」之前先核 X。
- `internal/plugin/sign` **一行不改**。本包只调用它的导出 API。
- 本包**不得** import `internal/plugin/manifest`、`internal/plugin/loader`、`internal/plugin/host`。它不知道插件是什么。
- 清单 URL **必须是 https**，且不受 `plugins.allow_insecure_sources` 影响。
- 上限：清单文档 **1 MiB**（`1 << 20`）；签名文档 **4 KiB**（`4 << 10`）；单次取回超时 **30 秒**；默认刷新间隔 **21600000 毫秒（6 小时）**。
- 撤销**单调**：见过一次就永久生效，与清单是否过期、后续清单里是否还有它无关。登记**不单调**。
- `serial` 严格单调：收到的清单 `serial` 小于本地已见值 → 拒绝，不给任何回落；等于且内容不同 → 拒绝。
- 错误信息里必须带 HTTP 状态码：GitHub 对带无效凭据的请求返回 **404 而不是 401**，状态码缺失会让鉴权问题伪装成「文件不存在」。
- 每个任务结束时 `gofmt -l .` 必须为空、`go vet ./...` 必须干净。
- 测试**不打真实网络**，一律用 `httptest`。
- 提交信息用中文正文，结尾带 `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`。

## 一个必须正视的风险

memory 里记着一条教训：**「机制交付了但没有调用方」会藏住任何东西**——上一期内置浏览器的安装机制交付了两个周期，其中一个是安全缺陷，因为前端从来没有调用它，没人发现它从未工作过。

S1 的消费者（分级安装体验）在 S2，所以本计划**必须**自带真实调用方，否则会重蹈覆辙。Task 7 的 `agent plugins trustlist refresh` / `show` 就是那个调用方：它端到端跑完取回→验签→serial→撤销累积→落盘→装配的全路径。**Task 9 的真机验证是本计划的验收条件，不是可选项。**

## File Structure

| 文件 | 职责 |
|------|------|
| `internal/plugin/trustlist/document.go` | 信封文档的类型、解析规则、验签；内嵌 root keyring |
| `internal/plugin/trustlist/root_keys.json` | 内嵌的 root 公钥（keyring 文档格式） |
| `internal/plugin/trustlist/revoked.go` | 撤销累积集：读、并入、编码，以及与清单合并成最终 keyring |
| `internal/plugin/trustlist/cache.go` | 缓存三文件的读写、原子落盘、损坏检测 |
| `internal/plugin/trustlist/cache_lock.go` | 跨平台的锁争用判据（与 `fetch` 同形，理由见 Task 3） |
| `internal/plugin/trustlist/cache_lock_windows.go` | Windows 的 delete-pending 折叠 |
| `internal/plugin/trustlist/cache_lock_other.go` | 非 Windows：恒 false |
| `internal/plugin/trustlist/fetch.go` | HTTP 取回：https 强制、大小上限、超时、重定向、非 2xx |
| `internal/plugin/trustlist/store.go` | `Store`/`Trust`/`Status`/哨兵；`Current`、`Refresh` |
| `internal/config/config.go`（改） | `PluginTrustlistConfig` 与它的校验 |
| `internal/cli/plugins_trustlist_command.go` | `agent plugins trustlist sign/refresh/show` |
| `internal/cli/plugins_command.go`（改 3 行） | 注册 `trustlist` 子命令 |

`plugins_command.go` 已经 2200+ 行，trustlist 的 CLI 另开文件；只在它里面加一行 `AddCommand`。

---

## Task 0（人工前置，不能由 subagent 代劳）：生成 root 密钥对

root 私钥是这整套机制的信任根。它**只能在本机生成、只能留在本机**，不进仓库、不进 CI、不上任何联网主机。所以这一步由人跑，不派给 subagent——一个 agent 在临时目录里生成生产环境的信任根私钥，是本设计明确要避免的事。

- [ ] **Step 1: 生成 root 密钥对**

```bash
go run ./cmd/agent plugins keygen --key-id root-2026 --private-key "$HOME/.legion/root-key.json"
```

预期：私钥写到 `~/.legion/root-key.json`（0600），标准输出打印一条 keyring 条目，形如：

```json
{
  "id": "root-2026",
  "algorithm": "ed25519",
  "public_key": "<base64 32 字节>"
}
```

- [ ] **Step 2: 把公钥条目写成内嵌文件**

创建 `internal/plugin/trustlist/root_keys.json`，把上一步打印的条目放进 `keys` 数组：

```json
{
  "keys": [
    {
      "id": "root-2026",
      "algorithm": "ed25519",
      "public_key": "<粘贴上一步打印的 public_key>"
    }
  ]
}
```

- [ ] **Step 3: 确认私钥没有进仓库**

```bash
git status --short && git check-ignore -v "$HOME/.legion/root-key.json" ; echo "exit=$?"
```

预期：`git status` 里只有 `root_keys.json` 一个新文件（`.legion` 在家目录，本来就不在仓库里）。**如果 `root-key.json` 出现在 `git status` 里，立刻停下**——它被放错地方了。

- [ ] **Step 4: 提交**

```bash
git add internal/plugin/trustlist/root_keys.json
git commit -m "$(cat <<'EOF'
feat(trustlist): 内嵌 root 公钥

信任清单的信任根。只有公钥进仓库；对应的私钥留在本机
~/.legion/root-key.json，永不进仓库、不进 CI、不上联网主机。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 1: 信封文档的解析与验签

**Files:**
- Create: `internal/plugin/trustlist/document.go`
- Test: `internal/plugin/trustlist/document_test.go`
- Consumes: `internal/plugin/trustlist/root_keys.json`（Task 0 已创建）

**Interfaces:**
- Produces:
  - `type Publisher struct { KeyID sign.KeyID; DisplayName string; Contact string }`
  - `type Document struct { Serial int64; IssuedAt, ExpiresAt time.Time; KeyringRaw json.RawMessage; Publishers map[sign.KeyID]Publisher; Raw []byte }`
  - `func ParseDocument(data []byte) (Document, error)`
  - `func VerifyDocument(listData, sigData []byte) (Document, error)`
  - `var ErrUntrustedList = errors.New("trustlist is not trusted")`
  - `func rootKeyring() *sign.Keyring`（包内）

- [ ] **Step 1: 写失败的测试**

创建 `internal/plugin/trustlist/document_test.go`：

```go
package trustlist

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// testDoc 组装一份形状正确的清单文档，供各用例按需改坏其中一处。
// 每个用例只改一处，其余保持合法——否则一条测试红了说明不了是哪条规则在起作用。
func testDoc(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	pub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	entry, err := sign.MarshalKeyEntry("dev-abc", pub)
	if err != nil {
		t.Fatalf("MarshalKeyEntry: %v", err)
	}
	var entryMap map[string]any
	if err := json.Unmarshal(entry, &entryMap); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	m := map[string]any{
		"serial":     float64(7),
		"issued_at":  "2026-09-05T02:00:00Z",
		"expires_at": "2026-10-05T02:00:00Z",
		"keyring": map[string]any{
			"keys": []any{entryMap},
		},
		"publishers": []any{
			map[string]any{"key_id": "dev-abc", "display_name": "张三", "contact": "z@example.com"},
		},
	}
	if mutate != nil {
		mutate(m)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	return data
}

func TestParseDocument_Accepts_AWellFormedDocument(t *testing.T) {
	t.Parallel()

	doc, err := ParseDocument(testDoc(t, nil))
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	if doc.Serial != 7 {
		t.Errorf("Serial = %d, want 7", doc.Serial)
	}
	if got := doc.Publishers["dev-abc"].DisplayName; got != "张三" {
		t.Errorf("Publishers[dev-abc].DisplayName = %q, want 张三", got)
	}
	// KeyringRaw 必须能被 sign.ParseKeyring 原样吃下：这是整个信封设计的
	// 全部理由——验签验的字节与装配信任集的字节是同一段。
	if _, err := sign.ParseKeyring(doc.KeyringRaw); err != nil {
		t.Errorf("sign.ParseKeyring(doc.KeyringRaw): %v", err)
	}
}

// TestParseDocument_RefusesADanglingPublisher：publishers 里出现 keys 中不存在的
// key_id，意味着这份清单是手拼的，或指向了一把已经删掉的钥匙。整份拒绝，
// 而不是丢弃那一条——丢弃会让「谁发布了这个插件」在 S2 里静默变成「未知」。
func TestParseDocument_RefusesADanglingPublisher(t *testing.T) {
	t.Parallel()

	data := testDoc(t, func(m map[string]any) {
		m["publishers"] = []any{
			map[string]any{"key_id": "nobody", "display_name": "查无此人"},
		}
	})
	err := ParseDocument(data)
	if err == nil {
		t.Fatal("悬空的 publisher 被接受了")
	}
	if !strings.Contains(err.Error(), "nobody") {
		t.Errorf("错误里没点名是哪个 key_id：%v", err)
	}
}

func TestParseDocument_RefusesMalformedDocuments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(m map[string]any)
		want   string
	}{
		{"serial 为 0", func(m map[string]any) { m["serial"] = float64(0) }, "serial"},
		{"serial 为负", func(m map[string]any) { m["serial"] = float64(-1) }, "serial"},
		{"issued_at 不是 RFC3339", func(m map[string]any) { m["issued_at"] = "2026/09/05" }, "issued_at"},
		{"expires_at 早于 issued_at", func(m map[string]any) {
			m["expires_at"] = "2026-08-05T02:00:00Z"
		}, "expires_at"},
		{"未知字段", func(m map[string]any) { m["surprise"] = "x" }, "surprise"},
		{"没有 keyring", func(m map[string]any) { delete(m, "keyring") }, "keyring"},
		{"publisher 显示名为空", func(m map[string]any) {
			m["publishers"] = []any{map[string]any{"key_id": "dev-abc", "display_name": ""}}
		}, "display_name"},
		{"publisher 重复", func(m map[string]any) {
			p := map[string]any{"key_id": "dev-abc", "display_name": "张三"}
			m["publishers"] = []any{p, p}
		}, "dev-abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ParseDocument(testDoc(t, tc.mutate))
			if err == nil {
				t.Fatalf("%s 被接受了", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误里没提到 %q：%v", tc.want, err)
			}
		})
	}
}

func TestParseDocument_RefusesTrailingContent(t *testing.T) {
	t.Parallel()

	data := append(testDoc(t, nil), []byte(`{"serial":8}`)...)
	if err := ParseDocument(data); err == nil {
		t.Fatal("第二个文档被静默忽略了")
	}
}

// TestVerifyDocument_AcceptsOnlyTheRootKey 是这一层存在的全部理由。
func TestVerifyDocument_AcceptsOnlyTheRootKey(t *testing.T) {
	t.Parallel()

	list := testDoc(t, nil)
	_, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	// 用一把不是 root 的钥匙签，且用 root keyring 里那个 id——冒充。
	sig, err := sign.Sign(priv, rootKeyID(t), list)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	sigData, err := sign.MarshalSignature(sig)
	if err != nil {
		t.Fatalf("MarshalSignature: %v", err)
	}
	_, err = VerifyDocument(list, sigData)
	if err == nil {
		t.Fatal("用非 root 的钥匙签的清单被接受了")
	}
	if !errors.Is(err, ErrUntrustedList) {
		t.Errorf("错误没裹 ErrUntrustedList：%v", err)
	}
}

func TestVerifyDocument_RefusesATamperedList(t *testing.T) {
	t.Parallel()

	list := testDoc(t, nil)
	priv := testRootPrivateKey(t)
	sig, err := sign.Sign(priv, rootKeyID(t), list)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	sigData, err := sign.MarshalSignature(sig)
	if err != nil {
		t.Fatalf("MarshalSignature: %v", err)
	}
	tampered := append([]byte(nil), list...)
	tampered[len(tampered)/2] ^= 0x01

	if _, err := VerifyDocument(tampered, sigData); !errors.Is(err, ErrUntrustedList) {
		t.Errorf("篡改一个字节后的清单：err = %v，want 裹 ErrUntrustedList", err)
	}
}

// TestRootKeyringParses：内嵌的 root keyring 必须能解析。写坏它的后果是
// 所有用户机器同时拒绝所有清单，而常规 CI 不会告诉你——除非有这条。
func TestRootKeyringParses(t *testing.T) {
	t.Parallel()

	kr := rootKeyring()
	if kr == nil {
		t.Fatal("rootKeyring() 返回 nil")
	}
	if len(kr.IDs()) == 0 {
		t.Fatal("内嵌的 root keyring 里一把钥匙都没有")
	}
}

// --- 测试用的 root 私钥注入 ---------------------------------------------
//
// 生产的 root 私钥不在仓库里（按设计），所以需要签一份能验过的清单时，
// 测试自己换掉内嵌的 root keyring。swapRootKeyring 在用例结束时还原。

func testRootPrivateKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	data, err := sign.MarshalKeyring("test-root", pub)
	if err != nil {
		t.Fatalf("MarshalKeyring: %v", err)
	}
	kr, err := sign.ParseKeyring(data)
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	swapRootKeyring(t, kr, "test-root")
	return priv
}

func rootKeyID(t *testing.T) sign.KeyID {
	t.Helper()
	ids := rootKeyring().IDs()
	if len(ids) == 0 {
		t.Fatal("root keyring 是空的")
	}
	return ids[0]
}
```

注意这些用例用到 `swapRootKeyring`，它是下一步要实现的测试钩子。

- [ ] **Step 2: 跑测试确认它失败**

```bash
go test ./internal/plugin/trustlist/ -run TestParseDocument -v
```

预期：编译失败，`undefined: ParseDocument`。

- [ ] **Step 3: 实现**

创建 `internal/plugin/trustlist/document.go`：

```go
// Package trustlist 取回、验证并缓存这个部署信任哪些插件签名钥匙的官方清单。
//
// 清单是一个信封：它自己带着发布元数据（第几版、什么时候签的、什么时候过期、
// 每把钥匙属于谁），而信任集本身是一份**原样嵌在里面的 keyring 文档**——与
// plugins.keyring 指向的本地文件逐字同构，由 internal/plugin/sign.ParseKeyring
// 解析。嵌成 json.RawMessage 而不是解析后再重新编码，是因为验签验的是清单的
// 原始字节：re-marshal 会让「被签名保护的那段字节」与「装配出信任集的那段字节」
// 成为两份东西，字段顺序或数字格式的任何差异都会让二者悄悄分家，而那正是签名
// 要消灭的不确定性。
//
// 这个包不知道插件是什么。它不 import manifest、loader 或 host，只认识 sign、
// net/http 和文件系统。谁在什么时候拿它返回的信任集去判一个插件，是调用方的事。
package trustlist

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// ErrUntrustedList 标记「这份清单文档不可信」的那一类失败：验签不过、格式非法、
// publisher 悬空。
//
// 它是哨兵，因为调用方必须把它与网络故障、文件 I/O 故障分开：那两类重试是有
// 意义的，而这一类重试永远不会变好，且值得高得多的告警级别——它意味着有人
// 在喂一份不是我签的清单。
var ErrUntrustedList = errors.New("trustlist is not trusted")

// maxPublishers 是一份清单里 publishers 条目数的上限。
//
// 它不是性能保护（1 MiB 的文档上限已经管住了体积），而是一条「这份文档不像是
// 我签的」的判据：真实的登记规模远小于这个数，而一份塞满条目的文档更像是有人
// 在试探解析器。
const maxPublishers = 10000

//go:embed root_keys.json
var rootKeysJSON []byte

var (
	rootOnce    sync.Once
	rootTrust   *sign.Keyring
	rootOverride *sign.Keyring // 仅测试通过 swapRootKeyring 设置
)

// rootKeyring 返回内嵌的 root 信任集——判断一份清单是不是我签的，全靠它。
//
// 解析失败直接 panic：内嵌数据在编译期就该是对的，而写坏它的后果是所有用户
// 机器同时拒绝所有清单。运行期把它降级成「没有 root」没有任何安全含义上的
// 意义——那等于关掉整套机制。
func rootKeyring() *sign.Keyring {
	if rootOverride != nil {
		return rootOverride
	}
	rootOnce.Do(func() {
		kr, err := sign.ParseKeyring(rootKeysJSON)
		if err != nil {
			panic(fmt.Sprintf("trustlist: 内嵌的 root_keys.json 无法解析：%v", err))
		}
		rootTrust = kr
	})
	return rootTrust
}

// Publisher 是一个已登记发布者的展示信息：这个包只校验它的完整性，不使用它。
type Publisher struct {
	KeyID       sign.KeyID
	DisplayName string
	Contact     string
}

// Document 是一份已经通过格式校验的清单。
//
// Raw 保留原始字节，因为落盘时写的必须是这一段（而不是重新编码的结果）——
// 缓存读回来时要走与网络路径完全相同的验签代码。
type Document struct {
	Serial     int64
	IssuedAt   time.Time
	ExpiresAt  time.Time
	KeyringRaw json.RawMessage
	Publishers map[sign.KeyID]Publisher
	Raw        []byte
}

type rawPublisher struct {
	KeyID       sign.KeyID `json:"key_id"`
	DisplayName string     `json:"display_name"`
	Contact     string     `json:"contact"`
}

type rawDocument struct {
	Serial     int64           `json:"serial"`
	IssuedAt   string          `json:"issued_at"`
	ExpiresAt  string          `json:"expires_at"`
	Keyring    json.RawMessage `json:"keyring"`
	Publishers []rawPublisher  `json:"publishers"`
}

// ParseDocument 解析并校验一份清单文档的**形状**。它不验签——那是
// VerifyDocument 的事，且 VerifyDocument 先验签再调这里。
//
// 拒绝的情况，每一条都对应一种「这份文档不是我签的那一份」或「发布侧出了事故」：
//
//   - 任意嵌套层级上出现未知字段（DisallowUnknownFields）。写错的字段名必须
//     解析失败，不能悄悄不生效；
//   - 单个 JSON 文档之后还有非空白内容；
//   - serial 不是 ≥ 1 的整数。0 与负数是错误，不是「未设置」；
//   - issued_at / expires_at 不是 RFC 3339，或 expires_at 不晚于 issued_at；
//   - keyring 字段缺失，或不能通过 sign.ParseKeyring（它自己会拒绝空的 keys、
//     拒绝「每把钥匙都被撤销」，那些错误原样往上冒）;
//   - publishers 条目数超过 maxPublishers；
//   - 某个 publisher 的 key_id 不在 keyring 的 keys 里（悬空）；
//   - 某个 publisher 的 display_name 为空；
//   - 同一个 key_id 出现两次。
//
// 每条错误都点名出问题的 key_id 或字段，而不是只说「解析失败」。
//
// 允许的一种情况值得单说：**已撤销的 key 可以保留 publisher 条目**。调用方要能
// 说出「张三的这把钥匙已被撤销」，而不是退化成「未知钥匙」。
func ParseDocument(data []byte) (Document, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw rawDocument
	if err := dec.Decode(&raw); err != nil {
		return Document{}, fmt.Errorf("parse trustlist: %w", err)
	}
	if dec.More() {
		return Document{}, fmt.Errorf("parse trustlist: unexpected content after the JSON document")
	}
	if raw.Serial < 1 {
		return Document{}, fmt.Errorf("parse trustlist: serial is %d; it must be at least 1 "+
			"(0 is not 'unset' here — it is the value a rollback check compares against)", raw.Serial)
	}
	issued, err := time.Parse(time.RFC3339, raw.IssuedAt)
	if err != nil {
		return Document{}, fmt.Errorf("parse trustlist: issued_at %q is not RFC 3339: %w", raw.IssuedAt, err)
	}
	expires, err := time.Parse(time.RFC3339, raw.ExpiresAt)
	if err != nil {
		return Document{}, fmt.Errorf("parse trustlist: expires_at %q is not RFC 3339: %w", raw.ExpiresAt, err)
	}
	if !expires.After(issued) {
		return Document{}, fmt.Errorf("parse trustlist: expires_at %s is not after issued_at %s",
			expires.Format(time.RFC3339), issued.Format(time.RFC3339))
	}
	if len(raw.Keyring) == 0 {
		return Document{}, fmt.Errorf("parse trustlist: keyring is missing; a trustlist with no trust set " +
			"carries nothing this deployment could act on")
	}
	keyring, err := sign.ParseKeyring(raw.Keyring)
	if err != nil {
		return Document{}, fmt.Errorf("parse trustlist: %w", err)
	}
	if len(raw.Publishers) > maxPublishers {
		return Document{}, fmt.Errorf("parse trustlist: %d publishers, at most %d "+
			"(a document this large does not look like one this project signed)",
			len(raw.Publishers), maxPublishers)
	}

	known := make(map[sign.KeyID]bool, len(keyring.IDs()))
	for _, id := range keyring.IDs() {
		known[id] = true
	}
	publishers := make(map[sign.KeyID]Publisher, len(raw.Publishers))
	for i, p := range raw.Publishers {
		if p.KeyID == "" {
			return Document{}, fmt.Errorf("parse trustlist: publishers[%d] has no key_id", i)
		}
		if p.DisplayName == "" {
			return Document{}, fmt.Errorf("parse trustlist: publisher %q has an empty display_name; "+
				"it is the name a human reads while deciding whether to trust a package", p.KeyID)
		}
		if !known[p.KeyID] {
			return Document{}, fmt.Errorf("parse trustlist: publisher %q names a key that is not in the "+
				"keyring; a dangling display name means this document was assembled by hand", p.KeyID)
		}
		if _, dup := publishers[p.KeyID]; dup {
			return Document{}, fmt.Errorf("parse trustlist: publisher %q appears twice; "+
				"'who published this' must have one answer", p.KeyID)
		}
		publishers[p.KeyID] = Publisher{KeyID: p.KeyID, DisplayName: p.DisplayName, Contact: p.Contact}
	}

	return Document{
		Serial:     raw.Serial,
		IssuedAt:   issued,
		ExpiresAt:  expires,
		KeyringRaw: raw.Keyring,
		Publishers: publishers,
		Raw:        append([]byte(nil), data...),
	}, nil
}

// VerifyDocument 先用内嵌的 root 信任集验 sigData 对 listData 的签名，通过之后
// 才解析 listData。
//
// 顺序是刻意的：先验签意味着解析器只会看到我签过的字节，一份构造得刁钻的文档
// 连解析都轮不到。所有失败——签名文档本身格式不对、key_id 不是 root、签名不
// 匹配、清单形状不合法——都裹 ErrUntrustedList，因为它们对调用方是同一个意思：
// 这份东西不可信，重试没用。
func VerifyDocument(listData, sigData []byte) (Document, error) {
	sig, err := sign.ParseSignature(sigData)
	if err != nil {
		return Document{}, fmt.Errorf("parse trustlist signature: %w: %w", ErrUntrustedList, err)
	}
	if err := rootKeyring().Verify(sig, listData); err != nil {
		return Document{}, fmt.Errorf("verify trustlist signature: %w: %w", ErrUntrustedList, err)
	}
	doc, err := ParseDocument(listData)
	if err != nil {
		return Document{}, fmt.Errorf("%w: %w", ErrUntrustedList, err)
	}
	return doc, nil
}
```

再创建测试钩子 `internal/plugin/trustlist/export_test.go`（`_test.go` 后缀保证它不进生产二进制）：

```go
package trustlist

import (
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// swapRootKeyring 让一个用例临时把内嵌的 root 信任集换成它自己生成的一把，
// 并在用例结束时还原。
//
// 生产的 root 私钥按设计不在仓库里，所以测试无法用真的 root 签任何东西。
// 换掉信任集是唯一能端到端跑通「签 → 验」的办法，而它只存在于 _test.go 里，
// 生产路径没有任何入口能改动 root。
func swapRootKeyring(t *testing.T, kr *sign.Keyring, _ sign.KeyID) {
	t.Helper()
	previous := rootOverride
	rootOverride = kr
	t.Cleanup(func() { rootOverride = previous })
}
```

注意：`rootOverride` 是包级变量，所以用到 `swapRootKeyring` 的用例**不能** `t.Parallel()`。上面测试里 `TestVerifyDocument_RefusesATamperedList` 和 `TestVerifyDocument_AcceptsOnlyTheRootKey` 调了它——把这两个用例的 `t.Parallel()` 删掉。

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/plugin/trustlist/ -v
```

预期：全部 PASS。

- [ ] **Step 5: 变异验证——把 publisher 悬空检查删掉，确认必红**

临时把 `if !known[p.KeyID] { ... }` 整块注释掉，跑：

```bash
go test ./internal/plugin/trustlist/ -run TestParseDocument_RefusesADanglingPublisher
```

预期：**FAIL**。确认后把代码改回来，再跑一次确认 PASS。

（这一步不是仪式：本仓上一期栽过一次「测了实现没测接线」，注入变异是分辨「测试真的在管这条规则」与「测试恰好绿」的唯一办法。）

- [ ] **Step 6: 提交**

```bash
gofmt -l . && go vet ./internal/plugin/trustlist/
git add internal/plugin/trustlist/
git commit -m "$(cat <<'EOF'
feat(trustlist): 清单信封的解析与 root 验签

信封把现有的 keyring 文档以 json.RawMessage 原样嵌在里面，验签验的字节
与装配信任集的字节是同一段——re-marshal 会让二者悄悄分家。

VerifyDocument 先验签再解析：解析器只会看到 root 签过的字节。所有不可信
的失败都裹 ErrUntrustedList，与网络/IO 故障分开——后者重试有意义，前者
永远不会变好。

内嵌 root keyring 解析失败直接 panic：那是编译期就该正确的数据，降级等于
关掉整套机制。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: 撤销累积集与信任集装配

**Files:**
- Create: `internal/plugin/trustlist/revoked.go`
- Test: `internal/plugin/trustlist/revoked_test.go`

**Interfaces:**
- Consumes: `Document`（Task 1）
- Produces:
  - `type revokedSet struct { ... }`（包内）
  - `func parseRevokedSet(data []byte) (*revokedSet, error)`
  - `func newRevokedSet() *revokedSet`
  - `func (s *revokedSet) mergeFrom(keyringRaw json.RawMessage) error`
  - `func (s *revokedSet) marshal() ([]byte, error)`
  - `func (s *revokedSet) len() int`
  - `func assembleKeyring(keyringRaw json.RawMessage, revoked *revokedSet) (*sign.Keyring, error)`

- [ ] **Step 1: 写失败的测试**

创建 `internal/plugin/trustlist/revoked_test.go`：

```go
package trustlist

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// keyringWith 造一份 keyring 文档：keys 里有 ids 列出的每一把，revoked 里有
// revoked 列出的每一个。
func keyringWith(t *testing.T, ids []sign.KeyID, revoked []sign.KeyID) json.RawMessage {
	t.Helper()
	keys := make([]any, 0, len(ids))
	for _, id := range ids {
		pub, _, err := sign.GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		entry, err := sign.MarshalKeyEntry(id, pub)
		if err != nil {
			t.Fatalf("MarshalKeyEntry: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(entry, &m); err != nil {
			t.Fatalf("unmarshal entry: %v", err)
		}
		keys = append(keys, m)
	}
	doc := map[string]any{"keys": keys}
	if len(revoked) > 0 {
		rs := make([]any, 0, len(revoked))
		for _, id := range revoked {
			rs = append(rs, map[string]any{
				"key_id":     string(id),
				"revoked_at": "2026-08-29T10:00:00Z",
				"reason":     "私钥泄漏",
			})
		}
		doc["revoked"] = rs
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal keyring: %v", err)
	}
	return data
}

// TestRevocationIsNeverForgotten 是这个文件存在的全部理由。
//
// 清单 A 撤销了 K。后来的清单 B（serial 更大、完全合法）的 revoked 里没有 K,
// keys 里也没有 K——发布侧误删，或者被诱导删掉。K 必须**仍然**被拒。
//
// 没有这条不变量，「把机器断网」或「诱导一次误删」就能让任何撤销失效，
// 而撤销正是这整套机制唯一的止血手段。
func TestRevocationIsNeverForgotten(t *testing.T) {
	t.Parallel()

	set := newRevokedSet()
	listA := keyringWith(t, []sign.KeyID{"dev-k", "dev-other"}, []sign.KeyID{"dev-k"})
	if err := set.mergeFrom(listA); err != nil {
		t.Fatalf("mergeFrom(A): %v", err)
	}
	// B 里 K 彻底消失了。
	listB := keyringWith(t, []sign.KeyID{"dev-other"}, nil)
	if err := set.mergeFrom(listB); err != nil {
		t.Fatalf("mergeFrom(B): %v", err)
	}

	kr, err := assembleKeyring(listB, set)
	if err != nil {
		t.Fatalf("assembleKeyring: %v", err)
	}
	rev, ok := kr.Revoked("dev-k")
	if !ok {
		t.Fatal("dev-k 在 B 之后不再是撤销状态——撤销被遗忘了")
	}
	// 理由与时间也要留着：sign.Keyring 用它们生成人能读的拒绝理由，
	// 丢掉就退化成「未知钥匙」。
	if rev.Reason != "私钥泄漏" {
		t.Errorf("Revoked(dev-k).Reason = %q, want 私钥泄漏", rev.Reason)
	}
	if rev.At.IsZero() {
		t.Error("Revoked(dev-k).At 是零值，撤销时间被丢了")
	}
}

// TestRevokedSetOnlyGrows：并入一份没有任何 revoked 的清单，不会清空累积集。
func TestRevokedSetOnlyGrows(t *testing.T) {
	t.Parallel()

	set := newRevokedSet()
	if err := set.mergeFrom(keyringWith(t, []sign.KeyID{"a"}, []sign.KeyID{"a"})); err != nil {
		t.Fatalf("mergeFrom: %v", err)
	}
	before := set.len()
	if err := set.mergeFrom(keyringWith(t, []sign.KeyID{"b"}, nil)); err != nil {
		t.Fatalf("mergeFrom: %v", err)
	}
	if set.len() < before {
		t.Errorf("累积集从 %d 缩到了 %d", before, set.len())
	}
}

// TestRevokedSetRoundTrips：编码再读回来，内容不变——这是它落盘的形式。
func TestRevokedSetRoundTrips(t *testing.T) {
	t.Parallel()

	set := newRevokedSet()
	if err := set.mergeFrom(keyringWith(t, []sign.KeyID{"a", "b"}, []sign.KeyID{"a"})); err != nil {
		t.Fatalf("mergeFrom: %v", err)
	}
	data, err := set.marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := parseRevokedSet(data)
	if err != nil {
		t.Fatalf("parseRevokedSet: %v", err)
	}
	if back.len() != set.len() {
		t.Errorf("往返后 len = %d, want %d", back.len(), set.len())
	}
}

// TestParseRevokedSetRefusesGarbage：损坏的累积集不能被静默当成空集——
// 空集意味着「什么都没撤销过」，那是这个文件能造成的最坏的谎。
func TestParseRevokedSetRefusesGarbage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, data string }{
		{"截断", `{"revoked":[{"key_id":`},
		{"未知字段", `{"revoked":[],"extra":1}`},
		{"key_id 为空", `{"revoked":[{"key_id":""}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseRevokedSet([]byte(tc.data)); err == nil {
				t.Fatalf("%s 的累积集被接受了", tc.name)
			}
		})
	}
}

// TestAssembleRefusesAnEmptyTrustSet：合并后所有钥匙都被撤销时，
// sign.ParseKeyring 自己那条错误必须原样上报，而不是被吞成一个空信任集。
func TestAssembleRefusesAnEmptyTrustSet(t *testing.T) {
	t.Parallel()

	set := newRevokedSet()
	only := keyringWith(t, []sign.KeyID{"only"}, nil)
	if err := set.mergeFrom(keyringWith(t, []sign.KeyID{"only"}, []sign.KeyID{"only"})); err != nil {
		t.Fatalf("mergeFrom: %v", err)
	}
	_, err := assembleKeyring(only, set)
	if err == nil {
		t.Fatal("每把钥匙都被撤销的信任集被接受了")
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("错误没说清是撤销导致的：%v", err)
	}
	_ = errors.Is(err, nil) // 保持 errors 被使用
}
```

- [ ] **Step 2: 跑测试确认它失败**

```bash
go test ./internal/plugin/trustlist/ -run TestRevocation -v
```

预期：编译失败，`undefined: newRevokedSet`。

- [ ] **Step 3: 实现**

创建 `internal/plugin/trustlist/revoked.go`：

```go
package trustlist

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// revokedSet 是这台机器**见过的全部撤销**，只增不减。
//
// 它存在是为了堵住一个洞：清单过期不作废（否则一断网所有插件立刻失信，
// 可用性代价大于收益），但过期还能用就意味着断网可以绕过撤销——把机器断网，
// 撤销永远到不了。累积集让这件事不成立：一个 key_id 一旦在任何一份被接受的
// 清单里出现在 revoked 中，它在本机就永远是撤销状态，与清单是否过期、
// 后续清单里还有没有它、此后能否联网都无关。
//
// 撤销单调，登记不单调：一把钥匙可以从清单的 keys 里被移除从而不再被信任
// （之后还能加回来），但撤销过就永远撤销。
type revokedSet struct {
	entries map[sign.KeyID]rawRevocationEntry
}

// rawRevocationEntry 与 keyring 文档里 revoked 数组的条目同形，也是
// revoked-ever.json 落盘的形状。
//
// 字段与 sign 包的 revoked 条目逐字一致是必须的：assembleKeyring 会把这些条目
// 拼回一份 keyring 文档交给 sign.ParseKeyring，形状对不上就拼不回去。
type rawRevocationEntry struct {
	KeyID     sign.KeyID `json:"key_id"`
	RevokedAt string     `json:"revoked_at,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

type rawRevokedSet struct {
	Revoked []rawRevocationEntry `json:"revoked"`
}

// keyringShape 是 mergeFrom 从清单的 keyring 段里唯一关心的部分。
// 它只解 revoked：keys 交给 sign.ParseKeyring，这里不重复解析。
type keyringShape struct {
	Keys    json.RawMessage      `json:"keys"`
	Revoked []rawRevocationEntry `json:"revoked"`
}

func newRevokedSet() *revokedSet {
	return &revokedSet{entries: map[sign.KeyID]rawRevocationEntry{}}
}

// parseRevokedSet 从 revoked-ever.json 的字节读回累积集。
//
// 它和这个包里其他解析器一样严格（未知字段、尾随内容、空 key_id 都拒绝），
// 而这里的理由比别处更重：一个被静默当成空集的损坏文件，说的是「这台机器
// 从没见过任何撤销」——这是这个文件能造成的最坏的谎。宁可报错让调用方去
// 决定怎么办。
func parseRevokedSet(data []byte) (*revokedSet, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw rawRevokedSet
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse revoked-ever: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("parse revoked-ever: unexpected content after the JSON document")
	}
	set := newRevokedSet()
	for i, entry := range raw.Revoked {
		if entry.KeyID == "" {
			return nil, fmt.Errorf("parse revoked-ever: revoked[%d] has no key_id", i)
		}
		set.entries[entry.KeyID] = entry
	}
	return set, nil
}

// mergeFrom 把 keyringRaw（清单信封里那段 keyring 文档）的 revoked 条目并进
// 累积集。已经在集合里的 key_id 保留**先见到的那条**记录，不被后来的覆盖：
// 撤销时间与理由是操作者当时写下的事实，后一份清单把它改短、改空或改晚，
// 都只会让拒绝理由变得更没用。
func (s *revokedSet) mergeFrom(keyringRaw json.RawMessage) error {
	var shape keyringShape
	if err := json.Unmarshal(keyringRaw, &shape); err != nil {
		return fmt.Errorf("merge revocations: %w", err)
	}
	for i, entry := range shape.Revoked {
		if entry.KeyID == "" {
			return fmt.Errorf("merge revocations: revoked[%d] has no key_id", i)
		}
		if _, seen := s.entries[entry.KeyID]; seen {
			continue
		}
		s.entries[entry.KeyID] = entry
	}
	return nil
}

// marshal 编码成 revoked-ever.json 的字节，条目按 key_id 排序。
//
// 排序是为了让这个文件可 diff、可复现：一个每次写出来都不一样的文件，
// 无法靠 diff 看出「这次刷新到底新撤销了谁」。
func (s *revokedSet) marshal() ([]byte, error) {
	entries := make([]rawRevocationEntry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].KeyID < entries[j].KeyID })
	data, err := json.MarshalIndent(rawRevokedSet{Revoked: entries}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal revoked-ever: %w", err)
	}
	return append(data, '\n'), nil
}

func (s *revokedSet) len() int { return len(s.entries) }

// assembleKeyring 把清单里的信任集与本机的撤销累积集合并成最终的 *sign.Keyring。
//
// 做法是拼出一份新的 keyring 文档再走一次 sign.ParseKeyring，而**不是**在
// sign.Keyring 外面自己维护第二套撤销判断。理由是后者等于把同一条规则写在两个
// 地方，迟早分家——而分家的方向一定是「有个地方忘了查累积集」，也就是撤销失效。
//
// keys 段原样搬运（json.RawMessage），不重新编码。revoked 段是清单自己的条目
// 与累积集的并集；因为 mergeFrom 在调用这里之前已经把清单的 revoked 并进去了，
// 直接用累积集就是并集。
//
// sign.ParseKeyring 会拒绝「每把钥匙都被撤销」的信任集。合并后触发这一条是完全
// 可能的真实情况（撤销累积到覆盖了当前清单的全部 keys），它的错误原样往上冒，
// 由调用方归到 unavailable 状态——不在这里吞掉换一个空信任集。
func assembleKeyring(keyringRaw json.RawMessage, revoked *revokedSet) (*sign.Keyring, error) {
	var shape keyringShape
	if err := json.Unmarshal(keyringRaw, &shape); err != nil {
		return nil, fmt.Errorf("assemble keyring: %w", err)
	}
	merged := revokedSet{entries: map[sign.KeyID]rawRevocationEntry{}}
	for id, e := range revoked.entries {
		merged.entries[id] = e
	}
	for _, e := range shape.Revoked {
		if _, seen := merged.entries[e.KeyID]; !seen {
			merged.entries[e.KeyID] = e
		}
	}
	entries := make([]rawRevocationEntry, 0, len(merged.entries))
	for _, e := range merged.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].KeyID < entries[j].KeyID })

	doc := struct {
		Keys    json.RawMessage      `json:"keys"`
		Revoked []rawRevocationEntry `json:"revoked,omitempty"`
	}{Keys: shape.Keys, Revoked: entries}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("assemble keyring: %w", err)
	}
	keyring, err := sign.ParseKeyring(data)
	if err != nil {
		return nil, fmt.Errorf("assemble keyring: %w", err)
	}
	return keyring, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/plugin/trustlist/ -v
```

预期：全部 PASS。

- [ ] **Step 5: 变异验证——让 mergeFrom 每次重建集合，确认「撤销永不遗忘」必红**

临时在 `mergeFrom` 开头加一行 `s.entries = map[sign.KeyID]rawRevocationEntry{}`，跑：

```bash
go test ./internal/plugin/trustlist/ -run TestRevocationIsNeverForgotten
```

预期：**FAIL**，报「dev-k 在 B 之后不再是撤销状态」。改回来，再跑确认 PASS。

- [ ] **Step 6: 提交**

```bash
gofmt -l . && go vet ./internal/plugin/trustlist/
git add internal/plugin/trustlist/
git commit -m "$(cat <<'EOF'
feat(trustlist): 撤销累积集与信任集装配

清单过期不作废（断网时作废等于全线失信），但那样断网就能绕过撤销。
累积集堵住这个洞：撤销见过一次就永久生效，与清单是否过期、后续清单里
还有没有它、此后能否联网都无关。撤销单调，登记不单调。

装配走「拼回一份 keyring 文档再交给 sign.ParseKeyring」，而不是在
sign.Keyring 外面维护第二套撤销判断——后者是同一条规则写两遍，分家的
方向一定是某处忘了查累积集，也就是撤销失效。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: 缓存的读写、原子落盘与损坏检测

**Files:**
- Create: `internal/plugin/trustlist/cache.go`
- Create: `internal/plugin/trustlist/cache_lock.go`
- Create: `internal/plugin/trustlist/cache_lock_windows.go`
- Create: `internal/plugin/trustlist/cache_lock_other.go`
- Test: `internal/plugin/trustlist/cache_test.go`

**Interfaces:**
- Consumes: `Document`、`VerifyDocument`（Task 1）；`revokedSet`、`parseRevokedSet`（Task 2）
- Produces:
  - `type cache struct { dir string }`
  - `func newCache(dir string) (*cache, error)`
  - `func (c *cache) read() (Document, *revokedSet, error)`
  - `func (c *cache) write(doc Document, sigData []byte, revoked *revokedSet) error`
  - `var errNoCache = errors.New("no cached trustlist")`

**锁策略（本任务必须按这个来，不要另行发挥）**：本包的写者是 serve 的后台刷新循环与 `agent plugins trustlist refresh` 两个，跨进程并发是真实的但很稀疏，且一次刷新完全可以等一会儿。所以**采取「等待」策略**，与 `internal/plugin/fetch` 相同：`ErrExist` 计入争用。这与 `internal/taskledger` 相反——那一份**不等待**（2 次尝试 + mtime 陈旧判定），所以它刻意把 `ErrExist` 排除在争用之外，否则被杀死的进程留下的锁永远无法回收。**照抄任何一份之前先读它的等待策略**，两份判据长得几乎一样但语义相反。

- [ ] **Step 1: 写失败的测试**

创建 `internal/plugin/trustlist/cache_test.go`：

```go
package trustlist

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// signedDoc 造一份用测试 root 签好的清单：返回清单字节、签名字节，并把
// 内嵌 root 换成能验它的那一把（用例结束自动还原）。
func signedDoc(t *testing.T, serial int64) (list, sigData []byte) {
	t.Helper()
	priv := testRootPrivateKey(t)
	list = testDoc(t, func(m map[string]any) { m["serial"] = float64(serial) })
	sig, err := sign.Sign(priv, rootKeyID(t), list)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	sigData, err = sign.MarshalSignature(sig)
	if err != nil {
		t.Fatalf("MarshalSignature: %v", err)
	}
	return list, sigData
}

func TestCacheRoundTrips(t *testing.T) {
	list, sigData := signedDoc(t, 7)
	doc, err := VerifyDocument(list, sigData)
	if err != nil {
		t.Fatalf("VerifyDocument: %v", err)
	}
	c, err := newCache(t.TempDir())
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	set := newRevokedSet()
	if err := set.mergeFrom(doc.KeyringRaw); err != nil {
		t.Fatalf("mergeFrom: %v", err)
	}
	if err := c.write(doc, sigData, set); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, backSet, err := c.read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if back.Serial != 7 {
		t.Errorf("read().Serial = %d, want 7", back.Serial)
	}
	if backSet.len() != set.len() {
		t.Errorf("撤销累积集往返后 len = %d, want %d", backSet.len(), set.len())
	}
}

func TestCacheReportsNoCacheOnAnEmptyDir(t *testing.T) {
	c, err := newCache(t.TempDir())
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	if _, _, err := c.read(); !errors.Is(err, errNoCache) {
		t.Errorf("空目录的 read()：err = %v，want 裹 errNoCache", err)
	}
}

// TestCorruptCacheIsNeverSilentlyRepaired：损坏的缓存必须报错、文件必须保留。
//
// 静默重建会抹掉现场，而现场是判断「是磁盘坏了还是有人动过」的唯一依据；
// 静默当成空缓存则更糟——那等于宣告这台机器从没见过任何撤销。
func TestCorruptCacheIsNeverSilentlyRepaired(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(t *testing.T, dir string)
	}{
		{"清单被截断", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "trustlist.json")
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if err := os.WriteFile(p, data[:len(data)/2], 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}},
		{"清单被改一个字节", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "trustlist.json")
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			data[len(data)/2] ^= 0x01
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}},
		{"签名与清单不配对", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "trustlist.sig"),
				[]byte(`{"key_id":"x","algorithm":"ed25519","signature":"`+
					"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="+
					`"}`), 0o600); err != nil {
				t.Fatalf("write sig: %v", err)
			}
		}},
		{"累积集损坏", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "revoked-ever.json"),
				[]byte(`{"revoked":[{"key_id":`), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			list, sigData := signedDoc(t, 7)
			doc, err := VerifyDocument(list, sigData)
			if err != nil {
				t.Fatalf("VerifyDocument: %v", err)
			}
			c, err := newCache(dir)
			if err != nil {
				t.Fatalf("newCache: %v", err)
			}
			if err := c.write(doc, sigData, newRevokedSet()); err != nil {
				t.Fatalf("write: %v", err)
			}
			tc.corrupt(t, dir)

			if _, _, err := c.read(); err == nil {
				t.Fatal("损坏的缓存被当成有效的读了回来")
			}
			// 文件必须还在：现场不能被抹掉。
			for _, name := range []string{"trustlist.json", "trustlist.sig", "revoked-ever.json"} {
				if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
					t.Errorf("%s 在一次失败的 read 之后不见了：%v", name, err)
				}
			}
		})
	}
}

// TestWriteOrdersRevocationsFirst：先写累积集、成功后再写清单。
//
// 顺序反了会出现「清单已更新但撤销没记下」的窗口，而那个方向的丢失正是
// 累积集存在要防的事。这里靠把累积集所在的路径做成不可写来制造失败，
// 然后断言清单没有被更新。
func TestWriteOrdersRevocationsFirst(t *testing.T) {
	dir := t.TempDir()
	list7, sig7 := signedDoc(t, 7)
	doc7, err := VerifyDocument(list7, sig7)
	if err != nil {
		t.Fatalf("VerifyDocument: %v", err)
	}
	c, err := newCache(dir)
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	if err := c.write(doc7, sig7, newRevokedSet()); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 让累积集写不进去：把它变成一个目录。
	if err := os.Remove(filepath.Join(dir, "revoked-ever.json")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "revoked-ever.json"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	list9, sig9 := signedDoc(t, 9)
	doc9, err := VerifyDocument(list9, sig9)
	if err != nil {
		t.Fatalf("VerifyDocument: %v", err)
	}
	if err := c.write(doc9, sig9, newRevokedSet()); err == nil {
		t.Fatal("累积集写不进去时 write 却成功了")
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "trustlist.json"))
	if err != nil {
		t.Fatalf("read trustlist.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(onDisk, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["serial"].(float64) != 7 {
		t.Errorf("累积集写失败之后清单却被更新到了 serial=%v", m["serial"])
	}
}

// TestConcurrentWritesNeverLoseARevocation：两个写者各自并入不同的撤销，
// 并发写完之后两条都必须还在。
//
// 累积集是 read-modify-write，没有锁就会丢更新，而丢掉的正好是撤销。
func TestConcurrentWritesNeverLoseARevocation(t *testing.T) {
	dir := t.TempDir()
	c, err := newCache(dir)
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	list, sigData := signedDoc(t, 7)
	doc, err := VerifyDocument(list, sigData)
	if err != nil {
		t.Fatalf("VerifyDocument: %v", err)
	}
	if err := c.write(doc, sigData, newRevokedSet()); err != nil {
		t.Fatalf("write: %v", err)
	}

	var wg sync.WaitGroup
	for _, id := range []sign.KeyID{"gone-a", "gone-b"} {
		wg.Add(1)
		go func(id sign.KeyID) {
			defer wg.Done()
			_, set, err := c.read()
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			if err := set.mergeFrom(keyringWith(t, []sign.KeyID{id}, []sign.KeyID{id})); err != nil {
				t.Errorf("mergeFrom: %v", err)
				return
			}
			if err := c.write(doc, sigData, set); err != nil {
				t.Errorf("write: %v", err)
			}
		}(id)
	}
	wg.Wait()

	_, set, err := c.read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if set.len() != 2 {
		t.Errorf("并发写之后累积集有 %d 条，want 2——有撤销被丢了", set.len())
	}
}
```

- [ ] **Step 2: 跑测试确认它失败**

```bash
go test ./internal/plugin/trustlist/ -run TestCache -v
```

预期：编译失败，`undefined: newCache`。

- [ ] **Step 3: 实现锁的判据**

创建 `internal/plugin/trustlist/cache_lock.go`：

```go
package trustlist

import (
	"errors"
	"io/fs"
)

// lockCreateIsContended 判断一次 O_CREATE|O_EXCL 的失败是不是「另一个写者此刻
// 正持有锁」——那是唯一值得等待的情况——而不是等多久都不会好的故障。
//
// 本包**采取等待策略**：一次刷新完全可以等一会儿，而把争用误判为致命会让一次
// 本来没问题的刷新随机失败。所以「文件已存在」这个常规答案计入争用。
//
// 注意 internal/taskledger 里那份形状几乎一样的判据**语义相反**：它不等待
// （2 次尝试 + mtime 陈旧判定），所以刻意把 ErrExist 排除在争用之外，否则被
// 杀死的进程留下的锁永远无法回收。照抄任何一份之前先读它的等待策略。
func lockCreateIsContended(err error) bool {
	if errors.Is(err, fs.ErrExist) {
		return true
	}
	return lockCreateIsContendedOnThisPlatform(err)
}
```

创建 `internal/plugin/trustlist/cache_lock_windows.go`：

```go
//go:build windows

package trustlist

import (
	"errors"
	"syscall"
)

// lockCreateIsContendedOnThisPlatform 补上 Windows 对「另一个写者正持有这把锁」
// 的第二种拼法。
//
// Windows 删除一个文件不一定立刻解除它的名字：只要还有句柄开着，这个名字就处在
// delete-pending 状态，而对 delete-pending 的名字 CreateFile 失败于
// ERROR_ACCESS_DENIED（errno 5），**不是**「已存在」。一个写者释放锁（os.Remove）
// 的过程因此是一个窗口，窗口里另一个写者的 create 会对一个逻辑上只是「被占着」
// 的文件看到 errno 5。
//
// 这是实测出来的，不是推测：internal/plugin/fetch 里同一现象的探针在一条路径上
// 用 12 个 goroutine 反复 create/release，看到 611 次 ERROR_ACCESS_DENIED 对
// 28160 次 ErrExist——约 2% 的争用创建。把它读成致命错误，就是让测试与生产在
// Windows 上随机失败的原因。
//
// 折叠 errno 5 的代价：一把真正打不开的锁（ACL 拒绝、只读位置）现在要等满整个
// 等待时限才被报出来，而不是立刻失败。所以超时的错误信息里必须带上最后看到的
// 那个错误——权限问题仍然会浮出来，带着 errno，而不是被报成一个幽灵持锁者。
func lockCreateIsContendedOnThisPlatform(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.ERROR_ACCESS_DENIED
}
```

创建 `internal/plugin/trustlist/cache_lock_other.go`：

```go
//go:build !windows

package trustlist

// lockCreateIsContendedOnThisPlatform 在非 Windows 上恒为 false。
//
// POSIX 的 EACCES 说的就是权限被拒，没有 Windows 那种「名字正在被删除」的中间
// 态。把它折进争用，等于让一个目录权限配错的部署白等满整个等待时限，然后报出
// 一个误导性的「锁竞争」而不是「你没有写权限」。
func lockCreateIsContendedOnThisPlatform(error) bool { return false }
```

- [ ] **Step 4: 实现缓存**

创建 `internal/plugin/trustlist/cache.go`：

```go
package trustlist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	listFileName    = "trustlist.json"
	sigFileName     = "trustlist.sig"
	revokedFileName = "revoked-ever.json"
	lockFileName    = ".lock"

	// lockWait 是等一个并发写者放手的上限。刷新不是热路径，等几秒远好过
	// 随机失败；超时的错误里会带上最后看到的那个错误（见 cache_lock_windows.go）。
	lockWait = 5 * time.Second
	// lockPoll 是两次重试之间的间隔。
	lockPoll = 20 * time.Millisecond
)

// errNoCache 表示这台机器还没有任何缓存的清单——目录是空的。
//
// 它与「缓存损坏」是不同的两件事，且必须分得开：前者是全新安装的正常状态，
// 后者需要人来看一眼。调用方对二者的状态归类相同（都是 unavailable），但
// 日志与告警不同。
var errNoCache = errors.New("no cached trustlist")

// cache 是缓存目录。三个文件：
//
//	trustlist.json     最近一次被接受的清单**原始字节**
//	trustlist.sig      它的签名
//	revoked-ever.json  撤销累积集
//
// 存原始字节而不是解析后的结构，是为了让读回来时走的解析与验签代码和网络
// 路径**完全相同**——缓存无法成为一条绕过校验的旁路。
//
// 「见过的最大 serial」不单独存文件：它就是 trustlist.json 里的 serial。代价是
// 缓存损坏时防回滚保护随之失效；可以接受，因为损坏不会波及 revoked-ever.json
// （独立文件、只增不减），而回滚攻击的目标——让撤销失效——正是被累积集挡住的。
type cache struct {
	dir string
}

func newCache(dir string) (*cache, error) {
	if dir == "" {
		return nil, errors.New("trustlist cache dir is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve trustlist cache dir %s: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create trustlist cache dir %s: %w", abs, err)
	}
	return &cache{dir: abs}, nil
}

func (c *cache) path(name string) string { return filepath.Join(c.dir, name) }

// read 读回缓存的清单与撤销累积集。
//
// 清单走的是 VerifyDocument——与网络路径同一个函数。这不是多余的谨慎：如果
// 缓存读取绕过验签，那么任何能写这个目录的东西就能给这台机器换一份信任集。
//
// 三个文件里任何一个缺失都报 errNoCache；任何一个损坏都报错，且**不删除、不
// 重建**：静默重建会抹掉判断「是磁盘坏了还是有人动过」的唯一现场。
func (c *cache) read() (Document, *revokedSet, error) {
	listData, err := os.ReadFile(c.path(listFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Document{}, nil, errNoCache
		}
		return Document{}, nil, fmt.Errorf("read cached trustlist: %w", err)
	}
	sigData, err := os.ReadFile(c.path(sigFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 清单在而签名不在：这不是「还没缓存」，是缓存不完整。
			return Document{}, nil, fmt.Errorf("cached trustlist has no signature at %s; "+
				"the cache is incomplete and will not be used", c.path(sigFileName))
		}
		return Document{}, nil, fmt.Errorf("read cached trustlist signature: %w", err)
	}
	doc, err := VerifyDocument(listData, sigData)
	if err != nil {
		return Document{}, nil, fmt.Errorf("cached trustlist at %s: %w", c.dir, err)
	}
	revokedData, err := os.ReadFile(c.path(revokedFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Document{}, nil, fmt.Errorf("cached trustlist has no revocation record at %s; "+
				"treating the cache as unusable rather than as 'nothing was ever revoked'",
				c.path(revokedFileName))
		}
		return Document{}, nil, fmt.Errorf("read cached revocations: %w", err)
	}
	revoked, err := parseRevokedSet(revokedData)
	if err != nil {
		return Document{}, nil, fmt.Errorf("cached revocations at %s: %w", c.path(revokedFileName), err)
	}
	return doc, revoked, nil
}

// write 落盘一份新接受的清单。
//
// 顺序是**先写撤销累积集，成功后再写清单**。反过来会留下一个「清单已更新但
// 撤销没记下」的窗口，而这个方向的丢失正是累积集存在要防的事。
//
// 每个文件都走「写临时文件 → Sync → rename」，rename 在同一目录内是原子的，
// 所以读者永远看不到半份文件。
//
// 整个操作在目录锁下进行：累积集是 read-modify-write，两个并发写者不加锁会
// 丢更新，而丢掉的正好是撤销。
func (c *cache) write(doc Document, sigData []byte, revoked *revokedSet) (err error) {
	unlock, err := c.lock()
	if err != nil {
		return err
	}
	defer func() {
		if unlockErr := unlock(); unlockErr != nil && err == nil {
			err = unlockErr
		}
	}()

	revokedData, err := revoked.marshal()
	if err != nil {
		return err
	}
	if err := c.writeFileAtomically(revokedFileName, revokedData); err != nil {
		return err
	}
	if err := c.writeFileAtomically(sigFileName, sigData); err != nil {
		return err
	}
	return c.writeFileAtomically(listFileName, doc.Raw)
}

func (c *cache) writeFileAtomically(name string, data []byte) error {
	tmp, err := os.CreateTemp(c.dir, name+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", name, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后这次 Remove 找不到文件，无害

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := os.Rename(tmpName, c.path(name)); err != nil {
		return fmt.Errorf("publish %s: %w", name, err)
	}
	return nil
}

// lock 取得缓存目录的排他锁，返回释放它的函数。
//
// 用 O_CREATE|O_EXCL 建一个哨兵文件作锁：它是唯一一种在「检查」与「创建」之间
// 不会输掉竞争的做法。争用（见 lockCreateIsContended）会等待重试；等满 lockWait
// 之后报错，且错误里带上最后看到的那个 create 错误——一把因为权限而真正打不开
// 的锁，必须以权限问题的面目出现，而不是一个幽灵持锁者。
func (c *cache) lock() (func() error, error) {
	path := c.path(lockFileName)
	deadline := time.Now().Add(lockWait)
	var last error
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			if closeErr := f.Close(); closeErr != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("close trustlist cache lock: %w", closeErr)
			}
			return func() error {
				if err := os.Remove(path); err != nil {
					return fmt.Errorf("release trustlist cache lock: %w", err)
				}
				return nil
			}, nil
		}
		last = err
		if !lockCreateIsContended(err) {
			return nil, fmt.Errorf("acquire trustlist cache lock: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("acquire trustlist cache lock at %s: waited %s, last error: %w",
				path, lockWait, last)
		}
		time.Sleep(lockPoll)
	}
}
```

- [ ] **Step 5: 跑测试确认通过**

```bash
go test ./internal/plugin/trustlist/ -v
go test ./internal/plugin/trustlist/ -race -count=20
```

预期：两条都 PASS。`-count=20` 是必须的：§Task 3 的锁争用窗口在 Windows 上约 2% 概率，单次通过说明不了任何事——上一期正是「重跑 3 次没复现」把一个真实缺陷放过去的。

- [ ] **Step 6: 变异验证——去掉锁，确认并发用例必红**

临时把 `write` 里的 `unlock, err := c.lock()` 那一段连同 defer 注释掉，跑：

```bash
go test ./internal/plugin/trustlist/ -run TestConcurrentWritesNeverLoseARevocation -count=20
```

预期：**至少红一次**（丢更新是概率事件，所以要 `-count=20`）。改回来，再跑确认 20 次全绿。

- [ ] **Step 7: 提交**

```bash
gofmt -l . && go vet ./internal/plugin/trustlist/
git add internal/plugin/trustlist/
git commit -m "$(cat <<'EOF'
feat(trustlist): 缓存的原子落盘、目录锁与损坏检测

缓存存的是清单**原始字节**，读回来走的是与网络路径完全相同的
VerifyDocument——缓存不能成为绕过校验的旁路。

写顺序是先累积集后清单：反过来会留下「清单已更新但撤销没记下」的窗口，
而那个方向的丢失正是累积集要防的事。累积集是 read-modify-write，所以
整个写在目录锁下进行，否则并发写丢掉的正好是撤销。

锁采取等待策略（与 fetch 相同，与不等待的 taskledger 相反），Windows 的
delete-pending → errno 5 折进争用判据；两份判据形状几乎一样但语义相反，
注释里写清了照抄前要先读等待策略。

损坏的缓存报错且保留文件，不静默重建、不当成空缓存——后者等于宣告这台
机器从没见过任何撤销。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: HTTP 取回

**Files:**
- Create: `internal/plugin/trustlist/fetch.go`
- Test: `internal/plugin/trustlist/fetch_test.go`

**Interfaces:**
- Produces:
  - `const maxListBytes = 1 << 20`
  - `const maxSigBytes = 4 << 10`
  - `const fetchTimeout = 30 * time.Second`
  - `func sigURL(listURL string) (string, error)`
  - `func fetchBytes(ctx context.Context, client *http.Client, rawURL string, maxBytes int64) ([]byte, error)`

- [ ] **Step 1: 写失败的测试**

创建 `internal/plugin/trustlist/fetch_test.go`：

```go
package trustlist

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSigURLDerivesFromTheListURL(t *testing.T) {
	t.Parallel()

	got, err := sigURL("https://example.com/trust/trustlist.json")
	if err != nil {
		t.Fatalf("sigURL: %v", err)
	}
	if want := "https://example.com/trust/trustlist.sig"; got != want {
		t.Errorf("sigURL = %q, want %q", got, want)
	}
}

// TestSigURLRefusesAnUnexpectedSuffix：清单地址必须以 .json 结尾，否则推导出的
// 签名地址就是猜的。给两个可以各自配置的 URL 才是真正的危险（两份不匹配的
// 文档），但推导也必须是确定的，不能沉默地拼错。
func TestSigURLRefusesAnUnexpectedSuffix(t *testing.T) {
	t.Parallel()

	if _, err := sigURL("https://example.com/trust/trustlist"); err == nil {
		t.Fatal("不以 .json 结尾的清单地址被接受了")
	}
}

func TestFetchRefusesPlainHTTP(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	// httptest.NewServer 是 http://。清单地址必须是 https，且这条与
	// plugins.allow_insecure_sources 无关——那个开关放宽的是插件产物的取回，
	// 而产物有 digest 兜底，清单没有。
	_, err := fetchBytes(context.Background(), srv.Client(), srv.URL, maxListBytes)
	if err == nil {
		t.Fatal("http:// 的清单地址被接受了")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("错误没说清是 scheme 的问题：%v", err)
	}
}

func TestFetchReportsTheStatusCode(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := fetchBytes(context.Background(), srv.Client(), srv.URL, maxListBytes)
	if err == nil {
		t.Fatal("404 被当成了清单内容")
	}
	// GitHub 对带无效凭据的请求返回 404 而不是 401。不把状态码写进错误信息，
	// 会让鉴权问题伪装成「文件不存在」，把排查引向完全错误的方向。
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("错误里没有状态码：%v", err)
	}
}

func TestFetchRefusesAnOversizedBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 64<<10)
		for written := int64(0); written <= maxListBytes; written += int64(len(chunk)) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	if _, err := fetchBytes(context.Background(), srv.Client(), srv.URL, maxListBytes); err == nil {
		t.Fatal("超过上限的响应体被接受了")
	}
}

func TestFetchRefusesAnHTTPSToHTTPRedirect(t *testing.T) {
	t.Parallel()

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	defer plain.Close()

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer secure.Close()

	client := secure.Client()
	if _, err := fetchBytes(context.Background(), client, secure.URL, maxListBytes); err == nil {
		t.Fatal("从 https 降级到 http 的重定向被跟随了")
	}
}

func TestFetchReturnsTheBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	body, err := fetchBytes(context.Background(), srv.Client(), srv.URL, maxListBytes)
	if err != nil {
		t.Fatalf("fetchBytes: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}
}
```

- [ ] **Step 2: 跑测试确认它失败**

```bash
go test ./internal/plugin/trustlist/ -run TestFetch -v
```

预期：编译失败，`undefined: fetchBytes`。

- [ ] **Step 3: 实现**

创建 `internal/plugin/trustlist/fetch.go`：

```go
package trustlist

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// maxListBytes 是清单文档的上限。远超任何合理的公钥清单规模，又远小于
	// 「读进内存会造成麻烦」的量级。
	maxListBytes int64 = 1 << 20
	// maxSigBytes 是签名文档的上限。一份 plugin.sig 形状的文档只有几百字节。
	maxSigBytes int64 = 4 << 10
	// fetchTimeout 是单次取回的上限。
	fetchTimeout = 30 * time.Second
	// maxRedirects 与 internal/plugin/fetch 取同一个数，理由也相同：足够穿过
	// 常见的 CDN 跳转，又不至于让一条重定向环耗到超时。
	maxRedirects = 9
)

// sigURL 由清单地址推导签名地址：同目录、同文件名、后缀换成 .sig。
//
// 不给签名一个单独的配置项，是因为两个可以各自配置的 URL 就有可以指向两份
// 不匹配文档的配置——而那种不匹配的症状（验签失败）会把排查引向「被攻击了」，
// 而不是「配错了」。
func sigURL(listURL string) (string, error) {
	const suffix = ".json"
	if !strings.HasSuffix(listURL, suffix) {
		return "", fmt.Errorf("trustlist url %q does not end in %s; the signature's address is derived "+
			"from it by replacing that suffix, and guessing is not an option here", listURL, suffix)
	}
	return strings.TrimSuffix(listURL, suffix) + ".sig", nil
}

// fetchBytes 取回 rawURL 的响应体，至多 maxBytes 字节。
//
// 它不复用 internal/plugin/fetch.Fetch：那个函数要求调用方**预先知道内容的
// digest**，因为它是为「manifest 里钉死了 digest 的插件产物」设计的。信任清单
// 本来就是可变的，没有预知的 digest 可传。这里复用的是它的形状而不是它的代码。
//
// 拒绝的情况：
//
//   - rawURL 的 scheme 不是 https。清单没有 digest 兜底，明文传输意味着任何
//     中间人都能换掉它——注意这与 plugins.allow_insecure_sources 无关，那个开关
//     放宽的是有 digest 保护的插件产物；
//   - 任何一跳重定向的目标不是 https（同一个理由，降级同样致命）；
//   - 重定向超过 maxRedirects 跳；
//   - 响应状态不是 200。**错误里必须带状态码**：GitHub 对带无效凭据的请求返回
//     404 而不是 401，状态码缺失会让鉴权问题伪装成「文件不存在」；
//   - 响应体超过 maxBytes。判据是「读到 maxBytes+1 字节」——在读完之前就停下，
//     而不是读完再判断，否则上限保护不了内存。
func fetchBytes(ctx context.Context, client *http.Client, rawURL string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("fetch trustlist: max bytes is %d; it must be positive", maxBytes)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("fetch trustlist: parse url %q: %w", rawURL, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("fetch trustlist: url %q uses scheme %q, want https; a trustlist carries no "+
			"digest of its own, so plaintext transport lets any intermediary replace it "+
			"(plugins.allow_insecure_sources does not relax this — it relaxes artifact fetches, "+
			"which are digest-protected)", rawURL, u.Scheme)
	}

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch trustlist: build request: %w", err)
	}

	// 复制一份 client，只为装上重定向策略——不改调用方给的那个。
	guarded := *client
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("redirect to %q leaves https; a downgrade en route is the same exposure "+
				"as starting in plaintext", req.URL.Redacted())
		}
		return nil
	}

	resp, err := guarded.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch trustlist %s: %w", u.Redacted(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch trustlist %s: HTTP %d %s", u.Redacted(), resp.StatusCode,
			http.StatusText(resp.StatusCode))
	}

	// 多读一个字节，才能把「正好等于上限」与「超过上限」分开。
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch trustlist %s: read body: %w", u.Redacted(), err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("fetch trustlist %s: body exceeds %d bytes; a document this large is not "+
			"the one this project publishes", u.Redacted(), maxBytes)
	}
	return body, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/plugin/trustlist/ -v
```

预期：全部 PASS。

- [ ] **Step 5: 提交**

```bash
gofmt -l . && go vet ./internal/plugin/trustlist/
git add internal/plugin/trustlist/
git commit -m "$(cat <<'EOF'
feat(trustlist): 清单的 HTTP 取回

不复用 fetch.Fetch：那个函数要求调用方预知内容 digest（它服务的是
manifest 里钉死 digest 的插件产物），而清单本来就是可变的。这里复用的
是它的形状——大小上限、超时、重定向策略、非 2xx 判定。

https 强制且不受 allow_insecure_sources 影响：那个开关放宽的是有 digest
兜底的产物取回，清单没有兜底。重定向途中降级到 http 同样拒绝。

错误里必须带 HTTP 状态码：GitHub 对带无效凭据的请求返回 404 而不是 401，
状态码缺失会让鉴权问题伪装成「文件不存在」。

签名地址由清单地址推导而不是单独配置：两个可各自配置的 URL 就有指向两份
不匹配文档的配置，而那种不匹配看起来像被攻击，不像配错。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Store —— serial 单调、状态判定、Current/Refresh

**Files:**
- Create: `internal/plugin/trustlist/store.go`
- Test: `internal/plugin/trustlist/store_test.go`

**Interfaces:**
- Consumes: 全部前面四个任务
- Produces:
  - `type Status int`，常量 `StatusUnavailable`/`StatusStale`/`StatusFresh`，`func (s Status) String() string`
  - `type Trust struct { Keyring *sign.Keyring; Publishers map[sign.KeyID]Publisher; Status Status; Serial int64; IssuedAt, ExpiresAt time.Time }`
  - `type Config struct { URL string; CacheDir string; Client *http.Client; Now func() time.Time }`
  - `type Store struct { ... }`
  - `func NewStore(cfg Config) (*Store, error)`
  - `func (s *Store) Current() (Trust, error)`
  - `func (s *Store) Refresh(ctx context.Context) (Trust, error)`
  - `var ErrSerialRegressed = errors.New("trustlist serial regressed")`

- [ ] **Step 1: 写失败的测试**

创建 `internal/plugin/trustlist/store_test.go`：

```go
package trustlist

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// serveList 起一个 TLS 测试服务，按当前设置的清单/签名作答。改 cur 就能改下一次
// 取回拿到的内容。
type serveList struct {
	list []byte
	sig  []byte
}

func newListServer(t *testing.T, cur *serveList) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sig") {
			_, _ = w.Write(cur.sig)
			return
		}
		_, _ = w.Write(cur.list)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestStore(t *testing.T, srv *httptest.Server, now func() time.Time) *Store {
	t.Helper()
	s, err := NewStore(Config{
		URL:      srv.URL + "/trustlist.json",
		CacheDir: t.TempDir(),
		Client:   srv.Client(),
		Now:      now,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// fixedNow 落在 testDoc 的 issued_at 与 expires_at 之间。
func fixedNow() time.Time { return time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC) }

// afterExpiry 落在 testDoc 的 expires_at 之后。
func afterExpiry() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }

func TestRefreshAcceptsAndCachesAFreshList(t *testing.T) {
	list, sig := signedDoc(t, 7)
	cur := &serveList{list: list, sig: sig}
	store := newTestStore(t, newListServer(t, cur), fixedNow)

	trust, err := store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if trust.Status != StatusFresh {
		t.Errorf("Status = %v, want StatusFresh", trust.Status)
	}
	if trust.Serial != 7 {
		t.Errorf("Serial = %d, want 7", trust.Serial)
	}
	if trust.Keyring == nil {
		t.Fatal("Keyring 是 nil")
	}
	// Current 只读缓存，不发请求，必须能拿到刚落盘的那份。
	again, err := store.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if again.Serial != 7 {
		t.Errorf("Current().Serial = %d, want 7", again.Serial)
	}
}

// TestRefreshRefusesASerialRollback 挡的是对这套机制最便宜的攻击：重放一份
// 签名完全合法的旧清单，把用户挡在某次撤销之前。
func TestRefreshRefusesASerialRollback(t *testing.T) {
	list9, sig9 := signedDoc(t, 9)
	cur := &serveList{list: list9, sig: sig9}
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, fixedNow)

	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(9): %v", err)
	}
	// 换成一份 serial 更小、但签名同样合法的清单。
	list7, sig7 := signedDoc(t, 7)
	cur.list, cur.sig = list7, sig7

	trust, err := store.Refresh(context.Background())
	if !errors.Is(err, ErrSerialRegressed) {
		t.Fatalf("Refresh(7 after 9)：err = %v，want 裹 ErrSerialRegressed", err)
	}
	// 拒绝之后手上仍然是 9，而不是掉回 unavailable。
	if trust.Serial != 9 {
		t.Errorf("拒绝回滚后 Serial = %d, want 9", trust.Serial)
	}
	cached, err := store.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cached.Serial != 9 {
		t.Errorf("缓存被回滚的清单覆盖了：Serial = %d", cached.Serial)
	}
}

// TestRefreshRefusesTheSameSerialWithDifferentContent：发布侧改了内容却没进
// serial，是发布流程事故；无法判断哪一份才是当前的，所以拒绝。
func TestRefreshRefusesTheSameSerialWithDifferentContent(t *testing.T) {
	list, sig := signedDoc(t, 7)
	cur := &serveList{list: list, sig: sig}
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, fixedNow)

	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// 同一个 serial，但 keyring 里换了一把钥匙（signedDoc 每次都生成新钥匙）。
	list2, sig2 := signedDoc(t, 7)
	cur.list, cur.sig = list2, sig2

	if _, err := store.Refresh(context.Background()); err == nil {
		t.Fatal("serial 相同但内容不同的清单被接受了")
	}
}

// TestRefreshOnAFailureReturnsBothTheCachedTrustAndTheError：网络断了的时候，
// 调用方两件事都需要知道——拉取失败了，以及手上还有一份能用的。
func TestRefreshOnAFailureReturnsBothTheCachedTrustAndTheError(t *testing.T) {
	list, sig := signedDoc(t, 7)
	cur := &serveList{list: list, sig: sig}
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, fixedNow)

	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv.Close() // 断网

	trust, err := store.Refresh(context.Background())
	if err == nil {
		t.Fatal("服务器关掉之后 Refresh 却成功了")
	}
	if trust.Status == StatusUnavailable {
		t.Error("一次网络失败把状态降成了 unavailable；缓存还在，它应该继续可用")
	}
	if trust.Serial != 7 {
		t.Errorf("失败时返回的 Serial = %d, want 7", trust.Serial)
	}
}

// TestNoCacheAndNoNetworkIsUnavailable：从没成功取到过、且这次也没拉到 →
// unavailable，且 Keyring 必须是 nil。
//
// 调用方（S2）看到 unavailable 必须把所有插件按「未登记」处理。反过来做的诱惑
// 很实在（「拉不到就先放行吧」），而那等于给了攻击者一个把清单打掉就全线放行
// 的开关。这里断言 Keyring == nil，让「拿它去放行」在类型上就做不到。
func TestNoCacheAndNoNetworkIsUnavailable(t *testing.T) {
	cur := &serveList{}
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, fixedNow)
	srv.Close()

	trust, err := store.Refresh(context.Background())
	if err == nil {
		t.Fatal("既无缓存又无网络时 Refresh 却成功了")
	}
	if trust.Status != StatusUnavailable {
		t.Errorf("Status = %v, want StatusUnavailable", trust.Status)
	}
	if trust.Keyring != nil {
		t.Error("unavailable 却带回了一个非 nil 的 Keyring")
	}
}

// TestAnExpiredListIsStaleButUsable：过期不作废。断网久了清单会过期，
// 而作废意味着所有插件立刻失信——可用性代价大于收益。
func TestAnExpiredListIsStaleButUsable(t *testing.T) {
	list, sig := signedDoc(t, 7)
	cur := &serveList{list: list, sig: sig}
	store := newTestStore(t, newListServer(t, cur), afterExpiry)

	trust, err := store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if trust.Status != StatusStale {
		t.Errorf("Status = %v, want StatusStale", trust.Status)
	}
	if trust.Keyring == nil {
		t.Error("过期的清单不该让 Keyring 变成 nil")
	}
}

// TestRevocationSurvivesAcrossRefreshes 是端到端版本的「撤销永不遗忘」：
// 走完整的取回 → 落盘 → 再取回 → 装配，确认撤销没有在这条链上丢掉。
//
// Task 2 的同名单元测试只覆盖内存里的合并；这一条覆盖它穿过缓存的往返。
func TestRevocationSurvivesAcrossRefreshes(t *testing.T) {
	priv := testRootPrivateKey(t)
	id := rootKeyID(t)

	makeSigned := func(serial int64, revoked []sign.KeyID) ([]byte, []byte) {
		list := testDoc(t, func(m map[string]any) {
			m["serial"] = float64(serial)
			kr := m["keyring"].(map[string]any)
			if len(revoked) > 0 {
				rs := make([]any, 0, len(revoked))
				for _, r := range revoked {
					rs = append(rs, map[string]any{
						"key_id":     string(r),
						"revoked_at": "2026-08-29T10:00:00Z",
						"reason":     "私钥泄漏",
					})
				}
				kr["revoked"] = rs
			}
		})
		sig, err := sign.Sign(priv, id, list)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		sigData, err := sign.MarshalSignature(sig)
		if err != nil {
			t.Fatalf("MarshalSignature: %v", err)
		}
		return list, sigData
	}

	// A：撤销 dev-abc（testDoc 的 keyring 里就是这把）。
	listA, sigA := makeSigned(7, []sign.KeyID{"dev-abc"})
	cur := &serveList{list: listA, sig: sigA}
	store := newTestStore(t, newListServer(t, cur), fixedNow)
	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(A): %v", err)
	}

	// B：serial 更大，但 revoked 里没有 dev-abc 了。
	listB, sigB := makeSigned(9, nil)
	cur.list, cur.sig = listB, sigB
	trust, err := store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh(B): %v", err)
	}
	rev, ok := trust.Keyring.Revoked("dev-abc")
	if !ok {
		t.Fatal("dev-abc 在 B 之后不再是撤销状态——撤销穿过缓存时丢了")
	}
	if rev.Reason != "私钥泄漏" {
		t.Errorf("撤销理由丢了：%q", rev.Reason)
	}
}
```

- [ ] **Step 2: 跑测试确认它失败**

```bash
go test ./internal/plugin/trustlist/ -run TestRefresh -v
```

预期：编译失败，`undefined: NewStore`。

- [ ] **Step 3: 实现**

创建 `internal/plugin/trustlist/store.go`：

```go
package trustlist

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// ErrSerialRegressed 标记「收到的清单 serial 不大于本地已见的最大值」。
//
// 它是哨兵，因为它与网络故障是完全不同的事：对这套机制最便宜的攻击不是伪造
// 清单（那要 root 私钥），而是重放一份**签名完全合法**的旧清单，把用户挡在
// 某次撤销之前。它值得比一次超时高得多的告警级别，而且重试绝不会让它变好。
var ErrSerialRegressed = errors.New("trustlist serial regressed")

// Status 是手上这份清单的时效状态。
type Status int

const (
	// StatusUnavailable：从未成功取得过任何清单，或缓存损坏且这次也没拉到。
	// 此时 Trust.Keyring 是 nil，调用方必须把所有插件按「未登记」处理。
	StatusUnavailable Status = iota
	// StatusStale：验签通过但已过 expires_at（多半是长期断网）。信任集照常
	// 可用，撤销照常生效。
	StatusStale
	// StatusFresh：验签通过且未过期。
	StatusFresh
)

func (s Status) String() string {
	switch s {
	case StatusUnavailable:
		return "unavailable"
	case StatusStale:
		return "stale"
	case StatusFresh:
		return "fresh"
	default:
		return fmt.Sprintf("Status(%d)", int(s))
	}
}

// Trust 是一次装配的结果：信任集、发布者名录，以及它有多新。
//
// Status 为 StatusUnavailable 时 Keyring 为 nil、Publishers 为空。这不是「留空
// 待填」——它是让「拿不到清单就先放行」在类型层面做不到。
type Trust struct {
	Keyring    *sign.Keyring
	Publishers map[sign.KeyID]Publisher
	Status     Status
	Serial     int64
	IssuedAt   time.Time
	ExpiresAt  time.Time
}

// Config 是造一个 Store 需要的东西。
type Config struct {
	// URL 是清单文档的地址，必须是 https 且以 .json 结尾（签名地址由它推导）。
	URL string
	// CacheDir 是缓存目录。
	CacheDir string
	// Client 是发请求用的客户端。为 nil 时用 http.DefaultClient。
	Client *http.Client
	// Now 供测试注入时钟；为 nil 时用 time.Now。它只影响 fresh/stale 的判定。
	Now func() time.Time
}

// Store 持有缓存目录与取回配置，是这个包唯一有状态的类型。
//
// 它是并发安全的：mu 串行化 Refresh，因为一次刷新是「读缓存 → 取回 → 比较
// serial → 并入撤销 → 落盘」的读改写序列，两个并发的它会丢掉一次撤销并入。
// 跨进程的同一问题由缓存的目录锁挡住（见 cache.lock）。
type Store struct {
	url      string
	sigURL   string
	client   *http.Client
	cache    *cache
	now      func() time.Time
	mu       sync.Mutex
}

func NewStore(cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errors.New("trustlist: url is empty; an empty url means the remote list is not " +
			"configured, and building a Store for it is a caller mistake")
	}
	sig, err := sigURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	c, err := newCache(cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Store{url: cfg.URL, sigURL: sig, client: client, cache: c, now: now}, nil
}

// Current 返回当前信任状态，只读缓存，**绝不发起网络请求**。
//
// 挂载插件那类热路径调用它。缓存缺失或损坏时返回 StatusUnavailable 的 Trust
// 与一个 error——两者都返回，因为调用方既要知道出了什么事，也要一个能安全
// 使用的零状态。
func (s *Store) Current() (Trust, error) {
	doc, revoked, err := s.cache.read()
	if err != nil {
		return Trust{Status: StatusUnavailable}, err
	}
	return s.assemble(doc, revoked)
}

// Refresh 取回一次，通过全部校验后落盘，返回新的状态。
//
// 它同时返回 Trust 和 error，而不是常规的「返回零值 + error」：零值状态
// （StatusUnavailable）有明确的安全含义——调用方必须把所有插件按未登记处理——
// 把一次网络超时渲染成它，会造成一次不必要的全线降级。所以失败时返回的是
// 缓存里那份仍然可用的状态，加上说明这次没拉到的错误。
func (s *Store) Refresh(ctx context.Context) (Trust, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 先把手上已有的读出来，它既是 serial 比较的基准，也是失败时要返回的东西。
	cachedDoc, cachedRevoked, cacheErr := s.cache.read()
	fallback := Trust{Status: StatusUnavailable}
	haveCache := cacheErr == nil
	if haveCache {
		if t, err := s.assemble(cachedDoc, cachedRevoked); err == nil {
			fallback = t
		} else {
			// 缓存能读但装不出信任集（例如撤销累积到覆盖了全部 keys）。
			// 这不是「没有缓存」，但也不是一个能用的状态。
			haveCache = false
			cacheErr = err
		}
	}

	listData, err := fetchBytes(ctx, s.client, s.url, maxListBytes)
	if err != nil {
		return fallback, err
	}
	sigData, err := fetchBytes(ctx, s.client, s.sigURL, maxSigBytes)
	if err != nil {
		return fallback, err
	}
	doc, err := VerifyDocument(listData, sigData)
	if err != nil {
		return fallback, err
	}

	if haveCache {
		switch {
		case doc.Serial < cachedDoc.Serial:
			return fallback, fmt.Errorf("trustlist at %s has serial %d but this machine has already seen "+
				"%d; refusing a list that would undo revocations recorded since then: %w",
				s.url, doc.Serial, cachedDoc.Serial, ErrSerialRegressed)
		case doc.Serial == cachedDoc.Serial && !bytes.Equal(doc.Raw, cachedDoc.Raw):
			return fallback, fmt.Errorf("trustlist at %s has serial %d, the same as the cached one, but "+
				"different content; the publisher changed the list without advancing the serial, and "+
				"there is no way to tell which one is current", s.url, doc.Serial)
		case doc.Serial == cachedDoc.Serial:
			// 逐字相同：无变化，不重写缓存。
			return fallback, nil
		}
	}

	revoked := cachedRevoked
	if revoked == nil {
		revoked = newRevokedSet()
	}
	if err := revoked.mergeFrom(doc.KeyringRaw); err != nil {
		return fallback, err
	}
	if err := s.cache.write(doc, sigData, revoked); err != nil {
		return fallback, err
	}
	return s.assemble(doc, revoked)
}

// assemble 把一份文档与撤销累积集装配成 Trust，并按当前时间判定 fresh/stale。
func (s *Store) assemble(doc Document, revoked *revokedSet) (Trust, error) {
	keyring, err := assembleKeyring(doc.KeyringRaw, revoked)
	if err != nil {
		return Trust{Status: StatusUnavailable}, err
	}
	status := StatusFresh
	if !s.now().Before(doc.ExpiresAt) {
		status = StatusStale
	}
	return Trust{
		Keyring:    keyring,
		Publishers: doc.Publishers,
		Status:     status,
		Serial:     doc.Serial,
		IssuedAt:   doc.IssuedAt,
		ExpiresAt:  doc.ExpiresAt,
	}, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/plugin/trustlist/ -v
go test ./internal/plugin/trustlist/ -race -count=5
```

预期：都 PASS。

- [ ] **Step 5: 变异验证——把 serial 比较改成 `<=` 之外的方向，确认回滚用例必红**

临时把 `case doc.Serial < cachedDoc.Serial:` 改成 `case false:`，跑：

```bash
go test ./internal/plugin/trustlist/ -run TestRefreshRefusesASerialRollback
```

预期：**FAIL**。改回来，再跑确认 PASS。

- [ ] **Step 6: 提交**

```bash
gofmt -l . && go vet ./internal/plugin/trustlist/
git add internal/plugin/trustlist/
git commit -m "$(cat <<'EOF'
feat(trustlist): Store——serial 单调、状态判定、Current/Refresh

serial 严格单调挡的是对这套机制最便宜的攻击：重放一份签名完全合法的旧
清单，把用户挡在某次撤销之前。相等且内容不同也拒绝——发布侧改了内容没进
serial，无法判断哪一份才是当前的。

Refresh 同时返回 Trust 和 error 是有意的：零值状态 unavailable 有明确的
安全含义（调用方必须把所有插件按未登记处理），把一次网络超时渲染成它会
造成不必要的全线降级。所以失败时返回缓存那份仍可用的状态，外加错误。

unavailable 时 Keyring 为 nil，让「拿不到清单就先放行」在类型上做不到。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: 配置

**Files:**
- Modify: `internal/config/config.go`（在 `PluginsConfig` 里加字段、加 `PluginTrustlistConfig` 类型、在 `validatePlugins` 里加规则、在 `defaultConfig` 里加默认值）
- Test: `internal/config/config_test.go`（新增用例）

**Interfaces:**
- Produces:
  - `type PluginTrustlistConfig struct { URL string \`json:"url"\`; Cache string \`json:"cache"\`; RefreshIntervalMs int \`json:"refresh_interval_ms"\` }`
  - `PluginsConfig.Trustlist PluginTrustlistConfig \`json:"trustlist"\``
  - `func (c PluginTrustlistConfig) Enabled() bool`

- [ ] **Step 1: 写失败的测试**

在 `internal/config/config_test.go` 末尾追加：

```go
// TestTrustlistIsDisabledByAnEmptyURL：url 为空就是「没配远程清单」。
//
// 不做布尔开关：「未声明的布尔值该取哪一边」在这里没有安全上的正确答案——
// 联网能拿到撤销（更安全），不联网不引入新攻击面（也更安全）。用空串表达
// 「没配」沿用 plugins.keyring 的既有做法，把这个歧义整个绕开。
func TestTrustlistIsDisabledByAnEmptyURL(t *testing.T) {
	var cfg PluginTrustlistConfig
	if cfg.Enabled() {
		t.Error("空 url 的 trustlist 被当成启用了")
	}
	cfg.URL = "https://example.com/trust/trustlist.json"
	if !cfg.Enabled() {
		t.Error("配了 url 的 trustlist 被当成没启用")
	}
}

func TestValidatePluginsChecksTrustlist(t *testing.T) {
	base := func() PluginsConfig {
		return PluginsConfig{
			Manifest:    "plugins.json",
			Root:        "plugins",
			ApplyWaitMs: 1000,
			Limits:      PluginLimitsConfig{TimeoutMs: 1000},
			Fetch:       PluginFetchConfig{TimeoutMs: 1000, MaxBytes: 1024},
			Health:      PluginHealthConfig{MaxConsecutiveFaults: 3},
		}
	}

	t.Run("配了 url 但没配 cache", func(t *testing.T) {
		cfg := base()
		cfg.Trustlist = PluginTrustlistConfig{
			URL:               "https://example.com/trust/trustlist.json",
			RefreshIntervalMs: 21600000,
		}
		err := validatePlugins(cfg)
		if err == nil {
			t.Fatal("没有落点的 trustlist 被接受了")
		}
		if !strings.Contains(err.Error(), "cache") {
			t.Errorf("错误没说是 cache 的问题：%v", err)
		}
	})

	t.Run("refresh_interval_ms 非正", func(t *testing.T) {
		cfg := base()
		cfg.Trustlist = PluginTrustlistConfig{
			URL:               "https://example.com/trust/trustlist.json",
			Cache:             "trustlist",
			RefreshIntervalMs: 0,
		}
		if err := validatePlugins(cfg); err == nil {
			t.Fatal("refresh_interval_ms=0 被接受了")
		}
	})

	t.Run("url 不是 https", func(t *testing.T) {
		cfg := base()
		cfg.Trustlist = PluginTrustlistConfig{
			URL:               "http://example.com/trust/trustlist.json",
			Cache:             "trustlist",
			RefreshIntervalMs: 21600000,
		}
		err := validatePlugins(cfg)
		if err == nil {
			t.Fatal("http:// 的清单地址被接受了")
		}
		if !strings.Contains(err.Error(), "https") {
			t.Errorf("错误没说是 scheme 的问题：%v", err)
		}
	})

	t.Run("没配 url 时其余字段不受检查", func(t *testing.T) {
		cfg := base()
		cfg.Trustlist = PluginTrustlistConfig{}
		if err := validatePlugins(cfg); err != nil {
			t.Errorf("没启用远程清单的配置被拒了：%v", err)
		}
	})
}
```

（`strings` 已在 `config_test.go` 中被 import；若没有，加上。）

- [ ] **Step 2: 跑测试确认它失败**

```bash
go test ./internal/config/ -run TestTrustlist -v
```

预期：编译失败，`undefined: PluginTrustlistConfig`。

- [ ] **Step 3: 实现**

在 `internal/config/config.go` 的 `PluginsConfig` 结构体里，`ApplyWaitMs` 字段之后加：

```go
	// Trustlist 是这个部署从哪里取回官方信任清单、把它缓存在哪里、多久取一次。
	//
	// 它与 Keyring 是两条独立的路，并存而不互斥：Keyring 指向的本地文件服务于
	// 内网与离线部署，远程清单服务于公开生态。二者的优先级由消费方决定，配置
	// 层不替它们排序。
	Trustlist PluginTrustlistConfig `json:"trustlist"`
```

在 `PluginsConfig` 定义之后加新类型：

```go
// PluginTrustlistConfig 配置官方信任清单的取回。
//
// 空的 URL 就是「没有配置远程清单」，不另设一个布尔开关。理由是那个开关的
// 未声明值没有安全上的正确答案：联网能让撤销到达（更安全），不联网不引入新的
// 攻击面（也更安全）。用空串表达「没配」沿用 Keyring 的既有做法，把这个歧义
// 整个绕开——而本仓其他安全开关（RequireSignature、AllowInsecureSources）之所以
// 用指针，正是因为它们的未声明值有明确的安全一侧。
type PluginTrustlistConfig struct {
	// URL 是清单文档的地址。必须是 https：清单没有 digest 兜底，明文传输意味着
	// 任何中间人都能换掉它。签名文档的地址由这个值推导（同目录、后缀换成 .sig），
	// 不单独配置——两个可以各自配置的 URL 就有可以指向两份不匹配文档的配置。
	URL string `json:"url"`

	// Cache 是缓存目录。相对路径按进程工作目录解析，与 Manifest/Root 一致。
	//
	// URL 非空时它必须非空：清单没有落点，就等于每次启动前都有一段完全没有
	// 信任集的窗口。
	Cache string `json:"cache"`

	// RefreshIntervalMs 是两次后台取回之间的间隔，毫秒。必须为正——0 在这里
	// 没有「不限」或「只取一次」的读法。
	RefreshIntervalMs int `json:"refresh_interval_ms"`
}

// Enabled 报告这个部署是否配置了远程信任清单。
func (c PluginTrustlistConfig) Enabled() bool {
	return strings.TrimSpace(c.URL) != ""
}
```

在 `validatePlugins` 的 `return nil` 之前插入：

```go
	if cfg.Trustlist.Enabled() {
		if !strings.HasPrefix(cfg.Trustlist.URL, "https://") {
			return fmt.Errorf("plugins.trustlist.url is %q; it must be https, since a trustlist carries "+
				"no digest of its own and plaintext transport lets any intermediary replace it",
				cfg.Trustlist.URL)
		}
		if strings.TrimSpace(cfg.Trustlist.Cache) == "" {
			return fmt.Errorf("plugins.trustlist.cache is empty while plugins.trustlist.url is %q; "+
				"a trustlist with nowhere to land means every start has a window with no trust set at all",
				cfg.Trustlist.URL)
		}
		if cfg.Trustlist.RefreshIntervalMs <= 0 {
			return fmt.Errorf("plugins.trustlist.refresh_interval_ms is %d; it must be positive "+
				"(zero has no 'never' or 'once' reading here)", cfg.Trustlist.RefreshIntervalMs)
		}
	}
```

在 `defaultConfig()` 里，`Plugins` 那一段中加上默认间隔（若 `Plugins` 段目前没有显式构造，则在 `Load` 归一化的位置补：`if cfg.Plugins.Trustlist.RefreshIntervalMs == 0 { cfg.Plugins.Trustlist.RefreshIntervalMs = 21600000 }`，与其他默认值的归一化放在一起）：

```go
	// 6 小时。撤销要多久到达用户机器，由这个值决定；再短就是在给 GitHub
	// 发无谓的请求，再长会让一次紧急撤销拖太久。
	const defaultTrustlistRefreshMs = 21600000
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/config/ -v
```

预期：全部 PASS（包括既有用例）。

- [ ] **Step 5: 提交**

```bash
gofmt -l . && go vet ./internal/config/
git add internal/config/
git commit -m "$(cat <<'EOF'
feat(config): plugins.trustlist 配置段

空 url = 没配远程清单，不另设布尔开关：那个开关的未声明值没有安全上的
正确答案（联网能拿到撤销更安全，不联网不引入攻击面也更安全）。用空串
表达「没配」沿用 plugins.keyring 的做法，绕开这个歧义。本仓其他安全开关
用指针，是因为它们的未声明值有明确的安全一侧。

url 必须 https、启用时 cache 必须非空、refresh_interval_ms 必须为正。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: CLI —— `agent plugins trustlist sign/refresh/show`

**Files:**
- Create: `internal/cli/plugins_trustlist_command.go`
- Modify: `internal/cli/plugins_command.go`（第 715 行附近加一行 `cmd.AddCommand(newPluginsTrustlistCommand(out))`）
- Test: `internal/cli/plugins_trustlist_command_test.go`

**Interfaces:**
- Consumes: `trustlist.NewStore`/`Config`/`Trust`/`Status`（Task 5）；`config.PluginTrustlistConfig`（Task 6）；`sign.ParsePrivateKey`/`Sign`/`MarshalSignature`（既有）
- Produces: `func newPluginsTrustlistCommand(out io.Writer) *cobra.Command`

**这个任务是本计划的「真实调用方」。** 没有它，S1 交付的是一个没有人调用的机制——而本仓上一期正是那样藏住了两个缺陷（其中一个是安全缺陷）两个周期。

- [ ] **Step 1: 写失败的测试**

创建 `internal/cli/plugins_trustlist_command_test.go`：

```go
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// TestTrustlistSignRefusesAMalformedDocument：格式不对就不签。
//
// 签一份验证方会拒绝的文档，会教育所有人「验签是坏的」，比不签更糟。
func TestTrustlistSignRefusesAMalformedDocument(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "trustlist.json")
	if err := os.WriteFile(in, []byte(`{"serial":0}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	keyPath := filepath.Join(dir, "root.json")
	writeTestPrivateKey(t, keyPath, "root-test")
	out := filepath.Join(dir, "trustlist.sig")

	var buf bytes.Buffer
	cmd := newPluginsTrustlistCommand(&buf)
	cmd.SetArgs([]string{"sign", "--in", in, "--key", keyPath, "--out", out})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err == nil {
		t.Fatal("格式非法的清单被签了")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("拒绝之后仍然写出了 .sig 文件")
	}
}

// TestTrustlistSignRefusesAnInconsistentPrivateKey：私钥的两半不自洽时必须
// 当场失败。
//
// sign.ParsePrivateKey 的文档写明它不检查这一点：一个手工编辑过的私钥能愉快地
// 签出谁也验不过的签名。自验是唯一能当场发现它的地方——否则这个错误要等到
// 用户机器上才现形。
func TestTrustlistSignRefusesAnInconsistentPrivateKey(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "trustlist.json")
	if err := os.WriteFile(in, validTrustlistJSON(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	keyPath := filepath.Join(dir, "root.json")
	writeInconsistentPrivateKey(t, keyPath, "root-test")
	out := filepath.Join(dir, "trustlist.sig")

	var buf bytes.Buffer
	cmd := newPluginsTrustlistCommand(&buf)
	cmd.SetArgs([]string{"sign", "--in", in, "--key", keyPath, "--out", out})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err == nil {
		t.Fatal("两半不自洽的私钥签出的签名被接受了")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("自验失败之后仍然写出了 .sig 文件")
	}
}

// --- 测试辅助 -----------------------------------------------------------

func writeTestPrivateKey(t *testing.T, path string, id sign.KeyID) {
	t.Helper()
	_, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	data, err := sign.MarshalPrivateKey(id, priv)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// writeInconsistentPrivateKey 造一把两半对不上的私钥：seed 取 A 的，公钥半边
// 取 B 的。它能通过 ParsePrivateKey 的全部形状检查，签出的签名却验不过。
func writeInconsistentPrivateKey(t *testing.T, path string, id sign.KeyID) {
	t.Helper()
	_, privA, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pubB, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	broken := append([]byte(nil), privA...)
	copy(broken[32:], pubB) // 后 32 字节是公钥半边
	data, err := sign.MarshalPrivateKey(id, broken)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

func validTrustlistJSON(t *testing.T) []byte {
	t.Helper()
	pub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	entry, err := sign.MarshalKeyEntry("dev-abc", pub)
	if err != nil {
		t.Fatalf("MarshalKeyEntry: %v", err)
	}
	var entryMap map[string]any
	if err := json.Unmarshal(entry, &entryMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data, err := json.Marshal(map[string]any{
		"serial":     1,
		"issued_at":  "2026-09-05T02:00:00Z",
		"expires_at": "2026-10-05T02:00:00Z",
		"keyring":    map[string]any{"keys": []any{entryMap}},
		"publishers": []any{map[string]any{"key_id": "dev-abc", "display_name": "张三"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestTrustlistShowReportsUnavailableOnAnEmptyCache(t *testing.T) {
	var buf bytes.Buffer
	cmd := newPluginsTrustlistCommand(&buf)
	cmd.SetArgs([]string{"show", "--cache", t.TempDir()})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// 空缓存不是错误——它是全新安装的正常状态。命令要打印状态，不是崩掉。
	if err := cmd.Execute(); err != nil {
		t.Fatalf("show on an empty cache: %v", err)
	}
	if !strings.Contains(buf.String(), "unavailable") {
		t.Errorf("输出里没说状态是 unavailable：%s", buf.String())
	}
}
```

- [ ] **Step 2: 跑测试确认它失败**

```bash
go test ./internal/cli/ -run TestTrustlist -v
```

预期：编译失败，`undefined: newPluginsTrustlistCommand`。

- [ ] **Step 3: 实现**

创建 `internal/cli/plugins_trustlist_command.go`：

```go
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stardust/legion-agent/internal/plugin/sign"
	"github.com/stardust/legion-agent/internal/plugin/trustlist"
)

// newPluginsTrustlistCommand 组装 `agent plugins trustlist`：官方信任清单的
// 发布侧（sign）与运维侧（refresh / show）。
//
// 它单独一个文件，而不是加进 plugins_command.go：那个文件已经 2200 行了。
func newPluginsTrustlistCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trustlist",
		Short: "Publish, refresh and inspect the official plugin trustlist",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newTrustlistSignCommand(out))
	cmd.AddCommand(newTrustlistRefreshCommand(out))
	cmd.AddCommand(newTrustlistShowCommand(out))
	return cmd
}

// newTrustlistSignCommand 组装 `agent plugins trustlist sign`，在发布者本机
// 用 root 私钥签一份清单。
func newTrustlistSignCommand(out io.Writer) *cobra.Command {
	var inPath, keyPath, outPath string
	cmd := &cobra.Command{
		Use:   "sign",
		Short: "Sign a trustlist document with the root private key",
		Long: "Sign a trustlist document with the root private key.\n\n" +
			"The document is fully validated before it is signed: signing a document the verifier\n" +
			"would reject teaches everyone that verification is broken, which is worse than not\n" +
			"signing at all. The signature is then checked against the embedded root public key\n" +
			"before it is written.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runTrustlistSign(out, inPath, keyPath, outPath)
		},
	}
	cmd.Flags().StringVar(&inPath, "in", "", "the trustlist document to sign")
	cmd.Flags().StringVar(&keyPath, "key", "", "the root private key file")
	cmd.Flags().StringVar(&outPath, "out", "", "where to write the signature document")
	mustMarkFlagRequired(cmd, "in")
	mustMarkFlagRequired(cmd, "key")
	mustMarkFlagRequired(cmd, "out")
	return cmd
}

// runTrustlistSign 校验、签名、自验、写出，顺序不可调换。
//
// 自验（第 3 步）不是多余的：sign.ParsePrivateKey 明确不检查私钥的两半是否
// 自洽——一份手工编辑过的私钥能愉快地签出谁也验不过的签名。这里是唯一能当场
// 发现它的地方，否则这个错误要等到用户机器上才现形，而那时报出来的是「清单
// 不可信」，指向的方向完全错误。
func runTrustlistSign(out io.Writer, inPath, keyPath, outPath string) error {
	listData, err := os.ReadFile(inPath)
	if err != nil {
		return fmt.Errorf("plugins trustlist sign: read %s: %w", inPath, err)
	}
	doc, err := trustlist.ParseDocument(listData)
	if err != nil {
		return fmt.Errorf("plugins trustlist sign: %s is not a valid trustlist, so it will not be "+
			"signed: %w", inPath, err)
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("plugins trustlist sign: read %s: %w", keyPath, err)
	}
	keyID, priv, err := sign.ParsePrivateKey(keyData)
	if err != nil {
		return fmt.Errorf("plugins trustlist sign: %w", err)
	}
	sig, err := sign.Sign(priv, keyID, listData)
	if err != nil {
		return fmt.Errorf("plugins trustlist sign: %w", err)
	}
	sigData, err := sign.MarshalSignature(sig)
	if err != nil {
		return fmt.Errorf("plugins trustlist sign: %w", err)
	}
	// 自验：走的是用户机器上那条一模一样的路。
	if _, err := trustlist.VerifyDocument(listData, sigData); err != nil {
		return fmt.Errorf("plugins trustlist sign: the signature this command just produced does not "+
			"verify against the embedded root public key, so it was NOT written. Either --key is not "+
			"the root key, or its two halves disagree (an Ed25519 private key is a seed followed by "+
			"the public key it derives, and nothing stops a hand-edited file from pairing a seed with "+
			"someone else's public half): %w", err)
	}
	if err := os.WriteFile(outPath, sigData, 0o644); err != nil {
		return fmt.Errorf("plugins trustlist sign: write %s: %w", outPath, err)
	}
	fmt.Fprintf(out, "signed %s (serial %d, expires %s) with key %q -> %s\n",
		inPath, doc.Serial, doc.ExpiresAt.Format("2006-01-02"), keyID, outPath)
	return nil
}

func newTrustlistRefreshCommand(out io.Writer) *cobra.Command {
	var url, cacheDir string
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Fetch the trustlist now and report what happened",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := trustlist.NewStore(trustlist.Config{URL: url, CacheDir: cacheDir})
			if err != nil {
				return fmt.Errorf("plugins trustlist refresh: %w", err)
			}
			trust, refreshErr := store.Refresh(cmd.Context())
			printTrust(out, trust)
			if refreshErr != nil {
				// 打印之后再报错：操作者既要看到这次失败了，也要看到手上
				// 那份还能不能用。
				return fmt.Errorf("plugins trustlist refresh: %w", refreshErr)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "the trustlist document's https address")
	cmd.Flags().StringVar(&cacheDir, "cache", "", "the trustlist cache directory")
	mustMarkFlagRequired(cmd, "url")
	mustMarkFlagRequired(cmd, "cache")
	return cmd
}

func newTrustlistShowCommand(out io.Writer) *cobra.Command {
	var cacheDir string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the cached trustlist's status without touching the network",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// show 需要一个 Store 但不发请求，所以 URL 只是个占位——它必须
			// 通过 NewStore 的形状校验，而 show 永远不会用到它。
			store, err := trustlist.NewStore(trustlist.Config{
				URL:      "https://localhost/trustlist.json",
				CacheDir: cacheDir,
			})
			if err != nil {
				return fmt.Errorf("plugins trustlist show: %w", err)
			}
			trust, readErr := store.Current()
			printTrust(out, trust)
			if readErr != nil {
				// 空缓存是全新安装的正常状态，不是错误——printTrust 已经把
				// "unavailable" 打出来了。真正损坏的缓存值得一行说明。
				fmt.Fprintf(out, "note: %v\n", readErr)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cacheDir, "cache", "", "the trustlist cache directory")
	mustMarkFlagRequired(cmd, "cache")
	return cmd
}

// printTrust 打印一份 Trust，供 refresh 与 show 共用——两条命令报告同一件事，
// 用两种格式说会让操作者以为它们看的是两个东西。
func printTrust(out io.Writer, trust trustlist.Trust) {
	fmt.Fprintf(out, "status: %s\n", trust.Status)
	if trust.Keyring == nil {
		fmt.Fprintln(out, "no trust set on this machine; every plugin will be treated as unregistered")
		return
	}
	fmt.Fprintf(out, "serial: %d\nissued: %s\nexpires: %s\n",
		trust.Serial,
		trust.IssuedAt.Format("2006-01-02 15:04:05Z07:00"),
		trust.ExpiresAt.Format("2006-01-02 15:04:05Z07:00"))

	ids := trust.Keyring.IDs()
	revoked := trust.Keyring.RevokedIDs()
	revokedSet := make(map[sign.KeyID]bool, len(revoked))
	for _, id := range revoked {
		revokedSet[id] = true
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		label := string(id)
		if p, ok := trust.Publishers[id]; ok {
			label = fmt.Sprintf("%s (%s)", p.DisplayName, id)
		}
		if revokedSet[id] {
			label += " [REVOKED]"
		}
		names = append(names, label)
	}
	sort.Strings(names)
	fmt.Fprintf(out, "publishers (%d):\n", len(names))
	for _, n := range names {
		fmt.Fprintf(out, "  %s\n", n)
	}
	// 累积集里可能有已经不在当前清单 keys 里的 key_id——那正是「撤销永不
	// 遗忘」的结果，值得单独说一句，否则数字对不上会让人以为哪里坏了。
	extra := 0
	for _, id := range revoked {
		found := false
		for _, known := range ids {
			if known == id {
				found = true
				break
			}
		}
		if !found {
			extra++
		}
	}
	if extra > 0 {
		fmt.Fprintf(out, "%d revoked key(s) no longer listed in the current trustlist are still "+
			"refused on this machine (revocations are never forgotten)\n", extra)
	}
	_ = strings.TrimSpace
	_ = context.Background
	_ = errors.New
}
```

实现完成后**删掉**文件末尾那三行 `_ =` 占位（它们只是为了让草稿编译；最终代码里不该有）。相应地，如果 `context`、`errors`、`strings` 最后没有被用到，把它们从 import 里删掉。

在 `internal/cli/plugins_command.go` 第 715 行（`cmd.AddCommand(newPluginsCacheCommand(out))` 之后）加一行：

```go
	cmd.AddCommand(newPluginsTrustlistCommand(out))
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/cli/ -run TestTrustlist -v
go build ./...
```

预期：测试 PASS，构建成功。

- [ ] **Step 5: 手工验一遍命令能跑**

```bash
go run ./cmd/agent plugins trustlist --help
go run ./cmd/agent plugins trustlist show --cache "$(mktemp -d)"
```

预期：第一条列出 `sign`/`refresh`/`show`；第二条打印 `status: unavailable` 与那句「every plugin will be treated as unregistered」。

- [ ] **Step 6: 提交**

```bash
gofmt -l . && go vet ./internal/cli/
git add internal/cli/
git commit -m "$(cat <<'EOF'
feat(cli): agent plugins trustlist sign/refresh/show

sign 在发布者本机用 root 私钥签清单，四步不可调换：校验 → 签 → **用内嵌
root 公钥自验** → 写出。自验不是多余的：sign.ParsePrivateKey 明确不检查
私钥两半是否自洽，一份手工编辑过的私钥能签出谁也验不过的签名，而那个错误
不自验就要等到用户机器上才现形，且报出来的是「清单不可信」，方向完全错。

格式不对就不签：签一份验证方会拒绝的文档，会教育所有人「验签是坏的」。

refresh/show 是这一期唯一的真实调用方，端到端跑完取回→验签→serial→撤销
累积→落盘→装配。没有它，S1 交付的就是一个没有调用方的机制——上一期正是
那样藏住了两个缺陷两个周期。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: serve 接线与后台刷新

**Files:**
- Modify: `internal/cli/plugins_command.go`（新增 `resolvePluginTrustlist`，与既有的 `resolvePluginKeyring` 放在一起）
- Modify: serve 的装配处（用下面的命令定位）
- Test: `internal/cli/plugins_command_test.go`（新增用例）

**Interfaces:**
- Consumes: `trustlist.NewStore`/`Store`（Task 5）；`config.PluginTrustlistConfig`（Task 6）
- Produces:
  - `func resolvePluginTrustlist(cfg config.PluginsConfig) (*trustlist.Store, error)`
  - `func runTrustlistRefreshLoop(ctx context.Context, store *trustlist.Store, interval time.Duration, log func(string, ...any))`

- [ ] **Step 1: 找到 serve 的装配点**

```bash
grep -rn "resolvePluginKeyring" --include=*.go internal/ | grep -v _test.go
```

把 `resolvePluginTrustlist` 的调用放在同一个函数里，紧挨 keyring 的解析之后——两者是同一件事的两半，分开会让「这个部署到底信任什么」需要看两个地方。

- [ ] **Step 2: 写失败的测试**

在 `internal/cli/plugins_command_test.go` 末尾追加：

```go
// TestResolveTrustlistIsNilWhenNotConfigured：没配 url 就没有 Store，
// 而不是一个指向空 URL 的坏 Store。
func TestResolveTrustlistIsNilWhenNotConfigured(t *testing.T) {
	store, err := resolvePluginTrustlist(config.PluginsConfig{})
	if err != nil {
		t.Fatalf("resolvePluginTrustlist: %v", err)
	}
	if store != nil {
		t.Error("没配置 trustlist 时却造出了一个 Store")
	}
}

// TestResolveTrustlistFailsLoudOnABadCacheDir：配了但落不了地，serve 必须
// 起不来，而不是降级成「没有远程清单」。
//
// 这是 fail-loud：一个静默降级的部署会以为自己在收撤销，实际上永远收不到。
func TestResolveTrustlistFailsLoudOnABadCacheDir(t *testing.T) {
	// 把一个普通文件当作缓存目录：MkdirAll 会失败。
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := resolvePluginTrustlist(config.PluginsConfig{
		Trustlist: config.PluginTrustlistConfig{
			URL:               "https://example.com/trust/trustlist.json",
			Cache:             f,
			RefreshIntervalMs: 1000,
		},
	})
	if err == nil {
		t.Fatal("落不了地的 trustlist 配置被静默接受了")
	}
}

// TestRefreshLoopStopsWithItsContext：ctx 取消后循环必须退出，否则 serve
// 停不下来。
func TestRefreshLoopStopsWithItsContext(t *testing.T) {
	store, err := trustlist.NewStore(trustlist.Config{
		URL:      "https://localhost/trustlist.json",
		CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTrustlistRefreshLoop(ctx, store, time.Hour, func(string, ...any) {})
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消 5 秒后刷新循环仍未退出")
	}
}
```

（按需在该测试文件里补 `context`、`os`、`path/filepath`、`time`、`trustlist` 的 import。）

- [ ] **Step 3: 跑测试确认它失败**

```bash
go test ./internal/cli/ -run TestResolveTrustlist -v
```

预期：编译失败，`undefined: resolvePluginTrustlist`。

- [ ] **Step 4: 实现**

在 `internal/cli/plugins_command.go` 里，`resolvePluginKeyring` 之后加：

```go
// resolvePluginTrustlist 把 cfg 里的远程清单配置变成一个 Store，没配置时返回
// (nil, nil)。
//
// 配置了却造不出来（缓存目录建不了、URL 形状不对）是**错误**，不是降级：一个
// 静默降级的部署会以为自己在收撤销，实际上永远收不到——而这个部署恰恰是那个
// 特意配了远程清单的部署。它比完全没配的部署更需要知道自己没在收。
func resolvePluginTrustlist(cfg config.PluginsConfig) (*trustlist.Store, error) {
	if !cfg.Trustlist.Enabled() {
		return nil, nil
	}
	store, err := trustlist.NewStore(trustlist.Config{
		URL:      cfg.Trustlist.URL,
		CacheDir: cfg.Trustlist.Cache,
	})
	if err != nil {
		return nil, fmt.Errorf("plugins.trustlist: %w", err)
	}
	return store, nil
}

// runTrustlistRefreshLoop 立刻取回一次，之后每 interval 一次，直到 ctx 结束。
//
// 启动时那一次**不阻塞 serve 启动**（调用方在 goroutine 里跑它），因为启动期的
// 网络故障不该让 agent 起不来——手上如果有缓存，它照样能工作。
//
// 每次的结果都记日志，成功与失败都记：一个默默失败了三个月的刷新循环，与没有
// 刷新循环是同一件事，而只有日志能把两者分开。
func runTrustlistRefreshLoop(ctx context.Context, store *trustlist.Store, interval time.Duration,
	log func(string, ...any)) {
	refresh := func() {
		trust, err := store.Refresh(ctx)
		if err != nil {
			if errors.Is(err, trustlist.ErrSerialRegressed) || errors.Is(err, trustlist.ErrUntrustedList) {
				// 这两类不是网络抖动：它们意味着拿到的东西不是我签的那一份，
				// 或者有人在喂旧清单。重试不会让它们变好。
				log("plugin trustlist REFUSED a fetched list (status now %s): %v", trust.Status, err)
				return
			}
			log("plugin trustlist refresh failed, continuing with status %s: %v", trust.Status, err)
			return
		}
		log("plugin trustlist refreshed: status=%s serial=%d expires=%s",
			trust.Status, trust.Serial, trust.ExpiresAt.Format(time.RFC3339))
	}

	refresh()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}
```

在 serve 的装配处（Step 1 定位到的函数里，keyring 解析之后）加：

```go
	trustStore, err := resolvePluginTrustlist(cfg.Plugins)
	if err != nil {
		return err
	}
	if trustStore != nil {
		interval := time.Duration(cfg.Plugins.Trustlist.RefreshIntervalMs) * time.Millisecond
		go runTrustlistRefreshLoop(ctx, trustStore, interval, func(format string, args ...any) {
			log.Printf(format, args...)
		})
	}
```

（`log` 换成该函数里既有的日志器；用 `grep -n "log\." <那个文件>` 看它用的是什么。）

- [ ] **Step 5: 跑测试确认通过**

```bash
go test ./internal/cli/ -v
go build ./...
```

预期：都通过。

- [ ] **Step 6: 提交**

```bash
gofmt -l . && go vet ./...
git add internal/cli/
git commit -m "$(cat <<'EOF'
feat(cli): serve 接线 trustlist 与后台刷新循环

配了远程清单却造不出 Store（缓存目录建不了、URL 形状不对）是错误不是降级：
静默降级的部署会以为自己在收撤销，实际永远收不到——而那恰恰是特意配了远程
清单的部署，它比没配的更需要知道。

启动时那次取回在 goroutine 里跑，不阻塞 serve 启动：启动期网络故障不该让
agent 起不来，手上有缓存就照样能工作。

成功与失败都记日志，且把 ErrSerialRegressed / ErrUntrustedList 与网络抖动
分开记——前两者意味着拿到的不是我签的那份，或有人在喂旧清单。一个默默失败
了三个月的刷新循环，与没有刷新循环是同一件事。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: 真机端到端验证（**验收条件，不可跳过**）

这一步存在的理由写在计划开头：上一期「机制交付了但没有调用方」藏住了两个缺陷（其中一个是安全缺陷）两个周期。单元测试全绿证明不了这条链在真实的 GitHub、真实的文件系统、真实的时钟上能走通。

**Files:**
- Create: `trust/trustlist.json`、`trust/trustlist.sig`（仓库里的真实发布物）

- [ ] **Step 1: 造第一份真实清单**

在仓库根建 `trust/trustlist.json`。`serial` 从 1 开始，`keys` 先只放一个测试用的开发者钥匙（跑 `go run ./cmd/agent plugins keygen --key-id dev-selftest --private-key /tmp/dev-selftest.json` 生成，私钥用完即弃）：

```json
{
  "serial": 1,
  "issued_at": "<今天，RFC3339>",
  "expires_at": "<一个月后，RFC3339>",
  "keyring": {
    "keys": [
      { "id": "dev-selftest", "algorithm": "ed25519", "public_key": "<keygen 打印的 public_key>" }
    ]
  },
  "publishers": [
    { "key_id": "dev-selftest", "display_name": "自检用钥匙", "contact": "" }
  ]
}
```

- [ ] **Step 2: 用 root 私钥签它**

```bash
go run ./cmd/agent plugins trustlist sign \
  --in trust/trustlist.json \
  --key "$HOME/.legion/root-key.json" \
  --out trust/trustlist.sig
```

预期：打印 `signed trust/trustlist.json (serial 1, expires ...) with key "root-2026" -> trust/trustlist.sig`。

- [ ] **Step 3: 提交推送，让它在 GitHub 上真的可取**

```bash
git add trust/
git commit -m "$(cat <<'EOF'
feat(trust): 第一份官方信任清单（serial 1）

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
git push -u origin HEAD
```

- [ ] **Step 4: 从真实 URL 拉一次**

分支合并进 master 之后（或先用分支名做 ref 试）：

```bash
go run ./cmd/agent plugins trustlist refresh \
  --url "https://raw.githubusercontent.com/jxncyjq/stardust-agent-server/master/trust/trustlist.json" \
  --cache /tmp/legion-trustlist-check
```

预期输出包含：

```
status: fresh
serial: 1
publishers (1):
  自检用钥匙 (dev-selftest)
```

**如果这里报的是 404**：先确认不是鉴权问题——GitHub 对带无效凭据的请求返回 404 而不是 401。用 `curl -sI <那个 URL>` 对照；私有仓库的 raw 地址需要 token，而这套机制不带 token（清单是公开的）。若仓库是私有的，这一步说明**发布落点必须换成公开可取的地方**，那是一个需要回到 spec 的发现，不是一个可以绕过的障碍。

- [ ] **Step 5: 验「不发新版也能换清单」**

改 `trust/trustlist.json`：`serial` 改成 2，再加一个 publisher。重签、提交、推送。**不重新编译任何东西**，再跑一次 Step 4 的 refresh。

预期：`serial: 2`，新的 publisher 出现在列表里。这就是 D-2 的全部要求。

- [ ] **Step 6: 验防回滚**

把 `trust/trustlist.json` 改回 serial 1 的那份内容（`git show HEAD~1:trust/trustlist.json > trust/trustlist.json`，签名同理），推送，再 refresh。

预期：命令**失败**，错误里出现 `serial 1 but this machine has already seen 2`，且 `status:` 那一行仍然显示 serial 2 —— 拒绝之后手上还是新的那份。

验完把 serial 2 的版本恢复推回去。

- [ ] **Step 7: 验撤销永不遗忘**

serial 3：把 `dev-selftest` 加进 `keyring.revoked`。推送、refresh，确认 `show` 里它带 `[REVOKED]`。

serial 4：把 `revoked` 整段删掉，`keys` 里也把 `dev-selftest` 删掉（换一把新钥匙进去，否则 `keys` 会空，`ParseKeyring` 会拒）。推送、refresh、`show`。

预期：输出末尾出现 `1 revoked key(s) no longer listed in the current trustlist are still refused on this machine`。**这是这整套机制里最重要的一条不变量**，也是唯一一条只有真机才能验的（它跨越了三次网络取回与两次落盘）。

- [ ] **Step 8: 记录结果**

把 Step 4–7 的实际输出贴进 `docs/superpowers/plans/2026-09-05-plugin-trustlist-distribution.md` 的末尾（新开一节「真机验证记录」），包括任何**没有**按预期发生的事。上一期的教训是：真机验证只有在记录了实际输出时才算数，「跑过了」不是证据。

---

## Self-Review

**Spec 覆盖检查**（逐节对到任务）：

| spec 节 | 任务 |
|---------|------|
| §2 文档格式、解析规则、publishers 完整性 | Task 1 |
| §3 root 公钥内嵌、panic、轮换支持 | Task 0 + Task 1 |
| §4 serial 单调、相等且内容不同、缓存损坏的代价 | Task 5 + Task 3 |
| §5.1 撤销永不遗忘 | Task 2（单元）+ Task 5（端到端）+ Task 9 Step 7（真机） |
| §5.2 缓存三文件、存原始字节 | Task 3 |
| §5.3 三种状态、unavailable 时 Keyring 为 nil | Task 5 |
| §5.4 损坏不重建、写顺序、Windows 锁 | Task 3 |
| §6.1 不复用 fetch.Fetch、https、上限、状态码 | Task 4 |
| §6.2 配置项、空 url = 关闭、sig URL 推导 | Task 6 + Task 4 |
| §6.3 启动拉一次、定期、手动命令 | Task 7 + Task 8 |
| §6.4 上限数值 | Task 4 |
| §7 发布侧 `trustlist sign` 四步与自验 | Task 7 |
| §8 包边界与接口、哨兵 | Task 1 / 5 |
| §9 测试清单 | 分布在 Task 1–5、7、8 |
| §10 不做的事 | 不产生任务（正确） |

**spec §6.3 提到的「按需拉取不做」**：本计划没有任何任务实现它，符合。

**类型一致性**：`Document`、`Publisher`、`Trust`、`Status`、`revokedSet`、`cache`、`Store`、`Config` 在各任务间的字段与方法名逐处对过，一致。`sigURL`（Task 4）被 `NewStore`（Task 5）调用，签名一致。`assembleKeyring`（Task 2）被 `Store.assemble`（Task 5）调用，签名一致。

**已知的一处粗糙**：Task 7 的实现代码末尾留了三行 `_ =` 占位以保证草稿可编译，步骤里明确要求删除并清理 import。实施者若忘记，`go vet` 不会报，但 code review 会看到——已在步骤文字里点名。

**Task 8 的 serve 装配点是 grep 出来的，不是写死的行号**：那个函数的位置会随其他改动漂移，写死行号比写查找命令更容易过期。

---

## 执行方式

Plan complete and saved to `docs/superpowers/plans/2026-09-05-plugin-trustlist-distribution.md`. 两种执行方式：

**1. Subagent-Driven（推荐）** —— 每个任务派一个新的 subagent，任务之间做 review，迭代快

**2. Inline Execution** —— 在当前会话里按 executing-plans 批量执行，带检查点

注意 **Task 0 必须由人先做**（生成 root 密钥对），两种方式都一样。
