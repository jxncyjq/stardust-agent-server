package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// H1 的全部意义：搜索改到事件上之后，工具调用与结果一并可搜。
// 旧的 conversation_turns_fts 只索引对话正文，工具往返从不落盘，所以搜不到。
func TestSearchFindsToolCallsNotJustConversation(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if err := repo.Append(ctx, "sess-1", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-1:1", "task_id": "t", "content": "帮我看看配置",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file",
			"arguments": `{"path":"deploy/kubernetes.yaml"}`,
		}),
		evWith(4, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1",
			"preview": "replicas: 3", "is_error": false,
		}),
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// 工具参数里的路径
	hits, err := repo.SearchMessages(ctx, "kubernetes", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) == 0 {
		t.Error("搜不到工具调用的参数：H1 的全部意义就是让工具往返可搜")
	}

	// 工具结果的预览
	hits, err = repo.SearchMessages(ctx, "replicas", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) == 0 {
		t.Error("搜不到工具结果的预览")
	}
}

// 对话正文照样搜得到——这是既有能力，不能因为换了索引就丢。
func TestSearchStillFindsConversation(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if err := repo.Append(ctx, "sess-1", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-1:1", "task_id": "t",
			"content": "帮我查一下水獭的分布",
		}),
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}

	hits, err := repo.SearchMessages(ctx, "水獭", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) == 0 {
		t.Error("搜不到对话正文：换索引不能丢掉既有能力")
	}
}

// 索引必须与事件在**同一个事务**里提交。P1 的 indexConversationTurn 已经立下这个
// 规矩（「a turn is never persisted without being searchable」），事件侧照办：
// 索引写失败时整批 Append 必须回滚，不能留下搜不到的事件。
func TestAFailedIndexWriteRollsBackTheWholeAppend(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	// 把索引表拿掉，索引写必然失败。这是确定性的——不靠竞态、不靠 sleep。
	if _, err := repo.db.ExecContext(ctx, `DROP TABLE session_events_fts`); err != nil {
		t.Fatalf("drop fts table: %v", err)
	}

	err := repo.Append(ctx, "sess-1", []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-1:1", "task_id": "t", "content": "会被回滚掉",
		}),
	})
	if err == nil {
		t.Fatal("索引写失败了 Append 却成功了：事件会存进去但搜不到")
	}

	// 关键的一半：整批必须回滚，不能留下半条日志。
	events, readErr := repo.ReadFrom(ctx, "sess-1", 0, 0)
	if readErr != nil {
		t.Fatalf("ReadFrom: %v", readErr)
	}
	if len(events) != 0 {
		t.Errorf("回滚后还剩 %d 条事件：索引与事件必须同事务提交", len(events))
	}
}

// 一个连字母数字都没有的查询是调用方的错，不是「零条结果」。
//
// 空结果会被模型读成「历史里确实没有」，而真相是这次检索根本没能构造出来。
func TestAQueryWithNothingToSearchForIsRefused(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if _, err := repo.SearchMessages(ctx, "!!!", 10); err == nil {
		t.Error("没有可检索词的查询返回了 nil error：空结果会被读成「历史里没有」")
	}
	if _, err := repo.SearchMessages(ctx, "  ", 10); err == nil {
		t.Error("空白查询返回了 nil error")
	}
}

// 命中一条没有 task_id 的索引时必须指名报错，而不是返回一个回访不了的 turn。
//
// 写侧不为此拦截：P1 的 Append 契约只管类型/体积/JSON/seq 四道校验，载荷完整性
// 不在其中（一批 P1 的测试正是拿最小载荷造日志的）。所以缺失在**读侧**被抓住——
// hit 的 id 是从 task_id 拼出来的，没有它这条命中就没有地址，返回它等于交给模型一个
// 必然 anchor not found 的锚点。
func TestAHitWithNoTaskIDIsRefusedInsteadOfReturnedUnaddressable(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	// 一条**缺 task_id** 的 user/message：坏掉的发射端会写出这种载荷。
	if err := repo.Append(ctx, "sess-1", []domain.SessionEvent{
		evWith(0, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-1:0", "content": "帮我查一下水獭的分布",
		}),
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}

	hits, err := repo.SearchMessages(ctx, "水獭", 10)
	if err == nil {
		t.Fatalf("命中一条没有 task_id 的索引却返回了 %+v：那个 turn 回访不了", hits)
	}
	if !strings.Contains(err.Error(), "task_id") {
		t.Errorf("错误没说清缺的是什么：%v", err)
	}
}

