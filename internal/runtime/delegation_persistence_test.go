package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// blockingSubMaas 让推理一直阻塞到用例结束（release 由 t.Cleanup 关闭），于是子任务
// 在整条用例期间都跑不完：快照里那一行只能是 running 状态的开始行，不会被一个已经收
// 尾的子任务搅浑。
//
// 用例不在它身上同步，也不该同步：要断言的是「RunSubTaskAsync 返回时那一行就已经
// 在」，而那个时刻在 goroutine 跑起来之前——等模型进门再拍快照，恰好会把「插行挪进
// goroutine 里」这个错误实现放过去。
//
// 它的等待是有界的：用例忘了放行时它报错退出，而不是把 goroutine 永久停住。
type blockingSubMaas struct {
	release chan struct{}
}

func newBlockingSubMaas(t *testing.T) *blockingSubMaas {
	t.Helper()
	m := &blockingSubMaas{release: make(chan struct{})}
	t.Cleanup(func() { close(m.release) })
	return m
}

func (m *blockingSubMaas) Generate(context.Context, port.InferenceRequest) (port.InferenceResponse, error) {
	select {
	case <-m.release:
	case <-time.After(30 * time.Second):
		return port.InferenceResponse{}, errors.New("blockingSubMaas: 用例结束时没有放行")
	}
	return port.InferenceResponse{Text: "OK"}, nil
}

// countingSubMaas 数自己被调用了几次，用来证明「没起来」不是「起来了但很快就完了」。
type countingSubMaas struct {
	mu sync.Mutex
	n  int
}

func (m *countingSubMaas) Generate(context.Context, port.InferenceRequest) (port.InferenceResponse, error) {
	m.mu.Lock()
	m.n++
	m.mu.Unlock()
	return port.InferenceResponse{Text: "OK"}, nil
}

func (m *countingSubMaas) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n
}

// TestSubTaskIDsAreUUIDs：子任务 id 不再由进程内计数器拼出来。
//
// 老形态 "<父>:sub-<n>" 的 n 来自一个内存计数器，重启后从 1 重数——于是重启前后
// 两条不同的子任务会拿到同一个 id，而这份记录的全部意义就是按 id 找回它。
func TestSubTaskIDsAreUUIDs(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := newTaskRunRuntime(runs)
	handle, err := rt.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
	})
	if err != nil {
		t.Fatalf("RunSubTaskAsync: %v", err)
	}
	if _, err := uuid.Parse(handle.TaskID); err != nil {
		t.Fatalf("子任务 id %q 不是 UUID：%v", handle.TaskID, err)
	}
	if strings.Contains(handle.TaskID, ":sub-") {
		t.Errorf("子任务 id 仍然编码着父子关系：%q", handle.TaskID)
	}
}

// TestBackgroundSubTaskIsRecordedBeforeItStarts：插行发生在 goroutine 之前。
//
// 断言「RunSubTaskAsync 返回时记录已经在」——放进 goroutine 里写的话，父进程在这
// 之后立刻退出就什么都没留下，而那正是本设计要覆盖的场景。模型一直阻塞，所以子任务
// 在整条用例期间都跑不完，下面读到的那一行只能是派发那一刻插进去的。
func TestBackgroundSubTaskIsRecordedBeforeItStarts(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	model := newBlockingSubMaas(t)
	rt := newTaskRunRuntime(runs, func(cfg *Config) { cfg.Maas = model })
	handle, err := rt.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
	})
	if err != nil {
		t.Fatalf("RunSubTaskAsync: %v", err)
	}
	started, _ := runs.snapshot()
	if len(started) != 1 {
		t.Fatalf("返回时已落盘 %d 条, want 1", len(started))
	}
	got := started[0]
	if got.TaskID != handle.TaskID {
		t.Errorf("落盘的 task id = %q, handle 是 %q", got.TaskID, handle.TaskID)
	}
	// agent_id 是开始时定型的列：FinishTaskRun 的 UPDATE 不回写它（见
	// internal/storage/task_runs.go）。这里漏填，这条运行记录就永久没有归属 agent，
	// 查询出口会呈现一条不知道是谁跑的子任务，且没有任何东西报错。
	// 未具名委派跑的是父的克隆，childFor 给它的身份 id 就是子任务 id。
	if got.AgentID != handle.TaskID {
		t.Errorf("落盘那一行的 agent id = %q, want %q", got.AgentID, handle.TaskID)
	}
	if got.ID == "" {
		t.Error("落盘那一行没有 run id")
	}
	if _, err := uuid.Parse(got.ID); err != nil {
		t.Errorf("run id %q 不是 UUID：%v", got.ID, err)
	}
	if got.ParentTaskID != "task-parent" {
		t.Errorf("parent = %q, want task-parent", got.ParentTaskID)
	}
	if !got.Background {
		t.Error("后台子任务没有被标成 background")
	}
	if got.Goal != "把 A 查清楚" {
		t.Errorf("goal = %q", got.Goal)
	}
	if got.Status != domain.RunStatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.StartedAt.IsZero() {
		t.Error("落盘那一行没有开始时刻")
	}
}

