package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/task"
)

// newTaskRunFixture 装出一台只带任务表与运行记录表的服务器，并放进一条属于
// company-1 的 done 任务。
func newTaskRunFixture(t *testing.T, runs port.TaskRunStore) *HTTPServer {
	t.Helper()
	return NewHTTPServer(Config{Tasks: newTaskRunTasks(t), TaskRuns: runs})
}

// newFlakyTaskRunFixture 装的是与 newTaskRunFixture 同一台服务器，只是任务表外面
// 套了一层可开可关的故障开关，并把开关交回给用例。
//
// 开关要能在同一台服务器上开关，是因为「任务表读失败」这条用例必须自带阳性对照：
// 先在开关关着时读到 200，再打开读 500，否则一个恒 500 的装配也能让它绿。
func newFlakyTaskRunFixture(t *testing.T, runs port.TaskRunStore) (*HTTPServer, *flakyTaskStore) {
	t.Helper()
	tasks := &flakyTaskStore{inner: newTaskRunTasks(t)}
	return NewHTTPServer(Config{Tasks: tasks, TaskRuns: runs}), tasks
}

// newTaskRunTasks 造一张任务表，里面只有一条属于 company-1 的 done 任务。
func newTaskRunTasks(t *testing.T) *task.Scheduler {
	t.Helper()
	scheduler := task.NewScheduler()
	if err := scheduler.Add(context.Background(), domain.Task{
		ID:        "task-1",
		CompanyID: "company-1",
		Status:    domain.TaskDone,
		Input:     "跑一个任务",
	}); err != nil {
		t.Fatalf("scheduler.Add error = %v, want nil", err)
	}
	return scheduler
}

// flakyTaskStore 包着一张真的任务表，err 非空时让 Get 报错，其余方法原样转交。
//
// 只拦 Get：鉴权取 company 走的就是这一个方法，而这条用例要模拟的是 sqlite 被别的
// 写者短暂锁住的那一刻。
type flakyTaskStore struct {
	inner TaskStore
	err   error
}

func (s *flakyTaskStore) Add(ctx context.Context, task domain.Task) error {
	return s.inner.Add(ctx, task)
}

func (s *flakyTaskStore) Get(ctx context.Context, taskID string) (domain.Task, bool, error) {
	if s.err != nil {
		return domain.Task{}, false, s.err
	}
	return s.inner.Get(ctx, taskID)
}

func (s *flakyTaskStore) List(ctx context.Context) ([]domain.Task, error) {
	return s.inner.List(ctx)
}

var _ TaskStore = (*flakyTaskStore)(nil)

