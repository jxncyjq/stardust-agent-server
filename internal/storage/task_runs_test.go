package storage

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// newTaskRunRepo 开一个临时库。落盘用临时文件而不是内存库，因为迁移与重开是这一
// 组用例要考的东西。
func newTaskRunRepo(t *testing.T) *SQLiteRepository {
	t.Helper()
	repo, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	return repo
}

// fullTaskRun 造一条**每个字段都非零**的运行记录。字段全满是这条用例的全部意义：
// 表曾经只有 7 列而 TaskRun 有 12 个字段，多出来的那些写进去就丢，而丢的方式是
// 静默的。assertEveryFieldIsSet 把「全满」从人的纪律变成一条断言。
//
// 它同时带着 completed 与非空 Error，这一点与 domain.TaskRun.Error 的字段契约
// （「Status 为 failed 时的错误摘要，其余状态为空」）冲突，是故意的：这条夹具回答的
// 是「每一列都往返得了吗」，不是「这条记录在业务上讲不讲得通」。存储层不解释这两列
// 的关系，它只负责把写进去的东西原样读回来；契约那一半由运行时那边的用例守。
func fullTaskRun() domain.TaskRun {
	return domain.TaskRun{
		ID:               "run-1",
		TaskID:           "task-1",
		AgentID:          "agent-1",
		StartedAt:        time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC),
		EndedAt:          time.Date(2026, 9, 18, 10, 1, 0, 0, time.UTC),
		Result:           "答案",
		StopReason:       domain.StopReasonCompleted,
		ReasoningSummary: "想了想",
		PromptTokens:     11,
		CompletionTokens: 22,
		CachedTokens:     33,
		TotalTokens:      66,
		GeneratedFiles:   []string{"out/a.md", "out/b.md"},
		Status:           domain.RunStatusCompleted,
		ParentTaskID:     "task-parent",
		Background:       true,
		Goal:             "把 A 查清楚",
		Error:            "上一次尝试报的错",
	}
}

// assertEveryFieldIsSet 断言这条记录的每个导出字段都不是零值。
//
// 它守的是「TaskRun 每加一个字段，就要同时加一条列迁移」这条不变量。往返断言用
// reflect.DeepEqual 比整个结构体，看起来已经够硬，但它比的是 fullTaskRun 给出的
// 那些字段，而 fullTaskRun 是手工维护的：复审实测给 domain.TaskRun 加一个没有列
// 的新字段，go vet 干净、全仓测试全绿——写进去丢、读回来是零值，正是这份设计开篇
// 描述的、已经发生过一次的那个缺陷原样复现。
//
// 反射是这里唯一说得出「每个字段」的写法：新字段一出现就是零值，这条断言先逼人
// 把它补进 fullTaskRun，补进去之后 DeepEqual 才有机会抓到缺的那一列。
func assertEveryFieldIsSet(t *testing.T, run domain.TaskRun) {
	t.Helper()
	value := reflect.ValueOf(run)
	for i := range value.NumField() {
		field := value.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		if value.Field(i).IsZero() {
			t.Fatalf("domain.TaskRun.%s 在 fullTaskRun 里还是零值：新增字段必须同时补两处——"+
				"internal/storage/sqlite.go 的 columnMigrations 里加一条列迁移（并把它加进 "+
				"SaveTaskRun/StartTaskRun/FinishTaskRun/ListTaskRuns 的列清单），以及本文件 "+
				"fullTaskRun 里给它一个非零取值。少了前者这个字段写进去就丢、读回来是零值，"+
				"而少了后者这件事没有任何测试会红。", field.Name)
		}
	}
}

// TestTaskRunRoundTripsEveryField：写进去的每个字段都要读得回来。
func TestTaskRunRoundTripsEveryField(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	ctx := context.Background()
	want := fullTaskRun()
	// 先确认夹具本身是满的：字段没进夹具，下面的 DeepEqual 就看不见它缺列。
	assertEveryFieldIsSet(t, want)
	if err := repo.SaveTaskRun(ctx, want); err != nil {
		t.Fatalf("SaveTaskRun: %v", err)
	}
	runs, err := repo.ListTaskRuns(ctx, want.TaskID)
	if err != nil {
		t.Fatalf("ListTaskRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListTaskRuns 返回 %d 条，want 1", len(runs))
	}
	// 用 reflect.DeepEqual 而不是 go-cmp：这仓没有 go-cmp 依赖，为一条断言引进一个
	// 新依赖不值得。时间字段先归一到 UTC 再比——parseTime 读回来的是 UTC。
	want.StartedAt = want.StartedAt.UTC()
	want.EndedAt = want.EndedAt.UTC()
	got := runs[0]
	got.StartedAt = got.StartedAt.UTC()
	got.EndedAt = got.EndedAt.UTC()
	if !reflect.DeepEqual(want, got) {
		t.Errorf("往返之后字段不一致：\nwant %+v\ngot  %+v", want, got)
	}
}

