package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/storage"
	"github.com/stardust/legion-agent/internal/taskledger"
	"github.com/stardust/legion-agent/internal/tool"
)

func TestRunDemoIncludesModelAndToolAudit(t *testing.T) {
	t.Parallel()

	result, err := New().RunDemo(context.Background())
	if err != nil {
		t.Fatalf("RunDemo() error = %v, want nil", err)
	}
	for _, action := range []string{"model_inference_completed", "tool_executed"} {
		if !hasDemoAuditAction(result, action) {
			t.Errorf("RunDemo() audit missing %q: %#v", action, result.AuditActions)
		}
	}
}

func TestRunTaskUsesConfiguredMaasAndPrompt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	maas := adapter.NewRecordingMaas("custom result")
	result, err := New().RunTask(ctx, RunTaskOptions{
		TaskID:    "custom-task",
		Prompt:    "Summarize the runtime",
		Plain:     true,
		Maas:      maas,
		AgentID:   "agent-1",
		Role:      "developer",
		CompanyID: "company-1",
	})
	if err != nil {
		t.Fatalf("RunTask(%q) error = %v, want nil", "custom-task", err)
	}
	if result.TaskID != "custom-task" {
		t.Fatalf("RunTask(%q).TaskID = %q, want %q", "custom-task", result.TaskID, "custom-task")
	}
	if result.Result != "custom result" {
		t.Fatalf("RunTask(%q).Result = %q, want %q", "custom-task", result.Result, "custom result")
	}
	if maas.CallCount() != 1 {
		t.Fatalf("RecordingMaas.CallCount() = %d, want 1", maas.CallCount())
	}
}

func TestRunTaskSanitizesPseudoToolCallOutput(t *testing.T) {
	t.Parallel()

	result, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID: "pseudo-tool-task",
		Prompt: "底层 cache 怎么实现",
		Maas: &captureMaas{
			response: "我先搜索一下。\nsearch_content({\"pattern\":\"cache\",\"directory\":\".\"})",
		},
	})
	if err != nil {
		t.Fatalf("RunTask(pseudo tool) error = %v, want nil", err)
	}
	if bytes.Contains([]byte(result.Result), []byte("search_content")) {
		t.Fatalf("RunTask(pseudo tool).Result = %q, want pseudo tool call removed", result.Result)
	}
	if !bytes.Contains([]byte(result.Result), []byte("当前 Agent 尚未接入对应的真实工具执行能力")) {
		t.Fatalf("RunTask(pseudo tool).Result = %q, want capability boundary", result.Result)
	}
}

func TestRunTaskExecutesBuiltInReadOnlyToolCalls(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cache.go"), []byte("package cache\n// Ccache uses an in-memory map\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(cache.go) error = %v, want nil", err)
	}
	maas := &appToolCallingMaas{}
	result, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID:   "built-in-tool-task",
		Prompt:   "Ccache 怎么实现",
		Maas:     maas,
		ToolRoot: dir,
	})
	if err != nil {
		t.Fatalf("RunTask(built-in tool) error = %v, want nil", err)
	}
	if result.Result != "Ccache uses an in-memory map." {
		t.Fatalf("RunTask(built-in tool).Result = %q, want final answer", result.Result)
	}
	if len(maas.prompts) != 2 {
		t.Fatalf("appToolCallingMaas prompts = %d, want 2", len(maas.prompts))
	}
	if !bytes.Contains([]byte(maas.prompts[1]), []byte("cache.go")) {
		t.Fatalf("second prompt missing search result:\n%s", maas.prompts[1])
	}
}

// toolRootProbingMaas issues a single read_file tool call for path on its
// first Generate call, then returns a final text answer, capturing every
// prompt the runtime built. Round 2's prompt renders the tool result
// ("- <call> success: <content>" or "- <call> failed: <error>", see
// runtime.renderToolResult), so a test can observe whether the call actually
// reached the file (sandbox allowed it) or was rejected by
// WorkspacePathGuard, without a real inference backend. Mirrors
// cli.toolProbingMaas (internal/cli/command_test.go).
type toolRootProbingMaas struct {
	path    string
	prompts []string
}

