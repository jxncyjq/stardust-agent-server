package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/storage"
)

// 这个文件里的两个夹具都是 conversation_turns 退役（spec §3 取舍 A2）过程中的产物，
// 服务的是同一件事的两侧：
//
//   - appendTurnEvents：**读侧**。ListConversationTurns 现在从会话事件投影，所以
//     「先造几条 turn 行、再从 /turns 读回来」的测试必须改从事件一侧播种。
//   - openServerTestRepoWithPath / conversationTurnRows：**写侧**。HTTP 层仍然往
//     conversation_turns 写行（recordUserTurn / 结果端点记 assistant turn），那些行
//     今天已经没有读者了；要断言「这一行确实写了」，只能直接查表。Task 5 删掉这两个
//     写入点时，用到 conversationTurnRows 的测试与这个夹具一起删。

// appendTurnEvents 把一串 domain.ConversationTurn 写成这条会话的事件日志。
//
// 它是**测试夹具，不是生产写入路径**：生产上这些事件由 internal/runtime 的
// eventRecorder 发出，这里只是把同样形状的载荷直接摆进日志，好让 /turns 的读路径有
// 东西可读。只能对一条尚无事件的会话调用一次：seq 从 0 开始，Append 要求它正好接上
// 该会话的 next-seq。
//
// 传进来的 turn.ID 落进事件载荷的 turn_id（那是**事件**的标识）。投影出来的
// domain.ConversationTurn.ID 不是它，而是按 (task_id, role) 折叠出的
// "<task_id>:<role>"——与退役中 server/http.go 的 recordUserTurn /
// recordAssistantTurn 写的形状逐字一致。断言 id 时要用后者。
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

// openServerTestRepoWithPath 与 openServerTestRepo 相同，另外返回数据库文件路径，
// 供 conversationTurnRows 直接查表。
func openServerTestRepoWithPath(t *testing.T) (*storage.SQLiteRepository, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.db")
	repo, err := storage.OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	return repo, path
}

// conversationTurnRows 直接读 conversation_turns 表里属于 sessionID 的行，按写入
// 顺序返回。
//
// 走一条独立的只读连接，而不是 ListConversationTurns：后者在 Task 3 之后读的是事件
// 日志，看不见这张表——拿它来断言「HTTP 层写了这一行」只会永远数出 0，那是在量错的
// 东西。
func conversationTurnRows(t *testing.T, dbPath string, sessionID string) []domain.ConversationTurn {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open %q: %v", dbPath, err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close %q: %v", dbPath, err)
		}
	}()
	rows, err := db.QueryContext(context.Background(), `
		SELECT id, task_id, agent_id, role, content, generated_files
		FROM conversation_turns
		WHERE session_id = ?
		ORDER BY created_at, id
	`, sessionID)
	if err != nil {
		t.Fatalf("query conversation_turns for %q: %v", sessionID, err)
	}
	defer rows.Close()
	var turns []domain.ConversationTurn
	for rows.Next() {
		var (
			turn  domain.ConversationTurn
			files string
		)
		if err := rows.Scan(&turn.ID, &turn.TaskID, &turn.AgentID, &turn.Role, &turn.Content, &files); err != nil {
			t.Fatalf("scan conversation_turns row for %q: %v", sessionID, err)
		}
		if files != "" {
			if err := json.Unmarshal([]byte(files), &turn.GeneratedFiles); err != nil {
				t.Fatalf("decode generated_files of %q: %v", turn.ID, err)
			}
		}
		turn.SessionID = sessionID
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate conversation_turns for %q: %v", sessionID, err)
	}
	return turns
}