// TestTaskRunKeepsAFailedRunsError：failed 那条路的错误摘要也要往返。
func TestTaskRunKeepsAFailedRunsError(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	ctx := context.Background()
	run := fullTaskRun()
	run.Status = domain.RunStatusFailed
	run.Error = "模型调用超时"
	run.Result = ""
	if err := repo.SaveTaskRun(ctx, run); err != nil {
		t.Fatalf("SaveTaskRun: %v", err)
	}
	runs, err := repo.ListTaskRuns(ctx, run.TaskID)
	if err != nil {
		t.Fatalf("ListTaskRuns: %v", err)
	}
	if runs[0].Error != "模型调用超时" || runs[0].Status != domain.RunStatusFailed {
		t.Errorf("读回 status=%q error=%q", runs[0].Status, runs[0].Error)
	}
}

// TestTaskRunRefusesAnUnknownStatusOnRead：库里那一列被手改成不认得的值时，读要
// 报错而不是把它当成某个状态。
func TestTaskRunRefusesAnUnknownStatusOnRead(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	ctx := context.Background()
	if err := repo.SaveTaskRun(ctx, fullTaskRun()); err != nil {
		t.Fatalf("SaveTaskRun: %v", err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE task_runs SET status = 'weird'`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := repo.ListTaskRuns(ctx, "task-1"); err == nil {
		t.Fatal("库里一个不认得的 status 被静默读了过去")
	}
}

// TestColumnMigrationsAreIdempotent：迁移连跑两次不报错，老行的 status 落成
// completed。
//
// 老行都带着 ended_at，本来就是已结束的运行；默认成 running 会让第一次启动扫描把
// 全部历史数据标成中断。
func TestColumnMigrationsAreIdempotent(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	ctx := context.Background()
	// 用一条不带新列的插入模拟老数据。
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO task_runs (id, task_id, agent_id, started_at, ended_at, result)
		VALUES ('old-1', 'task-old', 'agent-old', ?, ?, '旧答案')
	`, formatTime(time.Now().UTC()), formatTime(time.Now().UTC())); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	if err := repo.applyColumnMigrations(ctx); err != nil {
		t.Fatalf("applyColumnMigrations（第二次）: %v", err)
	}
	runs, err := repo.ListTaskRuns(ctx, "task-old")
	if err != nil {
		t.Fatalf("ListTaskRuns: %v", err)
	}
	if runs[0].Status != domain.RunStatusCompleted {
		t.Errorf("老行的 status = %q, want completed", runs[0].Status)
	}
}

// TestTaskRunRefusesACorruptedGeneratedFilesColumn：生成文件清单读不懂时报错，不
// 退化成空清单。
//
// 复审实测：把 unmarshalGeneratedFiles 的解码错误改成吞掉返回 nil，go vet 干净、
// 全仓测试全绿——这条 fail-loud 规则此前没有任何用例守着。读成「这次运行没有生成
// 文件」与「这一列坏了」是两句完全不同的话。
func TestTaskRunRefusesACorruptedGeneratedFilesColumn(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	ctx := context.Background()
	if err := repo.SaveTaskRun(ctx, fullTaskRun()); err != nil {
		t.Fatalf("SaveTaskRun: %v", err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE task_runs SET generated_files = 'not-json'`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if runs, err := repo.ListTaskRuns(ctx, "task-1"); err == nil {
		t.Fatalf("坏掉的 generated_files 被读成了 %v", runs[0].GeneratedFiles)
	}
}

// TestFinishTaskRunDoesNotOverwriteTheOpeningFields：字段级写回，不整行覆盖。
//
// 结束时手里那份 TaskRun 是这一次运行自己组装的，它不必知道开始时写下的 goal、
// parent_task_id、background、started_at 是什么。让一次结束写去重申它们，等于给
// 「结束时那份不全的快照」一个覆盖开头的机会——这仓在任务状态落盘那次就是这么丢
// 过数据的。
func TestFinishTaskRunDoesNotOverwriteTheOpeningFields(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	ctx := context.Background()
	opening := domain.TaskRun{
		ID:           "run-1",
		TaskID:       "task-1",
		AgentID:      "agent-1",
		StartedAt:    time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC),
		Status:       domain.RunStatusRunning,
		ParentTaskID: "task-parent",
		Background:   true,
		Goal:         "把 A 查清楚",
	}
	if err := repo.StartTaskRun(ctx, opening); err != nil {
		t.Fatalf("StartTaskRun: %v", err)
	}
	// 结束时那份只带终态字段，开头那几个一律留空。
	if err := repo.FinishTaskRun(ctx, domain.TaskRun{
		ID:             "run-1",
		EndedAt:        time.Date(2026, 9, 18, 10, 1, 0, 0, time.UTC),
		Result:         "答案",
		StopReason:     domain.StopReasonCompleted,
		Status:         domain.RunStatusCompleted,
		TotalTokens:    66,
		GeneratedFiles: []string{"out/a.md"},
	}); err != nil {
		t.Fatalf("FinishTaskRun: %v", err)
	}

	got, found, err := repo.TaskRunByID(ctx, "run-1")
	if err != nil || !found {
		t.Fatalf("TaskRunByID = %v, %v, %v", got, found, err)
	}
	if got.Goal != "把 A 查清楚" || got.ParentTaskID != "task-parent" || !got.Background {
		t.Errorf("结束写回抹掉了开头写下的字段：goal=%q parent=%q background=%v",
			got.Goal, got.ParentTaskID, got.Background)
	}
	if !got.StartedAt.Equal(opening.StartedAt) {
		t.Errorf("started_at 被结束写回改成了 %s", got.StartedAt)
	}
	if got.Status != domain.RunStatusCompleted || got.Result != "答案" || got.TotalTokens != 66 {
		t.Errorf("终态没写进去：%+v", got)
	}
}

// TestFinishTaskRunRefusesAnUnknownRun：要结束一条不存在的记录是接线错了，不是
// 一次可以忽略的空操作。悄悄成功会让「开始那一步没落盘」这件事永远没人发现。
func TestFinishTaskRunRefusesAnUnknownRun(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	err := repo.FinishTaskRun(context.Background(), domain.TaskRun{
		ID:     "never-started",
		Status: domain.RunStatusCompleted,
	})
	if err == nil {
		t.Fatal("结束一条不存在的运行记录却成功了")
	}
}

// TestFinishTaskRunRefusesInterrupted：interrupted 只能由启动扫描写。
//
// 运行期的代码永远不处在能说「没人知道它跑到哪」这句话的位置上：它自己就在那里跑。
func TestFinishTaskRunRefusesInterrupted(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	ctx := context.Background()
	if err := repo.StartTaskRun(ctx, domain.TaskRun{
		ID: "run-1", TaskID: "task-1", AgentID: "a", StartedAt: time.Now().UTC(),
		Status: domain.RunStatusRunning,
	}); err != nil {
		t.Fatalf("StartTaskRun: %v", err)
	}
	if err := repo.FinishTaskRun(ctx, domain.TaskRun{ID: "run-1", Status: domain.RunStatusInterrupted}); err == nil {
		t.Fatal("运行期代码把一条记录写成了 interrupted")
	}
}

// TestFinishTaskRunAcceptsSuspended：挂起等审批是这条腿的终态，收尾写得进去。
//
// 挂起以前不写终态，于是那一行永远停在 running：活进程里它与真正在飞的记录一个字节
// 都分不出来，重启后又被扫成 interrupted——一次「人批准过、恢复腿跑完了」的任务却
// 留着一条「进程没了」的记录。
func TestFinishTaskRunAcceptsSuspended(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	ctx := context.Background()
	if err := repo.StartTaskRun(ctx, domain.TaskRun{
		ID: "run-1", TaskID: "task-1", AgentID: "a", StartedAt: time.Now().UTC(),
		Status: domain.RunStatusRunning,
	}); err != nil {
		t.Fatalf("StartTaskRun: %v", err)
	}
	endedAt := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	if err := repo.FinishTaskRun(ctx, domain.TaskRun{
		ID: "run-1", Status: domain.RunStatusSuspended, EndedAt: endedAt,
	}); err != nil {
		t.Fatalf("FinishTaskRun(suspended): %v", err)
	}
	run, found, err := repo.TaskRunByID(ctx, "run-1")
	if err != nil {
		t.Fatalf("TaskRunByID: %v", err)
	}
	if !found {
		t.Fatal("挂起收尾之后记录不见了")
	}
	if run.Status != domain.RunStatusSuspended {
		t.Errorf("status = %q, want suspended", run.Status)
	}
}

// TestSweepRunningLeavesSuspendedRowsAlone：启动扫描一行 suspended 都不许动。
//
// 扫描今天按 status='running' 过滤，所以这条自然成立——这个用例钉的正是「以后有人
// 把那个过滤条件放宽」。挂起的腿是自己收的尾，把它改写成 interrupted 等于拿「进程
// 没了」覆盖掉一件确凿发生过的事。
func TestSweepRunningLeavesSuspendedRowsAlone(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	ctx := context.Background()
	for _, id := range []string{"r1", "r2"} {
		if err := repo.StartTaskRun(ctx, domain.TaskRun{
			ID: id, TaskID: "t", AgentID: "a", StartedAt: time.Now().UTC(),
			Status: domain.RunStatusRunning,
		}); err != nil {
			t.Fatalf("StartTaskRun(%s): %v", id, err)
		}
	}
	suspendedAt := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	if err := repo.FinishTaskRun(ctx, domain.TaskRun{
		ID: "r2", Status: domain.RunStatusSuspended, EndedAt: suspendedAt,
	}); err != nil {
		t.Fatalf("FinishTaskRun(suspended): %v", err)
	}

	n, err := repo.SweepRunning(ctx, time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SweepRunning: %v", err)
	}
	if n != 1 {
		t.Errorf("SweepRunning 报了 %d 条, want 1——只有那条 running 该被扫", n)
	}
	run, found, err := repo.TaskRunByID(ctx, "r2")
	if err != nil {
		t.Fatalf("TaskRunByID: %v", err)
	}
	if !found {
		t.Fatal("挂起那一行不见了")
	}
	if run.Status != domain.RunStatusSuspended {
		t.Errorf("挂起那一行被扫成了 %q；它是自己收的尾，不是进程没了", run.Status)
	}
	if !run.EndedAt.Equal(suspendedAt) {
		t.Errorf("挂起那一行的 ended_at 被改成了 %s, want %s", run.EndedAt, suspendedAt)
	}
}

// TestStartTaskRunRefusesANonRunningStatus：开始那一步只能写 running。
func TestStartTaskRunRefusesANonRunningStatus(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	err := repo.StartTaskRun(context.Background(), domain.TaskRun{
		ID: "run-1", TaskID: "task-1", AgentID: "a", StartedAt: time.Now().UTC(),
		Status: domain.RunStatusCompleted,
	})
	if err == nil {
		t.Fatal("开始那一步写了一个不是 running 的状态")
	}
}

// TestSweepRunningOnlyTouchesRunningRows：扫描把 running 改成 interrupted，别的
// 状态一行都不许动。
func TestSweepRunningOnlyTouchesRunningRows(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	ctx := context.Background()
	for _, run := range []domain.TaskRun{
		{ID: "r1", TaskID: "t", AgentID: "a", StartedAt: time.Now().UTC(), Status: domain.RunStatusRunning},
		{ID: "r2", TaskID: "t", AgentID: "a", StartedAt: time.Now().UTC(), Status: domain.RunStatusRunning},
	} {
		if err := repo.StartTaskRun(ctx, run); err != nil {
			t.Fatalf("StartTaskRun(%s): %v", run.ID, err)
		}
	}
	done := fullTaskRun()
	done.ID = "r3"
	done.TaskID = "t"
	if err := repo.SaveTaskRun(ctx, done); err != nil {
		t.Fatalf("SaveTaskRun: %v", err)
	}

	sweptAt := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	n, err := repo.SweepRunning(ctx, sweptAt)
	if err != nil {
		t.Fatalf("SweepRunning: %v", err)
	}
	if n != 2 {
		t.Errorf("SweepRunning 报了 %d 条, want 2", n)
	}
	runs, err := repo.ListTaskRuns(ctx, "t")
	if err != nil {
		t.Fatalf("ListTaskRuns: %v", err)
	}
	for _, run := range runs {
		switch run.ID {
		case "r1", "r2":
			if run.Status != domain.RunStatusInterrupted {
				t.Errorf("%s 的 status = %q, want interrupted", run.ID, run.Status)
			}
			if !run.EndedAt.Equal(sweptAt) {
				t.Errorf("%s 的 ended_at = %s, want 扫描时刻 %s", run.ID, run.EndedAt, sweptAt)
			}
		case "r3":
			if run.Status != domain.RunStatusCompleted || run.Result != "答案" {
				t.Errorf("扫描动了一条已经结束的记录：%+v", run)
			}
		}
	}

	// 第二次扫描没有东西可扫。「扫过一次就干净了」这件事本身要有人守着。
	again, err := repo.SweepRunning(ctx, sweptAt)
	if err != nil {
		t.Fatalf("SweepRunning（第二次）: %v", err)
	}
	if again != 0 {
		t.Errorf("第二次扫描报了 %d 条, want 0", again)
	}
}

// TestTaskRunByIDReportsNotFoundWithoutError：查一条不存在的记录是「没有它」，不是
// 一次失败。
//
// 端口契约把这两件事分开写着，而复审实测：把 sql.ErrNoRows 那条分支去掉、让不存在
// 也包装成 error，go vet 干净、全仓全绿——这条分流此前没有任何用例守着。调用方据此
// 判「这条运行记录还在不在」，混起来会把一次正常的查不到报成存储故障。
func TestTaskRunByIDReportsNotFoundWithoutError(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	run, found, err := repo.TaskRunByID(context.Background(), "never-written")
	if err != nil {
		t.Fatalf("TaskRunByID = %v, %v, %v; want 零值, false, nil", run, found, err)
	}
	if found {
		t.Errorf("一条从没写过的 id 被报成了找得到：%+v", run)
	}
}

// TestStartTaskRunRefusesEmptyIDs：id 与 task_id 都不许为空。
//
// 两列在库里都是 NOT NULL，但空串通得过 NOT NULL：一条 id 为空的记录会占掉主键的
// 空串槽位，而它属于哪次运行无从得知。
func TestStartTaskRunRefusesEmptyIDs(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  domain.TaskRun
	}{
		{"id 为空", domain.TaskRun{TaskID: "task-1", AgentID: "a", StartedAt: time.Now().UTC(), Status: domain.RunStatusRunning}},
		{"task id 为空", domain.TaskRun{ID: "run-1", AgentID: "a", StartedAt: time.Now().UTC(), Status: domain.RunStatusRunning}},
	} {
		if err := repo.StartTaskRun(ctx, tc.run); err == nil {
			t.Errorf("%s：StartTaskRun 却成功了", tc.name)
		}
	}
}

// TestFinishTaskRunDoesNotRefuseASecondTerminalWrite：结束一条**已经是终态**的
// 记录不会被拒——第二次写回照样落地，并把第一次的终态覆盖掉。
//
// 这条用例钉的不是一个想要的能力，而是一个必须被别处补上的空缺：FinishTaskRun 的
// UPDATE 只按 id 匹配，不看当前 status，所以存储这一层不提供任何「只许结束一次」的
// 保证。于是「一次运行恰好落一次终态」只能由调用方保证——runtime.RunTask 的收口
// defer 用 finished 标记做到这件事（见 TestTheClosingWriteIsIdempotentBecauseTheStoreIsNot）。
//
// 哪天这里改成拒绝第二次写回，这条用例会红，那是提醒：runtime 那边的幂等论证要跟着改。
func TestFinishTaskRunDoesNotRefuseASecondTerminalWrite(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	ctx := context.Background()
	if err := repo.StartTaskRun(ctx, domain.TaskRun{
		ID:        "run-1",
		TaskID:    "task-1",
		AgentID:   "agent-1",
		StartedAt: time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC),
		Status:    domain.RunStatusRunning,
	}); err != nil {
		t.Fatalf("StartTaskRun: %v", err)
	}
	if err := repo.FinishTaskRun(ctx, domain.TaskRun{
		ID:         "run-1",
		EndedAt:    time.Date(2026, 9, 18, 10, 1, 0, 0, time.UTC),
		Result:     "答案",
		StopReason: domain.StopReasonCompleted,
		Status:     domain.RunStatusCompleted,
	}); err != nil {
		t.Fatalf("第一次 FinishTaskRun: %v", err)
	}
	if err := repo.FinishTaskRun(ctx, domain.TaskRun{
		ID:      "run-1",
		EndedAt: time.Date(2026, 9, 18, 10, 2, 0, 0, time.UTC),
		Status:  domain.RunStatusFailed,
		Error:   "第二次写回",
	}); err != nil {
		t.Fatalf("第二次 FinishTaskRun 被拒了；若这是有意加的守卫，runtime 侧的幂等论证要跟着改：%v", err)
	}
	got, found, err := repo.TaskRunByID(ctx, "run-1")
	if err != nil || !found {
		t.Fatalf("TaskRunByID = %v, %v, %v", got, found, err)
	}
	if got.Status != domain.RunStatusFailed || got.Error != "第二次写回" {
		t.Errorf("第二次写回没有覆盖第一次的终态：%+v", got)
	}
}
