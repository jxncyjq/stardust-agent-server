package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/task"
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

func (r *recordingTaskRuns) StartTaskRun(ctx context.Context, run domain.TaskRun) error {
	// 尊重 ctx：真实的 SQLite 写入走 ExecContext，ctx 一取消就失败。假存储若忽略它，
	// 「取消之后还写不写得进去」这条用例就恒绿，测不到任何东西。
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failStart != nil {
		return r.failStart
	}
	r.started = append(r.started, run)
	return nil
}

func (r *recordingTaskRuns) FinishTaskRun(ctx context.Context, run domain.TaskRun) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	// 「标识」那一半也要守：裸 return err 里同样有 "disk full"，只断言原因是放过它的。
	// 全局约束要的是 fmt.Errorf("<动作> <标识>: %w", err)——动作与是哪条任务都得在。
	if !strings.Contains(err.Error(), "record the start of task task-1") {
		t.Errorf("错误 %v 没有说清失败的是哪个动作、哪条任务", err)
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

// TestRunTaskKeepsBothCausesWhenTheClosingWriteAlsoFails：任务自己失败、终态又写
// 不进去时，返回的错误链里两条原因都要在。
//
// 这条用例守的是收口 defer 里的 errors.Join。复审实测：把它换成
// runErr = fmt.Errorf("record the end of task %s: %w", ...) 这样一条直接赋值，go vet
// 干净、整包全绿——因为在这之前唯一的守卫只断言错误串里含 "db is locked"，而那是
// 写库那条错误自带的，丢掉原始错误照样含它。
//
// 丢掉的是什么：模型超时导致任务失败、同时库被锁住导致终态写不进去，运维今天看到
// 的是「inference unavailable」加「record the end of task ...: db is locked」两条；
// 一次无意的简化之后只剩后者，任务**为什么**失败这件事从错误链里永久消失。按本仓
// fail-loud 铁律，传播时保留错误链不是风格问题。
func TestRunTaskKeepsBothCausesWhenTheClosingWriteAlsoFails(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{failEnd: errors.New("db is locked")}
	rt := newTaskRunRuntime(runs, func(cfg *Config) { cfg.Maas = failingMaas{} })
	_, err := rt.RunTask(context.Background(), testAgent(), testTask("task-1"))
	if err == nil {
		t.Fatal("模型失败、终态也写不进去，RunTask 却成功了")
	}
	if !strings.Contains(err.Error(), "inference unavailable") {
		t.Errorf("错误链里没有任务自己失败的原因（模型那条）：%v", err)
	}
	if !strings.Contains(err.Error(), "db is locked") {
		t.Errorf("错误链里没有终态写不进去的原因：%v", err)
	}
	if !strings.Contains(err.Error(), "record the end of task task-1") {
		t.Errorf("错误 %v 没有说清写失败的是哪个动作、哪条任务", err)
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
	// 没有落点也要给出这条腿的 id：返回值的形状不许随部署而变，否则同一个字段在
	// 一种部署里能查到记录、在另一种里是个编出来的字符串。
	if run.ID == "" {
		t.Error("run.ID 是空串；没有落点不等于这条腿没有 id")
	}
	if strings.Contains(run.ID, ":run-1") {
		t.Errorf("run.ID = %q 还是老形状；id 只许有一个出处", run.ID)
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

// cancellingMaas 在第一次推理时把 ctx 取消掉，模拟「任务跑到一半被用户中断」。
type cancellingMaas struct{ cancel context.CancelFunc }

func (m *cancellingMaas) Generate(context.Context, port.InferenceRequest) (port.InferenceResponse, error) {
	m.cancel()
	return port.InferenceResponse{}, context.Canceled
}

// TestRunTaskStillRecordsTheEndingWhenTheContextIsCancelled：任务跑到一半被取消，
// 终态照样落盘。
//
// 收口写入若跟着调用方的 ctx 一起被取消，那一行就永远停在 running，下一次启动把它
// 扫成 interrupted——正是这份设计要消灭的症状换了个触发条件。这仓有前科：中断路径
// 的收尾必须脱离取消。
func TestRunTaskStillRecordsTheEndingWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := newTaskRunRuntime(runs, func(cfg *Config) { cfg.Maas = &cancellingMaas{cancel: cancel} })
	if _, err := rt.RunTask(ctx, testAgent(), testTask("task-1")); err == nil {
		t.Fatal("ctx 中途被取消，RunTask 却成功了")
	}
	started, finished := runs.snapshot()
	if len(started) != 1 {
		t.Fatalf("开始写入 %d 次, want 1", len(started))
	}
	if len(finished) != 1 {
		t.Fatalf("终态写入 %d 次, want 1——取消不该让这一行停在 running", len(finished))
	}
	if finished[0].Status != domain.RunStatusFailed {
		t.Errorf("终态 status = %q, want failed", finished[0].Status)
	}
}

// TestRunTaskRecordsASuspendedEnding：挂起等审批是这条腿自己的终态，写 suspended。
//
// 写成 failed 会把「等人」说成「跑挂了」；而什么都不写会把那一行留在 running——活
// 进程里它与真正在飞的记录分不出来，重启后又被扫成 interrupted，于是一次「人批准
// 过、恢复腿跑完了」的任务留着一条「进程没了」的记录。恢复腿是另一条记录（各有
// 各的 id），所以这一行到此为止。
func TestRunTaskRecordsASuspendedEnding(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := NewRuntime(Config{
		Gate:        taskgate.NewTaskGate(),
		Maas:        &scriptedMaas{},
		Audit:       adapter.NewMemoryAuditLog(),
		Events:      adapter.NewMemoryEventBus(),
		Tools:       echoRegistry(t),
		Checkpoints: sessionstate.NewStore(t.TempDir()),
		ToolGate:    &gateOnce{},
		TaskRuns:    runs,
	})
	task := domain.Task{ID: "task-1", SessionID: "sess-1", AgentID: "agent-1", Status: domain.TaskRunning, Input: "go"}
	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "agent-1"}, task); !errors.Is(err, ErrSuspended) {
		t.Fatalf("RunTask err = %v, want ErrSuspended", err)
	}
	started, finished := runs.snapshot()
	if len(started) != 1 {
		t.Fatalf("开始写入 %d 次, want 1", len(started))
	}
	if len(finished) != 1 {
		t.Fatalf("终态写入 %d 次, want 恰好 1 次——挂起也要收尾", len(finished))
	}
	if finished[0].Status != domain.RunStatusSuspended {
		t.Errorf("终态 status = %q, want suspended", finished[0].Status)
	}
	if finished[0].ID != started[0].ID {
		t.Errorf("终态写的是另一条记录：start=%q finish=%q", started[0].ID, finished[0].ID)
	}
	if finished[0].Error != "" {
		t.Errorf("挂起那一行带了错误摘要 %q；等人不是出错", finished[0].Error)
	}
}

// TestEachRunLegGetsItsOwnID：同一个任务跑两条腿（挂起 + 恢复），两条记录的 id 不同。
//
// id 曾经硬编码成 "<任务>:run-1"，于是恢复腿会拿同一个主键再插一次——insert 冲突，
// 恢复根本起不来。而 ListTaskRuns 按 task_id 返回多条，本来就预期一个任务有多条腿。
func TestEachRunLegGetsItsOwnID(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	store := sessionstate.NewStore(t.TempDir())
	gate := taskgate.NewTaskGate()
	newLeg := func(g ToolGate) *Runtime {
		return NewRuntime(Config{
			Gate: gate, Maas: &scriptedMaas{},
			Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
			Tools: echoRegistry(t), Checkpoints: store, ToolGate: g, TaskRuns: runs,
		})
	}
	task := domain.Task{ID: "task-1", SessionID: "sess-1", AgentID: "agent-1", Status: domain.TaskRunning, Input: "go"}
	if _, err := newLeg(&gateOnce{}).RunTask(context.Background(), domain.Agent{ID: "agent-1"}, task); !errors.Is(err, ErrSuspended) {
		t.Fatalf("第一条腿 err = %v, want ErrSuspended", err)
	}
	if _, err := newLeg(allowAllGate{}).RunTask(context.Background(), domain.Agent{ID: "agent-1"}, task); err != nil {
		t.Fatalf("恢复腿: %v", err)
	}
	started, _ := runs.snapshot()
	if len(started) != 2 {
		t.Fatalf("开始写入 %d 次, want 2（挂起一条、恢复一条）", len(started))
	}
	if started[0].ID == started[1].ID {
		t.Errorf("两条腿共用同一个 run id %q；恢复腿会撞主键", started[0].ID)
	}
	for _, run := range started {
		if run.TaskID != "task-1" {
			t.Errorf("run %q 的 task id = %q", run.ID, run.TaskID)
		}
	}
}

// TestARecoveredSuspendedSubTaskKeepsItsParentage：挂起的后台子任务活过一次重启
// 之后，恢复腿写下的那一行仍然带着 parent_task_id / background / goal。
//
// 这条路整条都是可达的：具名委派同时接了 Checkpoints 与 ToolGate，而
// ManualToolGate 的插件征询那半边在任何 Mode 下都跑（runChild 造的子任务 Mode 是
// 空串）。于是一条具名后台子任务调用插件工具触发征询就会挂起。
//
// 断的地方在 RecoverSuspended：它从检查点重建 domain.Task 时只带
// ID/AgentID/SessionKey/Status/Mode/WorkingDir，这三个字段一个都不带。恢复腿因此
// 写出一行 parent_task_id=” / background=0 / goal=” —— 一条按自己的字段契约自称
// 「直连任务」的孤儿行。父子树对这条任务断掉，而且不报任何错。规格第三节写的是
// 「父子关系从此存在列里」，这条路径违反它。
//
// 这里刻意**不**带 RunID 过河：恢复腿是另一条腿，它该有自己的 run id 与自己的行。
// 带过去会让它去收尾上一条腿早已写成 suspended 的那一行。
func TestARecoveredSuspendedSubTaskKeepsItsParentage(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	checkpoints := sessionstate.NewStore(t.TempDir())
	gate := taskgate.NewTaskGate()
	newLeg := func(g ToolGate) *Runtime {
		return NewRuntime(Config{
			Gate: gate, Maas: &scriptedMaas{},
			Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
			Tools: echoRegistry(t), Checkpoints: checkpoints, ToolGate: g, TaskRuns: runs,
		})
	}
	const (
		parentID = "task-parent"
		goal     = "把 A 查清楚"
	)
	child := domain.Task{
		ID: "task-child", SessionID: "sess-1", AgentID: "agent-1",
		Status: domain.TaskRunning, Input: "go",
		ParentTaskID: parentID, Background: true, Goal: goal,
	}
	ctx := context.Background()
	if _, err := newLeg(&gateOnce{}).RunTask(ctx, domain.Agent{ID: "agent-1"}, child); !errors.Is(err, ErrSuspended) {
		t.Fatalf("第一条腿 err = %v, want ErrSuspended", err)
	}

	// 重启：进程里那份 domain.Task 没了，恢复腿知道的全部只有盘上那份检查点。
	suspended, err := checkpoints.ListSuspended()
	if err != nil {
		t.Fatalf("ListSuspended: %v", err)
	}
	scheduler := task.NewScheduler()
	coordinator := newTestCoordinator(t, scheduler, 4)
	if n, err := coordinator.RecoverSuspended(ctx, suspended); err != nil || n != 1 {
		t.Fatalf("RecoverSuspended = %d, %v; want 1, nil", n, err)
	}
	recovered, found, err := scheduler.Get(ctx, "task-child")
	if err != nil || !found {
		t.Fatalf("scheduler.Get(task-child) = %+v, %v, %v", recovered, found, err)
	}
	if _, err := newLeg(allowAllGate{}).RunTask(ctx, domain.Agent{ID: "agent-1"}, recovered); err != nil {
		t.Fatalf("恢复腿: %v", err)
	}

	started, finished := runs.snapshot()
	if len(started) != 2 {
		t.Fatalf("开始写入 %d 条, want 2（挂起一条、恢复一条）", len(started))
	}
	if len(finished) != 2 {
		t.Fatalf("终态写入 %d 条, want 2", len(finished))
	}
	if started[0].ID == started[1].ID {
		t.Errorf("两条腿共用同一个 run id %q；恢复腿该有自己的行", started[0].ID)
	}
	for _, run := range []domain.TaskRun{started[1], finished[1]} {
		if run.ParentTaskID != parentID {
			t.Errorf("恢复腿那一行的 parent_task_id = %q, want %q——它冒充了一条直连任务", run.ParentTaskID, parentID)
		}
		if !run.Background {
			t.Errorf("恢复腿那一行的 background = false, want true")
		}
		if run.Goal != goal {
			t.Errorf("恢复腿那一行的 goal = %q, want %q", run.Goal, goal)
		}
	}
}

// TestBackgroundRunFinishesTheRunIDItWasGiven：后台子任务的终态要写回派发方开的
// 那一行，不是 RunTask 另起的一条。
//
// 后台子任务的开始行由派发那一刻写下（那是父任务确凿还在飞的唯一时刻），RunTask
// 不写第二次。run id 因此必须由 task.RunID 带进来：让 RunTask 自己 mint 一个，收口
// 就会拿一个从没插过库的 id 去 FinishTaskRun——真实存储在这里硬失败（no such run
// was started），而派发方插的那一行没有任何人再碰它，永远停在 running。
func TestBackgroundRunFinishesTheRunIDItWasGiven(t *testing.T) {
	t.Parallel()

	const dispatched = "dispatched-run-1"
	runs := &recordingTaskRuns{}
	// 派发方在派发那一刻已经把开始行写下了。
	opening := testTask("task-bg")
	runs.started = append(runs.started, domain.TaskRun{
		ID: dispatched, TaskID: opening.ID, AgentID: "agent-1",
		StartedAt: time.Now(), Status: domain.RunStatusRunning, Background: true,
	})

	task := testTask("task-bg")
	task.Background = true
	task.RunID = dispatched
	rt := newTaskRunRuntime(runs)
	if _, err := rt.RunTask(context.Background(), testAgent(), task); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	started, finished := runs.snapshot()
	if len(started) != 1 {
		t.Fatalf("开始写入 %d 条, want 1——后台子任务的开始行不由 RunTask 写", len(started))
	}
	if len(finished) != 1 {
		t.Fatalf("终态写入 %d 次, want 恰好 1 次", len(finished))
	}
	if finished[0].ID != dispatched {
		t.Errorf("终态写去了 %q，而派发方开的那一行是 %q——那一行永远停在 running",
			finished[0].ID, dispatched)
	}
}

// TestRunTaskReturnsTheIDItPersisted：返回给调用方的 run.ID 就是落盘那一行的 id。
//
// 两者一旦分叉不会有任何报错：接口把 run.ID 回给前端，前端拿它查 TaskRunByID 只会
// 得到 found=false。id 只许有一个出处。
func TestRunTaskReturnsTheIDItPersisted(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := newTaskRunRuntime(runs)
	run, err := rt.RunTask(context.Background(), testAgent(), testTask("task-1"))
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	started, finished := runs.snapshot()
	if len(started) != 1 || len(finished) != 1 {
		t.Fatalf("写入 %d 开始 / %d 终态, want 1 / 1", len(started), len(finished))
	}
	if run.ID != started[0].ID {
		t.Errorf("返回的 run.ID = %q，落盘那一行是 %q", run.ID, started[0].ID)
	}
	if run.ID != finished[0].ID {
		t.Errorf("返回的 run.ID = %q，终态那一行是 %q", run.ID, finished[0].ID)
	}
}

// blockingTaskRuns 让前 blockFinishes 次终态写入一直阻塞到它自己的 ctx 结束。
//
// 它模仿的是「连接池只有一条连接、而这条连接被别人占着」：database/sql 取连接的
// 等待没有别的逃生口，只有 ctx。
type blockingTaskRuns struct {
	recordingTaskRuns
	blockFinishes int
}

func (b *blockingTaskRuns) FinishTaskRun(ctx context.Context, run domain.TaskRun) error {
	b.mu.Lock()
	blocked := b.blockFinishes > 0
	if blocked {
		b.blockFinishes--
	}
	b.mu.Unlock()
	if blocked {
		<-ctx.Done()
		return fmt.Errorf("finish task run %q: %w", run.ID, ctx.Err())
	}
	return b.recordingTaskRuns.FinishTaskRun(ctx, run)
}

var _ port.TaskRunStore = (*blockingTaskRuns)(nil)

// TestRunTaskFailsWhenTheClosingWriteOverrunsItsBudget：收尾写入卡住时，RunTask
// 带着一条说明「这次运行的结束没能记上」的错误返回，而不是挂死。
//
// 收尾脱离了调用方的取消（否则用户一中断，那一行就永远停在 running），于是它自己
// 必须有个预算：没有预算的话，一次卡住的收尾写入会同时卡住这条任务的 goroutine
// （用户中断也叫不醒它）和所有等这条任务边界的插件 apply。
//
// 两条收尾路径各测一次：成功那条走 completeRun，出错那条走收口 defer。
func TestRunTaskFailsWhenTheClosingWriteOverrunsItsBudget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		taskID  string
		opt     func(*Config)
		wantMsg string
	}{
		// 只阻塞第一次：completeRun 卡满预算后失败，收口 defer 随即补写 failed
		// （第二次不再阻塞），用例因此只等一个预算。
		{"成功路径的收尾", "budget-complete", nil, "record the completion of task budget-complete"},
		{"出错路径的收尾", "budget-fail", func(cfg *Config) { cfg.Maas = failingMaas{} }, "record the end of task budget-fail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runs := &blockingTaskRuns{blockFinishes: 1}
			opts := []func(*Config){}
			if tc.opt != nil {
				opts = append(opts, tc.opt)
			}
			rt := newTaskRunRuntime(runs, opts...)
			done := make(chan error, 1)
			go func() {
				_, err := rt.RunTask(context.Background(), testAgent(), testTask(tc.taskID))
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("收尾写入卡满了预算，RunTask 却成功了")
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("错误链里没有超时：%v", err)
				}
				if !strings.Contains(err.Error(), tc.wantMsg) {
					t.Errorf("错误没说清是哪一次写入没记上：%v", err)
				}
			case <-time.After(closingWriteBudget + 10*time.Second):
				t.Fatal("RunTask 挂死了：收尾写入没有预算")
			}
		})
	}
}

