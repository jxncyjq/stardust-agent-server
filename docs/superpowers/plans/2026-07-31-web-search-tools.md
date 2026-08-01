# web_search / web_extract 工具实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 Legion agent 新增 `web_search`（查 SearXNG 自建实例）和 `web_extract`（批量抓正文、大页面落盘分页）两个工具。

**Architecture:** 均在 `internal/tool` 包内。抽取现有 `fetch_url` 的抓取逻辑为共享 `fetchPage`，`web_extract` 复用之；`web_search` 经新 `SearchProvider` 接口（首版仅 SearXNG 实现）。落盘落在工具沙箱 root（`toolRoot`）内的 `.stardust/web_cache`，由 `read_file` 分页读取。遵守 `legionAgent/CLAUDE.md` fail-loud 铁律。

**Tech Stack:** Go；`net/http` + `net/http/httptest`（测试）；`golang.org/x/net/html`（已用于 HTML 提取）；SearXNG JSON API。

**参考 spec:** `docs/superpowers/specs/2026-07-31-web-search-tools-design.md`

**全局收尾门槛（每次 commit 前本地跑）：** `go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空。命令在 `legion/legionAgent` 目录下执行。

---

## 文件结构

| 文件 | 动作 | 职责 |
|------|------|------|
| `internal/tool/web.go` | 修改 | 抽出 `fetchPage`；`RegisterWebTools` 签名加 `toolRoot`，改为同时注册 web_search/web_extract |
| `internal/tool/websearch.go` | 新建 | `SearchResult` / `SearchProvider` / `searxngProvider` / web_search descriptor+handler |
| `internal/tool/webextract.go` | 新建 | web_extract descriptor+handler、落盘分页、base64 占位、密钥阻断 |
| `internal/tool/websearch_test.go` | 新建 | searxngProvider 与 web_search handler 测试 |
| `internal/tool/webextract_test.go` | 新建 | web_extract handler 测试（含落盘 read_file 读回端到端） |
| `internal/config/config.go` | 修改 | `WebToolConfig` 加字段 + 默认值 |
| `internal/tool/web.go`（WebToolOptions） | 修改 | 加搜索/抽取字段 |
| `internal/cli/command.go` | 修改 | `webToolOptions` 映射、`RegisterWebTools` 调用（defaultTaskRunner） |
| `internal/runtime/agent_resolver.go` | 修改 | `webToolOptions` 映射、`RegisterWebTools` 调用 |
| `internal/toolauth/catalog.go` | 修改 | gateable 加 web_search / web_extract |
| `internal/tool/builtin.go` | 修改 | 三个 registry 构造器 AutoAllowTools + 权限加两工具 |
| `internal/tool/web_test.go` | 修改 | 更新 `RegisterWebTools` 调用签名 |
| `internal/runtime/toolauth_drift_test.go` | 修改 | 更新 `RegisterWebTools` 调用签名 |

---

## Task 1: 抽取共享抓取函数 `fetchPage`（fetch_url 行为不变）

把 `handleFetchURL` 中「构建请求 → Do → 读限长 → 渲染」的部分抽成可复用的 `fetchPage`，供后续 `web_extract` 复用。fetch_url 对外行为必须完全不变（现有 3 个测试是回归护栏）。

**Files:**
- Modify: `internal/tool/web.go`
- Test: `internal/tool/web_test.go`（现有测试作回归，不新增）

- [ ] **Step 1: 先跑现有 web 测试，确认基线全绿**

Run: `go test ./internal/tool/ -run TestFetchURL -v`
Expected: PASS（TestFetchURLHTMLExtraction / TestFetchURLJSONPassthrough / TestFetchURLTruncation 等）

- [ ] **Step 2: 在 `web.go` 新增 `fetchPage` 函数**

在 `renderResponse` 函数下方插入：

```go
// fetchPage GETs rawURL with client and returns the rendered body plus whether
// it was truncated at maxBytes. HTML is reduced to readable text unless raw is
// true; JSON/plain text are returned as-is. A non-2xx/3xx status, a transport
// error, or a body read error is returned as an error (the caller decides how
// to surface it). The caller is responsible for SSRF/allowlist pre-checks on
// rawURL before calling; client's dialer provides the dial-time defense.
func fetchPage(ctx context.Context, client *http.Client, rawURL string, maxBytes int64, raw bool) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", webDefaultUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json,text/plain;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, truncated, err := readLimited(resp.Body, maxBytes)
	if err != nil {
		return "", false, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return "", false, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}
	return renderResponse(resp.Header.Get("Content-Type"), body, raw), truncated, nil
}
```

- [ ] **Step 3: 让 `handleFetchURL` 改用 `fetchPage`**

将 `handleFetchURL` 中从 `req, err := http.NewRequestWithContext(...)`（约当前 web.go:134）到 `return domain.ToolResult{...Success: true...}, nil`（约 web.go:160）这一段，替换为：

```go
	output, truncated, err := fetchPage(ctx, client, parsed.String(), maxBytes, parseWebBool(call.Arguments["raw"]))
	if err != nil {
		return webFailure(call.ID, fmt.Sprintf("fetch %s: %v", parsed.Redacted(), err)), nil
	}
	if truncated {
		output += webTruncationMarker
	}
	return domain.ToolResult{CallID: call.ID, Success: true, Output: output}, nil
