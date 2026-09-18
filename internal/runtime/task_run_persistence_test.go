package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// recordingTaskRuns 记下每一次写入，并可以让指定的那一步失败。
//
// 它对「同一条记录写第二次终态」不设防，这一点是刻意的：真实的
// storage.SQLiteRepository.FinishTaskRun 同样不设防（它按 id 更新，不看当前状态，
// 见 TestFinishTaskRunDoesNotRefuseASecondTerminalWrite）。所以「恰好落一次终态」
// 必须由 RunTask 的收口 defer 保证，假存储若替它把关，测试就测不到真实形状。
type recordingTaskRuns struct {
	mu        sync.Mutex
	started   []domain.TaskRun
	finished  []domain.TaskRun
	failStart error
	failEnd   error
}

func (r *recordingTaskRuns) StartTaskRun(_ context.Context, run domain.TaskRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failStart != nil {
		return r.failStart
	}
	r.started = append(r.started, run)
	return nil
}

func (r *recordingTaskRuns) FinishTaskRun(_ context.Context, run domain.TaskRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failEnd != nil {
		return r.failEnd
	}
	r.finished = append(r.finished, run)
	return nil
}

func (r *recordingTaskRuns) SweepRunning(context.Context, time.Time) (int, error) { return 0, nil }

func (r *recordingTaskRuns) TaskRunByID(context.Context, string) (domain.TaskRun, bool, error) {
	return domain.TaskRun{}, false, nil
}

func (r *recordingTaskRuns) ListTaskRuns(context.Context, string) ([]domain.TaskRun, error) {
	return nil, nil
}

func (r *recordingTaskRuns) snapshot() (started, finished []domain.TaskRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.TaskRun(nil), r.started...), append([]domain.TaskRun(nil), r.finished...)
}

var _ port.TaskRunStore = (*recordingTaskRuns)(nil)

// testAgent 是这些用例共用的直连 agent。
func testAgent() domain.Agent {
	return domain.Agent{ID: "agent-1", CompanyID: "company-1", Role: "developer", Status: domain.AgentActive}
}

// testTask 是这些用例共用的直连任务（不是后台子任务，所以开始那一行由 RunTask 自己写）。
func testTask(id string) domain.Task {
	return domain.Task{ID: id, CompanyID: "company-1", AgentID: "agent-1", Status: domain.TaskRunning, Input: "say OK"}
}