// toolRoundTripLog 是一条「一次用户输入 → 一次模型响应 → 一次工具往返」的完整日志，
// 事件顺序与 runtime 真实发射的顺序一致（generateStep 先记 assistant/message，
// runToolLoop 再记 tool/call、tool/result）。
func toolRoundTripLog(taskID string) []domain.SessionEvent {
	return []domain.SessionEvent{
		evWith(0, domain.SessionEventTurnStart, map[string]any{"turn": 0}),
		evWith(1, domain.SessionEventUserMessage, map[string]any{
			"turn": 0, "turn_id": "sess-1:0:0:user/message", "task_id": taskID,
			"agent_id": "researcher", "content": "帮我看看配置",
		}),
		evWith(2, domain.SessionEventStepStart, map[string]any{"turn": 0, "step": 0}),
		evWith(3, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 0, "turn_id": "sess-1:0:0:assistant/message",
			"task_id": taskID, "agent_id": "researcher", "content": "我去读一下部署清单",
			"tool_calls":      []any{map[string]any{"call_id": "c1", "name": "read_file"}},
			"usage":           map[string]any{"prompt": 1, "completion": 2, "cached": 0, "total": 3},
			"model_profile":   "dev",
			"generated_files": []any{},
		}),
		evWith(4, domain.SessionEventToolCall, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1", "name": "read_file",
			"arguments": `{"path":"deploy/kubernetes.yaml"}`,
		}),
		evWith(5, domain.SessionEventToolResult, map[string]any{
			"turn": 0, "step": 0, "call_id": "c1",
			"preview": "replicas: 3", "is_error": false, "duration_ms": 1,
		}),
		evWith(6, domain.SessionEventStepEnd, map[string]any{"turn": 0, "step": 0, "reason": "done"}),
		evWith(7, domain.SessionEventAssistantMessage, map[string]any{
			"turn": 0, "step": 1, "turn_id": "sess-1:0:1:assistant/message",
			"task_id": taskID, "agent_id": "researcher", "content": "副本数是 3",
			"tool_calls":      []any{},
			"usage":           map[string]any{"prompt": 4, "completion": 5, "cached": 0, "total": 9},
			"model_profile":   "dev",
			"generated_files": []any{},
		}),
		evWith(8, domain.SessionEventTurnEnd, map[string]any{"turn": 0, "reason": "done"}),
	}
}

// 工具类事件命中时返回的是**发起它的 assistant 那一行 turn 的地址**。
//
// domain.ConversationRole 只有 user/assistant 两个值，没有 tool，所以工具往返归到
// 发起它的 assistant 上（brief 给的两条出路里的第一条）。这条测试断言的不是「role
// 好看」，而是 discovery→scroll 真的接得上：模型拿搜索给的 id 回来 scroll，必须落在
// 投影出来的那一行上，否则就是 anchor not found。
func TestAToolHitIsAddressedByTheAssistantTurnThatOwnsIt(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if err := repo.Append(ctx, "sess-1", toolRoundTripLog("task-1")); err != nil {
		t.Fatalf("append events: %v", err)
	}

	hits, err := repo.SearchMessages(ctx, "kubernetes", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("SearchMessages(kubernetes) = %+v, want 唯一一条工具调用命中", hits)
	}
	hit := hits[0]
	if hit.Role != domain.ConversationRoleAssistant {
		t.Errorf("工具命中的 role = %q, want assistant：工具是 assistant 发起的", hit.Role)
	}
	if hit.ID != "task-1:assistant" {
		t.Errorf("工具命中的 id = %q, want %q：id 空间必须与投影出的 turn 合一",
			hit.ID, "task-1:assistant")
	}
	if hit.TaskID != "task-1" || hit.SessionID != "sess-1" || hit.AgentID != "researcher" {
		t.Errorf("工具命中没继承发起它的那个任务的身份：%+v", hit)
	}
	if hit.Content != `read_file {"path":"deploy/kubernetes.yaml"}` {
		t.Errorf("工具命中的 content = %q, want 工具名 + 参数：命中的是这条事件，正文就该是它",
			hit.Content)
	}

	// 真正的判据：拿 discovery 给的 id 回来 scroll，必须命中。
	scrolled, err := repo.ScrollMessages(ctx, "sess-1", hit.ID, 5)
	if err != nil {
		t.Fatalf("ScrollMessages(discovery 给的 %q): %v：discovery 与 scroll 的 id 空间分叉了",
			hit.ID, err)
	}
	if len(scrolled) != 2 {
		t.Fatalf("ScrollMessages = %+v, want 该任务的 user + assistant 两行", scrolled)
	}
}

