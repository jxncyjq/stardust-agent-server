package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/manualgate"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/tool"
)

// TestManualGateDenyThenResume drives a full Manual-mode round trip where the
// human denies the pending sensitive tool call: suspend on round 1, then
// resume completes without ever executing write_file.
func TestManualGateDenyThenResume(t *testing.T) {
	dir := t.TempDir()
	cpStore := sessionstate.NewStore(dir)
	apStore := approval.NewToolGateStore(dir)
	gate := manualgate.New(apStore)
	var writeCalled bool
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }), tool.NoopGuardrails{})
	reg.RegisterDescriptor(tool.Descriptor{Name: "write_file", Sensitive: true}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		writeCalled = true
		return domain.ToolResult{Success: true, Output: "wrote"}, nil
	}))
	maas := &oneToolThenTextMaas{toolName: "write_file", toolArgs: map[string]string{"path": "out/a.txt"}}
	r := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
		Tools: reg, Checkpoints: cpStore, ToolGate: gate})
	task := domain.Task{ID: "t1", SessionID: "s1", AgentID: "a1", Status: domain.TaskRunning, Mode: domain.ModeManual, Input: "go"}
	if _, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task); err != ErrSuspended {
		t.Fatalf("first run err = %v, want ErrSuspended", err)
	}
	if _, err := apStore.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalDenied, ""); err != nil {
		t.Fatal(err)
	}
	run, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task)
	if err != nil {
		t.Fatalf("resume run err = %v", err)
	}
	if writeCalled {
		t.Fatal("denied write_file executed on resume")
	}
	if run.Result == "" {
		t.Fatal("resume produced no final answer")
	}
}

// TestManualGateApproveThenResume mirrors the deny scenario but the human
// approves the ticket: resume must execute write_file and complete.
//
// 它同时是**恢复入口的事件覆盖**（final-review.md I-1）。恢复是计划点名的五条任务
// 入口之一，而 runtime.go 恢复分支里那块发射（recordStepStart + recordAssistantMessage）
// 原先删掉之后 `go test ./internal/runtime/` 依旧全绿——整个分支里唯一完全没被盯住的
// 发射点。这里给它接上 store 并断言：恢复开出来的那个 turn 里，第一条 tool/call 之上
// 必须先有 step/start 和 assistant/message。
//
// 为什么盯的是「之上」而不是「存在」：恢复路径带着 checkpoint 里的待办工具调用直接
// 进工具循环，少发这两条的症状正是「一个 turn 开头就在派发工具、上面没有任何 assistant
// 消息」——既不可读，step/end 也配不上 step/start。既有的 TestEveryStepEndPairsWithAStepStart
// 跑的不是恢复路径，所以恢复少发 step/start 时它一次也不会红。
func TestManualGateApproveThenResume(t *testing.T) {
	dir := t.TempDir()
	cpStore := sessionstate.NewStore(dir)
	apStore := approval.NewToolGateStore(dir)
	gate := manualgate.New(apStore)
	events := &captureEventStore{}
	var writeCalled bool
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }), tool.NoopGuardrails{})
	reg.RegisterDescriptor(tool.Descriptor{Name: "write_file", Sensitive: true}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		writeCalled = true
		return domain.ToolResult{Success: true, Output: "wrote"}, nil
	}))
	// 第 1 轮报一个非零用量，好让 checkpoint 里真的存下累计值——否则「恢复不得重复
	// 计数」那条断言会在一份全零的日志上永远成立。
	maas := &oneToolThenTextMaas{toolName: "write_file", toolArgs: map[string]string{"path": "out/a.txt"},
		usage: port.InferenceResponse{PromptTokens: 111, CompletionTokens: 22, CachedTokens: 3, TotalTokens: 136}}
	r := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
		Tools: reg, Checkpoints: cpStore, ToolGate: gate, SessionEvents: events, ModelProfile: "manual-e2e"})
	task := domain.Task{ID: "t1", SessionID: "s1", AgentID: "a1", Status: domain.TaskRunning, Mode: domain.ModeManual, Input: "go"}
	if _, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task); err != ErrSuspended {
		t.Fatalf("first run err = %v, want ErrSuspended", err)
	}
	if _, err := apStore.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalApproved, ""); err != nil {
		t.Fatal(err)
	}
	run, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task)
	if err != nil {
		t.Fatalf("resume run err = %v", err)
	}
	if !writeCalled {
		t.Fatal("approved write_file did not execute on resume")
	}
	if run.Result == "" {
		t.Fatal("resume produced no final answer")
	}
	assertResumedTurnIsFullyRecorded(t, events.eventsFor("s1"))
}