// getTaskRunRaw 打一次 GET /v1/task-runs/{run_id}，返回状态码与原始响应体。
// companyID 非空时带上 X-Company-ID，模拟一个真实租户的调用方。
func getTaskRunRaw(t *testing.T, srv *HTTPServer, runID, companyID string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/task-runs/"+runID, nil)
	if companyID != "" {
		req.Header.Set("X-Company-ID", companyID)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// getTaskRun 打一次 GET /v1/task-runs/{run_id} 并要求它成功。
func getTaskRun(t *testing.T, srv *HTTPServer, runID string) taskRunResponse {
	t.Helper()
	code, body := getTaskRunRaw(t, srv, runID, "")
	if code != http.StatusOK {
		t.Fatalf("GET task run status = %d, want %d body=%s", code, http.StatusOK, body)
	}
	var got taskRunResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("Decode(task run response) error = %v, want nil body=%s", err, body)
	}
	return got
}

// TestTaskRunIsReturnedFieldByField：按 run id 查得到的那条记录，每个字段都要报出来。
//
// 这个端点存在的理由就是「这条运行说得清自己停在哪」：少报一个字段，读的人就还得
// 去直连 sqlite 手查，而那正是这次改造要消灭的事。
func TestTaskRunIsReturnedFieldByField(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	ended := started.Add(90 * time.Second)
	runs := &stubTaskRuns{byID: map[string]domain.TaskRun{
		"run-1": {
			ID: "run-1", TaskID: "task-1", AgentID: "agent-1",
			StartedAt: started, EndedAt: ended,
			Result: "答案", StopReason: domain.StopReasonCompleted,
			PromptTokens: 11, CompletionTokens: 22, CachedTokens: 33, TotalTokens: 66,
			GeneratedFiles: []string{"out/a.md"},
			Status:         domain.RunStatusCompleted,
			ParentTaskID:   "task-parent", Background: true, Goal: "把 A 查清楚",
		},
	}}
	srv := newTaskRunFixture(t, runs)

	got := getTaskRun(t, srv, "run-1")
	if got.RunID != "run-1" || got.TaskID != "task-1" {
		t.Errorf("run id = %q / task id = %q, want run-1 / task-1", got.RunID, got.TaskID)
	}
	if got.ParentTaskID != "task-parent" || !got.Background || got.Goal != "把 A 查清楚" {
		t.Errorf("父子关系没报出来：parent=%q background=%v goal=%q", got.ParentTaskID, got.Background, got.Goal)
	}
	if got.RunStatus != string(domain.RunStatusCompleted) || got.Result != "答案" ||
		got.Error != "" || got.StopReason != string(domain.StopReasonCompleted) {
		t.Errorf("结局没报全：run_status=%q result=%q error=%q stop_reason=%q",
			got.RunStatus, got.Result, got.Error, got.StopReason)
	}
	if got.PromptTokens != 11 || got.CompletionTokens != 22 || got.CachedTokens != 33 || got.TotalTokens != 66 {
		t.Errorf("usage 没报全：%+v", got)
	}
	if len(got.GeneratedFiles) != 1 || got.GeneratedFiles[0] != "out/a.md" {
		t.Errorf("generated_files = %v, want [out/a.md]", got.GeneratedFiles)
	}
	if !got.StartedAt.Equal(started) || !got.EndedAt.Equal(ended) {
		t.Errorf("起止时刻 = %s / %s, want %s / %s", got.StartedAt, got.EndedAt, started, ended)
	}
}

// TestTaskRunKeepsAFailedRunsError：failed 那条路的错误摘要也要报出来。
//
// 「它为什么停下」正是运维打这个端点要问的问题；不报出来，这条记录就只说得出「它
// 失败了」。
func TestTaskRunKeepsAFailedRunsError(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	runs := &stubTaskRuns{byID: map[string]domain.TaskRun{
		"run-1": {
			ID: "run-1", TaskID: "task-1", StartedAt: started, EndedAt: started.Add(time.Second),
			Status: domain.RunStatusFailed, Error: "模型调用超时",
		},
	}}
	srv := newTaskRunFixture(t, runs)

	got := getTaskRun(t, srv, "run-1")
	if got.RunStatus != string(domain.RunStatusFailed) || got.Error != "模型调用超时" {
		t.Errorf("run_status = %q error = %q, want failed / 模型调用超时", got.RunStatus, got.Error)
	}
}

// TestTaskRunReachesABackgroundSubTask：后台子任务那一行按它自己的 run id 查得到。
//
// 这是这个端点被加出来的那个承诺：SubTaskHandle 的注释说「外部可以拿它查这条子任务
// 的运行记录——包括进程重启之后（那时它的状态是 interrupted）」。后台子任务**从不进
// 任务表**（runChild 就地造一个 domain.Task 直接喂给 RunTask，没有 scheduler.Add），
// 所以 GET /v1/tasks/{id}/result 对它一律 404——它在查 task_runs 之前先查任务表。
// 这条用例钉的正是「那一行不经 sqlite 手查也拿得到」。
func TestTaskRunReachesABackgroundSubTask(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	runs := &stubTaskRuns{byID: map[string]domain.TaskRun{
		"run-sub": {
			ID: "run-sub", TaskID: "sub-uuid", AgentID: "agent-1",
			StartedAt: started, EndedAt: started.Add(time.Minute),
			Status: domain.RunStatusInterrupted,
			// 子任务 id 不在任务表里，父任务 id 才在。
			ParentTaskID: "task-1", Background: true, Goal: "把 A 查清楚",
		},
	}}
	srv := newTaskRunFixture(t, runs)

	// 阳性对照：同一条子任务走任务结果那个端点是 404，因为它先查任务表。这条对照
	// 就是本端点存在的理由，它一旦不再成立，本端点也该重新论证。
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks/sub-uuid/result", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/v1/tasks/sub-uuid/result 的状态码 = %d, want 404", rec.Code)
	}

	got := getTaskRun(t, srv, "run-sub")
	if got.RunStatus != string(domain.RunStatusInterrupted) {
		t.Errorf("run_status = %q, want interrupted", got.RunStatus)
	}
	if got.TaskID != "sub-uuid" || got.ParentTaskID != "task-1" || !got.Background {
		t.Errorf("子任务那一行没报清它的身世：%+v", got)
	}
}

// TestTaskRunReportsNotFound：没有这条记录是 404，不是一个看起来正常的空响应。
func TestTaskRunReportsNotFound(t *testing.T) {
	t.Parallel()

	srv := newTaskRunFixture(t, &stubTaskRuns{})
	code, body := getTaskRunRaw(t, srv, "never-written", "")
	if code != http.StatusNotFound {
		t.Fatalf("status code = %d, want %d, body = %s", code, http.StatusNotFound, body)
	}
}