// TestBackgroundSubTaskFinishesTheRowItPreInserted：派发方插的那一行，最后由子任务
// 自己收尾——而不是 RunTask 另起一条。
//
// 这条钉住的是派发方必须同时设 task.RunID 与 task.Background：只设 Background，
// RunTask 会拿自己 mint 的 id 再插一行，派发方那一行永远停在 running；只设 RunID，
// 记录会被标成一条直连任务。两个方向的判据分别由 RunTask 侧的
// TestOnlyRunIDDecidesWhoWritesTheOpeningRow 与这里的字段断言守着。
func TestBackgroundSubTaskFinishesTheRowItPreInserted(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := newTaskRunRuntime(runs)
	handle, err := rt.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
	})
	if err != nil {
		t.Fatalf("RunSubTaskAsync: %v", err)
	}
	// 子任务在自己的 goroutine 里跑完才写终态，所以这里等它落下来。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, done := runs.snapshot(); len(done) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	started, finished := runs.snapshot()
	if len(started) != 1 {
		t.Fatalf("开始写入 %d 条, want 1——RunTask 不该再插一行", len(started))
	}
	if len(finished) != 1 {
		t.Fatalf("终态写入 %d 次, want 1", len(finished))
	}
	if finished[0].ID != started[0].ID {
		t.Errorf("终态写去了 %q，派发方开的那一行是 %q——那一行永远停在 running",
			finished[0].ID, started[0].ID)
	}
	if finished[0].TaskID != handle.TaskID {
		t.Errorf("终态的 task id = %q, handle 是 %q", finished[0].TaskID, handle.TaskID)
	}
	// 这三项走的是另一条路：派发方插行时填的那份不参与收尾，收尾写的是 RunTask 从
	// domain.Task 上读到的那份。派发方设了 RunID 却漏设 Background / ParentTaskID /
	// Goal，记录不会报错，只会把一条后台子任务记成一条顶层直连任务。
	if !finished[0].Background {
		t.Error("终态那一行没有被标成 background")
	}
	if finished[0].ParentTaskID != "task-parent" {
		t.Errorf("终态的 parent = %q, want task-parent", finished[0].ParentTaskID)
	}
	if finished[0].Goal != "把 A 查清楚" {
		t.Errorf("终态的 goal = %q", finished[0].Goal)
	}
}

// TestBackgroundSubTaskDoesNotStartWhenItCannotBeRecorded：插不进去就不起，
// 并且任务边界票要还回去。
//
// 边界票不还，插件变更会永远等这个从来没跑起来的子任务——一次落盘失败换来一把
// 卡死的闸门。
func TestBackgroundSubTaskDoesNotStartWhenItCannotBeRecorded(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{failStart: errors.New("disk full")}
	model := &countingSubMaas{}
	rt := newTaskRunRuntime(runs, func(cfg *Config) { cfg.Maas = model })
	_, err := rt.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
	})
	if err == nil {
		t.Fatal("记录插不进去，后台子任务却起来了")
	}
	// 错误必须带上动作与标识（全局约束的 fmt.Errorf("<动作> <标识>: %w", err) 形状）：
	// 裸 return err 只剩 "disk full"，读日志的人无从知道是哪一步、哪条子任务失败的。
	if !strings.Contains(err.Error(), "record the start of background sub-task") {
		t.Errorf("错误 %v 没有说清失败的是哪个动作", err)
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("错误 %v 丢掉了底层原因", err)
	}
	if n := model.calls(); n != 0 {
		t.Errorf("模型被调用了 %d 次", n)
	}
	// wait <= 0 是「此刻闸门空着才算数」，票没还回来这里就立刻失败。
	if err := rt.gate.ApplyAtBoundary(context.Background(), 0, func() error { return nil }); err != nil {
		t.Errorf("ApplyAtBoundary: %v——派发失败没有把任务边界票还回去", err)
	}
}