func (m *toolRootProbingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompts = append(m.prompts, req.Prompt)
	if len(m.prompts) == 1 {
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "probe-1",
			Name:      "read_file",
			Arguments: map[string]string{"path": m.path},
		}}}, nil
	}
	return port.InferenceResponse{Text: "done"}, nil
}

// TestRunTaskSandboxesToolsToWorkingDir guards Task 4's WorkingDir wiring:
// RunTaskOptions.WorkingDir must become the WorkspacePathGuard root, taking
// priority over ToolRoot (mirroring agentToolRoot's task.WorkingDir priority
// in internal/runtime/agent_resolver.go), so a tool call reaching a path
// inside WorkingDir succeeds while a path only inside the fallback ToolRoot
// is rejected as outside the workspace.
func TestRunTaskSandboxesToolsToWorkingDir(t *testing.T) {
	t.Parallel()

	toolRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolRoot, "root-only.txt"), []byte("root-content"), 0o600); err != nil {
		t.Fatalf("WriteFile(root-only.txt) error = %v, want nil", err)
	}
	workingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workingDir, "inside.txt"), []byte("inside-content"), 0o600); err != nil {
		t.Fatalf("WriteFile(inside.txt) error = %v, want nil", err)
	}

	t.Run("path inside working_dir succeeds", func(t *testing.T) {
		t.Parallel()
		maas := &toolRootProbingMaas{path: filepath.Join(workingDir, "inside.txt")}
		if _, err := New().RunTask(context.Background(), RunTaskOptions{
			TaskID:     "wd-sandbox-inside",
			Prompt:     "read inside file",
			Maas:       maas,
			ToolRoot:   toolRoot,
			WorkingDir: workingDir,
		}); err != nil {
			t.Fatalf("RunTask(inside) error = %v, want nil", err)
		}
		if len(maas.prompts) != 2 || !bytes.Contains([]byte(maas.prompts[1]), []byte("success: inside-content")) {
			t.Fatalf("RunTask(inside) prompts = %#v, want tool success reading inside.txt", maas.prompts)
		}
	})

	t.Run("path outside working_dir (inside fallback ToolRoot) is rejected", func(t *testing.T) {
		t.Parallel()
		maas := &toolRootProbingMaas{path: filepath.Join(toolRoot, "root-only.txt")}
		if _, err := New().RunTask(context.Background(), RunTaskOptions{
			TaskID:     "wd-sandbox-escape",
			Prompt:     "try to read outside working_dir",
			Maas:       maas,
			ToolRoot:   toolRoot,
			WorkingDir: workingDir,
		}); err != nil {
			t.Fatalf("RunTask(escape) error = %v, want nil", err)
		}
		if len(maas.prompts) != 2 || !bytes.Contains([]byte(maas.prompts[1]), []byte("failed: "+port.ErrPathOutsideWorkspace.Error())) {
			t.Fatalf("RunTask(escape) prompts = %#v, want tool call rejected as outside workspace", maas.prompts)
		}
	})
}

// TestRunTaskPlanModeRestrictsToReadOnlyTools guards Task 4's Mode wiring:
// RunTaskOptions.Mode must land on the constructed domain.Task.Mode, which
// Runtime.effectiveTools (internal/runtime/runtime.go) already keys off of to
// swap in the read-only tool subset whenever task.Mode == domain.ModePlan —
// so passing Mode through is sufficient and app.RunTask needs no separate
// Subset application. A write_file call issued in Plan mode must therefore
// miss the effective tool set (tool.ErrToolNotFound) and never touch disk.
func TestRunTaskPlanModeRestrictsToReadOnlyTools(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "created.go")
	maas := &appWriteFileMaas{path: target}
	result, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID:   "plan-mode-write-blocked",
		Prompt:   "写一个文件",
		Maas:     maas,
		ToolRoot: dir,
		Mode:     domain.ModePlan,
	})
	if err != nil {
		t.Fatalf("RunTask(plan mode write) error = %v, want nil", err)
	}
	if result.Result != "已创建文件。" {
		t.Fatalf("RunTask(plan mode write).Result = %q, want final answer text", result.Result)
	}
	if len(maas.prompts) != 2 || !bytes.Contains([]byte(maas.prompts[1]), []byte("failed: "+tool.ErrToolNotFound.Error())) {
		t.Fatalf("RunTask(plan mode write) prompts = %#v, want write_file rejected as tool not found", maas.prompts)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("RunTask(plan mode write) created %q on disk, want Plan mode to block the write", target)
	}
}

