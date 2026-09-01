package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/storage"
)

// stubEventStore 是一个什么都不做的 store，供不关心写入结果的用例使用。
type stubEventStore struct{}

func (stubEventStore) Append(context.Context, string, []domain.SessionEvent) error { return nil }
func (stubEventStore) ReadFrom(context.Context, string, int64) ([]domain.SessionEvent, error) {
	return nil, nil
}
func (stubEventStore) Load(context.Context, string) ([]domain.SessionEvent, error) { return nil, nil }

// 会话号的来源有两个，且**都不能是空**：一条写不进任何会话的事件等于没记。
//
// task.SessionID 是常态（server 与 CLI 都会填）；空的情况来自单次任务与委派子任务，
// 那时用 task.ID —— 每个这样的任务自成一条短日志，轨迹一样看得到，且不需要特例分支。
func TestTheSessionIDFallsBackToTheTaskID(t *testing.T) {
	t.Parallel()

	withSession := newEventRecorder(stubEventStore{}, domain.Task{ID: "t1", SessionID: "s1"})
	if got := withSession.sessionID(); got != "s1" {
		t.Errorf("sessionID() = %q, want %q", got, "s1")
	}

	withoutSession := newEventRecorder(stubEventStore{}, domain.Task{ID: "t1"})
	if got := withoutSession.sessionID(); got != "t1" {
		t.Errorf("sessionID() = %q, want the task id %q: 没有会话号的任务也要有自己的日志", got, "t1")
	}
}

// 两者都空 = 这条任务没有任何身份，写出来的事件谁也认不回去。fail-loud。
func TestARecorderWithNoIdentityIsRefused(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("既没有 SessionID 也没有 ID 的任务被接受了：写出来的事件谁也认不回去")
		}
	}()
	newEventRecorder(stubEventStore{}, domain.Task{})
}

// 没有配 store 的部署（内存后端、测试构造）不记事件。
//
// 这**不是兜底**：Config.SessionEvents 是契约里显式声明的可选项（见它的文档注释），
// 「没有配」是一种合法部署形态，与「配了但写不进去」是两回事——后者必须硬失败。
func TestNoStoreMeansNoRecording(t *testing.T) {
	t.Parallel()

	rec := newEventRecorder(nil, domain.Task{ID: "t1"})
	if rec.enabled() {
		t.Error("没有 store 却报告 enabled")
	}
}

// captureEventStore 记下被 Append 的事件，供断言序列与载荷。
//
// 事件同时按会话号分桶（bySession）：只看扁平的 events 无法回答「这条事件写进了哪条
// 会话日志」，而 D-A（没有 SessionID 就用 task.ID 当会话号）与 D-B（委派子任务写自己
// 的日志）恰恰只有这一个可观测面。之前这个夹具把 Append 的 sessionID 直接丢掉，于是
// 任何用它的测试在结构上就不可能断言 D-A/D-B —— 给子任务塞一个父会话号，测试照样绿。
type captureEventStore struct {
	mu        sync.Mutex
	events    []domain.SessionEvent
	bySession map[string][]domain.SessionEvent
	err       error // 非 nil 时 Append 失败，供屏障测试用
}

func (c *captureEventStore) Append(_ context.Context, sessionID string, events []domain.SessionEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.events = append(c.events, events...)
	if c.bySession == nil {
		c.bySession = make(map[string][]domain.SessionEvent)
	}
	c.bySession[sessionID] = append(c.bySession[sessionID], events...)
	return nil
}

// ReadFrom 真的按会话回放已落盘的事件（seq >= from）。
//
// 恒返回 nil 的版本让 newTaskRecorder 解出的 turn 永远是 0：「turn 号 = 已有事件里
// 最大 turn + 1」这段逻辑（spec §4.1 的单调性）一行都执行不到。回放真实内容才让它
// 可被断言。
func (c *captureEventStore) ReadFrom(_ context.Context, sessionID string, from int64) ([]domain.SessionEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []domain.SessionEvent
	for _, e := range c.bySession[sessionID] {
		if e.Seq >= from {
			out = append(out, e)
		}
	}
	return out, nil
}
func (c *captureEventStore) Load(context.Context, string) ([]domain.SessionEvent, error) {
	return nil, nil
}

// eventsFor 返回写进某条会话日志的事件。
func (c *captureEventStore) eventsFor(sessionID string) []domain.SessionEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]domain.SessionEvent(nil), c.bySession[sessionID]...)
}

func (c *captureEventStore) types() []domain.SessionEventType {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]domain.SessionEventType, 0, len(c.events))
	for _, e := range c.events {
		out = append(out, e.Type)
	}
	return out
}

