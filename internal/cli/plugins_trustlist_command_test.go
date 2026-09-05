package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/plugin/sign"
	"github.com/stardust/legion-agent/internal/plugin/trustlist"
)

// TestTrustlistSignRefusesAMalformedDocument：格式不对就不签。
//
// 签一份验证方会拒绝的文档，会教育所有人「验签是坏的」，比不签更糟。
func TestTrustlistSignRefusesAMalformedDocument(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "trustlist.json")
	if err := os.WriteFile(in, []byte(`{"serial":0}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	keyPath := filepath.Join(dir, "root.json")
	writeTestPrivateKey(t, keyPath, "root-test")
	out := filepath.Join(dir, "trustlist.sig")

	var buf bytes.Buffer
	cmd := newPluginsTrustlistCommand(&buf)
	cmd.SetArgs([]string{"sign", "--in", in, "--key", keyPath, "--out", out})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err == nil {
		t.Fatal("格式非法的清单被签了")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("拒绝之后仍然写出了 .sig 文件")
	}
}

// TestTrustlistSignRefusesAMalformedDocumentBeforeItReadsTheKey：格式校验必须
// 排在读私钥之前。
//
// 这条用例把私钥文件整个拿掉：如果第一步真的是格式校验，报出来的必须是清单的
// 问题；如果实现把读私钥挪到了前面，报出来的会是「读不到私钥」——那时操作者
// 修好了私钥路径，才会撞上清单本身的问题，白跑一轮。
func TestTrustlistSignRefusesAMalformedDocumentBeforeItReadsTheKey(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "trustlist.json")
	if err := os.WriteFile(in, []byte(`{"serial":0}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := filepath.Join(dir, "trustlist.sig")

	var buf bytes.Buffer
	cmd := newPluginsTrustlistCommand(&buf)
	cmd.SetArgs([]string{"sign", "--in", in, "--key", filepath.Join(dir, "absent.json"), "--out", out})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("格式非法的清单被签了")
	}
	if !strings.Contains(err.Error(), "not a valid trustlist") {
		t.Errorf("报的不是清单格式的问题，说明格式校验没有排在最前面：%v", err)
	}
}

