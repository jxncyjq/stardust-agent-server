package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// SearchResult is one web search hit returned to the model.
type SearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Position    int    `json:"position"`
}

// SearchProvider abstracts a web search backend. Search returns at most limit
// results. A backend that is unreachable or returns an unexpected payload
// returns an error — the caller converts it into a tool failure result.
type SearchProvider interface {
	Search(ctx context.Context, query string, limit int, engine string) ([]SearchResult, error)
}

// searxngProvider queries a SearXNG instance's JSON API. The instance URL comes
// from operator config (trusted), so the client here is a plain timeout client
// with no SSRF dialer guard — self-hosted SearXNG commonly lives on a private
// address that the fetch_url guard would otherwise block.
type searxngProvider struct {
	baseURL       string
	defaultEngine string
	client        *http.Client
}

// searxngResponse mirrors the subset of SearXNG's JSON we consume.
type searxngResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func (p *searxngProvider) Search(ctx context.Context, query string, limit int, engine string) ([]SearchResult, error) {
	if strings.TrimSpace(p.baseURL) == "" {
		return nil, fmt.Errorf("searxng: base URL is empty")
	}
	endpoint, err := url.Parse(strings.TrimRight(p.baseURL, "/") + "/search")
	if err != nil {
		return nil, fmt.Errorf("searxng: parse base URL %q: %w", p.baseURL, err)
	}
	q := endpoint.Query()
	q.Set("q", query)
	q.Set("format", "json")
	eng := strings.TrimSpace(engine)
	if eng == "" {
		eng = strings.TrimSpace(p.defaultEngine)
	}
	if eng != "" {
		q.Set("engines", eng)
	}
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("searxng: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searxng: request %s: %w", endpoint.Redacted(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("searxng: unexpected status %d from %s", resp.StatusCode, endpoint.Redacted())
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("searxng: read body: %w", err)
	}
	var parsed searxngResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("searxng: decode JSON (is the JSON format enabled on the instance?): %w", err)
	}

	if limit <= 0 {
		limit = 5
	}
	out := make([]SearchResult, 0, len(parsed.Results))
	for i, r := range parsed.Results {
		if len(out) >= limit {
			break
		}
		out = append(out, SearchResult{
			Title:       r.Title,
			URL:         r.URL,
			Description: r.Content,
			Position:    i + 1,
		})
	}
	return out, nil
}

func webSearchDescriptor(defaultLimit int, timeout time.Duration) Descriptor {
	return Descriptor{
		Name:        "web_search",
		Description: fmt.Sprintf("Search the web via the configured SearXNG instance. Returns up to %d results (title, url, description). Supports SearXNG-passthrough operators like site:example.com and filetype:pdf.", defaultLimit),
		RiskLevel:   "medium",
		Timeout:     timeout,
		Group:       "web",
		Sensitive:   true, // outbound network access
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"query"},
			"properties": map[string]any{
				"query":  map[string]any{"type": "string", "description": "Search query. May include SearXNG operators (site:, filetype:, intitle:, -term)."},
				"limit":  map[string]any{"type": "string", "description": fmt.Sprintf("Optional max results (1-20, default %d).", defaultLimit)},
				"engine": map[string]any{"type": "string", "description": "Optional engine: baidu, google, bing, or empty for the instance default. Overrides web.search_engine."},
			},
		},
	}
}

// registerWebSearchTool registers web_search only when a SearXNG URL is set.
func registerWebSearchTool(registry *Registry, opts WebToolOptions) {
	if strings.TrimSpace(opts.SearxngURL) == "" {
		return
	}
	provider := &searxngProvider{
		baseURL:       opts.SearxngURL,
		defaultEngine: opts.SearchEngine,
		client:        &http.Client{Timeout: opts.SearchTimeout},
	}
	registry.RegisterDescriptor(webSearchDescriptor(opts.SearchDefaultLimit, opts.SearchTimeout),
		HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			return handleWebSearch(ctx, provider, opts, call)
		}))
}

func handleWebSearch(ctx context.Context, provider SearchProvider, opts WebToolOptions, call domain.ToolCall) (domain.ToolResult, error) {
	query := strings.TrimSpace(call.Arguments["query"])
	if query == "" {
		return webFailure(call.ID, "query is required"), nil
	}
	limit, err := parseSearchLimit(call.Arguments["limit"], opts.SearchDefaultLimit)
	if err != nil {
		return webFailure(call.ID, err.Error()), nil
	}
	results, err := provider.Search(ctx, query, limit, call.Arguments["engine"])
	if err != nil {
		return webFailure(call.ID, fmt.Sprintf("web_search failed: %v", err)), nil
	}
	encoded, err := json.Marshal(map[string]any{"results": results})
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("marshal web_search results: %w", err)
	}
	return domain.ToolResult{CallID: call.ID, Success: true, Output: string(encoded)}, nil
}

// parseSearchLimit reads the optional string "limit" argument, clamping to
// [1,20]. An unparseable value is an error (fail-loud), not a silent default.
func parseSearchLimit(raw string, def int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("web_search limit must be an integer, got %q: %w", raw, err)
	}
	if v < 1 {
		v = 1
	}
	if v > 20 {
		v = 20
	}
	return v, nil
}
