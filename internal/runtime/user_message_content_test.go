package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/storage"
)

// 带图片的任务，历史里必须看得出「附了图」，但绝不能把 base64 存进日志。
//
// 这条断言原来在 internal/server（TestHTTPServerTaskWithImagesAnnotatesUserTurn），
// 量的是 recordUserTurn 写进 conversation_turns 的那一行。P3 Task 5 把那张表连同
// 写入方一起退役之后，唯一的 user/message 生产者是 runtime，所以断言也搬到这里，
// 并且改成从**投影**读回来——那正是 GUI 的 /turns 与模型的 recent-turns 窗口看到
// 的东西。
//
// 夹具用真 RunTask + 真 SQLite，不手工造事件：手工造事件只能证明投影会读，证明不了
// runtime 会写，而丢的正是「runtime 写的时候把标记漏了」这一种。
func TestAUserMessageEventAnnotatesAttachedImagesWithoutEmbeddingThem(t *testing.T) {
	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	rt := newTestRuntimeWithEvents(t, repo)
	if _, err := rt.RunTask(ctx, domain.Agent{ID: "legion-agent"}, domain.Task{
		ID: "task-img", SessionID: "sess-img", AgentID: "agent-a", Input: "描述这张图",
		Images: []string{"data:image/png;base64,iVBORw0KGgo=", "data:image/jpeg;base64,/9j/4AAQ=="},
	}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	turns, err := repo.ListConversationTurns(ctx, "sess-img", 0)
	if err != nil {
		t.Fatalf("ListConversationTurns: %v", err)
	}
	if len(turns) == 0 || turns[0].Role != domain.ConversationRoleUser {
		t.Fatalf("投影出 %#v，要第一条是 user turn", turns)
	}
	if want := "描述这张图\n[附图 2 张]"; turns[0].Content != want {
		t.Fatalf("user turn 正文 = %q，要 %q：附件标记丢了，历史里就看不出这条消息带过图",
			turns[0].Content, want)
	}
	for _, turn := range turns {
		if strings.Contains(turn.Content, "base64") {
			t.Fatalf("turn %q 的正文里嵌了 base64 图片数据：%q", turn.ID, turn.Content)
		}
	}
}

// userMessageContent 的两条边界：没有图片时原样返回；正文为空时只留标记（否则历史
// 里会出现一个开头是空行的气泡）。
func TestUserMessageContentEdges(t *testing.T) {
	if got := userMessageContent("你好", 0); got != "你好" {
		t.Errorf("userMessageContent(no images) = %q, want unchanged 你好", got)
	}
	if got := userMessageContent("", 3); got != "[附图 3 张]" {
		t.Errorf("userMessageContent(empty input, 3 images) = %q, want bare marker", got)
	}
}