// 命中一条**中间轮次**的 assistant 事件时，返回的仍是那一行折叠 turn 的地址，
// 正文则是命中的那条事件自己的正文。
//
// 投影按 (task_id, role) 折叠，assistant 一行的正文取最后一条事件（最终答案）。
// 中间轮次的正文因此不在任何一行 turn 里，但它确实发生过、也确实被搜到了：返回
// 命中事件的正文（「搜到的是什么」）+ 折叠 turn 的 id（「回去哪儿看」），两者都不丢。
func TestAHitOnAnIntermediateRoundStillAddressesTheFoldedTurn(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if err := repo.Append(ctx, "sess-1", toolRoundTripLog("task-1")); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// "部署清单" 只出现在 seq 3 那条中间轮次的 assistant/message 里；
	// 折叠出来的 turn 正文是 seq 7 的 "副本数是 3"。
	hits, err := repo.SearchMessages(ctx, "部署清单", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("SearchMessages(部署清单) = %+v, want 唯一一条中间轮次命中", hits)
	}
	if hits[0].ID != "task-1:assistant" {
		t.Errorf("中间轮次命中的 id = %q, want %q：它属于折叠出来的那一行",
			hits[0].ID, "task-1:assistant")
	}
	if hits[0].Content != "我去读一下部署清单" {
		t.Errorf("中间轮次命中的 content = %q, want 命中那条事件自己的正文", hits[0].Content)
	}

	turns, err := repo.ListConversationTurns(ctx, "sess-1", 0)
	if err != nil {
		t.Fatalf("ListConversationTurns: %v", err)
	}
	var assistant domain.ConversationTurn
	for _, turn := range turns {
		if turn.Role == domain.ConversationRoleAssistant {
			assistant = turn
		}
	}
	if assistant.ID != hits[0].ID {
		t.Errorf("命中的 id %q 不等于投影出的 assistant turn id %q：两个 id 空间必须相等",
			hits[0].ID, assistant.ID)
	}
	if assistant.Content != "副本数是 3" {
		t.Errorf("折叠出的 assistant 正文 = %q, want 最终答案", assistant.Content)
	}
}

