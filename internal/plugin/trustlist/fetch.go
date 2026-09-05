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
	// 体积上限是取回方的职责（这就是 document.go 里 maxPublishers 注释所指的
	// 「取回代码」）：fetchBytes 以流式方式强制**调用方传进来的那道上限**，读到
	// 上限+1 字节就停下，不把超大响应体整个读进内存。maxListBytes 与 maxSigBytes
	// 是本包为清单、签名两类文档各自选定的值，将由取回入口分别传给 fetchBytes；
	// 该入口尚未落地，所以这两个值现在还没有生产调用方。
	maxListBytes int64 = 1 << 20
	// maxSigBytes 是签名文档的上限。一份 plugin.sig 形状的文档只有几百字节。
	maxSigBytes int64 = 4 << 10
	// fetchTimeout 是单次取回的上限。体积上限拦不住慢速响应——一个每秒滴一个
	// 字节、总量永不超限的响应体永远不会触发那道判据——所以本函数自己装一道
	// 时间上限，不指望调用方的 ctx 带 deadline（调用方传 context.Background()
	// 时，没有它就是永久挂起）。
	fetchTimeout = 30 * time.Second
	// maxRedirects 与 internal/plugin/fetch 取同一个数（那边也是 10），理由也
	// 相同：足够穿过常见的 CDN 跳转，又不至于让一条重定向环耗到超时。两边的
	// 判据同为 len(via) >= maxRedirects，所以语义也一致：最多跟随 9 跳，整条链
	// 至多发出 10 次请求。
	maxRedirects = 10
)

// sigURL 由清单地址推导签名地址：把结尾的 .json 换成 .sig。
//
// 它对地址字符串逐字做后缀替换，不解析 URL 结构——scheme 之类的校验在
// fetchBytes 里做。因此判据是「整个地址以 .json 结尾」，带 query 或 fragment
// 的清单地址结尾不是 .json，会在这里被拒绝。
//
// 不给签名一个单独的配置项，是因为两个可以各自配置的 URL 就有可以指向两份
// 不匹配文档的配置——而那种不匹配的症状（验签失败）会把排查引向「被攻击了」，
// 而不是「配错了」。
func sigURL(listURL string) (string, error) {
	const suffix = ".json"
	if !strings.HasSuffix(listURL, suffix) {
		return "", fmt.Errorf("trustlist url %q does not end in %s — the check is on the whole address, "+
			"so an address carrying a query or a fragment is refused here as well; the signature's "+
			"address is derived from it by replacing that suffix, and guessing is not an option here",
			listURL, suffix)
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
//   - 重定向跳数超出上限：最多跟随 9 跳，第 10 跳被拒（见 maxRedirects）；
//   - 响应状态不是 200。**错误里必须带状态码**：GitHub 对带无效凭据的请求返回
//     404 而不是 401，状态码缺失会让鉴权问题伪装成「文件不存在」；
//   - 响应体超过 maxBytes。判据是「读到 maxBytes+1 字节」——在读完之前就停下，
//     而不是读完再判断，否则上限保护不了内存。
//
// 单次取回不超过 fetchTimeout。
func fetchBytes(ctx context.Context, client *http.Client, rawURL string, maxBytes int64) ([]byte, error) {
	return fetchBytesWithTimeout(ctx, client, rawURL, maxBytes, fetchTimeout)
}

// fetchBytesWithTimeout 是 fetchBytes 的实现，只把超时挪成参数。
//
// 拆出这一层唯一的理由是可测：超时是这段代码自带的唯一一道能终止慢速响应的
// 防线，而一个真等 fetchTimeout 的用例没人会跑，于是那条规则就没人守着。用例
// 调用本函数并传一个极短的超时；除用例外一律走 fetchBytes。
//
// 不选「把 fetchTimeout 改成包级 var + 测试钩子」那条路：包级可变量会把「哪
// 些用例不能并行」变成一条没人写下来的纪律。
func fetchBytesWithTimeout(ctx context.Context, client *http.Client, rawURL string, maxBytes int64,
	timeout time.Duration) ([]byte, error) {
	if client == nil {
		// 编程错误，不是运行期状态：裸的 nil 解引用只会给出一个指不到这里的
		// 崩溃点。与 internal/plugin/fetch.Fetch 的做法一致。
		panic("trustlist: client is nil")
	}
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

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch trustlist: build request: %w", err)
	}

	// 复制一份 client，只为装上重定向策略——不改调用方给的那个（它可能被多处
	// 并发共用）。注意调用方自己的 CheckRedirect 会被这里的策略取代：本包的
	// 重定向规则只能收紧，不接受调用方放宽。
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
