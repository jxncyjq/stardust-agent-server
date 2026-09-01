# 会话事件日志 P3 —— 实现计划（投影与退役）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `conversation_turns` 退役——turn 由事件日志投影得出，`search_session` 改为搜事件（工具调用与结果一并可搜）。

**Architecture:** 把 `SQLiteRepository.ListConversationTurns` 的**实现**从「查 `conversation_turns` 表」换成「`ReadFrom` 读事件 → `projectTurns` 投影」。返回类型不变，于是四个读侧调用点一行都不用改。投影**按 `call_id` 配对**（spec §4.3.1 第 2 条），不按位置。最后删表与写入方。

**Tech Stack:** Go 1.26，复用 P1 的 `port.SessionEventStore`/`domain.SessionEvent` 与 P2 的发射点；SQLite FTS5；无新依赖。

## Global Constraints

- Spec：`docs/superpowers/specs/2026-08-31-session-event-log-and-trajectory-design.md`（master）。本计划做其中的 **P3**。
- P1（`a431ce9`）与 P2（`678054d`）已合入 master。**不要改 P1 的存储层不变量语义**（`Append` 的 seq 连续校验、`ReadFrom` 的只读后缀、`Load` 的恢复语义）。
- **fail-loud 铁律**（`legionAgent/CLAUDE.md` §0）：禁止兜底/fallback、禁止丢弃错误、禁止静默跳过、禁止零值假装正常；错误 `fmt.Errorf("<动作> <标识>: %w", err)` 包装。唯一豁免是契约显式声明的「可选」。
- 完成判据：`go build ./... && go vet ./... && go test ./... -count=1` 全绿，`gofmt -l $(git ls-files '*.go')` 为空。
- 每条不变量都要有断言且**变异可验红**——每个任务最后一步写明「删掉什么会让哪条测试红」，实现者必须真跑并把输出贴进报告。
- 每个 `go test` 带 `-timeout`；`cli`/`plugin`/`server` 包用 `-p 1`。
- **P3 不包括**：`/v1/sessions/{id}/events` 端点、SSE `session_event` 帧、轨迹前端（都是 **P4**）；G3 开关（**P5**）。本计划不得触碰这些。

### spec §4.3.1 的四条硬约束（P1/P2 用实证换来的，P3 必须守住）

1. 每条记录过的 `tool/call` 都有结果事件——P2 已兑现，P3 只消费。
2. **投影必须按 `call_id` 配对** assistant 的 `tool_calls` 与 tool 消息，**不能按位置**。崩溃恢复补出的事件排在日志**尾部**，按位置投影会产出非法 transcript（尾随的 tool 消息前面没有相邻的 assistant tool_calls）。**这是本期最重要的一条。**
3. `Load` 只可对「确定没有活跃写入者」的会话调用——**P3 的投影读路径一律用 `ReadFrom`，绝不调用 `Load`**。
4. 同一 step 内未应答的 `tool/call` 不复用 `call_id`——P2 已兑现（`disambiguateCallIDs`）。

### 已定的取舍（spec §3，不要重新讨论）

- **A2**：事件日志是唯一真相源，`conversation_turns` 退役。
- **B3**：不迁移历史数据（用户会清库重置）。所以**不需要写任何迁移代码**，也不需要兼容旧表里的数据。
- **C1**：投影在**服务端**。
- **F1**：委派子任务写自己的会话日志，父日志只留那一次 `tool/call`/`tool/result`。
- **H1**：`search_session` 改为对事件建 FTS5 索引。

### 控制者已拍板的一个决定（spec 未覆盖）

- **正文存全文**：`user/message` 与 `assistant/message` 的 `content` **不截断**。对话正文是对话本体，不是工具输出；spec §3 取舍 D 的「大载荷走 spill」针对的是工具结果。超长单条消息撞 P1 的 64 KiB/条上限时，`Append` 会 fail-loud 报错——这是正确行为，不要为它加兜底。

---

## 本期开工前必须知道的：五个字段缺口

写这个计划时做 spec coverage 检查，发现 spec §6 那句「返回类型不变，五个模型侧消费者一行都不用改」**成立的前提并不满足**——P2 的事件载荷产不出 `domain.ConversationTurn` 的全部字段。逐条：

| `ConversationTurn` 字段 | P2 事件里有吗 | 不补会怎样 |
|---|---|---|
| `SessionID` | ✅ 事件行的 `session_id` | — |
| `Role` | ✅ 由事件类型推出 | — |
| `CreatedAt` | ✅ 事件行的 `time` | — |
| `ModelProfile` | ✅ `assistant/message.model_profile` | — |
| `PromptTokens`/`CompletionTokens`/`CachedTokens`/`TotalTokens` | ✅ `assistant/message.usage` | — |
| **`Content`** | ⚠️ 只有 2000 rune 预览 | 模型侧本来允许 `defaultMaxTurnChars = 6000`（`internal/runtime/session_turns.go:17`），历史对话会被砍到 1/3 |
| **`TaskID`** | ❌ | `session_turns.go:58` 的 `turn.TaskID == task.ID` 过滤失效 → **模型会看到重复的 user 消息** |
| **`ID`** | ❌ | `ScrollMessages`（`sqlite.go:748`）靠 `turns[i].ID == aroundID` 定位锚点，找不到锚点直接报错 |
| **`AgentID`** | ❌ | `/turns` 响应与 FTS5 索引都带这个字段 |
| **`GeneratedFiles`** | ❌ | **GUI 已交付的「对话生成文件卡片」会静默失效**（`server/http.go:550` 的 `generatedFilesDTO`） |

