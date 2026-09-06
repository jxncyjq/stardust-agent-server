package trustlist

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// revocation 是造清单用的一条撤销：key_id 之外还带上 revoked_at 与 reason，
// 这样同一个 key 才能在两份清单里配不同的时间与理由——「先见到的记录胜出」
// 这条规则只有在两条记录**内容不同**时才看得出来，keyringWith 里那份硬编码的
// 时间与理由构造不出这种冲突。
type revocation struct {
	id     sign.KeyID
	at     string
	reason string
}

// keyringWith 造一份 keyring 文档：keys 里有 ids 列出的每一把，revoked 里有
// revoked 列出的每一个，撤销时间与理由都用同一份缺省值。
func keyringWith(t *testing.T, ids []sign.KeyID, revoked []sign.KeyID) json.RawMessage {
	t.Helper()
	rs := make([]revocation, 0, len(revoked))
	for _, id := range revoked {
		rs = append(rs, revocation{id: id, at: "2026-08-29T10:00:00Z", reason: "私钥泄漏"})
	}
	return keyringRevoking(t, ids, rs)
}

// keyringRevoking 与 keyringWith 相同，但每条撤销的时间与理由由调用方指定。
func keyringRevoking(t *testing.T, ids []sign.KeyID, revoked []revocation) json.RawMessage {
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
		for _, r := range revoked {
			rs = append(rs, map[string]any{
				"key_id":     string(r.id),
				"revoked_at": r.at,
				"reason":     r.reason,
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

// laterAndEmpty 是一份「后来的清单」对同一个 key 写下的撤销：时间更晚、理由为空。
// 它是合法的（sign.ParseKeyring 收得下：revoked_at 是 RFC 3339，reason 可省），
// 正因为合法才危险——一旦被当成更新采纳，当初那句「私钥泄漏 / 2026-08-29」就被
// 换成了一句什么都没说的拒绝理由。
var laterAndEmpty = revocation{id: "k", at: "2030-01-01T00:00:00Z", reason: ""}

// wantFirstRecord 断言 sign.Keyring 里 k 的撤销仍然是先见到的那条。
func wantFirstRecord(t *testing.T, kr *sign.Keyring) {
	t.Helper()
	rev, ok := kr.Revoked("k")
	if !ok {
		t.Fatal("k 不再是撤销状态")
	}
	if rev.Reason != "私钥泄漏" {
		t.Errorf("拒绝理由被后来的清单改写了：reason = %q, want 私钥泄漏", rev.Reason)
	}
	if got := rev.At.UTC().Format(time.RFC3339); got != "2026-08-29T10:00:00Z" {
		t.Errorf("撤销时间被后来的清单改写了：at = %s, want 2026-08-29T10:00:00Z", got)
	}
}

// TestMergeFromKeepsTheFirstRecord：先并入带理由的 A，再并入把同一个 key 的理由
// 改空、时间改晚的 B，累积集里留的必须还是 A 那条。
//
// 这条守的是 mergeFrom 的「先见到的记录胜出」。改成 last-wins 不会让任何条目消失、
// 条数也分毫不差，坏掉的只是拒绝理由的内容——sign.Keyring 正是用 reason 与
// revoked_at 生成操作者能读的那句话，丢掉它们，撤销就退化成「未知钥匙」。
func TestMergeFromKeepsTheFirstRecord(t *testing.T) {
	t.Parallel()

	set := newRevokedSet()
	listA := keyringWith(t, []sign.KeyID{"k", "other"}, []sign.KeyID{"k"})
	if err := set.mergeFrom(listA); err != nil {
		t.Fatalf("mergeFrom(A): %v", err)
	}
	listB := keyringRevoking(t, []sign.KeyID{"k", "other"}, []revocation{laterAndEmpty})
	if err := set.mergeFrom(listB); err != nil {
		t.Fatalf("mergeFrom(B): %v", err)
	}

	got := set.entries["k"]
	want := rawRevocationEntry{KeyID: "k", RevokedAt: "2026-08-29T10:00:00Z", Reason: "私钥泄漏"}
	if got != want {
		t.Errorf("两次 mergeFrom 之后 entries[k] = %+v, want %+v", got, want)
	}

	kr, err := assembleKeyring(listB, set)
	if err != nil {
		t.Fatalf("assembleKeyring: %v", err)
	}
	wantFirstRecord(t, kr)
}

// TestAssembleKeepsTheAccumulatedRecord：装配时清单自己的 revoked 段与累积集
// 对同一个 key 说法不同，留的必须是累积集那条。
//
// 与上一条守的是同一条规则的另一个入口：这里**没有**先 mergeFrom(B)，冲突是在
// assembleKeyring 内部的「累积集 vs 本清单 revoked 段」那次并入里发生的。两个入口
// 各自都能把先见到的记录换掉，所以两个入口都得有用例。
func TestAssembleKeepsTheAccumulatedRecord(t *testing.T) {
	t.Parallel()

	set := newRevokedSet()
	if err := set.mergeFrom(keyringWith(t, []sign.KeyID{"k", "other"}, []sign.KeyID{"k"})); err != nil {
		t.Fatalf("mergeFrom(A): %v", err)
	}
	listB := keyringRevoking(t, []sign.KeyID{"k", "other"}, []revocation{laterAndEmpty})

	kr, err := assembleKeyring(listB, set)
	if err != nil {
		t.Fatalf("assembleKeyring: %v", err)
	}
	wantFirstRecord(t, kr)
}

// TestMergeFromRefusesWhatItCouldNotReadBack：写入侧与读取侧同严。
//
// mergeFrom 收下一个非 RFC 3339 的 revoked_at，marshal 就会把它写进
// revoked-ever.json，而 parseRevokedSet 下次读不回来——这个类型自己写出的文件
// 通不过自己的读入。错误还必须说清坏的是清单那一段，不是磁盘上的累积集文件。
func TestMergeFromRefusesWhatItCouldNotReadBack(t *testing.T) {
	t.Parallel()

	set := newRevokedSet()
	bad := keyringRevoking(t, []sign.KeyID{"k", "other"}, []revocation{{id: "k", at: "yesterday"}})
	err := set.mergeFrom(bad)
	if err == nil {
		t.Fatal("mergeFrom 收下了一个 parseRevokedSet 读不回来的 revoked_at")
	}
	if !strings.Contains(err.Error(), `the manifest keyring's revoked[0] key "k" revoked_at "yesterday" is not RFC 3339`) {
		t.Errorf("错误没指向清单 keyring 段里出问题的那条：%v", err)
	}
	if strings.Contains(err.Error(), "revoked-ever") {
		t.Errorf("错误把操作者引去查 revoked-ever.json，坏的却是清单：%v", err)
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
		// 缺了 revoked 键与 revoked 为 null：marshal 永远写出这个键（空数组也写），
		// 所以这两种文件都不是本包写出来的，只可能来自外部改写或截断。读成空集
		// 就是「这台机器从没见过任何撤销」——最坏的那句谎。
		{"缺 revoked 键", `{}`, `parse revoked-ever: revoked-ever.json has no revoked key`},
		{"revoked 是 null", `{"revoked":null}`, `parse revoked-ever: revoked-ever.json has no revoked key`},
		{
			"重复 key_id",
			`{"revoked":[{"key_id":"x","revoked_at":"2026-08-29T10:00:00Z","reason":"私钥泄漏"},{"key_id":"x"}]}`,
			`parse revoked-ever: key id "x" is revoked twice`,
		},
		{
			"revoked_at 不是 RFC 3339",
			`{"revoked":[{"key_id":"x","revoked_at":"yesterday"}]}`,
			`parse revoked-ever: revoked-ever.json revoked[0] key "x" revoked_at "yesterday" is not RFC 3339`,
		},
		// 重复的键走的是 encoding/json 的 last-wins：第二个 revoked 键把第一个
		// 整段顶掉，解码器不报错，DisallowUnknownFields 与 dec.More() 也都不管。
		// 读成空集就是「这台机器从没见过任何撤销」——最坏的那句谎，而且是唯一一种
		// 不响亮的坏法。
		{
			"revoked 键出现两次",
			`{"revoked":[{"key_id":"x","revoked_at":"2026-08-29T10:00:00Z","reason":"私钥泄漏"}],"revoked":[]}`,
			`parse revoked-ever: revoked-ever.json names "revoked" twice`,
		},
		// 同一条 last-wins 规则在条目里同样成立：重复的 key_id 让一条撤销无声
		// 换了主人，被撤销的那把钥匙就此不再被拒。
		{
			"条目里的 key_id 出现两次",
			`{"revoked":[{"key_id":"被撤销的","key_id":"无关的"}]}`,
			`parse revoked-ever: revoked-ever.json.revoked[0] names "key_id" twice`,
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

// TestParseRevokedSetAcceptsAnEmptyList：`{"revoked":[]}` 是合法的正常状态——
// 见过清单，但还没有任何撤销。它必须和「缺了 revoked 键」区分开，否则上一条
// 用例只要把「拒绝缺键」写成「拒绝一切空集」就能作弊通过，而一台还没见过任何
// 撤销的新机器从此再也起不来。
//
// 顺带守住 marshal 那一头：空累积集写出来的必须是这份能被自己读回来的字节。
func TestParseRevokedSetAcceptsAnEmptyList(t *testing.T) {
	t.Parallel()

	set, err := parseRevokedSet([]byte(`{"revoked":[]}`))
	if err != nil {
		t.Fatalf("空撤销列表被拒了：%v", err)
	}
	if set.len() != 0 {
		t.Errorf("空撤销列表读出了 %d 条", set.len())
	}

	data, err := newRevokedSet().marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"revoked": []`) {
		t.Errorf("空累积集没写出 revoked 键，下次读回来会被当成损坏文件：%s", data)
	}
	if _, err := parseRevokedSet(data); err != nil {
		t.Errorf("空累积集写出来的文件自己读不回来：%v", err)
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

// withADuplicateRevokedKey 在一份真实的 revoked-ever.json 后面再补一个空的
// revoked 键，其余字节一个不动。
//
// 它刻意不重新构造整份文件：这条用例要证的是「一份**已经记着真实撤销**的文件被
// 补上一个重复键之后会怎样」，重新构造会把那条记录换成夹具自己造的东西，也就不
// 再是同一件事了。
func withADuplicateRevokedKey(t *testing.T, record []byte) []byte {
	t.Helper()
	trimmed := strings.TrimRight(string(record), " \t\r\n")
	if !strings.HasSuffix(trimmed, "}") {
		t.Fatalf("revoked-ever.json 不是以 } 收尾，夹具改不动它：%s", record)
	}
	return []byte(trimmed[:len(trimmed)-1] + `,"revoked":[]}` + "\n")
}

// TestADamagedRevocationRecordIsNeverReadAsAnEmptySet 把三组对照放在同一条生产
// 链路（Store.Current / Store.Refresh / 磁盘上的字节）上走一遍：
//
//	A 删掉 revoked-ever.json —— Current 必须响亮
//	B revoked 键写两次       —— Current 同样必须响亮
//	C B 之后再来一份 revoked 段为空的新清单 —— 必须被拒，且磁盘上那份记录一个字节都不许变
//
// C 才是这三组的要害。encoding/json 对同一层重复出现的键取最后一个，于是
// `{"revoked":[…真实记录…],"revoked":[]}` 曾经被读成一个 err == nil 的空集；
// 空集顺着 Refresh 走下去会被当成「这台机器从没见过任何撤销」写回磁盘，
// 累积集就此永久消失，事后连取证都做不了——本期第一不变量上唯一一条安静的破口。
//
// A 是对照组：它证明这个文件的其余坏法本来就是响亮的，B 的静默不是「这类损坏
// 一律如此」，而是单独漏掉的一条。
func TestADamagedRevocationRecordIsNeverReadAsAnEmptySet(t *testing.T) {
	signer := newSigner(t)
	list, sig := signer(7, withSecondKeyRevoking(t, "dev-abc"))
	cur := newServeList(list, sig)
	srv := newListServer(t, cur)
	cacheDir := t.TempDir()
	store := newTestStore(t, srv, cacheDir, fixedNow)
	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	recordPath := filepath.Join(cacheDir, revokedFileName)
	recorded, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read %s: %v", revokedFileName, err)
	}
	if !strings.Contains(string(recorded), "dev-abc") {
		t.Fatalf("夹具没造出要保护的那条记录：%s", recorded)
	}

	// A：文件没了。
	if err := os.Remove(recordPath); err != nil {
		t.Fatalf("remove %s: %v", revokedFileName, err)
	}
	if _, err := store.Current(); err == nil {
		t.Error("A：revoked-ever.json 被删掉，Current 却报告一切正常")
	}

	// B：文件在，但 revoked 键写了两次，真实那条记录还原样躺在里面。
	duplicated := withADuplicateRevokedKey(t, recorded)
	if !strings.Contains(string(duplicated), "dev-abc") {
		t.Fatalf("夹具把要保护的那条记录弄丢了：%s", duplicated)
	}
	if err := os.WriteFile(recordPath, duplicated, 0o600); err != nil {
		t.Fatalf("write %s: %v", revokedFileName, err)
	}
	// B 与 C 之间刻意不用 t.Fatal 断开：C 才是这条破口真正的后果，B 一失守就停下
	// 会让 C 永远没机会说话，而两者是各自独立的性质（读的时候响不响亮 / 磁盘上那
	// 份记录还在不在）。
	switch _, currentErr := store.Current(); {
	case currentErr == nil:
		t.Error("B：revoked 键写了两次，Current 却把它读成了「这台机器从没见过任何撤销」")
	case !strings.Contains(currentErr.Error(), `names "revoked" twice`):
		t.Errorf("B：错误没点明坏在重复的 revoked 键上：%v", currentErr)
	}

	// C：一份 serial 更大、签名完全合法、revoked 段为空的清单。正常路径下它会被
	// 接受并把并集写回磁盘——那一写就是撤销永久消失的那一刻。
	list9, sig9 := signer(9, withSecondKeyRevoking(t))
	cur.set(list9, sig9)
	if _, err := store.Refresh(context.Background()); err == nil {
		t.Error("C：累积集读不回来，Refresh 却成功了——它写回去的是一个空集")
	}
	after, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read %s: %v", revokedFileName, err)
	}
	if !bytes.Equal(after, duplicated) {
		t.Errorf("C：那次被拒的刷新改写了累积集：\nbefore=%s\nafter=%s", duplicated, after)
	}
	if !strings.Contains(string(after), "dev-abc") {
		t.Errorf("C：磁盘上那条撤销记录没了，此后连取证都做不了：%s", after)
	}
}
