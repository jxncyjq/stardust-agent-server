# Harness 加固 P2：对话 LLM compaction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Steps use checkbox (`- [ ]`).

**Goal:** tool-loop 累计 token 超阈值时，把 basePrompt 之后、最近保留窗口之前的对话经一次 LLM 摘要成一条消息，遏制多轮累积膨胀；切点 tool-pair 边界安全，护 basePrompt 稳定前缀，LLM 失败不丢历史。

**Architecture:** 新增纯逻辑 `compactionSplit`(边界安全切分) + `conversationCompactor`(调 LLM 摘要并替换 convo)；`runToolLoop` 在 token 累计后按 `compactTokenThreshold` 触发；每任务压缩次数上限。

**Tech Stack:** Go 标准库 + 现有 `port.MaasInferenceClient`(摘要调用)。

## Global Constraints

- Fail-loud：LLM 摘要调用失败 → **不清空历史**，返回 error 或保留原对话并记 Warn，绝不把「压缩失败」当「历史清空」；切点计算不变式违反 fail-loud。
- **152-incident 硬约束**：压缩是摘要**旧**轮成一条可见的 `[对话摘要]` 消息，不是按 name/args 折叠隐藏重复；append-only 的当前窗口不动。
- **prompt-cache 稳定前缀**：`messages[0]`(basePrompt) 字节不动（它是 StablePrefixLen 断点）；摘要插在其后。
- **provider 兼容**：切点绝不落在 assistant(ToolCalls) 与其对应 RoleTool(ToolCallID) 之间——保留窗口不得以孤儿 RoleTool 开头。
- `go build/vet/test ./...` 全绿、`gofmt -l .` 空；公开/非导出符号 Go doc；错误路径有测试断言。
- 阈值 `compact_token_threshold=0` → **关闭**（可选配置，缺省即关，非兜底）。

## 现状 seam（实测）

- `internal/port/ports.go`：`InferenceMessage{Role, Content, Images, ToolCalls []domain.ToolCall, ToolCallID string}`。tool-pair = `RoleAssistant`+非空 `ToolCalls`，后跟若干 `RoleTool`(`ToolCallID` 匹配 call.ID)。
- `internal/runtime/messages.go`：`conversation.messages []port.InferenceMessage`；`messages[0]`=basePrompt(RoleUser)；`render(maxChars)` 只折叠 RoleTool。
- `internal/runtime/runtime.go`：`runToolLoop` 循环内 `st.promptTokens += st.resp.PromptTokens`（约 :492）累计；`loopState` 有 `convo *conversation`/`promptTokens int`；`r.maas` 是 `port.MaasInferenceClient`（`Generate(ctx, port.InferenceRequest)(port.InferenceResponse, error)`）。
- `internal/config/config.go`：`RuntimeConfig`（有 Debug/MaxToolRounds 等，参照加新字段 + normalize）。

---

### Task 1: config `compact_token_threshold` + plumb 到 Runtime

**Files:**
- Modify: `internal/config/config.go`（`RuntimeConfig` 加字段）
- Modify: `internal/runtime/runtime.go`（`Config`+`Runtime` 加字段+New 映射）
- Modify: `internal/runtime/agent_resolver.go`（serve 路径传入）
- Test: `internal/config/config_test.go`（或新建小测）

**Interfaces:**
- Produces：`RuntimeConfig.CompactTokenThreshold int json:"compact_token_threshold,omitempty"`（0=关）；`runtime.Config.CompactTokenThreshold int`；`Runtime.compactTokenThreshold int`。

- [ ] **Step 1: Write the failing test**
```go
// config 层：解析 compact_token_threshold
func TestRuntimeConfigCompactThresholdParses(t *testing.T) {
	var rc RuntimeConfig
	if err := json.Unmarshal([]byte(`{"compact_token_threshold":60000}`), &rc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rc.CompactTokenThreshold != 60000 {
		t.Fatalf("CompactTokenThreshold=%d want 60000", rc.CompactTokenThreshold)
	}
}
```

- [ ] **Step 2: Run to verify fail**
Run: `go test ./internal/config/ -run TestRuntimeConfigCompactThresholdParses -v`
Expected: FAIL — `CompactTokenThreshold undefined`。