func intFieldOf(t *testing.T, e domain.SessionEvent, name string) int {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(e.Data, &payload); err != nil {
		t.Fatalf("unmarshal %s: %v", e.Type, err)
	}
	value, ok := payload[name].(float64)
	if !ok {
		t.Fatalf("%s 的载荷里没有数值字段 %q", e.Type, name)
	}
	return int(value)
}

func stringFieldOf(t *testing.T, e domain.SessionEvent, name string) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(e.Data, &payload); err != nil {
		t.Fatalf("unmarshal %s: %v", e.Type, err)
	}
	value, ok := payload[name].(string)
	if !ok {
		t.Fatalf("%s 的载荷里没有字符串字段 %q", e.Type, name)
	}
	return value
}

// 事件序列的形状（spec §5）：一次带一轮工具调用的执行应当产出
// turn/start → user/message → step/start → assistant/message → tool/call → tool/result
// → step/end → turn/end，且 seq 连续、turn/step 编号正确。
func TestOneRoundProducesTheExpectedSequence(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rec := newEventRecorder(store, domain.Task{ID: "t1", SessionID: "s1"})
	ctx := context.Background()

	rec.recordTurnStart(0)
	rec.recordUserMessage("hello")
	rec.recordStepStart()
	rec.recordAssistantMessage("working", []domain.ToolCall{{ID: "c1", Name: "read_file"}}, eventUsage{}, "default")
	rec.recordToolCall(domain.ToolCall{ID: "c1", Name: "read_file"})
	rec.recordToolResult("c1", "ok", false, time.Millisecond)
	rec.recordStepEnd(domain.StepEndReasonCompleted)
	rec.recordTurnEnd(domain.TurnEndReasonCompleted)
	if err := rec.flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	want := []domain.SessionEventType{
		domain.SessionEventTurnStart, domain.SessionEventUserMessage,
		domain.SessionEventStepStart, domain.SessionEventAssistantMessage,
		domain.SessionEventToolCall, domain.SessionEventToolResult,
		domain.SessionEventStepEnd, domain.SessionEventTurnEnd,
	}
	got := store.types()
	if len(got) != len(want) {
		t.Fatalf("事件序列 = %v，want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条是 %s，want %s（完整序列 %v）", i, got[i], want[i], got)
		}
	}
	for i, e := range store.events {
		if e.Seq != int64(i) {
			t.Errorf("第 %d 条的 seq 是 %d：seq 必须连续", i, e.Seq)
		}
	}
}

// step 编号每 turn 内从 0 重置，turn 编号每会话单调（spec §4.1 取值约定）。
func TestStepNumbersResetPerTurn(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rec := newEventRecorder(store, domain.Task{ID: "t1", SessionID: "s1"})

	rec.recordTurnStart(0)
	rec.recordStepStart() // turn 0 step 0
	rec.recordStepEnd(domain.StepEndReasonCompleted)
	rec.recordStepStart() // turn 0 step 1
	rec.recordStepEnd(domain.StepEndReasonCompleted)
	rec.recordTurnEnd(domain.TurnEndReasonCompleted)
	rec.recordTurnStart(1)
	rec.recordStepStart() // turn 1 step 0 —— 必须回到 0
	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var steps []int
	for _, e := range store.events {
		if e.Type == domain.SessionEventStepStart {
			steps = append(steps, intFieldOf(t, e, "step"))
		}
	}
	if len(steps) != 3 || steps[0] != 0 || steps[1] != 1 || steps[2] != 0 {
		t.Errorf("step 编号 = %v，want [0 1 0]：step 每 turn 内重置", steps)
	}
}

// 大载荷不进事件（P1 不变量 6 在发射侧的对应物）：预览按上限截断，
// 事件里不放工具输出全文。
func TestAToolResultCarriesAPreviewNotTheWholeOutput(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rec := newEventRecorder(store, domain.Task{ID: "t1", SessionID: "s1"})
	huge := strings.Repeat("x", maxEventPreviewRunes*3)

	rec.recordToolResult("c1", huge, false, time.Millisecond)
	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	preview := stringFieldOf(t, store.events[0], "preview")
	if utf8.RuneCountInString(preview) > maxEventPreviewRunes {
		t.Errorf("预览有 %d 个 rune，超过上限 %d：事件表会随工具输出体积膨胀",
			utf8.RuneCountInString(preview), maxEventPreviewRunes)
	}
	if strings.Contains(preview, huge) {
		t.Error("预览里塞进了完整输出")
	}
}

