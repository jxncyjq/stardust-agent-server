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
// 静默的。
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
		Error:            "",
	}
}

// TestTaskRunRoundTripsEveryField：写进去的每个字段都要读得回来。
func TestTaskRunRoundTripsEveryField(t *testing.T) {
	t.Parallel()

	repo := newTaskRunRepo(t)
	ctx := context.Background()
	want := fullTaskRun()
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
