package runtime

import (
	"context"
	"errors"
	"fmt"
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

// 上一条把 applyCompaction 调在了 taskStart == preserveStart 这个点上，平移项
// (taskStart - preserveStart) 恰好是 0——于是「+1 的 off-by-one」和「删掉整个 A 支、
// 无条件置 2」两种写法都能让它通过。它证明的只是「边界不大过新长度」这个弱性质。
//
// 这两条把平移项做成非零，判别力来自对 taskStart 的**精确**断言。
//
// 别删那两条 taskStart 断言：这两个夹具里的 streak 断言其实是恒真的装饰——A 支
// 在 taskStart ∈ {2,4,5} 上 streak 都是 3，B 支在 taskStart ∈ {0..3} 上都是 1。
// 它们留着是为了说明「边界指对了位置」这件事的后果，不是判别力的来源。
func TestCompactionShiftsTheBoundaryWhenItLandsInTheKeptTail(t *testing.T) {
	t.Parallel()

	call := domain.ToolCall{ID: "c", Name: "read_file",
		Arguments: map[string]string{"path": "config.md"}}
	live := []domain.ToolCall{call}

	convo := newConversation("base", nil)
	convo.appendHistory([]port.InferenceMessage{
		{Role: port.RoleAssistant, ToolCalls: []domain.ToolCall{
			{ID: "old", Name: "list_files", Arguments: map[string]string{"path": "."}}}},
		{Role: port.RoleTool, ToolCallID: "old", Content: "历史结果"},
	})
	convo.appendCurrentInput("读 config.md")
	// 本任务已经跑过两轮同样的调用。
	for i := 0; i < 2; i++ {
		convo.appendAssistant("我读一下", []domain.ToolCall{call})
		convo.appendToolResults([]domain.ToolCall{call}, map[string]string{"c": "文件内容"})
	}
	// messages: [base, 历史A, 历史T, 当前输入, A1, T1, A2, T2] = 8 条，taskStart = 3。
	if convo.taskStart != 3 {
		t.Fatalf("夹具没搭对：taskStart = %d，要 3", convo.taskStart)
	}
	// preserveStart = 1：整段都保留，平移项 = 3 - 1 = 2（非零，这是关键）。
	convo.applyCompaction(1, "摘要")
	// 新数组 [base, 摘要] + 原 messages[1:]，本任务的起点前移到 2 + 2 = 4。
	if convo.taskStart != 4 {
		t.Errorf("压缩后 taskStart = %d，要 4（下标 2 的基准 + 平移项 2）：平移算错了", convo.taskStart)
	}
	// 本任务那两轮仍在边界之内 + 待发的这一轮 = 3。位移错一位就会把 A1 或当前输入
	// 挪到边界另一侧，这个数随之变化。
	if got := convo.repeatedCallStreak(live); got != 3 {
		t.Errorf("压缩后 streak = %d，要 3（本任务已跑 2 轮 + 待发 1 轮）："+
			"边界平移错位，本任务的轮次被切掉或历史被算了进来", got)
	}
}

// B 支：边界落在被压缩掉的那一段里，说明历史已被摘要吞并，本任务的消息全在 tail 中。
func TestCompactionPutsTheBoundaryAfterTheSummaryWhenHistoryIsFoldedIn(t *testing.T) {
	t.Parallel()

	call := domain.ToolCall{ID: "c", Name: "read_file",
		Arguments: map[string]string{"path": "config.md"}}
	live := []domain.ToolCall{call}

	convo := newConversation("base", nil)
	convo.appendHistory([]port.InferenceMessage{
		{Role: port.RoleAssistant, ToolCalls: []domain.ToolCall{call}}, // 与本轮相同的调用
		{Role: port.RoleTool, ToolCallID: "old", Content: "历史结果"},
	})
	convo.appendCurrentInput("读 config.md")
	convo.appendAssistant("我读一下", []domain.ToolCall{call})
	convo.appendToolResults([]domain.ToolCall{call}, map[string]string{"c": "文件内容"})
	// messages: [base, 历史A, 历史T, 当前输入, A1, T1] = 6 条，taskStart = 3。

	// preserveStart = 5 > taskStart=3 → B 支：历史与当前输入、A1 一起被摘要吞掉。
	convo.applyCompaction(5, "摘要")
	if convo.taskStart != 2 {
		t.Errorf("压缩后 taskStart = %d，要 2（摘要之后）", convo.taskStart)
	}
	// 新数组 [base, 摘要, T1]：本任务在边界之内没有任何 assistant 轮次，
	// 且被吞掉的那条历史 assistant 绝不能被算进来。
	if got := convo.repeatedCallStreak(live); got != 1 {
		t.Errorf("压缩后 streak = %d，要 1：摘要之前的东西（含那条与本轮相同的历史调用）"+
			"被算进了本任务的轮次", got)
	}
}

// 历史必须在本任务开口之前注入。晚了的话 appendHistory 会把边界推到本任务的轮次
// 之后，repeatedCallStreak 从此恒为 1——重复熔断**静默失效**，而不是报错。
//
// 今天只有一处调用（RunTask 里、第一次模型请求之前），所以这条断言守的是将来：
// 「在任务跑起来后补一次历史」这种改动必须当场炸掉，不能悄悄把熔断关掉。
func TestAppendingHistoryAfterTheTaskHasSpokenFailsLoud(t *testing.T) {
	t.Parallel()

	convo := newConversation("base", nil)
	convo.appendCurrentInput("本任务已经开口了")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("在本任务的轮次之后注入历史却没有 fail-loud：" +
				"边界会被推到那些轮次之后，重复熔断从此恒为 1，静默失效")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "appendHistory") {
			t.Errorf("panic 信息里没提 appendHistory，定位不到坏在哪：%q", msg)
		}
	}()
	convo.appendHistory([]port.InferenceMessage{{Role: port.RoleAssistant, Content: "迟到的历史"}})
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
		// 用 fmt.Sprint 取文本而不是断言成 string：写成 r.(string) 的话，谁把
		// panic 值换成 error（例如 panic(fmt.Errorf(...))），ok 就变 false，
		// 这条断言会**静默消失**，只剩「有没有 panic」——正是 §0 点名的静默跳过。
		if msg := fmt.Sprint(r); !strings.Contains(msg, "task_start") {
			t.Errorf("panic 信息里没提 task_start，定位不到坏在哪：%q", msg)
		}
	}()
	restoreConversation(snaps, 99)
}