func TestRunTaskExposesTaskLedgerToolsToMaas(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ledger, err := taskledger.New(taskledger.Config{WorkspaceRoot: root})
	if err != nil {
		t.Fatalf("taskledger.New() error = %v, want nil", err)
	}
	maas := &captureToolsMaas{}
	_, err = New().RunTask(context.Background(), RunTaskOptions{
		TaskID:     "task-ledger-tools",
		Prompt:     "整理任务",
		Maas:       maas,
		ToolRoot:   root,
		TaskLedger: ledger,
	})
	if err != nil {
		t.Fatalf("RunTask(task ledger tools) error = %v, want nil", err)
	}
	if !maas.hasTool("create_task") || !maas.hasTool("read_task") || !maas.hasTool("rebuild_tasks") {
		t.Fatalf("RunTask(task ledger tools) inference tools = %#v, want tasks tools", maas.tools)
	}
}

func TestRunTaskExposesAgentMessageToolsToMaas(t *testing.T) {
	t.Parallel()

	maas := &captureToolsMaas{}
	_, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID:       "agent-message-tools",
		Prompt:       "给 writer 发消息",
		Maas:         maas,
		MessageStore: newAppMemoryAgentMessageStore(),
	})
	if err != nil {
		t.Fatalf("RunTask(agent message tools) error = %v, want nil", err)
	}
	if !maas.hasTool("send_message") || !maas.hasTool("read_messages") {
		t.Fatalf("RunTask(agent message tools) inference tools = %#v, want message tools", maas.tools)
	}
}

func TestRunTaskPassesMaxToolRoundsToRuntime(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cache.go"), []byte("package cache\n// Ccache uses an in-memory map\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(cache.go) error = %v, want nil", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "eviction.go"), []byte("package cache\n// eviction uses LRU\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(eviction.go) error = %v, want nil", err)
	}

	maas := &appMultiRoundToolCallingMaas{}
	result, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID:        "built-in-multi-tool-task",
		Prompt:        "Ccache 怎么实现以及如何淘汰",
		Maas:          maas,
		ToolRoot:      dir,
		MaxToolRounds: 4,
	})
	if err != nil {
		t.Fatalf("RunTask(built-in multi-tool) error = %v, want nil", err)
	}
	if result.Result != "Ccache uses an in-memory map with LRU eviction." {
		t.Fatalf("RunTask(built-in multi-tool).Result = %q, want final answer", result.Result)
	}
	if len(maas.prompts) != 3 {
		t.Fatalf("appMultiRoundToolCallingMaas prompts = %d, want 3", len(maas.prompts))
	}
}

func TestRunTaskPassesContextPrefixToRuntime(t *testing.T) {
	t.Parallel()

	maas := &captureMaas{response: "context result"}
	_, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID:        "context-task",
		Prompt:        "ship context files",
		ContextPrefix: "Agent identity:\nLegion Soul",
		Maas:          maas,
	})
	if err != nil {
		t.Fatalf("RunTask(context prefix) error = %v, want nil", err)
	}
	if !bytes.Contains([]byte(maas.prompt), []byte("Legion Soul")) {
		t.Fatalf("RunTask(context prefix) MaaS prompt = %q, want context prefix", maas.prompt)
	}
	if !bytes.Contains([]byte(maas.prompt), []byte("ship context files")) {
		t.Fatalf("RunTask(context prefix) MaaS prompt = %q, want task input", maas.prompt)
	}
}

