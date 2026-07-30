# read_file 分页读 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Steps use checkbox (`- [ ]`).

**Goal:** 给 `read_file` 加 `offset`/`limit` 分页与可执行的续读提示，让模型能读完长文件，不再因「看不到后半」反复整篇重读。

**Architecture:** 在 `readFileTool` 内按 rune 切片；页大小默认 3500、clamp 到 ≤3500（硬低于 `maxToolResultChars=4000`，保证不被 runtime 二次截断）；仍有剩余时追加含具体 `offset=` 的续读提示；读盘上限 256KB→512KB。

**Tech Stack:** Go 标准库（strconv/strings/fmt）。

## Global Constraints

- **硬约束：单次返回 ≤ 3500 runes**，因为 `internal/runtime/messages.go` 的 `appendToolResults` 会 `truncateText(content, maxToolResultChars=4000)` 纯前截。超了分页就失效。
- **Fail-loud**：`offset < 0`、`offset >= 文件长度`、参数非数字 → 返回 error，**不静默返回空内容**（空内容会被模型读成「文件到此为止」）。
- `limit <= 0` 用默认 3500、`limit > 3500` clamp —— 契约在 description 里写明，属声明的可选，非兜底。
- 按 **rune** 切分，不得切出半个汉字。
- 不改 `maxToolResultChars`、`truncateText`、`search_content`、`list_files`。
- `go build/vet/test ./...` 全绿、`gofmt -l .` 空；错误路径有测试断言。

## 现状 seam（实测）

- `internal/tool/builtin.go:427-442`：`read_file` 描述符，`InputSchema.properties` 只有 `path`；handler 调 `readFileTool(ctx, absRoot, guard, call, options)`。
- `readFileTool` 内 `const maxReadFileBytes = 256 * 1024`，`io.ReadAll(io.LimitReader(file, maxReadFileBytes+1))`，超限时追加 `\n…[truncated: file exceeds N bytes]`；随后 `subtreeAgentsNote` 追加 agents.md 注入；`domain.ToolCall.Arguments` 是 `map[string]string`（数字参数需 `strconv.Atoi`）。

---

### Task 1: read_file 分页（schema + 切片 + 续读提示 + 512KB）

**Files:**
- Modify: `internal/tool/builtin.go`（`read_file` 描述符 schema、`readFileTool`、`maxReadFileBytes`）
- Test: `internal/tool/builtin_test.go`

**Interfaces:**
- Produces：
  - 常量 `readFilePageRunes = 3500`（默认/上限页大小）、`maxReadFileBytes = 512 * 1024`。
  - `read_file` schema 新增可选 `offset`（int，默认 0）、`limit`（int，默认 3500，上限 3500）。
  - `func paginateRunes(content string, offset, limit int) (page string, nextOffset int, total int, err error)` —— 纯函数：按 rune 切片，返回本页内容、下一页偏移（读完为 -1）、总 rune 数；`offset<0` 或 `offset>=total` 返回 error。

- [ ] **Step 1: Write the failing test（纯函数先行）**

```go
func TestPaginateRunes(t *testing.T) {
	content := strings.Repeat("汉", 1000) // 1000 runes, multibyte

	page, next, total, err := paginateRunes(content, 0, 400)
	if err != nil {
		t.Fatalf("paginateRunes(0,400) err = %v, want nil", err)
	}
	if len([]rune(page)) != 400 || next != 400 || total != 1000 {
		t.Fatalf("got page=%d next=%d total=%d, want 400/400/1000", len([]rune(page)), next, total)
	}
	// 末页：next 必须是 -1（读完），页长为剩余
	page, next, _, err = paginateRunes(content, 800, 400)
	if err != nil {
		t.Fatalf("paginateRunes(800,400) err = %v, want nil", err)
	}
	if len([]rune(page)) != 200 || next != -1 {
		t.Fatalf("last page: len=%d next=%d, want 200/-1", len([]rune(page)), next)
	}
	// 越界与负数：fail-loud
	if _, _, _, err := paginateRunes(content, 1000, 400); err == nil {
		t.Fatal("offset==total should error, not return empty content")
	}
	if _, _, _, err := paginateRunes(content, -1, 400); err == nil {
		t.Fatal("negative offset should error")
	}
	// 不切半个汉字
	page, _, _, _ = paginateRunes(content, 0, 1)
	if page != "汉" {
		t.Fatalf("page = %q, want a whole rune", page)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tool/ -run TestPaginateRunes -v`