// newTaskRunRuntime 按这个包既有的构造方式建一个运行时：默认给一个答 answer 的
// RecordingMaas、内存审计与内存事件总线，opts 再逐条覆盖需要变的那一项。
func newTaskRunRuntime(runs port.TaskRunStore, opts ...func(*Config)) *Runtime {
	cfg := Config{
		Gate:     taskgate.NewTaskGate(),
		Maas:     adapter.NewRecordingMaas("OK"),
		Audit:    adapter.NewMemoryAuditLog(),
		Events:   adapter.NewMemoryEventBus(),
		TaskRuns: runs,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return NewRuntime(cfg)
}

// TestRunTaskRecordsAStartThenACompletion：正常一轮，落一条 running 再落一条
// completed，终态带着结果与 usage。
func TestRunTaskRecordsAStartThenACompletion(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := newTaskRunRuntime(runs)
	if _, err := rt.RunTask(context.Background(), testAgent(), testTask("task-1")); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	started, finished := runs.snapshot()
	if len(started) != 1 || started[0].Status != domain.RunStatusRunning {
		t.Fatalf("开始写入 = %+v, want 一条 running", started)
	}
	if len(finished) != 1 {
		t.Fatalf("终态写入 %d 次, want 恰好 1 次", len(finished))
	}
	if finished[0].Status != domain.RunStatusCompleted || finished[0].Result != "OK" {
		t.Errorf("终态 = %+v", finished[0])
	}
	if finished[0].ID != started[0].ID {
		t.Errorf("终态写的是另一条记录：start=%q finish=%q", started[0].ID, finished[0].ID)
	}
}

// TestRunTaskDoesNotStartWhenTheOpeningWriteFails：开始那一步写不进去，任务就不
// 开始——模型一次都不许被调用。
//
// 断言「模型没被调用」而不只是「返回了错误」：先跑后记的实现同样会返回错误，但它
// 已经把活干了一半，而那一半没有任何记录。
func TestRunTaskDoesNotStartWhenTheOpeningWriteFails(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{failStart: errors.New("disk full")}
	model := adapter.NewRecordingMaas("OK")
	rt := newTaskRunRuntime(runs, func(cfg *Config) { cfg.Maas = model })
	_, err := rt.RunTask(context.Background(), testAgent(), testTask("task-1"))
	if err == nil {
		t.Fatal("开始写入失败了，RunTask 却成功了")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("错误链里没有写库失败的原因：%v", err)
	}
	if n := model.CallCount(); n != 0 {
		t.Errorf("模型被调用了 %d 次；开始没落盘就不该开跑", n)
	}
}

// TestRunTaskFailsWhenTheClosingWriteFails：结果算出来了但终态写不进去，任务整体
// 报错。
//
// 按拍板：写库失败终止任务。返回一个「结果有了但没人知道」的成功，正是这份记录
// 存在要防的事。
func TestRunTaskFailsWhenTheClosingWriteFails(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{failEnd: errors.New("db is locked")}
	rt := newTaskRunRuntime(runs)
	_, err := rt.RunTask(context.Background(), testAgent(), testTask("task-1"))
	if err == nil {
		t.Fatal("终态写不进去，RunTask 却成功了")
	}
	if !strings.Contains(err.Error(), "db is locked") {
		t.Errorf("错误链里没有写库失败的原因：%v", err)
	}
}

// TestRunTaskRecordsAFailedRunOnEveryErrorExit：出错的那条路也必须落终态。
//
// 这是本任务最容易坏的地方：RunTask 有二十多处错误出口，逐条手改必然漏一条，而漏
// 掉的那条留下的是一行永远 running 的记录——下一次启动会把它扫成 interrupted，于是
// 一次真实的失败被报成了「进程没了」。
//
// 三条路各自撞在不同的出口上：模型那条停在 generateStep，事件那条停在 RunTask 顶部
// 的 task_started 发布，审计那条一直跑到 runToolLoop 末尾的 finishRun——最后一条尤其
// 要紧，它证明收口覆盖的是整个调用链，不只是 RunTask 自己那段函数体。
func TestRunTaskRecordsAFailedRunOnEveryErrorExit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opt  func(*Config)
	}{
		{"模型调用失败", func(cfg *Config) { cfg.Maas = failingMaas{} }},
		{"事件发布失败", func(cfg *Config) {
			cfg.Events = &failOnTypeBus{EventBus: adapter.NewMemoryEventBus(), failType: "task_started"}
		}},
		{"审计写入失败", func(cfg *Config) {
			cfg.Audit = writeFailingAuditLog{err: errors.New("audit sink down")}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runs := &recordingTaskRuns{}
			rt := newTaskRunRuntime(runs, tc.opt)
			if _, err := rt.RunTask(context.Background(), testAgent(), testTask("task-1")); err == nil {
				t.Fatal("这一路本该失败")
			}
			started, finished := runs.snapshot()
			if len(started) != 1 {
				t.Fatalf("开始写入 %d 次, want 1", len(started))
			}
			if len(finished) != 1 {
				t.Fatalf("终态写入 %d 次, want 恰好 1 次——出错的路径也要落终态", len(finished))
			}
			if finished[0].Status != domain.RunStatusFailed {
				t.Errorf("终态 status = %q, want failed", finished[0].Status)
			}
			if finished[0].Error == "" {
				t.Error("failed 的记录没有错误摘要，读的人无从知道它为什么失败")
			}
		})
	}
}

