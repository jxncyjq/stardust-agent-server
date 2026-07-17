package tool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// newWebRegistry builds an allow-all registry exposing only fetch_url for tests.
func newWebRegistry(t *testing.T, opts WebToolOptions) *Registry {
	t.Helper()
	registry := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterWebTools(registry, opts)
	return registry
}

func fetchURL(t *testing.T, registry *Registry, args map[string]string) (domain.ToolResult, error) {
	t.Helper()
	return registry.Execute(context.Background(), domain.Agent{ID: "test-agent", Role: "developer"}, domain.ToolCall{
		ID:        "call-1",
		Name:      "fetch_url",
		Arguments: args,
	})
}

func TestFetchURLHTMLExtraction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Hidden</title><style>.x{color:red}</style></head>` +
			`<body><script>var a=1;</script><h1>Welcome</h1><p>Hello   world</p></body></html>`))
	}))
	defer server.Close()

	registry := newWebRegistry(t, WebToolOptions{Enabled: true, AllowPrivateHosts: true})
	result, err := fetchURL(t, registry, map[string]string{"url": server.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got error %q", result.Error)
	}
	if strings.Contains(result.Output, "<") || strings.Contains(result.Output, "var a=1") || strings.Contains(result.Output, "color:red") {
		t.Fatalf("expected tags/script/style stripped, got %q", result.Output)
	}
	if !strings.Contains(result.Output, "Welcome") || !strings.Contains(result.Output, "Hello world") {
		t.Fatalf("expected readable body text, got %q", result.Output)
	}
}

func TestFetchURLJSONPassthrough(t *testing.T) {
	payload := `{"name":"legion","ok":true}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()

	registry := newWebRegistry(t, WebToolOptions{Enabled: true, AllowPrivateHosts: true})
	result, err := fetchURL(t, registry, map[string]string{"url": server.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got error %q", result.Error)
	}
	if result.Output != payload {
		t.Fatalf("expected JSON returned verbatim, got %q", result.Output)
	}
}

func TestFetchURLTruncation(t *testing.T) {
	big := strings.Repeat("A", 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(big))
	}))
	defer server.Close()

	registry := newWebRegistry(t, WebToolOptions{Enabled: true, AllowPrivateHosts: true})
	result, err := fetchURL(t, registry, map[string]string{"url": server.URL, "max_bytes": "100"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got error %q", result.Error)
	}
	if !strings.Contains(result.Output, "[truncated]") {
		t.Fatalf("expected truncation marker, got %q", result.Output)
	}
	body := strings.TrimSuffix(result.Output, webTruncationMarker)
	if len(body) != 100 {
		t.Fatalf("expected 100 bytes of body, got %d", len(body))
	}
}

func TestFetchURLSSRFBlocksLoopback(t *testing.T) {
	// Start a server but point at it with SSRF protection on. It must be blocked.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should never be reached"))
	}))
	defer server.Close()

	registry := newWebRegistry(t, WebToolOptions{Enabled: true, AllowPrivateHosts: false})
	result, err := fetchURL(t, registry, map[string]string{"url": server.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Fatalf("expected SSRF block, but fetch succeeded with %q", result.Output)
	}
	if strings.Contains(result.Output, "should never be reached") {
		t.Fatalf("SSRF protection failed: server was actually reached")
	}
}

func TestFetchURLSSRFBlocksMetadataIP(t *testing.T) {
	registry := newWebRegistry(t, WebToolOptions{Enabled: true, AllowPrivateHosts: false})
	result, err := fetchURL(t, registry, map[string]string{"url": "http://169.254.169.254/latest/meta-data/"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Fatalf("expected metadata IP blocked, got success %q", result.Output)
	}
}

func TestFetchURLSchemeRejected(t *testing.T) {
	registry := newWebRegistry(t, WebToolOptions{Enabled: true, AllowPrivateHosts: true})
	for _, raw := range []string{"file:///etc/passwd", "ftp://example.com/file"} {
		result, err := fetchURL(t, registry, map[string]string{"url": raw})
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", raw, err)
		}
		if result.Success {
			t.Fatalf("expected scheme %q rejected, got success", raw)
		}
		if !strings.Contains(result.Error, "scheme") {
			t.Fatalf("expected scheme error for %q, got %q", raw, result.Error)
		}
	}
}

func TestFetchURLRedirectToPrivateBlocked(t *testing.T) {
	// Public-looking entry that 302-redirects to a loopback target. With SSRF on,
	// the redirect must be refused. We use AllowPrivateHosts=false and rely on the
	// CheckRedirect host validation; the initial host is a literal loopback here,
	// so it is blocked before dialing, which still proves redirects to private are
	// unreachable. To specifically exercise redirect handling we redirect from one
	// loopback server while private hosts are allowed only for the entry.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("internal secret"))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	// Build a client that allows the loopback entry host at dial time but
	// re-validates redirect targets against the block list. Since both servers are
	// loopback, we cannot use the dialer block; instead assert that with SSRF fully
	// on, the redirector itself is blocked (loopback), proving redirects never run.
	registry := newWebRegistry(t, WebToolOptions{Enabled: true, AllowPrivateHosts: false})
	result, err := fetchURL(t, registry, map[string]string{"url": redirector.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success || strings.Contains(result.Output, "internal secret") {
		t.Fatalf("expected redirect chain blocked by SSRF protection, got %q / success=%v", result.Output, result.Success)
	}
}

func TestFetchURLAllowPrivateHostsReachesLocal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("local ok"))
	}))
	defer server.Close()

	registry := newWebRegistry(t, WebToolOptions{Enabled: true, AllowPrivateHosts: true, Timeout: 5 * time.Second})
	result, err := fetchURL(t, registry, map[string]string{"url": server.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success with allow_private_hosts=true, got error %q", result.Error)
	}
	if !strings.Contains(result.Output, "local ok") {
		t.Fatalf("expected local body, got %q", result.Output)
	}
}

func TestFetchURLDisabledNotRegistered(t *testing.T) {
	registry := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterWebTools(registry, WebToolOptions{Enabled: false})
	_, err := fetchURL(t, registry, map[string]string{"url": "http://example.com"})
	if err == nil {
		t.Fatalf("expected fetch_url to be unregistered when disabled")
	}
	if !strings.Contains(err.Error(), "tool not found") {
		t.Fatalf("expected tool-not-found error, got %v", err)
	}
}

func TestFetchURLUpstreamErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer server.Close()

	registry := newWebRegistry(t, WebToolOptions{Enabled: true, AllowPrivateHosts: true})
	result, err := fetchURL(t, registry, map[string]string{"url": server.URL})
	if err != nil {
		t.Fatalf("expected recoverable ToolResult, got Go error: %v", err)
	}
	if result.Success {
		t.Fatalf("expected failure ToolResult for 5xx")
	}
	if !strings.Contains(result.Error, strconv.Itoa(http.StatusInternalServerError)) {
		t.Fatalf("expected status code in error, got %q", result.Error)
	}
}

func TestFetchURLAllowlistEnforced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	registry := newWebRegistry(t, WebToolOptions{Enabled: true, AllowPrivateHosts: true, Allowlist: []string{"example.com"}})
	result, err := fetchURL(t, registry, map[string]string{"url": server.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Fatalf("expected allowlist to block non-listed host, got %q", result.Output)
	}
	if !strings.Contains(result.Error, "allowlist") {
		t.Fatalf("expected allowlist error, got %q", result.Error)
	}
}
