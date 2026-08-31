package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stardust/legion-agent/internal/domain"
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
type captureEventStore struct {
	mu     sync.Mutex
	events []domain.SessionEvent
	err    error // 非 nil 时 Append 失败，供屏障测试用
}

func (c *captureEventStore) Append(_ context.Context, _ string, events []domain.SessionEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.events = append(c.events, events...)
	return nil
}

func (c *captureEventStore) ReadFrom(context.Context, string, int64) ([]domain.SessionEvent, error) {
	return nil, nil
}
func (c *captureEventStore) Load(context.Context, string) ([]domain.SessionEvent, error) {
	return nil, nil
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
