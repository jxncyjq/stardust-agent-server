package trustlist

import (
	"encoding/json"
	"testing"

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

		t.Run("只有 Keyring", func(t *testing.T) {
			t.Parallel()

			if _, _, err := Merge(nil, Trust{Keyring: full.Keyring, Status: StatusFresh}); err == nil {
				t.Fatal("缺了 KeyringRaw 的 Trust 被接受了；清单那一侧的公钥会整个消失")
			}
		})

		t.Run("只有 KeyringRaw", func(t *testing.T) {
			t.Parallel()

			if _, _, err := Merge(nil, Trust{KeyringRaw: full.KeyringRaw, Status: StatusFresh}); err == nil {
				t.Fatal("缺了 Keyring 的 Trust 被接受了；清单那一侧的撤销会整个消失")
			}
		})
	})
}
