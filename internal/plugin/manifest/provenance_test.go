package manifest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// provenancePackageDir 在 t.TempDir() 里造一个完整的插件包，并按需签名。
//
// 返回包目录。signWith 为 nil 时不写 plugin.sig——那正是「未签名」这一态要考
// 的输入，此时 keyID 不被使用。
//
// plugin.json 的字段是 validatePlugin 要求的最小集合（abi、limits、以及每个
// 工具的 group 与 timeout_ms 都是必填），因为这些用例考的是签名判定，任何一处
// 让 ParsePlugin 先失败的写法都会把它们考成别的东西。
func provenancePackageDir(t *testing.T, signWith ed25519.PrivateKey, keyID sign.KeyID) string {
	t.Helper()
	dir := t.TempDir()
	wasm := []byte("\x00asm\x01\x00\x00\x00")
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), wasm, 0o600); err != nil {
		t.Fatalf("write plugin.wasm: %v", err)
	}
	pm := []byte(`{"name":"probe","version":"1.0.0","abi":1,"sha256":"` + sha256Hex(wasm) + `",` +
		`"capabilities":[],"limits":{"max_memory_pages":1,"max_instances":1},` +
		`"tools":[{"name":"t","description":"d","group":"g","timeout_ms":1000,` +
		`"input_schema":{"type":"object"}}]}`)
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), pm, 0o600); err != nil {
		t.Fatalf("write plugin.json: %v", err)
	}
	if signWith != nil {
		sig, err := sign.Sign(signWith, keyID, pm)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		data, err := sign.MarshalSignature(sig)
		if err != nil {
			t.Fatalf("MarshalSignature: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "plugin.sig"), data, 0o600); err != nil {
			t.Fatalf("write plugin.sig: %v", err)
		}
	}
	return dir
}

// provenanceKeyring 造一个信任集：keys 里的每一把都进 "keys"，revoke 里的每一把
// **同时**留在 keys 里并进 revoked——sign 包刻意保留被撤销的公钥（见
// sign.Keyring.revoked 的注释），正是为了让拒绝理由能说出「这把钥匙曾被信任」，
// 测试不能把这条前提改掉。
func provenanceKeyring(t *testing.T, keys map[sign.KeyID]ed25519.PublicKey, revoke []sign.KeyID) *sign.Keyring {
	t.Helper()
	entries := make([]string, 0, len(keys))
	for id, pub := range keys {
		b, err := sign.MarshalKeyEntry(id, pub)
		if err != nil {
			t.Fatalf("MarshalKeyEntry: %v", err)
		}
		entries = append(entries, string(b))
	}
	doc := `{"keys":[` + strings.Join(entries, ",") + `]`
	if len(revoke) > 0 {
		rs := make([]string, 0, len(revoke))
		for _, id := range revoke {
			rs = append(rs, `{"key_id":"`+string(id)+`","revoked_at":"2026-08-29T10:00:00Z","reason":"私钥泄漏"}`)
		}
		doc += `,"revoked":[` + strings.Join(rs, ",") + `]`
	}
	doc += `}`
	kr, err := sign.ParseKeyring([]byte(doc))
	if err != nil {
		t.Fatalf("ParseKeyring(%s): %v", doc, err)
	}
	return kr
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestProvenanceRegistered(t *testing.T) {
	pub, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := provenancePackageDir(t, priv, "dev-abc")
	kr := provenanceKeyring(t, map[sign.KeyID]ed25519.PublicKey{"dev-abc": pub}, nil)

	_, _, prov, err := LoadPackage(dir, TrustInput{
		Keyring:    kr,
		Publishers: map[sign.KeyID]string{"dev-abc": "张三"},
	})
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if prov.State != ProvenanceRegistered {
		t.Errorf("State = %v, want ProvenanceRegistered", prov.State)
	}
	if prov.KeyID != "dev-abc" {
		t.Errorf("KeyID = %q, want dev-abc", prov.KeyID)
	}
	if prov.Publisher != "张三" {
		t.Errorf("Publisher = %q, want 张三", prov.Publisher)
	}
}

// TestProvenanceUnsignedWhenThereIsNoSignature：没有 plugin.sig 不是错误，
// 是一种**判定**。这与改动前的行为相反（那时非 nil keyring + 缺签名 = error），
// 而这正是三态存在的理由：放不放行是调用方的策略，不是这里的。
func TestProvenanceUnsignedWhenThereIsNoSignature(t *testing.T) {
	pub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := provenancePackageDir(t, nil, "")
	kr := provenanceKeyring(t, map[sign.KeyID]ed25519.PublicKey{"dev-abc": pub}, nil)

	_, _, prov, err := LoadPackage(dir, TrustInput{Keyring: kr})
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if prov.State != ProvenanceUnsigned {
		t.Errorf("State = %v, want ProvenanceUnsigned", prov.State)
	}
	if prov.KeyID != "" {
		t.Errorf("KeyID = %q, want 空——没有签名就没有 key 可报", prov.KeyID)
	}
}

// TestProvenanceUnsignedWhenTheKeyIsUnknown：签了，但签它的 key 这台机器不认识。
// 对用户的意义与「压根没签」完全相同：没有任何已登记的开发者为这份字节背书。
//
// KeyID 必须留空：把一把这台机器不认识的 key id 显示出来，只会让操作者以为它有意义。
func TestProvenanceUnsignedWhenTheKeyIsUnknown(t *testing.T) {
	known, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	_, stranger, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := provenancePackageDir(t, stranger, "dev-stranger")
	kr := provenanceKeyring(t, map[sign.KeyID]ed25519.PublicKey{"dev-abc": known}, nil)

	_, _, prov, err := LoadPackage(dir, TrustInput{Keyring: kr})
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if prov.State != ProvenanceUnsigned {
		t.Errorf("State = %v, want ProvenanceUnsigned", prov.State)
	}
	if prov.KeyID != "" {
		t.Errorf("KeyID = %q, want 空", prov.KeyID)
	}
}

// TestProvenanceRevoked：撤销的判定必须带上当初写下的时间与理由——
// sign.Keyring 保留它们正是为了让拒绝能说明自己，丢掉就退化成「未知钥匙」。
func TestProvenanceRevoked(t *testing.T) {
	pub, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := provenancePackageDir(t, priv, "dev-gone")
	kr := provenanceKeyring(t,
		map[sign.KeyID]ed25519.PublicKey{"dev-gone": pub, "dev-live": pub},
		[]sign.KeyID{"dev-gone"})

	_, _, prov, err := LoadPackage(dir, TrustInput{Keyring: kr})
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if prov.State != ProvenanceRevoked {
		t.Fatalf("State = %v, want ProvenanceRevoked", prov.State)
	}
	if prov.KeyID != "dev-gone" {
		t.Errorf("KeyID = %q, want dev-gone", prov.KeyID)
	}
	if prov.Reason != "私钥泄漏" {
		t.Errorf("Reason = %q, want 私钥泄漏", prov.Reason)
	}
	if prov.RevokedAt.IsZero() {
		t.Error("RevokedAt 是零值——撤销时间丢了")
	}
}

// TestProvenanceRevokedWhenTheKeyIsNoLongerListed：撤销记录不要求那把钥匙还留在
// keys 里——sign.ParseKeyring 接受一条指向 keys 中不存在的 id 的撤销。这一条钉住
// 撤销检查必须排在成员检查**之前**：反过来的话，这样一把钥匙会先被判成
// ProvenanceUnsigned——而未签名恰恰是操作者有办法认下的那一态，于是「删掉一个公钥」
// 就成了撤销的解药。
func TestProvenanceRevokedWhenTheKeyIsNoLongerListed(t *testing.T) {
	_, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	livePub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := provenancePackageDir(t, priv, "dev-gone")
	// keys 里只有 dev-live；dev-gone 的公钥已被删掉，只剩一条撤销记录。
	kr, err := sign.ParseKeyring([]byte(
		`{"keys":[{"id":"dev-live","algorithm":"ed25519","public_key":"` +
			base64.StdEncoding.EncodeToString(livePub) + `"}],` +
			`"revoked":[{"key_id":"dev-gone","revoked_at":"2026-08-29T10:00:00Z","reason":"私钥泄漏"}]}`))
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}

	_, _, prov, err := LoadPackage(dir, TrustInput{Keyring: kr})
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if prov.State != ProvenanceRevoked {
		t.Fatalf("State = %v, want ProvenanceRevoked", prov.State)
	}
	if prov.KeyID != "dev-gone" {
		t.Errorf("KeyID = %q, want dev-gone", prov.KeyID)
	}
	if prov.Reason != "私钥泄漏" {
		t.Errorf("Reason = %q, want 私钥泄漏", prov.Reason)
	}
}