func TestRunTaskWritesStructuredTaskLogs(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger, err := observability.NewLogger(&logs, observability.LoggerConfig{})
	if err != nil {
		t.Fatalf("NewLogger(default) error = %v, want nil", err)
	}
	_, err = New().RunTask(context.Background(), RunTaskOptions{
		TaskID: "task-log-app",
		Prompt: "Do not print this prompt in logs",
		Maas:   adapter.NewRecordingMaas("logged result"),
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("RunTask(%q) error = %v, want nil", "task-log-app", err)
	}

	entries := decodeAppLogEntries(t, logs.Bytes())
	assertAppLogEntry(t, entries, "task run started", "task-log-app")
	assertAppLogEntry(t, entries, "task run completed", "task-log-app")
	if bytes.Contains(logs.Bytes(), []byte("Do not print this prompt in logs")) {
		t.Fatalf("RunTask logs contain prompt text, want redacted task metadata only; logs=%s", logs.String())
	}
}

func TestRunTaskRecordsTaskMetrics(t *testing.T) {
	t.Parallel()
	metrics := observability.NewMetricsRecorder(nil)
	_, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID:  "task-metrics-app",
		Prompt:  "record task metrics",
		Maas:    adapter.NewRecordingMaas("metrics result"),
		Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("RunTask(%q) error = %v, want nil", "task-metrics-app", err)
	}
	snapshot := metrics.Snapshot()
	if snapshot.Tasks["running"] != 1 || snapshot.Tasks["done"] != 1 {
		t.Fatalf("RunTask(%q) metrics tasks = %#v, want running=1 done=1", "task-metrics-app", snapshot.Tasks)
	}
	if snapshot.ModelCalls["success"] != 1 {
		t.Fatalf("RunTask(%q) metrics model_calls = %#v, want success=1", "task-metrics-app", snapshot.ModelCalls)
	}
}

func decodeAppLogEntries(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	var entries []map[string]any
	for decoder.More() {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatalf("Decode(app log entry) error = %v, want nil; logs=%s", err, string(data))
		}
		entries = append(entries, entry)
	}
	return entries
}

func assertAppLogEntry(t *testing.T, entries []map[string]any, msg string, taskID string) {
	t.Helper()
	for _, entry := range entries {
		if entry["msg"] != msg {
			continue
		}
		if entry["level"] != slog.LevelInfo.String() {
			t.Fatalf("app log %q level = %#v, want %s", msg, entry["level"], slog.LevelInfo.String())
		}
		if entry["component"] != "app" {
			t.Fatalf("app log %q component = %#v, want app", msg, entry["component"])
		}
		if entry["task_id"] != taskID {
			t.Fatalf("app log %q task_id = %#v, want %s", msg, entry["task_id"], taskID)
		}
		return
	}
	t.Fatalf("app logs missing msg %q; entries=%#v", msg, entries)
}

func TestRunTaskPersistsTaskEventsAndAuditWhenStoresAreConfigured(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	result, err := New().RunTask(ctx, RunTaskOptions{
		TaskID:   "persist-task",
		Prompt:   "Persist this run",
		Maas:     adapter.NewRecordingMaas("persisted result"),
		Events:   storage.NewSQLiteEventBus(repo),
		Audit:    storage.NewSQLiteAuditLog(repo),
		TaskSink: repo,
	})
	if err != nil {
		t.Fatalf("RunTask(persistent) error = %v, want nil", err)
	}
	if result.Result != "persisted result" {
		t.Fatalf("RunTask(persistent).Result = %q, want persisted result", result.Result)
	}
	gotTask, ok, err := repo.GetTask(ctx, "persist-task")
	if err != nil {
		t.Fatalf("GetTask(%q) error = %v, want nil", "persist-task", err)
	}
	if !ok || gotTask.Status != domain.TaskDone {
		t.Fatalf("GetTask(%q) = %#v, %t, want done task", "persist-task", gotTask, ok)
	}
	events, err := repo.ListRuntimeEvents(ctx)
	if err != nil {
		t.Fatalf("ListRuntimeEvents() error = %v, want nil", err)
	}
	if len(events) == 0 {
		t.Fatalf("ListRuntimeEvents() len = 0, want persisted runtime events")
	}
	audits, err := repo.ListAuditEvents(ctx)
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v, want nil", err)
	}
	if len(audits) == 0 {
		t.Fatalf("ListAuditEvents() len = 0, want persisted audit events")
	}
}