```

保留其前面的 URL 解析、scheme 校验、SSRF `checkURLHostAllowed`、allowlist、`max_bytes` 解析逻辑不动。

- [ ] **Step 4: 跑回归测试，确认 fetch_url 行为不变**

Run: `go test ./internal/tool/ -run TestFetchURL -v`
Expected: PASS（全部原有断言仍通过）

- [ ] **Step 5: 格式化 + 构建**

Run: `gofmt -w internal/tool/web.go && go build ./...`
Expected: 无输出、构建成功

- [ ] **Step 6: Commit**

```bash
git add internal/tool/web.go
git commit -m "refactor(tool): 抽出 fetchPage 供 web_extract 复用，fetch_url 行为不变"
```

---

## Task 2: 扩展配置字段（WebToolConfig + WebToolOptions）

新增搜索/抽取相关配置项与默认值，并把它们映射进 `WebToolOptions`。此时还没有工具消费这些字段，仅打通配置管道。

**Files:**
- Modify: `internal/config/config.go`（`WebToolConfig` 结构 + 默认值）
- Modify: `internal/tool/web.go`（`WebToolOptions` 结构 + `normalized()`）
- Modify: `internal/cli/command.go:2031`（`webToolOptions`）
- Modify: `internal/runtime/agent_resolver.go:272`（`webToolOptions`）
- Test: `internal/config/config_test.go`

- [ ] **Step 1: 写失败测试——断言默认配置带新字段默认值**

在 `internal/config/config_test.go` 追加：

```go
func TestDefaultConfigWebSearchFields(t *testing.T) {
	cfg := defaultConfig() // internal/config/config.go:238，包内 unexported 构造器
	if cfg.Web.SearchDefaultLimit != 5 {
		t.Errorf("SearchDefaultLimit = %d, want 5", cfg.Web.SearchDefaultLimit)
	}
	if cfg.Web.ExtractCharLimit != 3000 {
		t.Errorf("ExtractCharLimit = %d, want 3000", cfg.Web.ExtractCharLimit)
	}
	if cfg.Web.ExtractCacheDir != ".stardust/web_cache" {
		t.Errorf("ExtractCacheDir = %q, want .stardust/web_cache", cfg.Web.ExtractCacheDir)
	}
	if cfg.Web.SearchTimeoutSeconds != 15 {
		t.Errorf("SearchTimeoutSeconds = %d, want 15", cfg.Web.SearchTimeoutSeconds)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/config/ -run TestDefaultConfigWebSearchFields -v`
Expected: 编译失败（字段不存在）——这是预期的红。

- [ ] **Step 3: 给 `WebToolConfig` 加字段**

`internal/config/config.go` 的 `WebToolConfig`（约 config.go:48）改为：

```go
type WebToolConfig struct {
	Enabled           bool     `json:"enabled"`
	AllowPrivateHosts bool     `json:"allow_private_hosts"`
	TimeoutSeconds    int      `json:"timeout_seconds"`
	MaxResponseKB     int      `json:"max_response_kb"`
	Allowlist         []string `json:"allowlist"`

	// web_search (SearXNG) —— SearxngURL 为空则 web_search 不注册。
	SearxngURL           string `json:"searxng_url"`
	SearchEngine         string `json:"search_engine"`          // baidu|google|bing|空=实例默认
	SearchDefaultLimit   int    `json:"search_default_limit"`   // 默认 5，上限 20
	SearchTimeoutSeconds int    `json:"search_timeout_seconds"` // 默认 15

	// web_extract
	ExtractCharLimit int    `json:"extract_char_limit"` // 内联预算，clamp [500,3500]，默认 3000
	ExtractCacheDir  string `json:"extract_cache_dir"`  // 相对 toolRoot，默认 .stardust/web_cache
}
```

- [ ] **Step 4: 给默认配置补值**

`internal/config/config.go` 默认构造里的 `Web: WebToolConfig{...}`（约 config.go:314）补齐新字段：

```go
		Web: WebToolConfig{
			Enabled:              true,
			AllowPrivateHosts:    false,
			TimeoutSeconds:       20,
			MaxResponseKB:        512,
			Allowlist:            nil,
			SearxngURL:           "",
			SearchEngine:         "",
			SearchDefaultLimit:   5,
			SearchTimeoutSeconds: 15,
			ExtractCharLimit:     3000,
			ExtractCacheDir:      ".stardust/web_cache",
		},
```

> 保留该块里原有的 `MaxResponseKB` 等既有值（若默认块里已有则不要重复；只补新增 4~6 个字段）。

- [ ] **Step 5: 给 `WebToolOptions` 加字段（web.go）**

`internal/tool/web.go` 的 `WebToolOptions`（约 web.go:37）追加字段：

```go
	// SearxngURL 为空表示不注册 web_search。
	SearxngURL           string
	SearchEngine         string
	SearchDefaultLimit   int
	SearchTimeout        time.Duration
	ExtractCharLimit     int
	ExtractCacheDir      string
```

并在 `normalized()`（约 web.go:53）末尾（`return o` 前）补默认/clamp：

```go
	if o.SearchTimeout <= 0 {
		o.SearchTimeout = 15 * time.Second
	}
	if o.SearchDefaultLimit <= 0 || o.SearchDefaultLimit > 20 {
		o.SearchDefaultLimit = 5
	}
	if o.ExtractCharLimit < 500 {
		o.ExtractCharLimit = 3000
	}
	if o.ExtractCharLimit > 3500 {
		o.ExtractCharLimit = 3500
	}
	if strings.TrimSpace(o.ExtractCacheDir) == "" {
		o.ExtractCacheDir = ".stardust/web_cache"
	}
```

- [ ] **Step 6: 更新两处 `webToolOptions` 映射**

`internal/cli/command.go:2031` 与 `internal/runtime/agent_resolver.go:272` 的 `webToolOptions` 函数体，改为（两处完全相同）：

```go
func webToolOptions(cfg config.WebToolConfig) tool.WebToolOptions {
	return tool.WebToolOptions{
		Enabled:            cfg.Enabled,
		AllowPrivateHosts:  cfg.AllowPrivateHosts,
		Timeout:            time.Duration(cfg.TimeoutSeconds) * time.Second,
		MaxBytes:           int64(cfg.MaxResponseKB) * 1024,
		Allowlist:          cfg.Allowlist,
		SearxngURL:         cfg.SearxngURL,
		SearchEngine:       cfg.SearchEngine,
		SearchDefaultLimit: cfg.SearchDefaultLimit,
		SearchTimeout:      time.Duration(cfg.SearchTimeoutSeconds) * time.Second,
		ExtractCharLimit:   cfg.ExtractCharLimit,
		ExtractCacheDir:    cfg.ExtractCacheDir,
	}
}
```

- [ ] **Step 7: 跑测试确认通过 + 构建**

Run: `go test ./internal/config/ -run TestDefaultConfigWebSearchFields -v && go build ./...`
Expected: PASS + 构建成功

- [ ] **Step 8: Commit**

```bash
git add internal/config/config.go internal/tool/web.go internal/cli/command.go internal/runtime/agent_resolver.go
git commit -m "feat(config): 新增 web_search/web_extract 配置字段与默认值"
```

---

## Task 3: SearchProvider 接口 + searxngProvider

新建 `websearch.go`，定义搜索抽象和 SearXNG 实现。SearXNG 实例来自运维配置（可信来源），故其 HTTP client 不加 SSRF dialer 限制（允许内网自建实例）。

**Files:**
- Create: `internal/tool/websearch.go`
- Create: `internal/tool/websearch_test.go`

- [ ] **Step 1: 写失败测试——searxngProvider 解析 mock JSON**

创建 `internal/tool/websearch_test.go`：

```go
package tool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tool/ -run TestSearxng -v`
Expected: 编译失败（`searxngProvider` / `Search` 未定义）

- [ ] **Step 3: 实现 `websearch.go`**

创建 `internal/tool/websearch.go`：

```go
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
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tool/ -run TestSearxng -v`
Expected: PASS（全部 4 个）

- [ ] **Step 5: 格式化 + 构建**

Run: `gofmt -w internal/tool/websearch.go internal/tool/websearch_test.go && go build ./...`
Expected: 无输出、构建成功

- [ ] **Step 6: Commit**

```bash
git add internal/tool/websearch.go internal/tool/websearch_test.go
git commit -m "feat(tool): 新增 SearchProvider 接口与 searxngProvider"
```

---

## Task 4: web_search 工具（descriptor + handler + 注册 + 登记）

在 `RegisterWebTools` 里注册 web_search（仅当 `SearxngURL` 非空），登记 gateable、AutoAllowTools、权限。

**Files:**
- Modify: `internal/tool/websearch.go`（descriptor + handler + 注册函数）
- Modify: `internal/tool/web.go`（`RegisterWebTools` 调用注册函数）
- Modify: `internal/toolauth/catalog.go`
- Modify: `internal/tool/builtin.go`
- Test: `internal/tool/websearch_test.go`

- [ ] **Step 1: 写失败测试——web_search handler 端到端 + 未配不注册**

在 `internal/tool/websearch_test.go` 追加：

```go
func TestWebSearchHandler(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://t.example","content":"C"}]}`))
	}))
	defer server.Close()

	registry := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterWebTools(registry, WebToolOptions{Enabled: true, SearxngURL: server.URL}, t.TempDir())

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
	RegisterWebTools(registry, WebToolOptions{Enabled: true}, t.TempDir()) // 无 SearxngURL
	for _, d := range registry.Descriptors() {
		if d.Name == "web_search" {
			t.Fatal("web_search must not register when SearxngURL is empty")
		}
	}
}

