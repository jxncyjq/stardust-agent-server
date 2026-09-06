package trustlist

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

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
//
// # 并发
//
// revokedSet **不是并发安全的**，而且刻意不在方法里各加一把内部锁。它的真实用法
// 是 read-modify-write 复合操作：parseRevokedSet 读出来、mergeFrom 改、marshal
// 写回。方法级的锁只能让单次方法调用不崩，挡不住两个复合操作交错造成的丢更新，
// 而丢更新正是「一条已经见过的撤销消失」——这个类型唯一要防的事。也就是说方法级
// 锁只会把一次响亮的 fatal error: concurrent map read and map write 换成一次静默
// 的数据丢失，比不加锁更糟。
//
// 真正串行化这个复合操作的责任在这个类型之外：凡是跨 goroutine 共享 revokedSet
// 的调用方，都必须自己串行化**整个 read-modify-write 序列**（读 → 改 → 写回），
// 而不只是单次方法调用；凡是跨进程共享同一份 revoked-ever.json 的地方，同样的
// 序列还要被一把跨进程的锁罩住。
type revokedSet struct {
	entries map[sign.KeyID]rawRevocationEntry
}

// rawRevocationEntry 与 keyring 文档里 revoked 数组的条目同形，也是
// revoked-ever.json 落盘的形状。
//
// 字段名与类型与 sign 包的 revoked 条目（sign.rawRevocation）逐字一致是必须的：
// assembleKeyring 会把这些条目拼回一份 keyring 文档交给 sign.ParseKeyring，
// 形状对不上就拼不回去。tag 上比 sign 那份多出来的 omitempty 只影响编码时省略空
// 字段，不影响 sign.ParseKeyring 解码：它对空 revoked_at 有显式的 != "" 分支，
// 而 DisallowUnknownFields 只管多出来的字段、不管缺的字段。
type rawRevocationEntry struct {
	KeyID     sign.KeyID `json:"key_id"`
	RevokedAt string     `json:"revoked_at,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

// validate 检查一条撤销条目**自身的形状**：key_id 不能为空，给了 revoked_at 就
// 必须是 RFC 3339。这两条与 sign.ParseKeyring 对 keyring 文档 revoked 段的规则
// 一字对齐，不是在这里新发明第二套规则。
//
// 它只管单条目，不管集合层面的事：同一个 key_id 出现两次是集合层面的问题（要看
// 已经收了哪些条目才判得了），由 parseRevokedSet 与 mergeFrom 各自决定怎么处理。
//
// 返回的错误刻意不带来源前缀。同一条规则有两个入口——清单信封里的 keyring
// revoked 段（mergeFrom）和磁盘上的 revoked-ever.json（parseRevokedSet）——
// fail-loud 要求错误点可定位，操作者必须能从错误文本看出该去查哪一份，所以
// 前缀由调用方各自补上。
func (e rawRevocationEntry) validate() error {
	if e.KeyID == "" {
		return errors.New("has no key_id")
	}
	if e.RevokedAt != "" {
		if _, err := time.Parse(time.RFC3339, e.RevokedAt); err != nil {
			// 无法解析的时间戳会原样走进操作者用来判断一个包是否安全的那句话。
			return fmt.Errorf("key %q revoked_at %q is not RFC 3339: %w", e.KeyID, e.RevokedAt, err)
		}
	}
	return nil
}

// rawRevokedSet 是 revoked-ever.json 整个文件的形状。
//
// Revoked 是指针而不是切片，为的是把「缺了 revoked 键」与「revoked 是空数组」
// 分开：前者报错，后者合法。理由见 parseRevokedSet 的注释。
type rawRevokedSet struct {
	Revoked *[]rawRevocationEntry `json:"revoked"`
}

// keyringShape 是本包从清单的 keyring 段里需要的部分：Revoked 给 mergeFrom 与
// assembleKeyring 用；Keys 只有 assembleKeyring 用，保持 json.RawMessage 原样搬运，
// 由 sign.ParseKeyring 解析，这里不重复解析。
type keyringShape struct {
	Keys    json.RawMessage      `json:"keys"`
	Revoked []rawRevocationEntry `json:"revoked"`
}

func newRevokedSet() *revokedSet {
	return &revokedSet{entries: map[sign.KeyID]rawRevocationEntry{}}
}

// parseRevokedSet 从 revoked-ever.json 的字节读回累积集。
//
// 它和这个包里其他解析器一样严格：未知字段、尾随内容、缺了 revoked 键、空
// key_id、同一个 key_id 出现两次、同一个 JSON 键在同一层出现两次、不是 RFC 3339
// 的 revoked_at，都拒绝。而这里的理由比别处更重：一个被静默当成空集的损坏文件，
// 说的是「这台机器从没见过任何撤销」——这是这个文件能造成的最坏的谎。宁可报错让
// 调用方去决定怎么办。
//
// 缺了 revoked 键（`{}`）与 revoked 是 JSON null（`{"revoked":null}`）都算损坏，
// 一并拒绝：marshal 永远写出 revoked 键（哪怕值是空数组），所以一份没有这个键的
// 文件根本不是本包写出来的，它只可能来自外部改写或截断——正是最该报错的那种输入，
// 而把它读成空集恰好就是上面那句最坏的谎。JSON null 与缺键在这里是同一件事的两种
// 拼法。空数组 `{"revoked":[]}` 则是合法的正常状态：见过清单，但还没有任何撤销。
//
// 单条目的两条形状规则在 rawRevocationEntry.validate 里，与 sign.ParseKeyring 对
// keyring 文档 revoked 段的规则一字对齐，不是在这里新发明第二套规则，而是把同一条
// 规则挪到 revoked-ever.json 自己的入口再执行一次。挪的理由是错误点要可定位：
// 累积集只增不减，一个坏条目一旦进来就永不消失，此后每次 assembleKeyring 都会失败，
// 而那时的错误文本写的是「assemble keyring: parse keyring:」，会把操作者引去查清单，
// 真正坏掉的却是 revoked-ever.json。经正常路径坏值进不来（清单的 keyring 段在
// ParseDocument 里已由 sign.ParseKeyring 把过关），真正的入口是磁盘上这个文件被
// 手改或损坏。
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
	// 重复键要单独再走一遍字节。上面那次 Decode 看不见它：encoding/json 对同一层
	// 重复出现的键取最后一个，DisallowUnknownFields 只看键名认不认得、dec.More()
	// 只看文档之后还有没有内容，两条规则都与「同一个键出现了几次」无关。放在
	// Decode 成功之后是有意的：那时字节已经是合法 JSON，语法层面的抱怨由 Decode
	// 用它更贴切的错误说完，这里只回答重复键这一个问题。
	if err := refuseDuplicateKeys(json.NewDecoder(bytes.NewReader(data)), "revoked-ever.json"); err != nil {
		return nil, fmt.Errorf("parse revoked-ever: %w", err)
	}
	if raw.Revoked == nil {
		return nil, fmt.Errorf("parse revoked-ever: revoked-ever.json has no revoked key (or it is null); " +
			"this file always carries the key, even when the list is empty, so a missing one means the file " +
			"was rewritten or truncated, and reading it as an empty set would claim this machine has never " +
			"seen a revocation")
	}
	set := newRevokedSet()
	for i, entry := range *raw.Revoked {
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("parse revoked-ever: revoked-ever.json revoked[%d] %w", i, err)
		}
		if _, dup := set.entries[entry.KeyID]; dup {
			// 两条记录对应一个 id：哪条在生效、拒绝理由该引哪个时间戳，没有答案。
			// 静默 last-wins 会把先前那条的 reason/revoked_at 无声抹掉，而这两个
			// 字段正是这个文件必须保住的东西。
			return nil, fmt.Errorf("parse revoked-ever: key id %q is revoked twice; a revocation must name "+
				"each key once so the refusal can say when and why", entry.KeyID)
		}
		set.entries[entry.KeyID] = entry
	}
	return set, nil
}

// refuseDuplicateKeys 从 dec 读**一个** JSON 值，逐层拒绝同一个对象里出现两次的
// 键。at 是这个值在文档里的位置，用来让错误指得出是哪一层重复了。
//
// 为什么需要它：encoding/json 对同一个对象里重复出现的键取**最后一个**，而且不
// 报错。落在 revoked-ever.json 上，`{"revoked":[…真实记录…],"revoked":[]}` 会被
// 解成一个 err == nil 的空集——「这台机器从没见过任何撤销」，这个文件能造成的最坏
// 的谎；而它是唯一一种不响亮的坏法（删除、截断、`{}`、`null`、乱码、重复 key_id
// 全都当场报错）。更糟的是它自我抹除：空集顺着一次刷新写回磁盘，那条撤销就永久
// 消失，事后连取证都做不了。
//
// 逐层做而不是只查顶层：条目里重复的 key_id（`{"key_id":"被撤销的",
// "key_id":"无关的"}`）走的是同一条 last-wins 规则，后果是一条撤销无声换了主人，
// 被撤销的那把钥匙就此不再被拒。两层是同一个洞的两个位置。
//
// 它靠 Token() 走字节流，所以看得见解码器丢掉的那些键。调用方必须先让同一段字节
// 通过一次成功的 Decode：那之后字节已是合法 JSON，这里再遇到语法错误就属于不该
// 发生的情况，照样报出来而不是当成没有重复键。同理，递归深度由那次 Decode 认可
// 的形状封顶，不是由输入随意决定的。
// duplicateKeyConsequence spells out what a duplicate of this particular key
// would have cost, for the one key where the cost is the whole point of this
// scan: a repeated top-level "revoked" makes the file read as an empty set,
// which is the claim that this machine has never seen a revocation.
//
// Every other duplicate is refused for the same underlying reason (the second
// key silently replaces the first) but does not carry that specific meaning,
// so it gets no clause rather than a borrowed one. Naming the "revoked" case
// on a duplicated "reason" would send the reader hunting for a repeat they do
// not have.
func duplicateKeyConsequence(at, key string) string {
	if at == revokedFileName && key == "revoked" {
		return ", and a duplicated \"revoked\" reads this file as an empty set — " +
			"the claim that this machine has never seen a revocation"
	}
	return ""
}

func refuseDuplicateKeys(dec *json.Decoder, at string) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("re-reading %s to look for duplicate keys: %w", at, err)
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		// 标量（字符串、数字、true/false/null）：没有键可重复。
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return fmt.Errorf("re-reading %s to look for duplicate keys: %w", at, err)
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("re-reading %s to look for duplicate keys: an object key came back as "+
					"%T, not a string", at, keyTok)
			}
			if _, dup := seen[key]; dup {
				// The consequence named here has to match the key that was actually
				// duplicated. Spelling out the top-level "revoked" case regardless of
				// which key repeated sends the reader looking for a duplicate they do
				// not have — the same defect this scan exists to stop, one level up.
				return fmt.Errorf("%s names %q twice; Go's JSON decoder keeps only the last one, so the "+
					"second key silently replaces everything the first one carried%s",
					at, key, duplicateKeyConsequence(at, key))
			}
			seen[key] = struct{}{}
			if err := refuseDuplicateKeys(dec, at+"."+key); err != nil {
				return err
			}
		}
	case '[':
		for i := 0; dec.More(); i++ {
			if err := refuseDuplicateKeys(dec, fmt.Sprintf("%s[%d]", at, i)); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("re-reading %s to look for duplicate keys: a value started with %q", at, delim)
	}
	// 吃掉收尾的 } 或 ]，好让调用者的 dec.More() 问的是外层还有没有内容。
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("re-reading %s to look for duplicate keys: %w", at, err)
	}
	return nil
}

