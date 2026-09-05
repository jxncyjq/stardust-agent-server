package trustlist

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSigURLDerivesFromTheListURL(t *testing.T) {
	t.Parallel()

	got, err := sigURL("https://example.com/trust/trustlist.json")
	if err != nil {
		t.Fatalf("sigURL: %v", err)
	}
	if want := "https://example.com/trust/trustlist.sig"; got != want {
		t.Errorf("sigURL = %q, want %q", got, want)
	}
}

// TestSigURLRefusesAnUnexpectedSuffix：清单地址必须以 .json 结尾，否则推导出的
// 签名地址就是猜的。给两个可以各自配置的 URL 才是真正的危险（两份不匹配的
// 文档），但推导也必须是确定的，不能沉默地拼错。
func TestSigURLRefusesAnUnexpectedSuffix(t *testing.T) {
	t.Parallel()

	if _, err := sigURL("https://example.com/trust/trustlist"); err == nil {
		t.Fatal("不以 .json 结尾的清单地址被接受了")
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

// TestFetchStopsAfterMaxRedirects：重定向链条超过 maxRedirects 跳必须被拒绝，
// 不能靠 http.Client 自带的默认上限（那个默认值不是本包选的，也不是本包能
// 保证不变的）替本包兜底。用一条比 maxRedirects 长、但始终停在 https 上的
// 重定向链——它不会撞上「降级到 http」那条规则，专门只测跳数本身。
func TestFetchStopsAfterMaxRedirects(t *testing.T) {
	t.Parallel()

	const hops = maxRedirects + 5

	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := 0
		if _, err := fmt.Sscanf(r.URL.Path, "/%d", &n); err != nil {
			t.Errorf("解析跳数失败: %v", err)
			return
		}
		if n >= hops {
			_, _ = w.Write([]byte("{}"))
			return
		}
		http.Redirect(w, r, fmt.Sprintf("%s/%d", srv.URL, n+1), http.StatusFound)
	}))
	defer srv.Close()

	_, err := fetchBytes(context.Background(), srv.Client(), srv.URL+"/0", maxListBytes)
	if err == nil {
		t.Fatal("超过 maxRedirects 跳的重定向链被跟随到底了")
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