func TestWebSearchMissingQueryFails(t *testing.T) {
	registry := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterWebTools(registry, WebToolOptions{Enabled: true, SearxngURL: "http://127.0.0.1:1"}, t.TempDir())
	_, err := registry.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"}, domain.ToolCall{
		ID: "c1", Name: "web_search", Arguments: map[string]string{},
	})
	// query 是 required —— schema 校验层直接返 Go error。
	if err == nil {
		t.Fatal("expected error for missing required query")
	}
}
```

> 注意此步依赖 Task 5 才引入的 `RegisterWebTools` 三参签名 `(registry, opts, toolRoot)`。本 Task 先把签名改成三参（toolRoot 此时仅 web_extract 用，web_search 不用但保持统一），见 Step 3。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tool/ -run TestWebSearch -v`
Expected: 编译失败（web_search 未注册 / 签名不符）

- [ ] **Step 3: 改 `RegisterWebTools` 签名为三参，并注册 web_search**

`internal/tool/web.go` 的 `RegisterWebTools`（约 web.go:73）改为：

```go
// RegisterWebTools registers fetch_url, and — when configured — web_search and
// web_extract, on registry. toolRoot is the tool sandbox root; web_extract
// writes full-page cache files under toolRoot so read_file can page them back.
// It is a no-op when registry is nil or opts.Enabled is false.
func RegisterWebTools(registry *Registry, opts WebToolOptions, toolRoot string) {
	if registry == nil || !opts.Enabled {
		return
	}
	opts = opts.normalized()
	client := newSSRFGuardedClient(opts)
	registry.RegisterDescriptor(fetchURLDescriptor(opts.Timeout), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return handleFetchURL(ctx, client, opts, call)
	}))
	registerWebSearchTool(registry, opts)
	registerWebExtractTool(registry, opts, client, toolRoot)
}
```

