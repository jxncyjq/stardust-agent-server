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
//
// 断言的是**身份与内容**而不只是条数：一个把 a 换成别的 id、或者把 a 的理由和
// 时间抹掉、但计数不变的实现，同样是「一条已经见过的撤销消失了」。
func TestRevokedSetOnlyGrows(t *testing.T) {
	t.Parallel()

	set := newRevokedSet()
	if err := set.mergeFrom(keyringWith(t, []sign.KeyID{"a"}, []sign.KeyID{"a"})); err != nil {
		t.Fatalf("mergeFrom: %v", err)
	}
	beforeLen := set.len()
	beforeEntry, ok := set.entries["a"]
	if !ok {
		t.Fatal("并入一份撤销了 a 的清单之后，a 不在累积集里")
	}
	if err := set.mergeFrom(keyringWith(t, []sign.KeyID{"b"}, nil)); err != nil {
		t.Fatalf("mergeFrom: %v", err)
	}
	if set.len() < beforeLen {
		t.Errorf("累积集从 %d 缩到了 %d", beforeLen, set.len())
	}
	afterEntry, ok := set.entries["a"]
	if !ok {
		t.Error("并入一份没有 revoked 的清单之后，a 从累积集里消失了")
	} else if afterEntry != beforeEntry {
		t.Errorf("a 的记录被改写了：%+v -> %+v", beforeEntry, afterEntry)
	}
}

// TestRevokedSetRoundTrips：编码再读回来，内容不变——这是它落盘的形式。
//
// 「内容不变」要逐条按 key 比，不能只比条数：一个落盘时丢掉 revoked_at/reason
// 的 marshal 条数完全对得上，而丢掉的正是拒绝理由赖以成句的两个字段。
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
	for id, want := range set.entries {
		got, ok := back.entries[id]
		if !ok {
			t.Errorf("往返后 %q 不在累积集里", id)
			continue
		}
		if got != want {
			t.Errorf("往返后 %q = %+v, want %+v", id, got, want)
		}
	}
}

// TestRevokedSetSurvivesARestart 走完整条重启链：marshal 落盘 → parseRevokedSet
// 读回 → assembleKeyring 装配，断言重启之后撤销的理由与时间还在。
//
// 累积集存在的全部意义就是跨重启活下来，而「重启后读回来的撤销还带不带理由」
// 只有走完这条链才验得到：TestRevocationIsNeverForgotten 走的是纯内存的
// mergeFrom → assembleKeyring，绕过了落盘这一段。
func TestRevokedSetSurvivesARestart(t *testing.T) {
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

	// 重启后拿到的是一份全新清单（它自己的 revoked 段是空的），撤销只能来自
	// 从磁盘读回来的累积集。
	kr, err := assembleKeyring(keyringWith(t, []sign.KeyID{"a", "b"}, nil), back)
	if err != nil {
		t.Fatalf("assembleKeyring: %v", err)
	}
	rev, ok := kr.Revoked("a")
	if !ok {
		t.Fatal("重启后 a 不再是撤销状态")
	}
	if rev.Reason != "私钥泄漏" {
		t.Errorf("重启后理由丢了：reason = %q, want 私钥泄漏", rev.Reason)
	}
	if rev.At.IsZero() {
		t.Errorf("重启后撤销时间丢了：at = %v", rev.At)
	}
}

// TestParseRevokedSetRefusesGarbage：损坏的累积集不能被静默当成空集——
// 空集意味着「什么都没撤销过」，那是这个文件能造成的最坏的谎。
func TestParseRevokedSetRefusesGarbage(t *testing.T) {
	t.Parallel()

	// contains 非空时还要断言错误文本：坏掉的是 revoked-ever.json，错误就必须
	// 指向它自己（parse revoked-ever: 前缀）并点名出问题的 key_id，否则操作者会
	// 被引去查清单。
	for _, tc := range []struct{ name, data, contains string }{
		{"截断", `{"revoked":[{"key_id":`, ""},
		{"未知字段", `{"revoked":[],"extra":1}`, ""},
		{"key_id 为空", `{"revoked":[{"key_id":""}]}`, ""},
		{"尾随内容", `{"revoked":[]} {"revoked":[]}`, ""},
		{
			"重复 key_id",
			`{"revoked":[{"key_id":"x","revoked_at":"2026-08-29T10:00:00Z","reason":"私钥泄漏"},{"key_id":"x"}]}`,
			`parse revoked-ever: key id "x" is revoked twice`,
		},
		{
			"revoked_at 不是 RFC 3339",
			`{"revoked":[{"key_id":"x","revoked_at":"yesterday"}]}`,
			`parse revoked-ever: key "x" revoked_at "yesterday" is not RFC 3339`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseRevokedSet([]byte(tc.data))
			if err == nil {
				t.Fatalf("%s 的累积集被接受了", tc.name)
			}
			if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("错误没指向 revoked-ever.json 里出问题的那条：%v", err)
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
