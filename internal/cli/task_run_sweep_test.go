package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/storage"

	_ "modernc.org/sqlite"
)

// sweepingStore 是一个只关心 SweepRunning 的假落点：它记下被扫过几次、返回预设的
// 条数或错误。其余四个方法只为满足端口契约而存在。
type sweepingStore struct {
	swept int
	err   error
	calls int
}

func (s *sweepingStore) StartTaskRun(context.Context, domain.TaskRun) error  { return nil }
func (s *sweepingStore) FinishTaskRun(context.Context, domain.TaskRun) error { return nil }
func (s *sweepingStore) SweepRunning(context.Context, time.Time) (int, error) {
	s.calls++
	return s.swept, s.err
}

func (s *sweepingStore) TaskRunByID(context.Context, string) (domain.TaskRun, bool, error) {
	return domain.TaskRun{}, false, nil
}

func (s *sweepingStore) ListTaskRuns(context.Context, string) ([]domain.TaskRun, error) {
	return nil, nil
}

// fakeSweepAuditLog 收住这一轮扫描写下的审计事件。
type fakeSweepAuditLog struct {
	mu   sync.Mutex
	rows []domain.AuditEvent
	err  error
}

func newFakeAuditLog() *fakeSweepAuditLog { return &fakeSweepAuditLog{} }

func (a *fakeSweepAuditLog) Append(_ context.Context, event domain.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.rows = append(a.rows, event)
	return nil
}

func (a *fakeSweepAuditLog) Events() ([]domain.AuditEvent, error) {
	return a.events(), nil
}

func (a *fakeSweepAuditLog) events() []domain.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]domain.AuditEvent(nil), a.rows...)
}

// TestSweepInterruptedRunsRecordsEveryRound：扫到 0 条也要记审计。
//
// 一个「从来没扫到过东西」的扫描和一个根本没跑的扫描，在日志里必须分得开——这与
// trustlist 刷新循环「每一轮都记」是同一条理由。
func TestSweepInterruptedRunsRecordsEveryRound(t *testing.T) {
	t.Parallel()

	for _, swept := range []int{0, 3} {
		store := &sweepingStore{swept: swept}
		audit := newFakeAuditLog()
		if err := sweepInterruptedRuns(context.Background(), store, audit, testLogger()); err != nil {
			t.Fatalf("sweepInterruptedRuns: %v", err)
		}
		events := audit.events()
		if len(events) != 1 {
			t.Fatalf("扫到 %d 条时记了 %d 条审计, want 1", swept, len(events))
		}
		if events[0].Action != "task_runs_swept" {
			t.Errorf("审计 action = %q", events[0].Action)
		}
	}
}

// TestSweepInterruptedRunsFailsLoud：扫不动就不许起。
//
// 扫描是这份记录可信的前提：扫不动意味着接下来每一行 running 的含义都是不确定的
// ——它可能是本次进程正在跑的，也可能是上一次留下的。
func TestSweepInterruptedRunsFailsLoud(t *testing.T) {
	t.Parallel()

	store := &sweepingStore{err: errors.New("db is locked")}
	err := sweepInterruptedRuns(context.Background(), store, newFakeAuditLog(), testLogger())
	if err == nil {
		t.Fatal("扫描失败了却放行了启动")
	}
}

// TestSweepInterruptedRunsFailsWhenTheRoundCannotBeRecorded：这一轮记不下来同样不许起。
//
// 扫描已经**改了**库里的行；记不下这件事发生过，就等于有一批 running 被悄悄改写而
// 没有任何痕迹——与扫不动一样，之后任何一条记录的含义都说不清了。
func TestSweepInterruptedRunsFailsWhenTheRoundCannotBeRecorded(t *testing.T) {
	t.Parallel()

	audit := newFakeAuditLog()
	audit.err = errors.New("audit table is gone")
	err := sweepInterruptedRuns(context.Background(), &sweepingStore{swept: 2}, audit, testLogger())
	if err == nil {
		t.Fatal("这一轮没能记下来，启动却被放行了")
	}
}

// TestSweepInterruptedRunsRefusesANilStore：serve 装配一定有这个 store，nil 说明
// 接线漏了。
func TestSweepInterruptedRunsRefusesANilStore(t *testing.T) {
	t.Parallel()

	if err := sweepInterruptedRuns(context.Background(), nil, newFakeAuditLog(), testLogger()); err == nil {
		t.Fatal("nil store 被当成了「没什么要扫的」")
	}
}

