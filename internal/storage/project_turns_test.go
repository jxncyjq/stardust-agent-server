package storage

import (
	"encoding/json"
	"reflect"
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

// evRaw 造一条载荷是任意原始字节的事件，供构造「损坏的 JSON」这类不能用
// map[string]any 表达的场景。不改 evWith 的签名，免得把其余测试的调用点都搅乱。
func evRaw(seq int64, typ domain.SessionEventType, data json.RawMessage) domain.SessionEvent {
	return domain.SessionEvent{
		Seq:  seq,
		Type: typ,
		Time: time.Date(2026, 9, 1, 12, 0, int(seq), 0, time.UTC),
		Data: data,
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
			"usage":           map[string]any{"prompt": 11, "completion": 22, "cached": 3, "total": 33},
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
//
// 本期投影不产出工具 turn（工具往返进模型上下文是 G3，属 P5），所以 tool/call、
// tool/result 在这一层被**统一忽略**，不参与任何配对。这条测试因此会通过——但
// 通过的理由是「投影对这两类事件的处理不看位置、不看顺序、完全一致地忽略」，不是
// 「已经验证了正确的 call_id 配对逻辑」。真正的按 call_id 配对要等 P5 打开 G3、
// 让 tool/call+tool/result 开始产出内容时才有代码可验——见 project_turns.go 里
// 那条 no-op 分支上的文档注释，以及本文件末尾 TestToolCallAndResultOrderNeverAffectsProjection
// 对这条判断的补充验证。
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
		evRaw(1, domain.SessionEventUserMessage, json.RawMessage(`{"turn":0`)), // 截断的 JSON
	}

	_, err := projectTurns("s", events)
	if err == nil {
		t.Fatal("损坏的载荷没有被拒绝")
	}
}

// 交接项（task-1-review.md 留下的承诺）：「同一事件源重复投影得到相同 ID」
// 至今没有测试——它要等投影存在才能验。ScrollMessages 靠 turns[i].ID == aroundID
// 定位锚点，ID 不稳定就是直接报错，所以这条必须补上。
//
// 同一批事件（同一个切片，不重新构造）投影两次，两次产出的 turn 必须逐条 ID
// 相同——包括顺序、条数、每条的 ID 字符串。
func TestProjectingTheSameEventsTwiceProducesIdenticalTurnIDs(t *testing.T) {
	events := []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-1:0:0:user/message", "task_id": "task-7", "content": "第一轮",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "sess-1:0:0:assistant/message",
			"task_id": "task-7", "agent_id": "agent-a", "content": "回复一",
			"tool_calls":    []any{},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(4, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "completed"}),
		evWith(5, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "completed"}),
		evWith(6, domain.SessionEventTurnStart, map[string]any{"turn": 1}),
		evWith(7, domain.SessionEventUserMessage, map[string]any{
			"turn": 1, "turn_id": "sess-1:1:0:user/message", "task_id": "task-7", "content": "第二轮",
		}),
		evWith(8, domain.SessionEventStepStart, map[string]any{"turn": 1, "step": 0}),
		evWith(9, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 1, "step": 0, "turn_id": "sess-1:1:0:assistant/message",
			"task_id": "task-7", "agent_id": "agent-a", "content": "回复二",
			"tool_calls":    []any{},
			"usage":         map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile": "fast",
		}),
		evWith(10, domain.SessionEventStepEnd, map[string]any{"turn": 1, "step": 0, "reason": "completed"}),
		evWith(11, domain.SessionEventTurnEnd, map[string]any{"turn": 1, "reason": "completed"}),
	}

	first, err := projectTurns("sess-1", events)
	if err != nil {
		t.Fatalf("projectTurns 第一次: %v", err)
	}
	second, err := projectTurns("sess-1", events)
	if err != nil {
		t.Fatalf("projectTurns 第二次: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("两次投影条数不同：%d vs %d", len(first), len(second))
	}
	if len(first) != 4 {
		t.Fatalf("投影出 %d 条 turn，要 4 条（两轮各一 user + 一 assistant）", len(first))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Errorf("turns[%d].ID 两次投影不一致：%q vs %q（ScrollMessages 靠它定位锚点）",
				i, first[i].ID, second[i].ID)
		}
	}
	// ID 本身也要彼此不同：四条 turn 落在四个不同的 (turn, step, type) 坐标上。
	seen := make(map[string]bool, len(first))
	for _, turn := range first {
		if seen[turn.ID] {
			t.Errorf("turn ID %q 在同一批投影里重复出现", turn.ID)
		}
		seen[turn.ID] = true
	}
}

// 对「按 call_id 配对」这条约束在本层是否还有可验证内容的补充验证：把
// TestToolResultsPairByCallIDNotByPosition 里 tool/call、tool/result 的相对顺序
// 整个打乱（先发两条 result 再发两条 call，且 call_id 顺序也反过来），投影结果必须
// 与原顺序完全一致（除了工具事件本身不出现在输出里之外，没有任何字段受影响）。
// 这证明本期忽略 tool/call、tool/result 时，代码里确实不存在任何隐式的位置假设——
// 不是因为没测到，而是因为分支逻辑本身就与顺序无关。P5 打开 G3、真正实现按 call_id
// 配对时，这条测试仍然成立，不需要跟着改。
func TestToolCallAndResultOrderNeverAffectsProjection(t *testing.T) {
	base := func(toolEvents []domain.SessionEvent) []domain.SessionEvent {
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
		}
		events = append(events, toolEvents...)
		events = append(events,
			evWith(8, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "interrupted"}),
			evWith(9, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "interrupted"}),
		)
		return events
	}

	// 顺序 A：两次 call 挨着，然后两次 result（倒序）——原测试的形状。
	orderA := base([]domain.SessionEvent{
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
		}),
		evWith(5, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "name": "read_file", "arguments": "{}",
		}),
		evWith(6, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "preview": "c2 的结果", "is_error": true,
		}),
		evWith(7, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "c1 的结果", "is_error": true,
		}),
	})

	// 顺序 B：完全打乱——result 先来、call_id 顺序也反过来。
	orderB := base([]domain.SessionEvent{
		evWith(4, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "c1 的结果", "is_error": true,
		}),
		evWith(5, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "name": "read_file", "arguments": "{}",
		}),
		evWith(6, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "preview": "c2 的结果", "is_error": true,
		}),
		evWith(7, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
		}),
	})

	turnsA, err := projectTurns("s", orderA)
	if err != nil {
		t.Fatalf("projectTurns(orderA): %v", err)
	}
	turnsB, err := projectTurns("s", orderB)
	if err != nil {
		t.Fatalf("projectTurns(orderB): %v", err)
	}

	if !reflect.DeepEqual(turnsA, turnsB) {
		t.Fatalf("tool/call、tool/result 的相对顺序影响了投影结果：\nA=%+v\nB=%+v", turnsA, turnsB)
	}
}
