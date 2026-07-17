package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"golang.org/x/net/html"
)

const (
	defaultWebTimeout      = 20 * time.Second
	defaultWebMaxBytes     = 512 * 1024
	webMaxRedirects        = 5
	webDefaultUserAgent    = "legion-agent/1.0 (+fetch_url)"
	webTruncationMarker    = "\n\n[truncated]"
	webResolveDialerErrTpl = "fetch_url: blocked private/loopback address %s (SSRF protection; set web.allow_private_hosts=true to allow)"
)

// errBlockedAddress signals that an address was rejected by SSRF protection.
var errBlockedAddress = errors.New("blocked address")

// WebToolOptions configures the fetch_url tool. The tool package owns this
// struct so it stays independent of the config package; callers map their own
// configuration onto it.
type WebToolOptions struct {
	// Enabled gates registration. When false, RegisterWebTools is a no-op.
	Enabled bool
	// AllowPrivateHosts disables SSRF protection when true, permitting
	// loopback, private, and link-local destinations. Defaults to false.
	AllowPrivateHosts bool
	// Timeout bounds a single fetch. Defaults to defaultWebTimeout.
	Timeout time.Duration
	// MaxBytes caps the response body read into memory. Defaults to
	// defaultWebMaxBytes.
	MaxBytes int64
	// Allowlist, when non-empty, restricts fetches to these domains or their
	// subdomains. Empty means any public domain is allowed.
	Allowlist []string
}

func (o WebToolOptions) normalized() WebToolOptions {
	if o.Timeout <= 0 {
		o.Timeout = defaultWebTimeout
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = defaultWebMaxBytes
	}
	allowlist := make([]string, 0, len(o.Allowlist))
	for _, domain := range o.Allowlist {
		trimmed := strings.ToLower(strings.TrimSpace(domain))
		if trimmed != "" {
			allowlist = append(allowlist, trimmed)
		}
	}
	o.Allowlist = allowlist
	return o
}

// RegisterWebTools registers the fetch_url tool on registry when opts.Enabled is
// true. It is a no-op when registry is nil or the tool is disabled.
func RegisterWebTools(registry *Registry, opts WebToolOptions) {
	if registry == nil || !opts.Enabled {
		return
	}
	opts = opts.normalized()
	client := newSSRFGuardedClient(opts)
	registry.RegisterDescriptor(fetchURLDescriptor(opts.Timeout), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return handleFetchURL(ctx, client, opts, call)
	}))
}

func fetchURLDescriptor(timeout time.Duration) Descriptor {
	return Descriptor{
		Name:        "fetch_url",
		Description: "Fetch a public http/https URL with GET and return its content. HTML is extracted to readable text by default; JSON and plain text are returned as-is. Private/internal addresses are blocked by SSRF protection.",
		RiskLevel:   "medium",
		Timeout:     timeout,
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"url"},
			"properties": map[string]any{
				"url":       map[string]any{"type": "string", "description": "Absolute http or https URL to fetch."},
				"max_bytes": map[string]any{"type": "string", "description": "Optional response size cap in bytes; defaults to the configured limit."},
				"raw":       map[string]any{"type": "string", "description": "When true, return the raw response body. When false or omitted, HTML is extracted to readable text."},
			},
		},
	}
}

func handleFetchURL(ctx context.Context, client *http.Client, opts WebToolOptions, call domain.ToolCall) (domain.ToolResult, error) {
	rawURL := strings.TrimSpace(call.Arguments["url"])
	if rawURL == "" {
		return webFailure(call.ID, "url is required"), nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return webFailure(call.ID, fmt.Sprintf("parse url: %v", err)), nil
	}
	if scheme := strings.ToLower(parsed.Scheme); scheme != "http" && scheme != "https" {
		return webFailure(call.ID, fmt.Sprintf("unsupported scheme %q: only http and https are allowed", parsed.Scheme)), nil
	}
	if !opts.AllowPrivateHosts {
		if err := checkURLHostAllowed(parsed); err != nil {
			return webFailure(call.ID, err.Error()), nil
		}
	}
	if len(opts.Allowlist) > 0 && !hostInAllowlist(parsed.Hostname(), opts.Allowlist) {
		return webFailure(call.ID, fmt.Sprintf("host %q is not in the configured allowlist", parsed.Hostname())), nil
	}

	maxBytes := opts.MaxBytes
	if value := strings.TrimSpace(call.Arguments["max_bytes"]); value != "" {
		parsedMax, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsedMax <= 0 {
			return webFailure(call.ID, fmt.Sprintf("invalid max_bytes %q: must be a positive integer", value)), nil
		}
		maxBytes = parsedMax
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return webFailure(call.ID, fmt.Sprintf("build request: %v", err)), nil
	}
	req.Header.Set("User-Agent", webDefaultUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json,text/plain;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return webFailure(call.ID, fmt.Sprintf("fetch %s: %v", parsed.Redacted(), err)), nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, truncated, err := readLimited(resp.Body, maxBytes)
	if err != nil {
		return webFailure(call.ID, fmt.Sprintf("read response body: %v", err)), nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return webFailure(call.ID, fmt.Sprintf("upstream returned status %d for %s", resp.StatusCode, parsed.Redacted())), nil
	}

	output := renderResponse(resp.Header.Get("Content-Type"), body, parseWebBool(call.Arguments["raw"]))
	if truncated {
		output += webTruncationMarker
	}
	return domain.ToolResult{CallID: call.ID, Success: true, Output: output}, nil
}

func renderResponse(contentType string, body []byte, raw bool) string {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if !raw && (mediaType == "text/html" || mediaType == "application/xhtml+xml") {
		return extractReadableText(body)
	}
	return string(body)
}

func readLimited(reader io.Reader, maxBytes int64) ([]byte, bool, error) {
	// Read one extra byte to detect truncation.
	limited := io.LimitReader(reader, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > maxBytes {
		return data[:maxBytes], true, nil
	}
	return data, false, nil
}

// extractReadableText strips script/style content and tags from HTML, returning
// collapsed plain text using the x/net/html tokenizer.
func extractReadableText(body []byte) string {
	tokenizer := html.NewTokenizer(strings.NewReader(string(body)))
	var b strings.Builder
	skipDepth := 0
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return collapseWhitespace(b.String())
		case html.StartTagToken:
			name, _ := tokenizer.TagName()
			if isSkippableTag(string(name)) {
				skipDepth++
			}
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			if isSkippableTag(string(name)) && skipDepth > 0 {
				skipDepth--
			}
		case html.TextToken:
			if skipDepth == 0 {
				b.WriteString(string(tokenizer.Text()))
				b.WriteByte(' ')
			}
		}
	}
}