**Task 1 就是补这五个缺口**，它是 P2 发射点的补丁。不先补，后面三个任务做出来的投影是残缺的，而残缺方式恰好都是「不报错、只是悄悄少了东西」——本仓栽过四次的那种。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/runtime/eventlog.go`（修改） | `recordUserMessage`/`recordAssistantMessage` 补齐 `task_id`/`agent_id`/`turn_id`/`generated_files`，`content` 改为不截断 |
| `internal/runtime/runtime.go`（修改） | 四个发射点（663、730、1313、1346）传入新增的值 |
| `internal/storage/project_turns.go`（新建） | `projectTurns(sessionID string, events []domain.SessionEvent) ([]domain.ConversationTurn, error)`——唯一的投影实现 |
| `internal/storage/project_turns_test.go`（新建） | 投影的不变量：按 `call_id` 配对、恢复补出的尾部事件不错配、字段齐全 |
| `internal/storage/sqlite.go`（修改） | `ListConversationTurns` 改为读事件→投影；`indexConversationTurn`/`SearchMessages` 改为对事件建索引；最后删 `conversation_turns` 及其写入方 |
| `internal/storage/session_search_test.go`（新建） | `search_session` 能搜到工具调用与结果 |

`project_turns.go` 单独成文件：`sqlite.go` 已经很大，而「事件怎么变成 turn」是一组自成一体的规则，且它是纯函数——不碰数据库，可以脱离 SQLite 直接单测。

---

## Task 1: 事件载荷补齐投影所需字段

**Files:**
- Modify: `internal/runtime/eventlog.go`
- Modify: `internal/runtime/runtime.go:663`、`:730`、`:1313`、`:1346`
- Test: `internal/runtime/eventlog_test.go`（在既有文件里加）

**Interfaces:**
- Consumes: P2 的 `eventRecorder`、`domain.SessionEventUserMessage`/`SessionEventAssistantMessage`
- Produces: `user/message` 载荷新增 `task_id`、`turn_id`；`assistant/message` 载荷新增 `task_id`、`agent_id`、`turn_id`、`generated_files`；两者的 `content` 均为全文

**为什么 `turn_id` 要事件自带、不能投影时现生成**：`ScrollMessages` 用 `turns[i].ID == aroundID` 定位锚点，调用方拿着上一次响应里的 ID 回来。投影每次现生成 ID 的话，同一条 turn 两次投影出来的 ID 不同，锚点必然找不到——而 `ScrollMessages` 找不到锚点是 `fmt.Errorf` 直接报错，不是返回空。

- [ ] **Step 1: 写失败的测试**

加到 `internal/runtime/eventlog_test.go`：

```go
// 投影（P3）要从事件里还原出完整的 domain.ConversationTurn。P2 的载荷缺五个字段，
// 缺任何一个都不会报错，只会让下游悄悄少点东西：TaskID 缺了模型会看到重复的 user
// 消息，GeneratedFiles 缺了 GUI 的文件卡片静默失效。所以这里逐字段断言。
func TestUserMessageEventCarriesEverythingAProjectionNeeds(t *testing.T) {
	store := newCaptureEventStore()
	rec := newEventRecorder(store, "sess-1", "task-7", "agent-a")
	rec.recordTurnStart()
	rec.recordUserMessage("请读一下 notes.md")
	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	data := payloadOfType(t, store.events, domain.SessionEventUserMessage)
	if got := data["task_id"]; got != "task-7" {
		t.Errorf("task_id = %v，要 task-7：session_turns.go 用它滤掉任务自己的 user turn，缺了模型会看到重复消息", got)
	}
	if got, _ := data["turn_id"].(string); got == "" {
		t.Error("turn_id 为空：ScrollMessages 用它定位锚点，投影时现生成会让同一条 turn 每次 ID 都不同")
	}
	if got := data["content"]; got != "请读一下 notes.md" {
		t.Errorf("content = %v，要原文", got)
	}
}

func TestAssistantMessageEventCarriesEverythingAProjectionNeeds(t *testing.T) {
	store := newCaptureEventStore()
	rec := newEventRecorder(store, "sess-1", "task-7", "agent-a")
	rec.recordTurnStart()
	rec.recordStepStart()
	rec.recordAssistantMessage(
		"读好了",
		nil,
		eventUsage{Prompt: 11, Completion: 22, Cached: 3, Total: 33},
		"fast",
		[]string{"out/report.md"},
	)
	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	data := payloadOfType(t, store.events, domain.SessionEventAssistantMessage)
	if got := data["task_id"]; got != "task-7" {
		t.Errorf("task_id = %v，要 task-7", got)
	}
	if got := data["agent_id"]; got != "agent-a" {
		t.Errorf("agent_id = %v，要 agent-a", got)
	}
	if got, _ := data["turn_id"].(string); got == "" {
		t.Error("turn_id 为空")
	}
	files, _ := data["generated_files"].([]any)
	if len(files) != 1 || files[0] != "out/report.md" {
		t.Errorf("generated_files = %v，要 [out/report.md]：缺了 GUI 的文件卡片会静默失效", files)
	}
}

// 对话正文是对话本体，不是工具输出。模型侧允许 defaultMaxTurnChars = 6000 字符，
// 而 maxEventPreviewRunes 只有 2000——截断会让历史对话缩到 1/3，
// 直接违反 P3 判据「五个模型侧消费者行为不变」。
func TestConversationContentIsStoredWhole(t *testing.T) {
	long := strings.Repeat("话", 5000) // 远超 maxEventPreviewRunes = 2000
	store := newCaptureEventStore()
	rec := newEventRecorder(store, "sess-1", "task-7", "agent-a")
	rec.recordTurnStart()
	rec.recordUserMessage(long)
	rec.recordStepStart()
	rec.recordAssistantMessage(long, nil, eventUsage{}, "fast", nil)
	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	for _, typ := range []domain.SessionEventType{
		domain.SessionEventUserMessage,
		domain.SessionEventAssistantMessage,
	} {
		data := payloadOfType(t, store.events, typ)
		got, _ := data["content"].(string)
		if len([]rune(got)) != 5000 {
			t.Errorf("%s 的 content 有 %d runes，要 5000：对话正文不该被截断",
				typ, len([]rune(got)))
		}
	}
}

// payloadOfType 取出指定类型的第一条事件的载荷。找不到就 Fatal——
// 「没有这条事件」和「这条事件字段不对」是两种不同的失败，不要混在一起报。
func payloadOfType(t *testing.T, events []domain.SessionEvent, typ domain.SessionEventType) map[string]any {
	t.Helper()
	for _, ev := range events {
		if ev.Type != typ {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("unmarshal %s payload: %v", typ, err)
		}
		return data
	}
	t.Fatalf("日志里没有 %s 事件", typ)
	return nil
}
```

**注意**：`newEventRecorder` 与 `recordAssistantMessage` 的现有签名不含 `taskID`/`agentID`/`generatedFiles`——上面的测试按**改造后**的签名写，所以它现在**编译不过**，这正是 Step 2 期待的红。

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/runtime/ -run "CarriesEverythingAProjectionNeeds|ContentIsStoredWhole" -count=1 -timeout 5m`

Expected: 编译失败，形如 `too many arguments in call to newEventRecorder` / `rec.recordAssistantMessage`。

**把真实输出记进报告。** 编译不过也是合法的红——它证明这几个参数今天确实不存在。

- [ ] **Step 3: 写实现**

`internal/runtime/eventlog.go`：