// TestRunTaskNeverWritesInterrupted：运行期永远不写 interrupted。
func TestRunTaskNeverWritesInterrupted(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := newTaskRunRuntime(runs, func(cfg *Config) { cfg.Maas = failingMaas{} })
	_, _ = rt.RunTask(context.Background(), testAgent(), testTask("task-1"))
	_, finished := runs.snapshot()
	for _, run := range finished {
		if run.Status == domain.RunStatusInterrupted {
			t.Fatal("运行期写出了 interrupted；那句话只有启动扫描说得出口")
		}
	}
}

// TestTheClosingWriteIsIdempotentBecauseTheStoreIsNot：「恰好一次」由收口 defer
// 保证，不是存储在替它把关。
//
// 成功路径自己写完 completed 之后，收口 defer 还会再跑一次；它不再写第二条，靠的
// 是自己的 finished 标记。这个用例把那句话钉死：先证明这个假存储（以及它模仿的真
// 存储）对同一条记录的第二次终态写入来者不拒，再证明跑完一次任务后终态只有一条。
// 少了这一层论证，「只有一条」可以是存储去重的结果，那样的话换个真存储就会冒出两条
// 冲突的终态。
func TestTheClosingWriteIsIdempotentBecauseTheStoreIsNot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runs := &recordingTaskRuns{}
	rt := newTaskRunRuntime(runs)
	if _, err := rt.RunTask(ctx, testAgent(), testTask("task-1")); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	_, finished := runs.snapshot()
	if len(finished) != 1 {
		t.Fatalf("终态写入 %d 次, want 恰好 1 次", len(finished))
	}
	// 存储这一侧没有任何门：同一条终态再写一次照样收下。
	if err := runs.FinishTaskRun(ctx, finished[0]); err != nil {
		t.Fatalf("假存储拒绝了第二次终态写入，它就不再是真存储的形状了：%v", err)
	}
	if _, again := runs.snapshot(); len(again) != 2 {
		t.Fatalf("手写的第二次终态没被记下（%d 条），这个论证就不成立了", len(again))
	}
}

// TestRunTaskWithoutATaskRunStoreStillRuns：TaskRuns 为 nil 是契约写明的合法部署
// 形态（CLI 一次性执行、绝大多数测试），此时一条记录都不写，也不因此失败。
func TestRunTaskWithoutATaskRunStoreStillRuns(t *testing.T) {
	t.Parallel()

	rt := NewRuntime(Config{
		Gate:   taskgate.NewTaskGate(),
		Maas:   adapter.NewRecordingMaas("OK"),
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
	})
	run, err := rt.RunTask(context.Background(), testAgent(), testTask("task-1"))
	if err != nil {
		t.Fatalf("没有运行记录落点的部署跑不动了：%v", err)
	}
	if run.Result != "OK" {
		t.Errorf("run.Result = %q, want %q", run.Result, "OK")
	}
}

// TestClonedSubRuntimeCarriesTheTaskRunStore：克隆出来的子运行时必须带着运行记录
// 的落点。
//
// 少了这一行，后台子任务的 RunTask 拿到的 taskRuns 是 nil，落盘在子任务这条路上
// 整条消失——而后台子任务正是这份设计要救的场景，一个不落盘的后台子任务崩掉之后
// 什么痕迹都不留。这类「接缝在，但没有东西送到那条接缝上」的漏配在本仓反复出现过，
// 所以这里直接钉住字段本身，不等下游用例替它把关。
func TestClonedSubRuntimeCarriesTheTaskRunStore(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	parent := newTaskRunRuntime(runs)
	child, err := parent.newSubRuntime(roleLeaf, nil)
	if err != nil {
		t.Fatalf("newSubRuntime: %v", err)
	}
	if child.taskRuns != port.TaskRunStore(runs) {
		t.Errorf("child.taskRuns = %v, want 父运行时那一个 %v", child.taskRuns, runs)
	}
}
