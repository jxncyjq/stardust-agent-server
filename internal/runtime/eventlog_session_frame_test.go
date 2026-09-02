package runtime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/eventbridge"
	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// notifiedBatch 是通知口收到的一次调用，供断言「什么时候通知的、通知了什么」。
type notifiedBatch struct {
	session string
	events  []domain.SessionEvent
}

// recordingNotifier 记下每一次通知。
type recordingNotifier struct {
	mu      sync.Mutex
	batches []notifiedBatch
}

func (n *recordingNotifier) notify(_ context.Context, session string, events []domain.SessionEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.batches = append(n.batches, notifiedBatch{
		session: session,
		events:  append([]domain.SessionEvent(nil), events...),
	})
}

func (n *recordingNotifier) snapshot() []notifiedBatch {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notifiedBatch(nil), n.batches...)
}

// flush 落盘之后必须逐条通知，且带的是**这一批事件最终的 seq**——前端拿 seq 判断
// 自己漏没漏帧，seq 错了它会以为自己没漏，比根本没收到还糟。
//
// seq 只在 flush 里才定得下来（P1 的 store 校验 seq 而不分配 seq，游标要到第一次
// flush 才与库对齐），所以这条断言同时钉死了发布点必须在 flush 之内、在 Append
// 之后：任何更早的发布点都只能猜 seq。
func TestFlushNotifiesWithTheSeqItActuallyPersisted(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	notifier := &recordingNotifier{}
	rec := newEventRecorder(store, domain.Task{ID: "t1", SessionID: "s1"}, notifier.notify)

	rec.recordTurnStart(0)
	rec.recordUserMessage("读一下那个文件")
	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// 第二批：seq 必须从第一批之后接着走，而不是每批都从 0 开始。
	rec.recordStepStart()
	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("second flush: %v", err)
	}

	batches := notifier.snapshot()
	if len(batches) != 2 {
		t.Fatalf("通知了 %d 批，想要 2 批（每次成功落盘一批）", len(batches))
	}

	var notified []domain.SessionEvent
	for _, b := range batches {
		if b.session != "s1" {
			t.Errorf("通知带的会话号 = %q，想要 %q", b.session, "s1")
		}
		notified = append(notified, b.events...)
	}

	persisted := store.eventsFor("s1")
	if len(notified) != len(persisted) {
		t.Fatalf("通知了 %d 条、落盘了 %d 条：每条落盘的事件都要有它的通知",
			len(notified), len(persisted))
	}
	for i, want := range persisted {
		got := notified[i]
		if got.Seq != want.Seq || got.Type != want.Type {
			t.Errorf("第 %d 条通知 = (seq %d, %s)，落盘的是 (seq %d, %s)",
				i, got.Seq, got.Type, want.Seq, want.Type)
		}
	}
	// 显式钉一次 seq 的绝对取值：上面的逐条比对在「两侧一起错」时仍然会绿。
	if len(persisted) != 3 || persisted[0].Seq != 0 || persisted[2].Seq != 2 {
		t.Fatalf("落盘的 seq 不是 0,1,2：%+v", persisted)
	}
}

// 落盘失败时**一条通知都不能发**：宣告一个库里并不存在的 seq，前端会去补拉一段
// 永远拉不到的区间，而且会把这个不存在的 seq 当成「我没漏」的证据。
func TestNothingIsNotifiedWhenTheAppendFails(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{err: errors.New("disk on fire")}
	notifier := &recordingNotifier{}
	rec := newEventRecorder(store, domain.Task{ID: "t1", SessionID: "s1"}, notifier.notify)

	rec.recordTurnStart(0)
	if err := rec.flush(context.Background()); err == nil {
		t.Fatal("flush 在 Append 失败时返回了 nil：屏障是 fail-closed 的")
	}
	if batches := notifier.snapshot(); len(batches) != 0 {
		t.Fatalf("落盘失败却发了 %d 批通知：%+v", len(batches), batches)
	}
}

