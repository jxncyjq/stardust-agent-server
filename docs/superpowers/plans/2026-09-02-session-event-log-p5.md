# 会话事件日志 P5 —— 实现计划（G3 开关：模型上下文含历史工具往返）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让模型在会话恢复时能看见历史的工具往返（不再失忆）——做成一个默认关闭、打开后体积变化可度量的开关。

**Architecture:** G3 是一条**分叉**而不是一个补丁：关闭时走今天的文本块路径（一个字节都不改，默认如此）；打开时历史改以 **provider transcript** 形式进模型——assistant 消息带 `tool_calls`，其后跟与之 `call_id` 配对的 tool 消息，内容是预览。新增一个从事件直接投影出 transcript 的纯函数（`projectTranscript`），与 P3 的 `projectTurns` 并列而不是取代它。

**Tech Stack:** Go 1.26；复用 P1 的 `port.SessionEventStore`、P3 的投影层、既有的 `port.InferenceMessage`；无新依赖。

## Global Constraints

- Spec：`docs/superpowers/specs/2026-08-31-session-event-log-and-trajectory-design.md`（master）。本计划做 spec §9 的 **P5**、§6 的 **G3** 那段。
- P1（`a431ce9`）、P2（`678054d`）、P3（`7e93cb0`）、P4a（`acaa107`）已合入本仓 master；P4b 已合入 GUI 仓（`b47cda0`）。**不要改这四期定下的语义。**
- **fail-loud 铁律**（`legionAgent/CLAUDE.md` §0）：禁止兜底/fallback、禁止丢弃错误、禁止静默跳过、禁止零值假装正常；错误 `fmt.Errorf("<动作> <标识>: %w", err)` 包装。唯一豁免是契约显式声明的「可选」。
- 完成判据：`go build ./... && go vet ./... && go test ./... -count=1` 全绿，`gofmt -l $(git ls-files '*.go')` 为空。
- 每条不变量都要有断言且**变异可验红**——每个任务最后一步写明「删掉什么会让哪条测试红」，实现者必须真跑并把输出贴进报告。
- 每个 `go test` 带 `-timeout`；`cli`/`plugin`/`server` 包用 `-p 1`。
- **P5 不做**：投影缓存、虚拟滚动、`assistant/chunk` 入库（spec §10 都写明了触发条件）。

### 默认必须是关

spec §3 取舍 G3 的理由原文：「它改的是每次请求的体积，**不该在做轨迹的顺路上悄悄打开**」。所以：

- 配置项零值 = 关；
- 任何「没配就按开处理」的写法都是违规；
- 关闭时的行为必须与今天**逐字节相同**——这一条要有测试守着。

### spec §4.3.1 的硬约束（P5 是第 2 条的兑现处）

1. 每条记录过的 `tool/call` 都有结果事件（P2 已兑现，P5 只消费）。
2. **投影按 `call_id` 配对，不按位置。** 崩溃恢复补出的 `tool/result` 排在日志**尾部**，可能排在自己那次调用的 `step/end` 之后；按位置配会把它配到错误的 assistant 上。**P3 的 Task 2 实现者论证过「真正的配对时机在 P5」——就是这里。**
3. **读路径一律用 `ReadFrom`，绝不调用 `Load`**。
4. 同一 step 内未应答的 `tool/call` 不复用 `call_id`（P2 的 `disambiguateCallIDs` 已兑现）。

### provider 的硬性契约（配错就 400）

`internal/port/ports.go:52` 的 `InferenceMessage`，其 `Validate` 强制 role=tool 必须有 `ToolCallID`，注释原文：

> an OpenAI-compatible provider rejects a tool message it cannot pair with a preceding tool call

**所以「按 call_id 配对」不是审美要求，是不配对就发不出去。** 一条 tool 消息若前面没有带同 `call_id` 的 assistant `tool_calls`，请求会被 provider 拒绝。

---

## 本期开工前必须知道的：spec 描述的形状与代码现状不符

spec §6 说 G3 打开时「消息形状是 provider transcript 的标准形状」。**但历史今天根本不以 message 形式进模型。**

现状（`internal/cognitive/core.go:166-210`）：整个 prompt 是**一段字符串**，按 section 拼起来——

```
[稳定前缀]  catalog + durable_memory + plugin_prompt + context_files
            ↑ stablePrefixLen := len([]rune(prompt)) 在这里取值
[易变后缀]  header(含 "Input: <当前任务输入>") + capability + prefetch + conversation
                                                                        ↑ conversationBlock(turns)
                                                                          "Recent conversation:\n- user: ...\n"
```

这整段交给 `newConversation(basePrompt, task.Images)`（`internal/runtime/messages.go:33`）→ `messages[0] = {Role: user, Content: 整段, Images}`，再由 `convo.pinCachePrefix(stablePrefixLen)`（`runtime.go:809`）把 `StablePrefixLen` 打在 **`messages[0]`** 上。

provider transcript 那套（`appendAssistant` / `appendToolResults`）**只用于当前任务内的工具循环**，历史从来没走过它。

### 由此产生的两个设计约束

**① prefix cache 会错位。** 稳定前缀今天在 `messages[0].Content` 的开头。历史一旦变成独立 message 插进去，`messages[0]` 就不再是那段以稳定前缀开头的文本，缓存断点会打在错的地方——而 `internal/adapter/http_maas.go:353/371` 正是照它设 provider 的 `cache_control`。

**② 当前任务的输入藏在 header 里。** `header` 段含 `Input: <当前任务输入>`，它在稳定前缀**之后**。所以不能简单地把「整段 prompt」当成 messages[0]、把历史插在它前面——那样当前输入会跑到历史前面去，时序颠倒。

### 本计划的排布（Task 3 落地）

G3 打开时：

```
messages[0] = {user, 稳定前缀 + header + capability + prefetch}   ← StablePrefixLen 照旧打在这里
messages[1..n] = 历史 transcript（user / assistant(+tool_calls) / tool(+tool_call_id) 交替）
```

