//go:build network

package browser

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 这一组走**真实网络**，因此默认不跑：`go test -tags network ./internal/browser/`。
//
// 单测里的代理面对的是编排好的解析器与本地服务器；真实网络会带来它们造不出的东西：
// 真的 TLS 握手（CONNECT 隧道之后我们只是拷字节，拷错了在本地看不出来）、真的跨站
// 重定向（每一跳都是新的一次受检请求）、以及一次量级足够的传输（隧道在中途断掉的
// 典型症状是拷到一半就停，而小响应永远碰不到）。
//
// 不进 CI：CI 的网络是另一回事，而一条依赖外网的红线会变成没人看的红线。它是**人在
// 真机上跑一遍**的那种验证，结论记在
// docs/superpowers/plans/2026-08-29-plugin-realmachine-verification.md 里。

func networkProxy(t *testing.T) *http.Client {
	t.Helper()

	proxy, err := startEgressProxy(egressProxyConfig{AllowPrivateHosts: false})
	if err != nil {
		t.Fatalf("startEgressProxy: %v", err)
	}
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Errorf("close proxy: %v", err)
		}
	})
	parsed, err := url.Parse(proxy.URL())
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(parsed)},
		Timeout:   90 * time.Second,
	}
}

// TestRealHTTPSGoesThroughTheTunnel：https 走 CONNECT，代理只校验目标再原样转发
// 字节。本地测试用的是明文，隧道里 TLS 握手能不能成从来没被验证过。
func TestRealHTTPSGoesThroughTheTunnel(t *testing.T) {
	resp, err := networkProxy(t).Get("https://example.com/")
	if err != nil {
		t.Fatalf("GET https://example.com/ through the proxy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(strings.ToLower(string(body)), "example domain") {
		t.Errorf("body does not look like the real page: %.200s", body)
	}
}

// TestRealRedirectChainIsCheckedHopByHop：每一跳都是经过代理的新请求，所以每一跳都
// 各自受检。http→https 是一次真实的跨协议重定向，也正是把 CONNECT 隧道的缺陷抖出来
// 的那条路径（首版实现在这里丢掉了握手的开头字节）。
//
// 目标从 github.com 换成了 cloudflare.com：开发这台机器到 github:443 的 TCP 本身
// 不稳（直连三次一通两超时），而一条依赖那种链路的断言只会周期性地骗人。选谁不重要，
// 重要的是它真的跨了协议。
func TestRealRedirectChainIsCheckedHopByHop(t *testing.T) {
	const start = "http://www.cloudflare.com/"

	resp, err := networkProxy(t).Get(start)
	if err != nil {
		t.Fatalf("GET %s through the proxy: %v", start, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after following the redirect", resp.StatusCode)
	}
	if resp.Request.URL.Scheme != "https" {
		t.Errorf("final scheme = %s, want https: the redirect was not followed through the proxy",
			resp.Request.URL.Scheme)
	}
}

// TestARealDownloadSurvivesTheTunnel：隧道断在中途的典型症状是拷到一半就停，而小
// 响应永远碰不到那种情况。
func TestARealDownloadSurvivesTheTunnel(t *testing.T) {
	const want = 10_000_000

	started := time.Now()
	resp, err := networkProxy(t).Get("https://speed.cloudflare.com/__down?bytes=10000000")
	if err != nil {
		t.Fatalf("start the download: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("the tunnel broke after %d of %d bytes: %v", n, want, err)
	}
	if n != want {
		t.Errorf("received %d bytes, want %d", n, want)
	}
	t.Logf("%d bytes through the tunnel in %s", n, time.Since(started).Round(time.Millisecond))
}

// TestCloudMetadataStaysBlockedOnARealNetwork：169.254.169.254 是 SSRF 最想去的
// 地方。这条在真实网络上再确认一次——本地测试用的是编排的解析器，真机上走的是系统
// 解析器与真实路由。
func TestCloudMetadataStaysBlockedOnARealNetwork(t *testing.T) {
	resp, err := networkProxy(t).Get("http://169.254.169.254/latest/meta-data/")
	if err != nil {
		// 传输层直接失败也算挡住了。
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403: %.200s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "blocked by the agent browser egress policy") {
		t.Errorf("body = %.200s, want the proxy's own refusal", body)
	}
}