> `registerWebExtractTool` 在 Task 5 实现。若想让本 Task 独立编译通过，先在 `webextract.go` 里放一个空壳 `func registerWebExtractTool(*Registry, WebToolOptions, *http.Client, string) {}`，Task 5 再填实现。**本 Task 结束时创建该空壳文件。**

- [ ] **Step 4: 在 `websearch.go` 实现 descriptor + handler + 注册**

追加到 `internal/tool/websearch.go`：

```go
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
		// Serializing our own well-typed struct should never fail — a programming
		// error, so surface it as a Go error rather than a tool failure.
		return domain.ToolResult{}, fmt.Errorf("marshal web_search results: %w", err)
	}
	return domain.ToolResult{CallID: call.ID, Success: true, Output: string(encoded)}, nil
}
```

在 `websearch.go` 的 import 补 `"net/http"` 与 `"time"`（若尚未引入）。

- [ ] **Step 5: 登记 gateable**

`internal/toolauth/catalog.go` 的 `gateable` 列表加两行（保持字母序附近即可）：

```go
	{"web_extract", "抓取网页 URL 的正文内容"},
	{"web_search", "通过 SearXNG 搜索网页"},
```

> 一次性把 web_extract 也加上，避免 Task 5 再动这文件。

- [ ] **Step 6: 登记 AutoAllowTools + 权限（builtin.go）**