```go
// eventRecorder 现有字段之外，新增三个投影要用的身份：
//
//	taskID  —— session_turns.go 用 turn.TaskID == task.ID 滤掉任务自己的 user turn
//	agentID —— /turns 响应与 FTS5 索引都带它
//
// 它们在 newEventRecorder 时定死，一个 recorder 只服务一个任务。
func newEventRecorder(store port.SessionEventStore, sessionID, taskID, agentID string) *eventRecorder {
	if strings.TrimSpace(sessionID) == "" {
		panic("runtime: event recorder requires a session id")
	}
	// ……保留现有的其余初始化，另存 taskID / agentID……
}

// recordUserMessage 记一次用户输入。
//
// content 存**全文**，不截断：对话正文是对话本体，不是工具输出。模型侧允许
// defaultMaxTurnChars = 6000 字符（session_turns.go），截到 maxEventPreviewRunes
// = 2000 会让历史对话缩到 1/3。超长单条消息撞 P1 的 64 KiB/条上限时 Append 会
// fail-loud 报错——那是正确行为，不要在这里替它兜底。
func (e *eventRecorder) recordUserMessage(content string) {
	e.append(domain.SessionEventUserMessage, map[string]any{
		"turn":    e.currentTurn(),
		"turn_id": e.newTurnID(),
		"task_id": e.taskID,
		"content": content,
	})
}

// recordAssistantMessage 记一次模型响应。
//
// generatedFiles 是这一步经 write_file 产出的工作区相对路径；GUI 的「对话生成
// 文件卡片」靠它渲染（server/http.go 的 generatedFilesDTO）。为空是合法的可选
// （这一步没写文件），不是兜底。
//
// tool_calls 摘要仍然**不截断**：spec §4.3.1 第 2 条要求投影按 call_id 配对，
// 截掉任何一项都会让投影缺项。（P2 已有的取舍，此处不变。）
func (e *eventRecorder) recordAssistantMessage(
	content string,
	calls []domain.ToolCall,
	usage eventUsage,
	profile string,
	generatedFiles []string,
) {
	names := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		names = append(names, map[string]any{"call_id": c.ID, "name": c.Name})
	}
	e.append(domain.SessionEventAssistantMessage, map[string]any{
		"turn":     e.currentTurn(),
		"step":     e.currentStep(),
		"turn_id":  e.newTurnID(),
		"task_id":  e.taskID,
		"agent_id": e.agentID,
		"content":  content,
		"tool_calls": names,
		"usage": map[string]any{
			"prompt": usage.Prompt, "completion": usage.Completion,
			"cached": usage.Cached, "total": usage.Total,
		},
		"model_profile":   profile,
		"generated_files": generatedFiles,
	})
}

// newTurnID 生成一个投影稳定的 turn 标识。
//
// 它必须**写进事件**、而不是投影时现生成：ScrollMessages 用 turns[i].ID == aroundID
// 定位锚点，调用方拿着上一次响应里的 ID 回来；投影每次现生成的话同一条 turn 两次
// 投影出来的 ID 不同，锚点必然找不到，而那是 fmt.Errorf 直接报错。
//
// 用会话号 + seq 派生而不是随机 UUID：seq 在一条会话里唯一且稳定，于是同一条事件
// 无论投影多少次都得到同一个 ID，且不需要额外的随机源。
func (e *eventRecorder) newTurnID() string {
	return fmt.Sprintf("%s:%d", e.sessionID, e.nextSeqForTurnID())
}
```

> 实现者注意：`newTurnID` 需要拿到这条事件将要占用的 seq。`append` 是把事件放进 `pending`、seq 在 `flush` 时才最终确定的——**请自己读 `eventlog.go` 现在的真实结构决定怎么取**（例如在 `append` 内部构造载荷时补上、或用 `pending` 的下标加基准）。**不要为了拿 seq 去调 `ReadFrom`/`Load`**：spec §4.3.1 第 3 条禁止在任务执行路径上调 `Load`，而每条消息一次 `ReadFrom` 是 O(n) 的额外读。若你判断在当前结构里取不到稳定 seq，改用「会话号 + turn 号 + step 号 + 事件类型」派生也满足稳定性要求——两种都可以，在报告里说明你选了哪种、为什么。

`internal/runtime/runtime.go` 四个发射点：

```go
// :663 —— 建 recorder 的地方，补 taskID/agentID
rec := newEventRecorder(r.sessionEvents, sessionKeyForTask(task), task.ID, task.AgentID)

// :663 附近的 recordUserMessage 调用不变（签名没改）

// :730 —— 恢复路径。usage 保持 P2 定下的显式零值（checkpoint 只有累计值，
// 记单响应用量会让 P3 的消费者重复计数）；generatedFiles 传 nil：
// 恢复点上这一步的产物尚未确定。
rec.recordAssistantMessage(st.resp.Text, st.resp.ToolCalls, eventUsage{}, r.modelProfile, nil)

// :1313 与 :1346 —— generateStep / generateFinalStep，传本步已产出的文件
rec.recordAssistantMessage(resp.Text, resp.ToolCalls, eventUsage{ /* 保持现状 */ }, r.modelProfile, st.generatedFiles)
```

> 实现者注意：`task.AgentID` 与 `st.generatedFiles` 请核对它们在那几个位置**真的可见**（`runtime.go:1166`/`1189` 已经在用 `st.generatedFiles` 构造 `TaskResult`，所以它在 `generateStep`/`generateFinalStep` 的作用域里应当是可取的）。若某处取不到，**不要传空值糊过去**——在报告里说明并停下来。

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/runtime/ -run "CarriesEverythingAProjectionNeeds|ContentIsStoredWhole" -count=1 -timeout 5m -v`

Expected: 三条测试都 `--- PASS`。核对 `-run` 模式确实匹配到了这三个函数名（`grep -n "^func Test" internal/runtime/eventlog_test.go`），不要接受 `[no tests to run]` 却报 `ok`。

再跑全量：`go test ./... -count=1 -timeout 30m`

- [ ] **Step 5: 变异验证（四条）**

| # | 变异 | 期望红在哪 |
|---|---|---|
| 1 | `recordUserMessage` 的载荷里删掉 `"task_id"` | `TestUserMessageEventCarriesEverythingAProjectionNeeds` |
| 2 | `recordAssistantMessage` 的载荷里删掉 `"generated_files"` | `TestAssistantMessageEventCarriesEverythingAProjectionNeeds` |
| 3 | `content` 改回 `truncateRunes(content, maxEventPreviewRunes)`（两处都改） | `TestConversationContentIsStoredWhole` |
| 4 | `newTurnID` 改成返回空串 | 两条 `CarriesEverything...` 都红 |

每条：改 → 跑 → 贴真实 FAIL 输出 → `git checkout --` 还原 → `git status --short` 确认为空。

**注意**：如果某条变异只造成编译失败而不是测试失败，那不算变异验证——改成能编译但行为错的形式再跑（P2 有过先例：直接删一行导致 `declared and not used`，改成 `_ = preview` 后才跑出真正的红）。

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/eventlog.go internal/runtime/runtime.go internal/runtime/eventlog_test.go
git commit -m "feat(runtime): 事件载荷补齐投影所需的身份与产物字段，正文存全文"
```

---

## Task 2: `projectTurns` 投影函数

**Files:**
- Create: `internal/storage/project_turns.go`
- Test: `internal/storage/project_turns_test.go`

**Interfaces:**
- Consumes: `domain.SessionEvent`、Task 1 补齐后的事件载荷
- Produces: `func projectTurns(sessionID string, events []domain.SessionEvent) ([]domain.ConversationTurn, error)`——Task 3 会调它

**这个函数是纯函数**：不碰数据库、不读文件、不看时钟。所以它可以脱离 SQLite 直接单测，而这正是本期最重要的不变量（按 `call_id` 配对）能被便宜地反复验证的原因。

- [ ] **Step 1: 写失败的测试**

`internal/storage/project_turns_test.go`：

