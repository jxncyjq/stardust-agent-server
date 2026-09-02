package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/testsupport"
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

// stepKeyOf names the step an event belongs to ("turn N step M"), for pairing
// step/start with step/end. Only events that carry a step may be passed.
func stepKeyOf(t *testing.T, e domain.SessionEvent) string {
	t.Helper()
	return fmt.Sprintf("turn %d step %d", intFieldOf(t, e, "turn"), intFieldOf(t, e, "step"))
}

// toolThenAnswerMaas asks for calls while the exchange it is shown does not yet
// contain marker, and answers in plain text once it does (or once no tools are
// offered at all, which is the closing generateNoTools request).
//
// It decides by the CONTENT of the request, never by how many times it has been
// called. That matters twice over. One instance serves several runs — a child
// runtime built by newSubRuntime shares its parent's inference client — and a
// call-counting fake would answer the child's first request as if it were the
// parent's third. And a fake whose answer does not depend on what it was asked
// is how a fixture ends up registering one tool while its model asks for
// another without anyone noticing: that mismatch is what left the successful
// tool/result emission point with zero coverage.
type toolThenAnswerMaas struct {
	calls  []domain.ToolCall
	marker string
	answer string

	mu   sync.Mutex
	last string
}

func (m *toolThenAnswerMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	text := testsupport.RequestText(req)
	m.mu.Lock()
	m.last = text
	m.mu.Unlock()
	if len(req.Tools) == 0 || strings.Contains(text, m.marker) {
		return port.InferenceResponse{Text: m.answer}, nil
	}
	return port.InferenceResponse{ToolCalls: append([]domain.ToolCall(nil), m.calls...)}, nil
}

// lastRequest is the exchange this fake was shown most recently — how a test
// asserts what the model actually got to see (e.g. that both of two same-id
// calls were answered back to it).
func (m *toolThenAnswerMaas) lastRequest() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

// readFileRegistry builds a registry whose one tool, read_file, succeeds and
// echoes the path it was given, so two calls with different arguments produce
// distinguishable output.
func readFileRegistry(audit *adapter.MemoryAuditLog) *tool.Registry {
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
		return domain.ToolResult{
			CallID:  call.ID,
			Success: true,
			Output:  "file contents of " + call.Arguments["path"],
		}, nil
	}))
	return registry
}

// newTestRuntimeWithEvents builds a Runtime wired to store as its session event
// log, with a fake model that asks for one read_file call and then answers.
//
// The registered tool and the tool the model asks for are the SAME tool on
// purpose: when they differ every run takes the ErrToolNotFound branch, the
// registered handler is dead code, and the successful-dispatch emission point
// is never reached by any test at all.
func newTestRuntimeWithEvents(t *testing.T, store port.SessionEventStore) *Runtime {
	t.Helper()
	audit := adapter.NewMemoryAuditLog()
	return NewRuntime(Config{
		Gate:  taskgate.NewTaskGate(),
		Maas:  readFileThenAnswerMaas(),
		Audit: audit,
		// A literal round budget, so a fake that somehow never satisfies its own
		// marker still terminates.
		MaxToolRounds: 3,
		Events:        adapter.NewMemoryEventBus(),
		Tools:         readFileRegistry(audit),
		SessionEvents: store,
	})
}

func readFileThenAnswerMaas() *toolThenAnswerMaas {
	return &toolThenAnswerMaas{
		calls:  []domain.ToolCall{{ID: "call-1", Name: "read_file", Arguments: map[string]string{"path": "notes.md"}}},
		marker: "file contents of",
		answer: "读完了：那个文件讲的是缓存",
	}
}