Expected: FAIL — `undefined: paginateRunes`。

- [ ] **Step 3: Implement the pure function**

在 `internal/tool/builtin.go` 加：

```go
// readFilePageRunes is both the default and the maximum number of runes one
// read_file call returns. It is deliberately below runtime's
// maxToolResultChars (4000): appendToolResults truncates any longer tool
// result to that cap, which would silently cut the page short and defeat
// pagination.
const readFilePageRunes = 3500

// paginateRunes returns the [offset, offset+limit) rune window of content, the
// offset the caller should pass next (-1 when the window reached the end) and
// the content's total rune count. Slicing by rune (not byte) keeps multibyte
// text intact.
//
// An offset at or past the end is an error rather than an empty page: an empty
// result reads to the model as "the file ends here", which is exactly the
// misunderstanding this pagination exists to prevent.
func paginateRunes(content string, offset, limit int) (string, int, int, error) {
	runes := []rune(content)
	total := len(runes)
	if offset < 0 {
		return "", 0, total, fmt.Errorf("read_file offset must not be negative, got %d", offset)
	}
	if offset >= total && total > 0 {
		return "", 0, total, fmt.Errorf("read_file offset %d is past the end of the file (%d chars)", offset, total)
	}
	if limit <= 0 || limit > readFilePageRunes {
		limit = readFilePageRunes
	}
	end := offset + limit
	if end >= total {
		return string(runes[offset:]), -1, total, nil
	}
	return string(runes[offset:end]), end, total, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/tool/ -run TestPaginateRunes -count=1 -v`
Expected: PASS。

- [ ] **Step 5: Write the failing handler test**

```go
func TestReadFilePaginatesLongFile(t *testing.T) {
	root := t.TempDir()
	body := strings.Repeat("汉", 5000) // > one page
	writeFile(t, filepath.Join(root, "big.md"), body)
	reg := NewFileReadWriteWorkspaceRegistry(root, nil)

	first, err := reg.Invoke(t.Context(), domain.ToolCall{
		Name: "read_file", ID: "1", Arguments: map[string]string{"path": "big.md"},
	})
	if err != nil {
		t.Fatalf("read_file(page1) err = %v", err)
	}
	if !strings.Contains(first.Output, "offset=3500") {
		t.Fatalf("page1 must tell the model how to continue, got tail: %q", tailOf(first.Output))
	}
	// 第二页：从提示给出的 offset 续读，且不应再要求 offset=3500
	second, err := reg.Invoke(t.Context(), domain.ToolCall{
		Name: "read_file", ID: "2", Arguments: map[string]string{"path": "big.md", "offset": "3500"},
	})
	if err != nil {
		t.Fatalf("read_file(page2) err = %v", err)
	}
	if strings.Contains(second.Output, "继续读用") {
		t.Fatalf("last page must not advertise a next page, got tail: %q", tailOf(second.Output))
	}
	// 越界 fail-loud
	if _, err := reg.Invoke(t.Context(), domain.ToolCall{
		Name: "read_file", ID: "3", Arguments: map[string]string{"path": "big.md", "offset": "999999"},
	}); err == nil {
		t.Fatal("offset past end should error")
	}
}

func TestReadFileShortFileHasNoContinuationHint(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "small.md"), "短文件")
	reg := NewFileReadWriteWorkspaceRegistry(root, nil)
	res, err := reg.Invoke(t.Context(), domain.ToolCall{
		Name: "read_file", ID: "1", Arguments: map[string]string{"path": "small.md"},
	})
	if err != nil {
		t.Fatalf("read_file err = %v", err)
	}
	if !strings.Contains(res.Output, "短文件") || strings.Contains(res.Output, "继续读用") {
		t.Fatalf("short file must return whole content with no hint, got %q", res.Output)
	}
}

// tailOf returns the last 200 runes of s for readable failure messages.
func tailOf(s string) string {
	r := []rune(s)
	if len(r) <= 200 {
		return s
	}
	return string(r[len(r)-200:])
}
```

