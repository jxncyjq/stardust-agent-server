package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// blockingSubMaas 让第一次推理一直阻塞到用例结束（release 由 t.Cleanup 关闭），
// 于是断言发生在子任务仍在飞的那一刻——这正是「派发时就已落盘」要证明的窗口。
//
// 它的等待是有界的：用例忘了放行时它报错退出，而不是把 goroutine 永久停住。
type blockingSubMaas struct {
	release chan struct{}
	entered chan struct{}
}

func newBlockingSubMaas(t *testing.T) *blockingSubMaas {
	t.Helper()
	m := &blockingSubMaas{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	t.Cleanup(func() { close(m.release) })
	return m
}

func (m *blockingSubMaas) Generate(context.Context, port.InferenceRequest) (port.InferenceResponse, error) {
	select {
	case m.entered <- struct{}{}:
	default:
	}
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
// 之后立刻退出就什么都没留下，而那正是本设计要覆盖的场景。模型被卡住，所以快照
// 拍的是子任务仍在飞的那一刻。
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
	if _, err := rt.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
	}); err == nil {
		t.Fatal("记录插不进去，后台子任务却起来了")
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
