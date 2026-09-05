package trustlist

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestSigURLDerivesFromTheListURL 钉住这个函数的全部承诺：返回的签名地址与清单
// 地址**只**在结尾那一处不同——砍掉 .json、接上 .sig，其余每一个字节原样保留。
//
// 带 %2F、%2E、%20、非 ASCII、+ 的路径都列在这里，因为它们正是「解析一遍再
// 序列化回去」会被悄悄改写的那些形状：%2F 会被解成真斜杠而多出一层目录，非 ASCII
// 会被重新百分号编码。每个 want 都手写出来（读的人一眼看得出推导做了什么），再由
// 循环里那条自检把它与「只换结尾那个后缀」对上——手写的期望值本身写错了，用例
// 就会先报自己写错了，而不是替一个错的实现背书。
func TestSigURLDerivesFromTheListURL(t *testing.T) {
	t.Parallel()

	const (
		listSuffix = ".json"
		sigSuffix  = ".sig"
	)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"寻常清单地址", "https://example.com/trust/trustlist.json", "https://example.com/trust/trustlist.sig"},
		// 路径别处含 .json 不影响推导：换掉的必须是结尾那一个。
		{"路径别处也含 .json", "https://example.com/a.json/b/trustlist.json",
			"https://example.com/a.json/b/trustlist.sig"},
		// 段内转义斜杠：%2F 是路径段内的一个字符，不是分隔符。解码之后再序列化
		// 回去会把它变成真斜杠，路径就多了一层目录——那是另一个资源。
		{"段内转义斜杠", "https://example.com/a%2Fb/trustlist.json",
			"https://example.com/a%2Fb/trustlist.sig"},
		{"转义的普通字符", "https://example.com/tr%75stlist.json", "https://example.com/tr%75stlist.sig"},
		{"转义的空格", "https://example.com/trust/my%20list.json", "https://example.com/trust/my%20list.sig"},
		{"未转义的空格", "https://example.com/trust/my list.json", "https://example.com/trust/my list.sig"},
		// 非 ASCII：逐字写的与百分号编码的都不能被改写成对方。
		{"逐字非 ASCII", "https://example.com/中/trustlist.json", "https://example.com/中/trustlist.sig"},
		{"编码后的非 ASCII", "https://example.com/%E4%B8%AD/trustlist.json",
			"https://example.com/%E4%B8%AD/trustlist.sig"},
		{"路径里的加号", "https://example.com/a+b/trustlist.json", "https://example.com/a+b/trustlist.sig"},
		// scheme 与 host 的大小写、端口、userinfo、点段：都不归一化。
		{"大写 scheme 与 host", "HTTPS://EXAMPLE.COM/Trust/TrustList.json",
			"HTTPS://EXAMPLE.COM/Trust/TrustList.sig"},
		{"端口", "https://example.com:8443/trustlist.json", "https://example.com:8443/trustlist.sig"},
		// userinfo 跟随：它属于 authority，而推导只动路径结尾，签名地址与清单地址
		// 的 authority 逐字相同——凭据去的还是同一个来源。
		{"userinfo", "https://user:pass@example.com/trustlist.json",
			"https://user:pass@example.com/trustlist.sig"},
		{"IPv6", "https://[::1]:8443/trustlist.json", "https://[::1]:8443/trustlist.sig"},
		{"点段不归一化", "https://example.com/p/../trustlist.json", "https://example.com/p/../trustlist.sig"},
		{"整个文件名就是后缀", "https://example.com/.json", "https://example.com/.sig"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if derived := tt.in[:len(tt.in)-len(listSuffix)] + sigSuffix; tt.want != derived {
				t.Fatalf("用例自己写错了：want %q，而「只换结尾那个后缀」给出的是 %q", tt.want, derived)
			}
			got, err := sigURL(tt.in)
			if err != nil {
				t.Fatalf("sigURL(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("sigURL(%q) = %q, want %q——推导动了结尾那个后缀以外的字节", tt.in, got, tt.want)
			}
		})
	}
}

