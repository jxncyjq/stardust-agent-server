package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// SSRF 防护此前只在 browser_open 那一刻查一次：解析主机名 → 看 IP 是不是私网 →
// 放行，然后 **Chromium 自己再解析一次**。中间那段窗口就是 DNS rebinding：同一个
// 名字第二次解析回 127.0.0.1 或 169.254.169.254，浏览器连过去，而检查早已通过。
//
// 更大的洞是它只看**第一个 URL**：页面 302 到内网、页面里的 <img src> 指向元数据
// 服务、JS fetch 一个私网地址——没有任何一条经过那次检查。
//
// 这些测试钉的是新的出口：浏览器的全部流量走一个本机代理，代理自己解析、自己校验、
// **自己拨号到刚校验过的那个 IP**。没有第二次解析，就没有可以被换掉的那一刻。

// fakeDNS 是一个可编排的解析器：按调用次序返回不同答案，用来制造 rebinding。
type fakeDNS struct {
	answers [][]net.IP
	calls   atomic.Int32
}

func (f *fakeDNS) lookup(_ context.Context, _ string) ([]net.IP, error) {
	i := int(f.calls.Add(1)) - 1
	if i >= len(f.answers) {
		i = len(f.answers) - 1
	}
	if len(f.answers[i]) == 0 {
		return nil, errors.New("no such host")
	}
	return f.answers[i], nil
}

// recordingDialer 记下每次拨号的目标地址，并把连接转到 upstream（真正的测试服务器）。
type recordingDialer struct {
	upstream string
	dialed   []string
}

func (d *recordingDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.dialed = append(d.dialed, addr)
	var std net.Dialer
	return std.DialContext(ctx, network, d.upstream)
}

func startTestProxy(t *testing.T, allowPrivate bool, dns *fakeDNS, dialer *recordingDialer) *egressProxy {
	t.Helper()

	p, err := startEgressProxy(egressProxyConfig{AllowPrivateHosts: allowPrivate})
	if err != nil {
		t.Fatalf("startEgressProxy: %v", err)
	}
	if dns != nil {
		p.resolve = dns.lookup
	}
	if dialer != nil {
		p.dial = dialer.dial
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("close proxy: %v", err)
		}
	})
	return p
}

func proxyClient(t *testing.T, p *egressProxy) *http.Client {
	t.Helper()

	u, err := url.Parse(p.URL())
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
}

func getThroughProxy(t *testing.T, p *egressProxy, target string) (int, string) {
	t.Helper()

	resp, err := proxyClient(t, p).Get(target)
	if err != nil {
		t.Fatalf("GET %s through the proxy: %v", target, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestTheProxyRefusesAPrivateTarget(t *testing.T) {
	t.Parallel()

	p := startTestProxy(t, false, &fakeDNS{answers: [][]net.IP{{net.ParseIP("169.254.169.254")}}}, nil)

	status, body := getThroughProxy(t, p, "http://metadata.example/latest/meta-data/")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a link-local target", status)
	}
	if !strings.Contains(body, "169.254.169.254") {
		t.Errorf("body = %q, want it to name the address it refused", body)
	}
}

// TestTheProxyDialsTheAddressItChecked is the rebinding property itself. The
// name resolves to a public address when checked and to loopback a moment
// later; the connection must go to the address that PASSED, and the second
// answer must never be asked for.
func TestTheProxyDialsTheAddressItChecked(t *testing.T) {
	t.Parallel()

	upstream := startEchoServer(t)
	dns := &fakeDNS{answers: [][]net.IP{
		{net.ParseIP("203.0.113.10")}, // checked: public, allowed
		{net.ParseIP("127.0.0.1")},    // rebound: what a second resolution would give
	}}
	dialer := &recordingDialer{upstream: upstream}
	p := startTestProxy(t, false, dns, dialer)

	status, body := getThroughProxy(t, p, "http://rebind.example/hello")
	if status != http.StatusOK || body != "echo:/hello" {
		t.Fatalf("status=%d body=%q, want the request to reach the checked address", status, body)
	}
	if len(dialer.dialed) != 1 || dialer.dialed[0] != "203.0.113.10:80" {
		t.Errorf("dialed = %v, want exactly the checked address 203.0.113.10:80", dialer.dialed)
	}
	if got := dns.calls.Load(); got != 1 {
		t.Errorf("resolutions = %d, want 1: a second lookup is the window rebinding uses", got)
	}
}

// TestARedirectIntoThePrivateNetworkIsRefused: the old check saw only the first
// URL. Every hop is a request through this proxy, so every hop is checked.
func TestARedirectIntoThePrivateNetworkIsRefused(t *testing.T) {
	t.Parallel()

	dns := &fakeDNS{answers: [][]net.IP{
		{net.ParseIP("203.0.113.10")}, // first hop: allowed
		{net.ParseIP("10.0.0.5")},     // redirect target: private
	}}
	upstream := startRedirectServer(t, "http://internal.example/admin")
	p := startTestProxy(t, false, dns, &recordingDialer{upstream: upstream})

	resp, err := proxyClient(t, p).Get("http://public.example/start")
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want the redirect hop refused with 403", resp.StatusCode)
		}
		return
	}
	if !strings.Contains(err.Error(), "403") && !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("error = %v, want a refusal of the private redirect target", err)
	}
}

// TestADeploymentThatAllowsPrivateHostsStillGetsAPinnedDial: turning the SSRF
// guard off is a development affordance; it must not also turn off pinning, or
// the dev-mode browser behaves differently from the one being debugged.
func TestADeploymentThatAllowsPrivateHostsStillGetsAPinnedDial(t *testing.T) {
	t.Parallel()

	upstream := startEchoServer(t)
	dialer := &recordingDialer{upstream: upstream}
	p := startTestProxy(t, true, &fakeDNS{answers: [][]net.IP{{net.ParseIP("127.0.0.1")}}}, dialer)

	if status, _ := getThroughProxy(t, p, "http://localhost.example/hello"); status != http.StatusOK {
		t.Errorf("status = %d, want 200 when private hosts are allowed", status)
	}
	if len(dialer.dialed) != 1 || !strings.HasPrefix(dialer.dialed[0], "127.0.0.1:") {
		t.Errorf("dialed = %v, want the checked loopback address", dialer.dialed)
	}
}

// TestConnectIsCheckedToo: https goes through CONNECT, and an unchecked CONNECT
// would leave every https URL unguarded — which is most of the web.
func TestConnectIsCheckedToo(t *testing.T) {
	t.Parallel()

	p := startTestProxy(t, false, &fakeDNS{answers: [][]net.IP{{net.ParseIP("127.0.0.1")}}}, nil)

	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprint(conn, "CONNECT internal.example:443 HTTP/1.1\r\nHost: internal.example:443\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "403") {
		t.Errorf("CONNECT response = %q, want 403", string(buf[:n]))
	}
}

// startEchoServer 起一个把请求路径回显出来的服务器，返回它的 host:port。
func startEchoServer(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "echo:"+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// startRedirectServer 起一个把任何请求 302 到 location 的服务器。
func startRedirectServer(t *testing.T, location string) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, location, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}
