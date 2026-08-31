# 会话事件日志 P2 —— 实现计划（发射点与屏障）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让真实任务把会话事件写进 P1 那条日志——在 `internal/runtime` 里发出 8 类事件，并在三个位置做 fail-closed 的落盘屏障。

**Architecture:** 五条任务入口（默认 runner、per-agent resolver、恢复、委派子任务、app 直调）**全部汇到 `Runtime.RunTask`**，所以发射点落在 `RunTask` / `runToolLoop` 这一个接缝上，而不是五个调用点各写一遍。事件经一个 `eventRecorder` 写入 P1 的 `port.SessionEventStore`；`Runtime` 没有配 store 时整个记录是 no-op（这是**契约声明的可选**，不是兜底——见 Task 1）。

**Tech Stack:** Go 1.26，复用 P1 的 `port.SessionEventStore` 与 `domain.SessionEvent`；无新依赖。

## Global Constraints

- Spec：`docs/superpowers/specs/2026-08-31-session-event-log-and-trajectory-design.md`（master）。本计划做其中的 **P2**。
- P1 已合入 master（`a431ce9`）：`session_events` 表、`Append`/`ReadFrom`/`Load`、六条不变量。**不要改 P1 的存储层语义**。
- **fail-loud 铁律**（`legionAgent/CLAUDE.md` §0）：禁止兜底/静默跳过/零值假装正常；错误 `fmt.Errorf("<动作> <标识>: %w", err)` 包装。
- 完成判据：`go build ./... && go vet ./... && go test ./... -count=1` 全绿，`gofmt -l $(git ls-files '*.go')` 为空。
- 每条不变量都要有断言且**变异可验红**——每个任务最后一步写明「删掉什么会让哪条测试红」，实现者必须真跑并把输出贴进报告。
- **P2 不包括**：投影、删 `conversation_turns`、FTS5、`/events` 端点、前端、G3 开关。本计划不得触碰这些。

### spec §4.3.1 的四条硬约束（P1 用实证换来的，P2 必须守住）

1. **每条记录过的 `tool/call` 都必须有结果事件**——工具失败、取消、被拒绝同样发 `tool/result{is_error:true}`。只有进程硬崩才允许留下未答调用（那时 `step/end` 也发不出，`Load` 追加到尾部恰好正确）。
2. 投影按 `call_id` 配对（P3 的事，P2 只需保证 call_id 的正确性）。
3. `Load` 只可对「确定没有活跃写入者」的会话调用——**P2 不得在任务执行路径上调用 `Load`**。
4. **同一 step 内未被应答的 `tool/call` 不得复用 `call_id`**（provider 的 call id 只保证单次响应内唯一）。

### 两个已拍板的决定

- **D-A**：`task.SessionID` 为空时，**用 `task.ID` 当会话号**。每个无会话任务自成一条短日志，轨迹一样看得到，且不需要任何特例分支。
- **D-B**：委派子任务用**子任务 ID 当会话号**（符合 spec F1：子任务写自己的日志，父日志只留那一次 `tool/call`+`tool/result`）。子任务 ID 已带父任务前缀，`ParentTaskIDForSubTask` 可解析，轨迹据此做层级下钻。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/runtime/eventlog.go`（新建） | `eventRecorder`：会话号解析、seq 分配、批量缓冲、三个屏障的 flush；八个 `record*` 方法 |
| `internal/runtime/eventlog_test.go`（新建） | 会话号解析、事件序列、屏障 fail-closed、`tool/result` 必发的断言 |
| `internal/runtime/runtime.go`（修改） | `Config` 加 `SessionEvents`；`RunTask` 建 recorder 并发 turn/user 事件；`runToolLoop` 发 step/assistant/tool/step-end；`Close`/收尾发 turn/end |
| `internal/runtime/delegation.go`（修改） | 子 Runtime 继承 `SessionEvents`（否则子任务一个字都不记） |
| `internal/cli/command.go`（修改） | 装配时把 SQLite 仓储注入 `Config.SessionEvents` |

`eventlog.go` 单独成文件：`runtime.go` 已经很大，而「什么时候发什么事件、什么时候必须落盘」是一组自成一体的规则。

---

### Task 1: eventRecorder 骨架与会话号解析

**Files:**
- Create: `internal/runtime/eventlog.go`
- Test: `internal/runtime/eventlog_test.go`

**Interfaces:**
- Consumes: `port.SessionEventStore`、`domain.SessionEvent`、`domain.Task`
- Produces: `newEventRecorder(store port.SessionEventStore, task domain.Task) *eventRecorder`；`(*eventRecorder).sessionID() string`；`(*eventRecorder).enabled() bool`

- [ ] **Step 1: 写失败的测试**

```go
package runtime

