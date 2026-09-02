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
func TestAnUnknownEventTypeIsRefusedByNameInTheTranscript(t *testing.T) {
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

// 空 call_id 的宣告必须被拒绝并指名 seq，不能当成「未答调用」悄悄跳过。
//
// 这条不在 brief 里，是自我复审补的：光有「只宣告有结果的调用」那条 continue，
// 一条 call_id 为空的宣告会走进同一条 continue——因为 results 里永远不会有空键
// （collectToolResults 已经拒绝空 call_id 的结果）。于是「写入方坏了」与「进程
// 硬崩留下未答调用」这两件性质完全不同的事被折叠成同一种无声后果：调用从
// transcript 里消失、没人知道。recordAssistantMessage 原样抄 domain.ToolCall.ID，
// 空值只可能来自坏掉的写入方，属 fail-loud 铁律里的「本不该发生」。
func TestAnAnnouncedCallWithAnEmptyCallIDIsRefused(t *testing.T) {
	_, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "干活",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":       "调工具",
			"tool_calls":    []any{map[string]any{"call_id": "", "name": "read_file"}},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
	})
	if err == nil {
		t.Fatal("空 call_id 的宣告没有被拒绝——它会被当成未答调用悄悄跳过")
	}
	if !strings.Contains(err.Error(), "call_id") || !strings.Contains(err.Error(), "seq 3") {
		t.Errorf("错误没有指明缺的是 call_id 以及是哪条事件：%v", err)
	}
}

// 空 call_id 的**结果**同样必须被拒绝并指名 seq：它配不上任何调用，
// 而无声丢掉它意味着那次调用会被当成「未答」而整个从 transcript 里消失。
func TestAToolResultWithAnEmptyCallIDIsRefused(t *testing.T) {
	_, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "", "preview": "结果", "is_error": false,
		}),
	})
	if err == nil {
		t.Fatal("空 call_id 的 tool/result 没有被拒绝")
	}
	if !strings.Contains(err.Error(), "call_id") || !strings.Contains(err.Error(), "seq 1") {
		t.Errorf("错误没有指明缺的是 call_id 以及是哪条事件：%v", err)
	}
}

// 载荷是坏 JSON 时必须报错并指名事件类型与 seq，不能当成空载荷接着投影。
func TestACorruptPayloadIsRefusedByTypeAndSeq(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  domain.SessionEventType
		seq  int64
	}{
		{"user/message", domain.SessionEventUserMessage, 1},
		{"assistant/message", domain.SessionEventAssistantMessage, 2},
		{"tool/result", domain.SessionEventToolResult, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := projectTranscript("s", []domain.SessionEvent{
				evRaw(tc.seq, tc.typ, []byte(`{"content":`)),
			})
			if err == nil {
				t.Fatalf("%s 的坏 JSON 载荷没有被拒绝", tc.name)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("错误没有指名事件类型 %s：%v", tc.name, err)
			}
		})
	}
}
