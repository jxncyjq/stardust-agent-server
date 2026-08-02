# 工具截断治理 P1（runtime 统一落盘分页）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 工具结果超过预算时，runtime 统一层把全文落盘到 sandbox 内 `toolRoot/.stardust/tool_results/`，in-context 只留 preview + 自我描述 footer（含 `read_file` 取回命令），让 agent 能翻页拿到完整数据而非盲目重试。web_extract 的工具层落盘重构并入统一层。

**Architecture:** 在 `internal/runtime` 层集中处理。新增 `Runtime.toolRoot` 字段（每 RunTask 建新 Runtime，注入该 run 的 sandbox root）。新增 `toolcache.go` 落盘函数与 `renderToolResultContent`（截断/落盘/footer 决策）。`conversation.appendToolResults` 改用它。`read_file` 结果与空 toolRoot 退化为 P0 纯截断（防 persist→read 环 / 无处落盘）。web_extract 移除自有落盘。复用 P0 的 `truncateText` 与 web_extract 已验证的 guard 落盘模式。

**Tech Stack:** Go；`crypto/sha256`、`os`、`path/filepath`、`port.WorkspacePathGuard`、`log/slog`。

**参考 spec:** `docs/superpowers/specs/2026-08-01-tool-result-truncation-governance-design.md`（§6 P1）
**前置:** P0 已合入 master（`truncateText` 自我描述 footer，commit 1a5b28a）。

**关键约束:**
- 落盘必须在 sandbox root 内（`guard.Check` 先于 `MkdirAll`），read_file 才能读回。
- `read_file` 结果豁免落盘（防 persist→read→persist 环）。
- toolRoot 空（测试/无 sandbox）→ 退化为 P0 纯截断，不落盘。
- 阈值复用现有 `maxToolResultChars`（默认 4000 rune）；cache_dir 用常量 `.stardust/tool_results`（YAGNI：不新增 config 字段，需要时再配置化——偏离 spec §8 的 config 化，简化首版）。

**全局门槛（commit 前）：** 在 `legion/legionAgent` 目录 `go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 空。

---

## 文件结构

| 文件 | 动作 | 职责 |
|------|------|------|
| `internal/runtime/runtime.go` | 修改 | `Config` + `Runtime` 加 `ToolRoot`/`toolRoot`；`NewRuntime` 传入；RunTask 调用 appendToolResults 传 toolRoot |
| `internal/runtime/toolcache.go` | 新建 | `writeToolResultCache` 落盘 + `renderToolResultContent` 截断/落盘/footer 决策 |
| `internal/runtime/toolcache_test.go` | 新建 | 落盘 + read_file 读回 + 防环 + 降级测试 |
| `internal/runtime/messages.go` | 修改 | `appendToolResults` 签名加 toolRoot/cacheDir/logger，改用 `renderToolResultContent` |
| `internal/runtime/agent_resolver.go` | 修改 | `NewRuntime(Config{... ToolRoot: toolRoot})` |
| `internal/cli/command.go` | 修改 | `defaultTaskRunner.RunTask` 设 `runtimeCfg.ToolRoot = root` |
| `internal/app/app.go` | 修改 | 两处 `NewRuntime` 传 `ToolRoot`（对应 registry 的 root） |
| `internal/tool/webextract.go` | 修改 | 移除工具层落盘（`truncateAndCache`/`writeExtractCache`/`sanitizeSlug`/`char_limit`），返回完整内容 |
| `internal/tool/webextract_test.go` | 修改 | 适配重构（移除落盘断言，改为完整内容断言） |
| 各 `*_test.go`（appendToolResults 调用者） | 修改 | 更新 appendToolResults 调用签名 |

---

## Task 1: Runtime.toolRoot 注入管道

打通 sandbox root 到 Runtime，后续落盘用。此 task 只加字段与注入，行为不变。

**Files:**
- Modify: `internal/runtime/runtime.go`（`Config` struct、`Runtime` struct、`NewRuntime`）
- Modify: `internal/runtime/agent_resolver.go:249`
- Modify: `internal/cli/command.go:1997`
- Modify: `internal/app/app.go`（NewRuntime 两处）
- Test: `internal/runtime/runtime_toolroot_test.go`（新建）

- [ ] **Step 1: 写失败测试——NewRuntime 保存 ToolRoot**

创建 `internal/runtime/runtime_toolroot_test.go`：

```go
package runtime