// TestFinishRunLeavesTheRunIDToItsCaller：finishRun 组装成功的 TaskRun 时不填 id。
//
// 这条腿的 run id 只有 RunTask 知道（开始那一行是它插的，或是派发方经 task.RunID
// 交给它的）。finishRun 在这里另编一个，就又多出一种 id 形状；今天 completeRun 会把
// 它盖掉，所以盖不掉的那天——比如有人让 finishRun 的返回值走别的出口——才会发现。
func TestFinishRunLeavesTheRunIDToItsCaller(t *testing.T) {
	t.Parallel()

	rt := NewRuntime(Config{
		Gate:   taskgate.NewTaskGate(),
		Maas:   adapter.NewRecordingMaas("OK"),
		Audit:  adapter.NewMemoryAuditLog(),
		Events: adapter.NewMemoryEventBus(),
	})
	run, err := rt.finishRun(context.Background(), "req-1", domain.Agent{ID: "agent-1"},
		domain.Task{ID: "task-1"},
		loopState{started: time.Now(), stopReason: domain.StopReasonCompleted})
	if err != nil {
		t.Fatalf("finishRun: %v", err)
	}
	if run.ID != "" {
		t.Errorf("finishRun 自己编了一个 run id %q；id 只许有一个出处", run.ID)
	}
}