// newTestRuntimeWithFailingTool builds a Runtime whose one registered tool
// always fails (returns a Go error from its handler), wired to store as its
// session event log -- the shape TestAFailingToolStillGetsAResultEvent needs.
func newTestRuntimeWithFailingTool(t *testing.T, store port.SessionEventStore) *Runtime {
	t.Helper()
	audit := adapter.NewMemoryAuditLog()
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
		Gate: taskgate.NewTaskGate(),
		Maas: &toolThenAnswerMaas{
			calls:  []domain.ToolCall{{ID: "lookup-1", Name: "lookup", Arguments: map[string]string{"query": "cache"}}},
			marker: "lookup backend unavailable",
			answer: "查不到，先这样回答",
		},
		Audit:         audit,
		MaxToolRounds: 3,
		Events:        adapter.NewMemoryEventBus(),
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
	// 成功工具那条发射点必须真的被走到。只断言「每条 call 都有 result」是不够的：
	// 工具不存在时走的是派发错误分支，两条断言一样绿，而成功分支一行没跑——删掉它
	// 整包仍然全绿。这条断言把两个分支区分开。
	sawSuccessfulResult := false
	for _, e := range store.events {
		switch e.Type {
		case domain.SessionEventToolCall:
			id := stringFieldOf(t, e, "call_id")
			if _, seen := answered[id]; !seen {
				answered[id] = false
			}
		case domain.SessionEventToolResult:
			answered[stringFieldOf(t, e, "call_id")] = true
			if !boolFieldOf(t, e, "is_error") {
				if preview := stringFieldOf(t, e, "preview"); preview != "" {
					sawSuccessfulResult = true
				}
			}
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
	if !sawSuccessfulResult {
		t.Error("没有一条 is_error:false 且 preview 非空的 tool/result：" +
			"这次执行根本没走成功工具那条发射点（多半是夹具注册的工具与假模型请求的工具对不上）")
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

// 委派子任务写自己的日志（决定 D-B）：runChild 构造的子任务没有 SessionID，
// recorder 落到 task.ID 当会话号，于是父日志与子日志互不侵入。
//
// 这条测试走的是**真正的委派入口** RunSubTask → runChild，而不是自己拼一个
// domain.Task 交给子 Runtime：D-B 这个决定就写在 runChild 的那个结构体字面量里，
// 测试自己造任务等于把被测的决定重写了一遍，往 runChild 里塞一个父会话号也不会红。
//
// 断言也不能只看「有事件」：夹具必须记住事件写进了哪条会话，否则「子任务写了自己的
// 日志」和「子任务写进了父亲的日志」这两件事在断言里长得一模一样。
func TestDelegatedSubTaskWritesItsOwnSessionLog(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rt := newTestRuntimeWithEvents(t, store)

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "读一下那个文件"}); err != nil {
		t.Fatalf("父任务 RunTask: %v", err)
	}
	parentBefore := len(store.eventsFor("s1"))

	res, err := rt.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "读一下那个文件",
	})
	if err != nil {
		t.Fatalf("RunSubTask: %v", err)
	}

	child := store.eventsFor(res.TaskID)
	if len(child) == 0 {
		t.Fatalf("子任务 %q 名下一条事件都没有：要么 newSubRuntime 没把 SessionEvents 传下去，"+
			"要么子任务的事件写进了别人的日志", res.TaskID)
	}
	var sawTurnStart, sawToolCall bool
	for _, e := range child {
		switch e.Type {
		case domain.SessionEventTurnStart:
			sawTurnStart = true
		case domain.SessionEventToolCall:
			sawToolCall = true
		}
	}
	if !sawTurnStart {
		t.Error("子任务的日志里没有 turn/start")
	}
	if !sawToolCall {
		t.Error("子任务的日志里没有 tool/call：子任务的事件日志是空的")
	}
	if got := len(store.eventsFor("s1")); got != parentBefore {
		t.Errorf("父会话日志从 %d 条涨到 %d 条：子任务的事件漏进了父亲的日志，"+
			"父日志本该只留 RunSubTask 自己那一对 tool/call+tool/result", parentBefore, got)
	}
}

// spec §4.1：turn 号在一条会话里单调递增。第二次 RunTask 必须从库里已有事件的
// 最大 turn + 1 开起，否则两次执行的事件会挤在同一个 turn 号下，投影分不开它们。
func TestTurnNumbersAreMonotonicAcrossRunsOfOneSession(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rt := newTestRuntimeWithEvents(t, store)

	for i := 0; i < 2; i++ {
		if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
			domain.Task{ID: fmt.Sprintf("t%d", i), SessionID: "s1", Input: "读一下那个文件"}); err != nil {
			t.Fatalf("第 %d 次 RunTask: %v", i, err)
		}
	}

	var turns []int
	for _, e := range store.eventsFor("s1") {
		if e.Type == domain.SessionEventTurnStart {
			turns = append(turns, intFieldOf(t, e, "turn"))
		}
	}
	if len(turns) != 2 || turns[0] != 0 || turns[1] != 1 {
		t.Errorf("两次执行的 turn/start 编号 = %v，want [0 1]：turn 号必须跨执行单调", turns)
	}
}

