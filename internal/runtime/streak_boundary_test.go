package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/taskgate"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/sessionstate"
)

// taskStart 是个下标，而 conversation 有两条路会让它失效：压缩会把 messages 变短，
// 检查点恢复则整个绕过 newConversation/appendHistory 直接构造 conversation。
//
// 这两条都不是假想：applyCompaction 把 messages[1:preserveStart] 换成**一条**摘要，
// 长度随之缩水；restoreConversation 用结构体字面量建 conversation，字段拿的是零值。
// 一个越界的 taskStart 会让 repeatedCallStreak 在切片上 panic，一个为 0 的 taskStart
// 会让历史重新被算进 streak——正是这次改动要堵的那个误报。

// 压缩之后 streak 仍要只数本任务的轮次，且不能 panic。
func TestCompactionKeepsTheTaskBoundaryValid(t *testing.T) {
	t.Parallel()

	histCall := domain.ToolCall{ID: "old", Name: "read_file",
		Arguments: map[string]string{"path": "config.md"}}
	live := []domain.ToolCall{{ID: "now", Name: "read_file",
		Arguments: map[string]string{"path": "config.md"}}}

	convo := newConversation("base", nil)
	// 一段长历史：连续 3 轮同样的调用。
	var history []port.InferenceMessage
	for i := 0; i < 3; i++ {
		history = append(history,
			port.InferenceMessage{Role: port.RoleAssistant, ToolCalls: []domain.ToolCall{histCall}},
			port.InferenceMessage{Role: port.RoleTool, ToolCallID: "old", Content: "内容"})
	}
	convo.appendHistory(history)
	convo.appendCurrentInput("当前任务")
	before := convo.taskStart

	// 压缩掉 message[0] 之后、末尾两条之前的一切——历史整段被摘要吞掉。
	convo.applyCompaction(len(convo.messages)-1, "之前聊了很多")

	if convo.taskStart > len(convo.messages) {
		t.Fatalf("压缩后 taskStart=%d 超过 len(messages)=%d（压缩前是 %d）："+
			"repeatedCallStreak 会在切片上 panic",
			convo.taskStart, len(convo.messages), before)
	}
	// 不 panic 只是底线；语义也要对：本任务还没跑过任何一轮，streak 必须是 1。
	if got := convo.repeatedCallStreak(live); got != 1 {
		t.Errorf("压缩后 streak = %d，要 1：边界指错了位置，历史（或摘要）被算进了本任务的轮次", got)
	}
}

// 检查点恢复必须把边界一起带回来，否则续跑的任务会把历史算进 streak。
func TestRestoringACheckpointKeepsTheTaskBoundary(t *testing.T) {
	t.Parallel()

	histCall := domain.ToolCall{ID: "old", Name: "read_file",
		Arguments: map[string]string{"path": "config.md"}}
	live := []domain.ToolCall{{ID: "now", Name: "read_file",
		Arguments: map[string]string{"path": "config.md"}}}

	convo := newConversation("base", nil)
	var history []port.InferenceMessage
	for i := 0; i < 3; i++ {
		history = append(history,
			port.InferenceMessage{Role: port.RoleAssistant, ToolCalls: []domain.ToolCall{histCall}},
			port.InferenceMessage{Role: port.RoleTool, ToolCallID: "old", Content: "内容"})
	}
	convo.appendHistory(history)
	convo.appendCurrentInput("当前任务")

	// 挂起 → 恢复，走的是生产那两个函数。
	restored := restoreConversation(snapshotMessages(convo), convo.taskStart)

	if restored.taskStart != convo.taskStart {
		t.Errorf("恢复后 taskStart = %d，挂起时是 %d：边界没跟着检查点走",
			restored.taskStart, convo.taskStart)
	}
	if got := restored.repeatedCallStreak(live); got != 1 {
		t.Errorf("恢复后 streak = %d，要 1：历史又被算进来了——"+
			"续跑的任务会平白收到重复调用警告（warn=%d）", got, repeatWarnStreak)
	}
}

