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
//
// 传进来的 turn.ID 落进事件载荷的 turn_id（那是**事件**的标识）。投影出来的
// domain.ConversationTurn.ID 不是它，而是按 (task_id, role) 折叠出的
// "<task_id>:<role>"——与退役中 server/http.go 的 recordUserTurn /
// recordAssistantTurn 写的形状逐字一致。断言 id 时要用后者。
//
// 同一个 (TaskID, Role) 传两条会折成一行（正文取最后一条、用量累加、
// generated_files 取并集），那正是生产上多轮工具循环与「挂起→恢复」的形状。
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
			"turn": 0, "turn_id": "sess-1:1", "task_id": "task-7",
			"agent_id": "agent-a", "content": "读 notes.md",
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

// 一个带工具循环的任务只投影出**一条** assistant turn，正文是最终答案。
//
// 这条测试播的是真实的多轮形状，不是「一条 user + 一条 assistant」那种简化夹具：
// runtime 的 generateStep 每一轮模型请求各记一条 assistant/message（runtime.go
// 首轮一条 + runToolLoop 循环内每轮一条），请求工具的那几轮 resp.Text 是空的。
// 逐事件产出 turn 的话，一次「一轮 read_file 之后回答」的任务会投影出 3 条 turn，
// 其中一条正文为空——GUI 历史面板每轮工具多一个空气泡（MaxToolRounds 可到 30）、
// 模型侧 recent-N 窗口被空 turn 塞满、文件卡片重复渲染、token 口径从「每任务一条」
// 变成「每轮一条」。退役中的 conversation_turns 是每任务恰好一条，投影必须复现它。
//
// 断言四件事，逐条对应折叠规则：条数与 ID（"<task_id>:<role>"，与退役中
// server/http.go 写的形状逐字一致）、正文取最后一条（最终答案，不是中间轮的空串）、
// 四个 token 字段累加（对齐 runToolLoop 的 st.*Tokens / 退役中的 taskUsage）、
// generated_files 取并集且去重。
func TestAToolLoopTaskProjectsToOneAssistantTurn(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if err := repo.Append(ctx, "sess-loop", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-loop:0:0:user/message", "task_id": "task-7",
			"agent_id": "agent-a", "content": "读 notes.md 再写份报告",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		// 第 1 轮：模型只请求工具，正文是空的。
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "sess-loop:0:0:assistant/message",
			"task_id": "task-7", "agent_id": "agent-a", "content": "",
			"tool_calls":      []any{map[string]any{"call_id": "c1", "name": "read_file"}},
			"usage":           map[string]any{"prompt": 100, "completion": 10, "cached": 4, "total": 110},
			"model_profile":   "fast",
			"generated_files": []any{},
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file", "arguments": `{"path":"notes.md"}`,
		}),
		evWith(5, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "preview": "缓存", "failed": false, "duration_ms": 3,
		}),
		evWith(6, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "completed"}),
		evWith(7, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 1}),
		// 第 2 轮：模型给出最终答案，generated_files 是 runtime 累积到此刻的全量。
		evWith(8, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 1, "turn_id": "sess-loop:0:1:assistant/message",
			"task_id": "task-7", "agent_id": "agent-a", "content": "读完了：那个文件讲的是缓存",
			"tool_calls":      []any{},
			"usage":           map[string]any{"prompt": 220, "completion": 30, "cached": 6, "total": 250},
			"model_profile":   "fast",
			"generated_files": []any{"out/report.md"},
		}),
		evWith(9, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 1, "reason": "completed"}),
		evWith(10, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "completed"}),
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}

	turns, err := repo.ListConversationTurns(ctx, "sess-loop", 0)
	if err != nil {
		t.Fatalf("ListConversationTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("一个带一轮工具的任务投影出 %d 条 turn，要 2 条（一条 user + 一条 assistant）："+
			"每轮模型请求各成一条 turn 的话，GUI 会多出空气泡、模型的 recent-N 窗口会被空 turn 挤满：%#v",
			len(turns), turns)
	}
	if turns[0].ID != "task-7:user" || turns[0].Role != domain.ConversationRoleUser {
		t.Errorf("turns[0] = {ID:%q Role:%q}，要 {task-7:user user}：id 必须与退役中 recordUserTurn 写的形状一致",
			turns[0].ID, turns[0].Role)
	}
	assistant := turns[1]
	if assistant.ID != "task-7:assistant" || assistant.Role != domain.ConversationRoleAssistant {
		t.Fatalf("turns[1] = {ID:%q Role:%q}，要 {task-7:assistant assistant}："+
			"search_session 的 discovery 搜 FTS（serve 写的 \"<task>:assistant\"）、scroll 走投影，两侧 id 必须相等",
			assistant.ID, assistant.Role)
	}
	if assistant.Content != "读完了：那个文件讲的是缓存" {
		t.Errorf("assistant 正文 = %q，要最终答案：取错轮次会把请求工具那轮的空正文当答案", assistant.Content)
	}
	if assistant.PromptTokens != 320 || assistant.CompletionTokens != 40 ||
		assistant.CachedTokens != 10 || assistant.TotalTokens != 360 {
		t.Errorf("token = {prompt:%d completion:%d cached:%d total:%d}，要 {320 40 10 360}："+
			"每轮记的是增量用量，折叠时必须累加成整个任务的用量（退役中 taskUsage 的口径）",
			assistant.PromptTokens, assistant.CompletionTokens, assistant.CachedTokens, assistant.TotalTokens)
	}
	if len(assistant.GeneratedFiles) != 1 || assistant.GeneratedFiles[0] != "out/report.md" {
		t.Errorf("GeneratedFiles = %v，要 [out/report.md] 一项：并集去重，重复挂在多条 turn 上会让 GUI 的文件卡片重复渲染",
			assistant.GeneratedFiles)
	}
}