// TestSynchronousSubTaskRecordsItsParentAndGoal：同步委派的运行记录也要认得自己的
// 父任务。
//
// parent_task_id 空串在这个字段的契约里是「顶层任务」，不是「不知道」。runChild
// 不填它的话，每一条同步子任务都会伪装成一条直连任务，父子树整棵断掉且不报错。
func TestSynchronousSubTaskRecordsItsParentAndGoal(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := newTaskRunRuntime(runs)
	res, err := rt.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
	})
	if err != nil {
		t.Fatalf("RunSubTask: %v", err)
	}
	started, _ := runs.snapshot()
	if len(started) != 1 {
		t.Fatalf("开始写入 %d 条, want 1", len(started))
	}
	got := started[0]
	if got.TaskID != res.TaskID {
		t.Errorf("落盘的 task id = %q, 返回的是 %q", got.TaskID, res.TaskID)
	}
	if got.ParentTaskID != "task-parent" {
		t.Errorf("parent = %q, want task-parent——同步子任务被记成了顶层任务", got.ParentTaskID)
	}
	if got.Goal != "把 A 查清楚" {
		t.Errorf("goal = %q", got.Goal)
	}
	if got.Background {
		t.Error("同步子任务被标成了 background")
	}
}

// namedDelegationRuntime 造一个能按名字委派的派发方：dispatcherRuns 是它自己的运行
// 记录落点，childRuns 是它解析出来的子运行时的落点。两者可以不同——具名路径
// （DelegationAgents.ResolveDelegate）与克隆路径不共享装配，这正是要检验的地方。
func namedDelegationRuntime(dispatcherRuns, childRuns port.TaskRunStore) *Runtime {
	agents := &recordingDelegationAgents{
		fakeDelegationAgents: fakeDelegationAgents{names: []string{"researcher"}},
		childTaskRuns:        childRuns,
	}
	return NewRuntime(Config{
		Gate:             taskgate.NewTaskGate(),
		Maas:             adapter.NewRecordingMaas("OK"),
		Audit:            adapter.NewMemoryAuditLog(),
		Events:           adapter.NewMemoryEventBus(),
		TaskRuns:         dispatcherRuns,
		DelegationAgents: agents,
		MaxSpawnDepth:    3,
	})
}

// waitForFinishedRuns 等终态落下来；子任务在自己的 goroutine 里跑完才写。
func waitForFinishedRuns(t *testing.T, runs *recordingTaskRuns, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, done := runs.snapshot(); len(done) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNamedBackgroundDelegationIsRefusedWhenTheChildWritesToAnotherStore：派发方插
// 开始那一行、子运行时写终态，两者必须是同一个 store；不是就拒绝这次委派。
//
// 具名委派（DelegationAgents.ResolveDelegate）今天组 Config 时根本不传 TaskRuns，于是
// 派发方插下的那一行永远没人收尾：它停在 running，下一次启动的 SweepRunning 把一条正
// 常跑完的子任务扫成 interrupted——比不落盘更坏，因为它给出的是一个确信的错误答案。
func TestNamedBackgroundDelegationIsRefusedWhenTheChildWritesToAnotherStore(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := namedDelegationRuntime(runs, nil)
	_, err := rt.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
		AgentID:      "researcher",
	})
	if err == nil {
		t.Fatal("子运行时写不到派发方那个 store，后台委派却被放行了")
	}
	if !strings.Contains(err.Error(), "different store") {
		t.Errorf("错误 %v 没有说清拒绝的理由", err)
	}
	if started, _ := runs.snapshot(); len(started) != 0 {
		t.Errorf("被拒的委派仍然留下了 %d 条开始行", len(started))
	}
	// wait <= 0 是「此刻闸门空着才算数」：拒绝路径不许拿着任务边界票不放。
	if err := rt.gate.ApplyAtBoundary(context.Background(), 0, func() error { return nil }); err != nil {
		t.Errorf("ApplyAtBoundary: %v——拒绝路径没有把任务边界票还回去", err)
	}
}

// TestNamedBackgroundDelegationFinishesTheRowWhenTheStoreIsShared：两边是同一个
// store 时，具名后台委派的那一行确实被收尾。
//
// 上一条只证明「不同就拒」，这一条证明「相同就通」——否则把校验写成「一律拒绝」也
// 能让上一条全绿，而具名后台委派就整条消失了。
func TestNamedBackgroundDelegationFinishesTheRowWhenTheStoreIsShared(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := namedDelegationRuntime(runs, runs)
	handle, err := rt.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
		AgentID:      "researcher",
	})
	if err != nil {
		t.Fatalf("RunSubTaskAsync: %v", err)
	}
	waitForFinishedRuns(t, runs, 1)
	started, finished := runs.snapshot()
	if len(started) != 1 || len(finished) != 1 {
		t.Fatalf("开始 %d 条 / 终态 %d 条, want 1/1", len(started), len(finished))
	}
	if finished[0].ID != started[0].ID {
		t.Errorf("终态写去了 %q，派发方开的那一行是 %q——那一行永远停在 running",
			finished[0].ID, started[0].ID)
	}
	if finished[0].TaskID != handle.TaskID {
		t.Errorf("终态的 task id = %q, handle 是 %q", finished[0].TaskID, handle.TaskID)
	}
}