// spec §4.3.1 第 4 条：同一 step 内未应答的 tool/call 不得复用 call_id。
//
// 这不是假想：provider 不给 tool call id 时，适配层把 id 退化成函数名
// （adapter.openAIToolCalls），于是一次并行请求两次 read_file 就带着两个一模一样的
// "read_file" 进来。两条 tool/call 同 id 且第一条还没被应答，投影就没法按 call_id
// 把结果配回去。消歧后的 id 必须同时用于 tool/call、tool/result 和回灌给模型的结果，
// 三处一致——所以这条测试既看事件，也看模型最终看到的那份对话。
func TestParallelCallsSharingAProviderIDGetDistinctCallIDs(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	audit := adapter.NewMemoryAuditLog()
	maas := &toolThenAnswerMaas{
		// 同一个 id 两次：provider 没给 id 时的真实形状。
		calls: []domain.ToolCall{
			{ID: "read_file", Name: "read_file", Arguments: map[string]string{"path": "a.md"}},
			{ID: "read_file", Name: "read_file", Arguments: map[string]string{"path": "b.md"}},
		},
		marker: "file contents of",
		answer: "两个文件都读完了",
	}
	rt := NewRuntime(Config{
		Gate:          taskgate.NewTaskGate(),
		Maas:          maas,
		Audit:         audit,
		MaxToolRounds: 3,
		Events:        adapter.NewMemoryEventBus(),
		Tools:         readFileRegistry(audit),
		SessionEvents: store,
	})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "读一下这两个文件"}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	// 同一 step 内的 tool/call：id 必须互不相同，且各自有一条同 id 的 tool/result。
	openCalls := map[string]bool{} // call_id -> 是否已应答
	for _, e := range store.eventsFor("s1") {
		switch e.Type {
		case domain.SessionEventToolCall:
			id := stringFieldOf(t, e, "call_id")
			if answered, seen := openCalls[id]; seen && !answered {
				t.Errorf("call_id %q 在 step %d 里被复用，且上一条还没有结果：违反 spec §4.3.1 第 4 条",
					id, intFieldOf(t, e, "step"))
			}
			openCalls[id] = false
		case domain.SessionEventToolResult:
			openCalls[stringFieldOf(t, e, "call_id")] = true
		}
	}
	if len(openCalls) != 2 {
		t.Fatalf("记录到 %d 个不同的 call_id，want 2：两条并行调用被当成同一条了", len(openCalls))
	}
	for id, answered := range openCalls {
		if !answered {
			t.Errorf("call %q 没有对应的 tool/result", id)
		}
	}

	// 三处一致的另一半：模型最终看到的对话里，两条结果都在。回灌用的 CallID 若与
	// 记录用的 id 对不上，appendToolResults 会把两条调用配到同一个结果上，模型只看得到一份。
	last := maas.lastRequest()
	if !strings.Contains(last, "file contents of a.md") || !strings.Contains(last, "file contents of b.md") {
		t.Errorf("模型看到的对话里少了某条工具结果：两条调用没有各自配到自己的结果\n%s", last)
	}
}

