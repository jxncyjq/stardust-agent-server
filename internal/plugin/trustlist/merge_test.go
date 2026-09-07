package trustlist

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// trustFrom 造一个「清单那一侧」的 Trust：keyringRaw 是清单信封里那段 keyring
// 文档，names 是发布者展示名。
//
// 它把同一段字节既交给 KeyringRaw 也交给 sign.ParseKeyring 造出 Keyring，因为
// Store.assemble 也是这么填这两个字段的（keys 原样搬运，Keyring 由同一段字节装配
// 而来）。两个字段各带一半：公钥只在字节里，撤销只在 Keyring 里。
//
// 它刻意不使用 document_test.go 里的 newSigner：那个辅助会换掉内嵌的 root 信任集，
// 而这里根本不需要一份签过名的清单——只需要一段 keyring 文档。引进来只会让用例之间
// 产生看不见的耦合。
func trustFrom(t *testing.T, keyringRaw json.RawMessage, names map[sign.KeyID]string) Trust {
	t.Helper()
	keyring, err := sign.ParseKeyring(keyringRaw)
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	publishers := make(map[sign.KeyID]Publisher, len(names))
	for id, name := range names {
		publishers[id] = Publisher{KeyID: id, DisplayName: name}
	}
	return Trust{
		Keyring:    keyring,
		KeyringRaw: keyringRaw,
		Publishers: publishers,
		Status:     StatusFresh,
		Serial:     7,
	}
}

// TestMergeTakesTheUnionOfBothSides：两侧登记的钥匙都要能用。
// 内网自己签的插件与公开生态的插件，是同一台机器上并存的两类东西。
func TestMergeTakesTheUnionOfBothSides(t *testing.T) {
	t.Parallel()

	local := keyringWith(t, []sign.KeyID{"ops-local"}, nil)
	listTrust := trustFrom(t, keyringWith(t, []sign.KeyID{"dev-abc"}, nil),
		map[sign.KeyID]string{"dev-abc": "张三"})

	merged, publishers, err := Merge(local, listTrust)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	ids := merged.IDs()
	if len(ids) != 2 {
		t.Fatalf("合并后有 %d 把钥匙，want 2：%v", len(ids), ids)
	}
	if ids[0] != "dev-abc" || ids[1] != "ops-local" {
		t.Errorf("IDs = %v, want [dev-abc ops-local]", ids)
	}
	if publishers["dev-abc"] != "张三" {
		t.Errorf("publishers[dev-abc] = %q, want 张三", publishers["dev-abc"])
	}
}

// TestMergeTakesTheUnionOfRevocations 是这个函数最重要的性质，两个方向都要守：
// 任何一边说撤销了就是撤销了。撤销单调这条不能因为来源多了就打折。
//
// 两个子用例里清单那一侧都留了一把没被撤销的钥匙，因为 sign.ParseKeyring 拒绝
// 「每把钥匙都被撤销」的信任集——一个所有钥匙都撤销了的 Trust 在真实系统里根本
// 装配不出来（见 Store.assemble），用它当夹具测的就不是这条性质了。
func TestMergeTakesTheUnionOfRevocations(t *testing.T) {
	t.Parallel()

	t.Run("只有本地撤销", func(t *testing.T) {
		t.Parallel()

		local := keyringWith(t, []sign.KeyID{"k", "live"}, []sign.KeyID{"k"})
		listTrust := trustFrom(t, keyringWith(t, []sign.KeyID{"k"}, nil), nil)
		merged, _, err := Merge(local, listTrust)
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if _, gone := merged.Revoked("k"); !gone {
			t.Error("本地撤销的 k 在合并后不再是撤销状态——被清单那一半覆盖掉了")
		}
	})

	t.Run("只有清单撤销", func(t *testing.T) {
		t.Parallel()

		local := keyringWith(t, []sign.KeyID{"k", "live"}, nil)
		listTrust := trustFrom(t, keyringWith(t, []sign.KeyID{"k", "dev-live"}, []sign.KeyID{"k"}), nil)
		merged, _, err := Merge(local, listTrust)
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if _, gone := merged.Revoked("k"); !gone {
			t.Error("清单撤销的 k 在合并后不再是撤销状态——本地那一半盖过了撤销")
		}
	})
}

