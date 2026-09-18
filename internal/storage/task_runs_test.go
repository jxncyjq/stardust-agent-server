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
