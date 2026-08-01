package tool

import (
	"context"
	"encoding/json"
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

func TestWebExtractTruncatesAndCaches(t *testing.T) {
	big := strings.Repeat("X", 20000) // 远超默认 3000 预算
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(big))
	}))
	defer server.Close()

	toolRoot := t.TempDir()
	registry := tool_NewFileReadWriteWorkspaceRegistryForTest(t, toolRoot)
	RegisterWebTools(registry, WebToolOptions{Enabled: true, AllowPrivateHosts: true, ExtractCharLimit: 3000}, toolRoot)

	res, err := registry.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"}, domain.ToolCall{
		ID: "c1", Name: "web_extract", Arguments: map[string]string{"urls": server.URL},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Output, "[TRUNCATED]") {
		t.Fatalf("expected truncation footer, got %q", res.Output[:minInt(400, len(res.Output))])
	}
	if !strings.Contains(res.Output, "read_file") || !strings.Contains(res.Output, ".stardust/web_cache") {
		t.Fatalf("footer missing read_file hint or cache path: %q", res.Output)
	}

	rel := extractCachePathFromFooter(t, res.Output)
	rf, err := registry.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"}, domain.ToolCall{
		ID: "c2", Name: "read_file", Arguments: map[string]string{"path": rel},
	})
	if err != nil {
		t.Fatalf("read_file error: %v", err)
	}
	if !rf.Success || !strings.Contains(rf.Output, "X") {
		t.Fatalf("read_file could not read cached full text: success=%v out=%q", rf.Success, rf.Output[:minInt(200, len(rf.Output))])
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func tool_NewFileReadWriteWorkspaceRegistryForTest(t *testing.T, root string) *Registry {
	t.Helper()
	return NewFileReadWriteWorkspaceRegistry(root, nil)
}

func extractCachePathFromFooter(t *testing.T, out string) string {
	t.Helper()
	// out is the JSON tool output; the footer lives inside results[0].content
	// where its %q double-quotes are JSON-escaped. Decode first, then apply the
	// read_file path marker to the raw (unescaped) footer text.
	var payload struct {
		Results []extractResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("unmarshal web_extract output: %v", err)
	}
	if len(payload.Results) == 0 {
		t.Fatalf("no results in web_extract output: %q", out)
	}
	content := payload.Results[0].Content
	const marker = `read_file path="`
	i := strings.Index(content, marker)
	if i < 0 {
		t.Fatalf("footer has no read_file path marker: %q", content)
	}
	rest := content[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("footer read_file path not quoted: %q", rest)
	}
	return rest[:j]
}