// assertResumedTurnIsFullyRecorded 断言「恢复开出来的那个 turn」自己是完整的。
//
// 恢复是本次 RunTask 开的**第二个** turn（第一次运行挂起时那个 turn 已经收尾），
// 所以它的 turn 号是 1。
func assertResumedTurnIsFullyRecorded(t *testing.T, all []domain.SessionEvent) {
	t.Helper()

	const resumedTurn = 1
	var turn []domain.SessionEvent
	for _, e := range all {
		if intFieldOfEvent(t, e, "turn") == resumedTurn {
			turn = append(turn, e)
		}
	}
	if len(turn) == 0 {
		t.Fatalf("恢复没有开出 turn %d：挂起→恢复这条入口一条事件都没写", resumedTurn)
	}

	firstToolCall := -1
	for i, e := range turn {
		if e.Type == domain.SessionEventToolCall {
			firstToolCall = i
			break
		}
	}
	if firstToolCall < 0 {
		t.Fatalf("恢复的 turn 里没有 tool/call：这条测试的前提（恢复会派发 checkpoint 里的待办调用）不成立了")
	}
	sawStepStart, sawAssistant := false, false
	for _, e := range turn[:firstToolCall] {
		switch e.Type {
		case domain.SessionEventStepStart:
			sawStepStart = true
		case domain.SessionEventAssistantMessage:
			sawAssistant = true
		}
	}
	// I-2：恢复重记的这条 assistant/message 的 usage 必须是 0。
	//
	// 这不是「拿不到就填零」：这条响应的 token 已经由生成它的那一轮按**单次响应用量**
	// 记过一次了，这里是同一条响应在新 turn 里的重记，增量确实是 0。checkpoint 存的
	// 是**整次运行的累计值**，填进来会让任何按 assistant/message 求和统计用量的消费者
	// 在「挂起→恢复」过的任务上多算一大截。
	//
	// I-3：同一条事件的 model_profile 必须带着装配传下来的档位名，不是空串。
	for _, e := range turn[:firstToolCall] {
		if e.Type != domain.SessionEventAssistantMessage {
			continue
		}
		var payload struct {
			Usage struct {
				Prompt     int `json:"prompt"`
				Completion int `json:"completion"`
				Cached     int `json:"cached"`
				Total      int `json:"total"`
			} `json:"usage"`
			ModelProfile string `json:"model_profile"`
		}
		if err := json.Unmarshal(e.Data, &payload); err != nil {
			t.Fatalf("unmarshal assistant/message: %v", err)
		}
		if payload.Usage != (struct {
			Prompt     int `json:"prompt"`
			Completion int `json:"completion"`
			Cached     int `json:"cached"`
			Total      int `json:"total"`
		}{}) {
			t.Errorf("恢复重记的 assistant/message 带了非零 usage %+v："+
				"那是 checkpoint 里的**整次运行累计值**，不是这条响应的单次用量，"+
				"按 assistant/message 求和的消费者会多算（I-2）", payload.Usage)
		}
		if payload.ModelProfile != "manual-e2e" {
			t.Errorf("恢复重记的 assistant/message 的 model_profile = %q, want %q（I-3）",
				payload.ModelProfile, "manual-e2e")
		}
	}

	if !sawStepStart {
		t.Error("恢复的 turn 在派发第一条工具之前没有 step/start：" +
			"这一步的 step/end 将没有可配对的 step/start（spec §4.1 的 step 配平）")
	}
	if !sawAssistant {
		t.Error("恢复的 turn 在派发第一条工具之前没有 assistant/message：" +
			"一个开头就在派发工具、上面没有任何模型消息的 turn 读不出它在做什么")
	}

	// step 配平：恢复的 turn 里每条 step/end 都要有同 (turn, step) 的 step/start。
	opened := map[int]bool{}
	for _, e := range turn {
		switch e.Type {
		case domain.SessionEventStepStart:
			opened[intFieldOfEvent(t, e, "step")] = true
		case domain.SessionEventStepEnd:
			if step := intFieldOfEvent(t, e, "step"); !opened[step] {
				t.Errorf("恢复的 turn 里 step %d 有 step/end 却没有 step/start", step)
			}
		}
	}
}

