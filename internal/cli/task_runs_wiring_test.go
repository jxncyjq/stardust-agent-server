package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/storage"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// 任务运行记录的装配断言。
//
// 与会话事件日志那一组（session_events_wiring_test.go）是同一个形状、同一条理由：
// 在这一组出现之前，生产上没有任何一处给 Config.TaskRuns 喂过 store——StartTaskRun /
// FinishTaskRun / SweepRunning 三个方法、五个状态、整张 task_runs 表都在，而一行
// 都不会被写。漏接的症状不是报错，是「这张表永远是空的」。
//
// 四个生产装配点各有一条断言：这里两条（默认 runner 的构造点、CLI 那条路的端口解析），
// internal/app 一条（App.RunTask），internal/runtime 一条（AgentRuntimeResolver）。
// 只接其中一条的形状是「直发任务落了盘、具名 agent 的任务悄悄没落」。

// TestTheTaskRunStoreReachesTheDefaultRunnerRuntimeConfig 守默认任务路径。
//
// 默认 runner 服务的是 AgentID 不在 agent 注册表里的每一个任务——GUI 自己的那条路，
// 也就是绝大多数任务。
func TestTheTaskRunStoreReachesTheDefaultRunnerRuntimeConfig(t *testing.T) {
	t.Parallel()

	store := &sweepingStore{}
	cfg := buildDefaultRunnerConfig(
		nil, nil, nil, nil,
		config.RuntimeConfig{},
		nil, nil, nil, nil,
		nil,
		nil,
		taskgate.NewTaskGate(),
		nil,
		"",
		nil,
		store,
	)

	if cfg.TaskRuns == nil {
		t.Fatal("buildDefaultRunnerConfig().TaskRuns = nil：默认 agent 的任务" +
			"（GUI 的主路径）一条运行记录都不会落盘")
	}
	if cfg.TaskRuns != port.TaskRunStore(store) {
		t.Fatalf("buildDefaultRunnerConfig().TaskRuns = %v, want 同一个 store %v",
			cfg.TaskRuns, store)
	}
}

// TestThePersistentRunPortsCarryTheTaskRunStore 守 `agent run --prompt` 与
// `agent tui` 走的那条路（app.App.RunTask）。
//
// 它与 serve 是两套装配：serve 的仓储解析在 BuildServeService 里，CLI 的在
// persistentRunPorts 里。
func TestThePersistentRunPortsCarryTheTaskRunStore(t *testing.T) {
	t.Parallel()

	ports, closePorts, err := persistentRunPorts(context.Background(), config.Config{
		Storage: config.StorageConfig{Driver: "sqlite", Path: filepath.Join(t.TempDir(), "agent.db")},
	})
	if err != nil {
		t.Fatalf("persistentRunPorts error = %v, want nil", err)
	}
	defer closePorts()

	if ports.taskRuns == nil {
		t.Fatal("persistentRunPorts().taskRuns = nil：`agent run` / `agent tui` " +
			"跑出来的任务一条运行记录都不会落盘")
	}
}

// TestNonPersistentRunPortsLeaveTheTaskRunStoreUnset 是上一条的对照：没有持久化
// 驱动时**必须**是 nil。
//
// 这不是兜底：Config.TaskRuns 把 nil 显式声明为一种合法部署形态（这次运行不落记录）。
// 断言它，是为了让「非 sqlite 驱动却塞进一个写不进去的 store」这种改动当场停下来。
func TestNonPersistentRunPortsLeaveTheTaskRunStoreUnset(t *testing.T) {
	t.Parallel()

	ports, closePorts, err := persistentRunPorts(context.Background(), config.Config{
		Storage: config.StorageConfig{Driver: "memory"},
	})
	if err != nil {
		t.Fatalf("persistentRunPorts error = %v, want nil", err)
	}
	defer closePorts()

	if ports.taskRuns != nil {
		t.Fatalf("非持久化驱动的 taskRuns = %v, want nil", ports.taskRuns)
	}
}

// capturingTaskRunStore 记下开始行，用来证明某条 CLI 路径的运行真的落到了这个 store。
type capturingTaskRunStore struct {
	mu      sync.Mutex
	started []domain.TaskRun
}