// TestOnlyRunIDDecidesWhoWritesTheOpeningRow：只设一半的两个方向都不许出洞。
//
// 「谁写开始行」与「用哪个 id 收尾」必须读同一个字段。读 Background 的话：派发方
// 漏设 RunID，RunTask 会再插一行、把终态写去一个没人插过的 id，派发方那一行永远
// 停在 running；漏设 Background，RunTask 会拿派发方的 id 再插一次、撞主键让整条
// 子任务起不来。两个方向都用真实形状的假存储（主键唯一、结束时找不到行即报错）考。
func TestOnlyRunIDDecidesWhoWritesTheOpeningRow(t *testing.T) {
	t.Parallel()

	t.Run("设了 RunID 没设 Background", func(t *testing.T) {
		t.Parallel()

		runs := newStrictTaskRuns()
		if err := runs.StartTaskRun(context.Background(), domain.TaskRun{
			ID: "dispatched-1", TaskID: "task-1", AgentID: "agent-1",
			StartedAt: time.Now(), Status: domain.RunStatusRunning,
		}); err != nil {
			t.Fatalf("派发方插行: %v", err)
		}
		task := testTask("task-1")
		task.RunID = "dispatched-1"
		rt := newTaskRunRuntime(runs)
		if _, err := rt.RunTask(context.Background(), testAgent(), task); err != nil {
			t.Fatalf("RunTask: %v", err)
		}
		row, ok := runs.row("dispatched-1")
		if !ok {
			t.Fatal("派发方那一行不见了")
		}
		if row.Status != domain.RunStatusCompleted {
			t.Errorf("派发方那一行的 status = %q, want completed", row.Status)
		}
		if n := runs.count(); n != 1 {
			t.Errorf("库里 %d 行, want 1——RunTask 又插了一行", n)
		}
	})

	t.Run("设了 Background 没设 RunID", func(t *testing.T) {
		t.Parallel()

		runs := newStrictTaskRuns()
		task := testTask("task-2")
		task.Background = true
		rt := newTaskRunRuntime(runs)
		if _, err := rt.RunTask(context.Background(), testAgent(), task); err != nil {
			t.Fatalf("RunTask: %v", err)
		}
		if n := runs.count(); n != 1 {
			t.Fatalf("库里 %d 行, want 1", n)
		}
		for _, row := range runs.rows() {
			if row.Status == domain.RunStatusRunning {
				t.Errorf("run %q 停在 running——没人写过它的开始行，收尾却写去了别处", row.ID)
			}
		}
	})
}