// TestTaskRunReportsAStoreFailure：查表失败报 500，绝不退化成一个空的 200。
//
// 一个字段全空的 200 会被读成「这条运行什么都没干」，而真相是这次查询根本没查成。
func TestTaskRunReportsAStoreFailure(t *testing.T) {
	t.Parallel()

	srv := newTaskRunFixture(t, &stubTaskRuns{err: errors.New("db is locked")})
	code, body := getTaskRunRaw(t, srv, "run-1", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want %d, body = %s", code, http.StatusInternalServerError, body)
	}
}

// TestTaskRunIsUnavailableWithoutAStore：这个部署不落盘运行记录时报 503。
//
// 报 404 会把「这里从来不存运行记录」说成「没有这条记录」，而那两件事对读的人是
// 相反的指示：前者该去看部署，后者该去看这条运行。
func TestTaskRunIsUnavailableWithoutAStore(t *testing.T) {
	t.Parallel()

	srv := newTaskRunFixture(t, nil)
	code, body := getTaskRunRaw(t, srv, "run-1", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status code = %d, want %d, body = %s", code, http.StatusServiceUnavailable, body)
	}
}

// TestTaskRunRefusesAnotherCompany：跨公司读不到别人的运行记录。
//
// /v1/tasks/{id}/result 一直是按任务的 company 鉴权的；这个端点报的是同一批内容，
// 不设同样的门等于给它开了一条绕过去的路。运行记录自己不带 company，所以门要从它
// 指向的任务上取：直连任务取自己那条，后台子任务取父任务那条（它自己从不进任务表）。
func TestTaskRunRefusesAnotherCompany(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	runs := &stubTaskRuns{byID: map[string]domain.TaskRun{
		"run-direct": {
			ID: "run-direct", TaskID: "task-1", StartedAt: started,
			Status: domain.RunStatusCompleted, Result: "答案",
		},
		"run-sub": {
			ID: "run-sub", TaskID: "sub-uuid", StartedAt: started,
			Status: domain.RunStatusInterrupted, ParentTaskID: "task-1", Background: true,
		},
	}}
	srv := newTaskRunFixture(t, runs)

	for _, runID := range []string{"run-direct", "run-sub"} {
		t.Run(runID, func(t *testing.T) {
			// 阳性对照：自家公司读得到，否则这条用例用一个 403 也能骗过去。
			if code, body := getTaskRunRaw(t, srv, runID, "company-1"); code != http.StatusOK {
				t.Fatalf("自家公司读 %s 的状态码 = %d, want 200, body = %s", runID, code, body)
			}
			if code, body := getTaskRunRaw(t, srv, runID, "company-2"); code != http.StatusForbidden {
				t.Errorf("别家公司读 %s 的状态码 = %d, want 403, body = %s", runID, code, body)
			}
		})
	}
}

// TestTaskRunReportsATaskStoreFailure：鉴权要取 company 时任务表读失败报 500，
// 绝不塌缩成「没有 company」。
//
// 塌缩的后果按部署形态相反：单机默认（RequireIdentity=false、调用方不带
// X-Company-ID）会把这条运行记录**一次租户校验都不做地**发出去；同一次故障在开了
// 身份强制的部署上却是 403。一次库故障在两种部署下给出相反的可见行为，比一个 500
// 难查得多，而「无门读」正是这个端点最不该有的那一支。
//
// 复审实测：把 taskRunCompany 里那条 return error 换成 continue，go vet 干净、
// server 与 cli 两包全绿——这条 fail-loud 此前没有任何用例守着。
func TestTaskRunReportsATaskStoreFailure(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	runs := &stubTaskRuns{byID: map[string]domain.TaskRun{
		"run-1": {
			ID: "run-1", TaskID: "task-1", StartedAt: started, EndedAt: started.Add(time.Second),
			Status: domain.RunStatusCompleted, Result: "答案",
		},
	}}
	srv, tasks := newFlakyTaskRunFixture(t, runs)

	// 阳性对照：任务表好着的时候同一条记录读得到 200。少了这一半，下面那个 500
	// 可能来自任何地方，这条用例就挡不住它本该挡的那件事。
	if code, body := getTaskRunRaw(t, srv, "run-1", ""); code != http.StatusOK {
		t.Fatalf("任务表没故障时的状态码 = %d, want 200, body = %s", code, body)
	}

	tasks.err = errors.New("db is locked")
	code, body := getTaskRunRaw(t, srv, "run-1", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("任务表读失败时的状态码 = %d, want 500——这条记录被无租户校验地发了出去，body = %s",
			code, body)
	}
	if !strings.Contains(body, "db is locked") {
		t.Errorf("响应体里看不出故障原因：%s", body)
	}
}
