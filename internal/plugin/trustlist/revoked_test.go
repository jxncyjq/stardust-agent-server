package trustlist

import (
	"encoding/json"
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
}
