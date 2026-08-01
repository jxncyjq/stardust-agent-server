package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

const webExtractMaxURLs = 5

// extractResult is one page's result returned to the model.
type extractResult struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}

// registerWebExtractTool registers web_extract. client is the SSRF-guarded HTTP
// client shared with fetch_url; toolRoot is the sandbox root under which full
// page text is cached (Task 6).
func registerWebExtractTool(registry *Registry, opts WebToolOptions, client *http.Client, toolRoot string) {
	registry.RegisterDescriptor(webExtractDescriptor(opts.ExtractCharLimit, opts.Timeout),
		HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			return handleWebExtract(ctx, client, opts, toolRoot, call)
		}))
}

func webExtractDescriptor(charLimit int, timeout time.Duration) Descriptor {
	return Descriptor{
		Name:        "web_extract",
		Description: fmt.Sprintf("Fetch and extract readable text from web page URLs (max %d, comma-separated). Pages within ~%d chars return whole; larger pages are head+tail truncated with the full text saved to a workspace file and a footer telling you the read_file call to page through the rest.", webExtractMaxURLs, charLimit),
		RiskLevel:   "medium",
		Timeout:     timeout,
		Group:       "web",
		Sensitive:   true,
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"urls"},
			"properties": map[string]any{
				"urls":       map[string]any{"type": "string", "description": fmt.Sprintf("Comma-separated URLs (or a JSON array string), max %d.", webExtractMaxURLs)},
				"char_limit": map[string]any{"type": "string", "description": fmt.Sprintf("Optional per-page inline char budget (default %d).", charLimit)},
			},
		},
	}
}

// parseExtractURLs accepts either a JSON array string (["a","b"]) or a
// comma-separated list. It trims, drops empties, de-dupes preserving order, and
// reports how many were dropped for exceeding webExtractMaxURLs.
func parseExtractURLs(raw string) (urls []string, dropped int, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, 0, fmt.Errorf("urls is required")
	}
	var candidates []string
	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &candidates); err != nil {
			return nil, 0, fmt.Errorf("parse urls JSON array: %w", err)
		}
	} else {
		candidates = strings.Split(raw, ",")
	}
	seen := make(map[string]bool)
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		urls = append(urls, c)
	}
	if len(urls) > webExtractMaxURLs {
		dropped = len(urls) - webExtractMaxURLs
		urls = urls[:webExtractMaxURLs]
	}
	return urls, dropped, nil
}

func handleWebExtract(ctx context.Context, client *http.Client, opts WebToolOptions, toolRoot string, call domain.ToolCall) (domain.ToolResult, error) {
	urls, dropped, err := parseExtractURLs(call.Arguments["urls"])
	if err != nil {
		return webFailure(call.ID, err.Error()), nil
	}
	charLimit := opts.ExtractCharLimit
	if raw := strings.TrimSpace(call.Arguments["char_limit"]); raw != "" {
		v, convErr := strconv.Atoi(raw)
		if convErr != nil {
			return webFailure(call.ID, fmt.Sprintf("char_limit must be an integer, got %q", raw)), nil
		}
		if v >= 500 && v <= 3500 {
			charLimit = v
		}
	}

	results := make([]extractResult, 0, len(urls))
	for _, rawURL := range urls {
		results = append(results, extractOne(ctx, client, opts, toolRoot, charLimit, rawURL))
	}

	payload := map[string]any{"results": results}
	if dropped > 0 {
		payload["notice"] = fmt.Sprintf("%d URL(s) beyond the %d-URL limit were dropped", dropped, webExtractMaxURLs)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("marshal web_extract results: %w", err)
	}
	return domain.ToolResult{CallID: call.ID, Success: true, Output: string(encoded)}, nil
}

// extractOne fetches and renders a single URL. Fetch/SSRF failures are recorded
// on the result's Error field (per-URL failure is not a whole-call failure).
// Task 6 adds truncation+cache; this version returns rendered text inline.
func extractOne(ctx context.Context, client *http.Client, opts WebToolOptions, toolRoot string, charLimit int, rawURL string) extractResult {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return extractResult{URL: rawURL, Error: fmt.Sprintf("parse url: %v", err)}
	}
	if scheme := strings.ToLower(parsed.Scheme); scheme != "http" && scheme != "https" {
		return extractResult{URL: rawURL, Error: fmt.Sprintf("unsupported scheme %q", parsed.Scheme)}
	}
	if !opts.AllowPrivateHosts {
		if err := checkURLHostAllowed(parsed); err != nil {
			return extractResult{URL: rawURL, Error: err.Error()}
		}
	}
	text, _, err := fetchPage(ctx, client, parsed.String(), opts.MaxBytes, false)
	if err != nil {
		return extractResult{URL: rawURL, Error: fmt.Sprintf("fetch %s: %v", parsed.Redacted(), err)}
	}
	// Task 6 inserts base64 stripping + truncate/cache here.
	return extractResult{URL: rawURL, Content: text}
}