func (s *capturingTaskRunStore) StartTaskRun(_ context.Context, run domain.TaskRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = append(s.started, run)
	return nil
}

func (s *capturingTaskRunStore) FinishTaskRun(context.Context, domain.TaskRun) error { return nil }

func (s *capturingTaskRunStore) SweepRunning(context.Context, time.Time) (int, error) {
	return 0, nil
}

func (s *capturingTaskRunStore) TaskRunByID(context.Context, string) (domain.TaskRun, bool, error) {
	return domain.TaskRun{}, false, nil
}

func (s *capturingTaskRunStore) ListTaskRuns(context.Context, string) ([]domain.TaskRun, error) {
	return nil, nil
}

func (s *capturingTaskRunStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.started)
}

var _ port.TaskRunStore = (*capturingTaskRunStore)(nil)

// TestBuildTUITaskRunConfigCarriesTheTaskRunStore 守 `agent tui` 的构造点：
// newTUICommand 把 persistent.taskRuns 塞进 tuiTaskRunConfig 那一行。
//
// 断言的是**落盘的结果**（这次运行真的插了开始行），不是「代码里有那一行」。
func TestBuildTUITaskRunConfigCarriesTheTaskRunStore(t *testing.T) {
	t.Parallel()

	runs := &capturingTaskRunStore{}
	runCfg := buildTUITaskRunConfig(
		config.Config{Runtime: config.RuntimeConfig{MaxToolRounds: 1}},
		nil, "介绍一下这个运行时", &cliCaptureMaas{response: "已完成"}, "", "", "",
		nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, runs,
	)
	if _, err := runTUITask(context.Background(), app.New(), runCfg); err != nil {
		t.Fatalf("runTUITask() error = %v, want nil", err)
	}
	if runs.count() != 1 {
		t.Fatalf("开始行 %d 条, want 1：buildTUITaskRunConfig 没有把运行记录落点传下去", runs.count())
	}
}

// TestRunMentionedTUIAgentTaskCarriesTheTaskRunStore 守 @提及 那条 TUI 入口的转发。
//
// 两个 TUI 入口是两处独立的 app.RunTaskOptions 字面量，只接一个的症状是「@某个
// agent 提问的那次运行没有记录」。
func TestRunMentionedTUIAgentTaskCarriesTheTaskRunStore(t *testing.T) {
	t.Parallel()

	runs := &capturingTaskRunStore{}
	cfg := config.Config{Runtime: config.RuntimeConfig{MaxToolRounds: 1}}
	registry := agentregistry.New(map[string]agentregistry.AgentConfig{
		"researcher": {ID: "researcher", Role: "researcher"},
	})
	if _, err := runTUITask(context.Background(), app.New(), tuiTaskRunConfig{
		Config:      cfg,
		Registry:    registry,
		Prompt:      "@researcher 调研一下当前实现",
		DefaultMaas: &cliCaptureMaas{response: "已完成"},
		TaskRuns:    runs,
	}); err != nil {
		t.Fatalf("runTUITask(@researcher) error = %v, want nil", err)
	}
	if runs.count() != 1 {
		t.Fatalf("开始行 %d 条, want 1：runMentionedTUIAgentTask 没有把 cfg.TaskRuns "+
			"转发进 RunTaskOptions", runs.count())
	}
}