import (
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// 会话号的来源有两个，且**都不能是空**：一条写不进任何会话的事件等于没记。
//
// task.SessionID 是常态（server 与 CLI 都会填）；空的情况来自单次任务与委派子任务，
// 那时用 task.ID —— 每个这样的任务自成一条短日志，轨迹一样看得到，且不需要特例分支。
func TestTheSessionIDFallsBackToTheTaskID(t *testing.T) {
	t.Parallel()

	withSession := newEventRecorder(stubEventStore{}, domain.Task{ID: "t1", SessionID: "s1"})
	if got := withSession.sessionID(); got != "s1" {
		t.Errorf("sessionID() = %q, want %q", got, "s1")
	}

	withoutSession := newEventRecorder(stubEventStore{}, domain.Task{ID: "t1"})
	if got := withoutSession.sessionID(); got != "t1" {
		t.Errorf("sessionID() = %q, want the task id %q: 没有会话号的任务也要有自己的日志", got, "t1")
	}
}

// 两者都空 = 这条任务没有任何身份，写出来的事件谁也认不回去。fail-loud。
func TestARecorderWithNoIdentityIsRefused(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("既没有 SessionID 也没有 ID 的任务被接受了：写出来的事件谁也认不回去")
		}
	}()
	newEventRecorder(stubEventStore{}, domain.Task{})
}

// 没有配 store 的部署（内存后端、测试构造）不记事件。
//
// 这**不是兜底**：Config.SessionEvents 是契约里显式声明的可选项（见它的文档注释），
// 「没有配」是一种合法部署形态，与「配了但写不进去」是两回事——后者必须硬失败。
func TestNoStoreMeansNoRecording(t *testing.T) {
	t.Parallel()

	rec := newEventRecorder(nil, domain.Task{ID: "t1"})
	if rec.enabled() {
		t.Error("没有 store 却报告 enabled")
	}
}
```

同文件加桩：

```go
// stubEventStore 是一个什么都不做的 store，供不关心写入结果的用例使用。
type stubEventStore struct{}

func (stubEventStore) Append(context.Context, string, []domain.SessionEvent) error { return nil }
func (stubEventStore) ReadFrom(context.Context, string, int64) ([]domain.SessionEvent, error) {
	return nil, nil
}
func (stubEventStore) Load(context.Context, string) ([]domain.SessionEvent, error) { return nil, nil }
```

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/runtime/ -run "SessionID|NoIdentity|NoStore" -count=1`
Expected: FAIL，`undefined: newEventRecorder`。（确认输出里有 `--- FAIL` 或 build 失败，不是 `no tests to run`。）

- [ ] **Step 3: 写实现**

```go
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// eventRecorder 把一次任务执行写成会话事件日志（spec §5）。
//
// 它是**每次 RunTask 一个**：seq 游标、缓冲区都属于这一次执行，不跨任务共享。
// 真正的串行化与 seq 连续性由 P1 的 store 保证（它在事务内查 next-seq 并持 per-session
// 写锁），这里只负责「发什么、什么时候必须落盘」。
type eventRecorder struct {
	store   port.SessionEventStore
	session string

	mu      sync.Mutex
	pending []domain.SessionEvent
	nextSeq int64
	// seqKnown 表示 nextSeq 已经与库对齐过。第一次 flush 之前不知道库里走到哪，
	// 所以 seq 在 flush 时才最终确定（见 flush）。
	seqKnown bool
	turn     int
	step     int
}

// newEventRecorder 建一次任务执行的记录器。
//
// 会话号取 task.SessionID，为空时退到 task.ID（决定 D-A：单次任务与委派子任务没有
// 会话号，让它们各自成为一条短日志，比加特例分支更简单，轨迹也一样看得到）。
// 两者都空说明这条任务没有任何身份——写出来的事件谁也认不回去，直接 panic：
// 这是编程错误，不是运行期状况。
func newEventRecorder(store port.SessionEventStore, task domain.Task) *eventRecorder {
	session := task.SessionID
	if session == "" {
		session = task.ID
	}
	if session == "" {
		panic("runtime: event recorder needs a session id or a task id; a task with neither cannot own a log")
	}
	return &eventRecorder{store: store, session: session}
}

// sessionID 是这次执行写入的会话日志。
func (e *eventRecorder) sessionID() string { return e.session }

// enabled 说明这个部署是否记录会话事件。
//
// 没有配 store 是**契约允许的可选**（见 Config.SessionEvents），不是错误：内存后端与
// 大量测试构造都不配。它与「配了但写不进去」是两回事——后者由 flush 硬失败。
func (e *eventRecorder) enabled() bool { return e != nil && e.store != nil }
```

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/runtime/ -run "SessionID|NoIdentity|NoStore" -count=1 -v`
Expected: 三条全 PASS（输出里真有三行 `--- PASS`）

- [ ] **Step 5: 变异验证**

把 `newEventRecorder` 里的 `if session == "" { panic(...) }` 删掉 → `TestARecorderWithNoIdentityIsRefused` 必须红。验完**恢复**并 `git status` 确认干净。

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/eventlog.go internal/runtime/eventlog_test.go
git commit -m "feat(runtime): 会话事件记录器的骨架与会话号解析"
```

