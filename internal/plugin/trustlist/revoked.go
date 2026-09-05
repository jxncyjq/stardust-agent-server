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