// 「挂起→恢复」的任务：user/message 被重记一次、assistant/message 多出一条特意
// 不带 generated_files 的重记，两者都必须折进同一行 turn，且已产出的文件不能被抹掉。
//
// 这是 runtime.go 的 resume 分支真实写出的形状：它手工补记一条 assistant/message
// （usage 记 0，因为那条响应的用量已由生成它的那一轮记过；generated_files 传 nil，
// 因为恢复点上 PendingCalls 还没执行）。GeneratedFiles 若按「取最后一条」而不是并集，
// 这条 nil 一旦落在末尾就会把整个任务的产出抹成空，且不报任何错。
func TestASuspendedAndResumedTaskStillProjectsToOneTurnPerRole(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if err := repo.Append(ctx, "sess-resume", []domain.SessionEvent{
		evWith(0, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-resume:0:0:user/message", "task_id": "task-9",
			"agent_id": "agent-a", "content": "写份报告",
		}),
		evWith(1, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "sess-resume:0:0:assistant/message",
			"task_id": "task-9", "agent_id": "agent-a", "content": "先写文件",
			"usage":           map[string]any{"prompt": 50, "completion": 5, "cached": 0, "total": 55},
			"generated_files": []any{"out/report.md"},
		}),
		// 恢复：RunTask 顶部重记 user，resume 分支重记 assistant（usage 0、无文件）。
		evWith(2, domain.SessionEventUserMessage, map[string]any{
			"turn": 1, "turn_id": "sess-resume:1:0:user/message", "task_id": "task-9",
			"agent_id": "agent-a", "content": "写份报告",
		}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 1, "step": 0, "turn_id": "sess-resume:1:0:assistant/message",
			"task_id": "task-9", "agent_id": "agent-a", "content": "报告已写好",
			"usage": map[string]any{"prompt": 0, "completion": 0, "cached": 0, "total": 0},
		}),
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}

	turns, err := repo.ListConversationTurns(ctx, "sess-resume", 0)
	if err != nil {
		t.Fatalf("ListConversationTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("挂起恢复过的任务投影出 %d 条 turn，要 2 条：重记的 user/assistant 必须折进同一行：%#v",
			len(turns), turns)
	}
	if turns[0].Content != "写份报告" || turns[0].ID != "task-9:user" {
		t.Errorf("user turn = {ID:%q Content:%q}，要 {task-9:user 写份报告}", turns[0].ID, turns[0].Content)
	}
	if turns[1].Content != "报告已写好" {
		t.Errorf("assistant 正文 = %q，要恢复后的最终答案", turns[1].Content)
	}
	if len(turns[1].GeneratedFiles) != 1 || turns[1].GeneratedFiles[0] != "out/report.md" {
		t.Errorf("GeneratedFiles = %v，要 [out/report.md]：恢复分支那条重记不带文件，"+
			"按「取最后一条」会把已产出的文件抹掉", turns[1].GeneratedFiles)
	}
	if turns[1].TotalTokens != 55 {
		t.Errorf("TotalTokens = %d，要 55：恢复分支特意记 0，累加不该重复计数", turns[1].TotalTokens)
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

	// 两条消息属于**两个不同的任务**：投影按 (task_id, role) 折叠，同一任务的多条
	// user/message 会折成一行（那正是「挂起→恢复」重记时该发生的事），拿它们来验
	// 「起点是不是 0」就验不出来了。
	if err := repo.Append(ctx, "sess-head", []domain.SessionEvent{
		evWith(0, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-head:0", "task_id": "task-1",
			"agent_id": "agent-a", "content": "第一句话",
		}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 1, "turn_id": "sess-head:1", "task_id": "task-2",
			"agent_id": "agent-a", "content": "第二句话",
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
	if turns[0].ID != "task-1:user" || turns[0].Content != "第一句话" {
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
		// 每条一个任务：投影折叠的键是 (task_id, role)，同一 task_id 下的多条
		// user/message 是「同一次提问」的重记，会折成一行。
		events = append(events, evWith(int64(i), domain.SessionEventUserMessage, map[string]any{
			"turn": i, "turn_id": fmt.Sprintf("sess-1:%d", i), "task_id": fmt.Sprintf("task-%d", i),
			"agent_id": "agent-a", "content": fmt.Sprintf("第 %d 条", i),
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