**历史排在 messages[0] 之后**，不是之前。理由是缓存：`messages[0]` 必须仍以那段跨任务逐字节相同的稳定前缀开头，否则每次请求都缓存未命中——而 G3 的全部代价就是体积，再赔上缓存不划算。代价是 `header` 里的 `Input` 出现在历史之前；这是**有意的取舍**，Task 3 要把它写进注释。

> 若实现时发现 provider 对「当前输入在历史之前」有可观察的行为差异，**停下来在报告里说明**，不要自行改排布——那会牵动缓存。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/config/config.go`（修改） | `SessionConfig` 加 `ToolTranscriptEnabled bool`（零值=关） |
| `internal/storage/project_transcript.go`（新建） | `projectTranscript`：事件流 → `[]port.InferenceMessage`，**按 call_id 配对** |
| `internal/storage/project_transcript_test.go`（新建） | 配对、乱序、未答调用、预览不是全文 |
| `internal/storage/sqlite.go`（修改） | 新增 `ListConversationTranscript`，与 `ListConversationTurns` 并列 |
| `internal/port/ports.go`（修改） | 读取端口加 `ListConversationTranscript` |
| `internal/runtime/session_turns.go`（修改） | 按开关选择走 turns 还是 transcript |
| `internal/runtime/runtime.go`（修改） | 打开时把 transcript 拼进 `conversation`，关闭时一个字节都不动 |
| `internal/runtime/transcript_volume_test.go`（新建） | **判据**：同一批事件，开/关两种形状的体积差可度量 |

`project_transcript.go` 单独成文件：它是纯函数，且「怎么把事件配成合法 transcript」是一组自成一体的规则——与 `project_turns.go` 并列，谁也不改谁。

---

## Task 1: 配置开关（默认关）

**Files:**
- Modify: `internal/config/config.go:465-472`（`SessionConfig`）
- Test: `internal/config/config_test.go`（在既有文件里加）

**Interfaces:**
- Produces: `config.SessionConfig.ToolTranscriptEnabled bool`，JSON 键 `tool_transcript_enabled`，**零值 false = 关**

- [ ] **Step 1: 写失败的测试**

加到 `internal/config/config_test.go`：

```go
// G3 默认必须是关。spec §3 的理由原文：「它改的是每次请求的体积，不该在做轨迹的
// 顺路上悄悄打开」。所以「配置里没写」必须解析成 false，而不是任何形式的默认开。
func TestToolTranscriptIsOffWhenTheKeyIsAbsent(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"session":{"enabled":true}}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Session.ToolTranscriptEnabled {
		t.Error("session.tool_transcript_enabled 缺席时被解析成了 true：G3 必须默认关")
	}
}

// 显式打开要生效——否则这个开关等于不存在。
func TestToolTranscriptCanBeTurnedOn(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"session":{"enabled":true,"tool_transcript_enabled":true}}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if !cfg.Session.ToolTranscriptEnabled {
		t.Error("显式配 true 没有生效")
	}
}
```

> **实现者注意**：`ParseConfig` 的真实函数名与签名以本包既有测试为准（`internal/config/config_test.go` 里一定有解析配置的既有写法）。**先读它**，照它写，不要新造一套。若解析入口不是 `ParseConfig`，用真实的那个并在报告里说明。

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/config/ -run "ToolTranscript" -count=1 -timeout 5m`

Expected: 编译失败 `cfg.Session.ToolTranscriptEnabled undefined`。**把真实输出记进报告。**

- [ ] **Step 3: 写实现**

`internal/config/config.go` 的 `SessionConfig` 加一个字段：

```go
type SessionConfig struct {
	Enabled                 bool `json:"enabled"`
	DefaultRecentTurns      int  `json:"default_recent_turns"`
	MaxTurnChars            int  `json:"max_turn_chars"`
	RestoreLatestOnTUIStart bool `json:"restore_latest_on_tui_start"`
	CacheEnabled            bool `json:"cache_enabled"`
	CacheMaxEntries         int  `json:"cache_max_entries"`
	// ToolTranscriptEnabled 打开后，注入模型的会话历史从「Recent conversation:」
	// 文本块换成 provider transcript——assistant 消息带 tool_calls，其后跟与之
	// call_id 配对的 tool 消息（spec §6 的 G3）。模型因此能看见历史的工具往返，
	// 不再在会话恢复时失忆。
	//
	// 零值 false 就是关，而且必须是关：它改的是**每次请求的体积**（可能涨数倍），
	// spec §3 明确「不该在做轨迹的顺路上悄悄打开」。这是一次单独的、可度量的决定。
	ToolTranscriptEnabled bool `json:"tool_transcript_enabled"`
}
```

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/config/ -run "ToolTranscript" -count=1 -timeout 5m -v`

Expected: 两条都 `--- PASS`。核对 `-run` 真的匹配到了这两个函数名（`grep -n "^func TestToolTranscript" internal/config/config_test.go`），不要接受 `[no tests to run]` 却报 `ok`。

再跑全量：`go test ./... -count=1 -p 1 -timeout 40m`

- [ ] **Step 5: 变异验证（两条）**

| # | 变异 | 期望红在哪 |
|---|---|---|
| 1 | 给字段加 `json:"tool_transcript_enabled,omitempty"` 之外的默认值逻辑（例如解析后 `if !set { v = true }`） | `TestToolTranscriptIsOffWhenTheKeyIsAbsent` |
| 2 | 把 JSON 标签改成别的名字（如 `tool_transcript`） | `TestToolTranscriptCanBeTurnedOn` |

每条：改 → 跑 → 贴真实 FAIL 输出 → `git checkout --` 还原 → `git status --short` 确认为空。

**注意**：变异只造成编译失败不算变异验证——改成能编译但行为错的形式再跑。

- [ ] **Step 6: 提交**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): G3 开关（默认关）"
```

---

## Task 2: `projectTranscript` —— 按 call_id 配对的纯函数

