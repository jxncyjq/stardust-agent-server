---
title: web_search 与 web_extract 工具设计
date: 2026-07-31
status: draft
tags: [tool, web-search, web-extract, searxng, ssrf]
related:
  - "[[2026-07-24-per-agent-tool-authorization-design]]"
---

# web_search 与 web_extract 工具设计

## 1. 背景与目标

Legion agent 目前有 `fetch_url`（抓单个 URL 正文）但**没有网络搜索能力**。参考 hermes-agent 的 `web_search` / `web_extract` 双工具，为 Legion 新增：

- **`web_search`** — 向 SearXNG 自建实例发起搜索，返回结果列表（标题/URL/摘要）。
- **`web_extract`** — 批量抓取 URL 正文，复用现有自建抓取层，大页面头+尾截断并落盘、由 `read_file` 分页读取。

`fetch_url` 保留不动。

**参考实现**：hermes-agent（Python），`tools/web_tools.py`、`agent/web_search_registry.py`、`website/docs/user-guide/features/web-search.md`。hermes 用插件式多 provider（firecrawl/searxng/tavily/exa/…）；Legion 首版**只做 SearXNG**，但保留 provider 接口以便日后扩展。

## 2. 范围

### 做
- 新增 `web_search` 工具：查 SearXNG JSON API，engine（baidu/google/bing/all）可配置、调用时可覆盖。
- 新增 `web_extract` 工具：批量抓正文，复用自建抓取层，字符预算内联 + 超预算落盘分页。
- 定义 `SearchProvider` 接口 + 一个 `searxngProvider` 实现。
- 抽取共享抓取层 `fetchPage`，`fetch_url` 与 `web_extract` 共用。
- 移植 hermes 的两项安全处理：URL 内嵌密钥阻断、内联 base64 图片转占位。
- 配置、gateable 登记、AutoAllowTools / 权限登记、测试。

### 不做（YAGNI）
- 不做第三方 extract API（Firecrawl/Tavily/Exa）——抓取一律走自建层。
- 不做 firecrawl/tavily/exa/brave/ddgs/xai 等其它 provider。
- 不做 Baidu/Google/Bing 官方 API 直连——它们作为 SearXNG 的 engine 参数暴露。
- 不改 runtime 的 `maxToolResultChars`（4000）硬截逻辑。

## 3. 架构与组件

新增/改动均在 `internal/tool` 包（与 `web.go`、`builtin.go` 同包）。

| 组件 | 位置 | 说明 |
|------|------|------|
| `SearchProvider` 接口 + `SearchResult` | 新文件 `internal/tool/websearch.go` | 搜索后端抽象，首版仅 SearXNG |
| `searxngProvider` | `internal/tool/websearch.go` | 查 `{url}/search?format=json`，解析 `results[]` |
| `web_search` descriptor + handler | `internal/tool/websearch.go` | 调 provider，返回 JSON 结果列表 |
| `web_extract` descriptor + handler | 新文件 `internal/tool/webextract.go` | 批量抓 + 截断落盘分页 |
| `fetchPage` 共享抓取函数 | 从 `web.go` 抽出 | SSRF client + fetch + HTML→text，`fetch_url` 与 `web_extract` 共用 |
| 落盘 + footer | `webextract.go` | 落 `toolRoot/.stardust/web_cache`，footer 提示 `read_file` |
| base64 图片转占位 | `webextract.go` | 移植 hermes `convert_base64_images_to_links` |
| 密钥阻断 | `webextract.go` | 移植 hermes 的密钥前缀检测（复用 Legion 现有 redact 设施，若有） |

**注册入口改动**：`RegisterWebTools` 当前签名 `(registry, opts)`，需扩展为携带 **toolRoot + guard**（落盘要落在 sandbox root 内、路径要过 `guard.Check`）。两个 caller（`agent_resolver.go:239` 的 per-agent 路径、`command.go` 的 `defaultTaskRunner.RunTask`）已持有 toolRoot，改为传入即可。

## 4. 数据结构与接口

```go
// internal/tool/websearch.go
type SearchResult struct {
    Title       string
    URL         string
    Description string
    Position    int
}

type SearchProvider interface {
    // Search 返回至多 limit 条结果。engine 为空表示用 provider 默认引擎集。
    // 后端不可达 / 返回非预期格式，返回 error（由 handler 转成工具失败结果）。
    Search(ctx context.Context, query string, limit int, engine string) ([]SearchResult, error)
}

type searxngProvider struct {
    baseURL       string
    defaultEngine string
    client        *http.Client // 复用 SSRF-guarded client（允许配置的 SearXNG 主机）
}
```

**SearXNG 请求**：`GET {baseURL}/search?q={query}&format=json&engines={engine}`（engine 为空则不带 engines 参数，用 SearXNG 实例默认）。响应 JSON 形如 `{"results":[{"title","url","content",...}]}`，映射 `content→Description`，`results` 下标 +1 作 `Position`，截断到 limit 条。

