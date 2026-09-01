package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/storage"
)

// spec §9 的 Task 4 判据：**search_session 能搜到工具调用**。
//
// 这条测试验的是**接线**，不是存储层的索引逻辑（那是 internal/storage 的事）。
// 存储层的测试自己造事件，能证明「给定这样一批事件，搜得到」；证明不了的是发射端
// 真的写出了那样的事件、并且按真实的屏障节奏落盘。P3 之前已经栽过一次同类：一个
// 读侧改动的全部测试都用 fake，接上真仓储才发现字段从来没被写出来。
//
// 具体的接线风险有两个，都只有真跑一次才暴露：
//
//   - tool/call 的载荷字段名（name/arguments）与索引解出来的必须对得上；
//   - 屏障（spec §5）在进工具体前 flush，tool/call 因此**单独成批**落盘，与发起它的
//     assistant/message 不在同一次 Append 里。索引只在批内找归属的话，工具往返会
//     搜得到却没有可回访的地址。
func TestARealToolLoopRunMakesItsToolCallSearchable(t *testing.T) {
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

	// 前提检查：这次执行确实调了工具，否则下面搜不搜得到都说明不了问题。
	events, err := repo.ReadFrom(ctx, "sess-real", 0)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	toolCalls := 0
	for _, event := range events {
		if event.Type == domain.SessionEventToolCall {
			toolCalls++
		}
	}
	if toolCalls == 0 {
		t.Fatal("这次执行一次工具都没调，判据无从验起")
	}

	hits, err := repo.SearchMessages(ctx, "read_file", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	var toolHit *domain.ConversationTurn
	for i := range hits {
		if strings.Contains(hits[i].Content, "read_file") {
			toolHit = &hits[i]
			break
		}
	}
	if toolHit == nil {
		t.Fatalf("一次真的工具调用搜不到：hits = %+v（spec §9 的判据就是这一条）", hits)
	}
	if toolHit.TaskID != "task-real" || toolHit.AgentID != "a1" {
		t.Errorf("工具命中没带上发起它的任务身份：%+v", *toolHit)
	}
	if toolHit.ID != "task-real:assistant" || toolHit.Role != domain.ConversationRoleAssistant {
		t.Errorf("工具命中的地址 = {ID:%q Role:%q}，要 {task-real:assistant assistant}",
			toolHit.ID, toolHit.Role)
	}

	// discovery 给的 id 必须能直接回 scroll——这是它对模型唯一的用处。
	scrolled, err := repo.ScrollMessages(ctx, "sess-real", toolHit.ID, 5)
	if err != nil {
		t.Fatalf("ScrollMessages(discovery 给的 %q): %v：discovery 与 scroll 的 id 空间分叉了",
			toolHit.ID, err)
	}
	if len(scrolled) == 0 {
		t.Error("scroll 回去一条 turn 都没有")
	}
}