// TestSigURLRefusesAnUnexpectedSuffix：清单地址必须**未解码的路径**以 .json 结尾、
// 地址本身也逐字以 .json 结尾、且不带 query / fragment，否则推导出的签名地址就是
// 猜的。给两个可以各自配置的 URL 才是真正的危险（两份不匹配的文档），但推导也
// 必须是确定的，不能沉默地拼错。
//
// 这里逐条钉住的正是「沉默地拼错」那几条路：对整个地址字符串做后缀替换时，
// `…/trustlist.json?fallback=old.json` 会被当成合法输入，砍掉的是 query 尾巴上
// 的 .json，推导出 `…/trustlist.json?fallback=old.sig`——既不是清单也不是签名；
// 拿解码后的 u.Path 判后缀时，`…/list%2Ejson` 会被当成清单，推导出 `…/list.sig`
// ——另一个资源。所以每个用例除了「被拒」还断言**拒绝的理由**：三条判据的理由
// 互不相同，理由说错了就说明判据挪了位置，而不是只挪了措辞。
func TestSigURLRefusesAnUnexpectedSuffix(t *testing.T) {
	t.Parallel()

	const (
		suffixReason   = "does not end in .json"
		queryReason    = "carries a query or a fragment"
		trailingReason = "trailing characters that url.Parse drops"
	)
	tests := []struct {
		name   string
		in     string
		reason string
	}{
		{"没有后缀", "https://example.com/trust/trustlist", suffixReason},
		{"后缀之后还有斜杠", "https://example.com/trustlist.json/", suffixReason},
		// 大小写：URL 路径区分大小写，同一台服务器上 .json 与 .JSON 是两个资源。
		{"大写 .JSON", "https://example.com/trust/trustlist.JSON", suffixReason},
		// 路径里别处含 .json、结尾却不是。
		{"路径别处含 .json", "https://example.com/a.json.d/list.txt", suffixReason},
		// 判据在**未解码**的路径上：解码后的 u.Path 是 /trust/list.json，据此推导
		// 会得出 …/list.sig，而 %2E 与 . 在同一台服务器上是两个资源。理由若不是
		// suffixReason，就说明判据回到了 u.Path。
		{"解码之后才以 .json 结尾", "https://example.com/trust/list%2Ejson", suffixReason},
		// 判据在路径上而不在整串上：整串确实以 .json 结尾，路径却不是。理由
		// 若报成 queryReason，就说明判据又回到了整个字符串。
		{"只有 query 以 .json 结尾", "https://example.com/x?a=b.json", suffixReason},
		{"带 query", "https://example.com/trust/trustlist.json?token=abc", queryReason},
		// 复审探针抓到的那一条：整串后缀替换会推导出 …?fallback=old.sig。
		{"query 也以 .json 结尾", "https://example.com/trustlist.json?fallback=old.json", queryReason},
		{"带 fragment", "https://example.com/trust/trustlist.json#frag", queryReason},
		{"fragment 也以 .json 结尾", "https://example.com/trustlist.json#a.json", queryReason},
		// 只写一个问号：RawQuery 是空的，但地址确实带着 query。
		{"空 query", "https://example.com/trustlist.json?", queryReason},
		// 只写一个井号：Fragment 是空串，前两条判据都看不见它，而路径判据看的是
		// 路径。少了第三条判据，推导就会从倒数第 5 个字节下刀，砍掉 `json#` 拼出
		// `…/trustlist..sig` 这样一个谁也不是的地址。
		{"空 fragment", "https://example.com/trustlist.json#", trailingReason},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := sigURL(tt.in)
			if err == nil {
				t.Fatalf("sigURL(%q) 被接受了，推导出 %q", tt.in, got)
			}
			if got != "" {
				t.Errorf("sigURL(%q) 出错时还返回了 %q", tt.in, got)
			}
			if !strings.Contains(err.Error(), tt.reason) {
				t.Errorf("sigURL(%q) 的拒绝理由不是 %q: %v", tt.in, tt.reason, err)
			}
		})
	}
}

