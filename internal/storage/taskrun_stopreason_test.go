package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// 写得进去，还要读得回来 —— 本仓的经典缺口是「写得出但读不回」。
func TestTaskRunStopReasonSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()
	repo := openTestSQLiteRepository(t)
	run := domain.TaskRun{
		ID:         "run-1",
		TaskID:     "task-1",
		AgentID:    "agent-1",
		StartedAt:  time.Now(),
		EndedAt:    time.Now(),
		Result:     "done",
		StopReason: domain.StopReasonToolLoopCap,
	}
	if err := repo.SaveTaskRun(context.Background(), run); err != nil {
		t.Fatalf("SaveTaskRun() error = %v, want nil", err)
	}

	got, err := repo.ListTaskRuns(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("ListTaskRuns() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListTaskRuns() returned %d runs, want 1", len(got))
	}
	if got[0].StopReason != domain.StopReasonToolLoopCap {
		t.Errorf("StopReason = %q, want %q: the column is written but not read back",
			got[0].StopReason, domain.StopReasonToolLoopCap)
	}
}

// 老行的 stop_reason 是空串：读出来是空值，不是错误。写入侧的零值断言拦的是
// 新写入，读取侧必须容忍历史行。
func TestTaskRunFromBeforeTheColumnReadsAsAnEmptyReason(t *testing.T) {
	t.Parallel()
	repo := openTestSQLiteRepository(t)
	if _, err := repo.db.ExecContext(context.Background(), `
		INSERT INTO task_runs (id, task_id, agent_id, started_at, ended_at, result)
		VALUES ('old-1', 'task-old', 'agent-1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:01Z', 'legacy')
	`); err != nil {
		t.Fatalf("insert legacy row error = %v, want nil", err)
	}

	got, err := repo.ListTaskRuns(context.Background(), "task-old")
	if err != nil {
		t.Fatalf("ListTaskRuns() error = %v, want nil: a row written before the column must still read", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListTaskRuns() returned %d runs, want 1", len(got))
	}
	if got[0].StopReason != "" {
		t.Errorf("StopReason = %q, want empty: a legacy row has no recorded reason", got[0].StopReason)
	}
}

// 迁移幂等：同一个库上跑两次不出错。
func TestStopReasonColumnMigrationIsIdempotent(t *testing.T) {
	t.Parallel()
	repo := openTestSQLiteRepository(t)
	if err := repo.applyColumnMigrations(context.Background()); err != nil {
		t.Fatalf("second applyColumnMigrations() error = %v, want nil", err)
	}
}