**Files:**
- Create: `internal/storage/project_transcript.go`
- Create: `internal/storage/project_transcript_test.go`

**Interfaces:**
- Consumes: `domain.SessionEvent`、P2/P4a 落进事件的载荷
- Produces: `func projectTranscript(sessionID string, events []domain.SessionEvent) ([]port.InferenceMessage, error)`

**这是 spec §4.3.1 第 2 条的兑现处。** P3 的 Task 2 实现者当时论证过：本层没有配对代码可验证，真正的配对时机在 P5。就是现在。

**它是纯函数**：不碰数据库、不读文件、不看时钟。所以能脱离 SQLite 直接单测——与 `projectTurns` 同一个范式。

- [ ] **Step 1: 写失败的测试**

`internal/storage/project_transcript_test.go`：

```go
package storage

import (
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// 一轮「问 → 调工具 → 答」投影成 provider transcript 的标准形状：
// user、assistant(带 tool_calls)、tool(带 tool_call_id)、assistant。
func TestOneToolRoundProjectsToAValidTranscript(t *testing.T) {
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "读 notes.md",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content": "我读一下",
			"tool_calls": []any{
				map[string]any{"call_id": "c1", "name": "read_file"},
			},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": `{"path":"notes.md"}`,
		}),
		evWith(5, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "文件的前几行", "is_error": false,
		}),
		evWith(6, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "completed"}),
		evWith(7, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 1}),
		evWith(8, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 1, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content": "读完了", "tool_calls": []any{},
			"usage":         map[string]any{"prompt": 2, "completion": 1, "cached": 0, "total": 3},
			"model_profile": "fast",
		}),
		evWith(9, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 1, "reason": "completed"}),
		evWith(10, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "completed"}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}

	if len(msgs) != 4 {
		t.Fatalf("投影出 %d 条消息，要 4 条（user / assistant+calls / tool / assistant）：%+v", len(msgs), msgs)
	}
	if msgs[0].Role != port.RoleUser || msgs[0].Content != "读 notes.md" {
		t.Errorf("msgs[0] = %+v，要 user「读 notes.md」", msgs[0])
	}
	if msgs[1].Role != port.RoleAssistant {
		t.Fatalf("msgs[1].Role = %q，要 assistant", msgs[1].Role)
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].ID != "c1" {
		t.Errorf("msgs[1].ToolCalls = %+v，要一条 call_id=c1", msgs[1].ToolCalls)
	}
	if msgs[2].Role != port.RoleTool || msgs[2].ToolCallID != "c1" {
		t.Errorf("msgs[2] = %+v，要 tool 且 tool_call_id=c1", msgs[2])
	}
	if msgs[3].Role != port.RoleAssistant || msgs[3].Content != "读完了" {
		t.Errorf("msgs[3] = %+v，要 assistant「读完了」", msgs[3])
	}
}

// spec §4.3.1 第 2 条：崩溃恢复补出的 tool/result 排在日志**尾部**，可能排在
// step/end、turn/end 之后，而且顺序与调用顺序相反。按位置配会把结果配到错的
// 调用上；必须按 call_id 配。
//
// provider 会拒绝配不上的 tool 消息（ports.go 的 Validate 注释写死了），
// 所以这不是审美问题——配错就发不出去。
func TestResultsPairByCallIDEvenWhenTheyTrailAtTheEnd(t *testing.T) {
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "并行读两个文件",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content": "同时读",
			"tool_calls": []any{
				map[string]any{"call_id": "c1", "name": "read_file"},
				map[string]any{"call_id": "c2", "name": "read_file"},
			},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
		}),
		evWith(5, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "name": "read_file", "arguments": "{}",
		}),
		// 崩了。恢复补出来的两条结果排在尾部，且顺序与调用相反。
		evWith(6, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "preview": "c2 的内容", "is_error": true,
		}),
		evWith(7, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "c1 的内容", "is_error": true,
		}),
		evWith(8, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "interrupted"}),
		evWith(9, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "interrupted"}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}

	// 找出两条 tool 消息，按 call_id 核对内容——**按位置配的实现会在这里把
	// c1 的内容配给 c2**。
	byCallID := map[string]string{}
	for _, m := range msgs {
		if m.Role == port.RoleTool {
			byCallID[m.ToolCallID] = m.Content
		}
	}
	if len(byCallID) != 2 {
		t.Fatalf("tool 消息 %d 条，要 2 条：%+v", len(byCallID), msgs)
	}
	if !strings.Contains(byCallID["c1"], "c1 的内容") {
		t.Errorf("c1 配到了 %q——按位置配对会把尾部乱序的结果配错", byCallID["c1"])
	}
	if !strings.Contains(byCallID["c2"], "c2 的内容") {
		t.Errorf("c2 配到了 %q", byCallID["c2"])
	}
}

// 每条 tool 消息前面必须有带同 call_id 的 assistant tool_calls，否则
// provider 拒收整个请求（ports.go 的 Validate 注释）。这条把「合法 transcript」
// 当成不变量来验，而不是只看字段填没填。
func TestEveryToolMessageIsPrecededByItsCall(t *testing.T) {
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "干活",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":       "调工具",
			"tool_calls":    []any{map[string]any{"call_id": "c1", "name": "read_file"}},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
		}),
		evWith(5, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "结果", "is_error": false,
		}),
		evWith(6, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "completed"}),
		evWith(7, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "completed"}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}

	announced := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case port.RoleAssistant:
			for _, c := range m.ToolCalls {
				announced[c.ID] = true
			}
		case port.RoleTool:
			if m.ToolCallID == "" {
				t.Fatalf("msgs[%d] 是 tool 但没有 tool_call_id——provider 会拒收整个请求", i)
			}
			if !announced[m.ToolCallID] {
				t.Errorf("msgs[%d] 的 tool_call_id=%q 之前没有任何 assistant 宣告过它——provider 会拒收", i, m.ToolCallID)
			}
		}
	}
}

// 一条被记录过、但没有结果事件的调用只可能来自进程硬崩（spec §4.3.1 第 1 条）。
// 它不能被原样放进 transcript——那会产出一条永远等不到 tool 消息的 tool_calls，
// provider 同样拒收。
func TestAnUnansweredCallIsNotAnnouncedInTheTranscript(t *testing.T) {
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "干活",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content": "调两个",
			"tool_calls": []any{
				map[string]any{"call_id": "c1", "name": "read_file"},
				map[string]any{"call_id": "c2", "name": "read_file"},
			},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
		}),
		evWith(5, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "name": "read_file", "arguments": "{}",
		}),
		// 只有 c1 有结果；c2 是硬崩留下的未答调用。
		evWith(6, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "c1 的结果", "is_error": false,
		}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}

	for i, m := range msgs {
		if m.Role != port.RoleAssistant {
			continue
		}
		for _, c := range m.ToolCalls {
			if c.ID == "c2" {
				t.Errorf("msgs[%d] 宣告了没有结果的 c2——provider 会等一条永远不来的 tool 消息", i)
			}
		}
	}
}

// 内容是**预览**不是全文（spec §6：全文仍靠 read_file 按定位符取）。
// 这条守的是 G3 的成本上界：把全文塞进每次请求正是它要避免的事。
func TestToolContentIsThePreviewNotTheWholeText(t *testing.T) {
	const preview = "只有这一小段"
	msgs, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "读",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":       "读",
			"tool_calls":    []any{map[string]any{"call_id": "c1", "name": "read_file"}},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
		}),
		evWith(5, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": preview, "is_error": false,
			"spill_locator": ".stardust/cache/read_file-abc.md",
		}),
		evWith(6, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "completed"}),
		evWith(7, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "completed"}),
	})
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}

	for _, m := range msgs {
		if m.Role != port.RoleTool {
			continue
		}
		if !strings.Contains(m.Content, preview) {
			t.Errorf("tool 消息没有带预览：%q", m.Content)
		}
		// 定位符可以出现（让模型知道去哪取全文），但全文本身绝不能在这里。
		if len([]rune(m.Content)) > 500 {
			t.Errorf("tool 消息 %d runes，太长了——G3 的成本上界就是靠「只放预览」守的", len([]rune(m.Content)))
		}
	}
}

// 未知事件类型必须报错并指名，不能静默跳过——server 侧的类型是闭集但它会长，
// 静默跳过意味着加了新类型后 transcript 会悄悄少东西。
func TestAnUnknownEventTypeIsRefusedByName(t *testing.T) {
	_, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventType("session/teleport"), map[string]any{"turn": 0}),
	})
	if err == nil {
		t.Fatal("未知事件类型没有被拒绝")
	}
	if !strings.Contains(err.Error(), "session/teleport") {
		t.Errorf("错误没有指名那个未知类型：%v", err)
	}
}
```

