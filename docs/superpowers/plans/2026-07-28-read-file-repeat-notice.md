# read_file 重读可见提醒 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Steps use checkbox (`- [ ]`).

**Goal:** read_file 在任务内重复读到内容未变的同一文件时，前置显眼提醒（第 N 次 + 引导 search_content），全文仍返回，促使模型停止重读、降低轮数/token。

**Architecture:** 仿 `injectedAgentsSet` 加任务级 `readHistory`（mutex+map[path]{hash,count}），每 registry 构造一个；`readFileTool` 记录内容哈希，内容未变的重读前置 `repeatNotice`。不动 render/append-only。

**Tech Stack:** Go 标准库（`crypto/sha256`、`sync`）。

## Global Constraints

- Fail-loud：内容**永不丢**（全文恒返回，提醒只前置）；去重逻辑不产生也不吞 error。
- 守 152-incident 硬约束：提醒含「第 N 次」，各轮不同、重复对模型可见，不折叠成字节相同。
- 仅 read_file；search_content/list_files 不动。
- `go build/vet/test ./...` 全绿、`gofmt -l .` 空。公开/非导出符号 Go doc 注释。错误路径有测试断言（本变更无 error 路径，重点覆盖 unchanged 判定各分支）。

---

### Task 1: readHistory + repeatNotice（纯逻辑）

**Files:**
- Modify: `internal/tool/builtin.go`
- Test: `internal/tool/builtin_test.go`

**Interfaces:**
- Produces:
  - `type readHistory struct { mu sync.Mutex; seen map[string]readEntry }`；`type readEntry struct { hash string; count int }`
  - `func newReadHistory() *readHistory`
  - `func (h *readHistory) record(path string, content string) (count int, unchanged bool)` — count=该 path 累计读取次数（含本次）；unchanged = count>1 且 content 的 sha256 与上次相同。线程安全。
  - `func repeatNotice(count int) string` — 固定文案（含 count），以两个换行结尾。

- [ ] **Step 1: Write the failing test**

```go
// builtin_test.go
func TestReadHistoryRecord(t *testing.T) {
	h := newReadHistory()
	if c, u := h.record("/x", "A"); c != 1 || u {
		t.Fatalf("first read = (%d,%v), want (1,false)", c, u)
	}
	if c, u := h.record("/x", "A"); c != 2 || !u {
		t.Fatalf("second identical read = (%d,%v), want (2,true)", c, u)
	}
	if c, u := h.record("/x", "B"); c != 3 || u {
		t.Fatalf("third changed read = (%d,%v), want (3,false)", c, u)
	}
	if c, u := h.record("/y", "A"); c != 1 || u {
		t.Fatalf("other path = (%d,%v), want (1,false)", c, u)
	}
}

func TestRepeatNoticeCarriesCount(t *testing.T) {
	n := repeatNotice(2)
	if !strings.Contains(n, "第 2 次") {
		t.Fatalf("notice missing count: %q", n)
	}
	if !strings.Contains(n, "search_content") {
		t.Fatalf("notice should guide to search_content: %q", n)
	}
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/tool/ -run 'TestReadHistoryRecord|TestRepeatNoticeCarriesCount' -v`
Expected: FAIL — `undefined: newReadHistory` / `repeatNotice`.

- [ ] **Step 3: Implement**

在 `internal/tool/builtin.go` 顶部 import 增加 `crypto/sha256`、`encoding/hex`（若未有）、`sync`（若未有）。新增：

```go
type readEntry struct {
	hash  string
	count int
}

// readHistory tracks, per task (one per workspace registry), how many times each
// file path has been read and the hash of the last content returned, so a repeat
// read of unchanged content can be flagged to the model instead of silently
// re-sending an identical full copy every round.
type readHistory struct {
	mu   sync.Mutex
	seen map[string]readEntry
}

func newReadHistory() *readHistory {
	return &readHistory{seen: make(map[string]readEntry)}
}

// record notes a read of path with the given content. It returns the running
// read count for that path (including this read) and whether this read returned
// the same content as the previous read of the same path (unchanged is always
// false on the first read). Thread-safe for concurrent tool calls.
func (h *readHistory) record(path string, content string) (count int, unchanged bool) {
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.seen[path]
	count = prev.count + 1
	unchanged = count > 1 && prev.hash == hash
	h.seen[path] = readEntry{hash: hash, count: count}
	return count, unchanged
}

// repeatNotice is the visible banner prepended to a read_file result when the
// model re-reads a file whose content has not changed within the task. It names
// the repeat count and steers the model toward search_content so it stops
// re-reading whole files (the tool-loop token blow-up this addresses).
func repeatNotice(count int) string {
	return fmt.Sprintf(
		"⚠️ 本任务已第 %d 次读取此文件，内容与前次相同、未变化。该内容此前已在上文出现；"+
			"若只需其中某段，请改用 search_content 精确检索，避免重复整篇读取消耗上下文。\n\n",
		count,
	)
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/tool/ -run 'TestReadHistoryRecord|TestRepeatNoticeCarriesCount' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tool/builtin.go internal/tool/builtin_test.go
git commit -m "feat(tool): readHistory 记录任务内读取次数+内容哈希 + repeatNotice 文案"
```

---

### Task 2: 接入 readFileTool + registry 构造