// TestATamperedSignatureIsAnErrorNotAState：签名对不上**不是**一种来源判定，
// 是完整性失败。三态说的是「谁为它背书」，篡改说的是「它被改过」，两件事不能混：
// 前者是策略，后者不是——把篡改说成 Unsigned，就等于让一个 --accept-unsigned
// 放行一份被改过的包。
func TestATamperedSignatureIsAnErrorNotAState(t *testing.T) {
	pub, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := provenancePackageDir(t, priv, "dev-abc")
	// 改一个字节：签名覆盖的是 plugin.json 的确切字节。
	pmPath := filepath.Join(dir, "plugin.json")
	data, err := os.ReadFile(pmPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	data = []byte(string(data[:len(data)-1]) + " }")
	if err := os.WriteFile(pmPath, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	kr := provenanceKeyring(t, map[sign.KeyID]ed25519.PublicKey{"dev-abc": pub}, nil)

	_, _, _, err = LoadPackage(dir, TrustInput{Keyring: kr})
	if err == nil {
		t.Fatal("被篡改的包没有报错")
	}
	if !errors.Is(err, ErrUntrustedPackage) {
		t.Errorf("错误没裹 ErrUntrustedPackage：%v", err)
	}
}

// TestNoTrustSetMeansUnsignedNotUnchecked：没有任何信任集时，一切都是 Unsigned，
// **不是**「不验签所以都放行」。判定与放行是两件事。
func TestNoTrustSetMeansUnsignedNotUnchecked(t *testing.T) {
	_, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := provenancePackageDir(t, priv, "dev-abc")

	_, _, prov, err := LoadPackage(dir, TrustInput{})
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if prov.State != ProvenanceUnsigned {
		t.Errorf("State = %v, want ProvenanceUnsigned", prov.State)
	}
}

// TestProvenanceStateString 钉住三个状态的渲染名，以及一个未定义值不得被某个
// default 分支冒充成三态之一——它必须自曝是个未定义值。
func TestProvenanceStateString(t *testing.T) {
	cases := []struct {
		state ProvenanceState
		want  string
	}{
		{ProvenanceUnsigned, "unsigned"},
		{ProvenanceRegistered, "registered"},
		{ProvenanceRevoked, "revoked"},
		{ProvenanceState(7), "ProvenanceState(7)"},
	}
	for _, c := range cases {
		if got := c.state.String(); got != c.want {
			t.Errorf("ProvenanceState(%d).String() = %q, want %q", int(c.state), got, c.want)
		}
	}
}