> **实现者注意**：`evWith` 是 P3 建的同包辅助函数（`internal/storage/project_turns_test.go`），**直接用，不要新建也不要改名成 `ev`**——`session_events_test.go:33` 已有一个签名不同的 `ev`，重名会直接编译失败。

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/storage/ -run "TranscriptProjects|PairByCallIDEvenWhen|PrecededByItsCall|UnansweredCallIsNot|PreviewNotTheWhole|UnknownEventTypeIsRefused" -count=1 -timeout 10m`

Expected: 编译失败 `undefined: projectTranscript`。**把真实输出记进报告。**

> 上面的 `-run` 模式是按测试函数名的**片段**拼的。跑之前先 `grep -n "^func Test" internal/storage/project_transcript_test.go` 核对每一段都匹配得到——本仓反复栽在「跑出 `[no tests to run]` 却报 ok」上。

- [ ] **Step 3: 写实现**

`internal/storage/project_transcript.go`：

```go
package storage

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// projectTranscript 把一条会话的事件流投影成 provider transcript——assistant
// 消息带 tool_calls，其后跟与之 call_id 配对的 tool 消息（spec §6 的 G3）。
//
// 它与 projectTurns 并列而不是取代它：turns 是「谁说了什么」的视图（G3 关闭时
// 走它），transcript 是「模型看到的完整往返」（G3 打开时走它）。两者读同一批
// 事件，谁也不改谁。
//
// **按 call_id 配对，不按位置**（spec §4.3.1 第 2 条）：崩溃恢复补出的
// tool/result 排在日志尾部，可能排在自己那次调用的 step/end 之后、且顺序与调用
// 相反。按位置配会把结果配到错误的调用上——而 port.InferenceMessage 的 Validate
// 注释写死了「an OpenAI-compatible provider rejects a tool message it cannot
// pair with a preceding tool call」，配错的后果是整个请求被 provider 拒收。
//
// **只宣告有结果的调用**：一条被记录过却没有结果事件的 tool/call 只可能来自进程
// 硬崩（spec §4.3.1 第 1 条）。把它放进 tool_calls 会让 provider 等一条永远不来
// 的 tool 消息，同样拒收。所以先扫一遍结果、再决定宣告什么。
//
// 纯函数：不碰数据库、不读文件、不看时钟。
func projectTranscript(sessionID string, events []domain.SessionEvent) ([]port.InferenceMessage, error) {
	// 第一遍：把所有结果按 call_id 收起来。必须先扫完，因为恢复补出的结果排在尾部。
	results, err := collectToolResults(sessionID, events)
	if err != nil {
		return nil, err
	}

	msgs := make([]port.InferenceMessage, 0, len(events)/3)
	for _, event := range events {
		switch event.Type {
		case domain.SessionEventUserMessage:
			var payload struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, fmt.Errorf("project transcript for %q: decode user/message at seq %d: %w",
					sessionID, event.Seq, err)
			}
			msgs = append(msgs, port.InferenceMessage{
				Role:    port.RoleUser,
				Content: payload.Content,
			})

		case domain.SessionEventAssistantMessage:
			var payload struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					CallID string `json:"call_id"`
					Name   string `json:"name"`
				} `json:"tool_calls"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, fmt.Errorf("project transcript for %q: decode assistant/message at seq %d: %w",
					sessionID, event.Seq, err)
			}
			// 只宣告有结果的调用——见函数头那段。
			calls := make([]domain.ToolCall, 0, len(payload.ToolCalls))
			for _, c := range payload.ToolCalls {
				if _, answered := results[c.CallID]; !answered {
					continue
				}
				calls = append(calls, domain.ToolCall{ID: c.CallID, Name: c.Name})
			}
			msg := port.InferenceMessage{Role: port.RoleAssistant, Content: payload.Content}
			if len(calls) > 0 {
				msg.ToolCalls = calls
			}
			msgs = append(msgs, msg)
			// 紧跟着把这批调用的结果按 call_id 取出来铺在后面——provider 要求
			// tool 消息紧随宣告它的 assistant。
			for _, c := range calls {
				msgs = append(msgs, port.InferenceMessage{
					Role:       port.RoleTool,
					ToolCallID: c.ID,
					Content:    results[c.ID],
				})
			}

		case domain.SessionEventTurnStart, domain.SessionEventTurnEnd,
			domain.SessionEventStepStart, domain.SessionEventStepEnd,
			domain.SessionEventToolCall, domain.SessionEventToolResult:
			// 这六类不直接产出消息：边界事件没有模型可读的内容；tool/call 的信息
			// 已经在 assistant 的 tool_calls 里；tool/result 已在第一遍收进 results。
			//
			// 逐条列出而不是 default 跳过，是为了让「加了新事件类型却忘了决定它
			// 怎么投影」落到下面那条 default 的运行期报错上，而不是悄悄少算。

		default:
			return nil, fmt.Errorf("project transcript for %q: unknown event type %q at seq %d",
				sessionID, event.Type, event.Seq)
		}
	}
	return msgs, nil
}

// collectToolResults 扫一遍事件，把每条 tool/result 的模型可读内容按 call_id 收起来。
//
// 单独一遍是必须的：恢复补出的结果排在日志尾部，边走边配会漏掉它们。
func collectToolResults(sessionID string, events []domain.SessionEvent) (map[string]string, error) {
	results := make(map[string]string)
	for _, event := range events {
		if event.Type != domain.SessionEventToolResult {
			continue
		}
		var payload struct {
			CallID       string `json:"call_id"`
			Preview      string `json:"preview"`
			IsError      bool   `json:"is_error"`
			SpillLocator string `json:"spill_locator"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil, fmt.Errorf("project transcript for %q: decode tool/result at seq %d: %w",
				sessionID, event.Seq, err)
		}
		if strings.TrimSpace(payload.CallID) == "" {
			return nil, fmt.Errorf("project transcript for %q: tool/result at seq %d has no call_id, so it cannot be paired",
				sessionID, event.Seq)
		}
		results[payload.CallID] = renderTranscriptToolContent(payload.Preview, payload.IsError, payload.SpillLocator)
	}
	return results, nil
}