// TestASuccessfulRunSurvivesACancelAtTheFinishLine：模型答完之后立刻被取消，那一次
// 运行仍然记成 completed。
//
// 成功那条路的收尾若跟着取消一起失败，finished 就留在 false，收口 defer 会把它改写
// 成 failed——一次真正跑完的运行被记成失败，而且没有任何东西会报错。
func TestASuccessfulRunSurvivesACancelAtTheFinishLine(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 事件与审计换成忽略 ctx 的版本：内存实现同样尊重取消，不换的话这条用例会先
	// 撞在事件发布上，断言就压不到收尾写入这条规则上。
	rt := newTaskRunRuntime(runs, func(cfg *Config) {
		cfg.Maas = &cancelAfterAnswerMaas{cancel: cancel}
		cfg.Events = ctxIgnoringEvents{adapter.NewMemoryEventBus()}
		cfg.Audit = ctxIgnoringAudit{adapter.NewMemoryAuditLog()}
	})
	if _, err := rt.RunTask(ctx, testAgent(), testTask("task-1")); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	_, finished := runs.snapshot()
	if len(finished) != 1 {
		t.Fatalf("终态写入 %d 次, want 1", len(finished))
	}
	if finished[0].Status != domain.RunStatusCompleted {
		t.Errorf("终态 status = %q, want completed——它确实跑完了", finished[0].Status)
	}
}

