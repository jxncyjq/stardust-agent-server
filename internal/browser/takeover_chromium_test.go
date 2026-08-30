//go:build chromium

package browser

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pollUntil 轮询 fn 直到为真或超时，超时即 t.Fatal（fail-loud，不静默放过）。
func pollUntil(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !fn() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

// 集成测试用系统 Chrome（PAL 定位），进接管后注入 click，断言页面 onclick 触发。
func TestInjectInputClickFiresOnClick(t *testing.T) {
	var clicked int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>
			<button id="b" style="position:absolute;left:0;top:0;width:100%;height:100%"
			onclick="fetch('/hit')">X</button></body></html>`))
	})
	mux.HandleFunc("/hit", func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&clicked, 1) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	obs, err := rt.Open(context.Background(), OpenReq{URL: srv.URL})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sid := obs.SessionID
	if err := rt.SetTakeover(sid, true); err != nil {
		t.Fatalf("SetTakeover: %v", err)
	}
	// 按钮铺满视口，点中心必命中。
	if err := rt.InjectInput(sid, []InputEvent{{Type: "click", X: 0.5, Y: 0.5, Button: "left"}}); err != nil {
		t.Fatalf("InjectInput: %v", err)
	}
	// 轮询等 onclick 的 fetch 落地。
	pollUntil(t, 5*time.Second, func() bool { return atomic.LoadInt32(&clicked) == 1 })
}

func TestInjectInputRequiresTakeover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>blank</body></html>`))
	}))
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	obs, err := rt.Open(context.Background(), OpenReq{URL: srv.URL})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	err = rt.InjectInput(obs.SessionID, []InputEvent{{Type: "mousemove", X: 0.5, Y: 0.5}})
	// TAKEOVER_REQUIRED，不是 SESSION_UNDER_TAKEOVER：后者说的是「会话正被人接管」，
	// 正是这里拒绝的理由的**反面**。
	var be *BrowserError
	if !asBrowserError(err, &be) || be.Code != CodeTakeoverRequired {
		t.Fatalf("inject without takeover should error CodeTakeoverRequired, got %v", err)
	}
}

// TestInjectInputAppliesModifiers 是修饰键契约的真机证据：注入 ctrl+c 之后，页面
// 看到的必须是「按下 c 且 ctrlKey 为真」，而不是一个字母 c 被打进输入框。
//
// 之前这条路根本走不通：`keydown Control` 被键名白名单拒掉，GUI 随后把 c 当普通
// 字符发出去，于是「复制」变成了「输入 c」。
func TestInjectInputAppliesModifiers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><input id="box" autofocus>
			<script>
			window.seen = [];
			addEventListener('keydown', e => { window.seen.push(e.key + ':ctrl=' + e.ctrlKey + ':shift=' + e.shiftKey); });
			</script></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	obs, err := rt.Open(context.Background(), OpenReq{URL: srv.URL})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sid := obs.SessionID
	if err := rt.SetTakeover(sid, true); err != nil {
		t.Fatalf("SetTakeover: %v", err)
	}
	if err := rt.InjectInput(sid, []InputEvent{
		{Type: "keydown", Key: "c", Modifiers: []string{"ctrl"}},
		{Type: "keyup", Key: "c", Modifiers: []string{"ctrl"}},
	}); err != nil {
		t.Fatalf("InjectInput(ctrl+c): %v", err)
	}

	_, page, err := rt.activePage(sid)
	if err != nil {
		t.Fatalf("activePage: %v", err)
	}
	seenJSON := func() string {
		t.Helper()
		res, err := page.Eval("() => JSON.stringify(window.seen)")
		if err != nil {
			t.Fatalf("eval window.seen: %v", err)
		}
		return res.Value.String()
	}
	pollUntil(t, 5*time.Second, func() bool {
		return strings.Contains(seenJSON(), "c:ctrl=true:shift=false")
	})

	// The focused input must still be empty: ctrl+c is a command, not text.
	// This is the defect stated as an assertion — the old path turned a copy
	// into a literal "c" in the box. Checked BEFORE anything else is typed, so
	// the box can only hold what ctrl+c put there.
	boxValue := func() string {
		t.Helper()
		value, err := page.Eval("() => document.getElementById('box').value")
		if err != nil {
			t.Fatalf("eval box value: %v", err)
		}
		return value.Value.String()
	}
	if got := boxValue(); got != "" {
		t.Errorf("input value = %q after ctrl+c, want empty: the shortcut was typed as text", got)
	}

	// And no modifier may be left stuck down: a later plain keystroke has to
	// arrive plain, and it types.
	if err := rt.InjectInput(sid, []InputEvent{{Type: "keydown", Key: "a"}}); err != nil {
		t.Fatalf("InjectInput(a): %v", err)
	}
	pollUntil(t, 5*time.Second, func() bool {
		return strings.Contains(seenJSON(), "a:ctrl=false:shift=false")
	})
	if got := boxValue(); got != "a" {
		t.Errorf("input value = %q after a plain keydown, want %q", got, "a")
	}
}

