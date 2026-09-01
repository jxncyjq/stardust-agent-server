package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/storage"
)

// conversation_turns 已在 P3 Task 5 退役（spec §3 取舍 A2），HTTP 层不再写任何对话
// 内容，所以这个文件里只剩读侧的夹具：ListConversationTurns 从会话事件投影，
// 「先造几条 turn、再从 /turns 读回来」的测试必须从事件一侧播种。
//
// 原来还有一对写侧夹具（openServerTestRepoWithPath / conversationTurnRows）直接查
// conversation_turns 表，用来断言「HTTP 层确实写了这一行」。写入点没了，它们守的东西
// 也就不存在了，随 Task 5 一并删除。

// appendTurnEvents 把一串 domain.ConversationTurn 写成这条会话的事件日志。
//
// 它是**测试夹具，不是生产写入路径**：生产上这些事件由 internal/runtime 的
// eventRecorder 发出，这里只是把同样形状的载荷直接摆进日志，好让 /turns 的读路径有
// 东西可读。只能对一条尚无事件的会话调用一次：seq 从 0 开始，Append 要求它正好接上
// 该会话的 next-seq。
//
// 传进来的 turn.ID 落进事件载荷的 turn_id（那是**事件**的标识）。投影出来的
// domain.ConversationTurn.ID 不是它，而是按 (task_id, role) 折叠出的
// "<task_id>:<role>"——与 storage.SearchMessages 拼 discovery 命中 id 的形状逐字
// 一致。断言 id 时要用后者。
//
// 同一个 (TaskID, Role) 传两条会折成一行（正文取最后一条、用量累加、
// generated_files 取并集），那正是生产上多轮工具循环与「挂起→恢复」的形状。
func appendTurnEvents(t *testing.T, repo *storage.SQLiteRepository, sessionID string, turns ...domain.ConversationTurn) {
	t.Helper()
	events := make([]domain.SessionEvent, 0, len(turns))
	for i, turn := range turns {
		var (
			typ  domain.SessionEventType
			data map[string]any
		)
		switch turn.Role {
		case domain.ConversationRoleUser:
			typ = domain.SessionEventUserMessage
			data = map[string]any{
				"turn": 0, "turn_id": turn.ID, "task_id": turn.TaskID,
				"agent_id": turn.AgentID, "content": turn.Content,
			}
		case domain.ConversationRoleAssistant:
			typ = domain.SessionEventAssistantMessage
			data = map[string]any{
				"turn": 0, "step": 0, "turn_id": turn.ID, "task_id": turn.TaskID,
				"agent_id": turn.AgentID, "content": turn.Content,
				"model_profile": turn.ModelProfile,
				"usage": map[string]any{
					"prompt": turn.PromptTokens, "completion": turn.CompletionTokens,
					"cached": turn.CachedTokens, "total": turn.TotalTokens,
				},
				"generated_files": turn.GeneratedFiles,
			}
		default:
			t.Fatalf("appendTurnEvents: turn %q 的 role 是 %q，投影只认 user/assistant", turn.ID, turn.Role)
		}
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatalf("marshal %s payload for %q: %v", typ, turn.ID, err)
		}
		events = append(events, domain.SessionEvent{
			Seq:  int64(i),
			Type: typ,
			Time: turn.CreatedAt,
			Data: raw,
		})
	}
	if err := repo.Append(context.Background(), sessionID, events); err != nil {
		t.Fatalf("append turn events for %q: %v", sessionID, err)
	}
}