`internal/tool/builtin.go` 中**三个**构造器（`NewWorkspaceRegistry`、`NewFileReadOnlyWorkspaceRegistry`、`NewFileReadWriteWorkspaceRegistry`）里：

- 每个 `AutoAllowTools: []string{...}` 切片中 `"fetch_url"` 同行/附近加入 `"web_search", "web_extract"`。
- 每个 `NewBatchRolePermissionEnforcer(map[string]bool{...})` 中 `"developer:fetch_url": true` 下方加：

```go
			"developer:web_search":          true,
			"developer:web_extract":         true,
```

共 3 处 AutoAllowTools + 3 处权限 map。

- [ ] **Step 7: 更新旧的 `RegisterWebTools` 两参调用点**

改为三参（toolRoot）：
- `internal/tool/web_test.go:19`（`newWebRegistry`）：`RegisterWebTools(registry, opts, t.TempDir())`
- `internal/runtime/toolauth_drift_test.go:53`：`tool.RegisterWebTools(tools, tool.WebToolOptions{Enabled: true}, t.TempDir())`
- `internal/runtime/agent_resolver.go:244`：`tool.RegisterWebTools(tools, webToolOptions(r.rootConfig.Web), toolRoot)`（`toolRoot` 变量已在该函数上文，见 agent_resolver.go:238）
- `internal/cli/command.go:1994`（defaultTaskRunner.RunTask）：`tool.RegisterWebTools(tools, d.webOptions, root)`（`root` 变量已在该函数上文，见 command.go:1985）

- [ ] **Step 8: 跑测试 + drift-guard + 全量**

Run: `go test ./internal/tool/ -run TestWebSearch -v && go test ./internal/runtime/ -run TestEveryProductionToolIsGateable -v`
Expected: PASS（web_search 三测通过；drift-guard 因 gateable 已登记而通过）

- [ ] **Step 9: 格式化 + 构建 + 全量测试**

Run: `gofmt -l . && go build ./... && go test ./...`
Expected: gofmt 无输出、构建成功、测试全绿

- [ ] **Step 10: Commit**

```bash
git add internal/tool/websearch.go internal/tool/websearch_test.go internal/tool/web.go internal/tool/webextract.go internal/toolauth/catalog.go internal/tool/builtin.go internal/tool/web_test.go internal/runtime/toolauth_drift_test.go internal/runtime/agent_resolver.go internal/cli/command.go
git commit -m "feat(tool): 新增 web_search 工具（SearXNG）并登记 gateable/权限"
```

---

## Task 5: web_extract 骨架（批量抓取 + 内联，无落盘）

实现 `webextract.go`：解析 urls、逐个 SSRF+抓取、内联返回（预算内）。大页面落盘留到 Task 6。复用 Task 1 的 `fetchPage` 与现有 `newSSRFGuardedClient`（经 `client` 参数传入）。

**Files:**
- Modify/Create: `internal/tool/webextract.go`（替换 Task 4 建的空壳）
- Test: `internal/tool/webextract_test.go`

- [ ] **Step 1: 写失败测试——批量抓取、内联、SSRF、顺序、>5 丢弃**

创建 `internal/tool/webextract_test.go`：

```go
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
	_, err := webExtract(t, registry, map[string]string{}) // urls required
	if err == nil {
		t.Fatal("expected error for missing required urls")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tool/ -run TestWebExtract -v`
Expected: 编译/运行失败（handler 未实现，空壳不注册 web_extract）

- [ ] **Step 3: 实现 `webextract.go`（内联版，无落盘）**

用以下内容**替换** Task 4 建的空壳 `internal/tool/webextract.go`：

```go
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
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tool/ -run TestWebExtract -v`
Expected: PASS（4 个骨架测试）

- [ ] **Step 5: 格式化 + 构建 + 全量测试**

Run: `gofmt -w internal/tool/webextract.go internal/tool/webextract_test.go && go build ./... && go test ./...`
Expected: 无输出、构建成功、全绿

- [ ] **Step 6: Commit**

