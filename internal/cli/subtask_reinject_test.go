package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
)

// fakeMessageStore is an in-memory AgentMessageStore that upserts by id, matching
// the persistence semantics the reinjection job relies on for idempotency.
type fakeMessageStore struct {
	byID map[string]domain.AgentMessage
}

func newFakeMessageStore() *fakeMessageStore {
	return &fakeMessageStore{byID: make(map[string]domain.AgentMessage)}
}

func (s *fakeMessageStore) SaveAgentMessage(_ context.Context, m domain.AgentMessage) error {
	s.byID[m.ID] = m
	return nil
}

func (s *fakeMessageStore) ListAgentMessages(_ context.Context, q domain.AgentMessageQuery) ([]domain.AgentMessage, error) {
	var out []domain.AgentMessage
	for _, m := range s.byID {
		if q.TaskID == "" || m.TaskID == q.TaskID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *fakeMessageStore) MarkAgentMessageRead(context.Context, string, time.Time) error { return nil }

func TestSubtaskReinjectionJobRoutesResultToParent(t *testing.T) {
	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	store := newFakeMessageStore()

	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:      "subtask_completed",
		TaskID:    "task-7:run:sub-1",
		Message:   "子任务摘要：完成",
		CreatedAt: time.Unix(10, 0),
	}); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	// An unrelated event must be ignored.
	if err := events.Publish(ctx, domain.RuntimeEvent{Type: "task_completed", TaskID: "other"}); err != nil {
		t.Fatalf("Publish(other) error = %v, want nil", err)
	}

	job := newSubtaskReinjectionJob(events, store)
	if err := job(ctx); err != nil {
		t.Fatalf("reinjection job error = %v, want nil", err)
	}

	msgs, err := store.ListAgentMessages(ctx, domain.AgentMessageQuery{TaskID: "task-7:run"})
	if err != nil {
		t.Fatalf("ListAgentMessages() error = %v, want nil", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("parent messages = %d, want 1", len(msgs))
	}
	got := msgs[0]
	if got.Type != domain.AgentMessageTypeResult || got.Summary != "子任务摘要：完成" || got.SourceEventID != "task-7:run:sub-1" {
		t.Fatalf("reinjected message = %+v, want result carrying subtask summary", got)
	}

	// Idempotent: a second run does not duplicate the message.
	if err := job(ctx); err != nil {
		t.Fatalf("second reinjection job error = %v, want nil", err)
	}
	if len(store.byID) != 1 {
		t.Fatalf("stored messages = %d, want 1 (idempotent)", len(store.byID))
	}
}

// TestReinjectUsesTheEventsParentTaskID：新事件带着 ParentTaskID，回注按它投递。
//
// 这也是下面那条拒绝用例的阳性对照之一：它证明这个 job 在正常事件上确实会投递，
// 于是「无法定位就不投递」不是因为整个 job 根本什么都没做而恒真。
func TestReinjectUsesTheEventsParentTaskID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:         "subtask_completed",
		TaskID:       "8f14e45f-ceea-467a-9f1b-8d0f0f0f0f0f",
		ParentTaskID: "task-parent",
		Message:      "查清楚了",
		CreatedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("Publish error = %v, want nil", err)
	}
	store := newFakeMessageStore()
	if err := newSubtaskReinjectionJob(events, store)(ctx); err != nil {
		t.Fatalf("job error = %v, want nil", err)
	}
	msgs, err := store.ListAgentMessages(ctx, domain.AgentMessageQuery{})
	if err != nil {
		t.Fatalf("ListAgentMessages error = %v, want nil", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("回注了 %d 条, want 1", len(msgs))
	}
	if msgs[0].TaskID != "task-parent" || msgs[0].ThreadID != "task-parent" {
		t.Errorf("投递给了 %q/%q, want task-parent/task-parent", msgs[0].TaskID, msgs[0].ThreadID)
	}
}

