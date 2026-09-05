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
// 它不是（也不能是）性能保护：本包的解析函数拿到的 data []byte 已经整个读进
// 内存，不会也不该在这一层做文档体积限制——那是取回方的职责：fetch.go 里的
// fetchBytes 以流式方式在读进内存之前强制 maxListBytes / maxSigBytes（读到
// 上限+1 字节即判定超限）。这里限的只是 publishers 条目数：真实的登记
// 规模远小于这个数，而一份塞满条目的文档更像是有人在试探解析器，是一条
// 「这份文档不像是我签的」的判据。
const maxPublishers = 10000

//go:embed root_keys.json
var rootKeysJSON []byte

var (
	rootOnce     sync.Once
	rootTrust    *sign.Keyring
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