**Files:**
- Modify: `internal/tool/builtin.go`（`workspaceRegistryOptions`、两个构造器、`readFileTool`）
- Test: `internal/tool/builtin_test.go`

**Interfaces:**
- Consumes: `newReadHistory`/`record`/`repeatNotice`（Task 1）。
- Produces: `workspaceRegistryOptions` 加 `readHistory *readHistory`；构造器 `NewWorkspaceRegistry`/`NewFileReadWriteWorkspaceRegistry` 内 `options.readHistory = newReadHistory()`（始终非 nil）；`readFileTool` 内容未变的重读前置 `repeatNotice`。

- [ ] **Step 1: Write the failing test**

```go
func TestReadFileFlagsUnchangedRepeat(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "hello world")
	reg := NewFileReadWriteWorkspaceRegistry(root, nil)

	call := domain.ToolCall{Name: "read_file", ID: "1", Arguments: map[string]string{"path": "a.txt"}}
	r1, err := reg.Invoke(t.Context(), call)
	if err != nil { t.Fatal(err) }
	if strings.Contains(r1.Output, "第") || !strings.Contains(r1.Output, "hello world") {
		t.Fatalf("first read must be plain content, got %q", r1.Output)
	}
	r2, err := reg.Invoke(t.Context(), domain.ToolCall{Name: "read_file", ID: "2", Arguments: map[string]string{"path": "a.txt"}})
	if err != nil { t.Fatal(err) }
	if !strings.Contains(r2.Output, "第 2 次") {
		t.Fatalf("repeat read must carry notice, got %q", r2.Output)
	}
	if !strings.Contains(r2.Output, "hello world") {
		t.Fatalf("repeat read must STILL contain full content (fail-loud), got %q", r2.Output)
	}
}

func TestReadFileNoNoticeWhenContentChanged(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	writeFile(t, p, "v1")
	reg := NewFileReadWriteWorkspaceRegistry(root, nil)
	if _, err := reg.Invoke(t.Context(), domain.ToolCall{Name: "read_file", ID: "1", Arguments: map[string]string{"path": "a.txt"}}); err != nil { t.Fatal(err) }
	writeFile(t, p, "v2-changed")
	r2, err := reg.Invoke(t.Context(), domain.ToolCall{Name: "read_file", ID: "2", Arguments: map[string]string{"path": "a.txt"}})
	if err != nil { t.Fatal(err) }
	if strings.Contains(r2.Output, "第") {
		t.Fatalf("changed content must NOT carry repeat notice, got %q", r2.Output)
	}
	if !strings.Contains(r2.Output, "v2-changed") {
		t.Fatalf("must return new content, got %q", r2.Output)
	}
}
```

（`reg.Invoke` 名以现有 Registry 分发方法为准——若为 `Dispatch`/`Call` 则相应改；`writeFile` helper 已在 builtin_test.go 存在。）

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/tool/ -run 'TestReadFileFlagsUnchangedRepeat|TestReadFileNoNoticeWhenContentChanged' -v`
Expected: FAIL — 重读无提醒 / `readHistory` 字段未定义 / nil deref。

- [ ] **Step 3: Implement**

1. `workspaceRegistryOptions` 结构体加字段：
```go
	readHistory  *readHistory
```
2. 两个构造器 `NewWorkspaceRegistry` 与 `NewFileReadWriteWorkspaceRegistry` 内，应用完 opts、注册描述符**之前**，加：
```go
	if options.readHistory == nil {
		options.readHistory = newReadHistory()
	}
```
（放在 `options.injected` seed 附近；确保 handler 闭包捕获到带 readHistory 的 options。）
3. `readFileTool` 内，把 `output := string(data)` 截断处理之后、`subtreeAgentsNote` 追加**之前**，插入：
```go
	if options.readHistory != nil {
		if count, unchanged := options.readHistory.record(resolved, output); unchanged {
			output = repeatNotice(count) + output
		}
	}
```
（`resolved` 是已解析绝对路径；record 基于文件内容 output，不含 agents note。）

- [ ] **Step 4: Run to verify pass + 全量门禁**

Run: `go test ./internal/tool/ -run 'ReadFile' -count=1 -v && go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l .`
Expected: 目标测试 PASS；全包全绿；gofmt 空。

- [ ] **Step 5: Commit**

```bash
git add internal/tool/builtin.go internal/tool/builtin_test.go
git commit -m "feat(tool): read_file 重读内容未变时前置可见提醒(全文仍返回)"
```

---

## Self-Review

- **Spec 覆盖**：触发键(path+hash)=Task1 record；仅 read_file=Task2 readFileTool；任务级状态=readHistory per-registry；提醒前置全文在后=Task2 Step3；内容永不丢=TestReadFileFlagsUnchangedRepeat 断言全文仍在；内容变不误伤=TestReadFileNoNoticeWhenContentChanged；per-task 隔离=每构造器 newReadHistory。均覆盖。
- **占位**：无；Task2 测试注明 `reg.Invoke` 以现有分发方法名为准（执行时对齐），非逻辑占位。
- **类型一致**：`readHistory`/`readEntry`/`record(path,content)(count,unchanged)`/`repeatNotice(count)`/`workspaceRegistryOptions.readHistory` 跨任务一致。