// 每条 step/end 都必须对得上一条同 turn/step 的 step/start（spec §5 的事件表），
// 反过来也一样：一次执行不该留下开着的 step。
//
// 盯的是 loopCut/capHit 这两条中断路径——重复调用守卫与 per-tool 上限，2026-07-23
// 那次 152 轮事故之后专门加的两道闸，生产上真的会走到。循环体在 break 之前已经把
// 这一步关掉了，退出后的「预算耗尽」分支若不看有没有开着的 step 就再关一次，
// 就会写出一条没有 start 的 end，还顺手偷走下一个 step 号。
func TestEveryStepEndPairsWithAStepStart(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	audit := adapter.NewMemoryAuditLog()
	rt := NewRuntime(Config{
		Gate: taskgate.NewTaskGate(),
		// marker 永远不出现 = 模型每一轮都请求同一条调用，直到重复守卫喊停
		// （repeatAbortCount = 6）。MaxToolRounds 取 8：比 6 大，让重复守卫先于
		// 轮次预算触发，同时仍是一个字面上界，测试不可能不终止。
		Maas: &toolThenAnswerMaas{
			calls:  []domain.ToolCall{{ID: "call-1", Name: "read_file", Arguments: map[string]string{"path": "notes.md"}}},
			marker: "\x00 这个标记永远不会出现在对话里",
			answer: "只好直接回答了",
		},
		Audit:         audit,
		MaxToolRounds: 8,
		Events:        adapter.NewMemoryEventBus(),
		Tools:         readFileRegistry(audit),
		SessionEvents: store,
	})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "反复读同一个文件"}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	starts := map[string]int{}
	ends := map[string]int{}
	for _, e := range store.eventsFor("s1") {
		switch e.Type {
		case domain.SessionEventStepStart:
			starts[stepKeyOf(t, e)]++
		case domain.SessionEventStepEnd:
			key := stepKeyOf(t, e)
			ends[key]++
			if starts[key] == 0 {
				t.Errorf("%s 有 step/end 却没有 step/start：日志结构失衡", key)
			}
		}
	}
	for key, n := range starts {
		if ends[key] != n {
			t.Errorf("%s 有 %d 条 step/start、%d 条 step/end：每步必须恰好关一次", key, n, ends[key])
		}
	}
	// 守卫真的触发过：重复守卫在第 6 轮喊停，加上收尾那一步 = 至少 7 步。少于这个数
	// 说明执行早早结束了，上面的断言其实没有走过中断路径。
	if len(starts) < 7 {
		t.Errorf("只开了 %d 步：重复调用守卫没有被触发，这条测试没有走到它要盯的中断路径", len(starts))
	}
}

// 挂起也要把 turn 收尾（spec §4.1 的四个 reason 里，「停在半路等人决定」只有
// cancelled 说得通）。
//
// 挂起是正常暂停，不是崩溃：恢复时开的是**新的** turn（newTaskRecorder 从库里已有
// 事件解出 max+1），没有任何人会回头关掉挂起时那个 turn。留着不关，下次 Load 就会
// 把它补成 interrupted——那个记号是留给「进程真的崩在轮次中间」的，而 plan 交给 P3
// 的承诺正是「正常路径永不产出 interrupted」。
func TestASuspendedRunClosesItsTurn(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rt := NewRuntime(Config{
		Gate:          taskgate.NewTaskGate(),
		Maas:          &scriptedMaas{},
		Audit:         adapter.NewMemoryAuditLog(),
		Events:        adapter.NewMemoryEventBus(),
		Tools:         echoRegistry(t),
		Checkpoints:   sessionstate.NewStore(t.TempDir()),
		ToolGate:      &gateOnce{},
		SessionEvents: store,
	})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "agent-1"},
		domain.Task{ID: "task-1", SessionID: "sess-1", AgentID: "agent-1", Input: "go"}); !errors.Is(err, ErrSuspended) {
		t.Fatalf("RunTask err = %v, want ErrSuspended", err)
	}

	starts, ends := 0, 0
	var turnEndReason string
	for _, e := range store.eventsFor("sess-1") {
		switch e.Type {
		case domain.SessionEventStepStart:
			starts++
		case domain.SessionEventStepEnd:
			ends++
		case domain.SessionEventTurnEnd:
			turnEndReason = stringFieldOf(t, e, "reason")
		}
	}
	if starts != ends {
		t.Errorf("挂起时开了 %d 步、关了 %d 步：留下开着的 step", starts, ends)
	}
	if turnEndReason == "" {
		t.Fatal("挂起的 turn 没有 turn/end：它永远不会被别人关掉，下次 Load 会把它补成 interrupted")
	}
	if turnEndReason != domain.TurnEndReasonCancelled {
		t.Errorf("挂起的 turn/end reason = %q，want %q：它既没做完也没失败，是停在半路等人决定",
			turnEndReason, domain.TurnEndReasonCancelled)
	}
}

// failOnTypeBus is a runtime event bus that fails to publish one event type and
// forwards the rest -- the way to reach an error return that only a publish
// failure can produce.
type failOnTypeBus struct {
	port.EventBus
	failType string
}

func (b *failOnTypeBus) Publish(ctx context.Context, event domain.RuntimeEvent) error {
	if event.Type == b.failType {
		return errors.New("event bus down")
	}
	return b.EventBus.Publish(ctx, event)
}

