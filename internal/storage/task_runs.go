package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// 编译期保证 SQLiteRepository 满足端口契约。
//
// 这一行的作用是让「方法签名改了但端口没改」在**编译时**就停下来，而不是等到
// 装配时才发现某个实现悄悄不再满足接口。
var _ port.TaskRunStore = (*SQLiteRepository)(nil)

// StartTaskRun 插入一条 running 记录。
//
// 它与 FinishTaskRun 分成两个方法，而不是一个 SaveTaskRun 用两次：两次写入要写的
// 列不是一回事（见 FinishTaskRun），而一个能写全部列的方法在结束时被调用，就有机会
// 拿结束时那份不全的快照覆盖开头写下的东西。
//
// status 不是 running 时报错：这个方法就是「运行开始了」这句话本身，用它说别的话
// 意味着调用点接错了。
func (r *SQLiteRepository) StartTaskRun(ctx context.Context, run domain.TaskRun) error {
	if run.Status != domain.RunStatusRunning {
		return fmt.Errorf("start task run %q: status is %q, want %q",
			run.ID, run.Status, domain.RunStatusRunning)
	}
	if run.ID == "" || run.TaskID == "" {
		return fmt.Errorf("start task run: id %q and task id %q must both be set", run.ID, run.TaskID)
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_runs (
			id, task_id, agent_id, started_at, ended_at, result, stop_reason,
			status, parent_task_id, background, goal, error,
			reasoning_summary, prompt_tokens, completion_tokens, cached_tokens, total_tokens, generated_files
		)
		VALUES (?, ?, ?, ?, '', '', '', ?, ?, ?, ?, '', '', 0, 0, 0, 0, '')
	`, run.ID, run.TaskID, run.AgentID, formatTime(run.StartedAt),
		string(run.Status), run.ParentTaskID, boolToInt(run.Background), run.Goal)
	if err != nil {
		return fmt.Errorf("start task run %q: %w", run.ID, err)
	}
	return nil
}

// FinishTaskRun 按 id 写回终态。
//
// 它只更新结束时才知道的那些列。started_at、goal、parent_task_id、background 在
// 开始时就已定型，不参与这次写入——那几个字段在 run 里通常是空的，写进去就是把
// 开头的记录抹平。
//
// 找不到那一行时报错而不是无声返回：这个方法的前提是 StartTaskRun 已经成功过一次，
// 前提不成立说明两个写入点之间断了，而那正是这份记录要防的事。
//
// 三个终态放行：completed、failed 与 suspended——挂起等审批同样是这条腿走到了头，
// 人批准之后跑的是另一条记录。interrupted 报错，理由见 domain.RunStatus：运行期的
// 代码说不出「没人知道它跑到哪」这句话。
func (r *SQLiteRepository) FinishTaskRun(ctx context.Context, run domain.TaskRun) error {
	switch run.Status {
	case domain.RunStatusCompleted, domain.RunStatusFailed, domain.RunStatusSuspended:
	default:
		return fmt.Errorf("finish task run %q: status is %q; only %q, %q and %q end a run here (%q is written "+
			"only by the startup sweep)", run.ID, run.Status,
			domain.RunStatusCompleted, domain.RunStatusFailed, domain.RunStatusSuspended,
			domain.RunStatusInterrupted)
	}
	files, err := marshalGeneratedFiles(run.GeneratedFiles)
	if err != nil {
		return fmt.Errorf("finish task run %q: %w", run.ID, err)
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE task_runs SET
			ended_at = ?,
			result = ?,
			stop_reason = ?,
			status = ?,
			error = ?,
			reasoning_summary = ?,
			prompt_tokens = ?,
			completion_tokens = ?,
			cached_tokens = ?,
			total_tokens = ?,
			generated_files = ?
		WHERE id = ?
	`, formatTime(run.EndedAt), run.Result, string(run.StopReason), string(run.Status), run.Error,
		run.ReasoningSummary, run.PromptTokens, run.CompletionTokens, run.CachedTokens, run.TotalTokens,
		files, run.ID)
	if err != nil {
		return fmt.Errorf("finish task run %q: %w", run.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish task run %q: %w", run.ID, err)
	}
	if affected == 0 {
		return fmt.Errorf("finish task run %q: no such run was started", run.ID)
	}
	return nil
}

// SweepRunning 把还停在 running 的记录全部改成 interrupted，返回改了几条。
//
// 它是「写 running 的那个进程没了」这句话唯一的出口，所以只在启动时、开始接任务
// 之前调用一次。它按状态扫全表，不区分是哪个进程写的：本部署假定一个 agent.db
// 只有一个 serve 进程在写（见规格第六节的取舍）。
//
// 过滤条件只认 running，这一条不许放宽：另外四个状态都是某条腿自己写下的结论，
// suspended 尤其——它是「这条腿停在这里等人」，改写成 interrupted 等于拿「进程没了」
// 覆盖掉一件确凿发生过的事（TestSweepRunningLeavesSuspendedRowsAlone 钉住这一点）。
func (r *SQLiteRepository) SweepRunning(ctx context.Context, at time.Time) (int, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE task_runs SET status = ?, ended_at = ?
		WHERE status = ?
	`, string(domain.RunStatusInterrupted), formatTime(at), string(domain.RunStatusRunning))
	if err != nil {
		return 0, fmt.Errorf("sweep running task runs: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sweep running task runs: %w", err)
	}
	return int(affected), nil
}

// TaskRunByID 按 run id 取一条记录。found 为 false 表示没有这条记录，与出错分开。
func (r *SQLiteRepository) TaskRunByID(ctx context.Context, runID string) (domain.TaskRun, bool, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, task_id, agent_id, started_at, ended_at, result, stop_reason,
		       status, parent_task_id, background, goal, error,
		       reasoning_summary, prompt_tokens, completion_tokens, cached_tokens, total_tokens, generated_files
		FROM task_runs
		WHERE id = ?
	`, runID)
	run, err := scanTaskRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TaskRun{}, false, nil
	}
	if err != nil {
		return domain.TaskRun{}, false, fmt.Errorf("get task run %q: %w", runID, err)
	}
	return run, true, nil
}