- [ ] **Step 3: Implement**
- `config.go` `RuntimeConfig` 加：
```go
	// CompactTokenThreshold triggers conversation compaction when the tool loop's
	// accumulated prompt tokens exceed it. 0 disables compaction (default).
	CompactTokenThreshold int `json:"compact_token_threshold,omitempty"`
```
- `runtime.go` `Config` 加 `CompactTokenThreshold int`；`Runtime` 结构加 `compactTokenThreshold int`；`NewRuntime` 里映射 `compactTokenThreshold: cfg.CompactTokenThreshold`（0 直接透传，0=关，无需 normalize）。
- `agent_resolver.go` 的 `NewRuntime(Config{...})` 加 `CompactTokenThreshold: r.rootConfig.Runtime.CompactTokenThreshold`（serve/GUI 路径）。

- [ ] **Step 4: Run + build**
Run: `go test ./internal/config/ -run TestRuntimeConfigCompactThresholdParses -v && go build ./...`
Expected: PASS + build 绿。

- [ ] **Step 5: Commit**
```bash
git add internal/config/config.go internal/runtime/runtime.go internal/runtime/agent_resolver.go internal/config/config_test.go
git commit -m "feat(config): runtime.compact_token_threshold 开关 + plumb 到 Runtime"
```

---

### Task 2: compactionSplit — 边界安全切分（纯逻辑）

**Files:**
- Create: `internal/runtime/compaction.go`
- Test: `internal/runtime/compaction_test.go`

**Interfaces:**
- Produces：`func compactionSplit(msgs []port.InferenceMessage, preserveTail int) (compactStart, preserveStart int, ok bool)` —— 返回可压缩区间 `[compactStart, preserveStart)` 与保留起点 `preserveStart`：
  - `compactStart` 恒为 1（`msgs[0]`=basePrompt 钉住）。
  - 目标保留尾 = 最后 `preserveTail` 条；`preserveStart` 从 `len-preserveTail` 起，**向前(向 0)walk 到一个「安全边界」**：`msgs[preserveStart]` 必须不是 `RoleTool`（否则它是孤儿 tool result——其 assistant 落在压缩区），即 preserveStart 落在一条 assistant/user turn 起点。
  - `ok=false`（不值得压缩）当：`preserveStart <= compactStart`（压缩区空/仅 basePrompt）或 `len < 4`。

- [ ] **Step 1: Write the failing test**
```go
// compaction_test.go
package runtime
import (
	"testing"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)
func msg(role, content string, calls []domain.ToolCall, toolCallID string) port.InferenceMessage {
	return port.InferenceMessage{Role: role, Content: content, ToolCalls: calls, ToolCallID: toolCallID}
}
func TestCompactionSplitAvoidsOrphanToolResult(t *testing.T) {
	// 0:base(user) 1:asst(callA) 2:tool(A) 3:asst(callB) 4:tool(B) 5:asst(text) 6:user
	msgs := []port.InferenceMessage{
		msg(port.RoleUser, "base", nil, ""),
		msg(port.RoleAssistant, "", []domain.ToolCall{{ID: "A"}}, ""),
		msg(port.RoleTool, "resA", nil, "A"),
		msg(port.RoleAssistant, "", []domain.ToolCall{{ID: "B"}}, ""),
		msg(port.RoleTool, "resB", nil, "B"),
		msg(port.RoleAssistant, "answer", nil, ""),
		msg(port.RoleUser, "next", nil, ""),
	}
	// preserveTail=2 → 目标 preserveStart=5(asst answer)，本就是安全边界(非 tool)
	cs, ps, ok := compactionSplit(msgs, 2)
	if !ok || cs != 1 || ps != 5 {
		t.Fatalf("got cs=%d ps=%d ok=%v, want 1,5,true", cs, ps, ok)
	}
	// preserveTail=3 → 目标 len-3=4 是 RoleTool(B) 孤儿 → 向前 walk 到 3(asst callB)
	cs, ps, ok = compactionSplit(msgs, 3)
	if !ok || ps != 3 {
		t.Fatalf("preserveTail=3: ps=%d ok=%v, want ps=3 (walked back off orphan tool)", ps, ok)
	}
	// 太短不压缩
	if _, _, ok := compactionSplit(msgs[:3], 2); ok {
		t.Fatalf("len<4 should not compact")
	}
}
```

- [ ] **Step 2: Run to verify fail**
Run: `go test ./internal/runtime/ -run TestCompactionSplitAvoidsOrphanToolResult -v`
Expected: FAIL — `undefined: compactionSplit`。