// TestBackgroundSubTaskWithoutADispatcherStoreLetsTheChildOpenItsOwnRow：派发方没有
// 落点就不铸 run id，开始那一行交给子运行时自己写。
//
// 不变量是「task.RunID 非空 ⟺ 确实有人插过那一行」。无条件铸 id 会让 RunTask 以为
// 行已经有了而跳过插行，接着收尾写向一条从未插入的行——真实的 FinishTaskRun 找不到
// 那一行会报错，于是一条正常跑完的子任务被记成 failed。
func TestBackgroundSubTaskWithoutADispatcherStoreLetsTheChildOpenItsOwnRow(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := namedDelegationRuntime(nil, runs)
	handle, err := rt.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
		AgentID:      "researcher",
	})
	if err != nil {
		t.Fatalf("RunSubTaskAsync: %v", err)
	}
	waitForFinishedRuns(t, runs, 1)
	started, finished := runs.snapshot()
	if len(started) != 1 {
		t.Fatalf("开始写入 %d 条, want 1——派发方没插行，子运行时也没插", len(started))
	}
	if len(finished) != 1 {
		t.Fatalf("终态写入 %d 次, want 1", len(finished))
	}
	if finished[0].ID != started[0].ID {
		t.Errorf("终态写去了 %q，插进去的那一行是 %q", finished[0].ID, started[0].ID)
	}
	if started[0].TaskID != handle.TaskID {
		t.Errorf("开始行的 task id = %q, handle 是 %q", started[0].TaskID, handle.TaskID)
	}
}

// failingEventBus 让每一次 Publish 都失败，好把「发布失败之后写的那条审计」这条路
// 逼出来。Events 仍然照常返回已收下的（一条都没有）。
type failingEventBus struct{}

func (failingEventBus) Publish(context.Context, domain.RuntimeEvent) error {
	return errors.New("event bus is down")
}

func (failingEventBus) Events() ([]domain.RuntimeEvent, error) { return nil, nil }

// TestSubTaskAuditEventsAgreeOnWhichTaskDelegated：两条 subtask_* 审计事件的
// RequestID 必须是同一个东西——发起委派的那条父任务。
//
// 一个字段在同一条路径上有两种含义，等于这一族事件谁都回答不了「哪条任务把活派了
// 出去」，而那正是它们存在的理由（见 domain.AuditEvent.RequestID 的字段契约）。
func TestSubTaskAuditEventsAgreeOnWhichTaskDelegated(t *testing.T) {
	t.Parallel()

	audit := adapter.NewMemoryAuditLog()
	agents := &recordingDelegationAgents{
		fakeDelegationAgents: fakeDelegationAgents{names: []string{"researcher"}},
	}
	rt := NewRuntime(Config{
		Gate:             taskgate.NewTaskGate(),
		Maas:             adapter.NewRecordingMaas("OK"),
		Audit:            audit,
		Events:           failingEventBus{},
		DelegationAgents: agents,
		MaxSpawnDepth:    3,
	})
	if _, err := rt.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
		AgentID:      "researcher",
	}); err != nil {
		t.Fatalf("RunSubTaskAsync: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var failed domain.AuditEvent
	for time.Now().Before(deadline) {
		if event, ok := findAuditEvent(mustAuditEvents(t, audit), "subtask_event_publish_failed"); ok {
			failed = event
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if failed.Action == "" {
		t.Fatal("没有等到 subtask_event_publish_failed：这条路根本没被走到")
	}
	delegated, ok := findAuditEvent(mustAuditEvents(t, audit), "subtask_delegated_to_agent")
	if !ok {
		t.Fatal("没有 subtask_delegated_to_agent 事件")
	}
	if delegated.RequestID != "task-parent" {
		t.Errorf("subtask_delegated_to_agent 的 RequestID = %q, want task-parent", delegated.RequestID)
	}
	if failed.RequestID != "task-parent" {
		t.Errorf("subtask_event_publish_failed 的 RequestID = %q, want task-parent——同一族事件里这个字段有了第二种含义",
			failed.RequestID)
	}
	if failed.SubjectID == "task-parent" {
		t.Error("子任务自己的 id 没了：SubjectID 也被写成了父任务")
	}
}