---

### Task 2: 八个 record 方法与 flush

**Files:**
- Modify: `internal/runtime/eventlog.go`
- Modify: `internal/runtime/eventlog_test.go`

**Interfaces:**
- Consumes: Task 1 的 `eventRecorder`
- Produces: `recordTurnStart(turn int)`、`recordUserMessage(content string)`、`recordStepStart()`、`recordAssistantMessage(content string, calls []domain.ToolCall, usage eventUsage, profile string)`；`eventUsage{Prompt, Completion, Cached, Total int}`、`recordToolCall(call domain.ToolCall)`、`recordToolResult(callID string, preview string, isError bool, dur time.Duration)`、`recordStepEnd(reason string)`、`recordTurnEnd(reason string)`；以及 `flush(ctx context.Context) error`

**注意（已核实，不要再猜）**：这个仓**没有** `domain.TokenUsage` 这种聚合类型——用量是四个独立的 `int`（`PromptTokens` / `CompletionTokens` / `CachedTokens` / `TotalTokens`），分别挂在 `domain.TaskRun`、`domain.RuntimeEvent`、`domain.ConversationTurn` 等结构上；`runtime.go:560` 附近从模型响应 `resp.PromptTokens` 等处取。所以本任务在 `eventlog.go` 里定义一个**本地**的小结构承载它们（不要往 `domain` 加新类型，P2 没有那个必要）：

```go
// eventUsage 是一次模型响应的 token 用量。
//
// 本地定义而不是复用某个领域结构：那些结构（TaskRun/RuntimeEvent/ConversationTurn）
// 各自还带着一堆与事件无关的字段，让事件记录去依赖它们会把两件事绑死。
type eventUsage struct {
	Prompt     int
	Completion int
	Cached     int
	Total      int
}
```

- [ ] **Step 1: 写失败的测试**

```go
// 事件序列的形状（spec §5）：一次带一轮工具调用的执行应当产出
// turn/start → user/message → step/start → assistant/message → tool/call → tool/result
// → step/end → turn/end，且 seq 连续、turn/step 编号正确。
func TestOneRoundProducesTheExpectedSequence(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rec := newEventRecorder(store, domain.Task{ID: "t1", SessionID: "s1"})
	ctx := context.Background()

	rec.recordTurnStart(0)
	rec.recordUserMessage("hello")
	rec.recordStepStart()
	rec.recordAssistantMessage("working", []domain.ToolCall{{ID: "c1", Name: "read_file"}}, eventUsage{}, "default")
	rec.recordToolCall(domain.ToolCall{ID: "c1", Name: "read_file"})
	rec.recordToolResult("c1", "ok", false, time.Millisecond)
	rec.recordStepEnd(domain.StepEndReasonCompleted)
	rec.recordTurnEnd(domain.TurnEndReasonCompleted)
	if err := rec.flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	want := []domain.SessionEventType{
		domain.SessionEventTurnStart, domain.SessionEventUserMessage,
		domain.SessionEventStepStart, domain.SessionEventAssistantMessage,
		domain.SessionEventToolCall, domain.SessionEventToolResult,
		domain.SessionEventStepEnd, domain.SessionEventTurnEnd,
	}
	got := store.types()
	if len(got) != len(want) {
		t.Fatalf("事件序列 = %v，want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条是 %s，want %s（完整序列 %v）", i, got[i], want[i], got)
		}
	}
	for i, e := range store.events {
		if e.Seq != int64(i) {
			t.Errorf("第 %d 条的 seq 是 %d：seq 必须连续", i, e.Seq)
		}
	}
}

// step 编号每 turn 内从 0 重置，turn 编号每会话单调（spec §4.1 取值约定）。
func TestStepNumbersResetPerTurn(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rec := newEventRecorder(store, domain.Task{ID: "t1", SessionID: "s1"})

	rec.recordTurnStart(0)
	rec.recordStepStart() // turn 0 step 0
	rec.recordStepEnd(domain.StepEndReasonCompleted)
	rec.recordStepStart() // turn 0 step 1
	rec.recordStepEnd(domain.StepEndReasonCompleted)
	rec.recordTurnEnd(domain.TurnEndReasonCompleted)
	rec.recordTurnStart(1)
	rec.recordStepStart() // turn 1 step 0 —— 必须回到 0
	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var steps []int
	for _, e := range store.events {
		if e.Type == domain.SessionEventStepStart {
			steps = append(steps, intFieldOf(t, e, "step"))
		}
	}
	if len(steps) != 3 || steps[0] != 0 || steps[1] != 1 || steps[2] != 0 {
		t.Errorf("step 编号 = %v，want [0 1 0]：step 每 turn 内重置", steps)
	}
}

// 大载荷不进事件（P1 不变量 6 在发射侧的对应物）：预览按上限截断，
// 事件里不放工具输出全文。
func TestAToolResultCarriesAPreviewNotTheWholeOutput(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rec := newEventRecorder(store, domain.Task{ID: "t1", SessionID: "s1"})
	huge := strings.Repeat("x", maxEventPreviewRunes*3)

	rec.recordToolResult("c1", huge, false, time.Millisecond)
	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	preview := stringFieldOf(t, store.events[0], "preview")
	if utf8.RuneCountInString(preview) > maxEventPreviewRunes {
		t.Errorf("预览有 %d 个 rune，超过上限 %d：事件表会随工具输出体积膨胀",
			utf8.RuneCountInString(preview), maxEventPreviewRunes)
	}
	if strings.Contains(preview, huge) {
		t.Error("预览里塞进了完整输出")
	}
}
```