// TestReinjectFallsBackToTheLegacyIDShape：老事件没有这个字段。
//
// runtime_events 是落盘的，真实库里存着改造之前发布的事件。按老形态 id 解析它们是
// 显式的老数据兼容，不是兜底——新事件一律走字段。
//
// 与上一条一起充当拒绝用例的阳性对照。
func TestReinjectFallsBackToTheLegacyIDShape(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:      "subtask_completed",
		TaskID:    "task-parent:sub-3",
		Message:   "查清楚了",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Publish error = %v, want nil", err)
	}
	store := newFakeMessageStore()
	if err := newSubtaskReinjectionJob(events, store)(ctx); err != nil {
		t.Fatalf("job error = %v, want nil", err)
	}
	msgs, err := store.ListAgentMessages(ctx, domain.AgentMessageQuery{})
	if err != nil {
		t.Fatalf("ListAgentMessages error = %v, want nil", err)
	}
	if len(msgs) != 1 || msgs[0].TaskID != "task-parent" {
		t.Fatalf("老事件没有按老形态解析出父任务：%+v", msgs)
	}
}

// TestReinjectRefusesAnEventItCannotAddress：两条路都不成立时报错。
func TestReinjectRefusesAnEventItCannotAddress(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:      "subtask_completed",
		TaskID:    "8f14e45f-ceea-467a-9f1b-8d0f0f0f0f0f", // UUID，且没有 ParentTaskID
		Message:   "查清楚了",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Publish error = %v, want nil", err)
	}
	store := newFakeMessageStore()
	err := newSubtaskReinjectionJob(events, store)(ctx)
	if err == nil {
		t.Fatal("一条无法定位父任务的事件被静默跳过了；父任务会永远等下去")
	}
	if !strings.Contains(err.Error(), "8f14e45f-ceea-467a-9f1b-8d0f0f0f0f0f") {
		t.Errorf("error = %q, want it to name the offending task id", err.Error())
	}
	if len(store.byID) != 0 {
		t.Error("无法定位父任务却还是投递了")
	}
}

// TestReinjectKeepsGoingPastAnUnaddressableEvent 钉死「报告」与「停摆」的区别。
//
// runtime_events 是落盘的：一条定位不了的事件会在每一次调度都被重新读到。若这个 job
// 在它身上直接 return，那么排在它后面的、完全正常的事件就再也轮不到——一条坏事件把
// 整条回注通道永久掐断。所以策略是：跳过它、记下它、把整轮攒下的错误在最后一并报出
// 去，同时该投递的照样投递。
func TestReinjectKeepsGoingPastAnUnaddressableEvent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	// 顺序是刻意的：坏事件排在前面，好事件排在它后面。
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:      "subtask_completed",
		TaskID:    "8f14e45f-ceea-467a-9f1b-8d0f0f0f0f0f",
		Message:   "定位不了的那条",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Publish error = %v, want nil", err)
	}
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:         "subtask_completed",
		TaskID:       "0a9f6d5c-1111-2222-3333-444455556666",
		ParentTaskID: "task-parent-after",
		Message:      "排在坏事件后面的正常结果",
		CreatedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("Publish error = %v, want nil", err)
	}
	store := newFakeMessageStore()
	err := newSubtaskReinjectionJob(events, store)(ctx)
	if err == nil {
		t.Fatal("定位不了的事件被静默吞掉了：job error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "8f14e45f-ceea-467a-9f1b-8d0f0f0f0f0f") {
		t.Errorf("error = %q, want it to name the offending task id", err.Error())
	}
	msgs, err := store.ListAgentMessages(ctx, domain.AgentMessageQuery{TaskID: "task-parent-after"})
	if err != nil {
		t.Fatalf("ListAgentMessages error = %v, want nil", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("排在坏事件后面的正常结果被回注了 %d 条, want 1："+
			"一条定位不了的事件把整条回注通道掐断了", len(msgs))
	}
}