> **SearXNG 主机的 SSRF 例外**：SearXNG 自建实例通常在内网/localhost。`web_search` 的 client 必须允许访问配置的 `searxng_url` 主机（即使它是私网地址），否则自建实例被 SSRF 防护挡掉。实现方式：搜索 client 与 extract client 分开构造——搜索 client 对 `searxng_url` 主机放行；extract client 沿用 `fetch_url` 的严格 SSRF（除非 `allow_private_hosts`）。

## 5. 工具 schema

> **注意**：Legion 工具参数是 `call.Arguments map[string]string`，所有值按字符串传递，schema 的 `type` 也标 `"string"`（见现有 `fetch_url` 的 `max_bytes`/`raw`）。数字/枚举在 handler 内用 `strconv` 解析，**解析失败 fail-loud 不兜底**（参考 `builtin.go` 的 `intArg`）。

### web_search
```
Name: "web_search"
Description: "Search the web via the configured SearXNG instance. Returns up to N results with title, URL and description."
Group: "web"
Sensitive: true   // 出站网络
RiskLevel: "medium"
InputSchema:
  query   (string, required) — 搜索词，可含 site: / filetype: 等 SearXNG 支持的操作符
  limit   (string, optional) — 最大结果数，默认 web.search_default_limit(5)，上限 20
  engine  (string, optional) — baidu|google|bing|留空=实例默认；覆盖 web.search_engine
```
返回：JSON `{"results":[{"title","url","description","position"},...]}`。

### web_extract
```
Name: "web_extract"
Description: "Fetch and extract readable text from web page URLs (max 5). Pages within the char budget return whole; larger pages are head+tail truncated with the full text saved to a workspace file and a footer telling you the read_file call to page through the rest. Inline base64 images become [IMAGE: alt] placeholders."
Group: "web"
Sensitive: true
RiskLevel: "medium"
InputSchema:
  urls        (string, required) — 逗号分隔或 JSON 数组字符串的 URL 列表，最多 5 个
  char_limit  (string, optional) — 每页内联字符预算，默认 web.extract_char_limit(3000)，上限受 4000 runes 硬截约束
```
返回：JSON `{"results":[{"url","title","content","error"},...]}`，顺序与输入一致。

> `urls` 因 `map[string]string` 只能传字符串：约定**逗号分隔**为主，同时容忍 JSON 数组字符串（`["a","b"]`）。handler 解析后去重、上限 5 个，超出的**明确报告被丢弃**（不静默截断）。

## 6. 配置

扩展 `WebToolOptions`（`internal/tool/web.go`）与 config 的 `web:` 段（`internal/config/config.go`）：

```yaml
web:
  # 现有
  enabled: true
  allow_private_hosts: false
  timeout: 20s
  max_bytes: 524288
  allowlist: []
  # 新增
  searxng_url: "http://localhost:8888"   # 配了才注册 web_search；空则 web_search 不注册
  search_engine: ""                       # baidu|google|bing|空=实例默认
  search_default_limit: 5                  # 上限 20
  search_timeout: 15s
  extract_char_limit: 3000                 # 内联预算，clamp 到 [500, 3500]
  extract_cache_dir: ".stardust/web_cache" # 相对 toolRoot；落盘目录
```

**门控**：
- `web_search` 仅当 `searxng_url` 非空才注册（类比 `fetch_url` 的 `Enabled`）。
- `web_extract` 不依赖 SearXNG，随 web 工具启用即注册（依赖自建抓取层）。

## 7. 数据流

**web_search**：
1. 校验 `query` 非空 → 解析 `limit`（clamp [1,20]）、`engine`（默认取配置）。
2. `searxngProvider.Search` → `GET .../search?format=json` → 解析 `results[]` → 映射 `SearchResult`。
3. 序列化 JSON 返回 `ToolResult{Success:true,Output}`。

**web_extract**：
1. 解析 `urls`（逗号/JSON 数组）→ 去重、≤5、超出报告丢弃。
2. 逐 URL：
   a. 密钥检测（URL 内嵌 API key 前缀）→ 命中即整体阻断，返失败结果。
   b. SSRF 校验（`checkURLHostAllowed`，除非 `allow_private_hosts`）。
   c. `fetchPage` 抓取 → HTML→可读文本。
   d. base64 图片转 `[IMAGE: alt]` 占位。
   e. 字符预算：`len(runes) ≤ char_limit` 直接内联；否则头(75%)+尾(25%)截断 + 落盘全文到 `toolRoot/.stardust/web_cache/{slug}-{hash}.md`（过 `guard.Check`）+ footer 提示 `read_file(path=相对路径, offset=中段起始)`。
3. 组装 `{"results":[...]}`，顺序还原，返回。

## 8. 错误处理（fail-loud 铁律）

遵循 `legionAgent/CLAUDE.md` fail-loud：**禁止兜底/fallback**。区分两条通道：

- **工具执行失败**（外部世界不如意）→ `ToolResult{Success:false, Error:...}`，`error` 返 `nil`。属于工具正常失败通道，非违反 fail-loud。
  - SearXNG 不可达 / 非 2xx / body 非 JSON / JSON 无 `results` 字段 → 失败结果，Error 说明原因。
  - URL SSRF 阻断、密钥命中、scheme 非法、抓取超时 → 失败结果（沿用 `fetch_url` 的 `webFailure`）。