// TestSigURLKeepsCredentialsOutOfItsErrors：拒绝理由里不得出现 query 与 fragment
// 的内容，也不得出现 userinfo 里的口令。
//
// 凭据最常出现在 query 里（私有仓库的 raw 链接就是 `?token=…`），而 url.URL 的
// Redacted 只挡 userinfo 里的口令、不挡 query——所以错误里提的必须是**去掉 query
// 与 fragment 之后**的那份地址。这条规则光写在注释里守不住：把 bare.Redacted()
// 换回 u.Redacted()（或者顺手改成打印原始的 listURL）不会让任何别的用例变红，
// 而 `?token=…` 就此进了日志。三条拒绝路径各测一次，因为它们各自格式化一次地址。
func TestSigURLKeepsCredentialsOutOfItsErrors(t *testing.T) {
	t.Parallel()

	const (
		querySecret    = "QUERYSECRET"
		fragmentSecret = "FRAGMENTSECRET"
		passwordSecret = "PASSWORDSECRET"
	)
	tests := []struct {
		name    string
		in      string
		secrets []string
	}{
		// 后缀分支：路径是 /x，不以 .json 结尾。
		{"后缀分支", "https://user:" + passwordSecret + "@example.com/x?t=" + querySecret +
			"#" + fragmentSecret, []string{querySecret, fragmentSecret, passwordSecret}},
		// query / fragment 分支：路径合法，带着 query 与 fragment。
		{"query 分支", "https://user:" + passwordSecret + "@example.com/trustlist.json?t=" + querySecret +
			"#" + fragmentSecret, []string{querySecret, fragmentSecret, passwordSecret}},
		// 尾部字符分支：空 fragment，所以这里只剩 userinfo 的口令可漏。
		{"尾部字符分支", "https://user:" + passwordSecret + "@example.com/trustlist.json#",
			[]string{passwordSecret}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := sigURL(tt.in)
			if err == nil {
				t.Fatalf("sigURL(%q) 被接受了，推导出 %q", tt.in, got)
			}
			for _, secret := range tt.secrets {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("拒绝理由里出现了 %s：%v", secret, err)
				}
			}
		})
	}
}

func TestFetchRefusesPlainHTTP(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	// httptest.NewServer 是 http://。清单地址必须是 https，且这条与
	// plugins.allow_insecure_sources 无关——那个开关放宽的是插件产物的取回，
	// 而产物有 digest 兜底，清单没有。
	_, err := fetchBytes(context.Background(), srv.Client(), srv.URL, maxListBytes)
	if err == nil {
		t.Fatal("http:// 的清单地址被接受了")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("错误没说清是 scheme 的问题：%v", err)
	}
}

func TestFetchReportsTheStatusCode(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := fetchBytes(context.Background(), srv.Client(), srv.URL, maxListBytes)
	if err == nil {
		t.Fatal("404 被当成了清单内容")
	}
	// GitHub 对带无效凭据的请求返回 404 而不是 401。不把状态码写进错误信息，
	// 会让鉴权问题伪装成「文件不存在」，把排查引向完全错误的方向。
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("错误里没有状态码：%v", err)
	}
}

func TestFetchRefusesAnOversizedBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 64<<10)
		for written := int64(0); written <= maxListBytes; written += int64(len(chunk)) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	if _, err := fetchBytes(context.Background(), srv.Client(), srv.URL, maxListBytes); err == nil {
		t.Fatal("超过上限的响应体被接受了")
	}
}

func TestFetchRefusesAnHTTPSToHTTPRedirect(t *testing.T) {
	t.Parallel()

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	defer plain.Close()

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer secure.Close()

	client := secure.Client()
	if _, err := fetchBytes(context.Background(), client, secure.URL, maxListBytes); err == nil {
		t.Fatal("从 https 降级到 http 的重定向被跟随了")
	}
}

