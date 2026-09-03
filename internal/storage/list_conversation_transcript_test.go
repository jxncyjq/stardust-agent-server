package storage

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// twoCallRoundEvents 是一条会话的日志：问一句、一轮宣告两次调用的 assistant、
// 两条结果、收尾的回答。投影出来是 5 条消息：
//
//	[0] user  [1] assistant(+c1,+c2)  [2] tool c1  [3] tool c2  [4] assistant
//
// 挑「一轮两次调用」是因为它让「从尾部截断会切散 assistant 与它的 tool」这件事
// 有多种切法：limit=2 切在两条 tool 中间，limit=3 切在 assistant 与它的第一条
// tool 中间。
func twoCallRoundEvents() []domain.SessionEvent {
	return []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "t:user", "task_id": "t", "agent_id": "a",
			"content": "并行读两个文件",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content":       "同时读",
			"model_profile": "fast",
			"tool_calls": []any{
				map[string]any{"call_id": "c1", "name": "read_file"},
				map[string]any{"call_id": "c2", "name": "read_file"},
			},
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": "{}",
		}),
		evWith(5, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "name": "read_file", "arguments": "{}",
		}),
		evWith(6, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "c1 的内容", "is_error": false,
		}),
		evWith(7, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c2", "preview": "c2 的内容", "is_error": false,
		}),
		evWith(8, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "completed"}),
		evWith(9, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 1}),
		evWith(10, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 1, "turn_id": "t:assistant", "task_id": "t", "agent_id": "a",
			"content": "两个都读完了", "model_profile": "fast",
		}),
		evWith(11, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 1, "reason": "completed"}),
		evWith(12, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "completed"}),
	}
}

// ListConversationTranscript 读的是事件、投影出的是 provider transcript。
func TestListConversationTranscriptProjectsFromTheEventLog(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()
	if err := repo.Append(ctx, "sess-tx", twoCallRoundEvents()); err != nil {
		t.Fatalf("append events: %v", err)
	}

	msgs, err := repo.ListConversationTranscript(ctx, "sess-tx", 0)
	if err != nil {
		t.Fatalf("ListConversationTranscript: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("投影出 %d 条消息，要 5 条（user / assistant+2 calls / tool / tool / assistant）：%+v",
			len(msgs), msgs)
	}
	if msgs[0].Role != port.RoleUser || msgs[4].Role != port.RoleAssistant {
		t.Errorf("首尾角色不对：%q ... %q", msgs[0].Role, msgs[4].Role)
	}
	assertPairedTranscript(t, msgs)
}

// limit 从尾部截断时，绝不能把一批 tool 消息与宣告它们的 assistant 切散：
// provider 拒收配不上的 tool 消息，整个请求 400（port.InferenceMessage 的注释）。
//
// 逐个 limit 都验，而不是只验一个「刚好切在中间」的值：切点落在哪一类边界上是
// 由 limit 与消息条数共同决定的，只挑一个值很容易正好挑到不触发这条逻辑的那个。
func TestTheLimitNeverSplitsAnAssistantFromItsToolMessages(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()
	if err := repo.Append(ctx, "sess-tx", twoCallRoundEvents()); err != nil {
		t.Fatalf("append events: %v", err)
	}

	for limit := 1; limit <= 6; limit++ {
		msgs, err := repo.ListConversationTranscript(ctx, "sess-tx", limit)
		if err != nil {
			t.Fatalf("ListConversationTranscript(limit=%d): %v", limit, err)
		}
		if len(msgs) == 0 {
			t.Fatalf("limit=%d 返回了空 transcript", limit)
		}
		if msgs[0].Role == port.RoleTool {
			t.Errorf("limit=%d 让 transcript 以一条无人宣告的 tool 消息开头（tool_call_id=%q）："+
				"provider 会拒收整个请求", limit, msgs[0].ToolCallID)
		}
		assertPairedTranscript(t, msgs)
		// 向前走过的步数由那一步的调用条数封顶，不是「一路退到会话开头」。
		if len(msgs) > limit+2 {
			t.Errorf("limit=%d 却返回了 %d 条消息：窗口向前多留的量必须有界", limit, len(msgs))
		}
	}
}

// limit 恰好切在 assistant 与它的第一条 tool 之间（这里是 limit=3）时，窗口必须
// 向前退到那条 assistant 上，而不是把两条 tool 丢掉——丢掉会让那次调用的结果
// 无声消失，模型看得见调用、看不见结果。
func TestTheLimitWalksBackToTheAnnouncingAssistant(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()
	if err := repo.Append(ctx, "sess-tx", twoCallRoundEvents()); err != nil {
		t.Fatalf("append events: %v", err)
	}

	msgs, err := repo.ListConversationTranscript(ctx, "sess-tx", 3)
	if err != nil {
		t.Fatalf("ListConversationTranscript: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("limit=3 拿到 %d 条，要 4 条（退到 assistant + 它的两条 tool + 收尾的 assistant）：%+v",
			len(msgs), msgs)
	}
	if msgs[0].Role != port.RoleAssistant || len(msgs[0].ToolCalls) != 2 {
		t.Fatalf("msgs[0] = %+v，要那条宣告了两次调用的 assistant", msgs[0])
	}
}

// assertPairedTranscript 断言每条 tool 消息前面都有宣告它的 assistant，并且整条
// 消息序列过得了 provider 请求的形状校验。
func assertPairedTranscript(t *testing.T, msgs []port.InferenceMessage) {
	t.Helper()
	announced := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case port.RoleAssistant:
			for _, c := range m.ToolCalls {
				announced[c.ID] = true
			}
		case port.RoleTool:
			if !announced[m.ToolCallID] {
				t.Errorf("msgs[%d] 的 tool_call_id=%q 之前没有 assistant 宣告过", i, m.ToolCallID)
			}
		}
	}
	if err := (port.InferenceRequest{RequestID: "r", Messages: msgs}).Validate(); err != nil {
		t.Errorf("这份 transcript 发不出去：%v", err)
	}
}