// twoDuplicateSensitiveCallsThenTextMaas issues two write_file calls in a
// single round, both carrying the SAME provider id -- the shape
// adapter.openAIToolCalls produces when the provider omits ids and both calls
// target the same function name (see disambiguateCallIDs's doc comment).
// Round 2 answers in text.
type twoDuplicateSensitiveCallsThenTextMaas struct{ calls int }

func (m *twoDuplicateSensitiveCallsThenTextMaas) Generate(ctx context.Context, _ port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.calls++
	if m.calls == 1 {
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{
			{ID: "write_file", Name: "write_file", Arguments: map[string]string{"path": "a.txt"}},
			{ID: "write_file", Name: "write_file", Arguments: map[string]string{"path": "b.txt"}},
		}}, nil
	}
	return port.InferenceResponse{Text: "done"}, nil
}

// TestManualGateApprovesBothOfTwoDuplicateIDSensitiveCalls pins the N-I1 fix
// (task-4-review-2.md §3): when a round asks for the same sensitive tool
// twice in parallel and the provider hands back the same degraded id for
// both (see disambiguateCallIDs), the human must be able to approve BOTH
// resulting tickets and see both writes execute on resume.
//
// Before the fix, ShouldSuspend opened its approval tickets under the
// PRE-disambiguation id (both calls collided on "write_file") while dispatch
// resolved against the POST-disambiguation id ("write_file", "write_file#2"),
// so the second call's ticket could never be found: dispatch failed loud with
// "undecided sensitive call" even after a human approved everything they were
// shown. Moving disambiguateCallIDs to where st.resp is produced (generateStep)
// means ShouldSuspend already sees the settled ids, so both tickets open under
// ids dispatch can actually resolve.
func TestManualGateApprovesBothOfTwoDuplicateIDSensitiveCalls(t *testing.T) {
	dir := t.TempDir()
	cpStore := sessionstate.NewStore(dir)
	apStore := approval.NewToolGateStore(dir)
	gate := manualgate.New(apStore)

	var mu sync.Mutex
	var written []string
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }), tool.NoopGuardrails{})
	reg.RegisterDescriptor(tool.Descriptor{Name: "write_file", Sensitive: true}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		mu.Lock()
		written = append(written, call.Arguments["path"])
		mu.Unlock()
		return domain.ToolResult{Success: true, Output: "wrote " + call.Arguments["path"]}, nil
	}))

	maas := &twoDuplicateSensitiveCallsThenTextMaas{}
	r := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
		Tools: reg, Checkpoints: cpStore, ToolGate: gate})
	task := domain.Task{ID: "t1", SessionID: "s1", AgentID: "a1", Status: domain.TaskRunning, Mode: domain.ModeManual, Input: "go"}
	if _, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task); err != ErrSuspended {
		t.Fatalf("first run err = %v, want ErrSuspended", err)
	}

	// Both tickets must exist under the DISAMBIGUATED ids -- "write_file" and
	// "write_file#2" per disambiguateCallIDs's suffixing rule -- for the human
	// to have anything distinct to approve in the first place.
	for _, id := range []string{"write_file", "write_file#2"} {
		if _, err := apStore.Decide("s1", approval.TicketID("t1", id), approval.ApprovalApproved, ""); err != nil {
			t.Fatalf("approve ticket %q: %v", id, err)
		}
	}

	run, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task)
	if err != nil {
		t.Fatalf("resume run err = %v, want nil", err)
	}
	if run.Result == "" {
		t.Fatal("resume produced no final answer")
	}

	mu.Lock()
	got := append([]string(nil), written...)
	mu.Unlock()
	sort.Strings(got)
	want := []string{"a.txt", "b.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("write_file executed for paths %v, want both %v: the second call's approval ticket was never resolved", got, want)
	}
}
