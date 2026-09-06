package trustlist

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
	_, err := ParseDocument(data)
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
			_, err := ParseDocument(testDoc(t, tc.mutate))
			if err == nil {
				t.Fatalf("%s 被接受了", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误里没提到 %q：%v", tc.want, err)
			}
		})
	}
}

// TestParseDocument_AcceptsAPublisherWhoseKeyIsRevoked：一个 key 即使在
// keyring.revoked 里，只要它还在 keyring.keys 里，它的 publisher 条目就不算
// 悬空——调用方要能说出「张三的这把钥匙已被撤销」，而不是退化成「未知钥匙」。
func TestParseDocument_AcceptsAPublisherWhoseKeyIsRevoked(t *testing.T) {
	t.Parallel()

	// 需要一把不受影响的第二把钥匙：如果 keyring 里唯一的 key（dev-abc）被
	// 撤销，sign.ParseKeyring 会把"每把钥匙都被撤销"当成空信任集直接拒绝，
	// 那样就测不出"撤销了还能保留 publisher 条目"这条行为本身。
	otherPub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	otherEntry, err := sign.MarshalKeyEntry("dev-other", otherPub)
	if err != nil {
		t.Fatalf("MarshalKeyEntry: %v", err)
	}
	var otherEntryMap map[string]any
	if err := json.Unmarshal(otherEntry, &otherEntryMap); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}

	data := testDoc(t, func(m map[string]any) {
		keyring := m["keyring"].(map[string]any)
		keyring["keys"] = append(keyring["keys"].([]any), otherEntryMap)
		keyring["revoked"] = []any{
			map[string]any{
				"key_id":     "dev-abc",
				"revoked_at": "2026-09-05T02:00:00Z",
				"reason":     "laptop stolen",
			},
		}
	})
	doc, err := ParseDocument(data)
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	pub, ok := doc.Publishers["dev-abc"]
	if !ok {
		t.Fatal("已撤销 key 的 publisher 条目被当成悬空丢掉了")
	}
	if pub.DisplayName != "张三" {
		t.Errorf("Publishers[dev-abc].DisplayName = %q, want 张三", pub.DisplayName)
	}
}

func TestParseDocument_RefusesTrailingContent(t *testing.T) {
	t.Parallel()

	data := append(testDoc(t, nil), []byte(`{"serial":8}`)...)
	if _, err := ParseDocument(data); err == nil {
		t.Fatal("第二个文档被静默忽略了")
	}
}

// TestVerifyDocument_AcceptsOnlyTheRootKey 是这一层存在的全部理由。
func TestVerifyDocument_AcceptsOnlyTheRootKey(t *testing.T) {
	// 先换上一把测试 root（signer 会做这件事），再用另一把完全不相干的钥匙、
	// 冒用 root 的 key_id 去签——冒充。
	signer := newSigner(t)
	list, _ := signer(7, nil)
	_, impostor, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sig, err := sign.Sign(impostor, "test-root", list)
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
	signer := newSigner(t)
	list, sigData := signer(7, nil)
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
// 生产的 root 私钥不在仓库里（按设计），所以需要签一份能验过的清单时，测试
// 自己换掉内嵌的 root keyring。swapRootKeyring 在用例结束时还原。
//
// newSigner 是后面几个任务（cache、store）也要用的那一个，定义在这里。

// newSigner 把内嵌的 root 换成一把测试用的（用例结束自动还原），返回一个能用
// 它签任意份清单的函数。
//
// **必须是「换一次 root，签多份清单」而不是「每签一份换一次 root」**：后者会让
// 前一份已经落盘的清单在 root 被换掉之后再也验不过，于是任何「先缓存一份、
// 再取回另一份」的用例都会在读缓存那一步就失败——而它要考的 serial 比较那段
// 根本走不到，测试却是绿的。这是一条会静默失效的测试夹具，不是风格问题。
//
// 用了它的用例**不能** t.Parallel()：rootOverride 是包级变量。
func newSigner(t *testing.T) func(serial int64, mutate func(m map[string]any)) (list, sigData []byte) {
	t.Helper()
	pub, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	krData, err := sign.MarshalKeyring("test-root", pub)
	if err != nil {
		t.Fatalf("MarshalKeyring: %v", err)
	}
	kr, err := sign.ParseKeyring(krData)
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	swapRootKeyring(t, kr)

	return func(serial int64, mutate func(m map[string]any)) ([]byte, []byte) {
		list := testDoc(t, func(m map[string]any) {
			m["serial"] = float64(serial)
			if mutate != nil {
				mutate(m)
			}
		})
		sig, err := sign.Sign(priv, "test-root", list)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		sigData, err := sign.MarshalSignature(sig)
		if err != nil {
			t.Fatalf("MarshalSignature: %v", err)
		}
		return list, sigData
	}
}