- [ ] **Step 3: Implement**
```go
// compaction.go
package runtime
import "github.com/stardust/legion-agent/internal/port"

// compactionSplit computes the range of messages that may be summarised away.
// msgs[0] (the base prompt / stable cache prefix) is always pinned, so
// compactStart is always 1. preserveStart is the index at which the preserved
// recent tail begins: it starts at len-preserveTail and is walked backward until
// it lands on a turn boundary that is NOT a RoleTool message — a RoleTool at the
// tail boundary would be an orphan whose RoleAssistant tool_calls fell into the
// compacted range, which providers reject. ok is false when there is nothing
// worth compacting (fewer than 4 messages, or the preserved tail already covers
// everything after the base prompt).
func compactionSplit(msgs []port.InferenceMessage, preserveTail int) (compactStart, preserveStart int, ok bool) {
	compactStart = 1
	if len(msgs) < 4 || preserveTail < 1 {
		return 0, 0, false
	}
	preserveStart = len(msgs) - preserveTail
	if preserveStart < compactStart {
		preserveStart = compactStart
	}
	// Walk backward off any orphan RoleTool boundary (its assistant is earlier).
	for preserveStart > compactStart && msgs[preserveStart].Role == port.RoleTool {
		preserveStart--
	}
	if preserveStart <= compactStart {
		return 0, 0, false
	}
	return compactStart, preserveStart, true
}
```

- [ ] **Step 4: Run to verify pass + 边界补测**
```go
func TestCompactionSplitNothingToCompact(t *testing.T) {
	msgs := []port.InferenceMessage{
		msg(port.RoleUser, "base", nil, ""),
		msg(port.RoleAssistant, "a", nil, ""),
		msg(port.RoleUser, "b", nil, ""),
		msg(port.RoleAssistant, "c", nil, ""),
	}
	// preserveTail 覆盖 base 之后全部 → 无可压缩
	if _, _, ok := compactionSplit(msgs, 3); ok {
		t.Fatalf("preserveTail covering all-after-base should be ok=false")
	}
}
```
Run: `go test ./internal/runtime/ -run TestCompactionSplit -count=1 -v`
Expected: PASS。

- [ ] **Step 5: Commit**
```bash
git add internal/runtime/compaction.go internal/runtime/compaction_test.go
git commit -m "feat(runtime): compactionSplit 边界安全切分(钉 basePrompt+避孤儿 tool)"
```

---

### Task 3: conversationCompactor — LLM 摘要 + 替换 + fail-loud

**Files:**
- Modify: `internal/runtime/compaction.go`
- Modify: `internal/runtime/messages.go`（conversation 加 compact 替换方法）
- Test: `internal/runtime/compaction_test.go`

**Interfaces:**
- Consumes：`compactionSplit`（Task 2）、`port.MaasInferenceClient`、`conversation`。
- Produces：
  - `func summarizePrompt(msgs []port.InferenceMessage) string` —— 把待压消息拼成给摘要模型的输入文本（角色标注 + 内容）。
  - `func (c *conversation) applyCompaction(preserveStart int, summary string)` —— 替换 `c.messages = [msgs[0], {RoleUser, "[对话摘要]\n"+summary}, msgs[preserveStart:]...]`（**msgs[0] 不动**）。
  - `func (r *Runtime) compactConversation(ctx context.Context, convo *conversation) (bool, error)` —— 计算 split（preserveTail 用常量 `compactPreserveTail=8`）；`ok=false` 返回 (false,nil)；调 `r.maas.Generate` 摘要（用轻量请求：单条 user 消息 = summarizePrompt(待压消息)，无 tools）；成功 → `applyCompaction` 返回 (true,nil)；**LLM 失败 → 返回 (false, fmt.Errorf("compact summarize: %w", err))，不改 convo**。