补测试辅助（同文件）：

```go
// captureEventStore 记下被 Append 的事件，供断言序列与载荷。
type captureEventStore struct {
	mu     sync.Mutex
	events []domain.SessionEvent
	err    error // 非 nil 时 Append 失败，供屏障测试用
}

func (c *captureEventStore) Append(_ context.Context, _ string, events []domain.SessionEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.events = append(c.events, events...)
	return nil
}

func (c *captureEventStore) ReadFrom(context.Context, string, int64) ([]domain.SessionEvent, error) {
	return nil, nil
}
func (c *captureEventStore) Load(context.Context, string) ([]domain.SessionEvent, error) {
	return nil, nil
}

func (c *captureEventStore) types() []domain.SessionEventType {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]domain.SessionEventType, 0, len(c.events))
	for _, e := range c.events {
		out = append(out, e.Type)
	}
	return out
}

func intFieldOf(t *testing.T, e domain.SessionEvent, name string) int {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(e.Data, &payload); err != nil {
		t.Fatalf("unmarshal %s: %v", e.Type, err)
	}
	value, ok := payload[name].(float64)
	if !ok {
		t.Fatalf("%s 的载荷里没有数值字段 %q", e.Type, name)
	}
	return int(value)
}

func stringFieldOf(t *testing.T, e domain.SessionEvent, name string) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(e.Data, &payload); err != nil {
		t.Fatalf("unmarshal %s: %v", e.Type, err)
	}
	value, ok := payload[name].(string)
	if !ok {
		t.Fatalf("%s 的载荷里没有字符串字段 %q", e.Type, name)
	}
	return value
}
```

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/runtime/ -run "ExpectedSequence|StepNumbers|PreviewNotTheWhole" -count=1`
Expected: FAIL，未定义的方法

- [ ] **Step 3: 写实现**

追加到 `internal/runtime/eventlog.go`：

```go
// maxEventPreviewRunes 是事件里预览文本的上限。
//
// 它守的是 spec §4.3 不变量 6 在发射侧的对应物：事件表的增长与调用次数成正比，
// 不与工具输出体积成正比。全文仍由既有的截断治理落盘（toolcache.go），事件里只留
// 这段预览。按 rune 而不是 byte 计，与该仓其余截断口径一致（中文一个字符 3 字节，
// 按 byte 截会把预览砍成三分之一）。
const maxEventPreviewRunes = 2000

// append 把一条事件放进缓冲。seq 在 flush 时统一分配（见 flush）。
//
// 记录器没启用时直接丢弃：没有配 store 是契约允许的部署形态。
func (e *eventRecorder) append(typ domain.SessionEventType, payload map[string]any) {
	if !e.enabled() {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		// 载荷是本文件自己构造的 map，编不出 JSON 属编程错误。
		panic(fmt.Sprintf("runtime: marshal %s payload: %v", typ, err))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending = append(e.pending, domain.SessionEvent{Type: typ, Time: time.Now(), Data: data})
}

// recordTurnStart 记一个轮次的开始，并把 step 计数归零（spec §4.1：step 每 turn 重置）。
func (e *eventRecorder) recordTurnStart(turn int) {
	e.mu.Lock()
	e.turn, e.step = turn, 0
	e.mu.Unlock()
	e.append(domain.SessionEventTurnStart, map[string]any{"turn": turn})
}

// recordUserMessage 记这一轮的用户输入。
func (e *eventRecorder) recordUserMessage(content string) {
	e.append(domain.SessionEventUserMessage, map[string]any{
		"turn": e.currentTurn(), "content": truncateRunes(content, maxEventPreviewRunes),
	})
}

// recordStepStart 记一次模型请求的开始。
func (e *eventRecorder) recordStepStart() {
	e.append(domain.SessionEventStepStart, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(),
	})
}

// recordAssistantMessage 记模型响应（含它请求的工具调用与 token 用量）。
func (e *eventRecorder) recordAssistantMessage(content string, calls []domain.ToolCall, usage eventUsage, profile string) {
	names := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		names = append(names, map[string]any{"call_id": c.ID, "name": c.Name})
	}
	e.append(domain.SessionEventAssistantMessage, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(),
		"content": truncateRunes(content, maxEventPreviewRunes), "tool_calls": names,
		"usage": map[string]any{
			"prompt": usage.Prompt, "completion": usage.Completion,
			"cached": usage.Cached, "total": usage.Total,
		},
		"model_profile": profile,
	})
}