// TestFetchStopsAfterMaxRedirects：重定向跳数上限必须由本包自己的 CheckRedirect
// 守着，不能靠 http.Client 自带的默认上限（那个默认值不是本包选的，也不是本包
// 能保证不变的）替本包兜底；而且守住的必须是**具体跟随了几跳**，不只是「存在
// 某个上限」。
//
// 两条链都始终停在 https 上，不会撞上「降级到 http」那条规则，专门只测跳数。
// 一条恰好用满允许的跳数（必须成功），一条多一跳（必须被拒），两条都数服务端
// 实际收到了几次请求。
//
// followedHops / requestsInChain 写成字面量而不是由 maxRedirects 算出来：算出来
// 的期望值会跟着常量一起变，改动 maxRedirects 时用例照样全绿，什么也守不住。
// 这两个数就是 fetch.go 常量注释里承诺的那两个——判据是 len(via) >= maxRedirects，
// 所以最多跟随 9 跳，整条链至多发出 10 次请求。
func TestFetchStopsAfterMaxRedirects(t *testing.T) {
	t.Parallel()

	const (
		followedHops    = 9  // 允许被跟随的重定向跳数
		requestsInChain = 10 // 原始请求 + followedHops，两种情形下都应停在这里
	)

	for _, tc := range []struct {
		name    string
		hops    int // 服务端准备好的重定向跳数
		wantErr bool
	}{
		{name: "恰好用满允许的跳数", hops: followedHops, wantErr: false},
		{name: "比允许的跳数多一跳", hops: followedHops + 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int64
			var srv *httptest.Server
			srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				n := 0
				if _, err := fmt.Sscanf(r.URL.Path, "/%d", &n); err != nil {
					t.Errorf("解析跳数失败: %v", err)
					return
				}
				if n >= tc.hops {
					_, _ = w.Write([]byte("{}"))
					return
				}
				http.Redirect(w, r, fmt.Sprintf("%s/%d", srv.URL, n+1), http.StatusFound)
			}))
			defer srv.Close()

			body, err := fetchBytes(context.Background(), srv.Client(), srv.URL+"/0", maxListBytes)
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("%d 跳的重定向链被跟随到底了", tc.hops)
			case tc.wantErr && !strings.Contains(err.Error(), "redirect"):
				// 「被拒了」还不够：将来某次编辑让别的规则误伤这条链，用例
				// 照样绿，而它本该守的规则已经没了。
				t.Fatalf("链被拒了，但错误说的不是跳数: %v", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("%d 跳（未超上限）的重定向链被拒了: %v", tc.hops, err)
			case !tc.wantErr && string(body) != "{}":
				t.Errorf("body = %q", body)
			}
			if got := requests.Load(); got != requestsInChain {
				t.Errorf("服务端收到 %d 次请求, want %d：实际跟随的跳数变了", got, requestsInChain)
			}
		})
	}
}

func TestFetchReturnsTheBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	body, err := fetchBytes(context.Background(), srv.Client(), srv.URL, maxListBytes)
	if err != nil {
		t.Fatalf("fetchBytes: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}
}

// TestFetchBodySizeBoundary：「恰好等于上限」必须被接受，「上限+1」必须被拒——
// 这正是 fetchBytes 多读一个字节所买到的唯一东西。把 > 手滑写成 >= 会让一份恰好
// 顶到上限的合法清单被永久拒绝，而症状（「body exceeds N bytes」对着一份正好 N
// 字节的文档）恰恰是最难相信自己眼睛的那种。
//
// 用一个很小的上限而不是 maxListBytes：分界与上限具体是多少无关，而按 1 MiB
// 写只能证明「太大会被拒」。
func TestFetchBodySizeBoundary(t *testing.T) {
	t.Parallel()

	const limit int64 = 100

	for _, tc := range []struct {
		name    string
		size    int64
		wantErr bool
	}{
		{name: "上限之下", size: limit - 1, wantErr: false},
		{name: "恰好等于上限", size: limit, wantErr: false},
		{name: "超出上限一个字节", size: limit + 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(make([]byte, tc.size))
			}))
			defer srv.Close()

			body, err := fetchBytes(context.Background(), srv.Client(), srv.URL, limit)
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("%d 字节的响应体（上限 %d）被接受了", tc.size, limit)
			case !tc.wantErr && err != nil:
				t.Fatalf("%d 字节的响应体（上限 %d）被拒了: %v", tc.size, limit, err)
			case !tc.wantErr && int64(len(body)) != tc.size:
				t.Errorf("body 长度 = %d, want %d", len(body), tc.size)
			}
		})
	}
}