- [ ] **Step 1: Write the failing test**（用 fake maas）
```go
type fakeSummaryMaas struct{ out string; err error; got port.InferenceRequest }
func (f *fakeSummaryMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	f.got = req
	if f.err != nil { return port.InferenceResponse{}, f.err }
	return port.InferenceResponse{Text: f.out}, nil
}
func TestCompactConversationReplacesAndPreservesBase(t *testing.T) {
	convo := &conversation{messages: []port.InferenceMessage{
		msg(port.RoleUser, "BASE-PROMPT", nil, ""),
		msg(port.RoleAssistant, "old1", nil, ""),
		msg(port.RoleUser, "old2", nil, ""),
		msg(port.RoleAssistant, "old3", nil, ""),
		msg(port.RoleUser, "old4", nil, ""),
		msg(port.RoleAssistant, "recentA", nil, ""),
		msg(port.RoleUser, "recentB", nil, ""),
	}}
	r := &Runtime{maas: &fakeSummaryMaas{out: "SUMMARY-TEXT"}}
	ok, err := r.compactConversation(t.Context(), convo)
	if err != nil || !ok { t.Fatalf("compact = (%v,%v), want (true,nil)", ok, err) }
	if convo.messages[0].Content != "BASE-PROMPT" {
		t.Fatalf("base prompt must be untouched, got %q", convo.messages[0].Content)
	}
	if convo.messages[1].Role != port.RoleUser || !strings.Contains(convo.messages[1].Content, "SUMMARY-TEXT") {
		t.Fatalf("msg[1] should be summary, got %+v", convo.messages[1])
	}
	// 消息数应下降
	if len(convo.messages) >= 7 { t.Fatalf("messages not reduced: %d", len(convo.messages)) }
}
func TestCompactConversationFailLoudKeepsHistory(t *testing.T) {
	orig := []port.InferenceMessage{
		msg(port.RoleUser,"BASE",nil,""), msg(port.RoleAssistant,"a",nil,""),
		msg(port.RoleUser,"b",nil,""), msg(port.RoleAssistant,"c",nil,""),
		msg(port.RoleUser,"d",nil,""), msg(port.RoleAssistant,"e",nil,""),
	}
	convo := &conversation{messages: append([]port.InferenceMessage(nil), orig...)}
	r := &Runtime{maas: &fakeSummaryMaas{err: fmt.Errorf("boom")}}
	ok, err := r.compactConversation(t.Context(), convo)
	if ok || err == nil { t.Fatalf("LLM 失败应 (false,err), got (%v,%v)", ok, err) }
	if len(convo.messages) != len(orig) { t.Fatalf("失败后历史被改动: %d != %d", len(convo.messages), len(orig)) }
}
```
（`preserveTail` 常量取 `compactPreserveTail`；上例 7 条、tail=8 会 ok=false——请把常量在测试可控，或测试用 len 足够长/tail 较小。实现时把 `compactPreserveTail` 设为常量并在测试里用能触发压缩的消息条数；若 8 太大，测试构造 ≥12 条消息，或把常量设小如 4 并据此调整断言。**执行者：先定 `compactPreserveTail` 值，再让测试消息条数 > 常量+2 以触发。**）

- [ ] **Step 2: Run to verify fail**
Run: `go test ./internal/runtime/ -run TestCompactConversation -v`
Expected: FAIL — `undefined: (*Runtime).compactConversation` 等。

- [ ] **Step 3: Implement**
```go
// compaction.go
const compactPreserveTail = 8 // recent messages kept verbatim (walked to safe boundary)

func summarizePrompt(msgs []port.InferenceMessage) string {
	var b strings.Builder
	b.WriteString("将以下对话历史压缩成简洁要点，保留关键事实、已获取的信息、未决事项，供后续继续对话参考：\n\n")
	for _, m := range msgs {
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func (r *Runtime) compactConversation(ctx context.Context, convo *conversation) (bool, error) {
	cs, ps, ok := compactionSplit(convo.messages, compactPreserveTail)
	if !ok {
		return false, nil
	}
	resp, err := r.maas.Generate(ctx, port.InferenceRequest{
		Messages: []port.InferenceMessage{{Role: port.RoleUser, Content: summarizePrompt(convo.messages[cs:ps])}},
	})
	if err != nil {
		return false, fmt.Errorf("compact summarize: %w", err)
	}
	convo.applyCompaction(ps, resp.Text)
	return true, nil
}
```
```go
// messages.go
// applyCompaction replaces messages[1:preserveStart] with a single summary user
// message, pinning messages[0] (the stable cache prefix) and keeping the recent
// tail from preserveStart verbatim.
func (c *conversation) applyCompaction(preserveStart int, summary string) {
	tail := append([]port.InferenceMessage(nil), c.messages[preserveStart:]...)
	c.messages = append([]port.InferenceMessage{
		c.messages[0],
		{Role: port.RoleUser, Content: "[对话摘要]\n" + summary},
	}, tail...)
}
```
（import `strings`/`fmt`/`context`/`port` 按需。）

- [ ] **Step 4: Run to verify pass**
Run: `go test ./internal/runtime/ -run TestCompactConversation -count=1 -v`
Expected: PASS（若因 `compactPreserveTail` 触发不了，按 Step1 注释调测试消息条数）。

- [ ] **Step 5: Commit**
```bash
git add internal/runtime/compaction.go internal/runtime/messages.go internal/runtime/compaction_test.go
git commit -m "feat(runtime): conversationCompactor LLM 摘要+替换(护 base, fail-loud 不丢史)"
```

---

### Task 4: 接入 runToolLoop（阈值触发 + 次数上限）+ example

