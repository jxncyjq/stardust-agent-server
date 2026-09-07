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
	"time"

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
	if prov.UnrecognizedKeyID != "" {
		t.Errorf("UnrecognizedKeyID = %q, want 空——这把钥匙是认得的，两个字段同时有值就把"+
			"「认出来了」和「没认出来」搅成了一件事", prov.UnrecognizedKeyID)
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
	if prov.UnrecognizedKeyID != "" {
		t.Errorf("UnrecognizedKeyID = %q, want 空——压根没有 plugin.sig，就不存在「自称由谁签名」这回事；"+
			"这里非空会让拒绝反过来暗示存在一份签名", prov.UnrecognizedKeyID)
	}
}

// TestProvenanceUnsignedWhenTheKeyIsUnknown：签了，但签它的 key 这台机器不认识。
// 对用户的意义与「压根没签」完全相同：没有任何已登记的开发者为这份字节背书。
//
// 那个 id 报在 UnrecognizedKeyID 而**不是** KeyID：两者分开，才使「读到 KeyID」
// 永远等于「这台机器有这把钥匙的记录」。混着填等于把一个陌生字符串放进一个
// 表示「已认出」的字段里，读到它的人无从分辨。
//
// 同时 UnrecognizedKeyID 必须非空：「有人签了但我们不知道是谁」严格弱于
// 「dev-stranger 签的，而我们不认识 dev-stranger」——过期的登记、写错的 key id、
// 攻击者是三种不同的应对，只有那个 id 能把它们分开。
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
	if prov.UnrecognizedKeyID != "dev-stranger" {
		t.Errorf("UnrecognizedKeyID = %q, want dev-stranger——拒绝必须说得出这个包自称由谁签名",
			prov.UnrecognizedKeyID)
	}
	if prov.Publisher != "" {
		t.Errorf("Publisher = %q, want 空——信任集外的 key 不该有展示名", prov.Publisher)
	}
}

// TestProvenanceRevoked：撤销的判定必须带上当初写下的时间与理由——
// sign.Keyring 保留它们正是为了让拒绝能说明自己，丢掉就退化成「未知钥匙」。
//
// 它同样必须带上 Publisher：一把被撤销的钥匙，界面上要能说出「谁的钥匙」被撤销了，
// 而不是只甩一个 key id——TrustInput.Publishers 里有名字时，Revoked 判定不能把它
// 丢在半路，退化成跟「未知钥匙」一样只剩一个 id。
func TestProvenanceRevoked(t *testing.T) {
	pub, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dir := provenancePackageDir(t, priv, "dev-gone")
	kr := provenanceKeyring(t,
		map[sign.KeyID]ed25519.PublicKey{"dev-gone": pub, "dev-live": pub},
		[]sign.KeyID{"dev-gone"})

	_, _, prov, err := LoadPackage(dir, TrustInput{
		Keyring:    kr,
		Publishers: map[sign.KeyID]string{"dev-gone": "张三"},
	})
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if prov.State != ProvenanceRevoked {
		t.Fatalf("State = %v, want ProvenanceRevoked", prov.State)
	}
	if prov.KeyID != "dev-gone" {
		t.Errorf("KeyID = %q, want dev-gone", prov.KeyID)
	}
	if prov.Publisher != "张三" {
		t.Errorf("Publisher = %q, want 张三", prov.Publisher)
	}
	if prov.Reason != "私钥泄漏" {
		t.Errorf("Reason = %q, want 私钥泄漏", prov.Reason)
	}
	if prov.RevokedAt.IsZero() {
		t.Error("RevokedAt 是零值——撤销时间丢了")
	}
	if prov.UnrecognizedKeyID != "" {
		t.Errorf("UnrecognizedKeyID = %q, want 空——被撤销的钥匙是这台机器认得的一把，"+
			"它不属于「不认识的 key」那一栏", prov.UnrecognizedKeyID)
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
	if prov.UnrecognizedKeyID != "" {
		t.Errorf("UnrecognizedKeyID = %q, want 空——没有信任集时 plugin.sig 根本没被读过，"+
			"报出一个 id 等于报告一次从未发生的比对", prov.UnrecognizedKeyID)
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

// TestManifestDigestHashesTheBytesOnDisk：摘要必须是 plugin.json 磁盘字节的
// sha256，而不是任何一次「解码再编码」的结果——一份会因为字段顺序变化而变化的
// 摘要，会对没人动过的包报警。格式也一并钉住：它要与 Entry.AcceptedUnsigned
// 完全一致，否则一份存下来的确认永远读不回。
func TestManifestDigestHashesTheBytesOnDisk(t *testing.T) {
	dir := provenancePackageDir(t, nil, "")
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	sum := sha256.Sum256(raw)
	want := "sha256:" + hex.EncodeToString(sum[:])

	got, err := ManifestDigest(dir)
	if err != nil {
		t.Fatalf("ManifestDigest: %v", err)
	}
	if got != want {
		t.Errorf("ManifestDigest = %q, want %q", got, want)
	}
	if !digestPattern.MatchString(got) {
		t.Errorf("ManifestDigest = %q，不符合 Entry.AcceptedUnsigned 的形状（digestPattern）", got)
	}
}

// TestManifestDigestReportsAMissingManifest：读不到就报错，绝不回落成空串。
// 一个空摘要会与「从没人认过」撞在一起，把一次读盘失败变成一句「去认一下这个
// 包」——那是对着错误的问题给出的补救。
func TestManifestDigestReportsAMissingManifest(t *testing.T) {
	dir := t.TempDir()

	got, err := ManifestDigest(dir)
	if err == nil {
		t.Fatalf("ManifestDigest = %q, error = nil，want 一个错误：目录里没有 plugin.json", got)
	}
	if got != "" {
		t.Errorf("ManifestDigest 出错时返回了 %q，want 空串", got)
	}
	if !strings.Contains(err.Error(), "plugin.json") {
		t.Errorf("ManifestDigest error = %v, want 它指出读的是哪个文件", err)
	}
}

// TestDescribeRevocation：撤销记录里的两个字段都是可选的，四种组合各有一种读法，
// 两个都没有时返回空串——那不是兜底，是「操作者什么都没写下来」这个合法状态，
// 而它被附加到的那句话本身已经说清了 key 被撤销了。
func TestDescribeRevocation(t *testing.T) {
	at := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		prov Provenance
		want string
	}{
		{name: "both", prov: Provenance{RevokedAt: at, Reason: "laptop stolen"},
			want: " at 2026-08-29T10:00:00Z (laptop stolen)"},
		{name: "time only", prov: Provenance{RevokedAt: at}, want: " at 2026-08-29T10:00:00Z"},
		{name: "reason only", prov: Provenance{Reason: "laptop stolen"}, want: " (laptop stolen)"},
		{name: "neither", prov: Provenance{}, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DescribeRevocation(c.prov); got != c.want {
				t.Errorf("DescribeRevocation = %q, want %q", got, c.want)
			}
		})
	}
}
