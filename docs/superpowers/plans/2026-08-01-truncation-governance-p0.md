# 工具截断治理 P0（截断自我描述）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 runtime 工具结果硬截断时输出**自我描述 footer**（明确"这是硬截、非数据/参数问题、重试无帮助、显示 N/共 M 字符"），消除 agent 把硬截误判为参数错而盲目重试的动机。

**Architecture:** 只改 `internal/runtime/runtime.go` 的 `truncateText` 一个纯函数——截断文本从 `…[truncated N chars]` 改为结构化自我描述 footer。不落盘、不改阈值（仍 `defaultMaxToolResultChars=4000` rune）、不动 tool-loop。P1（落盘分页）、P2（熔断）是后续独立 plan。

**Tech Stack:** Go；单元测试 `internal/runtime/messages_test.go`。

**参考 spec:** `docs/superpowers/specs/2026-08-01-tool-result-truncation-governance-design.md`（§5 P0）

**事故背景:** session-1785390610840955100 抓 K 线大数据被硬截 4000 rune，截断文本只说 `[truncated N chars]`，agent 误判为"数据太多"→换参数/换工具盲目重试 60 次→input 831.7k/291s→失败。

**全局门槛（commit 前）：** 在 `legion/legionAgent` 目录 `go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 空。

---

## 文件结构

| 文件 | 动作 | 职责 |
|------|------|------|
| `internal/runtime/runtime.go` | 修改 | `truncateText`（runtime.go:894）截断文本改为自我描述 footer |
| `internal/runtime/messages_test.go` | 修改 | 适配 `TestConversationTruncatesOversizedToolResult` 断言 + 新增 footer 内容断言 |

---

## Task 1: truncateText 自我描述 footer

**Files:**
- Modify: `internal/runtime/runtime.go:894-903`（`truncateText`）
- Test: `internal/runtime/messages_test.go`

现状 `truncateText`：
```go
func truncateText(text string, maxChars int) string {
	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	return string(runes[:maxChars]) + fmt.Sprintf("\n…[truncated %d chars]", len(runes)-maxChars)
}
```

现有测试 `messages_test.go:62 TestConversationTruncatesOversizedToolResult` 断言 `strings.Contains(msgs[2].Content, "truncated")`（小写）。新 footer 不含小写 `truncated`，故该断言必须同步改。

- [ ] **Step 1: 改测试——断言新 footer 的稳定关键词与字符计数**

把 `internal/runtime/messages_test.go` 的 `TestConversationTruncatesOversizedToolResult`（messages_test.go:62-74）整体替换为：

```go
func TestConversationTruncatesOversizedToolResult(t *testing.T) {
	t.Parallel()
	convo := newConversation("base", nil)
	calls := []domain.ToolCall{{ID: "c1", Name: "read_file"}}

	convo.appendAssistant("", calls)
	// 100 runes of content, truncated to 10 → footer must appear.
	convo.appendToolResults(calls, []domain.ToolResult{{CallID: "c1", Success: true, Output: strings.Repeat("x", 100)}}, 10)

	msgs := convo.render(0)
	content := msgs[2].Content
	// Self-describing footer: names the hard truncation and the shown/total counts.
	if !strings.Contains(content, "硬截断") {
		t.Fatalf("tool content = %q, want the hard-truncation self-description", content)
	}
	if !strings.Contains(content, "重试") {
		t.Fatalf("footer must tell the model retrying won't help, got %q", content)
	}
	// Shown count (10) and total count (100) must both appear so the model knows
	// how much it is missing.
	if !strings.Contains(content, "10") || !strings.Contains(content, "100") {
		t.Fatalf("footer must state shown/total rune counts, got %q", content)
	}
	// The preview head is still present.
	if !strings.HasPrefix(content, strings.Repeat("x", 10)) {
		t.Fatalf("preview head (first 10 runes) missing, got %q", content)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run TestConversationTruncatesOversizedToolResult -v`
Expected: FAIL（旧 footer 无 "硬截断"）

- [ ] **Step 3: 改 `truncateText` 为自我描述 footer**

把 `internal/runtime/runtime.go:894-903` 的 `truncateText` 整体替换为：

```go
// truncateText caps text at maxChars runes. When it truncates, it appends a
// self-describing footer stating this is a HARD truncation (a context-budget
// limit, not a data or parameter problem) so the model does not misread a cut
// result as "wrong arguments" and retry with different parameters — the failure
// mode that ran one task to 60 tool calls / 831.7k input (session-…955100). The
// footer names the shown and total rune counts so the model knows how much it is
// missing. maxChars<=0 disables truncation.
func truncateText(text string, maxChars int) string {
	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	total := len(runes)
	if total <= maxChars {
		return text
	}
	return string(runes[:maxChars]) + fmt.Sprintf(
		"\n\n──────── [输出被硬截断 / OUTPUT HARD-TRUNCATED] ────────\n"+
			"这是硬截断（上下文预算限制），非数据或参数问题——换参数或换工具重试不会有帮助。\n"+
			"This is a hard truncation (a context-budget limit), not a data/parameter problem; retrying with different arguments or tools will not help.\n"+
			"显示 %d / 共 %d 字符（rune）。\n",
		maxChars, total)
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ -run TestConversationTruncatesOversizedToolResult -v`
Expected: PASS

- [ ] **Step 5: 全量门槛**

Run: `go build ./... ; go vet ./... ; go test ./... ; gofmt -l .`
Expected: 构建成功、vet 无告警、测试全绿、gofmt 无输出

> 若其它测试断言旧 `[truncated N chars]` 文本而失败，grep `truncated` 定位并同步改为新 footer 的稳定关键词（`硬截断` / `HARD-TRUNCATED`）。P0 只应影响截断文本，不改行为语义。

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/runtime.go internal/runtime/messages_test.go
git commit -m "feat(runtime): 工具结果硬截断改为自我描述 footer（P0）"
```

---

## 自检结论（写计划者已核对）

- **Spec 覆盖（§5 P0）**：截断自我描述 footer（硬截声明 + 重试无帮助 + shown/total 字符数）= Task 1。P1 落盘 footer 追加、P2 熔断不在本 plan（后续 plan）。
- **Placeholder 扫描**：无 TBD/TODO；Step 5 的"若其它测试断言旧文本"是明确的 grep 补救指引，非占位。
- **一致性**：`truncateText(text, maxChars)` 签名不变；footer 关键词 `硬截断`/`HARD-TRUNCATED` 在测试与实现中一致；`maxChars`（shown）与 `total` 计数一致。
- **实现锚点**：`runtime.go:894 truncateText`（`defaultMaxToolResultChars=4000` @runtime.go:153，本 plan 不改）；`messages_test.go:62 TestConversationTruncatesOversizedToolResult`；`appendToolResults`（messages.go:63）调用 `truncateText`，本 plan 不改其签名。
