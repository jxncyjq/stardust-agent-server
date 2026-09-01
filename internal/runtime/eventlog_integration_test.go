package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/tool"
)

// boolFieldOf reads a bool field out of a session event's JSON payload,
// failing the test if it is missing or the wrong type.
func boolFieldOf(t *testing.T, e domain.SessionEvent, name string) bool {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(e.Data, &payload); err != nil {
		t.Fatalf("unmarshal %s: %v", e.Type, err)
	}
	value, ok := payload[name].(bool)
	if !ok {
		t.Fatalf("%s 的载荷里没有布尔字段 %q", e.Type, name)
	}
	return value
}

// newTestRuntimeWithEvents builds a Runtime wired to store as its session
// event log, with a fake model that answers one tool call ("read_file") and
// then a final text answer -- the shape TestRunTaskWritesABalancedEventLog
// needs to see a full turn/step/tool sequence.
func newTestRuntimeWithEvents(t *testing.T, store port.SessionEventStore) *Runtime {
	t.Helper()
	maas := &toolCallingMaas{}
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"read_file"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "read_file",
		Description: "read a file",
		InputSchema: map[string]any{
			"required":   []string{"path"},
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
		},
	}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: call.ID, Success: true, Output: "file contents"}, nil
	}))
	return NewRuntime(Config{
		Gate:          taskgate.NewTaskGate(),
		Maas:          maas,
		Audit:         audit,
		Events:        events,
		Tools:         registry,
		SessionEvents: store,
	})
}

// newTestRuntimeWithFailingTool builds a Runtime whose one registered tool
// always fails (returns a Go error from its handler), wired to store as its
// session event log -- the shape TestAFailingToolStillGetsAResultEvent needs.
func newTestRuntimeWithFailingTool(t *testing.T, store port.SessionEventStore) *Runtime {
	t.Helper()
	maas := &toolCallingMaas{}
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"lookup"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "lookup",
		Description: "lookup test data",
		InputSchema: map[string]any{
			"required":   []string{"query"},
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
		},
	}, tool.HandlerFunc(func(_ context.Context, _ domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, errors.New("lookup backend unavailable")
	}))
	return NewRuntime(Config{
		Gate:          taskgate.NewTaskGate(),
		Maas:          maas,
		Audit:         audit,
		Events:        events,
		Tools:         registry,
		SessionEvents: store,
	})
}

// 一次真实的 RunTask 应当在日志里留下完整且平衡的事件序列（spec §9 的 P2 判据）。
//
// 「平衡」的判据不是条数，而是：每个 tool/call 都有同 call_id 的 tool/result，
// 且 turn 以非 interrupted 收尾——interrupted 只由崩溃恢复补出，正常执行绝不该产生。
func TestRunTaskWritesABalancedEventLog(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rt := newTestRuntimeWithEvents(t, store)

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "读一下那个文件"}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	answered := map[string]bool{}
	sawTurnEnd := false
	for _, e := range store.events {
		switch e.Type {
		case domain.SessionEventToolCall:
			id := stringFieldOf(t, e, "call_id")
			if _, seen := answered[id]; !seen {
				answered[id] = false
			}
		case domain.SessionEventToolResult:
			answered[stringFieldOf(t, e, "call_id")] = true
		case domain.SessionEventTurnEnd:
			sawTurnEnd = true
			if reason := stringFieldOf(t, e, "reason"); reason == domain.TurnEndReasonInterrupted {
				t.Errorf("正常执行产出了 interrupted：那是崩溃恢复才该补的记号")
			}
		}
	}
	if len(answered) == 0 {
		t.Fatal("整条日志里没有任何 tool/call：发射点没接上")
	}
	for id, ok := range answered {
		if !ok {
			t.Errorf("call %q 没有对应的 tool/result：spec §4.3.1 第 1 条要求每条调用都有结果", id)
		}
	}
	if !sawTurnEnd {
		t.Error("没有 turn/end：日志不平衡，下次打开会被恢复逻辑当成中断")
	}
}

// 工具失败时**照样**要发 tool/result（is_error 为真）——spec §4.3.1 第 1 条。
// 少发一条，恢复时会把它当成「崩在工具里」补一条合成结果，日志与真实发生的事不符。
func TestAFailingToolStillGetsAResultEvent(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rt := newTestRuntimeWithFailingTool(t, store)

	_, _ = rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "跑那个会失败的工具"})

	var results int
	for _, e := range store.events {
		if e.Type == domain.SessionEventToolResult {
			results++
			if !boolFieldOf(t, e, "is_error") {
				t.Error("失败的工具产出了 is_error:false 的结果")
			}
		}
	}
	if results == 0 {
		t.Fatal("工具失败时一条 tool/result 都没发：恢复会把它当成崩在工具里")
	}
}