import "testing"

func TestNewRuntimeStoresToolRoot(t *testing.T) {
	rt := NewRuntime(Config{ToolRoot: "/tmp/sandbox"})
	if rt.toolRoot != "/tmp/sandbox" {
		t.Fatalf("toolRoot = %q, want /tmp/sandbox", rt.toolRoot)
	}
}

func TestNewRuntimeEmptyToolRootOK(t *testing.T) {
	rt := NewRuntime(Config{})
	if rt.toolRoot != "" {
		t.Fatalf("empty ToolRoot must stay empty (no sandbox → no cache), got %q", rt.toolRoot)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run TestNewRuntimeStoresToolRoot -v`
Expected: 编译失败（Config/Runtime 无 ToolRoot/toolRoot）

- [ ] **Step 3: `Config` 加 `ToolRoot` 字段**

`internal/runtime/runtime.go` 的 `Config` struct（runtime.go:68，`MaxToolResultChars` 附近）加：

```go
	// ToolRoot is the tool sandbox root for THIS run (task.WorkingDir, or the
	// context root fallback). When non-empty, an oversized tool result is cached
	// to ToolRoot/.stardust/tool_results/ so read_file can page it back; empty
	// means no sandbox (tests / no workspace) and results fall back to plain
	// self-describing truncation. Every production NewRuntime is built per-task,
	// so a fixed field is correct.
	ToolRoot string
```

- [ ] **Step 4: `Runtime` 加 `toolRoot` 字段 + `NewRuntime` 传入**

`Runtime` struct（runtime.go:157，`maxToolResultChars` 附近）加 `toolRoot string`。`NewRuntime` 返回值（runtime.go:297）加：

```go
		toolRoot:              cfg.ToolRoot,
```

- [ ] **Step 5: 生产注入点传 ToolRoot**

- `internal/runtime/agent_resolver.go:249` `NewRuntime(Config{...})` 加 `ToolRoot: toolRoot,`（`toolRoot` 变量已在该函数 agent_resolver.go:238）。
- `internal/cli/command.go` `defaultTaskRunner.RunTask`（:1997-1998 `runtimeCfg := d.runtimeCfg; runtimeCfg.Tools = tools` 之后）加：
  ```go
  	runtimeCfg.ToolRoot = root
  ```
  （`root` 变量已在 command.go:1985）。
- `internal/app/app.go:105`（RunDemo）`runtime.Config{...}` 加 `ToolRoot: ".",`（该 registry 是 `NewWorkspaceRegistry(".", audit)`，root="."）。
- `internal/app/app.go:241`（另一处 NewRuntime）：grep 该处 `Tools:` 用的 registry root 变量，把同一 root 作为 `ToolRoot` 传入。若该处 registry root 也是 "." 或某变量，对应传入。

> grep 确认没有遗漏的生产 `NewRuntime(runtime.Config{`：`grep -rn "NewRuntime(runtime.Config{\|NewRuntime(Config{" --include=*.go internal/ | grep -v _test.go`。每个生产调用点都要传 ToolRoot=其 Tools 的 sandbox root。测试调用点无需改（空 ToolRoot 合法）。

- [ ] **Step 6: 跑测试 + 全量**

Run: `go test ./internal/runtime/ -run TestNewRuntime -v ; go build ./... ; go test ./...`
Expected: PASS + 构建成功 + 全绿（行为未变）

- [ ] **Step 7: Commit**

```bash
git add internal/runtime/runtime.go internal/runtime/runtime_toolroot_test.go internal/runtime/agent_resolver.go internal/cli/command.go internal/app/app.go
git commit -m "feat(runtime): 注入 ToolRoot 到 Runtime（P1 落盘前置）"
```

---

## Task 2: toolcache.go — 落盘 + 渲染决策

**Files:**
- Create: `internal/runtime/toolcache.go`
- Test: `internal/runtime/toolcache_test.go`

- [ ] **Step 1: 写失败测试——落盘 + 读回路径 + 防环 + 降级**

创建 `internal/runtime/toolcache_test.go`：

```go
package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"log/slog"
)

func TestRenderToolResultCachesAndFooter(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("X", 20000)
	out := renderToolResultContent("fetch_url", big, 4000, root, defaultToolResultCacheDir, slog.Default())

	if !strings.Contains(out, "硬截断") {
		t.Fatalf("missing hard-truncation footer: %q", out[:min2(300, len(out))])
	}
	if !strings.Contains(out, "read_file") || !strings.Contains(out, ".stardust/tool_results") {
		t.Fatalf("footer missing read_file/cache path: %q", out)
	}
	// preview head present, and shorter than the original.
	if !strings.HasPrefix(out, strings.Repeat("X", 4000)) {
		t.Fatalf("preview head missing")
	}
	// The cache file exists and holds the full content.
	rel := footerCachePath(t, out)
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("cache file unreadable: %v", err)
	}
	if len([]rune(string(data))) != 20000 {
		t.Fatalf("cache file should hold full 20000 runes, got %d", len([]rune(string(data))))
	}
}

func TestRenderToolResultReadFileExemptFromCache(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("Y", 20000)
	out := renderToolResultContent("read_file", big, 4000, root, defaultToolResultCacheDir, slog.Default())
	// read_file results must NOT be cached (persist→read loop guard): plain truncation only.
	if strings.Contains(out, ".stardust/tool_results") {
		t.Fatalf("read_file result must not be cached, got %q", out)
	}
	if !strings.Contains(out, "硬截断") {
		t.Fatalf("read_file oversize should still get plain truncation footer")
	}
}

func TestRenderToolResultEmptyRootPlainTruncation(t *testing.T) {
	big := strings.Repeat("Z", 20000)
	out := renderToolResultContent("fetch_url", big, 4000, "", defaultToolResultCacheDir, slog.Default())
	if strings.Contains(out, "tool_results") {
		t.Fatalf("empty toolRoot must not cache, got %q", out[:min2(200, len(out))])
	}
	if !strings.Contains(out, "硬截断") {
		t.Fatalf("empty toolRoot oversize should get plain truncation")
	}
}

func TestRenderToolResultUnderBudgetUnchanged(t *testing.T) {
	small := "short"
	out := renderToolResultContent("fetch_url", small, 4000, t.TempDir(), defaultToolResultCacheDir, slog.Default())
	if out != small {
		t.Fatalf("under-budget content must be returned verbatim, got %q", out)
	}
}

// helpers
func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func footerCachePath(t *testing.T, out string) string {
	t.Helper()
	const marker = `read_file path="`
	i := strings.Index(out, marker)
	if i < 0 {
		t.Fatalf("no read_file path marker in %q", out)
	}
	rest := out[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("unterminated read_file path in %q", rest)
	}
	return rest[:j]
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run TestRenderToolResult -v`
Expected: 编译失败（`renderToolResultContent` / `defaultToolResultCacheDir` / `writeToolResultCache` 未定义）

- [ ] **Step 3: 实现 `toolcache.go`**

创建 `internal/runtime/toolcache.go`：

```go
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/stardust/legion-agent/internal/port"
)

const (
	// defaultToolResultCacheDir is where oversized tool results are cached,
	// relative to the run's tool sandbox root, so read_file can page them back.
	defaultToolResultCacheDir = ".stardust/tool_results"
	// toolResultCacheFileMax bounds a single cached file so a giant result cannot
	// write unbounded bytes into the workspace.
	toolResultCacheFileMax = 2_000_000
)

// renderToolResultContent decides how one tool result's text reaches the model.
//
//   - Within maxResultChars: returned verbatim.
//   - Oversized AND cacheable (non-empty toolRoot, tool is not read_file): the
//     full text is cached under toolRoot/cacheDir and the model gets a preview +
//     a self-describing footer naming the cache path and the exact read_file call
//     to page the rest.
//   - Oversized but NOT cacheable (empty toolRoot = no sandbox, or a read_file
//     result whose cache would create a persist→read→persist loop): plain
//     self-describing truncation via truncateText (P0), no disk write.
//
// A cache write failure is never fatal: it is logged at Warn (fail-loud 铁律
// requires recording, not swallowing) and the result falls back to plain
// truncation so the model still gets a usable, self-describing answer.
func renderToolResultContent(toolName, content string, maxResultChars int, toolRoot, cacheDir string, logger *slog.Logger) string {
	if maxResultChars <= 0 {
		return content
	}
	runes := []rune(content)
	total := len(runes)
	if total <= maxResultChars {
		return content
	}
	if strings.TrimSpace(toolRoot) == "" || toolName == "read_file" {
		return truncateText(content, maxResultChars)
	}
	relPath, err := writeToolResultCache(toolRoot, cacheDir, toolName, content)
	if err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		// A sandbox escape is "本不该发生" (cacheDir is a constant); either way the
		// error is recorded and we degrade to plain truncation rather than dropping
		// the result or crashing the tool loop.
		logger.Warn("tool result cache write failed; falling back to plain truncation",
			"tool", toolName, "cache_dir", cacheDir, "error", err,
			"sandbox_escape", errors.Is(err, port.ErrPathOutsideWorkspace))
		return truncateText(content, maxResultChars)
	}
	return string(runes[:maxResultChars]) + fmt.Sprintf(
		"\n\n──────── [输出被硬截断 / OUTPUT HARD-TRUNCATED] ────────\n"+
			"这是硬截断（上下文预算限制），非数据或参数问题——重试不会有帮助。全文已完整保存。\n"+
			"This is a hard truncation; retrying won't help. The full text is saved.\n"+
			"显示 %d / 共 %d 字符（rune）。\n"+
			"取回剩余：read_file path=%q offset=%d\n",
		maxResultChars, total, relPath, maxResultChars)
}

// writeToolResultCache writes content to toolRoot/cacheDir/<tool>-<hash>.md,
// guarded to stay inside toolRoot, and returns the path RELATIVE to toolRoot
// (forward-slashed, what read_file expects). guard.Check runs BEFORE any mkdir
// so a traversal cacheDir cannot create directories outside the sandbox. Errors
// are returned, never swallowed.
func writeToolResultCache(toolRoot, cacheDir, toolName, content string) (string, error) {
	absRoot, err := filepath.Abs(toolRoot)
	if err != nil {
		return "", fmt.Errorf("resolve tool root %q: %w", toolRoot, err)
	}
	sum := sha256.Sum256([]byte(content))
	name := fmt.Sprintf("%s-%s.md", sanitizeToolName(toolName), hex.EncodeToString(sum[:])[:10])
	dir := filepath.Join(absRoot, cacheDir)
	target := filepath.Join(dir, name)

	guard := port.NewWorkspacePathGuard(absRoot)
	if _, err := guard.Check(context.Background(), target); err != nil {
		return "", fmt.Errorf("cache path escapes workspace: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	if runes := []rune(content); len(runes) > toolResultCacheFileMax {
		content = string(runes[:toolResultCacheFileMax]) + "\n\n[... stored copy capped ...]"
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

// sanitizeToolName keeps only filename-safe chars from a tool name for the cache
// file. Tool names are already [a-z_], so this is a defensive guard.
func sanitizeToolName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "tool"
	}
	return out
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ -run TestRenderToolResult -v`
Expected: PASS（4 个）

- [ ] **Step 5: 格式化 + 构建**

Run: `gofmt -w internal/runtime/toolcache.go internal/runtime/toolcache_test.go && go build ./...`

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/toolcache.go internal/runtime/toolcache_test.go
git commit -m "feat(runtime): 工具结果落盘 + 渲染决策（renderToolResultContent）"
```

---

## Task 3: appendToolResults 接入统一落盘

**Files:**
- Modify: `internal/runtime/messages.go`（`appendToolResults`）
- Modify: `internal/runtime/runtime.go:507`（调用点）
- Modify: `internal/runtime/messages_test.go`、`internal/runtime/checkpoint_helpers_test.go`（调用签名）
- Test: `internal/runtime/messages_test.go`

- [ ] **Step 1: 改 `appendToolResults` 签名 + 改用 renderToolResultContent**

`internal/runtime/messages.go` 顶部 import 加 `"log/slog"`。把 `appendToolResults`（messages.go:63-83）替换为：

```go
// appendToolResults records one tool turn per executed call, paired by call ID.
// A failed call is reported to the model as its own tool turn rather than being
// dropped: the model needs to see the failure to recover, and a provider
// rejects an assistant tool call left unanswered. Oversized successful results
// are cached to toolRoot/cacheDir and replaced with a preview + read_file footer
// (renderToolResultContent); an empty toolRoot or a read_file result degrades to
// plain self-describing truncation.
func (c *conversation) appendToolResults(calls []domain.ToolCall, results []domain.ToolResult, maxResultChars int, toolRoot, cacheDir string, logger *slog.Logger) {
	byID := make(map[string]domain.ToolResult, len(results))
	for _, res := range results {
		byID[res.CallID] = res
	}
	for _, call := range calls {
		res, ok := byID[call.ID]
		if !ok {
			continue
		}
		content := res.Output
		if !res.Success {
			content = "failed: " + res.Error
		}
		c.messages = append(c.messages, port.InferenceMessage{
			Role:       port.RoleTool,
			ToolCallID: call.ID,
			Content:    renderToolResultContent(call.Name, content, maxResultChars, toolRoot, cacheDir, logger),
		})
	}
}
```

> 注意：failed 结果（`"failed: ..."`）也过 renderToolResultContent——通常短，不触发落盘；触发也无害（错误信息落盘可读回）。read_file 豁免按 call.Name 判定，对成功/失败一致。

- [ ] **Step 2: 改调用点（runtime.go:507）**

`internal/runtime/runtime.go:507`：
```go
	st.convo.appendToolResults(calls, results, r.maxToolResultChars, r.toolRoot, defaultToolResultCacheDir, r.logger)
```

- [ ] **Step 3: 改测试调用点签名**

- `internal/runtime/messages_test.go`：所有 `convo.appendToolResults(calls, results, N)` 调用（messages_test.go:18/53/68/85）改为 `convo.appendToolResults(calls, results, N, "", defaultToolResultCacheDir, slog.Default())`。文件 import 加 `"log/slog"`。
- `internal/runtime/checkpoint_helpers_test.go:22`：同样加三参 `"", defaultToolResultCacheDir, slog.Default()`，import 加 `"log/slog"`。

> 这些测试传空 toolRoot → 走纯截断（P0 行为），现有断言（含 `truncated`/`硬截断`）保持有效。`TestConversationTruncatesOversizedToolResult`（P0 已改为断言 `硬截断`）继续通过。

- [ ] **Step 4: 新增测试——非 read_file 大结果在 appendToolResults 里落盘**

在 `internal/runtime/messages_test.go` 追加：

```go
func TestAppendToolResultsCachesOversizedNonReadFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	convo := newConversation("base", nil)
	calls := []domain.ToolCall{{ID: "c1", Name: "fetch_url"}}
	convo.appendAssistant("", calls)
	convo.appendToolResults(calls,
		[]domain.ToolResult{{CallID: "c1", Success: true, Output: strings.Repeat("X", 20000)}},
		4000, root, defaultToolResultCacheDir, slog.Default())

	msgs := convo.render(0)
	if !strings.Contains(msgs[2].Content, ".stardust/tool_results") || !strings.Contains(msgs[2].Content, "read_file") {
		t.Fatalf("oversized fetch_url result should be cached with a read_file footer, got %q", msgs[2].Content)
	}
}
```

- [ ] **Step 5: 跑测试 + 全量**

Run: `go test ./internal/runtime/ -v -run "TestConversation|TestAppendToolResults|TestRenderToolResult" ; go build ./... ; go test ./...`
Expected: 全 PASS、构建成功、全绿

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/messages.go internal/runtime/runtime.go internal/runtime/messages_test.go internal/runtime/checkpoint_helpers_test.go
git commit -m "feat(runtime): appendToolResults 接入统一落盘分页（P1）"
```

---

## Task 4: web_extract 重构（移除工具层落盘）

web_extract 不再自己截断落盘，返回完整渲染内容，交 runtime 统一层处理。保留 base64 占位与密钥阻断。

**Files:**
- Modify: `internal/tool/webextract.go`
- Test: `internal/tool/webextract_test.go`

- [ ] **Step 1: 改测试——web_extract 返回完整内容（不再自带 footer/落盘）**

在 `internal/tool/webextract_test.go`：
- 删除 `TestWebExtractTruncatesAndCaches`、`TestWebExtractCacheDirTraversalBlocked`（这两个测的是工具层落盘，已移除）。
- 保留并确认 `TestWebExtractInlineSmallPage`、`TestWebExtractOrderAndMultiple`、`TestWebExtractRejectsMoreThanFive`、`TestWebExtractMissingURLsFails`、`TestWebExtractStripsBase64Images`、`TestWebExtractBlocksSecretInURL`。
- 追加：大页面现在原样返回完整内容（不截断、不落盘），runtime 层才截断：

```go
func TestWebExtractReturnsFullContentNoToolLevelTruncation(t *testing.T) {
	big := strings.Repeat("Z", 20000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(big))
	}))
	defer server.Close()

	registry := newExtractRegistry(t, t.TempDir())
	res, err := webExtract(t, registry, map[string]string{"urls": server.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// web_extract no longer truncates/caches; full content passes through.
	if strings.Contains(res.Output, "TRUNCATED") || strings.Contains(res.Output, "tool_results") {
		t.Fatalf("web_extract must not truncate/cache at tool level anymore, got footer in %q", res.Output[:min(300, len(res.Output))])
	}
	if !strings.Contains(res.Output, strings.Repeat("Z", 20000)) {
		t.Fatalf("full 20000-char content should pass through")
	}
}
```

> 若 `min` helper 在该测试包已存在则复用；否则用现有的（webextract_test.go 之前的 Task 用过 `min`——确认后复用，勿重复声明）。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tool/ -run TestWebExtract -v`
Expected: 编译失败（删了引用 truncateAndCache 的测试后）或新测失败

- [ ] **Step 3: 重构 `webextract.go`**

在 `internal/tool/webextract.go`：

1. `extractOne`（webextract.go:172-201）末尾改为返回完整内容（移除 truncateAndCache 调用）：
   ```go
   	text = stripBase64Images(text)
   	return extractResult{URL: rawURL, Content: text}
   ```
   （保留其上的 parse/scheme/密钥/SSRF/fetchPage/stripBase64Images。`extractOne` 签名中 `charLimit int` 参数不再使用——删除该参数，并更新 `handleWebExtract` 里的调用 `extractOne(ctx, client, opts, toolRoot, rawURL)`。`toolRoot` 参数也不再被 extractOne 使用——一并删除，调用改 `extractOne(ctx, client, opts, rawURL)`。）

2. `handleWebExtract`（webextract.go:131-167）移除 `char_limit` 解析块（:136-151）与 `charLimit` 变量；循环调用改 `extractOne(ctx, client, opts, rawURL)`。

3. `webExtractDescriptor`（:80-97）移除 `char_limit` 属性，描述改为不提落盘/footer：
   ```go
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
   ```
   并更新 `registerWebExtractTool`（:73-78）里 `webExtractDescriptor(opts.ExtractCharLimit, opts.Timeout)` → `webExtractDescriptor(opts.Timeout)`。

4. **删除** 不再使用的：`truncateAndCache`、`writeExtractCache`、`sanitizeSlug`、常量 `webExtractCacheFileMax`。删除随之变为未使用的 import：`crypto/sha256`、`encoding/hex`、`os`、`path/filepath`、`errors`（`log/slog` 也可能未用——`go build`/`go vet` 会指出，按提示删）。**保留** `port`？重构后 webextract.go 若不再用 `port.ErrPathOutsideWorkspace`/`NewWorkspacePathGuard` 则删除 `port` import。用 `go build` 逐个确认未使用 import 并删除。

> `opts.ExtractCharLimit` / `opts.ExtractCacheDir` 字段（`WebToolOptions`）现在无人读——**保留字段不删**（移除是另一次清理，YAGNI 边界；且 `webToolOptions` 映射仍填它们，删字段要连带改 config，超出本 task）。仅确保 web_extract 不再读它们。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tool/ -run TestWebExtract -v`
Expected: PASS（保留的 + 新增的；base64/密钥仍通过）

- [ ] **Step 5: 全量门槛**

Run: `go build ./... ; go vet ./... ; go test ./... ; gofmt -l .`
Expected: 构建成功、vet 无告警、全绿、gofmt 空

- [ ] **Step 6: Commit**

```bash
git add internal/tool/webextract.go internal/tool/webextract_test.go
git commit -m "refactor(tool): web_extract 移除工具层落盘，交 runtime 统一处理（P1）"
```

---

## 自检结论（写计划者已核对）

- **Spec 覆盖（§6 P1）**：runtime 统一落盘（T2/T3）、落盘位置 toolRoot/.stardust/tool_results（T2 常量）、toolRoot 注入（T1）、guard.Check 先于 mkdir（T2）、footer 读回契约（T2/T3）、read_file 豁免防环（T2/T3）、落盘上限 2MB（T2）、web_extract 重构保留 base64+密钥（T4）、落盘失败 Warn 降级（T2）。均有任务。偏离：cache_dir 用常量非 config（YAGNI，plan header 已注明）；阈值复用 maxToolResultChars 非新增 previewChars（默认 4000 非 spec 的 3000，已注明）。
- **Placeholder 扫描**：无 TBD/TODO。T1 Step5 app.go:241 与"grep 确认无遗漏 NewRuntime"是明确 grep 指引；T4 import 删除按 `go build` 提示——都是可执行动作非占位。
- **类型/签名一致性**：`renderToolResultContent(toolName, content string, maxResultChars int, toolRoot, cacheDir string, logger *slog.Logger)` 在 T2 定义、T3 调用一致；`appendToolResults(..., maxResultChars int, toolRoot, cacheDir string, logger *slog.Logger)` 在 T3 定义与所有调用点（runtime.go:507 + 测试）一致；`defaultToolResultCacheDir` 常量 T2 定义、T3 用；`writeToolResultCache` T2 定义、renderToolResultContent 调用；`extractOne` 去 `charLimit`/`toolRoot` 参数后签名 `(ctx, client, opts, rawURL)` 在 T4 定义与调用一致。
- **实现锚点**：Config@runtime.go:68、Runtime@:157、NewRuntime@:297、RunTask appendToolResults@:507；agent_resolver NewRuntime@:249（toolRoot@:238）；defaultTaskRunner@command.go:1984（root@:1985，runtimeCfg@:1997）；app.go NewRuntime@:105/:241；appendToolResults@messages.go:63；webextract extractOne@:172 / handleWebExtract@:131 / descriptor@:80 / truncateAndCache@:218 / writeExtractCache@:253。