// TestRunTaskInjectsSubdirAgentsIntoToolResult verifies the on-demand agents.md
// injection: when the model writes a file inside a subdirectory that has its own
// agents.md, the write_file tool result (fed back as the next inference prompt)
// must carry that directory's local conventions, and must NOT re-inject the
// resident workspace agents.md.
func TestRunTaskInjectsSubdirAgentsIntoToolResult(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// workspace agents.md is resident — must not be re-injected.
	if err := os.WriteFile(filepath.Join(root, "agents.md"), []byte("根目录项目约定"), 0o600); err != nil {
		t.Fatalf("WriteFile(workspace agents.md) error = %v", err)
	}
	fooDir := filepath.Join(root, "internal", "foo")
	if err := os.MkdirAll(fooDir, 0o700); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(fooDir, "agents.md"), []byte("本目录所有函数必须加注释"), 0o600); err != nil {
		t.Fatalf("WriteFile(foo agents.md) error = %v", err)
	}

	maas := &appWriteFileMaas{path: filepath.ToSlash(filepath.Join("internal", "foo", "bar.go"))}
	_, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID:           "e2e-subdir-agents",
		Prompt:           "在 internal/foo 下新建 bar.go",
		Maas:             maas,
		ToolRoot:         root,
		ToolMaxFileChars: 20000,
	})
	if err != nil {
		t.Fatalf("RunTask(subdir agents) error = %v, want nil", err)
	}
	if len(maas.prompts) < 2 {
		t.Fatalf("appWriteFileMaas prompts = %d, want >= 2 (tool round happened)", len(maas.prompts))
	}
	secondPrompt := maas.prompts[1]
	if !bytes.Contains([]byte(secondPrompt), []byte("本目录约定")) {
		t.Fatalf("second prompt missing local-conventions marker (model did not see foo/agents.md):\n%s", secondPrompt)
	}
	if !bytes.Contains([]byte(secondPrompt), []byte("本目录所有函数必须加注释")) {
		t.Fatalf("second prompt missing foo/agents.md content:\n%s", secondPrompt)
	}
	if bytes.Contains([]byte(secondPrompt), []byte("根目录项目约定")) {
		t.Fatalf("second prompt unexpectedly re-injected resident workspace agents.md:\n%s", secondPrompt)
	}
}

// TestRunTaskDoesNotInjectResidentWorkspaceAgentsIntoToolResult writes a
// top-level file; the nearest agents.md is the resident workspace one — the
// tool result fed back to the model must not repeat it.
func TestRunTaskDoesNotInjectResidentWorkspaceAgentsIntoToolResult(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "agents.md"), []byte("根目录项目约定"), 0o600); err != nil {
		t.Fatalf("WriteFile(workspace agents.md) error = %v", err)
	}

	maas := &appWriteFileMaas{path: "top.go"}
	_, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID:           "e2e-root-no-inject",
		Prompt:           "新建 top.go",
		Maas:             maas,
		ToolRoot:         root,
		ToolMaxFileChars: 20000,
	})
	if err != nil {
		t.Fatalf("RunTask(root no inject) error = %v, want nil", err)
	}
	if len(maas.prompts) < 2 {
		t.Fatalf("appWriteFileMaas prompts = %d, want >= 2", len(maas.prompts))
	}
	if bytes.Contains([]byte(maas.prompts[1]), []byte("本目录约定")) || bytes.Contains([]byte(maas.prompts[1]), []byte("根目录项目约定")) {
		t.Fatalf("second prompt re-injected resident workspace agents.md:\n%s", maas.prompts[1])
	}
}

