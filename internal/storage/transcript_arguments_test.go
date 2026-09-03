package storage

import (
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// 这一组守 P5 Task 3 复审的 🔴 C2：G3 打开时历史 tool_calls 的 arguments 在线上是
// 字符串 "null"，而 OpenAI 兼容契约要求它是一个 **JSON 对象**的字符串。
//
// 病根在这一层：assistant/message 的 tool_calls 载荷里只有 call_id 与 name，参数
// 落在 tool/call 事件里，而投影原来把 tool/call 整个跳过了（那条注释说「tool/call
// 的信息已经在 assistant 的 tool_calls 里」——arguments 恰恰不在）。
//
// 线上字节那一半由 internal/runtime 的 TestHistoryToolCallsCarryTheirArgumentsOnTheWire
// 守：那条测试走真的 adapter，把请求体拆开看 function.arguments。这里守的是投影
// 本身产出的 domain.ToolCall.Arguments。

// callArgumentsRound 造「问 → 调一次 read_file → 答」的最小日志，arguments 由调用方
// 给：本组每条测试的差别都只在那一段上。
func callArgumentsRound(arguments any) []domain.SessionEvent {
	return []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "content": "读 notes.md",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":    "我读一下",
			"tool_calls": []any{map[string]any{"call_id": "c1", "name": "read_file"}},
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file",
			"arguments": arguments,
		}),
		evWith(5, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "notes 正文", "is_error": false,
		}),
	}
}

// onlyAnnouncedCall 取出投影里唯一那条带 tool_calls 的 assistant 的唯一一次调用。
func onlyAnnouncedCall(t *testing.T, events []domain.SessionEvent) domain.ToolCall {
	t.Helper()
	msgs, err := projectTranscript("s", events)
	if err != nil {
		t.Fatalf("projectTranscript: %v", err)
	}
	var calls []domain.ToolCall
	for _, m := range msgs {
		calls = append(calls, m.ToolCalls...)
	}
	if len(calls) != 1 {
		t.Fatalf("投影出 %d 次调用，要 1 次：%+v", len(calls), msgs)
	}
	return calls[0]
}

// 参数原样到达模型：历史里的调用必须带着它当时真的传了什么。
func TestAnAnnouncedCallCarriesTheArgumentsItsToolCallEventRecorded(t *testing.T) {
	call := onlyAnnouncedCall(t, callArgumentsRound(`{"path":"notes.md","limit":"20"}`))

	if got := call.Arguments["path"]; got != "notes.md" {
		t.Errorf("arguments[path] = %q, want %q：历史调用没带上它当时传的参数", got, "notes.md")
	}
	if got := call.Arguments["limit"]; got != "20" {
		t.Errorf("arguments[limit] = %q, want %q", got, "20")
	}
}

// 没有参数的调用得到空对象，不是 nil。
//
// nil 才是 C2 的病根：adapter 把 nil map 编成 `null`，而 null 不是对象。空对象是
// 同一件事在线上的合法形状，也正是 adapter 入站方向对「没有参数」的处理
// （openAIToolCalls 造的是 map[string]string{}）。
func TestACallWithNoArgumentsGetsAnEmptyMapNotNil(t *testing.T) {
	call := onlyAnnouncedCall(t, callArgumentsRound(""))

	if call.Arguments == nil {
		t.Fatal("Arguments 是 nil：adapter 会把它编成 `null` 送上线，而 provider 要的是一个 JSON 对象")
	}
	if len(call.Arguments) != 0 {
		t.Errorf("Arguments = %+v，要空表：日志里这次调用没有参数", call.Arguments)
	}
}

// 日志里写着 JSON 字面量 null 的，与「没有参数」同解：仍然是空表，不是 nil。
//
// 这条不是假设出来的形状：recordToolCall 直接 json.Marshal(call.Arguments)，而一条
// Arguments 为 nil 的 domain.ToolCall 落盘就是 "null"。
func TestArgumentsRecordedAsJSONNullStillGetAnEmptyMap(t *testing.T) {
	call := onlyAnnouncedCall(t, callArgumentsRound("null"))

	if call.Arguments == nil {
		t.Fatal("Arguments 是 nil：`null` 会原样回到线上，正是这条 Critical 的形状")
	}
	if len(call.Arguments) != 0 {
		t.Errorf("Arguments = %+v，要空表", call.Arguments)
	}
}

// 值不是字符串的参数按 adapter 入站方向的同一规则铺平，两边保持同一个损失面。
func TestNonStringArgumentValuesAreFlattenedTheSameWayTheAdapterFlattensThem(t *testing.T) {
	call := onlyAnnouncedCall(t, callArgumentsRound(`{"limit":20,"deep":true}`))

	if got := call.Arguments["limit"]; got != "20" {
		t.Errorf("arguments[limit] = %q, want %q", got, "20")
	}
	if got := call.Arguments["deep"]; got != "true" {
		t.Errorf("arguments[deep] = %q, want %q", got, "true")
	}
}