// cancelAfterAnswerMaas 先给出答案，再把 ctx 取消掉：模型答完、用户随即中断的时序。
type cancelAfterAnswerMaas struct{ cancel context.CancelFunc }

func (m *cancelAfterAnswerMaas) Generate(context.Context, port.InferenceRequest) (port.InferenceResponse, error) {
	defer m.cancel()
	return port.InferenceResponse{Text: "OK"}, nil
}

// TestTerminalRowsCarryAnEndedAt：终态行必须带结束时刻。
//
// suspended 行的 ended_at 就是挂起时刻，「挂了多久」「按时间排序」都读它；failed 行
// 没有它就排不了序也算不了时长。
func TestTerminalRowsCarryAnEndedAt(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := newTaskRunRuntime(runs, func(cfg *Config) { cfg.Maas = failingMaas{} })
	if _, err := rt.RunTask(context.Background(), testAgent(), testTask("task-1")); err == nil {
		t.Fatal("模型必败，RunTask 却成功了")
	}
	_, finished := runs.snapshot()
	if len(finished) != 1 {
		t.Fatalf("终态写入 %d 次, want 1", len(finished))
	}
	if finished[0].EndedAt.IsZero() {
		t.Error("终态行没有 ended_at")
	}
}

// strictTaskRuns 是一个按真实 SQLite 那两条硬规则办事的假存储：主键唯一，结束一条
// 没插过的记录即报错。
//
// recordingTaskRuns 刻意不设防（好让「恰好落一次终态」只能由 RunTask 保证），但
// 「谁写开始行」这条规则只有在插重复主键真的会失败时才考得出来。
type strictTaskRuns struct {
	mu   sync.Mutex
	byID map[string]domain.TaskRun
}

