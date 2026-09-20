package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/task"
)

// stubTaskRuns 是一份内存里的运行记录表：byTask 按 taskID 给出这个任务的几条腿，
// err 非 nil 时每一次查询都失败，用来把「库读不出来」这条路钉住。
type stubTaskRuns struct {
	byTask map[string][]domain.TaskRun
	err    error
}

func (s *stubTaskRuns) StartTaskRun(context.Context, domain.TaskRun) error  { return nil }
func (s *stubTaskRuns) FinishTaskRun(context.Context, domain.TaskRun) error { return nil }

func (s *stubTaskRuns) SweepRunning(context.Context, time.Time) (int, error) { return 0, nil }

func (s *stubTaskRuns) TaskRunByID(context.Context, string) (domain.TaskRun, bool, error) {
	return domain.TaskRun{}, false, nil
}

func (s *stubTaskRuns) ListTaskRuns(_ context.Context, taskID string) ([]domain.TaskRun, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.byTask[taskID], nil
}

var _ port.TaskRunStore = (*stubTaskRuns)(nil)

// newTaskResultFixture 装出一台只带任务表、运行记录表与事件总线的服务器，并放进
// 一条 done 任务。runs 为 nil 表示这个部署不落盘运行记录。
func newTaskResultFixture(t *testing.T, taskID string, runs port.TaskRunStore, events ...domain.RuntimeEvent) *HTTPServer {
	t.Helper()
	ctx := context.Background()
	scheduler := task.NewScheduler()
	bus := adapter.NewMemoryEventBus()
	if err := scheduler.Add(ctx, domain.Task{
		ID:        taskID,
		CompanyID: "company-1",
		Status:    domain.TaskDone,
		Input:     "跑一个任务",
	}); err != nil {
		t.Fatalf("scheduler.Add error = %v, want nil", err)
	}
	for _, event := range events {
		if err := bus.Publish(ctx, event); err != nil {
			t.Fatalf("events.Publish error = %v, want nil", err)
		}
	}
	return NewHTTPServer(Config{Tasks: scheduler, WorkflowEvents: bus, TaskRuns: runs})
}

// getTaskResultRaw 打一次 /v1/tasks/{id}/result，返回状态码与原始响应体。
func getTaskResultRaw(t *testing.T, srv *HTTPServer, taskID string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID+"/result", nil))
	return rec.Code, rec.Body.String()
}

// getTaskResult 打一次 /v1/tasks/{id}/result 并要求它成功，返回解出来的响应。
func getTaskResult(t *testing.T, srv *HTTPServer, taskID string) taskResultResponse {
	t.Helper()
	code, body := getTaskResultRaw(t, srv, taskID)
	if code != http.StatusOK {
		t.Fatalf("GET result status = %d, want %d body=%s", code, http.StatusOK, body)
	}
	var got taskResultResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("Decode(result response) error = %v, want nil body=%s", err, body)
	}
	return got
}

// completedEvent 是落盘之前老任务仅剩的那条出处：事件总线上的 task_completed。
func completedEvent(taskID, message string, totalTokens int) domain.RuntimeEvent {
	return domain.RuntimeEvent{
		Type:        "task_completed",
		TaskID:      taskID,
		Message:     message,
		TotalTokens: totalTokens,
	}
}

// TestTaskResultPrefersThePersistedRun：表里有记录就以表为准。
//
// 表是唯一能报出 interrupted 的来源：事件总线里根本没有「中断」这种事件，因为写下
// 它的那个进程已经不在了。同一条任务上还挂着一条 task_completed 事件，它必须输给
// 表——否则一次被中断的运行会顶着一个「已完成」的答案报出去。
func TestTaskResultPrefersThePersistedRun(t *testing.T) {
	t.Parallel()

	started := time.Now().Add(-time.Minute)
	runs := &stubTaskRuns{byTask: map[string][]domain.TaskRun{
		"task-1": {{
			ID: "run-1", TaskID: "task-1", Status: domain.RunStatusInterrupted,
			Result: "", StartedAt: started, EndedAt: started.Add(30 * time.Second),
		}},
	}}
	srv := newTaskResultFixture(t, "task-1", runs, completedEvent("task-1", "老答案", 42))

	got := getTaskResult(t, srv, "task-1")
	if got.RunStatus != string(domain.RunStatusInterrupted) {
		t.Errorf("RunStatus = %q, want %q", got.RunStatus, domain.RunStatusInterrupted)
	}
	if got.Result != "" || got.TotalTokens != 0 {
		t.Errorf("表里那条腿没有结果，却报出了 %+v", got)
	}
	// 既有的 status 承载任务自身的生命周期，本任务不动它的语义：GUI 读的是它。
	if got.Status != string(domain.TaskDone) {
		t.Errorf("Status = %q, want %q", got.Status, domain.TaskDone)
	}
}

