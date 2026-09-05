package trustlist

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// maxListBytes 是清单文档的上限。远超任何合理的公钥清单规模，又远小于
	// 「读进内存会造成麻烦」的量级。
	//
	// 这就是 document.go 里 maxPublishers 注释所指的「取回代码」：它以流式
	// 方式在读进内存之前强制这道上限（见 fetchBytes：读到上限+1 字节即判定
	// 超限，在读完之前就停下）。
	maxListBytes int64 = 1 << 20
	// maxSigBytes 是签名文档的上限。一份 plugin.sig 形状的文档只有几百字节。
	maxSigBytes int64 = 4 << 10
	// fetchTimeout 是单次取回的上限。
	fetchTimeout = 30 * time.Second
	// maxRedirects 与 internal/plugin/fetch 取同一个数，理由也相同：足够穿过
	// 常见的 CDN 跳转，又不至于让一条重定向环耗到超时。
	maxRedirects = 9
)

// sigURL 由清单地址推导签名地址：同目录、同文件名、后缀换成 .sig。
//
// 不给签名一个单独的配置项，是因为两个可以各自配置的 URL 就有可以指向两份
// 不匹配文档的配置——而那种不匹配的症状（验签失败）会把排查引向「被攻击了」，
// 而不是「配错了」。
func sigURL(listURL string) (string, error) {
	const suffix = ".json"
	if !strings.HasSuffix(listURL, suffix) {
		return "", fmt.Errorf("trustlist url %q does not end in %s; the signature's address is derived "+
			"from it by replacing that suffix, and guessing is not an option here", listURL, suffix)
	}
	return strings.TrimSuffix(listURL, suffix) + ".sig", nil
}

// fetchBytes 取回 rawURL 的响应体，至多 maxBytes 字节。
//
// 它不复用 internal/plugin/fetch.Fetch：那个函数要求调用方**预先知道内容的
// digest**，因为它是为「manifest 里钉死了 digest 的插件产物」设计的。信任清单
// 本来就是可变的，没有预知的 digest 可传。这里复用的是它的形状而不是它的代码。
//
// 拒绝的情况：
//
//   - rawURL 的 scheme 不是 https。清单没有 digest 兜底，明文传输意味着任何
//     中间人都能换掉它——注意这与 plugins.allow_insecure_sources 无关，那个开关
//     放宽的是有 digest 保护的插件产物；
//   - 任何一跳重定向的目标不是 https（同一个理由，降级同样致命）；
//   - 重定向超过 maxRedirects 跳；
//   - 响应状态不是 200。**错误里必须带状态码**：GitHub 对带无效凭据的请求返回
//     404 而不是 401，状态码缺失会让鉴权问题伪装成「文件不存在」；
//   - 响应体超过 maxBytes。判据是「读到 maxBytes+1 字节」——在读完之前就停下，
//     而不是读完再判断，否则上限保护不了内存。
func fetchBytes(ctx context.Context, client *http.Client, rawURL string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("fetch trustlist: max bytes is %d; it must be positive", maxBytes)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("fetch trustlist: parse url %q: %w", rawURL, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("fetch trustlist: url %q uses scheme %q, want https; a trustlist carries no "+
			"digest of its own, so plaintext transport lets any intermediary replace it "+
			"(plugins.allow_insecure_sources does not relax this — it relaxes artifact fetches, "+
			"which are digest-protected)", rawURL, u.Scheme)
	}

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch trustlist: build request: %w", err)
	}

	// 复制一份 client，只为装上重定向策略——不改调用方给的那个。
	guarded := *client
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("redirect to %q leaves https; a downgrade en route is the same exposure "+
				"as starting in plaintext", req.URL.Redacted())
		}
		return nil
	}

	resp, err := guarded.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch trustlist %s: %w", u.Redacted(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch trustlist %s: HTTP %d %s", u.Redacted(), resp.StatusCode,
			http.StatusText(resp.StatusCode))
	}

	// 多读一个字节，才能把「正好等于上限」与「超过上限」分开。
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch trustlist %s: read body: %w", u.Redacted(), err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("fetch trustlist %s: body exceeds %d bytes; a document this large is not "+
			"the one this project publishes", u.Redacted(), maxBytes)
	}
	return body, nil
}