```bash
git add internal/tool/webextract.go internal/tool/webextract_test.go
git commit -m "feat(tool): web_extract 骨架——批量抓取内联返回"
```

---

## Task 6: web_extract 大页面落盘 + read_file 分页

超预算页面头+尾截断，全文落 `toolRoot/<ExtractCacheDir>/<slug>-<hash>.md`（过 `guard.Check`），footer 给出相对 toolRoot 的路径与 `read_file` 调用。核心断言：落盘文件能被同一 sandbox 的 `read_file` 读回。

**Files:**
- Modify: `internal/tool/webextract.go`
- Test: `internal/tool/webextract_test.go`

- [ ] **Step 1: 写失败测试——大页面截断 + 落盘 + read_file 读回**

在 `internal/tool/webextract_test.go` 追加：

```go
func TestWebExtractTruncatesAndCaches(t *testing.T) {
	big := strings.Repeat("X", 20000) // 远超默认 3000 预算
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(big))
	}))
	defer server.Close()

	toolRoot := t.TempDir()
	// 同一个 sandbox root，既跑 web_extract 也跑 read_file。
	registry := tool_NewFileReadWriteWorkspaceRegistryForTest(t, toolRoot)
	RegisterWebTools(registry, WebToolOptions{Enabled: true, AllowPrivateHosts: true, ExtractCharLimit: 3000}, toolRoot)

	res, err := registry.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"}, domain.ToolCall{
		ID: "c1", Name: "web_extract", Arguments: map[string]string{"urls": server.URL},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Output, "[TRUNCATED]") {
		t.Fatalf("expected truncation footer, got %q", res.Output[:min(400, len(res.Output))])
	}
	// footer 里应含相对路径与 read_file 提示。
	if !strings.Contains(res.Output, "read_file") || !strings.Contains(res.Output, ".stardust/web_cache") {
		t.Fatalf("footer missing read_file hint or cache path: %q", res.Output)
	}

	// 端到端：把 footer 里的相对路径喂给 read_file，必须读得回。
	rel := extractCachePathFromFooter(t, res.Output) // 见 Step 2 helper
	rf, err := registry.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"}, domain.ToolCall{
		ID: "c2", Name: "read_file", Arguments: map[string]string{"path": rel},
	})
	if err != nil {
		t.Fatalf("read_file error: %v", err)
	}
	if !rf.Success || !strings.Contains(rf.Output, "X") {
		t.Fatalf("read_file could not read cached full text: success=%v out=%q", rf.Success, rf.Output[:min(200, len(rf.Output))])
	}
}

func min(a, b int) int { if a < b { return a }; return b }
```

- [ ] **Step 2: 在测试文件补两个 helper**

在 `internal/tool/webextract_test.go` 追加：

```go
// tool_NewFileReadWriteWorkspaceRegistryForTest 复用生产构造器，保证 read_file
// 与 web_extract 共享同一 sandbox root。
func tool_NewFileReadWriteWorkspaceRegistryForTest(t *testing.T, root string) *Registry {
	t.Helper()
	return NewFileReadWriteWorkspaceRegistry(root, nil)
}

// extractCachePathFromFooter 从 footer 抽出 read_file path=... 的相对路径。
func extractCachePathFromFooter(t *testing.T, out string) string {
	t.Helper()
	const marker = `read_file path="`
	i := strings.Index(out, marker)
	if i < 0 {
		t.Fatalf("footer has no read_file path marker: %q", out)
	}
	rest := out[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("footer read_file path not quoted: %q", rest)
	}
	return rest[:j]
}
```

> `NewFileReadWriteWorkspaceRegistry` 已在同包 `builtin.go` 导出；`RegisterWebTools` 追加到它上面即可（生产构造器已把 web_extract 加进 AutoAllowTools/权限，Task 4 Step 6 完成）。

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/tool/ -run TestWebExtractTruncatesAndCaches -v`
Expected: FAIL（无 `[TRUNCATED]` footer，未落盘）

- [ ] **Step 4: 实现落盘 + 截断 + footer**

在 `internal/tool/webextract.go` 顶部 import 补 `"crypto/sha256"`、`"encoding/hex"`、`"os"`、`"path/filepath"`、`"github.com/stardust/legion-agent/internal/port"`。

新增常量与函数：