// recordToolCall 记一次工具调用**被派发之前**的事实（spec §5 屏障 2 的前提）。
func (e *eventRecorder) recordToolCall(call domain.ToolCall) {
	arguments, err := json.Marshal(call.Arguments)
	if err != nil {
		panic(fmt.Sprintf("runtime: marshal tool call arguments for %s: %v", call.ID, err))
	}
	e.append(domain.SessionEventToolCall, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(),
		"call_id": call.ID, "name": call.Name,
		"arguments": truncateRunes(string(arguments), maxEventPreviewRunes),
	})
}

// recordToolResult 记一次工具调用的结果。
//
// **每条记录过的 tool/call 都必须有它**（spec §4.3.1 第 1 条）：工具失败、取消、被
// 拒绝一样要发，`isError` 为真。少发一条，恢复时会把它当成「崩在工具里」而补一条
// 合成结果，日志就与真实发生的事不符了。
func (e *eventRecorder) recordToolResult(callID string, preview string, isError bool, dur time.Duration) {
	e.append(domain.SessionEventToolResult, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(),
		"call_id": callID, "preview": truncateRunes(preview, maxEventPreviewRunes),
		"is_error": isError, "duration_ms": dur.Milliseconds(),
	})
}

// recordStepEnd 记一步的结束，并把 step 计数推进一格。
func (e *eventRecorder) recordStepEnd(reason string) {
	e.append(domain.SessionEventStepEnd, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(), "reason": reason,
	})
	e.mu.Lock()
	e.step++
	e.mu.Unlock()
}

// recordTurnEnd 记一个轮次的结束。
func (e *eventRecorder) recordTurnEnd(reason string) {
	e.append(domain.SessionEventTurnEnd, map[string]any{
		"turn": e.currentTurn(), "reason": reason,
	})
}

func (e *eventRecorder) currentTurn() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.turn
}

func (e *eventRecorder) currentStep() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.step
}

// flush 把缓冲里的事件落盘（spec §5 的三个屏障调它）。
//
// seq 在这里才分配：P1 的 store 要求首个 seq 等于库里的 next-seq，而库里走到哪只有
// 这一刻才知道（同一会话可能有别的写入者）。第一次 flush 用 ReadFrom 对齐游标，
// 之后按本次执行自己写过的条数递增。
//
// **失败就是失败**：调用方（屏障）据此决定不发请求、不进工具体、不开下一步。
// 缓冲保持不变，让调用方能在重试时不丢事件。
func (e *eventRecorder) flush(ctx context.Context) error {
	if !e.enabled() {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.pending) == 0 {
		return nil
	}
	if !e.seqKnown {
		existing, err := e.store.ReadFrom(ctx, e.session, 0)
		if err != nil {
			return fmt.Errorf("align session event cursor for %q: %w", e.session, err)
		}
		e.nextSeq = int64(len(existing))
		e.seqKnown = true
	}
	batch := make([]domain.SessionEvent, len(e.pending))
	for i, event := range e.pending {
		event.Seq = e.nextSeq + int64(i)
		batch[i] = event
	}
	if err := e.store.Append(ctx, e.session, batch); err != nil {
		return fmt.Errorf("persist session events for %q: %w", e.session, err)
	}
	e.nextSeq += int64(len(batch))
	e.pending = e.pending[:0]
	return nil
}

// truncateRunes 按 rune 截断并标注截断量，使读的人知道自己看的是一段而不是全部。
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + fmt.Sprintf("\n…[truncated: %d of %d runes shown]", limit, len(runes))
}
```

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/runtime/ -run "ExpectedSequence|StepNumbers|PreviewNotTheWhole|SessionID|NoIdentity|NoStore" -count=1 -v`
Expected: 六条全 PASS

- [ ] **Step 5: 变异验证（三条）**

1. 把 `recordTurnStart` 里的 `e.step = 0` 删掉 → `TestStepNumbersResetPerTurn` 红
2. 把 `recordToolResult` 的 `truncateRunes(preview, ...)` 换成 `preview` → `TestAToolResultCarriesAPreviewNotTheWholeOutput` 红
3. 把 `flush` 里的 seq 分配改成恒 0 → `TestOneRoundProducesTheExpectedSequence` 的 seq 断言红