// renderTranscriptToolContent 把一条结果渲染成模型看到的文本。
//
// 只放**预览**，不放全文（spec §6：全文仍靠 read_file 按定位符取）——这正是 G3
// 的成本上界所在。定位符会一并给出，让模型知道去哪取全文；出错的结果显式标出，
// 否则模型会把一条错误当成数据。
func renderTranscriptToolContent(preview string, isError bool, spillLocator string) string {
	var b strings.Builder
	if isError {
		b.WriteString("[错误] ")
	}
	b.WriteString(preview)
	if strings.TrimSpace(spillLocator) != "" {
		b.WriteString("\n[全文见 ")
		b.WriteString(spillLocator)
		b.WriteString("，用 read_file 按需翻页]")
	}
	return b.String()
}
```

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/storage/ -run "TranscriptProjects|PairByCallIDEvenWhen|PrecededByItsCall|UnansweredCallIsNot|PreviewNotTheWhole|UnknownEventTypeIsRefused" -count=1 -timeout 10m -v`

Expected: 六条都 `--- PASS`。

再跑全量：`go test ./... -count=1 -p 1 -timeout 40m`

- [ ] **Step 5: 变异验证（四条）**

| # | 变异 | 期望红在哪 |
|---|---|---|
| 1 | tool 消息的内容改成按**位置**取（`results` 换成一个按出现顺序消费的切片） | `TestResultsPairByCallIDEvenWhenTheyTrailAtTheEnd` |
| 2 | 去掉「只宣告有结果的调用」那条 `continue` | `TestAnUnansweredCallIsNotAnnouncedInTheTranscript` |
| 3 | `default` 分支改成 `continue`（静默跳过未知类型） | `TestAnUnknownEventTypeIsRefusedByName` |
| 4 | tool 消息不填 `ToolCallID` | `TestEveryToolMessageIsPrecededByItsCall` |

**变异 1 是本任务最重要的一条**——它就是 spec §4.3.1 第 2 条。若它不红，说明那条测试没在验配对。

每条：改 → 跑 → 贴真实 FAIL 输出 → 还原 → `git status --short` 确认为空。

- [ ] **Step 6: 提交**

```bash
git add internal/storage/project_transcript.go internal/storage/project_transcript_test.go
git commit -m "feat(storage): 事件到 provider transcript 的投影，按 call_id 配对"
```

---

## Task 3: 接线 —— 开关选择走哪条路

**Files:**
- Modify: `internal/storage/sqlite.go`（新增 `ListConversationTranscript`）
- Modify: `internal/port/ports.go`（读取端口加方法）
- Modify: `internal/runtime/session_turns.go`
- Modify: `internal/runtime/runtime.go:795-810`
- Test: `internal/runtime/transcript_wiring_test.go`（新建）