// TestSweepInterruptedRunsLeavesSuspendedRowsAlone：扫描只认 running，suspended 必须
// 原样留下。
//
// 它走的是**真**仓储，而不是上面那个假 store：过滤条件是 SQL 里的一句 WHERE，假
// store 根本表达不出「放宽了过滤条件」这个错误。suspended 是某条腿自己写下的结论
// （停在这里等人审批），把它改成 interrupted 等于拿「进程没了」覆盖掉一件确凿发生
// 过的事。
func TestSweepInterruptedRunsLeavesSuspendedRowsAlone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("close repo: %v", err)
		}
	})

	start := func(id string) {
		t.Helper()
		if err := repo.StartTaskRun(ctx, domain.TaskRun{
			ID: id, TaskID: "task-" + id, Status: domain.RunStatusRunning, StartedAt: time.Now(),
		}); err != nil {
			t.Fatalf("StartTaskRun(%s): %v", id, err)
		}
	}
	start("run-left-running")
	start("run-suspended")
	if err := repo.FinishTaskRun(ctx, domain.TaskRun{
		ID: "run-suspended", Status: domain.RunStatusSuspended, EndedAt: time.Now(),
	}); err != nil {
		t.Fatalf("FinishTaskRun(suspended): %v", err)
	}

	if err := sweepInterruptedRuns(ctx, repo, newFakeAuditLog(), testLogger()); err != nil {
		t.Fatalf("sweepInterruptedRuns: %v", err)
	}

	suspended, found, err := repo.TaskRunByID(ctx, "run-suspended")
	if err != nil || !found {
		t.Fatalf("TaskRunByID(run-suspended) = found %v, err %v", found, err)
	}
	if suspended.Status != domain.RunStatusSuspended {
		t.Errorf("suspended 行被扫成了 %q：扫描的过滤条件被放宽了，一件确凿发生过的事"+
			"被「进程没了」覆盖掉", suspended.Status)
	}
	running, found, err := repo.TaskRunByID(ctx, "run-left-running")
	if err != nil || !found {
		t.Fatalf("TaskRunByID(run-left-running) = found %v, err %v", found, err)
	}
	if running.Status != domain.RunStatusInterrupted {
		t.Errorf("running 行 = %q, want %q：这条用例的前提（扫描真的动过库）不成立",
			running.Status, domain.RunStatusInterrupted)
	}
}

// TestServeStartupSweepsRunsLeftByADeadProcess 守的是**接线**，不是函数：把
// BuildServeService 里那次 sweepInterruptedRuns 调用删掉，这条必须红。
//
// 只测函数不测接线是这仓反复踩的那个形状——函数写得再对，没人在启动时调它，残留
// 的 running 就永远留在库里。
func TestServeStartupSweepsRunsLeftByADeadProcess(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	workDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "agent.db")
	// 上一次进程留下的两行：一行停在 running（它的进程没了），一行停在 suspended
	// （它自己走到了头，在等人）。
	seed, err := storage.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(seed): %v", err)
	}
	for _, id := range []string{"stale-running", "stale-suspended"} {
		if err := seed.StartTaskRun(ctx, domain.TaskRun{
			ID: id, TaskID: "task-" + id, Status: domain.RunStatusRunning, StartedAt: time.Now(),
		}); err != nil {
			t.Fatalf("StartTaskRun(%s): %v", id, err)
		}
	}
	if err := seed.FinishTaskRun(ctx, domain.TaskRun{
		ID: "stale-suspended", Status: domain.RunStatusSuspended, EndedAt: time.Now(),
	}); err != nil {
		t.Fatalf("FinishTaskRun(stale-suspended): %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed repo: %v", err)
	}

	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "这条用例不跑任务", http.StatusInternalServerError)
	}))
	t.Cleanup(model.Close)

	configPath := filepath.Join(t.TempDir(), "agent.json")
	body := `{
  "storage": {"driver": "sqlite", "path": ` + jsonString(dbPath) + `},
  "context_files": {"root": ` + jsonString(workDir) + `},
  "maas": {"base_url": ` + jsonString(model.URL) + `},
  "runtime": {"max_tool_rounds": 1},
  "service": {"background_interval": "50ms"}
}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	result, err := BuildServeService(ctx, ServeOptions{
		ConfigPath: configPath,
		Addr:       "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("BuildServeService: %v", err)
	}
	// 不 Start：扫描在装配期就该跑完，晚于装配才跑就意味着 serve 已经开始接任务了。
	result.Close()

	check, err := storage.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(check): %v", err)
	}
	t.Cleanup(func() {
		if err := check.Close(); err != nil {
			t.Errorf("close check repo: %v", err)
		}
	})

	stale, found, err := check.TaskRunByID(ctx, "stale-running")
	if err != nil || !found {
		t.Fatalf("TaskRunByID(stale-running) = found %v, err %v", found, err)
	}
	if stale.Status != domain.RunStatusInterrupted {
		t.Errorf("上一次进程留下的 running 行 = %q, want %q："+
			"BuildServeService 没有在启动时扫这张表", stale.Status, domain.RunStatusInterrupted)
	}
	suspended, found, err := check.TaskRunByID(ctx, "stale-suspended")
	if err != nil || !found {
		t.Fatalf("TaskRunByID(stale-suspended) = found %v, err %v", found, err)
	}
	if suspended.Status != domain.RunStatusSuspended {
		t.Errorf("suspended 行 = %q：启动扫描动了它", suspended.Status)
	}
}