// 每一条错误出口都要关掉自己开的那个 turn。
//
// 一个开着的 turn 不是「少一条事件」而已：下一次 Load 会把它补成 interrupted，
// 而 interrupted 是留给「进程真的崩在轮次中间」的记号。plan 交给 P3 的承诺是
// 「正常路径永不产出 interrupted」——一条忘了收尾的错误出口就足以让它变成谎话。
//
// 盯的是重复守卫那条出口：它发 tool_loop_broken 事件，发不出去就直接 return，
// 是这个函数里最容易被漏掉的两条之一。
func TestAPublishFailureStillClosesTheTurn(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	audit := adapter.NewMemoryAuditLog()
	rt := NewRuntime(Config{
		Gate: taskgate.NewTaskGate(),
		Maas: &toolThenAnswerMaas{
			calls:  []domain.ToolCall{{ID: "call-1", Name: "read_file", Arguments: map[string]string{"path": "notes.md"}}},
			marker: "\x00 这个标记永远不会出现在对话里",
			answer: "只好直接回答了",
		},
		Audit:         audit,
		MaxToolRounds: 8,
		Events:        &failOnTypeBus{EventBus: adapter.NewMemoryEventBus(), failType: "tool_loop_broken"},
		Tools:         readFileRegistry(audit),
		SessionEvents: store,
	})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "反复读同一个文件"}); err == nil {
		t.Fatal("RunTask err = nil, want an error：tool_loop_broken 事件发不出去应该让执行失败")
	}

	var sawTurnEnd bool
	for _, e := range store.eventsFor("s1") {
		if e.Type == domain.SessionEventTurnEnd {
			sawTurnEnd = true
			if reason := stringFieldOf(t, e, "reason"); reason != domain.TurnEndReasonFailed {
				t.Errorf("turn/end reason = %q，want %q", reason, domain.TurnEndReasonFailed)
			}
		}
	}
	if !sawTurnEnd {
		t.Error("这条错误出口没有写 turn/end：留下一个开着的 turn，" +
			"下次 Load 会把它补成 interrupted，而这次执行根本没崩过")
	}
}

// failAfterNStore succeeds its first n Append calls (so barrier 1's flush of
// turn/start+user/message+step/start goes through cleanly, exactly like a real
// run) and fails every one after that -- isolating barrier 2 (the flush of
// tool/call, right before dispatch) from barrier 1, which a permanently-failing
// store would trip first and so never actually exercise barrier 2 at all.
type failAfterNStore struct {
	captureEventStore
	n int

	mu    sync.Mutex
	calls int
}

func (f *failAfterNStore) Append(ctx context.Context, sessionID string, events []domain.SessionEvent) error {
	f.mu.Lock()
	f.calls++
	failing := f.calls > f.n
	f.mu.Unlock()
	if failing {
		return errors.New("disk on fire")
	}
	return f.captureEventStore.Append(ctx, sessionID, events)
}

// failOnceAtStore fails its at-th Append and succeeds every other one: a
// TRANSIENT failure (a disk that filled up and was freed again, a lock that
// timed out once), which is the case that decides whether a barrier failure can
// leave a lie in the log.
type failOnceAtStore struct {
	captureEventStore
	at int

	mu    sync.Mutex
	calls int
}