每条验完**恢复**并 `git status` 确认干净。

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/eventlog.go internal/runtime/eventlog_test.go
git commit -m "feat(runtime): 八类会话事件的记录与批量落盘"
```

---

### Task 3: 三个 fail-closed 屏障

**Files:**
- Modify: `internal/runtime/eventlog.go`
- Modify: `internal/runtime/eventlog_test.go`

**Interfaces:**
- Consumes: Task 2 的 `flush`
- Produces: `(*eventRecorder).barrier(ctx context.Context, at string) error`

- [ ] **Step 1: 写失败的测试**

```go
// 三个屏障都 fail-closed（spec §5）：刷不动就不发请求、不进工具体、不开下一步。
//
// 屏障 2 是这条设计的支点：tool/call 必须先落盘，否则崩在工具体里就成了
// 「工具真的执行过、但日志里没有这次调用」——恢复时补不出那条合成结果，
// 而工具是有外部副作用的那一端。
func TestABarrierFailsClosed(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{err: errors.New("disk on fire")}
	rec := newEventRecorder(store, domain.Task{ID: "t1", SessionID: "s1"})
	rec.recordToolCall(domain.ToolCall{ID: "c1", Name: "write_file"})

	err := rec.barrier(context.Background(), "before dispatching a tool")
	if err == nil {
		t.Fatal("落盘失败时屏障放行了：工具会在没有记录的情况下产生副作用")
	}
	if !strings.Contains(err.Error(), "before dispatching a tool") {
		t.Errorf("错误里没说是哪个屏障：%v", err)
	}
	if !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("错误没有包住底层原因：%v", err)
	}
}

// 屏障失败之后事件仍在缓冲里：调用方若重试，不该丢掉已经发生的事实。
func TestAFailedBarrierKeepsTheEventsBuffered(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{err: errors.New("disk on fire")}
	rec := newEventRecorder(store, domain.Task{ID: "t1", SessionID: "s1"})
	rec.recordToolCall(domain.ToolCall{ID: "c1", Name: "write_file"})
	_ = rec.barrier(context.Background(), "before dispatching a tool")

	store.mu.Lock()
	store.err = nil
	store.mu.Unlock()
	if err := rec.barrier(context.Background(), "retry"); err != nil {
		t.Fatalf("重试仍失败：%v", err)
	}
	if got := len(store.events); got != 1 {
		t.Errorf("重试后落盘了 %d 条，want 1：屏障失败时把事件丢了", got)
	}
}

