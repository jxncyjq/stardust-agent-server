package cli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
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
//
// 它同时守着一件更根本的事：refresh 必须真的去那个地址取。取回失败的错误里必带
// 被请求的地址（trustlist 的取回路径一律把 URL 写进错误），而只读缓存的那条路径
// 造不出这个地址——它的错误里只有缓存目录。所以「地址出现在错误里」是这条命令
// 真的发过请求的凭据，把取回换成读缓存，下面那条断言随即变红。
func TestTrustlistRefreshPrintsTheStateBeforeItReportsFailure(t *testing.T) {
	srv := newSilentTLSServer(t)

	var buf bytes.Buffer
	cmd := newPluginsTrustlistCommand(&buf)
	cmd.SetArgs([]string{"refresh", "--url", srv.URL + "/trustlist.json", "--cache", t.TempDir()})
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("取不回清单，refresh 却报成功了")
	}
	if !strings.Contains(buf.String(), "status: unavailable") {
		t.Errorf("refresh 失败了却没先把手上那份状态打出来：%s", buf.String())
	}
	if !strings.Contains(err.Error(), srv.URL) {
		t.Errorf("错误里没有 %s，refresh 从没向那个地址发过请求：%v", srv.URL, err)
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
// 两种格式会让操作者以为它们看的是两个东西。
//
// 这条用例比的**只是空缓存这一支**：两条命令在没有信任集时打出的那两行状态。
// 有内容时的那一段（serial / issued / expires / publishers / 「撤销永不遗忘」
// 那一句）不在比较范围内，因为本包造不出一份能通过验签的缓存。所以一份在空缓存
// 上逐字相同、有内容时另换一套格式的打印，这条用例抓不住——抓那件事的是
// TestBothTrustlistCommandsRenderThroughPrintTrust。
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

// TestBothTrustlistCommandsRenderThroughPrintTrust：refresh 与 show 都必须经
// printTrust 出字，不得各写各的。
//
// 上一条用例只能比到空缓存那一支，而操作者真正要读的是有内容的那一段——恰恰是
// 「两种格式」最容易长出来、也最会误导人的地方。本包造不出一份能通过验签的缓存，
// 没法把有内容的输出跑出来比，所以这里改成读源码：解析 plugins_trustlist_command.go，
// 断言两条命令的构造函数体里都出现 printTrust 这个标识符。任何一条改调自己那一套
// 打印，这里就红。
func TestBothTrustlistCommandsRenderThroughPrintTrust(t *testing.T) {
	const file = "plugins_trustlist_command.go"
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	// 两个都预置成 false：函数被改名或整个不见了，与「函数体里没有 printTrust」
	// 一样是这条规则不再成立。
	rendersThroughPrintTrust := map[string]bool{
		"newTrustlistShowCommand":    false,
		"newTrustlistRefreshCommand": false,
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if _, tracked := rendersThroughPrintTrust[fn.Name.Name]; !tracked {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if id, isIdent := n.(*ast.Ident); isIdent && id.Name == "printTrust" {
				rendersThroughPrintTrust[fn.Name.Name] = true
				return false
			}
			return true
		})
	}
	for name, renders := range rendersThroughPrintTrust {
		if !renders {
			t.Errorf("%s 的函数体里没有 printTrust：它要么另写了一套打印，要么已经不叫这个名字；"+
				"两条命令用两种格式说同一件事，会让操作者以为它们看的是两个东西", name)
		}
	}
}

