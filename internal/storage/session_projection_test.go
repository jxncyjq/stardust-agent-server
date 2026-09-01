package storage

import (
	"context"
	"fmt"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// appendTurnEvents 把一串 domain.ConversationTurn 写成这条会话的事件日志。
//
// Task 3 之后 ListConversationTurns 读的是事件，不再读 conversation_turns 表，
// 于是「先造几条 turn 行、再读回来」的既有测试全都拿不到东西了。它们断言的是**行为**
// （这条会话里有哪些轮次），只是原来通过退役中的写入方播种；这个夹具把播种换到事件
// 一侧，断言本身一字不改。
//
// 它是**测试夹具，不是生产写入路径**：生产上这些事件由 internal/runtime 的
// eventRecorder 发出（P2 的发射点 + Task 1 补齐的字段），这里只是把同样形状的载荷
// 直接摆进日志，好让存储层的读路径有东西可读。
//
// 只能对一条尚无事件的会话调用一次：seq 从 0 开始，Append 要求它正好接上该会话的
// next-seq。
func appendTurnEvents(t *testing.T, repo *SQLiteRepository, sessionID string, turns ...domain.ConversationTurn) {
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
				"turn": 0, "turn_id": turn.ID, "task_id": turn.TaskID, "content": turn.Content,
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
		event := evWith(int64(i), typ, data)
		event.Time = turn.CreatedAt
		events = append(events, event)
	}
	if err := repo.Append(context.Background(), sessionID, events); err != nil {
		t.Fatalf("append turn events for %q: %v", sessionID, err)
	}
}

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
			"tool_calls":      []any{},
			"usage":           map[string]any{"prompt": 11, "completion": 22, "cached": 3, "total": 33},
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

// ReadFrom 的窗口起点必须是 0，不能是「跳过第一条」。
//
// 单靠上面那条测试守不住这个不变量：它的 seq 0 是 turn/start，而 turn/start 本来就
// 不投影出 turn，于是把 fromSeq 从 0 改成 1 时 turn 数一条不少，测试照样绿。真正会
// 丢东西的是**首条事件就是消息**的会话——`Append` 并不要求日志以 turn/start 开头
// （校验只管类型合法、JSON 合法、seq 连续），所以这是真实可达的形状，不是人造场景。
func TestListConversationTurnsReadsTheLogFromItsVeryFirstEvent(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if err := repo.Append(ctx, "sess-head", []domain.SessionEvent{
		evWith(0, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-head:0", "task_id": "task-1", "content": "第一句话",
		}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-head:1", "task_id": "task-1", "content": "第二句话",
		}),
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}

	turns, err := repo.ListConversationTurns(ctx, "sess-head", 0)
	if err != nil {
		t.Fatalf("ListConversationTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("拿到 %d 条 turn，要 2 条：读事件的起点不是 0，会话的第一条消息被吞了", len(turns))
	}
	if turns[0].ID != "sess-head:0" || turns[0].Content != "第一句话" {
		t.Errorf("turns[0] = {ID:%q Content:%q}，要 seq 0 那条", turns[0].ID, turns[0].Content)
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

// 投影失败必须一路传到调用方，不得被吞成「这条会话没有 turn」。
//
// 吞掉的后果不是「少一条」而是「整条会话历史凭空消失」：模型侧的
// RecentTurnsForTask 会当作没有历史接着跑，/turns 会返回空数组，GUI 的历史面板
// 会显示成一条空会话——全都不报错。所以这里断言的是 error 确实非 nil 且
// turns 为空，而不是「没崩就行」。
//
// 缺 task_id 的 user/message 是 projectTurns 显式拒绝的形状（Task 2 的非空校验），
// 且它写得进日志：Append 只验类型/JSON/seq，不看业务字段。
func TestAProjectionFailureIsReportedNotSwallowed(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if err := repo.Append(ctx, "sess-bad", []domain.SessionEvent{
		evWith(0, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-bad:0", "content": "没有 task_id",
		}),
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}

	turns, err := repo.ListConversationTurns(ctx, "sess-bad", 0)
	if err == nil {
		t.Fatalf("投影失败被吞了：拿到 %d 条 turn 和 nil error，"+
			"调用方会把「历史整条消失」当成「这条会话没说过话」", len(turns))
	}
	if len(turns) != 0 {
		t.Errorf("报错的同时还返回了 %d 条 turn", len(turns))
	}
}
