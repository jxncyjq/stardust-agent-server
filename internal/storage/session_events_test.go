package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

func newEventRepo(t *testing.T) *SQLiteRepository {
	t.Helper()
	repo, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// ev 造一个最小的合法事件。
//
// 载荷带 turn/step 两个字段（都是 0）而不是空对象：Load 的崩溃恢复
// （planRecovery）会从每一条 turn/start 取 "turn"、每一条 step/start 取
// "turn"/"step"，取不到就 fail-loud 报错（不允许编造零值接着跑）。多数任务
// 1-4 的测试只看 seq/type/count，不检查载荷内容，所以这两个多出来的字段
// 对它们无影响；但任务 5 的 Load 测试复用同一个 ev()，载荷就必须是真实字段。
func ev(seq int64, typ domain.SessionEventType) domain.SessionEvent {
	return domain.SessionEvent{
		Seq:  seq,
		Type: typ,
		Time: time.UnixMilli(1_700_000_000_000 + seq),
		Data: []byte(`{"turn":0,"step":0}`),
	}
}

// 建表这一条不是形式主义：表名/主键写错的症状是「一切正常，直到两条事件撞了 seq」，
// 而那时错误信息指向的是 UNIQUE 约束，不是这次改动。
func TestTheSessionEventsTableExistsWithACompositeKey(t *testing.T) {
	ctx := context.Background()
	repo, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()

	var count int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='session_events'`,
	).Scan(&count); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if count != 1 {
		t.Fatalf("session_events 表不存在")
	}

	// 主键必须是 (session_id, seq) 两列：只按 seq 建键会让不同会话互相挤号。
	rows, err := repo.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('session_events') WHERE pk > 0 ORDER BY pk`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer rows.Close()
	var pk []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan pk column: %v", err)
		}
		pk = append(pk, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pk columns: %v", err)
	}
	if len(pk) != 2 || pk[0] != "session_id" || pk[1] != "seq" {
		t.Errorf("主键 = %v, want [session_id seq]", pk)
	}
}

// 首个 seq 必须等于 next-seq（spec §4.3 不变量 1）。
//
// 这条挡的是「两个写入方各按各的计数往里写」：一旦 seq 出现重叠或跳号，
// 日志就不再能重建出唯一的历史，而错误会在很久之后以「读的时候少一段」出现。
func TestAppendRefusesASeqThatDoesNotContinueTheLog(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	if err := repo.Append(ctx, "s1", []domain.SessionEvent{ev(0, domain.SessionEventTurnStart)}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// 库里的 next-seq 现在是 1；递交一个从 2 开始的批次必须被拒。
	err := repo.Append(ctx, "s1", []domain.SessionEvent{ev(2, domain.SessionEventStepStart)})
	if err == nil {
		t.Fatal("跳号的批次被接受了")
	}
	if !strings.Contains(err.Error(), "2") || !strings.Contains(err.Error(), "1") {
		t.Errorf("错误没同时给出实际与期望的 seq：%v", err)
	}
}

// 批中途失败要整批回滚（spec §4.3 不变量 1）。
//
// 半批写入留下的是一个「seq 连续但内容缺了后半段」的日志——它读得出来、也验得过，
// 却与真实发生的事不符。那种损坏比读不出来更难发现。
func TestAFailedBatchLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	// 第二条与第一条撞 seq：INSERT 到它时违反主键，整批必须回滚。
	batch := []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(0, domain.SessionEventUserMessage),
	}
	if err := repo.Append(ctx, "s1", batch); err == nil {
		t.Fatal("批内重复 seq 被接受了")
	}

	var remaining int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_events WHERE session_id = ?`, "s1").Scan(&remaining); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if remaining != 0 {
		t.Errorf("回滚之后还剩 %d 条事件：半批写入留下的日志读得出来却与真实发生的事不符", remaining)
	}
}

// 懒物化（spec §4.3 不变量 5）：没有事件的会话不在事件表里留任何痕迹。
func TestASessionWithNoEventsLeavesNoTrace(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	if err := repo.Append(ctx, "s1", nil); err != nil {
		t.Fatalf("Append(nil) = %v, want nil：空批次是合法的无操作", err)
	}
	var count int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_events`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Errorf("空批次写进了 %d 行", count)
	}
}