// mergeFrom 把 keyringRaw（清单信封里那段 keyring 文档）的 revoked 条目并进
// 累积集。已经在集合里的 key_id 保留**先见到的那条**记录，不被后来的覆盖：
// 撤销时间与理由是操作者当时写下的事实，后一份清单把它改短、改空或改晚，
// 都只会让拒绝理由变得更没用。
//
// 每条都过一遍 rawRevocationEntry.validate，与 parseRevokedSet 同一套形状规则。
// 写入侧和读取侧必须同严，否则 mergeFrom 收下一个坏 revoked_at、marshal 把它写
// 出去、parseRevokedSet 下次读不回来——这个类型自己的写出结果通不过自己的读入。
//
// 同一份清单内重复的 key_id 不报错，走的还是上面那条「先见到的胜出」。keyringRaw
// 与 assembleKeyring 收的是同一段字节，因而背着同一条调用方义务：必须是已通过
// sign.ParseKeyring 的那一段（即 Document.KeyringRaw）。sign.ParseKeyring 对「同一个
// key 在 revoked 里出现两次」是硬拒的，所以重复条目只可能来自违反那条义务的调用方；
// 真出现时第一条胜出，与跨清单的答案是同一个，不会让任何已经收下的记录被改写。
// 这与 parseRevokedSet 硬拒重复并不矛盾：那边的文件是撤销的唯一底本、上游没有任何
// 人校验过它，两条互相矛盾的记录无从裁决。
func (s *revokedSet) mergeFrom(keyringRaw json.RawMessage) error {
	var shape keyringShape
	if err := json.Unmarshal(keyringRaw, &shape); err != nil {
		return fmt.Errorf("merge revocations: %w", err)
	}
	for i, entry := range shape.Revoked {
		if err := entry.validate(); err != nil {
			return fmt.Errorf("merge revocations: the manifest keyring's revoked[%d] %w", i, err)
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
//
// entries 由 make 造出，永远非 nil，所以撤销为空时写出的是 "revoked": []
// 而不是 "revoked": null——parseRevokedSet 把缺键与 null 都当损坏拒绝。
func (s *revokedSet) marshal() ([]byte, error) {
	entries := make([]rawRevocationEntry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].KeyID < entries[j].KeyID })
	data, err := json.MarshalIndent(rawRevokedSet{Revoked: &entries}, "", "  ")
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
// keys 段原样搬运（json.RawMessage），不重新编码。revoked 段是累积集与本清单
// revoked 段的并集，这里逐条并一次。它**不依赖**调用顺序：即使调用方没有先调
// mergeFrom，本清单自己的撤销也会在这里被并进来，装配结果不会漏。已经在累积集里
// 的 key_id 保留累积集那条（先见到的记录胜出，与 mergeFrom 同规则）。
//
// 凡是调用 assembleKeyring 的地方，keyringRaw 都必须是已经通过 sign.ParseKeyring
// 的那一段（即 Document.KeyringRaw）。这是调用方的义务，签名收的是裸
// json.RawMessage，类型上约束不了。理由是上面那次并入顺带做了去重（同一个 key_id
// 只留一条），而 sign.ParseKeyring 对「同一个 key 在 revoked 里出现两次」是硬拒的
// ——传进来一段未经校验的 keyring，这里的去重会把它那条规则悄悄消解掉。这里的去重
// 只为「累积集优先」服务，不承担校验职责。
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