// TestTrustlistSignRefusesAnInconsistentPrivateKey：私钥的两半不自洽时必须
// 当场失败。
//
// sign.ParsePrivateKey 的文档写明它不检查这一点：一个手工编辑过的私钥能愉快地
// 签出谁也验不过的签名。自验是唯一能当场发现它的地方——否则这个错误要等到
// 用户机器上才现形。
//
// 注意这条用例在**哪一步**红：本包的用例拿不到 root 私钥（它按设计只在发布者
// 本机），所以这里造的 key id 不是 root_keys.json 里那个，自验会在「这不是
// root 那把钥匙」这一支上失败，而不是在「两半对不上」那一支。两支都在同一个
// 自验调用里——删掉自验，这条用例就会红——但「两半对不上」这一支只有拿着真
// root 私钥时才走得到，那属于人工验证的范围。
func TestTrustlistSignRefusesAnInconsistentPrivateKey(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "trustlist.json")
	if err := os.WriteFile(in, validTrustlistJSON(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	keyPath := filepath.Join(dir, "root.json")
	writeInconsistentPrivateKey(t, keyPath, "root-test")
	out := filepath.Join(dir, "trustlist.sig")

	var buf bytes.Buffer
	cmd := newPluginsTrustlistCommand(&buf)
	cmd.SetArgs([]string{"sign", "--in", in, "--key", keyPath, "--out", out})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err == nil {
		t.Fatal("两半不自洽的私钥签出的签名被接受了")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("自验失败之后仍然写出了 .sig 文件")
	}
}

// TestTrustlistSignRefusesAKeyThatIsNotTheEmbeddedRoot：一把形状完全正确、两半
// 也自洽的钥匙，只要不是内嵌的 root，签出来的东西谁也验不过，所以不写。
//
// 这条与上一条分开，是因为它单独盯住自验里「用**内嵌 root 公钥**验」这件事：
// 把自验换成任何一种只拿这把钥匙自己的公钥去验的检查，上一条仍然红（那把钥匙
// 本来就自相矛盾），这一条会绿——而那正是把一份没人能用的签名写到磁盘上。
func TestTrustlistSignRefusesAKeyThatIsNotTheEmbeddedRoot(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "trustlist.json")
	if err := os.WriteFile(in, validTrustlistJSON(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	keyPath := filepath.Join(dir, "not-root.json")
	writeTestPrivateKey(t, keyPath, "not-the-root-key")
	out := filepath.Join(dir, "trustlist.sig")

	var buf bytes.Buffer
	cmd := newPluginsTrustlistCommand(&buf)
	cmd.SetArgs([]string{"sign", "--in", in, "--key", keyPath, "--out", out})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("不是 root 的钥匙签出的签名被接受了")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("自验失败之后仍然写出了 .sig 文件")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("错误没有点明问题出在 root 钥匙上，操作者会往别处找：%v", err)
	}
}

// TestTrustlistShowReportsUnavailableOnAnEmptyCache：空缓存不是错误。
func TestTrustlistShowReportsUnavailableOnAnEmptyCache(t *testing.T) {
	var buf bytes.Buffer
	cmd := newPluginsTrustlistCommand(&buf)
	cmd.SetArgs([]string{"show", "--cache", t.TempDir()})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// 空缓存不是错误——它是全新安装的正常状态。命令要打印状态，不是崩掉。
	if err := cmd.Execute(); err != nil {
		t.Fatalf("show on an empty cache: %v", err)
	}
	if !strings.Contains(buf.String(), "unavailable") {
		t.Errorf("输出里没说状态是 unavailable：%s", buf.String())
	}
	if !strings.Contains(buf.String(), "every plugin will be treated as unregistered") {
		t.Errorf("输出里没说没有信任集的后果：%s", buf.String())
	}
}

// TestTrustlistShowNamesNoAddress：show 不发网络请求，所以它也永远不该在输出
// 里提一个地址。
//
// 这条同时守着两件事。一是那个只为满足 trustlist.NewStore 形状校验而存在的占位
// URL 绝不能出现在操作者眼前——它指向的主机不存在，一个照着它去排查的人会走
// 完全错误的方向。二是「不发请求」本身：取回失败的错误里带着被请求的地址
// （trustlist 的 fetch 路径就是这么报的），所以只要有人把 Current 换成 Refresh，
// 那个地址就会从 note 里冒出来，这条用例随即变红。
func TestTrustlistShowNamesNoAddress(t *testing.T) {
	cases := map[string]func(t *testing.T) string{
		"empty cache":  func(t *testing.T) string { return t.TempDir() },
		"broken cache": writeBrokenTrustlistCache,
	}
	for name, makeCache := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			cmd := newPluginsTrustlistCommand(&buf)
			cmd.SetArgs([]string{"show", "--cache", makeCache(t)})
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("show: %v", err)
			}
			if strings.Contains(buf.String(), "://") {
				t.Errorf("show 的输出里出现了一个地址，而它从不请求任何地址：%s", buf.String())
			}
		})
	}
}

// TestTrustlistShowReportsABrokenCacheWithoutFailing：缓存损坏时 show 仍然打印
// 状态，并把损坏的原因原样说出来。
//
// 它不静默：损坏的原因逐字打在输出里。它也不失败退出：show 是操作者在别的东西
// 已经不对劲时用来看现场的命令，让它自己崩掉等于把现场也一起拿走。真正会因为
// 拿不到清单而失败的是 refresh。
func TestTrustlistShowReportsABrokenCacheWithoutFailing(t *testing.T) {
	var buf bytes.Buffer
	cmd := newPluginsTrustlistCommand(&buf)
	cmd.SetArgs([]string{"show", "--cache", writeBrokenTrustlistCache(t)})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("show on a broken cache: %v", err)
	}
	if !strings.Contains(buf.String(), "unavailable") {
		t.Errorf("输出里没说状态是 unavailable：%s", buf.String())
	}
	if !strings.Contains(buf.String(), "note:") {
		t.Errorf("缓存损坏了却没说是为什么：%s", buf.String())
	}
}