func isSkippableTag(name string) bool {
	switch strings.ToLower(name) {
	case "script", "style", "noscript", "head", "template", "svg":
		return true
	default:
		return false
	}
}

var whitespaceRun = regexp.MustCompile(`\s+`)

func collapseWhitespace(text string) string {
	return strings.TrimSpace(whitespaceRun.ReplaceAllString(text, " "))
}

// newSSRFGuardedClient builds an http.Client whose dialer rejects private and
// loopback IPs at dial time (defending against DNS rebinding) unless
// AllowPrivateHosts is set. Redirects are capped and each redirect target host
// is re-validated.
func newSSRFGuardedClient(opts WebToolOptions) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	if !opts.AllowPrivateHosts {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			return controlBlockPrivate(address)
		}
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Timeout:   opts.Timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= webMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", webMaxRedirects)
			}
			if !opts.AllowPrivateHosts {
				if err := checkURLHostAllowed(req.URL); err != nil {
					return err
				}
			}
			if len(opts.Allowlist) > 0 && !hostInAllowlist(req.URL.Hostname(), opts.Allowlist) {
				return fmt.Errorf("redirect host %q is not in the configured allowlist", req.URL.Hostname())
			}
			return nil
		},
	}
}

// controlBlockPrivate inspects the resolved dial address (ip:port) and rejects
// private/loopback/link-local IPs.
func controlBlockPrivate(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: cannot parse dial address %q", errBlockedAddress, address)
	}
	if isBlockedAddr(addr) {
		return fmt.Errorf(webResolveDialerErrTpl, addr.String())
	}
	return nil
}

// checkURLHostAllowed resolves the URL host and rejects it if any resolved IP is
// private/loopback/link-local. This is an explicit pre-dial check; the dialer
// Control callback remains the authoritative defense against DNS rebinding.
func checkURLHostAllowed(target *url.URL) error {
	host := target.Hostname()
	if host == "" {
		return fmt.Errorf("fetch_url: missing host in url")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if isBlockedAddr(addr) {
			return fmt.Errorf(webResolveDialerErrTpl, addr.String())
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("fetch_url: resolve host %q: %w", host, err)
	}
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		if isBlockedAddr(addr.Unmap()) {
			return fmt.Errorf(webResolveDialerErrTpl, host+" -> "+addr.Unmap().String())
		}
	}
	return nil
}

// isBlockedAddr reports whether addr falls in a range disallowed by SSRF
// protection: loopback, private, link-local, unspecified, multicast, and the
// IPv4-mapped equivalents.
func isBlockedAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return true
	}
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() || addr.IsInterfaceLocalMulticast() {
		return true
	}
	// fc00::/7 unique-local IPv6 is covered by IsPrivate in Go 1.17+, but guard
	// explicitly for clarity and forward-compat.
	if addr.Is6() {
		first := addr.As16()[0]
		if first&0xfe == 0xfc {
			return true
		}
	}
	return false
}

func hostInAllowlist(host string, allowlist []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, allowed := range allowlist {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

func parseWebBool(value string) bool {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "1", "true", "yes", "y":
		return true
	default:
		return false
	}
}

func webFailure(callID, reason string) domain.ToolResult {
	return domain.ToolResult{CallID: callID, Success: false, Error: reason}
}