// TestFetchRefusesANonPositiveMaxBytes：0 在本包不是「不限制」——本包没有无上限
// 的取回路径。（internal/plugin/fetch 那边 Limits.MaxBytes == 0 是合法值，语义是
// 「拒绝任何响应体」；两个包的 0 不是一个意思，各自守住自己那条。）
func TestFetchRefusesANonPositiveMaxBytes(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	for _, maxBytes := range []int64{0, -1} {
		_, err := fetchBytes(context.Background(), srv.Client(), srv.URL, maxBytes)
		if err == nil {
			t.Errorf("maxBytes=%d 被接受了", maxBytes)
			continue
		}
		// 「返回了 error」还不够。去掉这道前置校验，请求照样会发出去，然后被
		// 体积判据拒掉，报的是「body exceeds 0 bytes」——一个越界的调用参数
		// 伪装成了「对端发来的文档太大」，而这正是本函数从头到尾在防的那件事。
		if !strings.Contains(err.Error(), "must be positive") {
			t.Errorf("maxBytes=%d 的错误说的不是这个参数本身: %v", maxBytes, err)
		}
	}
}

// TestFetchRefusesANonPositiveTimeout：0 在这里不是「不限时」。把超时抽成参数
// 之后，非正的 timeout 会让 WithTimeout 造出一个一出生就过期的 context，请求照
// 样发出去，报回来的是 context deadline exceeded——一个越界的调用参数伪装成
// 「对端太慢」，与 maxBytes<=0 那条是同一类错误，守卫也必须对称。
func TestFetchRefusesANonPositiveTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	for _, timeout := range []time.Duration{0, -time.Second} {
		_, err := fetchBytesWithTimeout(context.Background(), srv.Client(), srv.URL, maxListBytes, timeout)
		if err == nil {
			t.Errorf("timeout=%s 被接受了", timeout)
			continue
		}
		if !strings.Contains(err.Error(), "must be positive") {
			t.Errorf("timeout=%s 的错误说的不是这个参数本身: %v", timeout, err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("timeout=%s 的参数错误伪装成了取回超时: %v", timeout, err)
		}
	}
}

// TestFetchPanicsOnANilClient：nil client 是编程错误，允许 panic；但铁律同时
// 要求错误点可定位，裸的 nil 解引用给不出这个。与 internal/plugin/fetch.Fetch
// 的做法一致。
func TestFetchPanicsOnANilClient(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("client 为 nil 时没有 panic")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "client is nil") {
			t.Fatalf("panic 的信息定位不到错误点: %v", r)
		}
	}()

	_, _ = fetchBytes(context.Background(), nil, "https://example.invalid/trustlist.json", maxListBytes)
}

// deadlineProbe 是一个不发起任何连接的 RoundTripper：它只记下请求 context 上的
// deadline，然后回一个固定的 200。用它是因为超时这条规则在服务端一侧看不见。
type deadlineProbe struct {
	deadline    time.Time
	hasDeadline bool
}

func (p *deadlineProbe) RoundTrip(req *http.Request) (*http.Response, error) {
	p.deadline, p.hasDeadline = req.Context().Deadline()
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("{}")),
		Request:    req,
	}, nil
}