```go
package storage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// evWith 造一条带载荷的事件。载荷用 map 是为了让每个测试只写它关心的字段。
//
// 名字不叫 ev：`internal/storage/session_events_test.go:33` 已经有一个
// `func ev(seq int64, typ domain.SessionEventType) domain.SessionEvent`（不带载荷），
// 同包同名会直接编译失败（`ev redeclared in this block`）。**不要去改那个既有的 ev**
// ——它服务着 P1 的一批测试。
func evWith(seq int64, typ domain.SessionEventType, data map[string]any) domain.SessionEvent {
	raw, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return domain.SessionEvent{
		Seq:  seq,
		Type: typ,
		Time: time.Date(2026, 9, 1, 12, 0, int(seq), 0, time.UTC),
		Data: raw,
	}
}

// 一轮正常对话投影成一条 user turn + 一条 assistant turn，字段齐全。
func TestOneRoundProjectsToTwoTurns(t *testing.T) {
	events := []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-1:1", "task_id": "task-7", "content": "读 notes.md",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "sess-1:3",
			"task_id": "task-7", "agent_id": "agent-a",
			"content": "读好了", "tool_calls": []any{},
			"usage": map[string]any{"prompt": 11, "completion": 22, "cached": 3, "total": 33},
			"model_profile":   "fast",
			"generated_files": []any{"out/report.md"},
		}),
		evWith(4, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "completed"}),
		evWith(5, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "completed"}),
	}

	turns, err := projectTurns("sess-1", events)
	if err != nil {
		t.Fatalf("projectTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("投影出 %d 条 turn，要 2 条", len(turns))
	}

	user := turns[0]
	if user.Role != domain.ConversationRoleUser {
		t.Errorf("turns[0].Role = %v，要 user", user.Role)
	}
	if user.ID != "sess-1:1" || user.SessionID != "sess-1" || user.TaskID != "task-7" {
		t.Errorf("user turn 身份不对：ID=%q SessionID=%q TaskID=%q", user.ID, user.SessionID, user.TaskID)
	}
	if user.Content != "读 notes.md" {
		t.Errorf("user.Content = %q", user.Content)
	}

	asst := turns[1]
	if asst.Role != domain.ConversationRoleAssistant {
		t.Errorf("turns[1].Role = %v，要 assistant", asst.Role)
	}
	if asst.AgentID != "agent-a" || asst.ModelProfile != "fast" {
		t.Errorf("assistant turn 身份不对：AgentID=%q ModelProfile=%q", asst.AgentID, asst.ModelProfile)
	}
	if asst.PromptTokens != 11 || asst.CompletionTokens != 22 || asst.CachedTokens != 3 || asst.TotalTokens != 33 {
		t.Errorf("usage 四件套不对：%d/%d/%d/%d",
			asst.PromptTokens, asst.CompletionTokens, asst.CachedTokens, asst.TotalTokens)
	}
	if len(asst.GeneratedFiles) != 1 || asst.GeneratedFiles[0] != "out/report.md" {
		t.Errorf("GeneratedFiles = %v：GUI 的文件卡片靠它渲染", asst.GeneratedFiles)
	}
}

// spec §4.3.1 第 2 条：崩溃恢复补出的 tool/result 排在日志**尾部**，可能排在
// 自己那次调用的 step/end、turn/end 之后。按位置配对会把它配到错误的 assistant
// 上；必须按 call_id 配。
//
// 这条测试造的正是那个形状：两次调用 c1、c2 都没答，恢复补出的两条 result 挤在
// 尾部，且顺序与调用顺序相反。
func TestToolResultsPairByCallIDNotByPosition(t *testing.T) {
	events := []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "s:1", "task_id": "t", "content": "干活",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "s:3", "task_id": "t", "agent_id": "a",
			"content": "调两个工具",
			"tool_calls": []any{
				map[string]any{"call_id": "c1", "name": "read_file"},
				map[string]any{"call_id": "c2", "name": "read_file"},
			},
			"usage":         map[string]any{"prompt": 0, "completion": 0, "cached": 0, "total": 0},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
		}),
		evWith(5, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "name": "read_file", "arguments": "{}",
		}),
		// 崩了：step/end 与 turn/end 都没发出。恢复时补出来的，排在尾部且顺序颠倒。
		evWith(6, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "preview": "c2 的结果", "is_error": true,
		}),
		evWith(7, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "c1 的结果", "is_error": true,
		}),
		evWith(8, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "interrupted"}),
		evWith(9, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "interrupted"}),
	}

	turns, err := projectTurns("s", events)
	if err != nil {
		t.Fatalf("projectTurns: %v", err)
	}

	// P3 的投影产出的是 user/assistant 两种 turn（工具往返进模型上下文是 G3，属 P5）。
	// 这里要断言的是：尾部那两条乱序的 tool/result **没有**把 assistant turn 弄坏，
	// 且投影没有因为它们排在 turn/end 之后就报错或丢事件。
	if len(turns) != 2 {
		t.Fatalf("投影出 %d 条 turn，要 2 条（user + assistant）", len(turns))
	}
	if turns[1].Content != "调两个工具" {
		t.Errorf("assistant turn 被尾部的 tool/result 弄坏了：Content = %q", turns[1].Content)
	}
}

// 未知事件类型必须被拒绝并指名（spec §4.3 不变量：未知类型拒绝）。
// 不许静默跳过——静默跳过意味着将来加了新事件类型，老投影会悄悄少算。
func TestAnUnknownEventTypeIsRefusedByName(t *testing.T) {
	events := []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventType("session/teleport"), map[string]any{"turn": 0}),
	}

	_, err := projectTurns("s", events)
	if err == nil {
		t.Fatal("未知事件类型没有被拒绝：静默跳过意味着将来加新事件类型时投影会悄悄少算")
	}
	if !strings.Contains(err.Error(), "session/teleport") {
		t.Errorf("错误没有指名那个未知类型：%v", err)
	}
}

// 载荷缺字段是数据损坏，不是「可选」。缺了必须报错，不能拿零值凑一条 turn 出来。
func TestAMalformedPayloadIsRefused(t *testing.T) {
	events := []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, json.RawMessage(`{"turn":0`)), // 截断的 JSON
	}

	_, err := projectTurns("s", events)
	if err == nil {
		t.Fatal("损坏的载荷没有被拒绝")
	}
}
```

> 上面最后一条用了 `json.RawMessage` 直接当 `data`，而 `ev` 的签名收的是 `map[string]any`——实现者请按需要给 `evWith` 加一个兄弟辅助函数（例如 `evRaw`）来造损坏载荷，不要为此改 `evWith` 的签名把其余测试搅乱。

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/storage/ -run "ProjectsToTwoTurns|PairByCallID|UnknownEventType|MalformedPayload" -count=1 -timeout 10m`

Expected: 编译失败 `undefined: projectTurns`。

- [ ] **Step 3: 写实现**

`internal/storage/project_turns.go`：

```go
package storage

import (
	"encoding/json"
	"fmt"

	"github.com/stardust/legion-agent/internal/domain"
)

// projectTurns 把一条会话的事件流投影成对话轮次。
//
// 这是 conversation_turns 退役之后**唯一**的 turn 来源（spec §3 取舍 A2）。它是
// 纯函数：不碰数据库、不读文件、不看时钟，于是同一批事件永远投影出同一批 turn。
//
// 按 call_id 配对，不按位置（spec §4.3.1 第 2 条）：崩溃恢复补出的 tool/result
// 排在日志**尾部**，可能排在自己那次调用的 step/end 之后；按位置配会把它配到错误
// 的 assistant 上，产出非法 transcript。
//
// 未知事件类型一律**报错并指名**，不静默跳过——静默跳过意味着将来加了新事件类型，
// 这个函数会悄悄少算而没人发现。
func projectTurns(sessionID string, events []domain.SessionEvent) ([]domain.ConversationTurn, error) {
	turns := make([]domain.ConversationTurn, 0, len(events)/3)

	for _, event := range events {
		switch event.Type {
		case domain.SessionEventUserMessage:
			var payload struct {
				TurnID  string `json:"turn_id"`
				TaskID  string `json:"task_id"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, fmt.Errorf("project session %q: decode user/message at seq %d: %w",
					sessionID, event.Seq, err)
			}
			turns = append(turns, domain.ConversationTurn{
				ID:        payload.TurnID,
				SessionID: sessionID,
				TaskID:    payload.TaskID,
				Role:      domain.ConversationRoleUser,
				Content:   payload.Content,
				CreatedAt: event.Time,
			})

		case domain.SessionEventAssistantMessage:
			var payload struct {
				TurnID       string `json:"turn_id"`
				TaskID       string `json:"task_id"`
				AgentID      string `json:"agent_id"`
				Content      string `json:"content"`
				ModelProfile string `json:"model_profile"`
				Usage        struct {
					Prompt     int `json:"prompt"`
					Completion int `json:"completion"`
					Cached     int `json:"cached"`
					Total      int `json:"total"`
				} `json:"usage"`
				GeneratedFiles []string `json:"generated_files"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, fmt.Errorf("project session %q: decode assistant/message at seq %d: %w",
					sessionID, event.Seq, err)
			}
			turns = append(turns, domain.ConversationTurn{
				ID:               payload.TurnID,
				SessionID:        sessionID,
				TaskID:           payload.TaskID,
				AgentID:          payload.AgentID,
				ModelProfile:     payload.ModelProfile,
				Role:             domain.ConversationRoleAssistant,
				Content:          payload.Content,
				CreatedAt:        event.Time,
				PromptTokens:     payload.Usage.Prompt,
				CompletionTokens: payload.Usage.Completion,
				CachedTokens:     payload.Usage.Cached,
				TotalTokens:      payload.Usage.Total,
				GeneratedFiles:   payload.GeneratedFiles,
			})

		case domain.SessionEventTurnStart, domain.SessionEventTurnEnd,
			domain.SessionEventStepStart, domain.SessionEventStepEnd,
			domain.SessionEventToolCall, domain.SessionEventToolResult:
			// 这六类不产出 turn：它们是轨迹（P4）与搜索（Task 4）的素材。
			// 把工具往返也放进模型上下文是 G3，属 P5，本期不做。
			//
			// 逐条列出而不是用 default 跳过，是为了让「加了新事件类型却忘了在这里
			// 决定它怎么投影」变成一个编译期就能被发现的遗漏 + 下面那条 default 的
			// 运行期报错，而不是悄悄少算。

		default:
			return nil, fmt.Errorf("project session %q: unknown event type %q at seq %d",
				sessionID, event.Type, event.Seq)
		}
	}

	return turns, nil
}
```

> **实现者注意**：上面的 `switch` 里，`tool/call` 与 `tool/result` 落在「不产出 turn」那一支。`TestToolResultsPairByCallIDNotByPosition` 因此会通过——但**通过的理由必须是「投影正确地忽略了它们、且没有被尾部乱序弄坏」，而不是「反正没处理」**。请你自己判断：本期的投影既然不产出工具 turn，「按 `call_id` 配对」这条硬约束在**这一层**是否还有可验证的内容？如果你认为它实际落在 P5（G3 打开时才需要真正配对），**在报告里说清楚**，并说明本期用什么保证 P5 接手时不会踩到位置配对的坑——不要默默让那条测试变成一条没有守住任何东西的绿灯。这正是本仓栽过四次的「绿得不是地方」。

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/storage/ -run "ProjectsToTwoTurns|PairByCallID|UnknownEventType|MalformedPayload" -count=1 -timeout 10m -v`

Expected: 四条都 `--- PASS`。核对 `-run` 真的匹配到了这四个函数名。

- [ ] **Step 5: 变异验证（三条）**

| # | 变异 | 期望红在哪 |
|---|---|---|
| 1 | `default` 分支改成 `continue`（静默跳过未知类型） | `TestAnUnknownEventTypeIsRefusedByName` |
| 2 | assistant 分支里不填 `GeneratedFiles` | `TestOneRoundProjectsToTwoTurns` |
| 3 | user 分支的 `ID` 改成 `""` | `TestOneRoundProjectsToTwoTurns` |

每条：改 → 跑 → 贴真实 FAIL 输出 → 还原 → `git status --short` 确认为空。

- [ ] **Step 6: 提交**

```bash
git add internal/storage/project_turns.go internal/storage/project_turns_test.go
git commit -m "feat(storage): 会话事件到对话轮次的投影"
```

---

## Task 3: `ListConversationTurns` 改为读事件→投影

**Files:**
- Modify: `internal/storage/sqlite.go:495`（`ListConversationTurns`）
- Test: `internal/storage/session_projection_test.go`（新建）

**Interfaces:**
- Consumes: Task 2 的 `projectTurns`、P1 的 `ReadFrom`
- Produces: 行为不变的 `ListConversationTurns(ctx, sessionID, limit) ([]domain.ConversationTurn, error)`

**这一步是本期的接线任务**，也是「接缝在但没人调用它」的高发区。读侧有**四个**调用点，它们一行都不用改，但**每一个都要有一条断言它行为没变的测试**：

| # | 调用点 | 是什么 | 它依赖 turn 的什么 |
|---|---|---|---|
| ① | `internal/cli/command.go:1205` | TUI session controller 取最近轮次 | 顺序、`limit` 语义 |
| ② | `internal/runtime/session_turns.go:52` | 多轮 messages 喂模型 | `TaskID`（滤掉任务自己的 user turn）、`Content` 完整 |
| ③ | `internal/server/http.go:503` | `GET /v1/sessions/{id}/turns` | `GeneratedFiles`（GUI 文件卡片）、全部 DTO 字段 |
| ④ | `internal/storage/sqlite.go:748` | `ScrollMessages` 内部调用 | `ID`（定位锚点） |

- [ ] **Step 1: 写失败的测试**

`internal/storage/session_projection_test.go`：

```go
package storage

import (
	"context"
	"fmt"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// ListConversationTurns 现在从事件投影。这条测试写事件、读 turn，
// 断言的是**读写两侧真的接在一起了**，不是投影函数本身对不对（那是 Task 2 的事）。
func TestListConversationTurnsReadsFromTheEventLog(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if err := repo.Append(ctx, "sess-1", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-1:1", "task_id": "task-7", "content": "读 notes.md",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "sess-1:3",
			"task_id": "task-7", "agent_id": "agent-a", "content": "读好了",
			"tool_calls": []any{},
			"usage":      map[string]any{"prompt": 11, "completion": 22, "cached": 3, "total": 33},
			"model_profile":   "fast",
			"generated_files": []any{"out/report.md"},
		}),
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}

	turns, err := repo.ListConversationTurns(ctx, "sess-1", 0)
	if err != nil {
		t.Fatalf("ListConversationTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("拿到 %d 条 turn，要 2 条：事件写进去了但 ListConversationTurns 没从事件读", len(turns))
	}
	if turns[0].Role != domain.ConversationRoleUser || turns[1].Role != domain.ConversationRoleAssistant {
		t.Errorf("顺序不对：%v, %v", turns[0].Role, turns[1].Role)
	}
	if turns[1].TaskID != "task-7" {
		t.Errorf("TaskID = %q：session_turns.go 用它滤掉任务自己的 user turn", turns[1].TaskID)
	}
	if len(turns[1].GeneratedFiles) != 1 {
		t.Errorf("GeneratedFiles = %v：GUI 的文件卡片靠它渲染", turns[1].GeneratedFiles)
	}
}

// limit 的语义必须与旧实现一致：取**最近**的 N 条，且返回时仍按时间正序。
// 旧实现是 ORDER BY created_at DESC LIMIT n 之后再反转，很容易在改写时把
// 「最近 N 条」写成「最早 N 条」——那种错不会报错，只会让模型看见错的历史。
func TestListConversationTurnsLimitTakesTheMostRecent(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	events := []domain.SessionEvent{evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0})}
	for i := 1; i <= 6; i++ {
		events = append(events, evWith(int64(i), domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": fmt.Sprintf("sess-1:%d", i), "task_id": "t",
			"content": fmt.Sprintf("第 %d 条", i),
		}))
	}
	if err := repo.Append(ctx, "sess-1", events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	turns, err := repo.ListConversationTurns(ctx, "sess-1", 2)
	if err != nil {
		t.Fatalf("ListConversationTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("拿到 %d 条，要 2 条", len(turns))
	}
	if turns[0].Content != "第 5 条" || turns[1].Content != "第 6 条" {
		t.Errorf("limit 取错了一端：拿到 %q, %q，要「第 5 条」「第 6 条」",
			turns[0].Content, turns[1].Content)
	}
}

// 没有事件的会话返回空切片而不是报错——「这条会话还没说过话」是合法状态。
func TestAnEmptySessionProjectsToNoTurns(t *testing.T) {
	repo := newEventRepo(t)
	turns, err := repo.ListConversationTurns(context.Background(), "sess-empty", 0)
	if err != nil {
		t.Fatalf("空会话不该报错：%v", err)
	}
	if len(turns) != 0 {
		t.Errorf("空会话投影出 %d 条 turn", len(turns))
	}
}
```

> `newEventRepo(t)` 是本包既有的建库辅助函数（`session_events_test.go:16`，用 `OpenSQLite` 建在 `t.TempDir()` 下并注册 `Cleanup`），直接用，不要新造一套。`repo.Append(ctx, sessionID, events)` 是 P1 的方法（`session_events.go:75`）。`evWith` 是 Task 2 建的辅助函数，同包可直接用。

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/storage/ -run "ReadsFromTheEventLog|LimitTakesTheMostRecent|EmptySessionProjects" -count=1 -timeout 10m`

Expected: 前两条 FAIL——写了事件却拿不到 turn（旧实现查的是 `conversation_turns` 表，那里是空的）。**把真实输出记进报告。**

- [ ] **Step 3: 写实现**

`internal/storage/sqlite.go`，整体替换 `ListConversationTurns`：

```go
// ListConversationTurns 返回一条会话的对话轮次，按时间正序；limit > 0 时只取
// **最近** limit 条。
//
// 轮次由会话事件投影得出（spec §3 取舍 A2：事件日志是唯一真相源）。这里用
// ReadFrom(0) 而不是 Load：spec §4.3.1 第 3 条要求 Load 只对「确定没有活跃写入者」
// 的会话调用，而这个方法在任务执行期间也会被调用（多轮 messages 就走它）。
// ReadFrom 只读后缀、不触发恢复，正是这里要的。
func (r *SQLiteRepository) ListConversationTurns(ctx context.Context, sessionID string, limit int) ([]domain.ConversationTurn, error) {
	events, err := r.ReadFrom(ctx, sessionID, 0)
	if err != nil {
		return nil, fmt.Errorf("list conversation turns for %q: %w", sessionID, err)
	}
	turns, err := projectTurns(sessionID, events)
	if err != nil {
		return nil, fmt.Errorf("list conversation turns for %q: %w", sessionID, err)
	}
	// limit 取最近的 N 条：事件是正序的，所以从尾部截。旧实现是
	// ORDER BY created_at DESC LIMIT n 再反转，语义相同。
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, nil
}
```

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/storage/ -run "ReadsFromTheEventLog|LimitTakesTheMostRecent|EmptySessionProjects" -count=1 -timeout 10m -v`

Expected: 三条都 `--- PASS`。

再跑全量：`go test ./... -count=1 -timeout 30m -p 1`

**全量这一步是本任务的重点**：四个读侧调用点的既有测试现在跑的是新实现。**任何一条既有测试变红，都是「行为变了」的直接证据，不要改测试去迁就实现**——先判断是实现错了还是那条测试原本就在断言旧存储的实现细节，把判断写进报告。

- [ ] **Step 5: 变异验证（三条）**

| # | 变异 | 期望红在哪 |
|---|---|---|
| 1 | `limit` 那段改成 `turns = turns[:limit]`（取最早 N 条） | `TestListConversationTurnsLimitTakesTheMostRecent` |
| 2 | `ReadFrom` 的 `fromSeq` 从 `0` 改成 `1` | `TestListConversationTurnsReadsFromTheEventLog`（少一条 turn） |
| 3 | `projectTurns` 的错误改成 `return nil, nil`（吞掉） | Task 2 的 `TestAnUnknownEventTypeIsRefusedByName` 不受影响，但本包应有测试变红；若**没有**测试红，说明「投影失败被吞掉」这条路没人守——**补一条**再继续 |

第 3 条是本仓「绿得不是地方」的专治：变异不红时，**不要**记成「这条变异不适用」，要么补测试，要么在报告里论证为什么它不可达。

- [ ] **Step 6: 提交**

```bash
git add internal/storage/sqlite.go internal/storage/session_projection_test.go
git commit -m "feat(storage): 对话轮次改由事件投影得出"
```

---

## Task 4: `search_session` 改为搜事件

**Files:**
- Modify: `internal/storage/sqlite.go`（`schemaStatements` 加 `session_events_fts`、`CurrentSchemaVersion` 9→10、`SearchMessages`、`indexConversationTurn` 的去留）
- Modify: `internal/storage/session_events.go`（`Append` 里同事务写索引）
- Test: `internal/storage/session_search_test.go`（新建）

**Interfaces:**
- Consumes: P1 的 `Append`（索引必须在同一个事务里写）
- Produces: `SearchMessages(ctx, query, limit) ([]domain.ConversationTurn, error)` 行为扩展——现在也能命中工具调用与结果

**判据（spec §9）**：`search_session` 能搜到工具调用。这是 H1 取舍的全部意义：单一真相源，顺带让工具调用与结果可搜。

- [ ] **Step 1: 写失败的测试**

`internal/storage/session_search_test.go`：

```go
package storage

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// H1 的全部意义：搜索改到事件上之后，工具调用与结果一并可搜。
// 旧的 conversation_turns_fts 只索引对话正文，工具往返从不落盘，所以搜不到。
func TestSearchFindsToolCallsNotJustConversation(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if err := repo.Append(ctx, "sess-1", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-1:1", "task_id": "t", "content": "帮我看看配置",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file",
			"arguments": `{"path":"deploy/kubernetes.yaml"}`,
		}),
		evWith(4, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1",
			"preview": "replicas: 3", "is_error": false,
		}),
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// 工具参数里的路径
	hits, err := repo.SearchMessages(ctx, "kubernetes", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) == 0 {
		t.Error("搜不到工具调用的参数：H1 的全部意义就是让工具往返可搜")
	}

	// 工具结果的预览
	hits, err = repo.SearchMessages(ctx, "replicas", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) == 0 {
		t.Error("搜不到工具结果的预览")
	}
}

// 对话正文照样搜得到——这是既有能力，不能因为换了索引就丢。
func TestSearchStillFindsConversation(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if err := repo.Append(ctx, "sess-1", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-1:1", "task_id": "t",
			"content": "帮我查一下水獭的分布",
		}),
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}

	hits, err := repo.SearchMessages(ctx, "水獭", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) == 0 {
		t.Error("搜不到对话正文：换索引不能丢掉既有能力")
	}
}

// 索引必须与事件在**同一个事务**里提交。P1 的 indexConversationTurn 已经立下这个
// 规矩（「a turn is never persisted without being searchable」），事件侧照办：
// 索引写失败时整批 Append 必须回滚，不能留下搜不到的事件。
func TestAFailedIndexWriteRollsBackTheWholeAppend(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	// 把索引表拿掉，索引写必然失败。这是确定性的——不靠竞态、不靠 sleep。
	if _, err := repo.db.ExecContext(ctx, `DROP TABLE session_events_fts`); err != nil {
		t.Fatalf("drop fts table: %v", err)
	}

	err := repo.Append(ctx, "sess-1", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-1:1", "task_id": "t", "content": "会被回滚掉",
		}),
	})
	if err == nil {
		t.Fatal("索引写失败了 Append 却成功了：事件会存进去但搜不到")
	}

	// 关键的一半：整批必须回滚，不能留下半条日志。
	events, readErr := repo.ReadFrom(ctx, "sess-1", 0)
	if readErr != nil {
		t.Fatalf("ReadFrom: %v", readErr)
	}
	if len(events) != 0 {
		t.Errorf("回滚后还剩 %d 条事件：索引与事件必须同事务提交", len(events))
	}
}
```

> **实现者注意**：最后那条测试直接 `DROP TABLE session_events_fts` 来制造索引写失败——确定性的做法，不靠竞态也不靠 sleep。它访问了 `repo.db`（同包，可直接取）。如果你发现 `Append` 的事务结构让这条测试无法如期红/绿，**先在报告里说明**再动手改生产代码。

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/storage/ -run "SearchFindsToolCalls|SearchStillFindsConversation|FailedIndexWrite" -count=1 -timeout 10m`

Expected: 前两条 FAIL（写了事件但 `conversation_turns_fts` 里什么都没有，搜不到）。

- [ ] **Step 3: 写实现**

三处改动：

**(a) 建表**——`internal/storage/sqlite.go` 的 `schemaStatements` 里加：

```sql
CREATE VIRTUAL TABLE IF NOT EXISTS session_events_fts USING fts5(
	content,
	session_id UNINDEXED,
	seq UNINDEXED,
	type UNINDEXED,
	turn_id UNINDEXED,
	task_id UNINDEXED,
	agent_id UNINDEXED,
	created_at UNINDEXED
);
```

并把 `CurrentSchemaVersion` 从 `9` 提到 `10`，同时按该常量注释的要求更新它上面那段版本说明（`sqlite.go:36-50`）。

> **不要写迁移代码**：spec §3 取舍 B3 已定「不迁移历史数据（用户会清库重置）」。`migrate()` 无条件执行全部 `CREATE TABLE IF NOT EXISTS`，既有库照样建出新表。

**(b) 写索引**——在 `internal/storage/session_events.go` 的 `appendLocked` 里，事件插入之后、同一个事务内，为**可搜的事件类型**写索引：

```go
// indexSessionEvent 把一条事件的可搜文本镜进 FTS5 索引。它在调用方的事务里运行，
// 于是索引与事件一起提交；失败包装后返回而不是吞掉，事件永远不会「存进去了却搜不到」。
//
// 可搜的是四类：user/assistant 的正文，以及 tool/call 的参数与 tool/result 的预览
// ——最后两类正是 H1 要解决的「工具往返从不可搜」。turn/step 的边界事件没有可搜
// 文本，跳过它们不是遗漏：它们本来就没有内容可搜。
func indexSessionEvent(ctx context.Context, ex execer, sessionID string, event domain.SessionEvent) error {
	text, meta, ok, err := searchableText(event)
	if err != nil {
		return fmt.Errorf("index session event %d of %q for search: %w", event.Seq, sessionID, err)
	}
	if !ok {
		return nil
	}
	if _, err := ex.ExecContext(ctx, `
		INSERT INTO session_events_fts (
			content, session_id, seq, type, turn_id, task_id, agent_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, text, sessionID, event.Seq, string(event.Type),
		meta.TurnID, meta.TaskID, meta.AgentID, formatTime(event.Time)); err != nil {
		return fmt.Errorf("index session event %d of %q for search: %w", event.Seq, sessionID, err)
	}
	return nil
}
```

> 实现者：`searchableText` 与 `meta` 的具体形状由你定——它要从四类事件的载荷里取出可搜文本与身份字段。`execer` 接口 `sqlite.go:671` 已有，直接复用。

**(c) 搜索**——把 `SearchMessages` 改为查 `session_events_fts`，命中行投影成 `domain.ConversationTurn` 返回（返回类型不变，`session_search` 工具一行不用改）。

> **一个硬约束**：`domain.ConversationRole` 只有 `ConversationRoleUser` 与 `ConversationRoleAssistant` 两个值（`internal/domain/types.go:203-204`），**没有 tool**。所以工具类事件命中时你不能凭空造一个 role。两条出路：把工具往返归到发起它的 `assistant` 上（`tool/call`/`tool/result` 的载荷里有 `turn`/`step`，能定位到那一步的 assistant），或者给 `ConversationRole` 加一个取值。**后者要改领域类型，会波及所有消费者**——若你选它，先在报告里说明影响面。无论选哪条，都不要用空 role 或空字符串糊过去。

**(d) 旧索引的去留**——`indexConversationTurn` 与 `conversation_turns_fts` 留到 Task 5 一起删。本任务只加不删，这样 Task 4 出问题时可以单独回滚。

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/storage/ -run "SearchFindsToolCalls|SearchStillFindsConversation|FailedIndexWrite" -count=1 -timeout 10m -v`

Expected: 三条都 `--- PASS`。

再跑全量：`go test ./... -count=1 -timeout 30m -p 1`

- [ ] **Step 5: 变异验证（三条）**

| # | 变异 | 期望红在哪 |
|---|---|---|
| 1 | `indexSessionEvent` 里跳过 `tool/call` 与 `tool/result` | `TestSearchFindsToolCallsNotJustConversation` |
| 2 | `indexSessionEvent` 的错误改成 `return nil`（吞掉） | `TestAFailedIndexWriteRollsBackTheWholeAppend` |
| 3 | 索引写移到事务**外面** | `TestAFailedIndexWriteRollsBackTheWholeAppend` |

每条：改 → 跑 → 贴真实 FAIL 输出 → 还原 → `git status --short` 确认为空。

- [ ] **Step 6: 提交**

```bash
git add internal/storage/sqlite.go internal/storage/session_events.go internal/storage/session_search_test.go
git commit -m "feat(storage): search_session 改为搜事件，工具往返一并可搜"
```

---

## Task 5: 删 `conversation_turns` 及其写入方

**Files:**
- Modify: `internal/storage/sqlite.go`（删表、删 `AppendConversationTurn`/`AppendConversationTurnIfAbsent`/`indexConversationTurn`/`scanConversationTurn`、删 `conversation_turns` 与 `conversation_turns_fts` 的建表语句）
- Modify: `internal/cli/command.go:1030-1031`、`:1238`（接口与调用点）
- Modify: `internal/server/http.go:63`、`:1146`、`:1316`（接口与两个调用点）
- Test: 既有测试的调整 + `internal/storage/retirement_test.go`（新建）

**Interfaces:**
- Consumes: Task 3 的投影读路径、Task 4 的事件索引
- Produces: 一个不再有 `conversation_turns` 的 schema

**这一步不可逆，放在最后**：spec §9 明确「P3 删表之前，P2 必须已在真机上写出完整事件流，否则删掉的是唯一能用的那份数据」。P2 的真机验证已经做过（`.superpowers/sdd/task-5-report.md`），且 Task 3/4 已经让读侧与搜索都走事件——到这一步，`conversation_turns` 才真的没有消费者。

- [ ] **Step 1: 写失败的测试**

`internal/storage/retirement_test.go`：

```go
package storage

import (
	"context"
	"testing"
)

// 表必须真的没了。留着一张没人写的空表比删掉更糟：下一个人会以为它是真相源之一。
func TestTheConversationTurnsTableIsGone(t *testing.T) {
	repo := newEventRepo(t)
	for _, table := range []string{"conversation_turns", "conversation_turns_fts"} {
		var name string
		err := repo.db.QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE name = ?`, table).Scan(&name)
		if err == nil {
			t.Errorf("%s 还在：事件日志是唯一真相源（spec §3 取舍 A2），两个真相源迟早会漂移", table)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/storage/ -run "ConversationTurnsTableIsGone" -count=1 -timeout 10m`

Expected: FAIL，两张表都还在。

- [ ] **Step 3: 写实现**

按依赖顺序删，每删一处就 `go build ./...` 一次，让编译器把剩下的引用逐个指出来：

1. `internal/server/http.go`：删 `AppendConversationTurnIfAbsent` 的接口方法（`:63`）与两个调用点（`:1146`、`:1316`）。
   > **这两处是「记录 assistant turn」的写入点。** 删之前先确认：P2 的事件发射已经覆盖了它们记录的全部内容（Task 1 补齐字段正是为此）。若发现某个字段只有这里有、事件里没有，**停下来在报告里说明**——那意味着 Task 1 的缺口清单漏了一项。
2. `internal/cli/command.go`：删 `AppendConversationTurn`/`ListConversationTurns` 接口里的写方法（`:1030`）与调用点（`:1238`）。**`ListConversationTurns` 保留**——它现在读的是投影。
3. `internal/storage/sqlite.go`：删 `AppendConversationTurn`（`:405`）、`AppendConversationTurnIfAbsent`（`:449`）、`indexConversationTurn`（`:679`）、`scanConversationTurn`，以及 `schemaStatements` 里 `conversation_turns` 与 `conversation_turns_fts` 的建表语句；`:320` 的 `DELETE FROM conversation_turns`（删会话时清 turn）也一并删——事件由会话删除路径负责清理，请**核实那条路径确实会清 `session_events`**，若不会，这是一处必须补的清理。
4. `CurrentSchemaVersion` 10→11，并更新版本说明注释。

> **不要留「兼容期」的双写或双读**：spec §3 取舍 A2 是「不留两个真相源」，B3 是「不迁移历史数据」。留一半是最坏的选择。

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/storage/ -run "ConversationTurnsTableIsGone" -count=1 -timeout 10m -v`

再跑全量：`go build ./... && go vet ./... && go test ./... -count=1 -timeout 30m -p 1`

**删表会让一批既有测试编译不过或变红**（它们直接建 turn、直接查表）。逐条判断：
- 断言的是**行为**（会话里有哪些 turn）→ 改成写事件、读投影；
- 断言的是**旧存储的实现细节**（表里有几行）→ 删掉，它守的东西已经不存在了。

**把每一条的判断写进报告**，不要笼统写「适配了测试」。

- [ ] **Step 5: 变异验证（两条）**

| # | 变异 | 期望红在哪 |
|---|---|---|
| 1 | 把 `conversation_turns` 的建表语句加回 `schemaStatements` | `TestTheConversationTurnsTableIsGone` |
| 2 | 把 `CurrentSchemaVersion` 改回 10 | 本包应有断言版本号与 `schemaStatements` 同步的测试；若**没有**，在报告里说明——那条自述「改 schemaStatements 就要提版本」的注释就没人守（P1 有过同形的 Minor） |

- [ ] **Step 6: 提交**

```bash
git add internal/storage/sqlite.go internal/cli/command.go internal/server/http.go internal/storage/retirement_test.go
git commit -m "refactor: conversation_turns 退役，事件日志成为唯一真相源"
```

---

## 完成判据（P3 全部做完时逐条核对）

- [ ] 四个读侧调用点（TUI / 多轮 messages / `/turns` handler / `ScrollMessages`）行为不变，**每一个都有自己的断言**
- [ ] 投影按 `call_id` 配对而非按位置（spec §4.3.1 第 2 条），或已在报告里论证清楚这条在本期落在哪一层
- [ ] 投影路径**从不调用 `Load`**（spec §4.3.1 第 3 条）
- [ ] `search_session` 能搜到工具调用的参数与结果的预览，且对话正文照样搜得到
- [ ] 索引与事件在同一个事务里提交
- [ ] `conversation_turns` 与 `conversation_turns_fts` 都真的不存在了
- [ ] `GeneratedFiles` 一路活到 `/turns` 的 DTO（GUI 文件卡片没坏）
- [ ] 对话正文没有被截断（模型侧 `defaultMaxTurnChars = 6000` 仍然是那个上限，而不是被 2000 卡住）
- [ ] `go build`/`go vet`/`go test ./...` 全绿，`gofmt -l` 为空
- [ ] **P3 没有碰**：`/events` 端点、SSE 帧、轨迹前端（P4）、G3 开关（P5）

## 交给 P4 的东西

- 事件已是唯一真相源，`ReadFrom` 可直接喂 `GET /v1/sessions/{id}/events`
- `tool/call` 与 `tool/result` 的载荷里有 `call_id`、`name`、`arguments`/`preview`——轨迹表格要的就是它们
- 投影是纯函数且与存储解耦，P4 的 SSE 帧可以复用同一批事件而不必再算一次

## 本期已知、不在范围内的事（P2 交上来，控制者已分诊为「可带走」）

- `App.RunTask` 用 `task.ID` 而非 TUI 会话号，每轮 TUI 对话写成一条独立短日志
- 每轮两次 O(n) `ReadFrom`（要 store 侧的 next-turn 查询才能优化，属 P1 的面）；**Task 3 让 `ListConversationTurns` 也变成 O(n) 全量读**，若真机上测出慢，spec §6 已写明「投影缓存先不做，测出慢再加」
- GUI 仓（`stardust-agent-gui`）走 `BuildServeService`，本期改动自动覆盖它，但未在 GUI 仓做真机走查
