package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/storage"
)

// RecentTurnsForTask 是「多轮 messages 喂模型」这条读侧调用点。它自己的既有测试都用
// fakeTurnLister，所以从来没跑过真的存储实现——而 P3 把 ListConversationTurns 的实现
// 从查 conversation_turns 表换成了读事件投影。这条测试把真仓储接进来，验的是**接线**：
//
//   - TaskID 一路活到过滤条件：任务自己的那条 user turn 必须被滤掉，否则模型会在
//     task.Input 之外再看见一遍同样的问题（P3 计划里点名的五个字段缺口之一）；
//   - Content 是全文：事件载荷里的正文不截断（Task 1 的决定），模型侧
//     MaxTurnChars = 6000 才拿得回 2000 rune 以上的历史。
//
// 断言的是这两条穿过「事件 → 投影 → RecentTurnsForTask」全程都没丢，不是投影函数本身
// 对不对（那是 project_turns_test.go 的事）。
func TestRecentTurnsForTaskReadsTheProjectedEventLog(t *testing.T) {
	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	// 3000 rune：超过事件预览上限 maxEventPreviewRunes(2000)，不到模型侧上限
	// defaultMaxTurnChars(6000)。正文若在写侧被按预览截断，这里会看见 2000。
	longAnswer := strings.Repeat("答", 3000)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	events := []domain.SessionEvent{
		projectionEvent(t, 0, domain.SessionEventUserMessage, at, map[string]any{
			"turn": 0, "turn_id": "sess-run:0", "task_id": "task-old",
			"agent_id": "agent-a", "content": "上一轮的问题",
		}),
		projectionEvent(t, 1, domain.SessionEventAssistantMessage, at.Add(time.Second), map[string]any{
			"turn": 0, "step": 0, "turn_id": "sess-run:1", "task_id": "task-old",
			"agent_id": "agent-a", "content": longAnswer,
		}),
		projectionEvent(t, 2, domain.SessionEventUserMessage, at.Add(2*time.Second), map[string]any{
			"turn": 1, "turn_id": "sess-run:2", "task_id": "task-now",
			"agent_id": "agent-a", "content": "这一轮的问题",
		}),
	}
	if err := repo.Append(ctx, "sess-run", events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	turns, err := RecentTurnsForTask(ctx, repo,
		config.SessionConfig{Enabled: true, DefaultRecentTurns: 6, MaxTurnChars: 6000},
		domain.Task{ID: "task-now", SessionID: "sess-run"})
	if err != nil {
		t.Fatalf("RecentTurnsForTask: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("拿到 %d 条 turn，要 2 条（任务自己的 user turn 该被滤掉）：%#v", len(turns), turns)
	}
	for _, turn := range turns {
		if turn.TaskID != "task-old" {
			t.Errorf("turn %q 的 TaskID = %q，任务自己的那条没滤掉，模型会看见重复的 user 消息",
				turn.ID, turn.TaskID)
		}
	}
	if got := len([]rune(turns[1].Content)); got != 3000 {
		t.Errorf("assistant turn 正文 %d runes，要 3000：历史被截断了，模型侧的 6000 上限拿不回来", got)
	}
}

// 一次**真的** RunTask（带一轮工具）写进真仓储之后，读回来必须正好是两条 turn，
// 且字段齐全。上面那条测试自己摆事件载荷，摆得对不对由测试作者决定；这条测试让
// runtime 自己去写，验的是**发射点与投影真的对得上**。
//
// 它一次守三件事，每一件都在 P3 Task 3 复审里被实测证明过是活的缺口：
//
//   - C-1：generateStep 每轮模型请求各记一条 assistant/message，一次「一轮
//     read_file 之后回答」的任务写出 2 条。逐事件投影会得到 3 条 turn（其中一条
//     正文为空）；折叠之后必须是 2 条。
//   - I-1：user turn 的 AgentID。recordUserMessage 不写 agent_id 的话，整个
//     internal/runtime 包在这条测试之前是**全绿**的——写侧没人守。
//   - C-2：投影出来的 ID 必须是 "<task_id>:<role>"，与 serve 写进
//     conversation_turns_fts 的形状逐字相同，否则 search_session 的
//     discovery→scroll 会 anchor not found。
func TestARealToolLoopRunProjectsToOneTurnPerRole(t *testing.T) {
	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	rt := newTestRuntimeWithEvents(t, repo)
	if _, err := rt.RunTask(ctx, domain.Agent{ID: "a1"}, domain.Task{
		ID: "task-real", SessionID: "sess-real", AgentID: "a1", Input: "读 notes.md",
	}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	// 前提检查：这次执行确实走了多轮（否则下面的「折叠成一条」是白验的）。
	events, err := repo.ReadFrom(ctx, "sess-real", 0)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	assistantEvents := 0
	for _, event := range events {
		if event.Type == domain.SessionEventAssistantMessage {
			assistantEvents++
		}
	}
	if assistantEvents < 2 {
		t.Fatalf("这次执行只写了 %d 条 assistant/message，夹具没跑出多轮工具循环，"+
			"折叠这件事就没被验到", assistantEvents)
	}

	turns, err := repo.ListConversationTurns(ctx, "sess-real", 0)
	if err != nil {
		t.Fatalf("ListConversationTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("%d 条 assistant/message 事件投影出 %d 条 turn，要 2 条（一条 user + 一条 assistant）：%#v",
			assistantEvents, len(turns), turns)
	}
	if turns[0].ID != "task-real:user" || turns[0].Role != domain.ConversationRoleUser {
		t.Errorf("turns[0] = {ID:%q Role:%q}，要 {task-real:user user}：id 与 serve 写进 FTS 的形状必须相同",
			turns[0].ID, turns[0].Role)
	}
	if turns[0].AgentID != "a1" {
		t.Errorf("user turn 的 AgentID = %q，要 a1：recordUserMessage 没写 agent_id，"+
			"/turns 的 user 项就恒为空（P3 计划列出的字段缺口之一）", turns[0].AgentID)
	}
	if turns[1].ID != "task-real:assistant" || turns[1].Role != domain.ConversationRoleAssistant {
		t.Errorf("turns[1] = {ID:%q Role:%q}，要 {task-real:assistant assistant}", turns[1].ID, turns[1].Role)
	}
	if turns[1].Content != "读完了：那个文件讲的是缓存" {
		t.Errorf("assistant 正文 = %q，要最终答案：取错轮次会把请求工具那轮的空正文当答案", turns[1].Content)
	}
	if turns[1].AgentID != "a1" {
		t.Errorf("assistant turn 的 AgentID = %q，要 a1", turns[1].AgentID)
	}
}

func projectionEvent(t *testing.T, seq int64, typ domain.SessionEventType, at time.Time, payload map[string]any) domain.SessionEvent {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload at seq %d: %v", typ, seq, err)
	}
	return domain.SessionEvent{Seq: seq, Type: typ, Time: at, Data: data}
}