// 委派子任务写自己的日志（决定 D-B）：子任务没有 SessionID，recorder 落到
// task.ID 当会话号，于是父日志与子日志互不侵入——父会话只留一次
// tool/call+tool/result（run_sub_task 本身），子会话有自己完整的一套
// turn/step/tool 序列。这条至今没有测试断言过（Task 1 复审留下的跨任务项），
// 归这里补上。
func TestDelegatedSubTaskWritesItsOwnSessionLog(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rt := newTestRuntimeWithEvents(t, store)

	child, err := rt.newSubRuntime(roleLeaf, nil)
	if err != nil {
		t.Fatalf("newSubRuntime: %v", err)
	}
	if _, err := child.RunTask(context.Background(), domain.Agent{ID: "child-agent"},
		domain.Task{ID: "sub-1", Input: "读一下那个文件"}); err != nil {
		t.Fatalf("child.RunTask: %v", err)
	}

	var sawTurnStart, sawToolCall bool
	for _, e := range store.events {
		switch e.Type {
		case domain.SessionEventTurnStart:
			sawTurnStart = true
		case domain.SessionEventToolCall:
			sawToolCall = true
		}
	}
	if !sawTurnStart {
		t.Error("子任务没有写出 turn/start：newSubRuntime 没有把 SessionEvents 传给子 Runtime")
	}
	if !sawToolCall {
		t.Error("子任务没有写出 tool/call：子任务的事件日志是空的")
	}
}

// failAfterNStore succeeds its first n Append calls (so barrier 1's flush of
// turn/start+user/message+step/start+assistant/message goes through cleanly,
// exactly like a real run) and fails every one after that -- isolating
// barrier 2 (the flush of tool/call, right before dispatch) from barrier 1,
// which a permanently-failing store would trip first and so never actually
// exercise barrier 2 at all.
type failAfterNStore struct {
	captureEventStore
	n     int
	calls int
}

func (f *failAfterNStore) Append(ctx context.Context, sessionID string, events []domain.SessionEvent) error {
	f.calls++
	if f.calls > f.n {
		return errors.New("disk on fire")
	}
	return f.captureEventStore.Append(ctx, sessionID, events)
}

// 屏障 2 是 fail-closed 的支点：tool/call 落不了盘，工具体就绝不能被派发。
// 这条比"barrier 返回了 error"更硬——它直接盯着工具体本身有没有被真的调用过：
// 用一个会留痕的假工具，断言在落盘失败时它的痕迹压根不存在。
//
// 用 failAfterNStore 而不是一个永远失败的 store：永远失败的 store 会被屏障 1
// （模型请求前那次 flush）先挡住，屏障 2 根本没机会被走到——那样这条测试其实在
// 验证屏障 1，不是屏障 2。放行第一次 Append（对应屏障 1），只让第二次起失败，
// 才是真的把屏障 2 单独隔离出来验证。
func TestABarrierTwoFailureNeverDispatchesTheTool(t *testing.T) {
	t.Parallel()

	store := &failAfterNStore{n: 1}
	maas := &toolCallingMaas{}
	audit := adapter.NewMemoryAuditLog()
	dispatched := false
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"lookup"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "lookup",
		Description: "lookup test data",
		InputSchema: map[string]any{
			"required":   []string{"query"},
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
		},
	}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		dispatched = true // the trace: did the tool body actually run?
		return domain.ToolResult{CallID: call.ID, Success: true, Output: "should never happen"}, nil
	}))
	rt := NewRuntime(Config{
		Gate:          taskgate.NewTaskGate(),
		Maas:          maas,
		Audit:         audit,
		Events:        adapter.NewMemoryEventBus(),
		Tools:         registry,
		SessionEvents: store,
	})

	_, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "跑一下 lookup"})
	if err == nil {
		t.Fatal("RunTask err = nil, want an error: 屏障 2 落盘失败应该让整次执行失败")
	}
	if dispatched {
		t.Error("屏障 2 落盘失败后，工具体仍然被派发了：tool/call 没先落盘，副作用却先发生了")
	}
}
