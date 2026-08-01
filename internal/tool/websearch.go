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