// TestTrustlistSignWritesASignatureOverExactlyTheDocumentItVerified：sign 的成功
// 路径——它到底写没写、写出来的东西签的是不是它刚校验过的那段字节。
//
// 这条用例必须自己传一个自验桩：本包拿不到 root 私钥（它按设计只在发布者本机），
// 所以真的 trustlist.VerifyDocument 在这里永远不会通过，成功路径一步也走不到。
// 桩只替掉「验」这一步，其余每一步都是生产代码，因此它守得住三件事：
//
//  1. 磁盘上确实有 .sig——把写出那几行删掉，命令仍会报告 signed，这条会红；
//  2. 送去自验的就是 --in 的原始字节；
//  3. 写出的签名覆盖的也是 --in 的原始字节——这一条不用桩验，用签名私钥自己的
//     公钥半边、走生产的 sign.Keyring.Verify 验，所以把 sign.Sign 签的字节换成
//     别的（哪怕只多一个空格），这条会红。
func TestTrustlistSignWritesASignatureOverExactlyTheDocumentItVerified(t *testing.T) {
	dir := t.TempDir()
	listData := validTrustlistJSON(t)
	in := filepath.Join(dir, "trustlist.json")
	if err := os.WriteFile(in, listData, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	const keyID sign.KeyID = "stand-in-for-root"
	keyPath := filepath.Join(dir, "root.json")
	writeTestPrivateKey(t, keyPath, keyID)
	out := filepath.Join(dir, "trustlist.sig")

	var verified [][]byte
	verify := func(list, _ []byte) (trustlist.Document, error) {
		verified = append(verified, append([]byte(nil), list...))
		return trustlist.ParseDocument(list)
	}

	var buf bytes.Buffer
	if err := runTrustlistSign(&buf, verify, in, keyPath, out); err != nil {
		t.Fatalf("sign: %v", err)
	}

	sigData, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("sign 报告成功，磁盘上却没有 %s：%v", out, err)
	}
	if len(verified) != 1 {
		t.Fatalf("自验被调用了 %d 次，应当恰好一次", len(verified))
	}
	if !bytes.Equal(verified[0], listData) {
		t.Errorf("送去自验的不是 --in 的原始字节：\n送去的：%s\n--in：%s", verified[0], listData)
	}

	signature, err := sign.ParseSignature(sigData)
	if err != nil {
		t.Fatalf("写出的 .sig 解析不出来：%v (%s)", err, sigData)
	}
	if err := publicKeyringFor(t, keyPath).Verify(signature, listData); err != nil {
		t.Errorf("写出的签名覆盖的不是 --in 的那段字节：%v", err)
	}
	if !strings.Contains(buf.String(), out) {
		t.Errorf("成功输出里没说签名写到了哪：%s", buf.String())
	}
}

// TestTrustlistSignCommandVerifiesThroughTheEmbeddedRoot：命令本身接的必须是
// 走内嵌 root 的那个自验，不是一个宽松的替身。
//
// 上一条用例传的是桩，桩不证明命令接了什么。这条走完整的 cobra 路径，用一把形状
// 正确但不是 root 的钥匙，断言错误是 trustlist.ErrUntrustedList——只有
// trustlist.VerifyDocument 会给出这个错误。把命令构造时传的换成一个永远成功的
// 桩（或任何别的检查），这条随即变红。
func TestTrustlistSignCommandVerifiesThroughTheEmbeddedRoot(t *testing.T) {
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
	if !errors.Is(err, trustlist.ErrUntrustedList) {
		t.Errorf("自验走的不是 trustlist 的验签（那条路径失败时一律裹 ErrUntrustedList）：%v", err)
	}
}