```go
// webExtractCacheFileMax bounds a single cached full-text file so a giant page
// cannot write unbounded bytes to the workspace.
const webExtractCacheFileMax = 2_000_000

// truncateAndCache returns the model-facing text for one page. Within charLimit
// it returns content whole. Larger content is head(75%)+tail(25%) truncated,
// the full text written under toolRoot/<cacheDir>/<slug>-<hash>.md (validated by
// the workspace guard), and a footer appended pointing read_file at the file.
// A cache write failure is logged-in-band (footer says so) but never fatal.
func truncateAndCache(toolRoot, cacheDir, rawURL, content string, charLimit int) string {
	runes := []rune(content)
	if len(runes) <= charLimit {
		return content
	}
	head := int(float64(charLimit) * 0.75)
	tail := charLimit - head
	model := string(runes[:head]) + "\n\n[... middle omitted — see footer ...]\n\n" + string(runes[len(runes)-tail:])

	relPath, writeErr := writeExtractCache(toolRoot, cacheDir, rawURL, content)
	footer := "\n\n──────── [TRUNCATED] ────────\n" +
		fmt.Sprintf("Showing %d of %d chars.\n", head+tail, len(runes))
	if writeErr == nil {
		footer += fmt.Sprintf("Full text saved. To read the omitted middle: read_file path=%q offset=%d\n", relPath, head)
	} else {
		footer += "Full text could not be cached; re-run web_extract on a more specific URL or use fetch_url.\n"
	}
	return model + footer
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

	guard := port.NewWorkspacePathGuard(absRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	if _, err := guard.Check(context.Background(), target); err != nil {
		return "", fmt.Errorf("cache path escapes workspace: %w", err)
	}
	if len(content) > webExtractCacheFileMax {
		content = content[:webExtractCacheFileMax] + "\n\n[... stored copy capped ...]"
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
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}
```

- [ ] **Step 5: 在 `extractOne` 接入截断落盘**

把 `extractOne` 结尾的

```go
	// Task 6 inserts base64 stripping + truncate/cache here.
	return extractResult{URL: rawURL, Content: text}
```

改为：

```go
	text = truncateAndCache(toolRoot, opts.ExtractCacheDir, parsed.String(), text, charLimit)
	return extractResult{URL: rawURL, Content: text}
```

- [ ] **Step 6: 跑测试确认通过（含 read_file 读回）**

Run: `go test ./internal/tool/ -run TestWebExtract -v`
Expected: PASS（含 TestWebExtractTruncatesAndCaches 的端到端 read_file 断言）

- [ ] **Step 7: 格式化 + 构建 + 全量**

Run: `gofmt -w internal/tool/webextract.go internal/tool/webextract_test.go && go build ./... && go test ./...`
Expected: 无输出、构建成功、全绿

- [ ] **Step 8: Commit**

```bash
git add internal/tool/webextract.go internal/tool/webextract_test.go
git commit -m "feat(tool): web_extract 大页面头尾截断落盘 + read_file 分页"
```

---

## Task 7: base64 图片占位 + URL 内嵌密钥阻断

移植 hermes 的两项处理：内联 base64 图片转 `[IMAGE: alt]`（省 token），URL 内嵌 API key 前缀则整体拒绝抓取（防泄漏）。

**Files:**
- Modify: `internal/tool/webextract.go`
- Test: `internal/tool/webextract_test.go`

- [ ] **Step 1: 写失败测试——base64 转占位、密钥 URL 阻断**

在 `internal/tool/webextract_test.go` 追加：

```go
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
	// 该 URL 那条结果必须带阻断错误，且不应发起抓取。
	if !strings.Contains(res.Output, "secret") && !strings.Contains(res.Output, "key") {
		t.Errorf("expected secret-in-URL block, got %q", res.Output)
	}
}
```