// TestMergeKeepsARevocationWhoseKeyIsNoLongerListed：一条指向「合并后 keys 里
// 根本没有那把钥匙」的撤销，必须原样保留。
//
// 丢掉它就等于「把一个公钥从登记里删掉」成了撤销的解药：撤销记录消失，那把钥匙
// 只要在下一份文档里重新登记就又可信了。sign.ParseKeyring 接受这种悬空的撤销
// （它只校验 key_id 非空、不重复、revoked_at 是 RFC 3339），所以保留它是做得到的。
func TestMergeKeepsARevocationWhoseKeyIsNoLongerListed(t *testing.T) {
	t.Parallel()

	local := keyringWith(t, []sign.KeyID{"ops-local"}, nil)
	listTrust := trustFrom(t, keyringWith(t, []sign.KeyID{"dev-abc"}, []sign.KeyID{"dev-gone"}), nil)

	merged, _, err := Merge(local, listTrust)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if _, gone := merged.Revoked("dev-gone"); !gone {
		t.Errorf("dev-gone 的撤销在合并后消失了；merged 的撤销集是 %v", merged.RevokedIDs())
	}
}

// TestMergeSurvivesEitherSideBeingAbsent：断网（清单不可用）与没配本地 keyring
// 都是正常状态，各自都要能单独工作。
func TestMergeSurvivesEitherSideBeingAbsent(t *testing.T) {
	t.Parallel()

	t.Run("只有本地", func(t *testing.T) {
		t.Parallel()

		local := keyringWith(t, []sign.KeyID{"ops"}, nil)
		merged, _, err := Merge(local, Trust{Status: StatusUnavailable})
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if len(merged.IDs()) != 1 {
			t.Errorf("IDs = %v, want 只有 ops", merged.IDs())
		}
	})

	t.Run("只有清单", func(t *testing.T) {
		t.Parallel()

		listTrust := trustFrom(t, keyringWith(t, []sign.KeyID{"dev"}, nil), nil)
		merged, _, err := Merge(nil, listTrust)
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if len(merged.IDs()) != 1 {
			t.Errorf("IDs = %v, want 只有 dev", merged.IDs())
		}
	})

	t.Run("两边都没有", func(t *testing.T) {
		t.Parallel()

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

// TestMergeRefusesInputItCannotAccountFor：坏输入必须响亮地失败，不能悄悄合成
// 一个少了一半的信任集——少掉的那一半可能正是撤销。
func TestMergeRefusesInputItCannotAccountFor(t *testing.T) {
	t.Parallel()

	t.Run("本地文档不是合法 JSON", func(t *testing.T) {
		t.Parallel()

		if _, _, err := Merge(json.RawMessage("{"), Trust{Status: StatusUnavailable}); err == nil {
			t.Fatal("坏掉的本地 keyring 文档被接受了")
		}
	})

	t.Run("本地文档一把钥匙都没登记", func(t *testing.T) {
		t.Parallel()

		if _, _, err := Merge(json.RawMessage(`{"keys":[]}`), Trust{Status: StatusUnavailable}); err == nil {
			t.Fatal("空的 keys 被接受了")
		}
	})

	t.Run("Trust 的两个 keyring 字段对不上", func(t *testing.T) {
		t.Parallel()

		full := trustFrom(t, keyringWith(t, []sign.KeyID{"dev"}, nil), nil)

		// 两个子用例都断言错误文本，不只断言「有错」。缺了这一条时，两支硬拒
		// 里「只有 Keyring」那一支其实是被偶然守住的：把它短路掉，
		// keyEntriesOf(nil, "trust list") 里的 json.Unmarshal(nil) 照样会报错，
		// 用例照样绿——分不出「函数显式拒绝了这个形状」和「撞上了一个 JSON
		// 解码错误」。而这两者对操作者是完全不同的两句话。
		t.Run("只有 Keyring", func(t *testing.T) {
			t.Parallel()

			_, _, err := Merge(nil, Trust{Keyring: full.Keyring, Status: StatusFresh})
			if err == nil {
				t.Fatal("缺了 KeyringRaw 的 Trust 被接受了；清单那一侧的公钥会整个消失")
			}
			if !strings.Contains(err.Error(), "its public keys live only in the document") {
				t.Errorf("错误文本没说清缺的是哪一半，operator 无从下手：%v", err)
			}
		})

		t.Run("只有 KeyringRaw", func(t *testing.T) {
			t.Parallel()

			_, _, err := Merge(nil, Trust{KeyringRaw: full.KeyringRaw, Status: StatusFresh})
			if err == nil {
				t.Fatal("缺了 Keyring 的 Trust 被接受了；清单那一侧的撤销会整个消失")
			}
			if !strings.Contains(err.Error(), "its revocations live only in the parsed one") {
				t.Errorf("错误文本没说清缺的是哪一半，operator 无从下手：%v", err)
			}
		})
	})
}

// keyringOf 造一份 keyring 文档，keys 段登记的公钥由调用方给出。
//
// 它与 keyringWith 的分工是：keyringWith 每把钥匙现生成一对，谁也不知道公钥是哪
// 一把；而「同一个 id 两侧登记了不同公钥、合并后留下的是哪一把」这个问题，只有
// 在调用方手里握着那两把公钥（以及其中一把的私钥）时才问得出来。
func keyringOf(t *testing.T, keys map[sign.KeyID]ed25519.PublicKey) json.RawMessage {
	t.Helper()
	entries := make([]json.RawMessage, 0, len(keys))
	for _, id := range sortedIDs(keys) {
		entry, err := sign.MarshalKeyEntry(id, keys[id])
		if err != nil {
			t.Fatalf("MarshalKeyEntry %q: %v", id, err)
		}
		entries = append(entries, entry)
	}
	data, err := json.Marshal(struct {
		Keys []json.RawMessage `json:"keys"`
	}{Keys: entries})
	if err != nil {
		t.Fatalf("marshal keyring: %v", err)
	}
	return data
}

// TestMergeKeepsARevocationThisMachineAccumulatedThatTheListNoLongerNames 守的是
// 「撤销永不遗忘」这条不变量在合并这一层的投影。
//
// 本机的撤销累积集（revoked-ever.json）只在 assembleKeyring 装配 Trust.Keyring 时
// 被并进去；Trust.KeyringRaw 的 revoked 段里只有当前这一份清单自己写下的那些。
// 于是「清单侧的撤销从哪个字段取」不是风格问题：取 KeyringRaw 就意味着每次合并
// 都把本机累积了几个月的撤销静默清空——而那些撤销正是断网/重放绕不过去的那道闸。
//
// 夹具刻意让两个字段**内容不同**（这正是生产里 Store.assemble 每次产出的形状），
// 否则用例在原理上分辨不出撤销取自哪一边；下面两条前提断言把这个「不同」钉死，
// 将来夹具要是退化成两边相等，这条用例会先响亮地失败，而不是悄悄失去分辨力。
func TestMergeKeepsARevocationThisMachineAccumulatedThatTheListNoLongerNames(t *testing.T) {
	t.Parallel()

	// 上一份清单撤销了 old；这一份清单的 revoked 段是空的（发布侧误删，或者被诱导）。
	previous := keyringWith(t, []sign.KeyID{"live", "old"}, []sign.KeyID{"old"})
	ever := newRevokedSet()
	if err := ever.mergeFrom(previous); err != nil {
		t.Fatalf("mergeFrom(previous): %v", err)
	}
	current := keyringWith(t, []sign.KeyID{"live", "old"}, nil)

	keyring, err := assembleKeyring(current, ever)
	if err != nil {
		t.Fatalf("assembleKeyring: %v", err)
	}

	// 前提一：当前清单的 revoked 段确实是空的。
	var shape keyringShape
	if err := json.Unmarshal(current, &shape); err != nil {
		t.Fatalf("unmarshal current: %v", err)
	}
	if len(shape.Revoked) != 0 {
		t.Fatalf("夹具坏了：当前清单自己写了 %d 条撤销，这条用例就分辨不出撤销取自哪个字段了",
			len(shape.Revoked))
	}
	// 前提二：装配出来的 Keyring 里确实有 old——两个字段带的东西不一样。
	if _, gone := keyring.Revoked("old"); !gone {
		t.Fatalf("夹具坏了：累积集里的 old 没有进到 Keyring 里，撤销集是 %v", keyring.RevokedIDs())
	}

	merged, _, err := Merge(nil, Trust{Keyring: keyring, KeyringRaw: current, Status: StatusFresh})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if _, gone := merged.Revoked("old"); !gone {
		t.Fatalf("本机累积的撤销 old 在合并后消失了；merged 的撤销集是 %v", merged.RevokedIDs())
	}
}

// TestMergeKeepsTheLocalPublicKeyWhenTheListAlsoRegistersThatID：两侧登记同一个
// id 时，落地的必须是本地那把公钥。
//
// 这是一条安全优先级：谁能改写 keys[id] 的公钥，谁就能让自己签的包在本机验签通过。
// 断言方式是拿本地那把私钥签一段消息、要求合并后的信任集验得过——直接问「留下的
// 是哪一把公钥」，而不是数一数 id 的个数（两侧登记同一个 id 时，覆盖与否 id 数
// 完全一样）。
func TestMergeKeepsTheLocalPublicKeyWhenTheListAlsoRegistersThatID(t *testing.T) {
	t.Parallel()

	localPub, localPriv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey(local): %v", err)
	}
	listPub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey(list): %v", err)
	}
	otherPub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey(other): %v", err)
	}

	local := keyringOf(t, map[sign.KeyID]ed25519.PublicKey{"k": localPub})
	listTrust := trustFrom(t, keyringOf(t, map[sign.KeyID]ed25519.PublicKey{
		"k":         listPub,
		"dev-other": otherPub,
	}), nil)

	merged, _, err := Merge(local, listTrust)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	message := []byte("一个由本机记下的那把私钥签出来的包")
	sig, err := sign.Sign(localPriv, "k", message)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := merged.Verify(sig, message); err != nil {
		t.Fatalf("合并后 k 验不过本机那把私钥的签名——联网清单改写了操作者写在这台机器上的公钥：%v", err)
	}
}