// 这是**接线守卫**：一次真实的 RunTask 必须让每一条写进日志的事件在平台事件总线上
// 留下一帧 session_event，帧里带得住 session_id / seq / event_type。
//
// 它走的是完整链路（eventRecorder.flush → Runtime 的发布闭包 → port.EventBus →
// eventbridge.translate → observability.EventBus），因为本仓栽过两次「接缝在、但没有
// 任何人调用它」：只测 flush 会通知、只测 translate 会翻译，两条都绿，而中间的装配
// 一行没接上时整条链路是死的。
func TestRunTaskPublishesASessionEventFrameForEveryEventItLogs(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	platform := observability.NewEventBus(512)
	rt := newTestRuntimeWithEventsAndBus(t, store, eventbridge.New(platform, nil), nil)

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "读一下那个文件"}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	frames := sessionEventFrames(t, platform)
	persisted := store.eventsFor("s1")
	if len(persisted) == 0 {
		t.Fatal("这次执行一条事件都没落盘，这条测试就什么也没证明")
	}
	if len(frames) != len(persisted) {
		t.Fatalf("落盘 %d 条事件、SSE 上只有 %d 帧 session_event：帧与事件必须一一对应\n帧=%+v",
			len(persisted), len(frames), frames)
	}
	for i, want := range persisted {
		got := frames[i]
		if got["session_id"] != "s1" {
			t.Errorf("第 %d 帧的 session_id = %v，想要 %q", i, got["session_id"], "s1")
		}
		// 这里读到的是**进程内**的信封，值还是 Go 值（int64），不是 JSON 解出来的
		// float64——JSON 化发生在 SSE 写出那一步（internal/server 的用例覆盖它）。
		if seq, ok := got["seq"].(int64); !ok || seq != want.Seq {
			t.Errorf("第 %d 帧的 seq = %v，落盘的是 %d（前端靠它判断漏帧）", i, got["seq"], want.Seq)
		}
		if got["event_type"] != string(want.Type) {
			t.Errorf("第 %d 帧的 event_type = %v，落盘的是 %s", i, got["event_type"], want.Type)
		}
	}

	// 帧只做通知：事件正文不进帧，否则一条大事件会把这条还载着别的帧的流撑爆。
	for i, frame := range frames {
		for _, forbidden := range []string{"content", "data", "arguments", "preview", "tool_calls"} {
			if _, present := frame[forbidden]; present {
				t.Errorf("第 %d 帧带了 %q：帧只做通知，正文回 /v1/sessions/{id}/events 拉", i, forbidden)
			}
		}
	}
}

// 通知发不出去不能连累任务：事件此刻已经落盘了，把一次 SSE 通知的失败反向变成
// fail-closed 屏障的失败，等于因为通知渠道抖动而中断一个数据完好的任务。
//
// 但也不能静默：必须留下一条带 session 与 seq 的 Warn，否则就是吞错。
func TestAFailedSessionEventPublishNeitherFailsTheTaskNorGoesUnrecorded(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	var logs bytes.Buffer
	bus := &sessionEventFailingBus{}
	rt := newTestRuntimeWithEventsAndBus(t, store, bus,
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "读一下那个文件"}); err != nil {
		t.Fatalf("RunTask 因为一次通知发布失败而失败了：%v", err)
	}

	persisted := store.eventsFor("s1")
	if len(persisted) == 0 {
		t.Fatal("事件没有落盘：这条测试要证明的是「落盘成功、通知失败」")
	}
	if bus.rejected() == 0 {
		t.Fatal("这次执行一次 session_event 发布都没尝试过：夹具没被走到")
	}
	logged := logs.String()
	if !strings.Contains(logged, "s1") || !strings.Contains(logged, "seq") {
		t.Fatalf("发布失败没有被带着 session/seq 记下来（静默吞错）：\n%s", logged)
	}
}

// sessionEventFailingBus 只让 session_event 的发布失败，其余照收。
//
// 让所有发布都失败是不行的：runtime 里 tool_result / tool_executed 两处发布**会**把
// 错误往上传，任务会因为与本测试无关的原因失败，于是这条测试测不到它想测的东西。
type sessionEventFailingBus struct {
	mu     sync.Mutex
	events []domain.RuntimeEvent
	denied int
}

func (b *sessionEventFailingBus) Publish(ctx context.Context, event domain.RuntimeEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if event.Type == domain.RuntimeEventSessionEvent {
		b.denied++
		return errors.New("platform bus unavailable")
	}
	b.events = append(b.events, event)
	return nil
}

func (b *sessionEventFailingBus) Events() ([]domain.RuntimeEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]domain.RuntimeEvent(nil), b.events...), nil
}

func (b *sessionEventFailingBus) rejected() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.denied
}

// sessionEventFrames 订阅 bus 并取回它保留的全部 session_event 帧的 Data，按发布
// 顺序。observability.EventBus 会把保留的历史回放给新订阅者，所以在 RunTask 之后
// 订阅即可，不必与它抢时序。
func sessionEventFrames(t *testing.T, bus *observability.EventBus) []map[string]any {
	t.Helper()
	envelopes, cancel := bus.Subscribe(context.Background())
	defer cancel()

	var frames []map[string]any
	for {
		select {
		case envelope, ok := <-envelopes:
			if !ok {
				return frames
			}
			if envelope.Type == domain.RuntimeEventSessionEvent {
				frames = append(frames, envelope.Data)
			}
		default:
			return frames
		}
	}
}

// newTestRuntimeWithEventsAndBus 是 newTestRuntimeWithEvents 的变体，让调用方自带
// 事件总线与 logger——本文件要断言的正是总线上出现了什么、以及发布失败被记进了哪里。
// logger 传 nil 时走 NewRuntime 自己的默认值。
func newTestRuntimeWithEventsAndBus(t *testing.T, store *captureEventStore, bus port.EventBus, logger *slog.Logger) *Runtime {
	t.Helper()
	audit := adapter.NewMemoryAuditLog()
	return NewRuntime(Config{
		Gate:          taskgate.NewTaskGate(),
		Maas:          readFileThenAnswerMaas(),
		Audit:         audit,
		MaxToolRounds: 3,
		Events:        bus,
		Tools:         readFileRegistry(audit),
		SessionEvents: store,
		Logger:        logger,
	})
}
