# 工具结果落盘按会话隔离 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 工具结果落盘路径加 session 子目录（`toolRoot/.stardust/tool_results/<session>/<tool>-<hash>.md`），实现跨会话隔离——会话 B 的 agent 不再能读到会话 A 的落盘，且可按会话清理。

**Architecture:** runtime 层把 `appendToolResults` 的 `cacheDir` 从常量 `defaultToolResultCacheDir` 改为按 task 运行时拼接的 session 子目录（新 helper `sessionCacheDir`）。不改 `appendToolResults`/`writeToolResultCache`/`renderToolResultContent` 签名——`cacheDir` 参数透传，`filepath.Join` 自动含 session 段，`guard.Check` 仍验证。首版不主动清理（YAGNI）。

**Tech Stack:** Go；`path/filepath`、`strings`、`domain.Task.SessionID`。

**参考 spec:** `docs/superpowers/specs/2026-08-03-tool-result-cache-session-isolation-design.md`
**前置:** P1（统一落盘分页）已在 master；`toolcache.go` 有 `defaultToolResultCacheDir`/`writeToolResultCache`/`sanitizeToolName`。

**全局门槛（commit 前）：** 在 `legion/legionAgent` 目录 `go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 空。

---

## 文件结构

| 文件 | 动作 | 职责 |
|------|------|------|
| `internal/runtime/toolcache.go` | 修改 | 新增 `sessionCacheDir(task)` + `sanitizeCacheSegment(s)`；import 补 `domain` |
| `internal/runtime/runtime.go` | 修改 | `runToolLoop` 调用点 `defaultToolResultCacheDir` → `sessionCacheDir(task)` |
| `internal/runtime/toolcache_test.go` | 修改 | helper 单测 + 隔离/读回集成测 |

---

## Task 1: session 隔离落盘

**Files:**
- Modify: `internal/runtime/toolcache.go`
- Modify: `internal/runtime/runtime.go:516`
- Test: `internal/runtime/toolcache_test.go`

- [ ] **Step 1: 写 helper 单测 + 隔离集成测**

在 `internal/runtime/toolcache_test.go` 追加（文件已有 `package runtime` + imports os/path/filepath/strings/testing/log/slog；下面用到 `domain`，若测试文件未 import 则加 `"github.com/stardust/legion-agent/internal/domain"`）：

```go
func TestSessionCacheDir(t *testing.T) {
	cases := []struct {
		name string
		task domain.Task
		want string // relative, forward-slashed
	}{
		{"session id used", domain.Task{ID: "gui-task-1", SessionID: "session-abc"}, defaultToolResultCacheDir + "/session-abc"},
		{"fallback to task id", domain.Task{ID: "gui-task-1", SessionID: ""}, defaultToolResultCacheDir + "/gui-task-1"},
		{"both empty -> no-session", domain.Task{ID: "", SessionID: ""}, defaultToolResultCacheDir + "/no-session"},
		{"illegal chars sanitized", domain.Task{ID: "x", SessionID: "a/b c:d"}, defaultToolResultCacheDir + "/a-b-c-d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filepath.ToSlash(sessionCacheDir(tc.task))
			if got != tc.want {
				t.Fatalf("sessionCacheDir = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSanitizeCacheSegmentAllIllegal(t *testing.T) {
	if got := sanitizeCacheSegment("///"); got != "session" {
		t.Fatalf("all-illegal segment = %q, want session", got)
	}
}

// Two different sessions writing the SAME content must land in DIFFERENT
// directories (the isolation guarantee), and each file is read-backable.
func TestSessionIsolationDifferentDirsSameContent(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("X", 20000)
	dirA := sessionCacheDir(domain.Task{ID: "t1", SessionID: "sess-A"})
	dirB := sessionCacheDir(domain.Task{ID: "t2", SessionID: "sess-B"})

	relA, err := writeToolResultCache(root, dirA, "fetch_url", big)
	if err != nil {
		t.Fatalf("write A: %v", err)
	}
	relB, err := writeToolResultCache(root, dirB, "fetch_url", big)
	if err != nil {
		t.Fatalf("write B: %v", err)
	}
	if relA == relB {
		t.Fatalf("same content in different sessions must not share a path: %q", relA)
	}
	if !strings.Contains(relA, "sess-A") || !strings.Contains(relB, "sess-B") {
		t.Fatalf("paths not session-isolated: A=%q B=%q", relA, relB)
	}
	// Both files exist and hold the full content.
	for _, rel := range []string{relA, relB} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if len([]rune(string(data))) != 20000 {
			t.Fatalf("%s len = %d, want 20000", rel, len([]rune(string(data))))
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run "TestSessionCacheDir|TestSanitizeCacheSegmentAllIllegal|TestSessionIsolation" -v`
Expected: 编译失败（`sessionCacheDir`/`sanitizeCacheSegment` 未定义）

- [ ] **Step 3: 实现 helper（toolcache.go）**

在 `internal/runtime/toolcache.go` import 块加 `"github.com/stardust/legion-agent/internal/domain"`（`path/filepath`、`strings` 已在）。在 `sanitizeToolName` 附近追加：

```go
// sessionCacheDir returns the tool-result cache dir for a task, isolated by
// session: <defaultToolResultCacheDir>/<sanitized session key>. The key is
// task.SessionID, falling back to task.ID, then to "no-session" (a task always
// has an ID, so the final fallback guards the impossible rather than silently
// defaulting). Isolating by session keeps one session's cached tool output out
// of another session's file tools (list_files/read_file/search_content share
// the sandbox root) and makes per-session cleanup possible.
func sessionCacheDir(task domain.Task) string {
	key := strings.TrimSpace(task.SessionID)
	if key == "" {
		key = strings.TrimSpace(task.ID)
	}
	if key == "" {
		key = "no-session"
	}
	return filepath.Join(defaultToolResultCacheDir, sanitizeCacheSegment(key))
}

// sanitizeCacheSegment keeps only filename-safe chars from a path segment (a
// session or task id). IDs are already safe (e.g. "session-1785…",
// "gui-task-…"); this is a defensive guard mirroring sanitizeToolName. An
// all-illegal segment degrades to "session" rather than an empty path element.
func sanitizeCacheSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "session"
	}
	return out
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ -run "TestSessionCacheDir|TestSanitizeCacheSegmentAllIllegal|TestSessionIsolation" -v`
Expected: PASS

- [ ] **Step 5: 改 runToolLoop 调用点用 session 目录**

`internal/runtime/runtime.go` 的 `runToolLoop` 里（当前 runtime.go:516）：
```go
// 前
st.convo.appendToolResults(calls, results, r.maxToolResultChars, r.toolRoot, defaultToolResultCacheDir, r.logger)
// 后
st.convo.appendToolResults(calls, results, r.maxToolResultChars, r.toolRoot, sessionCacheDir(task), r.logger)
```
`task` 是 `runToolLoop` 的参数，直接可用。grep 确认 `runToolLoop` 内只有这一处 `appendToolResults` 调用（生产路径），且 `defaultToolResultCacheDir` 在此文件其余处不再被直接传入生产调用（测试仍用常量，不改）。

- [ ] **Step 6: 端到端读回测试（session 子目录落盘 + read_file 读回）**

在 `internal/runtime/toolcache_test.go` 追加（复用 P1 已有的 `footerCachePath` helper；`renderToolResultContent` 已存在）：

```go
func TestSessionScopedRenderReadBack(t *testing.T) {
	root := t.TempDir()
	dir := sessionCacheDir(domain.Task{ID: "t1", SessionID: "sess-X"})
	big := strings.Repeat("Y", 20000)
	out := renderToolResultContent("fetch_url", big, 4000, root, dir, slog.Default())

	if !strings.Contains(out, "tool_results/sess-X/") {
		t.Fatalf("footer path not session-scoped: %q", out)
	}
	rel := footerCachePath(t, out)
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read back session-scoped cache: %v", err)
	}
	if len([]rune(string(data))) != 20000 {
		t.Fatalf("read-back len = %d, want 20000", len([]rune(string(data))))
	}
}
```

- [ ] **Step 7: 跑测试确认通过**

Run: `go test ./internal/runtime/ -run "TestSession" -v`
Expected: PASS（含 TestSessionScopedRenderReadBack）

- [ ] **Step 8: 全量门槛**

Run: `go build ./... ; go vet ./... ; go test ./... ; gofmt -l .`
Expected: 构建成功、vet 无告警、全绿、gofmt 空

- [ ] **Step 9: Commit**

```bash
git add internal/runtime/toolcache.go internal/runtime/runtime.go internal/runtime/toolcache_test.go
git commit -m "feat(runtime): 工具结果落盘按会话隔离（tool_results/<session>/）"
```

---

## 自检结论（写计划者已核对）

- **Spec 覆盖**：session 子目录（§3 组件 = Step3/5）、空 fallback task_id→no-session（sessionCacheDir 逻辑 + TestSessionCacheDir）、sanitize（sanitizeCacheSegment + 测试）、隔离断言（TestSessionIsolation）、read_file 读回不变（TestSessionScopedRenderReadBack）、不主动清（本 plan 无清理任务，符合 YAGNI）。均有覆盖。
- **Placeholder 扫描**：无 TBD/TODO。Step5 "grep 确认只一处调用" 是明确核对指引。
- **类型/签名一致**：`sessionCacheDir(task domain.Task) string` / `sanitizeCacheSegment(s string) string` 定义与调用一致；`writeToolResultCache(root, cacheDir, tool, content)` / `renderToolResultContent(tool, content, max, root, cacheDir, logger)` 签名不变（只是 cacheDir 实参从常量变 helper 结果）；复用 P1 的 `footerCachePath` helper（toolcache_test.go 已有）。
- **实现锚点**：`toolcache.go` `defaultToolResultCacheDir`/`writeToolResultCache`/`sanitizeToolName`；`runtime.go:516` appendToolResults 调用点；`domain.Task.SessionID` @types.go:66；现有 `appendToolResults` 测试调用传常量 cacheDir **不改**。