> HTML 提取器（`extractReadableText`）会把 `![shot](...)` 原样保留为文本（不是标签），所以 base64 正则能在渲染后文本上命中。密钥前缀集用最小集 `sk-`, `ghp_`, `xoxb-` 等常见前缀。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tool/ -run "TestWebExtractStripsBase64Images|TestWebExtractBlocksSecretInURL" -v`
Expected: FAIL

- [ ] **Step 3: 实现 base64 占位 + 密钥检测**

在 `internal/tool/webextract.go` import 补 `"regexp"`。新增：

```go
var (
	// mdBase64Image 匹配 markdown 图片里的 base64 源，保留 alt 文本。
	mdBase64Image = regexp.MustCompile(`!\[([^\]]*)\]\(\s*data:image/[^;]+;base64,[A-Za-z0-9+/=\s]+\)`)
	// bareBase64Image 匹配裸/括号内的 base64 图片数据。
	bareBase64Image = regexp.MustCompile(`\(?\s*data:image/[^;]+;base64,[A-Za-z0-9+/=]+\)?`)
	// secretInURL 匹配常见凭据前缀，命中则拒绝抓取该 URL（防止把 URL 里的密钥
	// 发给第三方页面 / 出现在日志）。前缀集刻意保守，避免误伤普通 URL。
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
```

- [ ] **Step 4: 在 `extractOne` 接入两项处理**

在 `extractOne` 里，scheme 校验之后、SSRF 检查之前，插入密钥阻断：

```go
	if urlHasEmbeddedSecret(rawURL) {
		return extractResult{URL: rawURL, Error: "blocked: URL appears to contain an API key or token; secrets must not be sent in URLs"}
	}
```

并把落盘前的一行

```go
	text = truncateAndCache(toolRoot, opts.ExtractCacheDir, parsed.String(), text, charLimit)
```

改为先转占位再截断：

```go
	text = stripBase64Images(text)
	text = truncateAndCache(toolRoot, opts.ExtractCacheDir, parsed.String(), text, charLimit)
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/tool/ -run "TestWebExtractStripsBase64Images|TestWebExtractBlocksSecretInURL" -v`
Expected: PASS

- [ ] **Step 6: 格式化 + 构建 + 全量测试 + vet + gofmt 检查**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./...`
Expected: gofmt 无输出、构建成功、vet 无告警、测试全绿

- [ ] **Step 7: Commit**

```bash
git add internal/tool/webextract.go internal/tool/webextract_test.go
git commit -m "feat(tool): web_extract 移植 base64 图片占位与 URL 密钥阻断"
```

---

## Task 8: 收尾——SearXNG 私网放行回归 + 文档

确认 SearXNG client 允许私网自建实例（httptest 默认监听 127.0.0.1，前面 web_search 测试已隐式覆盖：searxngProvider 用普通 client，不受 SSRF 阻断）。补一条显式断言并收尾。

**Files:**
- Test: `internal/tool/websearch_test.go`

- [ ] **Step 1: 写显式测试——SearXNG 命中 127.0.0.1 不被 SSRF 挡**

在 `internal/tool/websearch_test.go` 追加：

```go
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
	RegisterWebTools(registry, WebToolOptions{Enabled: true, SearxngURL: server.URL}, t.TempDir())
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
```

- [ ] **Step 2: 跑测试**

Run: `go test ./internal/tool/ -run TestWebSearchReachesPrivateSearxng -v`
Expected: PASS（若失败，说明 searxngProvider 误用了 SSRF client——回到 Task 4 Step 4 用普通 `&http.Client{Timeout:...}`）

- [ ] **Step 3: 全量门槛检查**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l .`
Expected: 构建成功、vet 无告警、测试全绿、gofmt 无输出

- [ ] **Step 4: Commit**

```bash
git add internal/tool/websearch_test.go
git commit -m "test(tool): 断言 web_search 可达私网自建 SearXNG"
```

---

## 自检结论（写计划者已核对）

- **Spec 覆盖**：双工具（web_search T3/T4、web_extract T5/T6/T7）、复用自建抓取（T1 fetchPage）、SearXNG engine 可配可覆盖（T3/T4）、落盘 sandbox 内 read_file 读回（T6 端到端断言）、inline 预算 clamp（T2 normalized）、base64 占位（T7）、密钥阻断（T7）、gateable/AutoAllow/权限登记（T4 Step5-7）、SearXNG 私网放行（T8）、fail-loud（各 handler 失败走 webFailure、编程错误走 Go error）。均有对应任务。
- **签名一致性**：`RegisterWebTools(registry, opts, toolRoot)` 全 4 调用点在 T4 Step7 统一；`SearchResult`/`SearchProvider`/`searxngProvider`/`extractResult`/`fetchPage`/`truncateAndCache`/`writeExtractCache`/`stripBase64Images` 命名前后一致。
- **实现锚点已确认**：默认配置构造器 = `defaultConfig()`（config.go:238）；`WebToolConfig` @48、默认块 @314；`webToolOptions` 两处 @command.go:2031 / @agent_resolver.go:272；`RegisterWebTools` 四调用点 @web.go:73(定义) / agent_resolver.go:244 / command.go:1994 / web_test.go:19 / drift_test.go:53；`toolRoot` 变量在 agent_resolver.go:238、`root` 在 command.go:1985。