// TestRunCommandRecordsItsTaskRun 守 `agent run --prompt` 那条入口的转发
// （newRunCommand 的 RunTaskOptions.TaskRuns 那一行）。
//
// 它跑的是真命令 + 真 SQLite，断言的是**库里真的有这条运行记录**：这一行被删掉之后
// 任务照跑照返回，只是 task_runs 表永远是空的。
func TestRunCommandRecordsItsTaskRun(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "agent.db")
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"storage": {"driver": "sqlite", "path": "`+filepath.ToSlash(dbPath)+`"}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}

	var out bytes.Buffer
	if err := Execute(app.New(), &out, []string{
		"run", "--plain", "--config", configPath, "--prompt", "Task run record CLI check",
	}); err != nil {
		t.Fatalf("Execute(run --prompt) error = %v, want nil", err)
	}

	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) error = %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	// `run --prompt` 不带 SessionID，会话键就是任务号本身；这次只跑了一个任务，
	// 任何一条审计事件的 SubjectID 都是它（与 model_profile 那条用例同源的取法）。
	audits, err := repo.ListAuditEvents(ctx)
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v, want nil", err)
	}
	if len(audits) == 0 {
		t.Fatal("ListAuditEvents() len = 0：拿不到任务号，查不了运行记录")
	}
	runs, err := repo.ListTaskRuns(ctx, audits[0].SubjectID)
	if err != nil {
		t.Fatalf("ListTaskRuns(%q) error = %v, want nil", audits[0].SubjectID, err)
	}
	if len(runs) != 1 {
		t.Fatalf("task_runs 里这条任务有 %d 行, want 1：`agent run --prompt` 没有把 "+
			"persistent.taskRuns 传进 RunTaskOptions", len(runs))
	}
	if runs[0].Status != domain.RunStatusCompleted {
		t.Errorf("终态 = %q, want %q", runs[0].Status, domain.RunStatusCompleted)
	}
}

// TestServeRecordsTaskRunsForBothTaskPaths 守 serve 装配里那两句实参：
// AgentRuntimeResolverConfig.TaskRuns 与 buildDefaultRunnerConfig 的最后一个实参。
//
// 上面那几条守的是**构造点**（那个函数把字段填对了），这一条守的是**调用点**
// （BuildServeService 真的把 taskRuns 送进了那两个构造点）。少送一个不会报错：任务
// 照跑，只是那半边的运行在 task_runs 里一行都没有——而缺的那半边与「没发生过」在库里
// 长得一模一样。本仓两次同形事故（插件工具、审批仲裁者）都是这么漏的。
//
// 两条任务、两条生产路径、一台真 serve：agent_id 为空落默认 runner，agent_id =
// researcher 落 per-agent resolver。
func TestServeRecordsTaskRunsForBothTaskPaths(t *testing.T) {
	t.Parallel()

	fixture := newServeEventsFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	result, err := BuildServeService(ctx, ServeOptions{
		ConfigPath: fixture.configPath,
		Addr:       "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("BuildServeService: %v", err)
	}
	serveDone := make(chan error, 1)
	serveCtx, stopServe := context.WithCancel(ctx)
	go func() { serveDone <- result.Service.Start(serveCtx) }()
	shutdown := sync.OnceFunc(func() {
		stopServe()
		<-serveDone
		result.Close()
	})
	t.Cleanup(shutdown)

	baseURL := result.BaseURL
	if baseURL == "" {
		t.Fatal("ServeResult.BaseURL 是空的：没法向这台 serve 提交任务")
	}
	if err := waitForServeListening(strings.TrimPrefix(baseURL, "http://"), serveDone, serveReadyTimeout); err != nil {
		t.Fatalf("wait for serve: %v", err)
	}

	const defaultTaskID = "serve-runs-default-task"
	const agentTaskID = "serve-runs-agent-task"
	createTask(t, baseURL, defaultTaskID, "")
	waitForTaskDone(t, baseURL, defaultTaskID)
	createTask(t, baseURL, agentTaskID, serveEventsAgentName)
	waitForTaskDone(t, baseURL, agentTaskID)

	// serve 停下来（并 Close 仓储）之后再查库，读到的就是真正落盘的东西。
	shutdown()

	repo, err := storage.OpenSQLite(ctx, fixture.dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("close repo: %v", err)
		}
	})
	for _, probe := range []struct {
		path   string
		taskID string
	}{
		{"默认 runner（buildDefaultRunnerConfig 的实参）", defaultTaskID},
		{"per-agent resolver（AgentRuntimeResolverConfig.TaskRuns 的实参）", agentTaskID},
	} {
		runs, err := repo.ListTaskRuns(ctx, probe.taskID)
		if err != nil {
			t.Fatalf("ListTaskRuns(%q): %v", probe.taskID, err)
		}
		if len(runs) == 0 {
			t.Errorf("%s 这条路跑完的任务在 task_runs 里一行都没有：serve 装配没有把运行"+
				"记录落点送到这条路上", probe.path)
			continue
		}
		if runs[0].Status == domain.RunStatusRunning {
			t.Errorf("%s 的那一行停在 running：终态没有被写回", probe.path)
		}
	}
}