（`reg.Invoke` 与 `writeFile` 以 `internal/tool/builtin_test.go` 现有 harness 为准；若分发方法名不同请对齐现状。）

- [ ] **Step 6: Run to verify it fails**

Run: `go test ./internal/tool/ -run 'TestReadFile' -count=1 -v`
Expected: FAIL —— 目前无 offset 参数、无续读提示。

- [ ] **Step 7: Implement the handler + schema + 512KB**

1) 描述符（`internal/tool/builtin.go:427-439` 附近）：description 说明分页，`properties` 增加 `offset`/`limit`：

```go
		Description: fmt.Sprintf("Read a UTF-8 text file inside the workspace root (%s). The path argument can be relative (resolved against workspace root) or absolute (must be within workspace root). Long files are returned one page at a time: pass offset to continue reading where the previous page ended.", absRoot),
```
```go
				"offset": map[string]any{"type": "integer", "description": "Start reading at this character (rune) offset. Defaults to 0. Use the offset printed at the end of a truncated page to read the rest."},
				"limit":  map[string]any{"type": "integer", "description": fmt.Sprintf("Maximum characters to return, capped at %d (the default).", readFilePageRunes)},
```

2) `maxReadFileBytes` 由 `256 * 1024` 改为 `512 * 1024`（注释说明：与分页配合，512KB 内均可经 offset 触达）。

3) `readFileTool` 中，把 `output := string(data)`（及超 `maxReadFileBytes` 的标注）之后、`subtreeAgentsNote` 之前，改为分页：

```go
	offset, err := intArg(call.Arguments, "offset", 0)
	if err != nil {
		return domain.ToolResult{}, err
	}
	limit, err := intArg(call.Arguments, "limit", readFilePageRunes)
	if err != nil {
		return domain.ToolResult{}, err
	}
	page, next, total, err := paginateRunes(output, offset, limit)
	if err != nil {
		return domain.ToolResult{}, err
	}
	output = page
	if next >= 0 {
		output += fmt.Sprintf("\n…[已返回第 %d-%d 字，共 %d 字；继续读用 read_file(path=%q, offset=%d)]",
			offset+1, next, total, call.Arguments["path"], next)
	}
```

4) 参数解析 helper（同文件）：

```go
// intArg parses an optional integer tool argument. An absent or empty value
// yields def; a present but unparseable value is an error rather than a silent
// fallback to the default, which would hide a malformed call from the model.
func intArg(args map[string]string, name string, def int) (int, error) {
	raw := strings.TrimSpace(args[name])
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("read_file %s must be an integer, got %q: %w", name, raw, err)
	}
	return v, nil
}
```
（`strconv` 若未导入则加入 import。）

- [ ] **Step 8: Run to verify it passes + 全量门禁**

Run: `go test ./internal/tool/ -run 'TestReadFile|TestPaginateRunes' -count=1 -v && go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l .`
Expected: 目标测试 PASS；全包全绿；gofmt 空。

- [ ] **Step 9: Commit**

```bash
git add internal/tool/builtin.go internal/tool/builtin_test.go
git commit -m "feat(tool): read_file 分页读(offset/limit)+续读提示，读盘上限提至 512KB"
```

---

## Self-Review

- **Spec 覆盖**：offset/limit schema=Step 7.1；页大小 3500 与 clamp=`readFilePageRunes` + `paginateRunes`；续读提示含具体参数=Step 7.3；512KB=Step 7.2；越界/负数/非数字 fail-loud=`paginateRunes` + `intArg` 及其测试；末页无提示=Step 5 测试；短文件全文=Step 5 测试；rune 切分=Step 1 测试。`search_content`/`maxToolResultChars`/`truncateText` 未动（非目标）。均覆盖。
- **占位**：无。测试注明 `reg.Invoke`/`writeFile` 以现有 harness 为准（因文件而异，执行时对齐），非逻辑占位。
- **类型一致**：`readFilePageRunes`、`paginateRunes(content, offset, limit) (string, int, int, error)`、`intArg(args, name, def) (int, error)`、`maxReadFileBytes` 跨步骤一致；`next == -1` 表示读完，与提示条件 `next >= 0` 对应。
