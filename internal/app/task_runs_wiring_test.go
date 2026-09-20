package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// recordingTaskRunStore 记下开始行与终态行。它不校验状态转换——那是
// storage.SQLiteRepository 的职责，这里只回答「装配有没有把 store 送到」。
type recordingTaskRunStore struct {
	mu       sync.Mutex
	started  []domain.TaskRun
	finished []domain.TaskRun
}

func (s *recordingTaskRunStore) StartTaskRun(_ context.Context, run domain.TaskRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = append(s.started, run)
	return nil
}

func (s *recordingTaskRunStore) FinishTaskRun(_ context.Context, run domain.TaskRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = append(s.finished, run)
	return nil
}

func (s *recordingTaskRunStore) SweepRunning(context.Context, time.Time) (int, error) {
	return 0, nil
}

func (s *recordingTaskRunStore) TaskRunByID(context.Context, string) (domain.TaskRun, bool, error) {
	return domain.TaskRun{}, false, nil
}

func (s *recordingTaskRunStore) ListTaskRuns(context.Context, string) ([]domain.TaskRun, error) {
	return nil, nil
}

func (s *recordingTaskRunStore) snapshot() (started, finished []domain.TaskRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.TaskRun(nil), s.started...), append([]domain.TaskRun(nil), s.finished...)
}

var _ port.TaskRunStore = (*recordingTaskRunStore)(nil)

// TestARunTaskWithAStoreRecordsItsRun 守 `agent run --prompt` / `agent tui` 这条路
// （app.App.RunTask，四个生产 NewRuntime 装配点之一）。
//
// 它断言的是**装配的结果**：给 RunTaskOptions 一个 store，这次运行就真的留下了开始
// 行与终态行——而不是「Config 里有 TaskRuns 那一行」。少接这一处不会有任何报错：任务
// 照跑照返回，只是这条路径的运行永远不落盘。
func TestARunTaskWithAStoreRecordsItsRun(t *testing.T) {
	t.Parallel()

	store := &recordingTaskRunStore{}
	_, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID:   "app-runs-task",
		Prompt:   "介绍一下这个运行时",
		Maas:     adapter.NewRecordingMaas("跑完了"),
		ToolRoot: t.TempDir(),
		TaskRuns: store,
	})
	if err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}

	started, finished := store.snapshot()
	if len(started) != 1 {
		t.Fatalf("开始行 %d 条, want 1：app.RunTask 配了运行记录 store，库里却没有开始行", len(started))
	}
	if started[0].TaskID != "app-runs-task" {
		t.Errorf("开始行的 task id = %q, want %q", started[0].TaskID, "app-runs-task")
	}
	if len(finished) != 1 {
		t.Fatalf("终态行 %d 条, want 1：那一行会永远停在 running，下一次启动扫描把它记成 interrupted", len(finished))
	}
	if finished[0].Status != domain.RunStatusCompleted {
		t.Errorf("终态 = %q, want %q", finished[0].Status, domain.RunStatusCompleted)
	}
}

// TestARunTaskWithoutAStoreStillRunsItsTask 是上一条的对照：没有配 store 是契约
// 允许的部署形态（非持久化驱动、测试），任务必须照常跑完。
func TestARunTaskWithoutAStoreStillRunsItsTask(t *testing.T) {
	t.Parallel()

	result, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID:   "app-no-runs-task",
		Prompt:   "介绍一下这个运行时",
		Maas:     adapter.NewRecordingMaas("跑完了"),
		ToolRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("RunTask error = %v, want nil：没有配 store 是合法部署形态", err)
	}
	if result.TaskID != "app-no-runs-task" {
		t.Errorf("TaskID = %q, want %q", result.TaskID, "app-no-runs-task")
	}
}

// TestTheDemoRuntimeRecordsNoTaskRuns 守 demo 这条路（第四个装配点
// internal/app/app.go 的 demoRuntimeConfig）。
//
// 它断言的是一个**刻意的 nil**：这条路全用内存适配器，作用域内根本没有持久化仓储，
// 所以它正确地不接 TaskRuns——与它同样不接 SessionEvents 是同一条理由。断言写在这里，
// 是为了让「顺手塞一个写不进去的 store 进来」这种改动当场停下来，也为了让「这处是
// 判断过的」而不是「漏了」有个落点。
func TestTheDemoRuntimeRecordsNoTaskRuns(t *testing.T) {
	t.Parallel()

	cfg := demoRuntimeConfig(adapter.NewRecordingMaas("ok"), adapter.NewMemoryAuditLog(), adapter.NewMemoryEventBus())
	if cfg.TaskRuns != nil {
		t.Errorf("demoRuntimeConfig().TaskRuns = %v, want nil：demo 路径没有任何持久化仓储可写",
			cfg.TaskRuns)
	}
}