// TestMergeKeepsTheLocalRevocationRecordWhenBothSidesRevokeTheSameKey：两侧都撤销
// 同一个 key 时，留下的撤销时间与理由必须是本地那条。
//
// 撤销的时间与理由是操作者当时写下的事实，不是可以被后来的文档改写的意见；它们
// 会原样出现在拒绝一个包时给操作者看的那句话里（sign.Revocation.describe）。
func TestMergeKeepsTheLocalRevocationRecordWhenBothSidesRevokeTheSameKey(t *testing.T) {
	t.Parallel()

	local := keyringRevoking(t, []sign.KeyID{"k", "live"}, []revocation{
		{id: "k", at: "2026-01-02T03:04:05Z", reason: "本机操作者当时写下的理由"},
	})
	listTrust := trustFrom(t, keyringRevoking(t, []sign.KeyID{"k", "dev-live"}, []revocation{
		{id: "k", at: "2026-05-06T07:08:09Z", reason: "清单后来写下的理由"},
	}), nil)

	merged, _, err := Merge(local, listTrust)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	record, gone := merged.Revoked("k")
	if !gone {
		t.Fatalf("k 在合并后不再是撤销状态；撤销集是 %v", merged.RevokedIDs())
	}
	if record.Reason != "本机操作者当时写下的理由" {
		t.Errorf("reason = %q，本地那条被清单改写了", record.Reason)
	}
	if got := record.At.Format(time.RFC3339); got != "2026-01-02T03:04:05Z" {
		t.Errorf("revoked_at = %q，本地那条被清单改写了", got)
	}
}