// TestTrustlistRefreshPrintsTheStateBeforeItReportsFailure：refresh 失败时先把
// 手上那份状态打给操作者，再报错。
//
// 操作者要的是两件事，不是一件：这次刷新失败了（所以要报错、要非零退出），以及
// 手上那份还能不能用（所以要打印）。只报错等于让他再跑一次 show 才知道自己是不是
// 已经彻底没有信任集了。
func TestTrustlistRefreshPrintsTheStateBeforeItReportsFailure(t *testing.T) {
	srv := newSilentTLSServer(t)

	var buf bytes.Buffer
	cmd := newPluginsTrustlistCommand(&buf)
	cmd.SetArgs([]string{"refresh", "--url", srv.URL + "/trustlist.json", "--cache", t.TempDir()})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err == nil {
		t.Fatal("取不回清单，refresh 却报成功了")
	}
	if !strings.Contains(buf.String(), "status: unavailable") {
		t.Errorf("refresh 失败了却没先把手上那份状态打出来：%s", buf.String())
	}
}

// TestPrintTrustMarksRevokedKeysAndCountsTheOnesNoLongerListed：撤销累积集里
// 那些已经不在当前清单 keys 里的 key_id 仍然被拒，输出必须说出来。
//
// 这是「撤销永不遗忘」在操作者眼前唯一的证据。不说这一句，publishers 的条数与
// 被拒的钥匙数就对不上，而一个数字对不上又没有解释的输出，会让人以为哪里坏了。
func TestPrintTrustMarksRevokedKeysAndCountsTheOnesNoLongerListed(t *testing.T) {
	keyring := keyringWithRevocations(t, []sign.KeyID{"dev-alive", "dev-dead"},
		[]sign.KeyID{"dev-dead", "dev-long-gone"})
	trust := trustlist.Trust{
		Keyring: keyring,
		Publishers: map[sign.KeyID]trustlist.Publisher{
			"dev-alive": {KeyID: "dev-alive", DisplayName: "张三"},
		},
		Status:    trustlist.StatusFresh,
		Serial:    7,
		IssuedAt:  time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}

	var buf bytes.Buffer
	if err := printTrust(&buf, trust); err != nil {
		t.Fatalf("printTrust: %v", err)
	}
	got := buf.String()

	for _, want := range []string{
		"status: fresh",
		"serial: 7",
		"张三 (dev-alive)",
		"dev-dead [REVOKED]",
		"1 revoked key(s) no longer listed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出里没有 %q：\n%s", want, got)
		}
	}
}

// TestPrintTrustIsSharedByRefreshAndShow：refresh 与 show 报告的是同一件事，
// 必须用同一种格式说。
//
// 两种格式会让操作者以为它们看的是两个东西。这条用例把两条命令在同一个空缓存上
// 的输出逐字比较——它们此刻描述的是同一份状态，所以只要有一条命令自己另写了一套
// 打印，比较就不相等。
func TestPrintTrustIsSharedByRefreshAndShow(t *testing.T) {
	cacheDir := t.TempDir()

	var showBuf bytes.Buffer
	show := newPluginsTrustlistCommand(&showBuf)
	show.SetArgs([]string{"show", "--cache", cacheDir})
	show.SetOut(&showBuf)
	show.SetErr(&showBuf)
	if err := show.Execute(); err != nil {
		t.Fatalf("show: %v", err)
	}

	srv := newSilentTLSServer(t)
	var refreshBuf bytes.Buffer
	refresh := newPluginsTrustlistCommand(&refreshBuf)
	refresh.SetArgs([]string{"refresh", "--url", srv.URL + "/trustlist.json", "--cache", t.TempDir()})
	refresh.SetOut(&refreshBuf)
	refresh.SetErr(&refreshBuf)
	if err := refresh.Execute(); err == nil {
		t.Fatal("取不回清单，refresh 却报成功了")
	}

	// show 在空缓存上还会追加一行 note（缓存为什么是空的），refresh 的输出后面
	// 跟着 cobra 打的错误；比较的是两者共有的那段状态。
	wantPrefix := "status: unavailable\n" +
		"no trust set on this machine; every plugin will be treated as unregistered\n"
	if !strings.HasPrefix(showBuf.String(), wantPrefix) {
		t.Errorf("show 的状态段与共用的打印不一致：\n%s", showBuf.String())
	}
	if !strings.HasPrefix(refreshBuf.String(), wantPrefix) {
		t.Errorf("refresh 的状态段与共用的打印不一致：\n%s", refreshBuf.String())
	}
}