**Files:**
- Modify: `internal/runtime/runtime.go`（loopState 加压缩计数 + runToolLoop 触发）
- Modify: `configs/agent.full.example.json`（runtime 段补 compact_token_threshold 说明）
- Test: `internal/runtime/runtime_test.go`

**Interfaces:**
- Consumes：`r.compactConversation`、`r.compactTokenThreshold`。
- Produces：`loopState` 加 `compactions int`（本任务已压次数）；常量 `maxCompactionsPerTask=3`；runToolLoop 在 token 累计后触发。

- [ ] **Step 1: Write the failing test**
```go
// 用 fake maas：让 tool-loop 跑几轮累积 promptTokens 超阈值，注入的 fake maas 摘要返回固定串；
// 断言压缩发生（convo 里出现 "[对话摘要]"）且压缩次数不超过 maxCompactionsPerTask。
// 若 threshold=0 则永不压缩。按 internal/runtime 既有 fake maas 测试模式实现。
func TestRunToolLoopCompactsOverThreshold(t *testing.T) {
	// 安排：compactTokenThreshold 设小(如 10)，fake maas 每轮返回带 tool_calls 使多轮，
	//       PromptTokens 每轮 >10 触发压缩；断言 convo 含 "[对话摘要]"。
	// 具体按既有 fake maas harness 写。
}
```

- [ ] **Step 2: Run to verify fail**
Run: `go test ./internal/runtime/ -run TestRunToolLoopCompactsOverThreshold -v`
Expected: FAIL（无触发逻辑）。

- [ ] **Step 3: Implement**
- `loopState` 加：`compactions int`（两处创建默认 0，无需显式）。常量（compaction.go 或 runtime.go）：`const maxCompactionsPerTask = 3`。
- `runToolLoop` 在现有 `st.promptTokens += st.resp.PromptTokens ... st.round++` **之后**、循环体末尾加触发：
```go
		if r.compactTokenThreshold > 0 && st.promptTokens > r.compactTokenThreshold && st.compactions < maxCompactionsPerTask {
			compacted, err := r.compactConversation(ctx, st.convo)
			if err != nil {
				// Fail-loud but non-fatal: keep the un-compacted history and press on;
				// a failed summary must never abort a task or drop context.
				r.logger.Warn("conversation compaction failed",
					"task_id", task.ID, "err", err)
			} else if compacted {
				st.compactions++
			}
		}
```
（放在 `st.round++` 后。注意：压缩改的是下一轮 render 的输入，本轮已发的 resp 不受影响。）

- `configs/agent.full.example.json` runtime 段补：
```json
    "_comment_compact_token_threshold": "对话 token 累计超此值触发一次 LLM 摘要压缩(摘要旧轮、保留最近若干、护稳定前缀)。0=关闭(默认)。建议 40000-80000。",
    "compact_token_threshold": 0
```

- [ ] **Step 4: Run + 全量门禁**
Run: `go test ./internal/runtime/ -run TestRunToolLoopCompactsOverThreshold -count=1 -v && go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l .`
Expected: 目标 PASS；全绿；gofmt 空；example JSON 合法(`python -c "import json;json.load(open('configs/agent.full.example.json'))"`)。

- [ ] **Step 5: Commit**
```bash
git add internal/runtime/runtime.go configs/agent.full.example.json internal/runtime/runtime_test.go
git commit -m "feat(runtime): runToolLoop 阈值触发对话 compaction(次数上限, fail-loud 非致命)"
```

---

## Self-Review

- **Spec 覆盖**（组件2）：阈值触发=Task1+Task4；边界安全切分=Task2 compactionSplit；LLM 摘要+替换+护 base=Task3；fail-loud 不丢史=Task3 test + Task4 Warn 非致命；次数上限=Task4；example=Task4。均覆盖。
- **占位**：Task4 测试体标注「按既有 fake maas harness 写」——因 runtime 测试 harness 因文件而异，执行时对齐（非逻辑占位；核心断言明确：出现 "[对话摘要]" + 次数上限 + threshold=0 不压）。Task3 测试注明按 `compactPreserveTail` 调消息条数。
- **类型一致**：`CompactTokenThreshold`/`compactTokenThreshold`/`compactionSplit(msgs,preserveTail)(cs,ps,ok)`/`compactPreserveTail`/`summarizePrompt`/`applyCompaction(preserveStart,summary)`/`compactConversation`/`maxCompactionsPerTask`/`loopState.compactions` 跨任务一致。

## 关联
memory：[[legion-token-multiround-debug-probe]]（多轮累积成本=本组件根治目标）、[[legion-tool-loop-multiturn]]（152 红线）。spec：`2026-07-28-harness-hardening-design.md` 组件2。