**Interfaces:**
- Consumes: Task 1 的 `config.SessionConfig.ToolTranscriptEnabled`、Task 2 的 `projectTranscript`
- Produces: `(*SQLiteRepository).ListConversationTranscript(ctx, sessionID string, limit int) ([]port.InferenceMessage, error)`；G3 打开时 `conversation` 里多出历史 transcript

**这是本期的接线任务**，也是「接缝在但没人调用它」的高发区（本仓栽过两次）。

### 排布（本计划开头那节的落地）

G3 打开时：

```
messages[0] = {user, 整段 prompt}        ← StablePrefixLen 照旧打在这里
messages[1..n] = 历史 transcript
```

**历史排在 messages[0] 之后**。理由是缓存：`messages[0]` 必须仍以那段跨任务逐字节相同的稳定前缀开头，否则每次请求都缓存未命中——G3 的代价本就是体积，再赔上缓存不划算。

代价是 `header` 段里的 `Input:` 出现在历史之前。**这是有意的取舍**，要写进注释。

**G3 打开时，`conversationBlock` 那段文本必须不再拼进 prompt**——否则历史会出现两次（一次文本块、一次 transcript），体积白涨一倍。

- [ ] **Step 1: 写失败的测试**

`internal/runtime/transcript_wiring_test.go`：

```go
package runtime

import (
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/port"
)

// 关闭时（默认）行为必须与今天逐字节相同：历史仍是 prompt 里那段
// "Recent conversation:" 文本块，messages 里没有任何 tool 角色。
//
// 这条守的是 spec §3 那句「不该在做轨迹的顺路上悄悄打开」。
func TestWithTheSwitchOffHistoryStaysInThePromptText(t *testing.T) {
	// 实现者：照本包既有的 runtime 测试搭法建一个跑得起来的 Runtime
	// （newTestRuntimeWithEvents 一类），会话里预置一轮带工具往返的历史，
	// 开关保持默认（关），跑一个任务，然后断言送给模型的 messages：
	//   1. 没有任何 Role == port.RoleTool 的消息；
	//   2. messages[0].Content 里含 "Recent conversation:"。
	// 断言的是**送到 provider 的东西**，不是「代码里有那个分支」。
	t.Fatal("实现者：按上面的说明写出真实断言，然后删掉这一行")
}

// 打开时历史变成 transcript：出现 tool 角色的消息，且每条都能在它前面找到
// 宣告它的 assistant。
func TestWithTheSwitchOnHistoryBecomesATranscript(t *testing.T) {
	// 实现者：同上搭法，但把 SessionConfig.ToolTranscriptEnabled 设为 true。
	// 断言：
	//   1. 存在 Role == port.RoleTool 的消息；
	//   2. 每条 tool 消息的 ToolCallID 都被前面某条 assistant 的 ToolCalls 宣告过；
	//   3. messages[0].Content 里**不再**含 "Recent conversation:"（否则历史进了两次）。
	t.Fatal("实现者：按上面的说明写出真实断言，然后删掉这一行")
}

// 缓存断点不能被这条改动挪走：StablePrefixLen 仍打在 messages[0]，
// 且 messages[0] 仍以那段稳定前缀开头。
func TestTheCacheBreakpointStaysOnTheFirstMessage(t *testing.T) {
	// 实现者：开关打开，跑一个任务，断言 messages[0].StablePrefixLen > 0
	// 且其余消息的 StablePrefixLen 都是 0。
	t.Fatal("实现者：按上面的说明写出真实断言，然后删掉这一行")
}

// assertTranscriptIsValid 是上面几条共用的断言：每条 tool 消息前面都有宣告它的
// assistant。provider 拒收配不上的 tool 消息（port.InferenceMessage 的 Validate
// 注释），所以这是「能不能发出去」的判据，不是风格问题。
func assertTranscriptIsValid(t *testing.T, msgs []port.InferenceMessage) {
	t.Helper()
	announced := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case port.RoleAssistant:
			for _, c := range m.ToolCalls {
				announced[c.ID] = true
			}
		case port.RoleTool:
			if strings.TrimSpace(m.ToolCallID) == "" {
				t.Fatalf("msgs[%d] 是 tool 但没有 tool_call_id——provider 会拒收整个请求", i)
			}
			if !announced[m.ToolCallID] {
				t.Errorf("msgs[%d] 的 tool_call_id=%q 之前没有 assistant 宣告过", i, m.ToolCallID)
			}
		}
	}
}
```