// 生产上 tool/call 与它的 assistant/message **不在同一批**里落盘。
//
// runtime 的三个屏障（spec §5）在发模型请求前、进工具体前、开下一步前各 flush 一次，
// 所以一次工具往返被切成好几个 Append 批次：assistant/message 早就提交过了，tool/call
// 单独成批。工具事件的身份因此必须能回库里查到发起它的那个任务——只在批内往前找的
// 实现在单批测试里全绿，一接上真的 runtime 就退化成「工具往返搜得到、却没有 task_id
// 可回访」。
func TestAToolEventFlushedInItsOwnBatchStillInheritsItsTask(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	log := toolRoundTripLog("task-1")
	// 按屏障切批：[turn/start..assistant/message] / [tool/call] / [tool/result..]。
	for _, batch := range [][]domain.SessionEvent{log[0:4], log[4:5], log[5:]} {
		if err := repo.Append(ctx, "sess-1", batch); err != nil {
			t.Fatalf("append batch starting at seq %d: %v", batch[0].Seq, err)
		}
	}

	hits, err := repo.SearchMessages(ctx, "kubernetes", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("SearchMessages(kubernetes) = %+v, want 唯一一条工具调用命中", hits)
	}
	if hits[0].ID != "task-1:assistant" || hits[0].TaskID != "task-1" || hits[0].AgentID != "researcher" {
		t.Errorf("单独成批的 tool/call 没继承发起它的任务身份：%+v", hits[0])
	}

	// tool/result 也单独一批（它排在 log[5:] 的批首，前面没有消息事件）。
	resultHits, err := repo.SearchMessages(ctx, "replicas", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(resultHits) != 1 || resultHits[0].ID != "task-1:assistant" {
		t.Errorf("单独成批的 tool/result 没继承发起它的任务身份：%+v", resultHits)
	}
}

// 删掉会话要把它从检索索引里一并删掉。
//
// 检索现在读的是 session_events_fts；只删事件不删索引，被删会话的对话会继续被
// discovery 搜出来，而 scroll 回去时会 anchor not found——一次「看得见地什么都没删掉」
// 的删除。
func TestDeletingASessionRemovesItFromTheSearchIndex(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if err := repo.SaveAgentSession(ctx, domain.AgentSession{
		ID: "sess-1", CompanyID: "acme", AgentID: "researcher",
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("SaveAgentSession: %v", err)
	}
	if err := repo.Append(ctx, "sess-1", toolRoundTripLog("task-1")); err != nil {
		t.Fatalf("append events: %v", err)
	}
	if hits, err := repo.SearchMessages(ctx, "kubernetes", 10); err != nil || len(hits) != 1 {
		t.Fatalf("删除前 SearchMessages = %+v (err %v), want 1 条", hits, err)
	}

	if err := repo.DeleteAgentSession(ctx, "sess-1"); err != nil {
		t.Fatalf("DeleteAgentSession: %v", err)
	}

	hits, err := repo.SearchMessages(ctx, "kubernetes", 10)
	if err != nil {
		t.Fatalf("SearchMessages after delete: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("会话删掉了却还搜得到 %+v：删除必须级联到检索索引", hits)
	}
}

// evAt 是 evWith 的定时版本：跨会话的先后由 created_at 决定，而 evWith 的时间是从
// seq 推出来的，两条会话的 seq 都从 0 起，光靠它排不出「谁更早」。
func evAt(seq int64, typ domain.SessionEventType, at time.Time, data map[string]any) domain.SessionEvent {
	event := evWith(seq, typ, data)
	event.Time = at
	return event
}

// chattyTaskLog 造一条「一个任务跑了 rounds 轮工具，每一轮都提到 word」的日志。
//
// 形状与 toolRoundTripLog 一致（assistant/message → tool/call → tool/result），
// 只是把轮数拉到工具循环的上限量级：这正是生产上一个话痨任务留下的可搜行数。
func chattyTaskLog(taskID string, word string, rounds int, start time.Time) []domain.SessionEvent {
	seq := int64(0)
	at := func() time.Time {
		return start.Add(time.Duration(seq) * time.Second)
	}
	events := []domain.SessionEvent{
		evAt(seq, domain.SessionEventTurnStart, at(), map[string]any{"turn": 0}),
	}
	seq++
	events = append(events, evAt(seq, domain.SessionEventUserMessage, at(), map[string]any{
		"turn": 0, "turn_id": "chatty:0:0:user/message", "task_id": taskID,
		"agent_id": "researcher", "content": "帮我查一下" + word + "的分布",
	}))
	for round := 0; round < rounds; round++ {
		seq++
		events = append(events, evAt(seq, domain.SessionEventAssistantMessage, at(), map[string]any{
			"turn": 0, "step": round,
			"turn_id": fmt.Sprintf("chatty:0:%d:assistant/message", round),
			"task_id": taskID, "agent_id": "researcher",
			"content":         fmt.Sprintf("第 %d 次翻%s的资料", round, word),
			"tool_calls":      []any{map[string]any{"call_id": fmt.Sprintf("c%d", round), "name": "read_file"}},
			"usage":           map[string]any{"prompt": 1, "completion": 1, "cached": 0, "total": 2},
			"model_profile":   "dev",
			"generated_files": []any{},
		}))
		seq++
		events = append(events, evAt(seq, domain.SessionEventToolCall, at(), map[string]any{
			"turn": 0, "step": round, "call_id": fmt.Sprintf("c%d", round),
			"name": "read_file", "arguments": fmt.Sprintf(`{"path":"%s-%d.md"}`, word, round),
		}))
		seq++
		events = append(events, evAt(seq, domain.SessionEventToolResult, at(), map[string]any{
			"turn": 0, "step": round, "call_id": fmt.Sprintf("c%d", round),
			"preview": word + "的分布见下表", "is_error": false, "duration_ms": 1,
		}))
	}
	return events
}

// 一个话痨任务不许占满一页 discovery——别的会话的命中必须还看得见。
//
// 这是 discovery 存在的全部理由（「跨会话找回历史」）。一个任务折叠出来只有两行
// turn，却按事件写出上百条可搜行：工具循环上限 30 轮 ≈ 30 条 tool/call + 30 条
// tool/result + 31 条 assistant/message。它们只要词面命中，默认 20 条结果就会被
// 这**一个任务的两个 id** 全部占满，模型看不到任何别的会话（P3 Task 4 复审
// Important-2）。
//
// 用两个字的中文查询是刻意的：它走 LIKE 分支，排序退化成 created_at DESC，最近
// 那个话痨任务会稳定霸榜——这正是复审指出的最坏情况，也让这条断言不依赖 bm25 的
// 打分碰巧把谁排前面。
func TestAChattyTaskDoesNotCrowdOtherSessionsOffTheDiscoveryPage(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	// 更早的一条会话：只提了一次「水獭」。
	if err := repo.Append(ctx, "sess-quiet", []domain.SessionEvent{
		evAt(0, domain.SessionEventUserMessage, time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC),
			map[string]any{
				"turn": 0, "turn_id": "sess-quiet:0:0:user/message", "task_id": "task-quiet",
				"agent_id": "researcher", "content": "水獭那次结论是什么",
			}),
	}); err != nil {
		t.Fatalf("append quiet session: %v", err)
	}

	// 更晚的一条会话：一个任务跑满 30 轮，每一轮都提到「水獭」。
	chatty := chattyTaskLog("task-chatty", "水獭", 30, time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	if err := repo.Append(ctx, "sess-chatty", chatty); err != nil {
		t.Fatalf("append chatty session: %v", err)
	}
	// 前提自检：这个任务确实写出了远超一页的可搜行，否则这条测试什么也没守住。
	var searchable int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_events_fts WHERE session_id = ?`, "sess-chatty",
	).Scan(&searchable); err != nil {
		t.Fatalf("count searchable rows: %v", err)
	}
	if searchable <= 20 {
		t.Fatalf("话痨任务只写出 %d 条可搜行，撑不满一页：这条测试没有守住任何东西", searchable)
	}

	hits, err := repo.SearchMessages(ctx, "水獭", 20)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}

	perTurn := map[string]int{}
	quietSeen := false
	for _, hit := range hits {
		perTurn[hit.ID]++
		if hit.SessionID == "sess-quiet" {
			quietSeen = true
		}
	}
	if !quietSeen {
		t.Errorf("话痨任务占满了这一页 %+v：discovery 的全部意义是跨会话找回历史，"+
			"别的会话的命中不能被一个任务挤掉", hits)
	}
	for id, count := range perTurn {
		if count > 2 {
			t.Errorf("turn %q 在一页里出现了 %d 次, want <= 2", id, count)
		}
	}
	if len(hits) > 20 {
		t.Errorf("返回了 %d 条, want <= limit 20", len(hits))
	}
}

// 限流不是去重：同一行 turn 的两条命中都要留下来。
//
// 去重会把「哪一轮匹配的」这个信息藏掉——而那正是一条命中要携带的内容。上限取 2
// 就是为了让「调用」与「结果」这一对还能同时出现。
func TestTheCapKeepsTwoHitsOfTheSameTurn(t *testing.T) {
	repo := newEventRepo(t)
	ctx := context.Background()

	if err := repo.Append(ctx, "sess-1", toolRoundTripLog("task-1")); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// "3" 在同一行 turn 的两条事件里各出现一次：tool/result 的预览 "replicas: 3"
	// 与最后那条 assistant/message 的正文 "副本数是 3"。
	hits, err := repo.SearchMessages(ctx, "3", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("SearchMessages(3) = %+v, want 同一行 turn 的两条命中（上限是 2，不是去重成 1）", hits)
	}
	if hits[0].ID != "task-1:assistant" || hits[1].ID != "task-1:assistant" {
		t.Errorf("两条命中的 id = %q/%q, want 都是 %q", hits[0].ID, hits[1].ID, "task-1:assistant")
	}
	if hits[0].Content == hits[1].Content {
		t.Errorf("两条命中的正文相同（%q）：命中携带的是那条事件自己的正文", hits[0].Content)
	}
}