func (f *failOnceAtStore) Append(ctx context.Context, sessionID string, events []domain.SessionEvent) error {
	f.mu.Lock()
	f.calls++
	failing := f.calls == f.at
	f.mu.Unlock()
	if failing {
		return errors.New("disk on fire (transient)")
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
	maas := &toolThenAnswerMaas{
		calls:  []domain.ToolCall{{ID: "lookup-1", Name: "lookup", Arguments: map[string]string{"query": "cache"}}},
		marker: "never mind",
		answer: "never mind",
	}
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
		MaxToolRounds: 3,
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

// 屏障 2 失败后，那条 tool/call 绝不能被后来的 flush 补写进日志。
//
// 屏障失败 = 这次调用没有被派发，工具体一次也没跑。可 flush 在 Append 失败时按设计
// 保留缓冲（让重试不丢事件），而这次执行在收尾时还会再 flush 一次；只要第一次失败
// 是瞬时的，那条从未发生的调用就会被写下去，成为永远等不到 tool/result 的孤儿，
// 恢复逻辑还会为它补一条合成结果。这比漏记结果更糟：记的是一件没发生过的事。
func TestATransientBarrierTwoFailureLeavesNoOrphanToolCall(t *testing.T) {
	t.Parallel()

	// 第 1 次 Append = 屏障 1（turn/start + user/message + step/start），
	// 第 2 次 = 屏障 2（assistant/message + tool/call）。只让第 2 次失败。
	store := &failOnceAtStore{at: 2}
	audit := adapter.NewMemoryAuditLog()
	rt := NewRuntime(Config{
		Gate:          taskgate.NewTaskGate(),
		Maas:          readFileThenAnswerMaas(),
		Audit:         audit,
		MaxToolRounds: 3,
		Events:        adapter.NewMemoryEventBus(),
		Tools:         readFileRegistry(audit),
		SessionEvents: store,
	})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "读一下那个文件"}); err == nil {
		t.Fatal("RunTask err = nil, want an error：屏障 2 落盘失败应该让整次执行失败")
	}

	var sawToolCall, sawTurnEnd bool
	for _, e := range store.eventsFor("s1") {
		switch e.Type {
		case domain.SessionEventToolCall:
			sawToolCall = true
		case domain.SessionEventTurnEnd:
			sawTurnEnd = true
		}
	}
	if !sawTurnEnd {
		t.Fatal("收尾那次 flush 没有落盘：这条测试要的是「后来的 flush 成功了」这个前提，" +
			"前提不成立的话下面那条断言什么也证明不了")
	}
	if sawToolCall {
		t.Error("屏障 2 失败后，那条从未被派发的 tool/call 还是被写进了日志：" +
			"它永远等不到 tool/result，恢复会为一次没发生过的调用补一条合成结果")
	}
}

// newTestRuntimeWithOversizedTool builds a Runtime whose one tool returns far
// more text than maxToolResultChars allows, sandboxed to toolRoot — the shape
// that makes the tool loop actually spill a full result to disk.
func newTestRuntimeWithOversizedTool(t *testing.T, store port.SessionEventStore, toolRoot string, body string) *Runtime {
	t.Helper()
	audit := adapter.NewMemoryAuditLog()
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"fetch_url"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	).WithAuditLog(audit)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "fetch_url",
		Description: "fetch a url",
		InputSchema: map[string]any{
			"required":   []string{"url"},
			"properties": map[string]any{"url": map[string]any{"type": "string"}},
		},
	}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: call.ID, Success: true, Output: body}, nil
	}))
	return NewRuntime(Config{
		Gate: taskgate.NewTaskGate(),
		Maas: &toolThenAnswerMaas{
			calls:  []domain.ToolCall{{ID: "call-1", Name: "fetch_url", Arguments: map[string]string{"url": "https://example.invalid/big"}}},
			marker: "输出被硬截断",
			answer: "读完了",
		},
		Audit:              audit,
		MaxToolRounds:      3,
		Events:             adapter.NewMemoryEventBus(),
		Tools:              registry,
		SessionEvents:      store,
		ToolRoot:           toolRoot,
		MaxToolResultChars: 200,
	})
}

// newTestRuntimeWithFailingToolOversizedError builds a Runtime whose one
// registered tool's handler returns a Go error (not a domain.ToolResult) long
// enough to be spilled to disk -- the shape
// TestDispatchErrorRecordsASpillLocatorThatNamesARealFile needs to exercise
// the DISPATCH-ERROR emission point in executeToolCalls (runtime.go's
// `if err != nil` branch around recordToolResult(call.ID, err.Error(), true,
// ...)), as distinct from the successful-dispatch emission point a few lines
// below it that newTestRuntimeWithOversizedTool / TestRunTaskRecordsASpillLocatorThatNamesARealFile
// already guards.
func newTestRuntimeWithFailingToolOversizedError(t *testing.T, store port.SessionEventStore, toolRoot string, errMsg string) *Runtime {
	t.Helper()
	audit := adapter.NewMemoryAuditLog()
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
		// A Go error, not a ToolResult{Success:false} -- this is what makes
		// executeToolCalls take the `err != nil` dispatch-error branch instead
		// of the ordinary successful-dispatch branch.
		return domain.ToolResult{}, errors.New(errMsg)
	}))
	return NewRuntime(Config{
		Gate: taskgate.NewTaskGate(),
		Maas: &toolThenAnswerMaas{
			calls: []domain.ToolCall{{ID: "lookup-1", Name: "lookup", Arguments: map[string]string{"query": "cache"}}},
			// Same hard-truncation footer text newTestRuntimeWithOversizedTool
			// uses: it only appears once the oversized result actually goes
			// through the cache-and-footer render path, which is exactly what
			// this test needs to confirm happened on the ERROR branch too.
			marker: "输出被硬截断",
			answer: "查不到，先这样回答",
		},
		Audit:              audit,
		MaxToolRounds:      3,
		Events:             adapter.NewMemoryEventBus(),
		Tools:              registry,
		SessionEvents:      store,
		ToolRoot:           toolRoot,
		MaxToolResultChars: 200,
	})
}