> **实现者注意**：三条测试体留了 `t.Fatal` 占位，**必须替换成真实断言**。用 `t.Fatal` 而不是 `t.Skip` 是有意的：`t.Skip` 挡不住编译期检查、留着毫无意义（本仓栽过），`t.Fatal` 会让它永远红、逼你写完。
>
> 它们必须照本包既有的 runtime 测试搭法写（`internal/runtime/eventlog_integration_test.go` 里有接真 `storage.OpenSQLite` 的 `newTestRuntimeWithEvents` 一类），而那套夹具我不能凭空写对。**动手前先读它。**
>
> **假模型要按请求内容作答，不要按调用次数**——本仓栽过：按次数的假模型会让「工具不存在」之类的错误路径被伪装成成功路径。

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/runtime/ -run "SwitchOffHistoryStays|SwitchOnHistoryBecomes|CacheBreakpointStays" -count=1 -timeout 10m`

Expected: 三条都 FAIL（`t.Fatal` 占位）。**把真实输出记进报告。**

- [ ] **Step 3: 写实现**

**(a) 存储层**——`internal/storage/sqlite.go` 加一个与 `ListConversationTurns` 并列的方法：

```go
// ListConversationTranscript 返回一条会话的历史，形状是 provider transcript
// （spec §6 的 G3）。limit > 0 时只保留**最近** limit 条消息。
//
// 与 ListConversationTurns 并列而不是取代它：G3 关闭时走 turns，打开时走这里。
// 两者读同一批事件，谁也不改谁。
//
// 用 ReadFrom(0) 而不是 Load：spec §4.3.1 第 3 条要求 Load 只对「确定没有活跃
// 写入者」的会话调用，而这个方法在任务执行期间也会被调用。
func (r *SQLiteRepository) ListConversationTranscript(ctx context.Context, sessionID string, limit int) ([]port.InferenceMessage, error) {
	events, err := r.ReadFrom(ctx, sessionID, 0)
	if err != nil {
		return nil, fmt.Errorf("list conversation transcript for %q: %w", sessionID, err)
	}
	msgs, err := projectTranscript(sessionID, events)
	if err != nil {
		return nil, fmt.Errorf("list conversation transcript for %q: %w", sessionID, err)
	}
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	return msgs, nil
}
```

> **实现者注意**：从尾部截断可能把一条 tool 消息与宣告它的 assistant 切散，产出 provider 拒收的 transcript。**请自己判断怎么处理**——往前多留到最近的 user 边界、或截断后再做一次合法性收敛都可以。**在报告里说明你选了哪种、为什么**，并补一条测试：`limit` 恰好切在 assistant 与它的 tool 之间时，结果仍是合法 transcript。

**(b) 端口**——`internal/port/ports.go` 的读取端口加上这个方法（**具体是哪个接口由你定位**：`ListConversationTurns` 声明在哪，就加在哪的旁边）。

**(c) 选路**——`internal/runtime/session_turns.go` 按开关选择。它今天返回 `[]domain.ConversationTurn`；G3 打开时要返回 transcript，两者类型不同，所以**不要硬塞进同一个返回值**。请自己判断形状（例如返回一个带两种可能的小结构体，或加一个并列函数），**在报告里说明**。

**(d) 组装**——`internal/runtime/runtime.go:795-810`：

```go
// G3 打开时，历史以 provider transcript 的形式排在 message[0] 之后；关闭时
// 历史仍在 basePrompt 的 "Recent conversation:" 段里，这里一个字节都不动。
//
// 为什么历史排在 message[0] 之后而不是之前：message[0] 必须仍以那段跨任务
// 逐字节相同的稳定前缀开头，pinCachePrefix 打在它上面、adapter 据此设 provider
// 的缓存断点。历史插到前面会让每次请求都缓存未命中——G3 的代价本就是体积，
// 再赔上缓存不划算。
//
// 代价是 basePrompt 的 header 段里那句 "Input: <当前任务输入>" 出现在历史之前。
// 这是有意的取舍：缓存命中比时序美观值钱。
convo := newConversation(basePrompt, task.Images)
convo.pinCachePrefix(stablePrefixLen)
if len(historyTranscript) > 0 {
	convo.appendHistory(historyTranscript)
}
```

> `appendHistory` 需要你在 `internal/runtime/messages.go` 里加（照 `appendAssistant` / `appendToolResults` 的风格）。**它必须原样追加，不得改写 `StablePrefixLen`**——那个字段只在 `messages[0]` 上有意义。

**(e) G3 打开时不要再拼文本块**——`cognitive` 侧的 `add("conversation", ...)` 在开关打开时应当拿到空的 turns（或跳过）。**具体怎么传由你判断**，但结果必须是：打开时 prompt 里**没有** `Recent conversation:`。这一条有测试守着（Step 1 的第二条）。

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/runtime/ -run "SwitchOffHistoryStays|SwitchOnHistoryBecomes|CacheBreakpointStays" -count=1 -timeout 10m -v`

再跑全量：`go build ./... && go vet ./... && go test ./... -count=1 -p 1 -timeout 40m`

**全量这一步是本任务的重点**：你改了 `session_turns.go` 与 `runtime.go` 的组装，那是被到处依赖的路径。**任何既有测试变红都是「行为变了」的直接证据，不要改测试去迁就实现**——先判断是实现错了还是那条测试原本在断言旧形状，把判断写进报告。

- [ ] **Step 5: 变异验证（四条）**

| # | 变异 | 期望红在哪 |
|---|---|---|
| 1 | 忽略开关，恒走 transcript | `TestWithTheSwitchOffHistoryStaysInThePromptText` |
| 2 | 忽略开关，恒走文本块 | `TestWithTheSwitchOnHistoryBecomesATranscript` |
| 3 | 打开时**同时**保留文本块与 transcript | `TestWithTheSwitchOnHistoryBecomesATranscript`（第 3 条断言） |
| 4 | `appendHistory` 把 `StablePrefixLen` 也拷到历史消息上 | `TestTheCacheBreakpointStaysOnTheFirstMessage` |

变异 1 与 2 是**接线守卫**：开关不生效，这条改动就等于没做，而它不会报任何错。

- [ ] **Step 6: 提交**

```bash
git add internal/storage/sqlite.go internal/port/ports.go internal/runtime/session_turns.go internal/runtime/runtime.go internal/runtime/messages.go internal/runtime/transcript_wiring_test.go
git commit -m "feat(runtime): G3 打开时历史以 provider transcript 进模型"
```

---

## Task 4: 体积度量 —— 把判据落成可跑的东西

**Files:**
- Create: `internal/runtime/transcript_volume_test.go`
- Modify: `internal/cognitive/core.go`（`Blocks` 里补一项，见 Step 3）

**Interfaces:**
- Consumes: Task 2 的 `projectTranscript`、Task 3 的接线
- Produces: 一条把「开/关两种形状的体积差」量出来的测试；`BuiltContext.Blocks` 里能看到历史那一段的归属

**spec §9 的 P5 判据原文**：「打开后 token 体积变化**可度量**」。这一任务把它从一句话变成可跑的东西。

**现成的基础设施**：`cognitive.BuiltContext.Blocks`（`core.go:42`）已经是 per-section 的体积核算，`BlockSize{Name, Chars}`；`runtime/debug_probe.go:80` 的 `logContextBlocks` 已经在按 block 记日志。度量应当接在这上面，**不要新造一套**。

- [ ] **Step 1: 写失败的测试**

`internal/runtime/transcript_volume_test.go`：

