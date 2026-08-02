package tool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

func newExtractRegistry(t *testing.T, toolRoot string) *Registry {
	t.Helper()
	registry := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterWebTools(registry, WebToolOptions{Enabled: true, AllowPrivateHosts: true}, toolRoot)
	return registry
}

func webExtract(t *testing.T, registry *Registry, args map[string]string) (domain.ToolResult, error) {
	t.Helper()
	return registry.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"}, domain.ToolCall{
		ID: "c1", Name: "web_extract", Arguments: args,
	})
}

func TestWebExtractInlineSmallPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Title</h1><p>short body</p></body></html>"))
	}))
	defer server.Close()

	registry := newExtractRegistry(t, t.TempDir())
	res, err := webExtract(t, registry, map[string]string{"urls": server.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got %q", res.Error)
	}
	if !strings.Contains(res.Output, "short body") {
		t.Errorf("expected body text inline, got %q", res.Output)
	}
}

func TestWebExtractOrderAndMultiple(t *testing.T) {
	mk := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(body))
		}))
	}
	s1, s2 := mk("first-body"), mk("second-body")
	defer s1.Close()
	defer s2.Close()

	registry := newExtractRegistry(t, t.TempDir())
	res, err := webExtract(t, registry, map[string]string{"urls": s1.URL + "," + s2.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	i1 := strings.Index(res.Output, "first-body")
	i2 := strings.Index(res.Output, "second-body")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Errorf("expected first-body before second-body, output=%q", res.Output)
	}
}

func TestWebExtractRejectsMoreThanFive(t *testing.T) {
	registry := newExtractRegistry(t, t.TempDir())
	urls := strings.Join([]string{"http://a.example", "http://b.example", "http://c.example", "http://d.example", "http://e.example", "http://f.example"}, ",")
	res, err := webExtract(t, registry, map[string]string{"urls": urls})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Output, "dropped") && !strings.Contains(res.Error, "dropped") {
		t.Errorf("expected a report that URLs beyond 5 were dropped, got output=%q err=%q", res.Output, res.Error)
	}
}

func TestWebExtractMissingURLsFails(t *testing.T) {
	registry := newExtractRegistry(t, t.TempDir())
	_, err := webExtract(t, registry, map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing required urls")
	}
}

func TestWebExtractReturnsFullContentNoToolLevelTruncation(t *testing.T) {
	big := strings.Repeat("Z", 20000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(big))
	}))
	defer server.Close()

	registry := newExtractRegistry(t, t.TempDir())
	res, err := webExtract(t, registry, map[string]string{"urls": server.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res.Output, "TRUNCATED") || strings.Contains(res.Output, "tool_results") {
		t.Fatalf("web_extract must not truncate/cache at tool level anymore: %q", res.Output[:min(300, len(res.Output))])
	}
	if !strings.Contains(res.Output, strings.Repeat("Z", 20000)) {
		t.Fatalf("full 20000-char content should pass through")
	}
}

func TestWebExtractStripsBase64Images(t *testing.T) {
	html := `<html><body><p>before</p>` +
		`![shot](data:image/png;base64,AAAABBBBCCCCDDDD)` +
		`<p>after</p></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	}))
	defer server.Close()

	registry := newExtractRegistry(t, t.TempDir())
	res, err := webExtract(t, registry, map[string]string{"urls": server.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res.Output, "base64,AAAABBBB") {
		t.Errorf("raw base64 blob leaked into output: %q", res.Output)
	}
	if !strings.Contains(res.Output, "[IMAGE") {
		t.Errorf("expected [IMAGE...] placeholder, got %q", res.Output)
	}
}

func TestWebExtractBlocksSecretInURL(t *testing.T) {
	registry := newExtractRegistry(t, t.TempDir())
	res, err := webExtract(t, registry, map[string]string{"urls": "https://evil.example/?k=sk-ABCDEFGHIJKLMNOPQRSTUVWX"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Output, "secret") && !strings.Contains(res.Output, "key") {
		t.Errorf("expected secret-in-URL block, got %q", res.Output)
	}
}