// TestPluginsCommandRegistersTrustlist：子命令必须真的挂在 `agent plugins`
// 下面。
//
// 实现写好了却没接线，是本仓反复出现的一类缺陷：单元测试全绿，而操作者敲下去
// 得到的是 "unknown command"。
func TestPluginsCommandRegistersTrustlist(t *testing.T) {
	var buf bytes.Buffer
	root := NewRoot(app.New(), &buf)
	root.SetArgs([]string{"plugins", "trustlist", "show", "--cache", t.TempDir()})
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatalf("agent plugins trustlist show: %v", err)
	}
	if !strings.Contains(buf.String(), "status: unavailable") {
		t.Errorf("`agent plugins trustlist show` 没有走到 show：%s", buf.String())
	}
}

// --- 测试辅助 -----------------------------------------------------------

func writeTestPrivateKey(t *testing.T, path string, id sign.KeyID) {
	t.Helper()
	_, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	data, err := sign.MarshalPrivateKey(id, priv)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// writeInconsistentPrivateKey 造一把两半对不上的私钥：seed 取 A 的，公钥半边
// 取 B 的。它能通过 ParsePrivateKey 的全部形状检查，签出的签名却验不过。
func writeInconsistentPrivateKey(t *testing.T, path string, id sign.KeyID) {
	t.Helper()
	_, privA, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pubB, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	broken := append([]byte(nil), privA...)
	copy(broken[32:], pubB) // 后 32 字节是公钥半边
	data, err := sign.MarshalPrivateKey(id, broken)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

func validTrustlistJSON(t *testing.T) []byte {
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
		t.Fatalf("unmarshal: %v", err)
	}
	data, err := json.Marshal(map[string]any{
		"serial":     1,
		"issued_at":  "2026-09-05T02:00:00Z",
		"expires_at": "2026-10-05T02:00:00Z",
		"keyring":    map[string]any{"keys": []any{entryMap}},
		"publishers": []any{map[string]any{"key_id": "dev-abc", "display_name": "张三"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// writeBrokenTrustlistCache 造一个损坏的缓存目录：清单在、签名不在。
//
// 这是 trustlist 的缓存读取判定为「不完整、不予使用」的一种真实残局（一次首写
// 崩在两个文件中间就是这样），与「什么都还没有」是两回事。
func writeBrokenTrustlistCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "trustlist.json"), []byte("not json at all"), 0o600); err != nil {
		t.Fatalf("write broken cache: %v", err)
	}
	return dir
}

// keyringWithRevocations 造一个 keys 为 ids、revoked 为 revoked 的信任集。
//
// revoked 里允许出现不在 ids 里的 key_id——那正是撤销累积集并进来之后的样子，
// 也是这组用例要看的东西。
func keyringWithRevocations(t *testing.T, ids, revoked []sign.KeyID) *sign.Keyring {
	t.Helper()
	keys := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		pub, _, err := sign.GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		keys = append(keys, map[string]string{
			"id":         string(id),
			"algorithm":  "ed25519",
			"public_key": base64.StdEncoding.EncodeToString(pub),
		})
	}
	entries := make([]map[string]string, 0, len(revoked))
	for _, id := range revoked {
		entries = append(entries, map[string]string{"key_id": string(id)})
	}
	data, err := json.Marshal(map[string]any{"keys": keys, "revoked": entries})
	if err != nil {
		t.Fatalf("marshal keyring: %v", err)
	}
	keyring, err := sign.ParseKeyring(data)
	if err != nil {
		t.Fatalf("ParseKeyring: %v (%s)", err, data)
	}
	return keyring
}

// newSilentTLSServer 起一个 https 测试服务器，并把它自己的错误日志丢掉。
//
// refresh 用的是 trustlist 自己造的 http 客户端，它不认这台服务器的自签证书，
// 于是握手会失败——那正是这几条用例要的「取不回来」。服务器默认会把每次握手
// 失败打到 stderr，那是预期之内的噪声，留着只会让真正的失败更难看见。
func newSilentTLSServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	// 在 StartTLS 之前设：服务器一旦跑起来，那个字段就有另一个 goroutine 在读了。
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}