// tool_calls 摘要数组故意不截断（见 recordAssistantMessage 的文档注释：截断会破坏
// spec §4.3.1 第 2 条要求的「按 call_id 配对」）。这条测试用证据代替假定：在一个
// 远超真实单步工具调用数量的上界下，事件确实能落盘，不会撞到 P1 的
// 64 KiB/事件硬上限（internal/storage.maxSessionEventDataBytes）。
//
// 上界取 500 的理由：runtime.go 的 executeToolCalls 是顺序 for 循环，不是并发派发，
// 真实模型单步返回的并行工具调用数以个位数、至多两位数计，500 远超这个量级。
// 这里用的是真实的 SQLiteRepository（而不是不做容量校验的 captureEventStore），
// 让 flush 真的经过 P1 的 appendLocked 校验，不是把实现重写一遍去猜一个数字。
func TestRecordAssistantMessageFlushesWithManyToolCalls(t *testing.T) {
	t.Parallel()

	repo, err := storage.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	const callCount = 500
	calls := make([]domain.ToolCall, callCount)
	for i := range calls {
		calls[i] = domain.ToolCall{ID: fmt.Sprintf("call-%04d", i), Name: "read_file"}
	}

	rec := newEventRecorder(repo, domain.Task{ID: "t1", SessionID: "s1"})
	rec.recordAssistantMessage(
		strings.Repeat("x", maxEventPreviewRunes), // content 也顶到截断上限，模拟最坏情况
		calls, eventUsage{Prompt: 1, Completion: 2, Cached: 3, Total: 6}, "default",
	)

	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("flush with %d tool calls: %v", callCount, err)
	}

	events, err := repo.ReadFrom(context.Background(), "s1", 0)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("落盘事件数 = %d，want 1", len(events))
	}

	var payload struct {
		ToolCalls []map[string]any `json:"tool_calls"`
	}
	if err := json.Unmarshal(events[0].Data, &payload); err != nil {
		t.Fatalf("unmarshal 落盘事件: %v", err)
	}
	if len(payload.ToolCalls) != callCount {
		t.Errorf("落盘的 tool_calls 长度 = %d，want %d：说明数组被截断了", len(payload.ToolCalls), callCount)
	}
}

// 三个屏障都 fail-closed（spec §5）：刷不动就不发请求、不进工具体、不开下一步。
//
// 屏障 2 是这条设计的支点：tool/call 必须先落盘，否则崩在工具体里就成了
// 「工具真的执行过、但日志里没有这次调用」——恢复时补不出那条合成结果，
// 而工具是有外部副作用的那一端。
func TestABarrierFailsClosed(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{err: errors.New("disk on fire")}
	rec := newEventRecorder(store, domain.Task{ID: "t1", SessionID: "s1"})
	rec.recordToolCall(domain.ToolCall{ID: "c1", Name: "write_file"})

	err := rec.barrier(context.Background(), "before dispatching a tool")
	if err == nil {
		t.Fatal("落盘失败时屏障放行了：工具会在没有记录的情况下产生副作用")
	}
	if !strings.Contains(err.Error(), "before dispatching a tool") {
		t.Errorf("错误里没说是哪个屏障：%v", err)
	}
	if !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("错误没有包住底层原因：%v", err)
	}
}

// 屏障失败之后事件仍在缓冲里：调用方若重试，不该丢掉已经发生的事实。
func TestAFailedBarrierKeepsTheEventsBuffered(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{err: errors.New("disk on fire")}
	rec := newEventRecorder(store, domain.Task{ID: "t1", SessionID: "s1"})
	rec.recordToolCall(domain.ToolCall{ID: "c1", Name: "write_file"})
	_ = rec.barrier(context.Background(), "before dispatching a tool")

	store.mu.Lock()
	store.err = nil
	store.mu.Unlock()
	if err := rec.barrier(context.Background(), "retry"); err != nil {
		t.Fatalf("重试仍失败：%v", err)
	}
	if got := len(store.events); got != 1 {
		t.Errorf("重试后落盘了 %d 条，want 1：屏障失败时把事件丢了", got)
	}
}

// 没有配 store 的部署里，屏障永远放行——它守的是「记录不上就别做」，
// 而不是「必须有记录才能做」。
func TestABarrierIsANoOpWithoutAStore(t *testing.T) {
	t.Parallel()

	rec := newEventRecorder(nil, domain.Task{ID: "t1"})
	if err := rec.barrier(context.Background(), "before the model request"); err != nil {
		t.Errorf("没有 store 的部署被屏障挡住了：%v", err)
	}
}