// TestMergeNamesTheLocalDocumentWhenItsRevocationIsMalformed：本地文档里一条坏掉的
// revoked_at，错误必须指向**本地那份文档的第几条**。
//
// 少了入口处这次校验仍然会失败（末尾的 sign.ParseKeyring 会挡下），但错误会变成
// 一句关于「合并出来的那份文档」的话——那份文档没有任何人写过，operator 拿着它
// 无处可去。这正是 merge.go 里那条注释声明要避免的事，所以它需要一条用例。
func TestMergeNamesTheLocalDocumentWhenItsRevocationIsMalformed(t *testing.T) {
	t.Parallel()

	local := keyringRevoking(t, []sign.KeyID{"k", "live"}, []revocation{
		{id: "k", at: "上周二", reason: "私钥泄漏"},
	})

	_, _, err := Merge(local, Trust{Status: StatusUnavailable})
	if err == nil {
		t.Fatal("坏掉的 revoked_at 被接受了")
	}
	if !strings.Contains(err.Error(), "the local keyring's revoked[0]") {
		t.Errorf("错误没指向本地文档的那一条，operator 会被指到一份没人写过的文档上：%v", err)
	}
}

// recordedRevocations 造一份「这台机器已经记下的撤销」，用来填 Trust 那个不导出
// 的字段——也就是清单文档已经不可用、而撤销仍然已知的那种 Trust。
//
// 它走 parseRevokedSet 而不是直接拼一个 revokedSet，因为磁盘上那份记录进内存只有
// 这一条路；绕过它就是在测试里另造一份形状，而形状对不上正是这个类型最容易坏的
// 地方。
func recordedRevocations(t *testing.T, ids ...sign.KeyID) *revokedSet {
	t.Helper()

	entries := make([]any, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, map[string]any{
			"key_id":     string(id),
			"revoked_at": "2026-08-29T10:00:00Z",
			"reason":     "磁盘上那份累积记录",
		})
	}
	data, err := json.Marshal(map[string]any{"revoked": entries})
	if err != nil {
		t.Fatalf("marshal revoked-ever: %v", err)
	}
	set, err := parseRevokedSet(data)
	if err != nil {
		t.Fatalf("parseRevokedSet: %v", err)
	}
	return set
}