// TestFetchAppliesTheDefaultTimeout：fetchBytes 必须给每一次取回装上 fetchTimeout，
// 而且装的就是那个数。
//
// 这条规则被删掉之后不会有任何请求失败：慢速响应只会一直挂着，挂到调用方的 ctx
// 到期为止——而调用方传 context.Background() 就是永久挂起。所以这里不等超时发生，
// 直接观察请求 context 上的 deadline。
//
// 期望值写成字面量而不是 fetchTimeout：拿常量自己去比常量，改动它时用例会跟着
// 一起变，守不住「就是这个数」。
func TestFetchAppliesTheDefaultTimeout(t *testing.T) {
	t.Parallel()

	const wantTimeout = 30 * time.Second

	probe := &deadlineProbe{}
	before := time.Now()
	body, err := fetchBytes(context.Background(), &http.Client{Transport: probe},
		"https://example.invalid/trustlist.json", maxListBytes)
	after := time.Now()
	if err != nil {
		t.Fatalf("fetchBytes: %v", err)
	}
	if string(body) != "{}" {
		t.Fatalf("body = %q", body)
	}
	if !probe.hasDeadline {
		t.Fatal("请求 context 上没有 deadline：这次取回没有任何时间上限")
	}
	if lo, hi := before.Add(wantTimeout), after.Add(wantTimeout); probe.deadline.Before(lo) || probe.deadline.After(hi) {
		t.Errorf("deadline 在 %v 之后，want ≈ %v", probe.deadline.Sub(before), wantTimeout)
	}
}

// TestFetchStopsASlowResponse：超时到了必须返回错误，且错误链里认得出是超时。
// 体积上限拦不住慢速攻击——一个每秒滴一个字节、总量永不超限的响应体永远不会
// 触发 io.LimitReader，超时是这条路径上唯一能终止它的东西。
//
// 用例走 fetchBytesWithTimeout 传一个极短的超时，而不是真等 fetchTimeout。
func TestFetchStopsASlowResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	_, err := fetchBytesWithTimeout(context.Background(), srv.Client(), srv.URL, maxListBytes,
		50*time.Millisecond)
	if err == nil {
		t.Fatal("超过超时的响应被接受了")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误链里认不出是超时: %v", err)
	}
}

// TestFetchHonorsCallerCancellation：本函数自己的超时不能盖掉调用方的取消——
// 调用方取消时必须立刻返回，且错误链里认得出是取消而不是超时。
func TestFetchHonorsCallerCancellation(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := fetchBytes(ctx, srv.Client(), srv.URL, maxListBytes)
	if err == nil {
		t.Fatal("调用方取消之后仍然返回了内容")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误链里认不出是调用方取消: %v", err)
	}
}

// TestFetchDoesNotMutateTheCallersClient：本函数只在自己的那份副本上装重定向
// 策略，不动调用方给的 client（它可能被多处并发共用）。顺带钉住另一半：调用方
// 自己的 CheckRedirect 会被取代、一次都不会跑——本包的重定向规则只能收紧，不
// 接受调用方放宽。
func TestFetchDoesNotMutateTheCallersClient(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			_, _ = w.Write([]byte("{}"))
			return
		}
		http.Redirect(w, r, srv.URL+"/final", http.StatusFound)
	}))
	defer srv.Close()

	callerPolicyRan := false
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		callerPolicyRan = true
		return nil
	}
	want := reflect.ValueOf(client.CheckRedirect).Pointer()

	body, err := fetchBytes(context.Background(), client, srv.URL, maxListBytes)
	if err != nil {
		t.Fatalf("fetchBytes: %v", err)
	}
	if string(body) != "{}" {
		t.Errorf("body = %q", body)
	}

	if client.CheckRedirect == nil {
		t.Fatal("调用方 client 的 CheckRedirect 被清掉了")
	}
	if got := reflect.ValueOf(client.CheckRedirect).Pointer(); got != want {
		t.Error("调用方 client 的 CheckRedirect 被换掉了：本函数应当只改自己的那份副本")
	}
	if callerPolicyRan {
		t.Error("调用方自己的重定向策略被执行了：本包的策略必须取代它")
	}
}