// TestTaskResultFallsBackToTheEventLog：表里没有（落盘之前的老任务）就回落事件。
func TestTaskResultFallsBackToTheEventLog(t *testing.T) {
	t.Parallel()

	runs := &stubTaskRuns{}
	srv := newTaskResultFixture(t, "task-1", runs, completedEvent("task-1", "老答案", 42))

	got := getTaskResult(t, srv, "task-1")
	if got.Result != "老答案" || got.TotalTokens != 42 {
		t.Errorf("回落没生效：%+v", got)
	}
	// 老任务在表里没有腿，run_status 为空串。空串不是 RunStatus 的取值，读的人由此
	// 知道「这条任务没有落盘记录」，而不会把它错当成某个真实状态。
	if got.RunStatus != "" {
		t.Errorf("RunStatus = %q, want 空串", got.RunStatus)
	}
}

// TestTaskResultReportsAStoreFailure：查表失败要报错，不能悄悄回落到事件。
//
// 悄悄回落会把一次数据库故障渲染成「这条任务没有落盘记录」，而那两件事对读的人
// 完全不同。
func TestTaskResultReportsAStoreFailure(t *testing.T) {
	t.Parallel()

	runs := &stubTaskRuns{err: errors.New("db is locked")}
	srv := newTaskResultFixture(t, "task-1", runs, completedEvent("task-1", "老答案", 42))

	code, body := getTaskResultRaw(t, srv, "task-1")
	if code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want %d, body = %s", code, http.StatusInternalServerError, body)
	}
}

// TestTaskResultReportsTheLatestLeg：一个任务有多条腿时，报最后开始的那一条。
//
// 挂起等审批是一条腿，批准之后的续跑是另一条腿。答案在后一条腿上。
func TestTaskResultReportsTheLatestLeg(t *testing.T) {
	t.Parallel()

	first := time.Now().Add(-10 * time.Minute)
	second := first.Add(5 * time.Minute)
	runs := &stubTaskRuns{byTask: map[string][]domain.TaskRun{
		"task-1": {
			{
				ID: "run-1", TaskID: "task-1", Status: domain.RunStatusSuspended,
				Result: "", StartedAt: first, EndedAt: first.Add(time.Minute),
			},
			{
				ID: "run-2", TaskID: "task-1", Status: domain.RunStatusCompleted,
				Result: "最终答案", TotalTokens: 7, StartedAt: second, EndedAt: second.Add(2 * time.Second),
			},
		},
	}}
	srv := newTaskResultFixture(t, "task-1", runs)

	got := getTaskResult(t, srv, "task-1")
	if got.RunStatus != string(domain.RunStatusCompleted) {
		t.Errorf("RunStatus = %q, want %q", got.RunStatus, domain.RunStatusCompleted)
	}
	if got.Result != "最终答案" || got.TotalTokens != 7 {
		t.Errorf("续跑那条腿的结果没报出来：%+v", got)
	}
}

// TestTaskResultLatestSuspendedLegOverridesAnEarlierCompletedOne：最后一条腿停在
// suspended 时，不能拿更早那条已完成的腿的答案去填。
//
// 那会把一个还在等人审批的任务说成已经有答案了，而这两件事对调用方是相反的指示。
func TestTaskResultLatestSuspendedLegOverridesAnEarlierCompletedOne(t *testing.T) {
	t.Parallel()

	first := time.Now().Add(-10 * time.Minute)
	second := first.Add(5 * time.Minute)
	runs := &stubTaskRuns{byTask: map[string][]domain.TaskRun{
		"task-1": {
			{
				ID: "run-1", TaskID: "task-1", Status: domain.RunStatusCompleted,
				Result: "第一段答案", TotalTokens: 11, StartedAt: first, EndedAt: first.Add(time.Minute),
			},
			{
				ID: "run-2", TaskID: "task-1", Status: domain.RunStatusSuspended,
				Result: "", StartedAt: second, EndedAt: second.Add(time.Second),
			},
		},
	}}
	srv := newTaskResultFixture(t, "task-1", runs)

	got := getTaskResult(t, srv, "task-1")
	if got.RunStatus != string(domain.RunStatusSuspended) {
		t.Errorf("RunStatus = %q, want %q", got.RunStatus, domain.RunStatusSuspended)
	}
	if got.Result != "" || got.TotalTokens != 0 {
		t.Errorf("挂起的那条腿还没有答案，却报出了更早那条腿的：%+v", got)
	}
}

// TestTaskResultRunningLegReportsNoElapsed：还在跑的腿没有 ended_at，耗时报 0。
//
// running 与 suspended 之外的终态才写 ended_at；拿一个零值时间去减开始时间会得出
// 一个巨大的负数，并作为「耗时」报给调用方。
func TestTaskResultRunningLegReportsNoElapsed(t *testing.T) {
	t.Parallel()

	runs := &stubTaskRuns{byTask: map[string][]domain.TaskRun{
		"task-1": {{
			ID: "run-1", TaskID: "task-1", Status: domain.RunStatusRunning,
			StartedAt: time.Now().Add(-time.Minute),
		}},
	}}
	srv := newTaskResultFixture(t, "task-1", runs)

	got := getTaskResult(t, srv, "task-1")
	if got.RunStatus != string(domain.RunStatusRunning) {
		t.Errorf("RunStatus = %q, want %q", got.RunStatus, domain.RunStatusRunning)
	}
	if got.ElapsedMs != 0 {
		t.Errorf("ElapsedMs = %d, want 0", got.ElapsedMs)
	}
}