func newStrictTaskRuns() *strictTaskRuns {
	return &strictTaskRuns{byID: map[string]domain.TaskRun{}}
}

func (s *strictTaskRuns) StartTaskRun(ctx context.Context, run domain.TaskRun) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.byID[run.ID]; dup {
		return fmt.Errorf("start task run %q: UNIQUE constraint failed: task_runs.id", run.ID)
	}
	s.byID[run.ID] = run
	return nil
}

func (s *strictTaskRuns) FinishTaskRun(ctx context.Context, run domain.TaskRun) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[run.ID]
	if !ok {
		return fmt.Errorf("finish task run %q: no such run was started", run.ID)
	}
	existing.Status = run.Status
	existing.Result = run.Result
	existing.Error = run.Error
	existing.EndedAt = run.EndedAt
	s.byID[run.ID] = existing
	return nil
}

func (s *strictTaskRuns) SweepRunning(context.Context, time.Time) (int, error) { return 0, nil }

func (s *strictTaskRuns) TaskRunByID(_ context.Context, id string) (domain.TaskRun, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.byID[id]
	return run, ok, nil
}

func (s *strictTaskRuns) ListTaskRuns(context.Context, string) ([]domain.TaskRun, error) {
	return nil, nil
}

func (s *strictTaskRuns) row(id string) (domain.TaskRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.byID[id]
	return run, ok
}

func (s *strictTaskRuns) rows() []domain.TaskRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.TaskRun, 0, len(s.byID))
	for _, run := range s.byID {
		out = append(out, run)
	}
	return out
}

func (s *strictTaskRuns) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

var _ port.TaskRunStore = (*strictTaskRuns)(nil)

// ctxIgnoringEvents / ctxIgnoringAudit 把 ctx 挡在外面，好让「取消之后还剩哪条路会
// 失败」这个问题只剩收尾写入一个答案。
type ctxIgnoringEvents struct{ port.EventBus }

func (e ctxIgnoringEvents) Publish(_ context.Context, event domain.RuntimeEvent) error {
	return e.EventBus.Publish(context.WithoutCancel(context.Background()), event)
}

type ctxIgnoringAudit struct{ port.AuditLog }

func (a ctxIgnoringAudit) Append(_ context.Context, event domain.AuditEvent) error {
	return a.AuditLog.Append(context.Background(), event)
}