// TestTrustlistSignRefusesAMissingVerification：没有自验就不签。
//
// 自验这一步是可传入的，于是「一个都不传」成了一种可能的状态；它必须当场报错，
// 而不是往下走到写出一份没人检查过的签名。
func TestTrustlistSignRefusesAMissingVerification(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "trustlist.json")
	if err := os.WriteFile(in, validTrustlistJSON(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	keyPath := filepath.Join(dir, "root.json")
	writeTestPrivateKey(t, keyPath, "root-test")
	out := filepath.Join(dir, "trustlist.sig")

	err := runTrustlistSign(io.Discard, nil, in, keyPath, out)
	if err == nil {
		t.Fatal("没有自验也签了")
	}
	if !strings.Contains(err.Error(), "verification") {
		t.Errorf("错误没点明缺的是自验：%v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("拒绝之后仍然写出了 .sig 文件")
	}
}

// TestTrustlistSignRefusesEmptyPaths：三个路径 flag 给了空串就当场失败。
//
// cobra 的 required 只保证 flag 出现过，`--in ""` 照样能过那一关。空串一路走下去
// 得到的是 open : no such file or directory 这类不知所云的报错。
func TestTrustlistSignRefusesEmptyPaths(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "trustlist.json")
	if err := os.WriteFile(in, validTrustlistJSON(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	keyPath := filepath.Join(dir, "root.json")
	writeTestPrivateKey(t, keyPath, "root-test")
	out := filepath.Join(dir, "trustlist.sig")
	verify := func(list, _ []byte) (trustlist.Document, error) {
		return trustlist.ParseDocument(list)
	}

	cases := map[string]struct {
		in, key, out string
		want         string
	}{
		"--in":  {in: "   ", key: keyPath, out: out, want: "--in is empty"},
		"--key": {in: in, key: "   ", out: out, want: "--key is empty"},
		"--out": {in: in, key: keyPath, out: "   ", want: "--out is empty"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := runTrustlistSign(io.Discard, verify, tc.in, tc.key, tc.out)
			if err == nil {
				t.Fatalf("%s 是空串也签了", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误没点明是 %s 空了：%v", name, err)
			}
		})
	}
}

// TestTrustlistCommandsReportAFailedWrite：打印失败必须报出来，并且带得上是哪条
// 命令。
//
// 状态打不出去而命令报成功，是最坏的一种结果：操作者以为自己看到了现场，其实
// 什么都没看到。fail-loud 铁律要求错误既不能被吞掉，也要能定位到出错的地方，
// 所以这里连命令前缀一起断言。
//
// 用的 writer 只在第一次 Write 上失败（见 failingWriter），因此任何一行把自己的
// 写失败丢掉，这条都会红。
func TestTrustlistCommandsReportAFailedWrite(t *testing.T) {
	cases := map[string]struct {
		args func(t *testing.T) []string
		want string
	}{
		"show": {
			args: func(t *testing.T) []string { return []string{"show", "--cache", t.TempDir()} },
			want: "plugins trustlist show:",
		},
		"refresh": {
			args: func(t *testing.T) []string {
				return []string{"refresh", "--url", newSilentTLSServer(t).URL + "/trustlist.json",
					"--cache", t.TempDir()}
			},
			want: "plugins trustlist refresh:",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var sink bytes.Buffer
			cmd := newPluginsTrustlistCommand(&failingWriter{})
			cmd.SetArgs(tc.args(t))
			cmd.SetOut(&sink)
			cmd.SetErr(&sink)
			err := cmd.Execute()
			if err == nil {
				t.Fatal("状态一个字都没写出去，命令却报成功了")
			}
			// refresh 失败时错误是 errors.Join 出来的两条：取不回来那条，和打印
			// 失败那条。所以这里找的是**说到写失败的那一条**，再看它自己带没带
			// 命令前缀——只看整段文本会把取回失败那条的前缀算到打印失败头上。
			var reported string
			for _, line := range strings.Split(err.Error(), "\n") {
				if strings.Contains(line, writeRefusedByTest) {
					reported = line
					break
				}
			}
			if reported == "" {
				t.Fatalf("写失败的原因被吞掉了：%v", err)
			}
			if !strings.Contains(reported, tc.want) {
				t.Errorf("写失败那条错误里没有 %q，看不出是哪条命令写不出去：%s", tc.want, reported)
			}
		})
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

// writeRefusedByTest 是 failingWriter 报出来的原因，断言里照它找。
const writeRefusedByTest = "write refused by the test writer"

// failingWriter 只让第一次 Write 失败，之后照常写。
//
// 「只失败一次」是刻意的：一个每次都失败的 writer 抓不住「某一行悄悄丢掉了自己的
// 写失败」——后面任何一行的失败都会把错误重新带出来，看上去仍然是报了错。只有第
// 一次失败、之后成功，才能把「第一行的错被吞了」单独暴露出来。
type failingWriter struct{ wrote int }

func (w *failingWriter) Write(p []byte) (int, error) {
	w.wrote++
	if w.wrote == 1 {
		return 0, errors.New(writeRefusedByTest)
	}
	return len(p), nil
}

// publicKeyringFor 读回 path 上那把私钥，造一个只含它公钥半边的信任集。
//
// 走生产的 sign.MarshalKeyring + sign.ParseKeyring，而不是自己拼一份 JSON：拿它
// 验出来的结论才和真正的验签路径是同一条。
func publicKeyringFor(t *testing.T, path string) *sign.Keyring {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	id, priv, err := sign.ParsePrivateKey(data)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatalf("私钥的公钥半边不是 ed25519.PublicKey：%T", priv.Public())
	}
	keyringData, err := sign.MarshalKeyring(id, pub)
	if err != nil {
		t.Fatalf("MarshalKeyring: %v", err)
	}
	keyring, err := sign.ParseKeyring(keyringData)
	if err != nil {
		t.Fatalf("ParseKeyring: %v (%s)", err, keyringData)
	}
	return keyring
}

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
// 这几条用例没有给 trustlist.Config 配客户端，取回走的是 http.DefaultClient，
// 它不认这台服务器的自签证书，于是握手会失败——那正是这几条用例要的「取不回
// 来」，而且失败发生在真的连上去之后，所以错误里带着被请求的地址。服务器默认会
// 把每次握手失败打到 stderr，那是预期之内的噪声，留着只会让真正的失败更难看见。
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
