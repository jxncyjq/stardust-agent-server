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
			"turn": 0, "turn_id": "sess-run:0", "task_id": "task-old", "content": "上一轮的问题",
		}),
		projectionEvent(t, 1, domain.SessionEventAssistantMessage, at.Add(time.Second), map[string]any{
			"turn": 0, "step": 0, "turn_id": "sess-run:1", "task_id": "task-old",
			"agent_id": "agent-a", "content": longAnswer,
		}),
		projectionEvent(t, 2, domain.SessionEventUserMessage, at.Add(2*time.Second), map[string]any{
			"turn": 1, "turn_id": "sess-run:2", "task_id": "task-now", "content": "这一轮的问题",
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

func projectionEvent(t *testing.T, seq int64, typ domain.SessionEventType, at time.Time, payload map[string]any) domain.SessionEvent {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload at seq %d: %v", typ, seq, err)
	}
	return domain.SessionEvent{Seq: seq, Type: typ, Time: at, Data: data}
}
