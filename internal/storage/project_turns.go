package storage

import (
	"encoding/json"
	"fmt"

	"github.com/stardust/legion-agent/internal/domain"
)

// projectTurns 把一条会话的事件流投影成对话轮次。
//
// 这是 conversation_turns 退役之后**唯一**的 turn 来源（spec §3 取舍 A2）。它是
// 纯函数：不碰数据库、不读文件、不看时钟，于是同一批事件永远投影出同一批 turn——
// TestProjectingTheSameEventsTwiceProducesIdenticalTurnIDs 验的就是这条：turn_id
// 本身在事件载荷里写死（见 internal/runtime/eventlog.go 的 newTurnID），这里只是
// 原样透传，不重新生成，所以稳定性来自「不生成」而不是某种确定性算法。
//
// events 必须按 seq 升序传入——这正是 ReadFrom 的返回契约（session_events.go：
// `ORDER BY seq`）。本函数不重新排序：P3 的读路径一律用 ReadFrom、绝不调用 Load
// （spec §4.3.1 第 3 条），而 ReadFrom 已经保证了这个顺序；重新排序既是不必要的
// 工作，也会掩盖「调用方传错顺序」这类编程错误。
//
// 按 call_id 配对，不按位置（spec §4.3.1 第 2 条）：崩溃恢复补出的 tool/result
// 排在日志**尾部**，可能排在自己那次调用的 step/end 之后；按位置配会把它配到错误
// 的 assistant 上，产出非法 transcript。
//
// 这条约束在本函数里没有「配对代码」可看：P3 投影只产出 user/assistant 两种 turn
// （domain.ConversationRole 也只定义了这两个值），工具往返进入模型上下文是 G3，
// 属 P5，本期不做。tool/call 与 tool/result 在下面的 switch 里落在同一个 no-op
// 分支，无论它们出现在事件流的哪个位置、以什么顺序出现、call_id 互相之间是什么
// 关系，这条分支都不读取它们的任何字段——因此谈不上「按位置配对」，因为根本没有
// 配对发生。TestToolResultsPairByCallIDNotByPosition 与
// TestToolCallAndResultOrderNeverAffectsProjection 验证的正是这一点：尾部乱序的
// tool/result 不会腐蚀相邻的 assistant turn、也不会让投影出错。
//
// P5 打开 G3、真正要把工具调用及其结果投影成内容时，必须在这里新增一个按 call_id
// 建索引（例如 map[callID]toolResultPayload）、再消费 tool_calls 摘要去查表的分支，
// **不能**假设 tool/result 出现在对应 tool/call 之后的固定相对位置——上面这段
// 注释和这两条测试就是留给那次改动的证据：位置无关性从一开始就是不变量的一部分，
// 不是这次实现漏掉的东西。
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
			// 把工具往返也放进模型上下文是 G3，属 P5，本期不做——见本函数文档
			// 注释里对「按 call_id 配对」这条约束在本层为什么没有代码可写的说明。
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