// 未知事件类型在写入时就被拒（不变量 4）：让它进库，读的那一方就只剩两个坏选择
// ——静默跳过（历史缺一段）或整条会话读不出来。
func TestAppendRefusesAnUnknownEventType(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	bad := domain.SessionEvent{Seq: 0, Type: "tool/telepathy", Time: time.UnixMilli(1), Data: []byte(`{}`)}
	err := repo.Append(ctx, "s1", []domain.SessionEvent{bad})
	if err == nil {
		t.Fatal("未知事件类型被写进库了")
	}
	if !strings.Contains(err.Error(), "tool/telepathy") {
		t.Errorf("错误没指名类型：%v", err)
	}
}

// 批内跳号必须被拒（spec §4.3 不变量 1），不能只验首条。
//
// 首条对上库里的 next-seq 只保证了批次的起点正确；批内后续元素若跳号，
// 会在日志中间留一个永久空洞——ReadFrom 一旦读到这个洞就判定整条会话损坏，
// 而这正是首条校验本想防住、却漏掉的同一类损坏。
func TestAppendRefusesASeqGapWithinTheBatch(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	// 首条 seq=0 对得上 next-seq=0；第二条本该是 1，却给了 2。
	batch := []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(2, domain.SessionEventStepStart),
	}
	err := repo.Append(ctx, "s1", batch)
	if err == nil {
		t.Fatal("批内跳号的批次被接受了")
	}
	if !strings.Contains(err.Error(), "1") || !strings.Contains(err.Error(), "2") {
		t.Errorf("错误没同时给出实际与期望的 seq：%v", err)
	}
	// 错误要能看出是批内第二个元素（下标 1）出的问题。
	if !strings.Contains(err.Error(), "element 1") {
		t.Errorf("错误没指出是批内哪一个元素：%v", err)
	}

	var remaining int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_events WHERE session_id = ?`, "s1").Scan(&remaining); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if remaining != 0 {
		t.Errorf("批内跳号被拒后表里还剩 %d 条事件：校验必须在任何 INSERT 之前完成", remaining)
	}
}

// 非法 JSON 载荷在写入时就被拒（decodeSessionEvent 的镜像检查）：写入侧不查，
// 一段非法 JSON 就能进库，而读路径会拒绝解码它——这条会话此后永远读不出来。
func TestAppendRefusesInvalidJSONData(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	bad := domain.SessionEvent{
		Seq: 0, Type: domain.SessionEventTurnStart, Time: time.UnixMilli(1),
		Data: []byte(`{not valid json`),
	}
	err := repo.Append(ctx, "s1", []domain.SessionEvent{bad})
	if err == nil {
		t.Fatal("非法 JSON 载荷被写进库了")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("错误没提到 JSON：%v", err)
	}

	var remaining int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_events WHERE session_id = ?`, "s1").Scan(&remaining); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if remaining != 0 {
		t.Errorf("非法 JSON 被拒后表里还剩 %d 条事件", remaining)
	}
}

