package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

const webExtractMaxURLs = 5

var (
	// mdBase64Image 匹配 markdown 图片里的 base64 源，保留 alt 文本。
	mdBase64Image = regexp.MustCompile(`!\[([^\]]*)\]\(\s*data:image/[^;]+;base64,[A-Za-z0-9+/=\s]+\)`)
	// bareBase64Image 匹配裸/括号内的 base64 图片数据。
	bareBase64Image = regexp.MustCompile(`\(?\s*data:image/[^;]+;base64,[A-Za-z0-9+/=\s]+\)?`)
	// secretInURL 匹配常见凭据前缀，命中则拒绝抓取该 URL。前缀集刻意保守，避免误伤普通 URL。
	secretInURL = regexp.MustCompile(`(sk-[A-Za-z0-9]{16,}|ghp_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]{10,}|AKIA[A-Z0-9]{16})`)
)

// stripBase64Images replaces inline base64 image blobs with labeled placeholders
// so image bytes never reach the model, while keeping any alt text and real
// http(s) image links intact.
func stripBase64Images(text string) string {
	text = mdBase64Image.ReplaceAllStringFunc(text, func(m string) string {
		sub := mdBase64Image.FindStringSubmatch(m)
		alt := strings.TrimSpace(sub[1])
		if alt != "" {
			return "[IMAGE: " + alt + "]"
		}
		return "[IMAGE]"
	})
	return bareBase64Image.ReplaceAllString(text, "[IMAGE]")
}

// urlHasEmbeddedSecret reports whether rawURL (decoded) contains an API-key-like
// token. Such URLs are refused before any fetch.
func urlHasEmbeddedSecret(rawURL string) bool {
	if secretInURL.MatchString(rawURL) {
		return true
	}
	if decoded, err := url.QueryUnescape(rawURL); err == nil {
		return secretInURL.MatchString(decoded)
	}
	return false
}

// extractResult is one page's result returned to the model.
type extractResult struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}

// registerWebExtractTool registers web_extract. client is the SSRF-guarded HTTP
// client shared with fetch_url. Oversized results are handled uniformly by the
// runtime tool loop (renderToolResultContent), so web_extract no longer caches
// at the tool level.
func registerWebExtractTool(registry *Registry, opts WebToolOptions, client *http.Client) {
	registry.RegisterDescriptor(webExtractDescriptor(opts.Timeout),
		HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			return handleWebExtract(ctx, client, opts, call)
		}))
}

func webExtractDescriptor(timeout time.Duration) Descriptor {
	return Descriptor{
		Name:        "web_extract",
		Description: fmt.Sprintf("Fetch and extract readable text from web page URLs (max %d, comma-separated). Returns the clean page text; oversized results are handled by the runtime (preview + read_file to page the rest).", webExtractMaxURLs),
		RiskLevel:   "medium",
		Timeout:     timeout,
		Group:       "web",
		Sensitive:   true,
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"urls"},
			"properties": map[string]any{
				"urls": map[string]any{"type": "string", "description": fmt.Sprintf("Comma-separated URLs (or a JSON array string), max %d.", webExtractMaxURLs)},
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

func handleWebExtract(ctx context.Context, client *http.Client, opts WebToolOptions, call domain.ToolCall) (domain.ToolResult, error) {
	urls, dropped, err := parseExtractURLs(call.Arguments["urls"])
	if err != nil {
		return webFailure(call.ID, err.Error()), nil
	}

	results := make([]extractResult, 0, len(urls))
	for _, rawURL := range urls {
		results = append(results, extractOne(ctx, client, opts, rawURL))
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

// extractOne fetches and renders a single URL. Fetch/SSRF/secret failures are
// recorded on the result's Error field (per-URL failure is not a whole-call
// failure). Oversized results are handled uniformly by the runtime tool loop
// (renderToolResultContent), not here.
func extractOne(ctx context.Context, client *http.Client, opts WebToolOptions, rawURL string) extractResult {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return extractResult{URL: rawURL, Error: fmt.Sprintf("parse url: %v", err)}
	}
	if scheme := strings.ToLower(parsed.Scheme); scheme != "http" && scheme != "https" {
		return extractResult{URL: rawURL, Error: fmt.Sprintf("unsupported scheme %q", parsed.Scheme)}
	}
	if urlHasEmbeddedSecret(rawURL) {
		return extractResult{URL: rawURL, Error: "blocked: URL appears to contain an API key or token; secrets must not be sent in URLs"}
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
	return extractResult{URL: rawURL, Content: stripBase64Images(text)}
}