// TestMergeKeepsRecordedRevocationsWhenTheListHalfHasNoKeyring 守的是第三种合法
// 形状：清单那一侧既没有 Keyring 也没有 KeyringRaw（两条守卫都不触发），但这台
// 机器已经记下的撤销仍然必须并进来。
//
// 这是「清单拉不到时按安装期结论挂载，**但撤销仍硬拒**」这条产品拍板的后半句落在
// 合并层的样子。没有它，一个缺失的 trustlist.json 就会让并集里只剩本地那一半，
// 而少掉的正好是撤销。
func TestMergeKeepsRecordedRevocationsWhenTheListHalfHasNoKeyring(t *testing.T) {
	t.Parallel()

	// live 是那把没被撤销的钥匙：sign.ParseKeyring 拒绝「每把钥匙都被撤销」的信任
	// 集，只登记 k 的话这条用例会卡在解析那一步，考的就不是这条性质了。
	local := keyringWith(t, []sign.KeyID{"k", "live"}, nil)
	listed := Trust{Status: StatusUnavailable, revocations: recordedRevocations(t, "k")}

	merged, _, err := Merge(local, listed)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged == nil {
		t.Fatal("merged = nil，可本地 keyring 登记了两把钥匙")
	}
	rev, ok := merged.Revoked("k")
	if !ok {
		t.Fatal("清单那一侧没有 keyring 文档时，这台机器已经记下的撤销被丢掉了")
	}
	if rev.Reason != "磁盘上那份累积记录" {
		t.Errorf("撤销理由 = %q，读到的不是那份累积记录", rev.Reason)
	}
	if len(merged.IDs()) != 2 {
		t.Errorf("IDs = %v, want 本地那两把都在：撤销并进来不该顺手改动登记那一半",
			merged.IDs())
	}
}

// TestMergePrefersTheListsRevocationRecordOverTheRecordedOne 钉住这条通道的优先
// 级：清单那一侧的 Keyring 与累积集说的是同一条撤销时，保留先见到的那条。
//
// 「先见到的胜出」是本函数与 mergeFrom、assembleKeyring 共用的一条规则，两条记录
// 的差别在 revoked_at 与 reason ——也就是操作者读到的那句拒绝理由。新开的这条通道
// 如果掉过头来覆盖它，规则就分了家。
func TestMergePrefersTheListsRevocationRecordOverTheRecordedOne(t *testing.T) {
	t.Parallel()

	local := keyringWith(t, []sign.KeyID{"live"}, nil)
	listTrust := trustFrom(t, keyringWith(t, []sign.KeyID{"k", "live"}, []sign.KeyID{"k"}), nil)
	listTrust.revocations = recordedRevocations(t, "k")

	merged, _, err := Merge(local, listTrust)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	rev, ok := merged.Revoked("k")
	if !ok {
		t.Fatal("k 不再是撤销状态")
	}
	if rev.Reason != "私钥泄漏" {
		t.Errorf("撤销理由 = %q, want 清单那一侧先见到的那条（私钥泄漏）", rev.Reason)
	}
}