// 大载荷不进事件（不变量 6）：事件表的增长必须与**调用次数**成正比，
// 而不与工具输出体积成正比。超限的输出走 spill，事件里只留预览 + 定位符。
//
// 这条守在存储层，是为了让 P2 接发射点的人在**写错的当场**就看见，
// 而不是在库涨到几个 G 之后。
func TestAnOversizedPayloadIsRefused(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	huge := make([]byte, maxSessionEventDataBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}
	payload, err := json.Marshal(map[string]string{"preview": string(huge)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	err = repo.Append(ctx, "s1", []domain.SessionEvent{{
		Seq: 0, Type: domain.SessionEventToolResult, Time: time.UnixMilli(1), Data: payload,
	}})
	if err == nil {
		t.Fatal("超限载荷被写进事件了：那会让事件表随工具输出体积膨胀")
	}
	if !strings.Contains(err.Error(), "spill") {
		t.Errorf("错误没告诉写的人该走 spill：%v", err)
	}
}

func TestReadFromReturnsOnlyTheSuffix(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(1, domain.SessionEventUserMessage),
		ev(2, domain.SessionEventStepStart),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := repo.ReadFrom(ctx, "s1", 1)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("ReadFrom(1) 返回 %d 条（seq %v），want seq [1 2]", len(got), seqsOf(got))
	}
}

// 越过末尾返回空，不是错误：轨迹的增量拉取会不断问「有没有比我这条更新的」，
// 「暂时没有」是正常答案。
func TestReadFromPastTheEndIsEmptyNotAnError(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{ev(0, domain.SessionEventTurnStart)}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := repo.ReadFrom(ctx, "s1", 99)
	if err != nil {
		t.Fatalf("ReadFrom(99) = %v, want nil error", err)
	}
	if len(got) != 0 {
		t.Errorf("ReadFrom(99) 返回了 %d 条", len(got))
	}
}

// ReadFrom 不改库：一次「看一眼」不该改变被看的东西。轨迹在翻页，
// 而 Load 的崩溃恢复会写入——两者混在一起，翻页就会静默地改写历史。
func TestReadFromDoesNotWriteAnything(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	// 半个 turn：有 tool/call 没有 tool/result。Load 会为它补事件，ReadFrom 不该。
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(1, domain.SessionEventStepStart),
		toolCall(2, "call-1"),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	before := countEvents(t, repo, "s1")
	if _, err := repo.ReadFrom(ctx, "s1", 0); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if after := countEvents(t, repo, "s1"); after != before {
		t.Errorf("ReadFrom 之后行数从 %d 变成 %d：它不该写任何东西", before, after)
	}
}

// 中间断裂 = 损坏，拒绝（spec §4.3 不变量 3）。
//
// 静默跳过一个洞，等于把「这段历史缺了一块」变成「这段历史就是这样」——
// 而缺掉的恰好可能是那次出问题的工具调用。
func TestAHoleInTheMiddleIsRefused(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	// 需要 seq 1 之后还有更高的 seq（这里是 2）：删掉 1 之后剩下 {0, 2}，
	// 这才是「中间」真断了一截——如果只写到 seq 1 就删掉它，剩下的 {0} 和
	// 「这个会话本来就只有一条事件」在数据上完全等价，读不出任何异常。
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(1, domain.SessionEventStepStart),
		ev(2, domain.SessionEventAssistantMessage),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 绕过 Append 直接制造一个洞（seq 1 被删掉）。真实成因是行损坏或人工干预。
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM session_events WHERE session_id='s1' AND seq=1`); err != nil {
		t.Fatalf("制造断裂: %v", err)
	}

	_, err := repo.ReadFrom(ctx, "s1", 0)
	if err == nil {
		t.Fatal("seq 有洞却读成功了")
	}
	if !strings.Contains(err.Error(), "1") {
		t.Errorf("错误没指出断在哪里：%v", err)
	}
}

// 窗口起点本身落在洞里必须被拒（spec §4.3 不变量 3 的一个特殊情形）。
//
// 相邻行检查只验「已返回的行之间」，天生放过窗口起点：表里剩 {0, 2}（seq=1 被
// 删掉），ReadFrom(s1, 1) 命中的第一行就是 seq=2，expected 的初值 -1 让这一行
// 绕过检查，函数会返回 [{Seq:2}] 和 nil error——调用方（轨迹翻页，典型调用正是
// ReadFrom(sessionID, 上次读到的 seq + 1)）就会把「seq=1 曾经存在又消失了」
// 误当成「从这里开始本来就是这样」而永远无法察觉。
func TestReadFromWithFromSeqLandingInAHoleIsRefused(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(1, domain.SessionEventStepStart),
		ev(2, domain.SessionEventAssistantMessage),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 绕过 Append 直接制造一个洞（seq 1 被删掉），表里剩 {0, 2}。
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM session_events WHERE session_id='s1' AND seq=1`); err != nil {
		t.Fatalf("制造断裂: %v", err)
	}

	_, err := repo.ReadFrom(ctx, "s1", 1)
	if err == nil {
		t.Fatal("fromSeq 落在洞里却读成功了：调用方无法察觉 seq=1 曾经存在又消失了")
	}
	if !strings.Contains(err.Error(), "1") || !strings.Contains(err.Error(), "2") {
		t.Errorf("错误没同时给出请求起点与实际首条：%v", err)
	}
}