// TestTheBrowsersTrafficGoesThroughTheEgressProxy 是「代理确实在路径上」的真机
// 证据。单测能证明代理本身按策略放行/拒绝，证明不了 Chromium 真的在用它——而
// --proxy-server 拼错、被平台参数覆盖、或被 Chromium 对回环的默认绕过跳过，都是
// 静默失效：页面照常打开，SSRF 防护形同虚设。
//
// 计数发生在代理的拨号上：页面加载出来了、而代理一次也没被拨过，就说明浏览器绕过
// 了它。
//
// 这条测试**不**覆盖 --proxy-bypass-list=<-loopback>（它的目标是公网地址）；那个参数
// 由 TestLoopbackIsNotExemptFromTheEgressPolicy 钉住。
//
// 早先这里写过一句「那个参数的效果测不出来」，依据是一次变异没有变红——**那个结论
// 是错的**，错在当时的代理放行一切、于是「绕没绕过代理」在结果上看不出差别。换成
// 「让代理拒绝回环，再看那台服务器有没有被碰到」之后，删掉该参数立刻变红。
func TestTheBrowsersTrafficGoesThroughTheEgressProxy(t *testing.T) {
	var served atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><h1>through the proxy</h1></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	proxied := 0
	rt.mgr.egress.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		proxied++
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}

	if _, err := rt.Open(context.Background(), OpenReq{URL: srv.URL}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if proxied == 0 {
		t.Error("the page loaded without a single dial through the egress proxy: the browser is not using it")
	}
	if served.Load() == 0 {
		t.Error("the fixture server was never reached; the navigation did not happen at all")
	}
}

// TestTheRealBrowserProcessIsConfined 是「隔离确实套在了真的 Chromium 上」的证据。
// 单测只能证明按 pid 收束这件事本身成立；接线错了（拿错 pid、忘了调用、被 launcher
// 的启动顺序绕过）在单测里全是绿的。
func TestTheRealBrowserProcessIsConfined(t *testing.T) {
	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	if _, err := NewPlatformAdapter().ConfineProcess(os.Getpid()); errors.Is(err, ErrConfinementUnsupported) {
		t.Skipf("no outer confinement on this platform")
	}
	if firstChromium(t, rt).confinement == nil {
		t.Error("the browser process was launched without a confinement; a crash of this agent leaves " +
			"Chromium processes behind")
	}
}

// TestClosingTheRuntimeLeavesNoBrowserBehind 是自管启动之后必须自己扛起来的那件
// 事：进程是我们起的，就得由我们送走。
//
// go-rod 的 launcher 曾经用 leakless 做这件事；绕过它之后，如果 Close 只关 CDP
// 连接，Chromium 会留在机器上——每跑一次任务留一个，直到用户自己去清。
func TestClosingTheRuntimeLeavesNoBrowserBehind(t *testing.T) {
	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	// 先把这个进程抓在手里：Close 会把池清空，之后再去池里找它就找不到了（而
	// 「找不到」与「它已经被送走」是两件事，混为一谈会让这条断言变成空的）。
	chromium := firstChromium(t, rt)
	pid := chromium.launched.PID()
	if pid == 0 {
		t.Fatal("no browser pid; the runtime did not launch a process of its own")
	}

	if err := rt.Close(context.Background(), CloseReq{}); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 看 ProcessState 而不是 Signal(0)：Windows 上 os.Process.Signal 对任何信号都
	// 返回错误，用它探活等于写一条永远绿的断言。ProcessState 非 nil 意味着 Close
	// 真的等到了这个进程结束。
	//
	// 这条测试在 Windows 上由 Job Object 的 kill-on-close 满足（变异实测：把 Close
	// 里的 Kill 删掉它照样绿）。Kill 本身的作用在没有收束的平台上，由
	// TestCloseKillsTheProcessEvenWithoutConfinement 单独钉。
	if chromium.launched.cmd.ProcessState == nil {
		t.Fatalf("browser process %d was never reaped; Close left it running", pid)
	}
}

// firstChromium 取出运行时池里的第一个真 Chromium 进程。真机测试要看的是那个进程
// 本身（收束、pid、退出状态），而池的其余部分（复用、扩容、回收）在 pool_test.go
// 里用假成员测——那些分支在真机上几乎不可能稳定复现。
func firstChromium(t *testing.T, rt *Runtime) *chromiumInstance {
	t.Helper()

	inst := rt.mgr.pool.firstInstance()
	if inst == nil {
		t.Fatal("the runtime has no browser process")
	}
	chromium, ok := inst.(*chromiumInstance)
	if !ok {
		t.Fatalf("pool instance is %T, want a real chromium process", inst)
	}
	return chromium
}