// 接线守卫（dispatch 错误路径）：runtime.go 里第二个 recordToolResult 发射点——
// dispatchToolCall 返回 Go error 时那条——同样必须传真实的 spill_locator，不是
// 一路传空串到事件里。这条测试是 TestRunTaskRecordsASpillLocatorThatNamesARealFile
// 在错误分支上的对称版：两个生产发射点各自有测试守着，其中一个改坏了，只有对应的
// 那条测试会红，不会两条一起哑。
//
// 断言同样用证据而不是形状：拿事件里的定位符去 toolRoot 下 Stat，文件必须真的在，
// 内容必须真的是这次 dispatch 错误的全文（"failed: " 前缀 + handler 返回的 error）。
func TestDispatchErrorRecordsASpillLocatorThatNamesARealFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	errMsg := strings.Repeat("X", 5000)
	store := &captureEventStore{}
	rt := newTestRuntimeWithFailingToolOversizedError(t, store, root, errMsg)

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "跑一下那个会失败的 lookup"}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	data := payloadOfType(t, store.events, domain.SessionEventToolResult)
	if isError, ok := data["is_error"].(bool); !ok || !isError {
		t.Fatalf("tool/result 载荷的 is_error = %v，want true：这条结果本该来自 dispatch 错误分支", data["is_error"])
	}
	locator, ok := data["spill_locator"].(string)
	if !ok {
		t.Fatalf("tool/result 载荷里没有字符串字段 spill_locator：%v", data)
	}
	if locator == "" {
		t.Fatal("spill_locator 是空串，但这次 dispatch 错误的全文确实超长并落了盘：" +
			"dispatch 错误分支的渲染点与记录点之间的线没接上")
	}
	full := filepath.Join(root, filepath.FromSlash(locator))
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("按 spill_locator=%q 在工具根 %q 下取全文失败：%v（定位符必须是工具根相对路径）",
			locator, root, err)
	}
	wantBody := "failed: " + errMsg
	if string(got) != wantBody {
		t.Errorf("取回的全文长度 = %d，要 %d：落盘的不是这次 dispatch 错误的全文", len(got), len(wantBody))
	}
}

// 接线守卫：spill_locator 必须是**这次执行真的写出来的那个文件**的路径，而不是
// 一路传空串到事件里。renderToolResultContent 落盘、recordToolResult 记录，两者
// 之间的那根线断了，单看载荷断言（recordToolResult 直接收一个字面量）照样是绿的。
//
// 断言用的是证据而不是形状：拿事件里的定位符去 toolRoot 下 Stat，文件必须真的在，
// 内容必须真的是被截掉的那段全文。
func TestRunTaskRecordsASpillLocatorThatNamesARealFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	body := strings.Repeat("X", 5000)
	store := &captureEventStore{}
	rt := newTestRuntimeWithOversizedTool(t, store, root, body)

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"},
		domain.Task{ID: "t1", SessionID: "s1", Input: "抓一下那个大页面"}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	data := payloadOfType(t, store.events, domain.SessionEventToolResult)
	locator, ok := data["spill_locator"].(string)
	if !ok {
		t.Fatalf("tool/result 载荷里没有字符串字段 spill_locator：%v", data)
	}
	if locator == "" {
		t.Fatal("spill_locator 是空串，但这次执行的工具结果确实超长并落了盘：" +
			"渲染点与记录点之间的线没接上")
	}
	full := filepath.Join(root, filepath.FromSlash(locator))
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("按 spill_locator=%q 在工具根 %q 下取全文失败：%v（定位符必须是工具根相对路径）",
			locator, root, err)
	}
	if string(got) != body {
		t.Errorf("取回的全文长度 = %d，要 %d：落盘的不是这次工具结果的全文", len(got), len(body))
	}
}