// fail-loud 分支须有测试断言「确实返回 error」（CLAUDE.md「测试」一节）：
// fromSeq < 0 的校验此前没有任何用例覆盖它真的会报错。
//
// 会话故意留空（不 Append 任何事件）：如果这里先写入事件，"seq >= -1" 的查询
// 会命中那些事件，窗口起点校验（events[0].Seq != fromSeq）也会报错，测试就算
// 删掉 fromSeq < 0 这条校验也照样红→绿地"通过"——那样就验不出这条校验本身是否
// 存在。留空会话时，没有这条校验查询会直接返回空切片和 nil error，只有专门的
// 负数校验能让这个用例报错。
func TestReadFromRefusesANegativeFromSeq(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	_, err := repo.ReadFrom(ctx, "s1", -1)
	if err == nil {
		t.Fatal("fromSeq 为负数却读成功了")
	}
	if !strings.Contains(err.Error(), "-1") {
		t.Errorf("错误没提到非法值 -1：%v", err)
	}
}

func seqsOf(events []domain.SessionEvent) []int64 {
	out := make([]int64, 0, len(events))
	for _, e := range events {
		out = append(out, e.Seq)
	}
	return out
}

func countEvents(t *testing.T, repo *SQLiteRepository, sessionID string) int {
	t.Helper()
	var count int
	if err := repo.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM session_events WHERE session_id = ?`, sessionID).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return count
}

// toolCall 造一条带 call_id 的 tool/call 事件。
func toolCall(seq int64, callID string) domain.SessionEvent {
	data, err := json.Marshal(map[string]any{"turn": 0, "step": 0, "call_id": callID, "name": "read_file", "arguments": "{}"})
	if err != nil {
		panic(err)
	}
	return domain.SessionEvent{Seq: seq, Type: domain.SessionEventToolCall, Time: time.UnixMilli(1), Data: data}
}

// 崩溃恢复：半个 turn 要被补成合法的 provider transcript（spec §4.3 不变量 2）。
//
// 判据不是「补了几条」，而是**每个 tool/call 都有与之 call_id 对应的 tool/result**，
// 且 turn 以 interrupted 收尾。少了任何一条，重建出的消息数组发给模型就是非法的。
func TestLoadClosesAnInterruptedTurnIntoAValidTranscript(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	// 崩在两个工具调用之间：call-1 有结果，call-2 没有。
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(1, domain.SessionEventStepStart),
		toolCall(2, "call-1"),
		toolResult(3, "call-1"),
		toolCall(4, "call-2"),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	events, err := repo.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	answered := map[string]bool{}
	for _, e := range events {
		switch e.Type {
		case domain.SessionEventToolCall:
			// 记名但不覆盖已有的 true：自赋值（answered[x] = answered[x]）会被
			// go vet 判为错误，而这个仓要求 vet 全绿。
			if _, seen := answered[callIDOf(t, e)]; !seen {
				answered[callIDOf(t, e)] = false
			}
		case domain.SessionEventToolResult:
			answered[callIDOf(t, e)] = true
		}
	}
	for callID, ok := range answered {
		if !ok {
			t.Errorf("call %q 没有对应的 tool/result：这样重建出的消息数组发给模型是非法的", callID)
		}
	}

	last := events[len(events)-1]
	if last.Type != domain.SessionEventTurnEnd {
		t.Fatalf("最后一条是 %s，want turn/end", last.Type)
	}
	if reason := reasonOf(t, last); reason != domain.TurnEndReasonInterrupted {
		t.Errorf("turn/end 的 reason = %q, want %q——这是「这段历史不是自己结束的」的唯一记号",
			reason, domain.TurnEndReasonInterrupted)
	}
}

// 恢复要落盘：下一次 Append 从补齐后的 seq continue，而不是又撞回半个 turn。
func TestLoadPersistsTheRecoveryItSynthesized(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(1, domain.SessionEventStepStart),
		toolCall(2, "call-1"),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	recovered, err := repo.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 再读一次：补出来的事件必须在库里，而不只是在返回值里。
	again, err := repo.ReadFrom(ctx, "s1", 0)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if len(again) != len(recovered) {
		t.Fatalf("库里有 %d 条，Load 返回了 %d 条：恢复没有落盘，下次打开还会再补一次",
			len(again), len(recovered))
	}
	// 且下一批能接着写。
	next := int64(len(again))
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{ev(next, domain.SessionEventTurnStart)}); err != nil {
		t.Errorf("恢复之后接着写失败：%v", err)
	}
}

// 已经收尾的日志不该被动：Load 是幂等的，读两次不会越补越长。
func TestLoadLeavesABalancedLogAlone(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(1, domain.SessionEventStepStart),
		toolCall(2, "call-1"),
		toolResult(3, "call-1"),
		stepEnd(4, domain.StepEndReasonCompleted),
		turnEnd(5, domain.TurnEndReasonCompleted),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	first, err := repo.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	second, err := repo.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if len(first) != 6 || len(second) != 6 {
		t.Errorf("Load 改了一个本来就平衡的日志：%d then %d, want 6", len(first), len(second))
	}
}

// toolResult 造一条带 call_id 的 tool/result 事件。
func toolResult(seq int64, callID string) domain.SessionEvent {
	data, err := json.Marshal(map[string]any{
		"turn": 0, "step": 0, "call_id": callID, "preview": "ok", "is_error": false, "duration_ms": 1,
	})
	if err != nil {
		panic(err)
	}
	return domain.SessionEvent{Seq: seq, Type: domain.SessionEventToolResult, Time: time.UnixMilli(1), Data: data}
}

// stepEnd 造一条 step/end 事件。
func stepEnd(seq int64, reason string) domain.SessionEvent {
	data, err := json.Marshal(map[string]any{"turn": 0, "step": 0, "reason": reason})
	if err != nil {
		panic(err)
	}
	return domain.SessionEvent{Seq: seq, Type: domain.SessionEventStepEnd, Time: time.UnixMilli(1), Data: data}
}

// turnEnd 造一条 turn/end 事件。
func turnEnd(seq int64, reason string) domain.SessionEvent {
	data, err := json.Marshal(map[string]any{"turn": 0, "reason": reason})
	if err != nil {
		panic(err)
	}
	return domain.SessionEvent{Seq: seq, Type: domain.SessionEventTurnEnd, Time: time.UnixMilli(1), Data: data}
}

// callIDOf 从事件载荷里取 call_id，取不到就当场 Fatal——这是测试断言的一部分，
// 不是被测代码的行为。
func callIDOf(t *testing.T, e domain.SessionEvent) string {
	t.Helper()
	var payload struct {
		CallID string `json:"call_id"`
	}
	if err := json.Unmarshal(e.Data, &payload); err != nil {
		t.Fatalf("unmarshal call_id from %s: %v", e.Type, err)
	}
	return payload.CallID
}

// reasonOf 从事件载荷里取 reason，取不到就当场 Fatal。
func reasonOf(t *testing.T, e domain.SessionEvent) string {
	t.Helper()
	var payload struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(e.Data, &payload); err != nil {
		t.Fatalf("unmarshal reason from %s: %v", e.Type, err)
	}
	return payload.Reason
}