// 上一条测试手工把 taskStart 传给了 restoreConversation，绕过了「写进 Checkpoint
// 再读出来」那一环——写侧断链它抓不到。这一条走真实的挂起路径：跑 RunTask 到挂起，
// 从 store 里把检查点读回来，断言边界确实被存进去了。
//
// 存与取是一对：只要有一侧断了，续跑的任务就会把历史算进 streak。
func TestTheSuspendedCheckpointCarriesTheTaskBoundary(t *testing.T) {
	store := sessionstate.NewStore(t.TempDir())

	histCall := domain.ToolCall{ID: "old", Name: "echo",
		Arguments: map[string]string{"text": "旧的"}}
	var history []port.InferenceMessage
	for i := 0; i < 3; i++ {
		history = append(history,
			port.InferenceMessage{Role: port.RoleAssistant, ToolCalls: []domain.ToolCall{histCall}},
			port.InferenceMessage{Role: port.RoleTool, ToolCallID: "old", Content: "旧结果"})
	}

	runner := NewRuntime(Config{
		Gate: taskgate.NewTaskGate(), Maas: &scriptedMaas{},
		Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
		Tools: echoRegistry(t), Checkpoints: store, ToolGate: &gateOnce{},
		// G3 打开：历史以 transcript 进 conversation。
		HistoryTranscript: history,
	})
	task := domain.Task{ID: "task-b", SessionID: "sess-b", AgentID: "agent-1",
		Status: domain.TaskRunning, Input: "go"}

	if _, err := runner.RunTask(context.Background(), domain.Agent{ID: "agent-1"}, task); !errors.Is(err, ErrSuspended) {
		t.Fatalf("RunTask err = %v, want ErrSuspended", err)
	}
	cp, ok, err := store.Load("sess-b", "")
	if err != nil {
		t.Fatalf("Load checkpoint: %v", err)
	}
	if !ok {
		t.Fatal("挂起时没写检查点")
	}
	// 先确认这次真的走了 transcript 那条路，否则边界恒为 1，断言成了空过。
	if cp.TaskStart <= 1 {
		t.Fatalf("检查点里的 task_start = %d：历史没进 conversation，这条测试量不到写侧",
			cp.TaskStart)
	}
	// 边界必须落在历史之后：message[0] + 6 条历史 + 当前输入。
	if want := 1 + len(history); cp.TaskStart != want {
		t.Errorf("检查点里的 task_start = %d，要 %d（message[0] + %d 条历史）："+
			"边界没被存进检查点，续跑时历史会重新被算进重复熔断",
			cp.TaskStart, want, len(history))
	}
}

// 本字段引入之前写下的检查点没有这个下标，反序列化得到 0。那些检查点写于 G3 之前，
// conversation 里不存在历史段，起点恒为 1——契约把 0 定义为「按 1 处理」。
func TestACheckpointFromBeforeTheBoundaryFieldRestoresToOne(t *testing.T) {
	t.Parallel()

	snaps := []sessionstate.MessageSnapshot{
		{Role: port.RoleUser, Content: "base"},
		{Role: port.RoleAssistant, Content: "干活"},
	}
	restored := restoreConversation(snaps, 0)
	if restored.taskStart != 1 {
		t.Errorf("老检查点恢复出的 taskStart = %d，要 1", restored.taskStart)
	}
}

// 一个大过消息条数的下标是损坏的检查点，不是可以将就的输入：将就下去
// repeatedCallStreak 会在切片上 panic，而那时已经离现场很远了。
func TestACorruptBoundaryFailsLoudAtRestore(t *testing.T) {
	t.Parallel()

	snaps := []sessionstate.MessageSnapshot{
		{Role: port.RoleUser, Content: "base"},
		{Role: port.RoleAssistant, Content: "干活"},
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("taskStart 大过消息条数却没有 fail-loud：这个下标会在后面某处 panic，届时看不出是检查点坏了")
		}
		if msg, ok := r.(string); ok && !strings.Contains(msg, "task_start") {
			t.Errorf("panic 信息里没提 task_start，定位不到坏在哪：%q", msg)
		}
	}()
	restoreConversation(snaps, 99)
}