// 没有配 store 的部署里，屏障永远放行——它守的是「记录不上就别做」，
// 而不是「必须有记录才能做」。
func TestABarrierIsANoOpWithoutAStore(t *testing.T) {
	t.Parallel()

	rec := newEventRecorder(nil, domain.Task{ID: "t1"})
	if err := rec.barrier(context.Background(), "before the model request"); err != nil {
		t.Errorf("没有 store 的部署被屏障挡住了：%v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/runtime/ -run Barrier -count=1`
Expected: FAIL，`rec.barrier undefined`

- [ ] **Step 3: 写实现**

```go
// barrier 是一个 fail-closed 的落盘点（spec §5）。
//
// 三处调用：模型请求前、进工具体前、开下一步前。刷不动就返回错误，调用方**不做那件事**。
// 这是行为改变——数据库写不动时任务会失败而不是照跑——理由在屏障 2：tool/call 必须
// 先落盘，否则崩在工具体里就成了「工具真的执行过、但日志里没有这次调用」，恢复时补不出
// 合成结果，而工具正是有外部副作用的那一端。「先记录再执行」保证任何真发生过的副作用
// 在日志里都有它的调用。
//
// at 是这个屏障的位置，进错误信息——排查的人要立刻知道是哪一处挡住了。
func (e *eventRecorder) barrier(ctx context.Context, at string) error {
	if err := e.flush(ctx); err != nil {
		return fmt.Errorf("session event barrier %s: %w", at, err)
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/runtime/ -run Barrier -count=1 -v`
Expected: 三条全 PASS

- [ ] **Step 5: 变异验证（两条）**

1. 把 `barrier` 改成永远返回 nil → `TestABarrierFailsClosed` 红
2. 把 `flush` 失败路径改成清空缓冲（`e.pending = e.pending[:0]` 挪到 Append 之前）→ `TestAFailedBarrierKeepsTheEventsBuffered` 红

验完恢复。

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/eventlog.go internal/runtime/eventlog_test.go
git commit -m "feat(runtime): fail-closed 的落盘屏障"
```

---

### Task 4: 接进 RunTask 与 runToolLoop

**Files:**
- Modify: `internal/runtime/runtime.go`
- Modify: `internal/runtime/delegation.go`
- Test: `internal/runtime/eventlog_integration_test.go`（新建）

**Interfaces:**
- Consumes: Task 1-3 的 `eventRecorder`
- Produces: `Config.SessionEvents port.SessionEventStore`；`Runtime.events *eventRecorder` 的接线

**这是本期风险最高的一步**：它动的是所有任务都要走的那条路。

- [ ] **Step 1: 写失败的测试**

```go
// 一次真实的 RunTask 应当在日志里留下完整且平衡的事件序列（spec §9 的 P2 判据）。
//
// 「平衡」的判据不是条数，而是：每个 tool/call 都有同 call_id 的 tool/result，
// 且 turn 以非 interrupted 收尾——interrupted 只由崩溃恢复补出，正常执行绝不该产生。
func TestRunTaskWritesABalancedEventLog(t *testing.T) {
	store := &captureEventStore{}
	rt := newTestRuntimeWithEvents(t, store) // 见下方辅助：假模型先答一次工具调用，再答完成

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "读一下那个文件"}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	answered := map[string]bool{}
	sawTurnEnd := false
	for _, e := range store.events {
		switch e.Type {
		case domain.SessionEventToolCall:
			id := stringFieldOf(t, e, "call_id")
			if _, seen := answered[id]; !seen {
				answered[id] = false
			}
		case domain.SessionEventToolResult:
			answered[stringFieldOf(t, e, "call_id")] = true
		case domain.SessionEventTurnEnd:
			sawTurnEnd = true
			if reason := stringFieldOf(t, e, "reason"); reason == domain.TurnEndReasonInterrupted {
				t.Errorf("正常执行产出了 interrupted：那是崩溃恢复才该补的记号")
			}
		}
	}
	if len(answered) == 0 {
		t.Fatal("整条日志里没有任何 tool/call：发射点没接上")
	}
	for id, ok := range answered {
		if !ok {
			t.Errorf("call %q 没有对应的 tool/result：spec §4.3.1 第 1 条要求每条调用都有结果", id)
		}
	}
	if !sawTurnEnd {
		t.Error("没有 turn/end：日志不平衡，下次打开会被恢复逻辑当成中断")
	}
}

// 工具失败时**照样**要发 tool/result（is_error 为真）——spec §4.3.1 第 1 条。
// 少发一条，恢复时会把它当成「崩在工具里」补一条合成结果，日志与真实发生的事不符。
func TestAFailingToolStillGetsAResultEvent(t *testing.T) {
	store := &captureEventStore{}
	rt := newTestRuntimeWithFailingTool(t, store)

	_, _ = rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "跑那个会失败的工具"})

	var results int
	for _, e := range store.events {
		if e.Type == domain.SessionEventToolResult {
			results++
			if !boolFieldOf(t, e, "is_error") {
				t.Error("失败的工具产出了 is_error:false 的结果")
			}
		}
	}
	if results == 0 {
		t.Fatal("工具失败时一条 tool/result 都没发：恢复会把它当成崩在工具里")
	}
}
```

**辅助函数**：`newTestRuntimeWithEvents` / `newTestRuntimeWithFailingTool` 要复用 `internal/runtime` 里已有的测试构造方式。**动手前先 `grep -n "func newTestRuntime\|fakeInference\|stubInference" internal/runtime/*_test.go`**，照既有的假模型/假工具写法搭，不要另起一套。

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/runtime/ -run "BalancedEventLog|FailingToolStillGets" -count=1`
Expected: FAIL

- [ ] **Step 3: 写实现**

1. `Config` 加字段（放在既有字段旁，注释说明它是**契约声明的可选**）：

```go
	// SessionEvents 是会话事件日志的落点（P1 的 port.SessionEventStore）。
	//
	// 允许为 nil，且 nil 是一种**契约声明的合法部署形态**（内存后端、绝大多数测试
	// 构造），不是兜底：那时整个记录是 no-op，三个屏障永远放行。它与「配了但写不进去」
	// 是两回事——后者由屏障 fail-closed 挡住。
	SessionEvents port.SessionEventStore
```

2. `Runtime` 存下它（字段 `sessionEvents port.SessionEventStore`），并在 `newSubRuntime` 里**一并传给子 Runtime**——不传的话委派子任务一个字都不记。

3. `RunTask` 开头建 recorder 并发 turn/user 事件；每次进 `runToolLoop` 的循环体发 step/start 并过屏障 1；模型响应后发 assistant/message；每条调用派发前发 tool/call 并过**屏障 2**；工具返回后发 tool/result（失败也发）；循环体结束发 step/end（放在 `defer` 里，失败/取消也发）并过屏障 3；`RunTask` 收尾发 turn/end 并最后 flush 一次。

**turn 编号**：`RunTask` 一次执行 = 一个 turn。它的编号要跨会话单调（spec §4.1）——用 recorder 首次 flush 时对齐游标那次读到的既有事件里最大的 turn + 1；读不到就从 0 起。

**子任务**（决定 D-B）：`delegation.go` 里构造子任务 `domain.Task` 时不设 `SessionID`，于是 recorder 自动用子任务 ID 当会话号（Task 1 的 D-A 规则），子任务写自己的日志，父日志只留那一次 `tool/call`+`tool/result`——正是 spec F1 要的形状。**不需要额外代码**，但要在 `delegation.go` 的相应位置写一句注释说明这是有意为之。

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/runtime/ -count=1 -timeout 15m`
Expected: 全 PASS（含既有测试——这一步动的是所有任务的公共路径，既有测试是它的安全网）

- [ ] **Step 5: 变异验证（三条）**

1. 把屏障 2（tool/call 之后那次）删掉 → 需要一条测试红。**如果没有测试红，说明屏障 2 缺覆盖**——补一条：让 store 在第一次 Append 时失败，断言工具体**没有被执行**（用一个会留痕的假工具，断言痕迹不存在）。这条比"返回了 error"强得多。
2. 把 `newSubRuntime` 里传 `SessionEvents` 那行删掉 → 需要一条断言子任务事件的测试红。没有就补。
3. 把失败工具的 `recordToolResult` 删掉 → `TestAFailingToolStillGetsAResultEvent` 红

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/runtime.go internal/runtime/delegation.go internal/runtime/eventlog_integration_test.go
git commit -m "feat(runtime): 任务执行写出会话事件，三个屏障 fail-closed"
```

---

### Task 5: 装配接线与真机验证

**Files:**
- Modify: `internal/cli/command.go`（把 SQLite 仓储注入 `Config.SessionEvents`）
- Test: `internal/cli/session_events_wiring_test.go`（新建）

**Interfaces:**
- Consumes: Task 4 的 `Config.SessionEvents`
- Produces: 装配处的接线 + 一条「它确实被接上了」的断言

**这一步专治本仓栽过两次的毛病**：接缝在、但没人调用它。

- [ ] **Step 1: 写失败的测试**

```go
// 事件 store 必须真的被接进 runtime 配置。
//
// 这个仓栽过两次同形的：插件工具与审批仲裁者都只接了 per-agent resolver，默认任务
// 路径没接——而默认路径服务大多数任务。症状是「功能整体不工作」，不是一个显眼的报错。
//
// 这条断言的是**装配的结果**（配置里那个字段非 nil 且指向仓储），不是「代码里有那一行」。
func TestTheSessionEventStoreReachesTheRuntimeConfig(t *testing.T) {
	// 用与 browser_private_hosts_test.go 相同的路子：调用装配函数、检查它产出的配置。
	// 具体函数名以 command.go 里实际的为准（grep runtimeConfigFor / buildRuntime）。
}
```

**动手前先看** `internal/cli/browser_private_hosts_test.go` 里 `TestEveryBrowserConfigKeyReachesTheRuntime` 的写法——那是这个仓解决同类问题的既有范式，照它写。如果装配代码没有一个可直接调用的「配置构造」函数，**先把它抽出来**（就像那次为 browser 抽 `browserRuntimeConfig` 一样），否则这条测试无从写起。

- [ ] **Step 2-4**：红 → 接线 → 绿。

- [ ] **Step 5: 真机验证（本任务的核心交付，spec §9 的 P2 判据）**

在一台真的 serve 上跑一个真任务，然后查库：

```bash
# 1. 起 serve（用一个临时 agent.json，storage.driver = sqlite）
# 2. 提交一个会调工具的任务
# 3. 查事件
sqlite3 <db> "SELECT seq, type, json_extract(data,'$.call_id') FROM session_events ORDER BY seq;"
```

**验收判据**（逐条核对并把真实输出贴进报告）：
- seq 从 0 连续，无洞；
- 序列平衡：每个 `tool/call` 都有同 `call_id` 的 `tool/result`；
- 以 `turn/end` 收尾且 `reason` **不是** `interrupted`；
- `tool/result` 的 `preview` 是截断过的，不是工具输出全文。

- [ ] **Step 6: 提交**

```bash
git add internal/cli/command.go internal/cli/session_events_wiring_test.go
git commit -m "feat(cli): 把事件 store 接进 runtime 装配"
```

---

## 完成判据（P2 全部做完时逐条核对）

- [ ] 五条任务入口（默认 runner / per-agent resolver / 恢复 / 委派 / app 直调）都经 `Runtime.RunTask`，因而都产出事件——**至少默认路径与委派路径各有一条测试**
- [ ] 三个屏障都 fail-closed，且屏障 2 有「工具体没被执行」的断言（不是只看返回了 error）
- [ ] 每条记录过的 `tool/call` 都有 `tool/result`，工具失败/取消也发（spec §4.3.1 第 1 条）
- [ ] 正常执行绝不产出 `turn/end{interrupted}`
- [ ] 事件里是预览不是全文
- [ ] 真机上跑通一个任务，库里的序列完整且平衡
- [ ] `go build`/`go vet`/`go test ./...` 全绿，`gofmt -l` 为空
- [ ] **P2 没有碰**：投影、`conversation_turns`、FTS5、`/events`、前端、G3

## 交给 P3 的东西

- 事件已经在库里，`ReadFrom` 可直接投影
- `assistant/message` 的载荷里带 `tool_calls`（call_id + name）与 usage 四件套——P3 的 `projectTurns` 按 `call_id` 配对时要的就是它
- 正常路径永不产出 `interrupted`：P3 若在投影里看到它，那一定是崩溃恢复补的