// TestRunTaskStardustAgentsIsResident verifies that .stardust/agents.md written
// at the workspace root is treated as resident and not re-injected when a file
// is written into the .stardust directory itself.
func TestRunTaskStardustAgentsIsResident(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	sdDir := filepath.Join(root, ".stardust")
	if err := os.MkdirAll(sdDir, 0o700); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(sdDir, "agents.md"), []byte(".stardust resident rule"), 0o600); err != nil {
		t.Fatalf("WriteFile(.stardust/agents.md) error = %v", err)
	}

	maas := &appWriteFileMaas{path: filepath.Join(".stardust", "config.go")}
	_, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID:           "e2e-stardust-no-inject",
		Prompt:           "新建 .stardust/config.go",
		Maas:             maas,
		ToolRoot:         root,
		ToolMaxFileChars: 20000,
	})
	if err != nil {
		t.Fatalf("RunTask(.stardust resident) error = %v, want nil", err)
	}
	if len(maas.prompts) < 2 {
		t.Fatalf("appWriteFileMaas prompts = %d, want >= 2", len(maas.prompts))
	}
	if bytes.Contains([]byte(maas.prompts[1]), []byte(".stardust resident rule")) {
		t.Fatalf("second prompt re-injected resident .stardust/agents.md:\n%s", maas.prompts[1])
	}
}

func hasDemoAuditAction(result DemoResult, action string) bool {
	for _, got := range result.AuditActions {
		if got == action {
			return true
		}
	}
	return false
}

type captureMaas struct {
	response string
	prompt   string
}

type captureToolsMaas struct {
	tools []port.InferenceTool
}

type appToolCallingMaas struct {
	prompts []string
}

// appWriteFileMaas emits a single write_file tool call on the first inference,
// then returns a final text answer. It records every prompt so a test can assert
// what the second-round prompt (which embeds the write_file tool result)
// contains.
type appWriteFileMaas struct {
	path    string
	prompts []string
}

func (m *appWriteFileMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompts = append(m.prompts, req.Prompt)
	if len(m.prompts) == 1 {
		return port.InferenceResponse{
			ToolCalls: []domain.ToolCall{{
				ID:        "write-1",
				Name:      "write_file",
				Arguments: map[string]string{"path": m.path, "content": "package foo\n"},
			}},
		}, nil
	}
	return port.InferenceResponse{Text: "已创建文件。"}, nil
}

type appMultiRoundToolCallingMaas struct {
	prompts []string
}

func (m *appToolCallingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompts = append(m.prompts, req.Prompt)
	if len(m.prompts) == 1 {
		return port.InferenceResponse{
			ToolCalls: []domain.ToolCall{{
				ID:        "search-1",
				Name:      "search_content",
				Arguments: map[string]string{"pattern": "Ccache", "directory": ".", "file_types": ".go"},
			}},
		}, nil
	}
	return port.InferenceResponse{Text: "Ccache uses an in-memory map."}, nil
}

func (m *appMultiRoundToolCallingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompts = append(m.prompts, req.Prompt)
	switch len(m.prompts) {
	case 1:
		return port.InferenceResponse{
			ToolCalls: []domain.ToolCall{{
				ID:        "search-1",
				Name:      "search_content",
				Arguments: map[string]string{"pattern": "Ccache", "directory": ".", "file_types": ".go"},
			}},
		}, nil
	case 2:
		return port.InferenceResponse{
			ToolCalls: []domain.ToolCall{{
				ID:        "search-2",
				Name:      "search_content",
				Arguments: map[string]string{"pattern": "eviction", "directory": ".", "file_types": ".go"},
			}},
		}, nil
	default:
		return port.InferenceResponse{Text: "Ccache uses an in-memory map with LRU eviction."}, nil
	}
}

func (m *captureMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompt = req.Prompt
	return port.InferenceResponse{Text: m.response}, nil
}

func (m *captureToolsMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.tools = req.Tools
	return port.InferenceResponse{Text: "tasks tools visible"}, nil
}

func (m *captureToolsMaas) hasTool(name string) bool {
	for _, tool := range m.tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

type appMemoryAgentMessageStore struct{}

func newAppMemoryAgentMessageStore() *appMemoryAgentMessageStore {
	return &appMemoryAgentMessageStore{}
}

func (s *appMemoryAgentMessageStore) SaveAgentMessage(context.Context, domain.AgentMessage) error {
	return nil
}

func (s *appMemoryAgentMessageStore) ListAgentMessages(context.Context, domain.AgentMessageQuery) ([]domain.AgentMessage, error) {
	return nil, nil
}

func (s *appMemoryAgentMessageStore) MarkAgentMessageRead(context.Context, string, time.Time) error {
	return nil
}