// 被 maxEventPreviewRunes 截断过的参数解不出来，但那是**契约声明过的**合法状态
// （recordToolCall 明写按预览上限截 arguments，write_file 带一整个文件正文的调用
// 天天发生）。它必须仍然产出一个合法对象，且内容是日志里真实存着的字节——不报错
// 让整条会话发不出去，也不替模型编一份它没传过的参数。
func TestTruncatedArgumentsStayReadableInsteadOfBreakingTheWholeTranscript(t *testing.T) {
	truncated := `{"path":"notes.md","content":"aaaa` + "\n…[truncated: 34 of 9000 runes shown]"

	call := onlyAnnouncedCall(t, callArgumentsRound(truncated))

	if call.Arguments == nil {
		t.Fatal("截断过的参数让投影产出了 nil：那会在线上编成 `null`")
	}
	got, ok := call.Arguments[truncatedArgumentsKey]
	if !ok {
		t.Fatalf("截断过的参数没挂在 %q 下：%+v", truncatedArgumentsKey, call.Arguments)
	}
	if got != truncated {
		t.Errorf("挂上去的不是日志里那段原文：\n got=%q\nwant=%q", got, truncated)
	}
}

// 既不是合法 JSON、也没有截断记号的载荷只可能来自坏掉的写入方：报错并指名，
// 不静默产出一个空对象——那会把「写入方坏了」与「这次调用没有参数」折叠成同一件事。
func TestArgumentsThatAreNeitherValidJSONNorTruncatedAreRefused(t *testing.T) {
	_, err := projectTranscript("s", callArgumentsRound(`{"path":`))
	if err == nil {
		t.Fatal("坏掉的 arguments 载荷被放行了")
	}
	if !strings.Contains(err.Error(), "c1") || !strings.Contains(err.Error(), "seq 3") {
		t.Errorf("错误没有指名 call_id 与宣告它的那条事件：%v", err)
	}
}

// 有结果却没有 tool/call 事件的调用是坏日志：屏障 2 先记 tool/call 再落盘，落盘失败
// 时那次调用根本不派发（dropBufferedToolCall），所以「有结果」蕴含「有 tool/call」。
// 缺了它而静默给个空对象，模型会读到一次「什么参数都没传」的调用——错得无声无息。
//
// 注意它与「未答的调用不宣告」不是一回事：那条是崩溃后的合法残留，在这条检查之前
// 就被跳过了。
func TestAnAnsweredCallWithNoToolCallEventIsRefused(t *testing.T) {
	_, err := projectTranscript("s", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":    "我读一下",
			"tool_calls": []any{map[string]any{"call_id": "c1", "name": "read_file"}},
		}),
		evWith(2, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "结果", "is_error": false,
		}),
	})
	if err == nil {
		t.Fatal("一次没有 tool/call 事件的调用被宣告了：它的参数只能是空的，而那是编的")
	}
	if !strings.Contains(err.Error(), "c1") {
		t.Errorf("错误没有指名 call_id：%v", err)
	}
}

// 同一次调用被记了两条 tool/call 是坏日志：撞了却挑一个用，模型会读到一份张冠李戴
// 的参数。与 collectToolResults 撞键报错是同一条理由。
func TestTwoToolCallEventsForTheSameCallAreRefused(t *testing.T) {
	events := callArgumentsRound(`{"path":"notes.md"}`)
	events = append(events, evWith(6, domain.SessionEventToolCall, map[string]any{
		"turn": 0, "step": 0, "call_id": "c1", "name": "read_file",
		"arguments": `{"path":"别的文件.md"}`,
	}))

	_, err := projectTranscript("s", events)
	if err == nil {
		t.Fatal("同一次调用的两条 tool/call 被放行了")
	}
	if !strings.Contains(err.Error(), "c1") || !strings.Contains(err.Error(), "seq 6") {
		t.Errorf("错误没有指名 call_id 与那条重复事件：%v", err)
	}
}

// 配不上的 tool/call（空 call_id / 缺 turn-step）与 tool/result 侧同样必须报错：
// 它的参数会附到别的调用上，或者干脆让那次调用被判成「没有参数」。
func TestAToolCallEventThatCannotBePairedIsRefused(t *testing.T) {
	t.Run("空 call_id", func(t *testing.T) {
		_, err := projectTranscript("s", []domain.SessionEvent{
			evWith(1, domain.SessionEventToolCall, map[string]any{
				"turn": 0, "step": 0, "call_id": "", "name": "read_file", "arguments": "{}",
			}),
		})
		if err == nil || !strings.Contains(err.Error(), "seq 1") {
			t.Fatalf("err = %v，要一条指名 seq 1 的错误", err)
		}
	})
	t.Run("缺 step", func(t *testing.T) {
		_, err := projectTranscript("s", []domain.SessionEvent{
			evWith(1, domain.SessionEventToolCall, map[string]any{
				"turn": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
			}),
		})
		if err == nil || !strings.Contains(err.Error(), "c1") {
			t.Fatalf("err = %v，要一条指名 call_id 的错误", err)
		}
	})
}