```go
package runtime

import (
	"testing"
)

// P5 的判据（spec §9）：「打开后 token 体积变化可度量」。
//
// 这条测试就是那个度量：同一批历史事件，开与关两种形状各自送出去多少字符，
// 差值必须能算出来、且必须是正的（transcript 带上了工具往返，一定更大）。
//
// 它不是性能测试——不断言差值的具体数值（那会随夹具变），只断言**这个差值
// 是可测量的**，并把两个数字打进测试输出，让人能看见代价有多大。
func TestTheSwitchVolumeDifferenceIsMeasurable(t *testing.T) {
	// 实现者：用与 Task 3 相同的夹具搭两个 Runtime（一个开、一个关），
	// 喂同一批历史事件，各跑一个任务，量出送到假模型的 messages 的总字符数。
	//
	// 断言：
	//   1. 两个数字都 > 0；
	//   2. 开 > 关（transcript 多带了工具往返）；
	//   3. 用 t.Logf 把 "off=%d on=%d delta=%d ratio=%.2fx" 打出来——
	//      这就是判据要的「可度量」，人跑一次测试就能看见代价。
	t.Fatal("实现者：按上面的说明写出真实断言，然后删掉这一行")
}

// 历史那一段的体积必须在 Blocks 里可归因——「prompt 涨了 2 KB」应该能回答
// 「是谁涨的」。这是本仓 plugin_prompt 段已经立下的规矩（core.go 的注释）。
func TestHistoryVolumeIsAttributableInBlocks(t *testing.T) {
	// 实现者：跑一个带历史的任务，断言 BuiltContext.Blocks 里能找到历史那一段
	// （关闭时是 "conversation"；打开时历史不在 prompt 里，所以该段应当为 0 或缺席，
	// 而 transcript 的体积体现在 messages 上——请把这个区别在测试里写清楚）。
	t.Fatal("实现者：按上面的说明写出真实断言，然后删掉这一行")
}
```

> 两处 `t.Fatal` 是**故意的占位**，必须替换。理由同 Task 3。

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/runtime/ -run "VolumeDifferenceIsMeasurable|VolumeIsAttributable" -count=1 -timeout 10m`

Expected: 两条都 FAIL。

- [ ] **Step 3: 写实现**

**(a)** 把两条测试写实（见 Step 1 的说明）。

**(b)** `internal/cognitive/core.go`：G3 打开时 `conversation` 段为空，`add()` 会因为 `body == ""` 直接 return，于是 `Blocks` 里**没有**这一项。这会让「历史去哪了」变得不可归因。

请在打开时也记一条零长度的 block（或一条说明历史走了 transcript 的 block），使 `Blocks` 始终能回答「历史这一段占了多少」。**具体形式你定**，但要满足：跑一次任务，从 `Blocks` 能看出历史是走了 prompt 还是走了 transcript。把你的选择写进报告。

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/runtime/ -run "VolumeDifferenceIsMeasurable|VolumeIsAttributable" -count=1 -timeout 10m -v`

Expected: 两条 PASS，且 `-v` 输出里能看到 `off=... on=... delta=... ratio=...x` 那一行。**把它贴进报告**——那就是 P5 判据的证据。

再跑全量：`go build ./... && go vet ./... && go test ./... -count=1 -p 1 -timeout 40m`

- [ ] **Step 5: 变异验证（两条）**

| # | 变异 | 期望红在哪 |
|---|---|---|
| 1 | 让开关打开时也不带工具往返（transcript 里只留 user/assistant） | `TestTheSwitchVolumeDifferenceIsMeasurable`（差值不再为正） |
| 2 | 去掉 Step 3(b) 里补的那条 block | `TestHistoryVolumeIsAttributableInBlocks` |

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/transcript_volume_test.go internal/cognitive/core.go
git commit -m "test(runtime): 把 G3 的体积代价量出来"
```

---

## 完成判据（P5 全部做完时逐条核对）

- [ ] 配置项 `session.tool_transcript_enabled` 存在，**零值 = 关**
- [ ] **关闭时行为与今天逐字节相同**（历史仍是 `Recent conversation:` 文本块，messages 里没有 tool 角色）——有测试守着
- [ ] 打开时历史以 provider transcript 进模型：assistant 带 `tool_calls`，其后跟同 `call_id` 的 tool 消息
- [ ] **按 `call_id` 配对而非按位置**（spec §4.3.1 第 2 条）——恢复补出的尾部乱序结果不会配错
- [ ] **未答的调用不被宣告**（spec §4.3.1 第 1 条的另一面）——否则 provider 等一条永远不来的 tool 消息
- [ ] 每条 tool 消息都能在它前面找到宣告它的 assistant（provider 拒收配不上的 tool 消息）
- [ ] 内容是**预览**不是全文；定位符给出，全文靠 `read_file` 取
- [ ] 打开时 prompt 里**没有** `Recent conversation:`（历史不进两次）
- [ ] `StablePrefixLen` 仍在 `messages[0]`，其余消息为 0
- [ ] 读路径用 `ReadFrom`，**全条路径没有 `Load`**
- [ ] **体积差可度量**：一条测试打出 `off=... on=... delta=... ratio=...x`
- [ ] `go build`/`go vet`/`go test ./...` 全绿，`gofmt -l` 为空
- [ ] **P5 没有碰**：投影缓存、虚拟滚动、`assistant/chunk` 入库

## 本期已知、不在范围内的事

- 投影缓存不做（spec §6：「投影在真实会话长度上测出慢时」再加）——`ListConversationTranscript` 每次全量读并投影，与 `ListConversationTurns` 同一个量级
- `assistant/chunk` 不入库（spec §10：需要事后逐 token 回放流式过程时再考虑）
- 打开后 `header` 段的 `Input:` 出现在历史之前，是为保住缓存断点做的有意取舍
- 未绑定 `working_dir` 的会话，其 `spill_locator` 取不回来（P4a 的已知取舍）——transcript 里仍会给出定位符，模型按它调 `read_file` 会失败并得到一条明确的错误