- **编程错误 / 不该发生**（schema 常量畸形、guard.Check 对已解析路径失败、序列化失败）→ 返 **Go error**，向上传播。
- **落盘失败**：best-effort，但**不静默**——用项目 logger `Warn` 记录（带 URL），footer 降级为"全文无法落盘，请重抓更窄 URL 或用 fetch_url"，仍返截断内容。
- **空结果非错误**：SearXNG 返 0 条 / 抓取到空正文 → `Success:true` + 明确"无结果"，不伪装成失败也不静默。

## 9. 落盘与分页机制（关键约束）

- **落盘目录 = `toolRoot/.stardust/web_cache`**，`toolRoot` 即注册时注入的 sandbox root：
  - 有 `task.WorkingDir` → toolRoot = working_dir。
  - 无 → toolRoot = `ContextFiles.Root`（**非 home**）。
  - 落盘路径必须过 `guard.Check`，确保在 sandbox 内 → `read_file` 能读回。
- **文件名**：`{host-slug}-{sha256(url)[:10]}.md`，同 URL 覆盖。
- **footer**：给出**相对 toolRoot 的路径** + `read_file(path, offset=中段起始行)`，因为 `read_file` 的 path 相对 workspace root 解析。
- **内联预算受 runtime 4000 runes 硬截约束**：`read_file` 单页上限 3500 runes、工具结果整体被 `maxToolResultChars`(4000) 从前截断。故 `extract_char_limit` clamp 到 `[500, 3500]`，默认 3000。**不照抄 hermes 的 15000**。

## 10. 安全

- **SSRF**：extract 沿用 `web.go` 的 dialer control + pre-dial 解析 + redirect 重校验；SearXNG client 单独构造、对配置的 `searxng_url` 主机放行。
- **URL 内嵌密钥阻断**：移植 hermes——URL（含 percent-decode 后）匹配 API key 前缀则整体拒绝。优先复用 Legion 现有 `quality`/redact 设施的密钥前缀正则（实现时确认是否存在，否则新增最小前缀集）。
- **base64 图片转占位**：移植 `convert_base64_images_to_links`——`![alt](data:image/...;base64,...)` → `[IMAGE: alt]`，裸 base64 → `[IMAGE]`，保留真实 http(s) 图片链接。
- 输出经 registry 现有 `OutputSanitizer` 过一遍（沿用 `WithOutputSanitizer`）。

## 11. 必做登记点（漏一个测试就红）

1. **`internal/toolauth/catalog.go`** gateable 列表加 `{"web_search", "..."}` + `{"web_extract", "..."}`——否则 `internal/runtime` 的 drift-guard test 失败。
2. **`internal/tool/builtin.go`** 三个 registry 构造器（`NewWorkspaceRegistry` / `NewFileReadOnlyWorkspaceRegistry` / `NewFileReadWriteWorkspaceRegistry`）的 `AutoAllowTools` 与 `BatchRolePermissionEnforcer` 加 `web_search` / `web_extract`（照 `fetch_url`）——否则默认不允许调用。
3. **`RegisterWebTools` 签名**加 toolRoot + guard，更新两处 caller（`agent_resolver.go`、`command.go` defaultTaskRunner）。
4. **config**：`internal/config/config.go` `web:` 段加新字段 + `webToolOptions` 映射。

## 12. 测试计划

`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空。

- **searxngProvider**：mock HTTP 返 JSON — 测解析、`engines` 参数拼接、limit 截断、空 `results`、非 JSON body → error、非 2xx → error。
- **web_search handler**：query 缺失 → 失败结果；limit 越界 clamp；engine 覆盖；SearXNG 挂 → `Success:false`（fail-loud 断言）。
- **web_extract handler**：
  - 小页面内联；大页面头+尾截断 + 落盘 + footer。
  - 落盘文件能被同 registry 的 `read_file(相对路径, offset)` 读回（端到端断言那个坑）。
  - SSRF 阻断、密钥命中阻断、scheme 非法。
  - 批量顺序保持、去重、>5 报告丢弃。
  - base64 图片转占位。
  - 落盘失败（只读目录）→ Warn + footer 降级 + 仍返截断内容。
- **drift-guard**：`internal/runtime` 的 gateable 一致性测试通过。
- **未配 searxng_url**：`web_search` 不注册；`web_extract` 仍注册。

## 13. 关键约束速查（踩坑点）

1. 落盘必须在 sandbox root 内，否则 `read_file` 读不回 → 落 `toolRoot/.stardust/web_cache`，过 `guard.Check`。
2. 工具结果被 runtime 硬截 4000 runes → 内联预算 clamp ≤3500。
3. `Arguments` 全 string → 数字参数 `strconv` 解析，失败 fail-loud。
4. 新工具必须登记 gateable + AutoAllowTools + 权限，否则测试红/调用被拒。
5. SearXNG 自建实例常在内网 → 搜索 client 需对其主机放行 SSRF。
6. fail-loud：外部失败走 `ToolResult{Success:false}`，编程错误走 Go error，落盘失败 Warn+降级不静默。

## 14. 开放问题

无（所有设计决策已确认）。
