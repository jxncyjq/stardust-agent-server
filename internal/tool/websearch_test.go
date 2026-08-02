package tool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestSearxngProviderParsesResults(t *testing.T) {
	var gotQuery, gotEngines string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		gotEngines = r.URL.Query().Get("engines")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"A","url":"https://a.example","content":"desc a"},
			{"title":"B","url":"https://b.example","content":"desc b"}
		]}`))
	}))
	defer server.Close()

	p := &searxngProvider{
		baseURL:       server.URL,
		defaultEngine: "baidu",
		client:        &http.Client{Timeout: 5 * time.Second},
	}
	results, err := p.Search(context.Background(), "hello", 5, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotQuery != "hello" {
		t.Errorf("q = %q, want hello", gotQuery)
	}
	if gotEngines != "baidu" {
		t.Errorf("engines = %q, want baidu (defaultEngine used when arg empty)", gotEngines)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Title != "A" || results[0].URL != "https://a.example" || results[0].Description != "desc a" || results[0].Position != 1 {
		t.Errorf("results[0] = %+v", results[0])
	}
	if results[1].Position != 2 {
		t.Errorf("results[1].Position = %d, want 2", results[1].Position)
	}
}

func TestSearxngProviderLimitAndEngineOverride(t *testing.T) {
	var gotEngines string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEngines = r.URL.Query().Get("engines")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"1","url":"https://1.example","content":"d"},
			{"title":"2","url":"https://2.example","content":"d"},
			{"title":"3","url":"https://3.example","content":"d"}
		]}`))
	}))
	defer server.Close()

	p := &searxngProvider{baseURL: server.URL, defaultEngine: "baidu", client: &http.Client{Timeout: 5 * time.Second}}
	results, err := p.Search(context.Background(), "x", 2, "google")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotEngines != "google" {
		t.Errorf("engines = %q, want google (arg overrides default)", gotEngines)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2 (limit applied)", len(results))
	}
}

func TestSearxngProviderNonJSONFailsLoud(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>403 forbidden</html>"))
	}))
	defer server.Close()

	p := &searxngProvider{baseURL: server.URL, client: &http.Client{Timeout: 5 * time.Second}}
	_, err := p.Search(context.Background(), "x", 5, "")
	if err == nil {
		t.Fatal("expected error on non-JSON body, got nil")
	}
	if !strings.Contains(err.Error(), "searxng") {
		t.Errorf("error should mention searxng, got %v", err)
	}
}

func TestSearxngProviderHTTPErrorFailsLoud(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	p := &searxngProvider{baseURL: server.URL, client: &http.Client{Timeout: 5 * time.Second}}
	if _, err := p.Search(context.Background(), "x", 5, ""); err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
}

func TestWebSearchHandler(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://t.example","content":"C"}]}`))
	}))
	defer server.Close()

	registry := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterWebTools(registry, WebToolOptions{Enabled: true, SearxngURL: server.URL})

	res, err := registry.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"}, domain.ToolCall{
		ID: "c1", Name: "web_search", Arguments: map[string]string{"query": "hello"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
	if !strings.Contains(res.Output, "t.example") || !strings.Contains(res.Output, "\"position\"") {
		t.Errorf("output missing expected fields: %q", res.Output)
	}
}

func TestWebSearchNotRegisteredWithoutURL(t *testing.T) {
	registry := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterWebTools(registry, WebToolOptions{Enabled: true})
	for _, d := range registry.Descriptors() {
		if d.Name == "web_search" {
			t.Fatal("web_search must not register when SearxngURL is empty")
		}
	}
}

func TestWebSearchReachesPrivateSearxng(t *testing.T) {
	// httptest 监听 127.0.0.1（私网/回环）。若 web_search 误用了 SSRF-guarded
	// client，这里会失败——正是要防的回归。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"ok","url":"https://ok.example","content":"c"}]}`))
	}))
	defer server.Close()

	registry := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	// 注意 AllowPrivateHosts=false（默认），仍应能连自建 SearXNG。
	RegisterWebTools(registry, WebToolOptions{Enabled: true, SearxngURL: server.URL})
	res, err := registry.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"}, domain.ToolCall{
		ID: "c1", Name: "web_search", Arguments: map[string]string{"query": "x"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("web_search must reach a private SearXNG instance, got error %q", res.Error)
	}
}

func TestWebSearchMissingQueryFails(t *testing.T) {
	registry := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterWebTools(registry, WebToolOptions{Enabled: true, SearxngURL: "http://127.0.0.1:1"})
	_, err := registry.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"}, domain.ToolCall{
		ID: "c1", Name: "web_search", Arguments: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for missing required query")
	}
}
