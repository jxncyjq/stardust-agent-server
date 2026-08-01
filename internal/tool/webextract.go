package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
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
		// A non-integer is fail-loud above; an in-type out-of-range value is clamped
		// to [500,3500] (mirrors parseSearchLimit) rather than silently ignored.
		if v < 500 {
			v = 500
		}
		if v > 3500 {
			v = 3500
		}
		charLimit = v
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
	text = stripBase64Images(text)
	text, err = truncateAndCache(toolRoot, opts.ExtractCacheDir, parsed.String(), text, charLimit)
	if err != nil {
		// A sandbox-escape while caching is a security-relevant failure, not a
		// benign degradation: surface it on the per-URL failure channel (Error)
		// and do NOT return the truncated body as if the fetch succeeded.
		return extractResult{URL: rawURL, Error: err.Error()}
	}
	return extractResult{URL: rawURL, Content: text}
}

// webExtractCacheFileMax bounds a single cached full-text file so a giant page
// cannot write unbounded bytes to the workspace.
const webExtractCacheFileMax = 2_000_000

// truncateAndCache returns the model-facing text for one page. Within charLimit
// it returns content whole. Larger content is head(75%)+tail(25%) truncated,
// the full text written under toolRoot/<cacheDir>/<slug>-<hash>.md (validated by
// the workspace guard), and a footer appended pointing read_file at the file.
//
// Failure handling follows the fail-loud 铁律: a sandbox-escape write error
// (ErrPathOutsideWorkspace) is a security-relevant "本不该发生" condition and is
// returned as a Go error so the caller can surface it on the per-URL failure
// channel — never downgraded to a soft footer. Any other write error (disk full,
// read-only mount) MAY degrade gracefully to an in-band footer, but is recorded
// via the project logger at Warn so it is never silently swallowed.
func truncateAndCache(toolRoot, cacheDir, rawURL, content string, charLimit int) (string, error) {
	runes := []rune(content)
	if len(runes) <= charLimit {
		return content, nil
	}
	head := int(float64(charLimit) * 0.75)
	tail := charLimit - head
	model := string(runes[:head]) + "\n\n[... middle omitted — see footer ...]\n\n" + string(runes[len(runes)-tail:])

	relPath, writeErr := writeExtractCache(toolRoot, cacheDir, rawURL, content)
	if writeErr != nil && errors.Is(writeErr, port.ErrPathOutsideWorkspace) {
		return "", fmt.Errorf("web_extract cache path escaped workspace sandbox for %s: %w", rawURL, writeErr)
	}
	footer := "\n\n──────── [TRUNCATED] ────────\n" +
		fmt.Sprintf("Showing %d of %d chars.\n", head+tail, len(runes))
	if writeErr == nil {
		footer += fmt.Sprintf("Full text saved. To read the omitted middle: read_file path=%q offset=%d\n", relPath, head)
	} else {
		// Non-escape write failure (disk full / read-only mount). Graceful in-band
		// degradation is allowed, but the 铁律 forbids silently dropping the error.
		// We log via slog.Default() rather than threading a *slog.Logger from
		// RegisterWebTools through 4 internal signatures and ~13 call sites
		// (production + tests): that is genuinely invasive, and slog.Default() is
		// already this project's structured channel (see cli/command.go's
		// closeRepositoryLogging(slog.Default(), ...)).
		slog.Default().Warn("web_extract cache write failed; returning truncated body without cache file",
			"url", rawURL, "path", cacheDir, "error", writeErr)
		footer += "Full text could not be cached; re-run web_extract on a more specific URL or use fetch_url.\n"
	}
	return model + footer, nil
}

// writeExtractCache writes content to toolRoot/cacheDir/<slug>-<hash>.md, guarded
// to stay inside toolRoot, and returns the path RELATIVE to toolRoot (what
// read_file expects). Errors are returned, not swallowed.
func writeExtractCache(toolRoot, cacheDir, rawURL, content string) (string, error) {
	absRoot, err := filepath.Abs(toolRoot)
	if err != nil {
		return "", fmt.Errorf("resolve tool root %q: %w", toolRoot, err)
	}
	host := "page"
	if u, err := url.Parse(rawURL); err == nil && u.Hostname() != "" {
		host = strings.ReplaceAll(u.Hostname(), ":", "_")
	}
	slug := sanitizeSlug(host)
	sum := sha256.Sum256([]byte(rawURL))
	name := fmt.Sprintf("%s-%s.md", slug, hex.EncodeToString(sum[:])[:10])
	dir := filepath.Join(absRoot, cacheDir)
	target := filepath.Join(dir, name)

	// The guard must be authoritative over ALL filesystem side effects: check the
	// target BEFORE any mkdir, or a traversal cacheDir (e.g. "../../evil") would
	// have its directory created before the guard could reject it. Check resolves
	// the nearest existing ancestor for not-yet-existing targets, so checking
	// target here also covers dir (its parent). Only after the guard passes do we
	// create the directory and write the file.
	guard := port.NewWorkspacePathGuard(absRoot)
	if _, err := guard.Check(context.Background(), target); err != nil {
		return "", fmt.Errorf("cache path escapes workspace: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	if runes := []rune(content); len(runes) > webExtractCacheFileMax {
		content = string(runes[:webExtractCacheFileMax]) + "\n\n[... stored copy capped ...]"
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write cache file: %w", err)
	}
	rel, err := filepath.Rel(absRoot, target)
	if err != nil {
		return "", fmt.Errorf("relativize cache path: %w", err)
	}
	return filepath.ToSlash(rel), nil
}

func sanitizeSlug(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "page"
	}
	if r := []rune(out); len(r) > 60 {
		out = string(r[:60])
	}
	return out
}
