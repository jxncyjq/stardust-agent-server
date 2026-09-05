package trustlist

import (
	"bytes"
	"encoding/json"
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
// 真正串行化这个复合操作的地方在这个类型之外：同进程内由 Store 用自己的互斥锁
// 串行 Refresh，跨进程由缓存目录锁挡住。这两处都还没有编写（后续任务），本包目前
// 也没有任何调用方。第一个跨 goroutine 共享 revokedSet 的调用方必须自己串行化
// **整个 read-modify-write 序列**（读 → 改 → 写回），而不只是单次方法调用。
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

type rawRevokedSet struct {
	Revoked []rawRevocationEntry `json:"revoked"`
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
// 它和这个包里其他解析器一样严格：未知字段、尾随内容、空 key_id、同一个 key_id
// 出现两次、不是 RFC 3339 的 revoked_at，都拒绝。而这里的理由比别处更重：一个被
// 静默当成空集的损坏文件，说的是「这台机器从没见过任何撤销」——这是这个文件能
// 造成的最坏的谎。宁可报错让调用方去决定怎么办。
//
// 后两条与 sign.ParseKeyring 对 keyring 文档 revoked 段的规则一字对齐，不是在这里
// 新发明第二套规则，而是把同一条规则挪到 revoked-ever.json 自己的入口再执行一次。
// 挪的理由是错误点要可定位：累积集只增不减，一个坏条目一旦进来就永不消失，此后
// 每次 assembleKeyring 都会失败，而那时的错误文本写的是「assemble keyring: parse
// keyring:」，会把操作者引去查清单，真正坏掉的却是 revoked-ever.json。经正常路径
// 坏值进不来（清单的 keyring 段在 ParseDocument 里已由 sign.ParseKeyring 把过关），
// 真正的入口是磁盘上这个文件被手改或损坏。
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
		if _, dup := set.entries[entry.KeyID]; dup {
			// 两条记录对应一个 id：哪条在生效、拒绝理由该引哪个时间戳，没有答案。
			// 静默 last-wins 会把先前那条的 reason/revoked_at 无声抹掉，而这两个
			// 字段正是这个文件必须保住的东西。
			return nil, fmt.Errorf("parse revoked-ever: key id %q is revoked twice; a revocation must name "+
				"each key once so the refusal can say when and why", entry.KeyID)
		}
		if entry.RevokedAt != "" {
			if _, err := time.Parse(time.RFC3339, entry.RevokedAt); err != nil {
				// 无法解析的时间戳会原样走进操作者用来判断一个包是否安全的那句话。
				return nil, fmt.Errorf("parse revoked-ever: key %q revoked_at %q is not RFC 3339: %w",
					entry.KeyID, entry.RevokedAt, err)
			}
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
// keys 段原样搬运（json.RawMessage），不重新编码。revoked 段是累积集与本清单
// revoked 段的并集，这里逐条并一次。它**不依赖**调用顺序：即使调用方没有先调
// mergeFrom，本清单自己的撤销也会在这里被并进来，装配结果不会漏。已经在累积集里
// 的 key_id 保留累积集那条（先见到的记录胜出，与 mergeFrom 同规则）。
//
// keyringRaw 必须是已经通过 sign.ParseKeyring 的那一段（即 Document.KeyringRaw）。
// 这是调用方的义务，签名收的是裸 json.RawMessage，类型上约束不了；本包尚无调用方，
// 第一个接它的调用方必须遵守。理由是上面那次并入顺带做了去重（同一个 key_id 只留
// 一条），而 sign.ParseKeyring 对「同一个 key 在 revoked 里出现两次」是硬拒的——
// 传进来一段未经校验的 keyring，这里的去重会把它那条规则悄悄消解掉。这里的去重只
// 为「累积集优先」服务，不承担校验职责。
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