// TestTwoSessionsSpillIntoASecondBrowserProcess 是池在真机上的证据：把每进程的
// 会话数压到 1，第二个会话就必须落到**另一个真的 Chromium 进程**上，而且两个会话
// 都还能用。
//
// 池的其余分支（复用、拒绝、回收）在 pool_test.go 里用假成员测——那些在真机上要么
// 造不出（内存撑大），要么会把测试变成对时序的赌博。
func TestTwoSessionsSpillIntoASecondBrowserProcess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><h1>pooled</h1></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rt, err := NewRuntime(RuntimeConfig{
		Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest(),
		MaxProcesses: 2, MaxContextsPerProcess: 1,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	first, err := rt.Open(context.Background(), OpenReq{URL: srv.URL})
	if err != nil {
		t.Fatalf("open first session: %v", err)
	}
	second, err := rt.Open(context.Background(), OpenReq{URL: srv.URL})
	if err != nil {
		t.Fatalf("open second session: %v", err)
	}
	if first.SessionID == second.SessionID {
		t.Fatal("the two opens landed in one session; this test is not exercising the pool")
	}
	if got := rt.mgr.Processes(); got != 2 {
		t.Errorf("browser processes = %d, want 2: the second session should have spilled into a new one", got)
	}

	// 两个会话都还能用——扩容不是「起了个进程」就算数，落在新进程上的那个也得能读。
	for _, id := range []string{first.SessionID, second.SessionID} {
		if _, err := rt.Read(context.Background(), ReadReq{SessionID: id}); err != nil {
			t.Errorf("read session %s: %v", id, err)
		}
	}
}

// TestTheBrowserRunsInsideTheOuterSandbox 是「沙箱确实在路径上」的真机证据。
//
// profile 里的那些参数测试只能证明**拼得对**；接线错了（PrepareCommand 没被调用、
// 或者返回了没包过的命令）在那些测试里全是绿的，而浏览器照常打开——一层不存在的
// 隔离，没有任何症状。
//
// 判据是进程树：包过之后，我们启动的那个进程是 bwrap，Chromium 在它下面。
func TestTheBrowserRunsInsideTheOuterSandbox(t *testing.T) {
	// 每个平台的外层沙箱是不同的东西，但判据是同一条：我们**启动的那个进程**是包装
	// 器，浏览器在它下面。
	var wrapper string
	switch runtime.GOOS {
	case "linux":
		wrapper = "/bwrap"
	default:
		t.Skip("this platform has no outer sandbox for the browser process")
	}
	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	chromium := firstChromium(t, rt)
	launchedAs := chromium.launched.cmd.Path
	if !strings.HasSuffix(launchedAs, wrapper) {
		t.Errorf("the browser was launched as %q, not through %s: the sandbox is not on the path",
			launchedAs, strings.TrimPrefix(wrapper, "/"))
	}
}

// TestLoopbackIsNotExemptFromTheEgressPolicy 补上 --proxy-bypass-list=<-loopback>
// 的**效果**覆盖。
//
// 此前只有那个参数本身，没有任何东西守着它：变异实测（删掉它）不红，因为这个
// Chromium 版本在显式 --proxy-server 下本来就不绕回环。而各版本行为不一，Chromium
// 的文档行为是**绕过** localhost——一旦某个版本恢复那个默认，回环就整段脱离出口
// 策略，而 127.0.0.1 恰恰是 SSRF 最想去的地方，且没有任何症状。
//
// 所以断言的是结果而不是参数：让代理拒绝私网、让 runtime 放行（两者的开关在这个
// 测试里被故意拆开），然后导航到一个回环上的服务器。走代理 → 看到代理的拒绝页，
// 且那台服务器一次都没被碰过；绕过代理 → 服务器会被碰到，测试红。
func TestLoopbackIsNotExemptFromTheEgressPolicy(t *testing.T) {
	var reached atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		_, _ = w.Write([]byte("<html><body>the fixture server was reached</body></html>"))
	}))
	defer srv.Close()

	// runtime 放行私网（否则 checkURL 会在导航之前就拒掉，测不到代理这一层）。
	rt, err := NewRuntime(RuntimeConfig{Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest()})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})

	// 代理这一层收紧：现在「回环该不该连」只由出口策略回答。
	rt.mgr.egress.allowPrivateHosts = false

	obs, err := rt.Open(context.Background(), OpenReq{URL: srv.URL})
	if err != nil {
		// 导航失败也是一种「被挡住」，同样满足这条性质——只要服务器没被碰到。
		if reached.Load() != 0 {
			t.Fatalf("open failed (%v) yet the loopback server was reached", err)
		}
		return
	}
	if reached.Load() != 0 {
		t.Errorf("the loopback server was reached %d time(s): the browser bypassed the egress proxy, and "+
			"127.0.0.1 is exactly where an SSRF wants to go", reached.Load())
	}
	// 不断言拒绝页的**文字**：代理回的是 text/plain 403，a11y 树上未必有可读文本
	// （实测就是空的）。要钉的是「那台服务器没被碰到」，以及页面上没有它的内容。
	if strings.Contains(obs.Observation.Text, "the fixture server was reached") {
		t.Errorf("the page shows the loopback server's content: %q", obs.Observation.Text)
	}
}
