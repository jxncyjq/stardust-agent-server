# 后台子任务持久化（Spec C）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让每一次任务运行落盘，进程退出后按 id 查得到它停在哪（`interrupted`），且不自动重跑。

**Architecture:** 新增 `port.TaskRunStore` 端口（sqlite 实现），Runtime 在任务开始时插一行 `running`、在唯一一处 `defer` 收口写终态；serve 启动时把残留的 `running` 全部扫成 `interrupted`；子任务 id 改 UUID，父子关系落到 `parent_task_id` 列与 `RuntimeEvent.ParentTaskID` 字段。

**Tech Stack:** Go 1.27、modernc sqlite（`internal/storage`）、`github.com/google/uuid`（已在 go.mod，本计划把它从 indirect 提为直接依赖）。

**规格：** [2026-09-18-durable-background-subtasks-design.md](../specs/2026-09-18-durable-background-subtasks-design.md)

## Global Constraints

- **fail-loud 铁律优先于本计划任何写法**：不得返回零值假装正常、不得丢弃 error、不得静默跳过；传播一律 `fmt.Errorf("<动作> <标识>: %w", err)`。
- **写库失败 → 终止任务**。不允许「记日志继续」。
- **冷恢复只到 A 档**：不重跑、不落每轮对话、不做实例存活判定。
- **字段级写回，不整行 UPSERT**：`FinishTaskRun` 不得覆盖 `started_at` / `goal` / `parent_task_id` / `background`。
- **`interrupted` 只由启动扫描写**，运行期任何代码不得写它。
- **封闭枚举不设 `default` 兜底**：解析未知值必须报错。
- **注释是契约**：不写「谁调用它」这类句子。
- 每个任务结束前：`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空。Go 可执行文件是 `D:\DevTools\lang\go\1.27\bin\go`（PATH 上的 `go` 是旧版，一律用绝对路径或先 `export PATH=/d/DevTools/lang/go/1.27/bin:$PATH`）。
- **每个任务必须做变异实证**：把本任务新加的每条规则逐一短路（改成 `&& false`、删掉赋值、`%w` 改 `%v` 等，**变异后 `go vet` 必须干净**），确认有用例转红；变异前先跑一条空变异确认 harness 会报红，还原后用 `git status --porcelain` 确认工作树干净。

---

## 文件结构

| 文件 | 职责 | 本计划中的变化 |
|---|---|---|
| `internal/domain/types.go` | 领域类型 | 新增 `RunStatus` 枚举与 `ParseRunStatus`；`TaskRun` 加 `Status / ParentTaskID / Background / Goal / Error`；`RuntimeEvent` 加 `ParentTaskID` |
| `internal/storage/sqlite.go` | 建表、迁移、`task_runs` 读写 | 加 11 条幂等列迁移、`CurrentSchemaVersion` 12、`SaveTaskRun`/`ListTaskRuns` 补全字段 |
| `internal/storage/task_runs.go`（新建） | 任务运行记录的生命周期写入 | `StartTaskRun` / `FinishTaskRun` / `SweepRunning` / `TaskRunByID` |
| `internal/port/ports.go` | 端口定义 | 新增 `TaskRunStore` |
| `internal/runtime/runtime.go` | 任务主循环 | `Config.TaskRuns` / `Runtime.taskRuns`；`RunTask` 开始插行 + `defer` 收口写终态 |
| `internal/runtime/delegation.go` | 委派与后台子任务 | UUID id、`RunSubTaskAsync` 同步插行、`SubTaskHandle` 暴露 id、子运行时继承 `taskRuns` |
| `internal/cli/subtask_reinject.go` | 子任务结果回注父任务 | 改读 `RuntimeEvent.ParentTaskID`，老事件走老解析 |
| `internal/cli/command.go` | serve 装配 | 注入 `TaskRuns`，启动时调 `SweepRunning` 并记审计与日志 |
| `internal/server/http.go` | HTTP 出口 | `handleGetTaskResult` 先读表、回落事件，响应加 `status` |

---

## Task 1: `RunStatus` 枚举与 `TaskRun` 新字段

**Files:**
- Modify: `internal/domain/types.go:92-124`
- Test: `internal/domain/types_test.go`（不存在则新建）

**Interfaces:**
- Consumes: 无。
- Produces: `domain.RunStatus`、`domain.RunStatusRunning/Completed/Failed/Interrupted`、`domain.ParseRunStatus(string) (RunStatus, error)`、`TaskRun.Status/ParentTaskID/Background/Goal/Error`。

- [ ] **Step 1: 写失败的测试**

`internal/domain/types_test.go`：

```go
package domain

import "testing"

// TestParseRunStatusRefusesAnUnknownValue：未知值必须报错。
//
// 这个枚举的四个值是「一次运行可能处在的全部状态」的完整清单，而它要从数据库的
// TEXT 列读回来——那一列的内容可以被手改，也可能是更新的二进制写下的。猜一个默认
// 值（比如当成 running）会让一行来历不明的记录被下一次启动扫成 interrupted，
// 也就是拿一个编造的状态覆盖掉真实的那个。
func TestParseRunStatusRefusesAnUnknownValue(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"", "RUNNING", "done", "interupted"} {
		if got, err := ParseRunStatus(in); err == nil {
			t.Errorf("ParseRunStatus(%q) = %q, want an error", in, got)
		}
	}
}

// TestParseRunStatusRoundTripsEveryValue：四个值都认得，且 String 与解析互逆。
func TestParseRunStatusRoundTripsEveryValue(t *testing.T) {
	t.Parallel()

	all := []RunStatus{RunStatusRunning, RunStatusCompleted, RunStatusFailed, RunStatusInterrupted}
	for _, want := range all {
		got, err := ParseRunStatus(want.String())
		if err != nil {
			t.Fatalf("ParseRunStatus(%q): %v", want, err)
		}
		if got != want {
			t.Errorf("ParseRunStatus(%q) = %q", want, got)
		}
	}
	// 四个值两两不同：复制粘贴写重了一个字面量，这里会响。
	seen := map[RunStatus]bool{}
	for _, s := range all {
		if seen[s] {
			t.Fatalf("两个常量的字面值都是 %q", s)
		}
		seen[s] = true
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/domain/ -run TestParseRunStatus -v`
Expected: FAIL，`undefined: ParseRunStatus`

- [ ] **Step 3: 实现**

在 `internal/domain/types.go` 的 `StopReason` 常量块之后插入：

```go
// RunStatus 是一次任务运行所处的生命周期状态。空串不是它的取值。
//
// 四个值的分界是「谁写得出它」：running 由运行开始时写；completed 与 failed 由
// 那一次运行自己收尾时写；interrupted 只由启动扫描写——它的含义是「写 running 的
// 那个进程没了，没人知道它跑到哪」，运行期的代码永远不处在能说这句话的位置上。
type RunStatus string

const (
	// RunStatusRunning 是插入时的状态。它只有两种正当结局：被同一个进程改成终态，
	// 或被下一次启动扫成 RunStatusInterrupted。
	RunStatusRunning RunStatus = "running"
	// RunStatusCompleted 是这一次运行走完了它的工具循环。此时 StopReason 必非空。
	RunStatusCompleted RunStatus = "completed"
	// RunStatusFailed 是这一次运行以错误结束：它跑到了一个失败的结论。
	RunStatusFailed RunStatus = "failed"
	// RunStatusInterrupted 是进程消失在这次运行中间。与 RunStatusFailed 分开，
	// 因为「跑出了失败」与「没人知道它跑到哪」对读的人是两件事。
	RunStatusInterrupted RunStatus = "interrupted"
)

// String 返回这个状态的字面值。
func (s RunStatus) String() string { return string(s) }

// ParseRunStatus 把一个字面值解析成 RunStatus，不认得就报错。
func ParseRunStatus(s string) (RunStatus, error) {
	switch RunStatus(s) {
	case RunStatusRunning:
		return RunStatusRunning, nil
	case RunStatusCompleted:
		return RunStatusCompleted, nil
	case RunStatusFailed:
		return RunStatusFailed, nil
	case RunStatusInterrupted:
		return RunStatusInterrupted, nil
	default:
		return "", fmt.Errorf("unknown run status %q; the four values are %q, %q, %q and %q",
			s, RunStatusRunning, RunStatusCompleted, RunStatusFailed, RunStatusInterrupted)
	}
}
```

`TaskRun` 结构体末尾（`GeneratedFiles` 之后）追加：

```go
	// Status 是这次运行所处的状态。零值空串不是合法状态；每个造出 TaskRun 的
	// 地方都要显式填它。
	Status RunStatus `json:"status,omitempty"`
	// ParentTaskID 是派出这次运行的父任务；空串表示它不是子任务。父子关系存在
	// 这里而不编码进 ID，是因为 ID 要能换成 UUID 而关系要能被查询。
	ParentTaskID string `json:"parent_task_id,omitempty"`
	// Background 报告这次运行是不是后台子任务（RunSubTaskAsync 起的）。
	Background bool `json:"background,omitempty"`
	// Goal 是子任务的目标原文，用来让一条中断记录读起来知道它在干什么。它不是
	// 重跑用的输入：重跑不在本设计范围内。
	Goal string `json:"goal,omitempty"`
	// Error 是 Status 为 RunStatusFailed 时的错误摘要，其余状态为空。
	Error string `json:"error,omitempty"`
```

（`fmt` 已在该文件的 import 里；若不在，补上。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/domain/ -run TestParseRunStatus -v`
Expected: PASS

- [ ] **Step 5: 变异实证**

逐条短路、确认转红（每条变异后先 `go vet ./internal/domain/` 必须干净）：

1. `ParseRunStatus` 的 `default` 改成 `return RunStatusRunning, nil` → `TestParseRunStatusRefusesAnUnknownValue` 必须红。
2. `RunStatusFailed` 的字面值改成 `"completed"` → `TestParseRunStatusRoundTripsEveryValue` 必须红。

还原后 `git status --porcelain` 必须为空。

- [ ] **Step 6: 提交**

```bash
git add internal/domain/types.go internal/domain/types_test.go
git commit -m "feat(domain): 任务运行状态四值枚举与 TaskRun 新字段"
```

---

## Task 2: `task_runs` 补列，写读全字段往返

**Files:**
- Modify: `internal/storage/sqlite.go:63`（`CurrentSchemaVersion`）、`:1131-1180`（`SaveTaskRun`/`ListTaskRuns`）、`:1906-1945`（`columnMigrations`）、`:2176`（建表语句）
- Test: `internal/storage/task_runs_test.go`（新建）

**Interfaces:**
- Consumes: Task 1 的 `domain.RunStatus` 与 `TaskRun` 新字段。
- Produces: `task_runs` 表含全部列；`SaveTaskRun` / `ListTaskRuns` 覆盖 `domain.TaskRun` 的每个字段。

- [ ] **Step 1: 写失败的测试**

`internal/storage/task_runs_test.go`：

```go
package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

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
		Goal:            "把 A 查清楚",
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
	if diff := cmp.Diff(want, runs[0]); diff != "" {
		t.Errorf("往返之后字段不一致 (-want +got):\n%s", diff)
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/storage/ -run 'TestTaskRun|TestColumnMigrations' -v`
Expected: FAIL（`unknown field Status in struct literal` 之类的编译错误；Task 1 已加字段时则是 `no such column: status`）

- [ ] **Step 3: 实现**

3a. `internal/storage/sqlite.go` 建表语句（`:2176` 一带）改成新库直接带全部列：

```go
	`CREATE TABLE IF NOT EXISTS task_runs (
		id TEXT PRIMARY KEY,
		task_id TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		started_at TEXT NOT NULL,
		ended_at TEXT NOT NULL,
		result TEXT NOT NULL
	)`,
```

**保持原样不动**。新列一律走 `columnMigrations`，让新库与老库经过同一条路——两条路会分叉，而分叉的那一天没人会发现。

3b. `columnMigrations` 末尾追加：

```go
	// task_runs 的这一批列有两个来源：一半补上 domain.TaskRun 早就有、表里却没有
	// 的字段（写进去会被静默丢掉），另一半是运行状态本身。status 的默认值是
	// completed 而不是 running：老行都带着 ended_at，它们是已经结束的运行，默认成
	// running 会让第一次启动扫描把全部历史数据标成中断。
	{table: "task_runs", column: "status", stmt: `ALTER TABLE task_runs ADD COLUMN status TEXT NOT NULL DEFAULT 'completed'`},
	{table: "task_runs", column: "parent_task_id", stmt: `ALTER TABLE task_runs ADD COLUMN parent_task_id TEXT NOT NULL DEFAULT ''`},
	{table: "task_runs", column: "background", stmt: `ALTER TABLE task_runs ADD COLUMN background INTEGER NOT NULL DEFAULT 0`},
	{table: "task_runs", column: "goal", stmt: `ALTER TABLE task_runs ADD COLUMN goal TEXT NOT NULL DEFAULT ''`},
	{table: "task_runs", column: "error", stmt: `ALTER TABLE task_runs ADD COLUMN error TEXT NOT NULL DEFAULT ''`},
	{table: "task_runs", column: "reasoning_summary", stmt: `ALTER TABLE task_runs ADD COLUMN reasoning_summary TEXT NOT NULL DEFAULT ''`},
	{table: "task_runs", column: "prompt_tokens", stmt: `ALTER TABLE task_runs ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`},
	{table: "task_runs", column: "completion_tokens", stmt: `ALTER TABLE task_runs ADD COLUMN completion_tokens INTEGER NOT NULL DEFAULT 0`},
	{table: "task_runs", column: "cached_tokens", stmt: `ALTER TABLE task_runs ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`},
	{table: "task_runs", column: "total_tokens", stmt: `ALTER TABLE task_runs ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`},
	{table: "task_runs", column: "generated_files", stmt: `ALTER TABLE task_runs ADD COLUMN generated_files TEXT NOT NULL DEFAULT ''`},
```

3c. `CurrentSchemaVersion` 改成 `12`，并在它上方的版本说明块末尾追加一行：

```go
// Version 12 gave task_runs the columns a domain.TaskRun actually has (its
// reasoning summary, token counts and generated files were being dropped on
// write) plus the run's lifecycle state: status, parent_task_id, background,
// goal and error.
```

3d. `SaveTaskRun` 改成全字段（`generated_files` 以 JSON 数组落盘；空切片落 `""`，读回来是 nil，与 `omitempty` 的语义一致）：

```go
func (r *SQLiteRepository) SaveTaskRun(ctx context.Context, run domain.TaskRun) error {
	files, err := marshalGeneratedFiles(run.GeneratedFiles)
	if err != nil {
		return fmt.Errorf("save task run %q: %w", run.ID, err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO task_runs (
			id, task_id, agent_id, started_at, ended_at, result, stop_reason,
			status, parent_task_id, background, goal, error,
			reasoning_summary, prompt_tokens, completion_tokens, cached_tokens, total_tokens, generated_files
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			task_id = excluded.task_id,
			agent_id = excluded.agent_id,
			started_at = excluded.started_at,
			ended_at = excluded.ended_at,
			result = excluded.result,
			stop_reason = excluded.stop_reason,
			status = excluded.status,
			parent_task_id = excluded.parent_task_id,
			background = excluded.background,
			goal = excluded.goal,
			error = excluded.error,
			reasoning_summary = excluded.reasoning_summary,
			prompt_tokens = excluded.prompt_tokens,
			completion_tokens = excluded.completion_tokens,
			cached_tokens = excluded.cached_tokens,
			total_tokens = excluded.total_tokens,
			generated_files = excluded.generated_files
	`, run.ID, run.TaskID, run.AgentID, formatTime(run.StartedAt), formatTime(run.EndedAt), run.Result,
		string(run.StopReason), string(run.Status), run.ParentTaskID, run.Background, run.Goal, run.Error,
		run.ReasoningSummary, run.PromptTokens, run.CompletionTokens, run.CachedTokens, run.TotalTokens, files)
	if err != nil {
		return fmt.Errorf("save task run %q: %w", run.ID, err)
	}
	return nil
}

// marshalGeneratedFiles 把生成文件清单编码成落盘用的 JSON 数组；空清单编码成空串。
//
// 空串与 "[]" 在读回时都还原成 nil，所以这里选前者只是为了让老行的默认值（空串）
// 与新写的空清单在库里长得一样，不给一个「看得出是哪个版本写的」的痕迹留位置。
func marshalGeneratedFiles(files []string) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	data, err := json.Marshal(files)
	if err != nil {
		return "", fmt.Errorf("encode generated files: %w", err)
	}
	return string(data), nil
}

// unmarshalGeneratedFiles 是 marshalGeneratedFiles 的逆。空串是「没有生成文件」，
// 其余一律按 JSON 解；解不动就报错，不退化成空清单——那会把一次损坏读成「这次运行
// 什么文件都没写」。
func unmarshalGeneratedFiles(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var files []string
	if err := json.Unmarshal([]byte(s), &files); err != nil {
		return nil, fmt.Errorf("decode generated files %q: %w", s, err)
	}
	return files, nil
}
```

3e. `ListTaskRuns` 的 SELECT 与 Scan 同步补全，并且 **status 经 `domain.ParseRunStatus`**：

```go
func (r *SQLiteRepository) ListTaskRuns(ctx context.Context, taskID string) ([]domain.TaskRun, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, agent_id, started_at, ended_at, result, stop_reason,
		       status, parent_task_id, background, goal, error,
		       reasoning_summary, prompt_tokens, completion_tokens, cached_tokens, total_tokens, generated_files
		FROM task_runs
		WHERE task_id = ?
		ORDER BY started_at, id
	`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task runs for %q: %w", taskID, err)
	}
	defer rows.Close()

	var runs []domain.TaskRun
	for rows.Next() {
		run, err := scanTaskRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list task runs for %q: %w", taskID, err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list task runs for %q: %w", taskID, err)
	}
	return runs, nil
}

// taskRunScanner 是 *sql.Row 与 *sql.Rows 都满足的那一点点接口，让单行读与多行读
// 共用同一个 scanTaskRun：列清单写两遍，迟早会有一遍漏掉新列。
type taskRunScanner interface {
	Scan(dest ...any) error
}

func scanTaskRun(sc taskRunScanner) (domain.TaskRun, error) {
	var run domain.TaskRun
	var startedAt, endedAt, stopReason, status, files string
	if err := sc.Scan(&run.ID, &run.TaskID, &run.AgentID, &startedAt, &endedAt, &run.Result, &stopReason,
		&status, &run.ParentTaskID, &run.Background, &run.Goal, &run.Error,
		&run.ReasoningSummary, &run.PromptTokens, &run.CompletionTokens, &run.CachedTokens, &run.TotalTokens,
		&files); err != nil {
		return domain.TaskRun{}, fmt.Errorf("scan task run: %w", err)
	}
	parsedStartedAt, err := parseTime(startedAt)
	if err != nil {
		return domain.TaskRun{}, fmt.Errorf("parse task run %q started_at: %w", run.ID, err)
	}
	parsedEndedAt, err := parseTime(endedAt)
	if err != nil {
		return domain.TaskRun{}, fmt.Errorf("parse task run %q ended_at: %w", run.ID, err)
	}
	parsedStatus, err := domain.ParseRunStatus(status)
	if err != nil {
		return domain.TaskRun{}, fmt.Errorf("task run %q: %w", run.ID, err)
	}
	parsedFiles, err := unmarshalGeneratedFiles(files)
	if err != nil {
		return domain.TaskRun{}, fmt.Errorf("task run %q: %w", run.ID, err)
	}
	run.StartedAt = parsedStartedAt
	run.EndedAt = parsedEndedAt
	run.StopReason = domain.StopReason(stopReason)
	run.Status = parsedStatus
	run.GeneratedFiles = parsedFiles
	return run, nil
}
```

（`encoding/json` 已在 `sqlite.go` 的 import 里；不在就补。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/storage/ -run 'TestTaskRun|TestColumnMigrations' -v`
Expected: PASS

再跑一次全包，确认没碰坏迁移守卫：
Run: `go test ./internal/storage/`
Expected: ok

- [ ] **Step 5: 变异实证**

1. `SaveTaskRun` 的 `generated_files` 参数改成常量 `""` → `TestTaskRunRoundTripsEveryField` 必须红。
2. `scanTaskRun` 里 `run.Status = parsedStatus` 删掉（改为不赋值，用 `_ = parsedStatus` 保持 vet 干净）→ 往返用例必须红。
3. `ParseRunStatus` 那次调用改成 `run.Status = domain.RunStatus(status)` → `TestTaskRunRefusesAnUnknownStatusOnRead` 必须红。
4. `status` 迁移的默认值改成 `'running'` → `TestColumnMigrationsAreIdempotent` 必须红。

- [ ] **Step 6: 提交**

```bash
git add internal/storage/sqlite.go internal/storage/task_runs_test.go
git commit -m "feat(storage): task_runs 补齐 TaskRun 的全部字段与运行状态列"
```

---

## Task 3: `TaskRunStore` 端口与生命周期写入

**Files:**
- Create: `internal/storage/task_runs.go`
- Modify: `internal/port/ports.go:147` 之后
- Test: `internal/storage/task_runs_test.go`（追加）

**Interfaces:**
- Consumes: Task 2 的 `scanTaskRun` / `marshalGeneratedFiles` / `SaveTaskRun`。
- Produces:
  - `port.TaskRunStore` 接口，方法 `StartTaskRun(ctx, domain.TaskRun) error`、`FinishTaskRun(ctx, domain.TaskRun) error`、`SweepRunning(ctx, time.Time) (int, error)`、`TaskRunByID(ctx, string) (domain.TaskRun, bool, error)`、`ListTaskRuns(ctx, string) ([]domain.TaskRun, error)`
  - `*storage.SQLiteRepository` 实现这个接口。

- [ ] **Step 1: 写失败的测试**

追加到 `internal/storage/task_runs_test.go`：

```go
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
		ID:           "run-1",
		EndedAt:      time.Date(2026, 9, 18, 10, 1, 0, 0, time.UTC),
		Result:       "答案",
		StopReason:   domain.StopReasonCompleted,
		Status:       domain.RunStatusCompleted,
		TotalTokens:  66,
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/storage/ -run 'TestFinishTaskRun|TestStartTaskRun|TestSweepRunning' -v`
Expected: FAIL，`repo.StartTaskRun undefined`

- [ ] **Step 3: 实现**

新建 `internal/storage/task_runs.go`：

```go
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

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
		string(run.Status), run.ParentTaskID, run.Background, run.Goal)
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
// 前提不成立说明两个写入点之间断了，而那正是这份记录要防的事。interrupted 也报错，
// 理由见 domain.RunStatus。
func (r *SQLiteRepository) FinishTaskRun(ctx context.Context, run domain.TaskRun) error {
	switch run.Status {
	case domain.RunStatusCompleted, domain.RunStatusFailed:
	default:
		return fmt.Errorf("finish task run %q: status is %q; only %q and %q end a run here (%q is written "+
			"only by the startup sweep)", run.ID, run.Status,
			domain.RunStatusCompleted, domain.RunStatusFailed, domain.RunStatusInterrupted)
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
```

`internal/port/ports.go`（`AuditLog` 之后）：

```go
// TaskRunStore 是任务运行记录的落点：一次运行开始时插一行，结束时写回终态，
// 启动时把上一次进程留下的 running 记录摆正。
//
// 它与 AuditLog、EventBus 并列而不是合并进去，因为三者回答的是不同的问题：审计
// 记「发生过什么」，事件总线记「谁该被通知」，而这里记「这一次运行现在处在哪个
// 状态」——只有第三个需要被就地改写。
type TaskRunStore interface {
	StartTaskRun(ctx context.Context, run domain.TaskRun) error
	FinishTaskRun(ctx context.Context, run domain.TaskRun) error
	// SweepRunning 把所有 running 记录改成 interrupted，返回改了几条。只在启动时
	// 调用一次。
	SweepRunning(ctx context.Context, at time.Time) (int, error)
	// TaskRunByID 取一条运行记录；found 为 false 表示没有它，与查询失败分开。
	TaskRunByID(ctx context.Context, runID string) (run domain.TaskRun, found bool, err error)
	ListTaskRuns(ctx context.Context, taskID string) ([]domain.TaskRun, error)
}
```

（`internal/port/ports.go` 若未 import `time`，补上。）

在 `internal/storage/task_runs.go` 末尾加一行编译期断言：

```go
// 编译期确认这个实现满足端口。接线错了要在这里响，不是在 serve 装配那一行。
var _ port.TaskRunStore = (*SQLiteRepository)(nil)
```

（相应地 import `internal/port`。若 `storage` 包按既有约定不 import `port`，把这行断言改放 `internal/port/ports_test.go` 或 `internal/cli` 的装配处，**不要**为此制造反向依赖——照该仓既有做法执行。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/storage/ ./internal/port/`
Expected: ok

- [ ] **Step 5: 变异实证**

1. `FinishTaskRun` 的 UPDATE 加上 `started_at = ?` 与 `goal = ?` 并传 `run` 的对应字段 → `TestFinishTaskRunDoesNotOverwriteTheOpeningFields` 必须红。
2. `affected == 0` 那条判断改成 `affected < 0` → `TestFinishTaskRunRefusesAnUnknownRun` 必须红。
3. `FinishTaskRun` 的 `switch` 里把 `domain.RunStatusInterrupted` 也算作合法 → `TestFinishTaskRunRefusesInterrupted` 必须红。
4. `StartTaskRun` 的状态校验改成 `&& false` → `TestStartTaskRunRefusesANonRunningStatus` 必须红。
5. `SweepRunning` 的 `WHERE status = ?` 去掉 → `TestSweepRunningOnlyTouchesRunningRows` 必须红。

- [ ] **Step 6: 提交**

```bash
git add internal/storage/task_runs.go internal/storage/task_runs_test.go internal/port/ports.go
git commit -m "feat(storage): 任务运行记录的生命周期写入与 TaskRunStore 端口"
```

---

## Task 4: Runtime 接线——开始插行、收口写终态

**Files:**
- Modify: `internal/runtime/runtime.go`（`Config` 结构体、`Runtime` 结构体、`NewRuntime`、`RunTask:661`、`finishRun:1291`）
- Modify: `internal/runtime/delegation.go:190-205`（`newSubRuntime` 的字段搬运）
- Test: `internal/runtime/task_run_persistence_test.go`（新建）

**Interfaces:**
- Consumes: `port.TaskRunStore`（Task 3）、`domain.RunStatus`（Task 1）。
- Produces: `Config.TaskRuns port.TaskRunStore`；`RunTask` 保证「每一条出口恰好落一次终态」。

**关键约束（本任务最容易坏的地方）：** `RunTask` 主体在 `runtime.go:661`–`:1245` 之间有 **20 余处** `return domain.TaskRun{}, err`。逐条手改必然漏——用**一个 `defer` 收口**，让「无论怎么出去都写一次终态」在结构上成立。收口函数必须幂等：成功路径已经写过 `completed` 就不再写。

- [ ] **Step 1: 写失败的测试**

`internal/runtime/task_run_persistence_test.go`（沿用该包既有的假模型/假存储夹具；下面的 `newTestRuntime` 指该包测试里已有的构造助手，若名字不同，按实际的来，**不要另造一套**）：

```go
package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// recordingTaskRuns 记下每一次写入，并可以让指定的那一步失败。
type recordingTaskRuns struct {
	mu        sync.Mutex
	started   []domain.TaskRun
	finished  []domain.TaskRun
	failStart error
	failEnd   error
}

func (r *recordingTaskRuns) StartTaskRun(_ context.Context, run domain.TaskRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failStart != nil {
		return r.failStart
	}
	r.started = append(r.started, run)
	return nil
}

func (r *recordingTaskRuns) FinishTaskRun(_ context.Context, run domain.TaskRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failEnd != nil {
		return r.failEnd
	}
	r.finished = append(r.finished, run)
	return nil
}

func (r *recordingTaskRuns) SweepRunning(context.Context, time.Time) (int, error) { return 0, nil }

func (r *recordingTaskRuns) TaskRunByID(context.Context, string) (domain.TaskRun, bool, error) {
	return domain.TaskRun{}, false, nil
}

func (r *recordingTaskRuns) ListTaskRuns(context.Context, string) ([]domain.TaskRun, error) {
	return nil, nil
}

func (r *recordingTaskRuns) snapshot() (started, finished []domain.TaskRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.TaskRun(nil), r.started...), append([]domain.TaskRun(nil), r.finished...)
}

// TestRunTaskRecordsAStartThenACompletion：正常一轮，落一条 running 再落一条
// completed，终态带着结果与 usage。
func TestRunTaskRecordsAStartThenACompletion(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := newTestRuntime(t, withTaskRuns(runs), withModelAnswer("OK"))
	if _, err := rt.RunTask(context.Background(), testAgent(), testTask("task-1")); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	started, finished := runs.snapshot()
	if len(started) != 1 || started[0].Status != domain.RunStatusRunning {
		t.Fatalf("开始写入 = %+v, want 一条 running", started)
	}
	if len(finished) != 1 {
		t.Fatalf("终态写入 %d 次, want 恰好 1 次", len(finished))
	}
	if finished[0].Status != domain.RunStatusCompleted || finished[0].Result != "OK" {
		t.Errorf("终态 = %+v", finished[0])
	}
	if finished[0].ID != started[0].ID {
		t.Errorf("终态写的是另一条记录：start=%q finish=%q", started[0].ID, finished[0].ID)
	}
}

// TestRunTaskDoesNotStartWhenTheOpeningWriteFails：开始那一步写不进去，任务就不
// 开始——模型一次都不许被调用。
//
// 断言「模型没被调用」而不只是「返回了错误」：先跑后记的实现同样会返回错误，但它
// 已经把活干了一半，而那一半没有任何记录。
func TestRunTaskDoesNotStartWhenTheOpeningWriteFails(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{failStart: errors.New("disk full")}
	model := newCountingModel("OK")
	rt := newTestRuntime(t, withTaskRuns(runs), withModel(model))
	_, err := rt.RunTask(context.Background(), testAgent(), testTask("task-1"))
	if err == nil {
		t.Fatal("开始写入失败了，RunTask 却成功了")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("错误链里没有写库失败的原因：%v", err)
	}
	if n := model.calls(); n != 0 {
		t.Errorf("模型被调用了 %d 次；开始没落盘就不该开跑", n)
	}
}

// TestRunTaskFailsWhenTheClosingWriteFails：结果算出来了但终态写不进去，任务整体
// 报错。
//
// 按拍板：写库失败终止任务。返回一个「结果有了但没人知道」的成功，正是这份记录
// 存在要防的事。
func TestRunTaskFailsWhenTheClosingWriteFails(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{failEnd: errors.New("db is locked")}
	rt := newTestRuntime(t, withTaskRuns(runs), withModelAnswer("OK"))
	_, err := rt.RunTask(context.Background(), testAgent(), testTask("task-1"))
	if err == nil {
		t.Fatal("终态写不进去，RunTask 却成功了")
	}
	if !strings.Contains(err.Error(), "db is locked") {
		t.Errorf("错误链里没有写库失败的原因：%v", err)
	}
}

// TestRunTaskRecordsAFailedRunOnEveryErrorExit：出错的那条路也必须落终态。
//
// 这是本任务最容易坏的地方：RunTask 有二十多处错误出口，逐条手改必然漏一条，而漏
// 掉的那条留下的是一行永远 running 的记录——下一次启动会把它扫成 interrupted，于是
// 一次真实的失败被报成了「进程没了」。
func TestRunTaskRecordsAFailedRunOnEveryErrorExit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opt  testRuntimeOption
	}{
		{"模型调用失败", withModelError(errors.New("upstream 503"))},
		{"事件发布失败", withFailingEvents(errors.New("event sink down"))},
		{"审计写入失败", withFailingAudit(errors.New("audit sink down"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runs := &recordingTaskRuns{}
			rt := newTestRuntime(t, withTaskRuns(runs), tc.opt)
			if _, err := rt.RunTask(context.Background(), testAgent(), testTask("task-1")); err == nil {
				t.Fatal("这一路本该失败")
			}
			started, finished := runs.snapshot()
			if len(started) != 1 {
				t.Fatalf("开始写入 %d 次, want 1", len(started))
			}
			if len(finished) != 1 {
				t.Fatalf("终态写入 %d 次, want 恰好 1 次——出错的路径也要落终态", len(finished))
			}
			if finished[0].Status != domain.RunStatusFailed {
				t.Errorf("终态 status = %q, want failed", finished[0].Status)
			}
			if finished[0].Error == "" {
				t.Error("failed 的记录没有错误摘要，读的人无从知道它为什么失败")
			}
		})
	}
}

// TestRunTaskNeverWritesInterrupted：运行期永远不写 interrupted。
func TestRunTaskNeverWritesInterrupted(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := newTestRuntime(t, withTaskRuns(runs), withModelError(errors.New("boom")))
	_, _ = rt.RunTask(context.Background(), testAgent(), testTask("task-1"))
	_, finished := runs.snapshot()
	for _, run := range finished {
		if run.Status == domain.RunStatusInterrupted {
			t.Fatal("运行期写出了 interrupted；那句话只有启动扫描说得出口")
		}
	}
}
```

> 实施说明：`withTaskRuns` / `withModelAnswer` / `withModel` / `withModelError` / `withFailingEvents` / `withFailingAudit` / `newCountingModel` / `testAgent` / `testTask` 这些助手，**先在该包既有测试里找**（`internal/runtime/*_test.go` 已有假模型与假事件总线）。已有就复用，缺哪个补哪个，不要另起一套夹具。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run TestRunTask -v`
Expected: FAIL，`withTaskRuns undefined` 或 `Config has no field TaskRuns`

- [ ] **Step 3: 实现**

3a. `Config` 加字段（放在 `SessionEvents` 附近）：

```go
	// TaskRuns 是任务运行记录的落点。nil 表示这个部署不落盘运行记录（CLI 的
	// 一次性执行、以及绝大多数测试就是这个形状），此时 RunTask 不写任何记录，
	// 也不因此失败——这是契约里写明的可选，不是接线漏了。serve 装配一定给它
	// 一个非 nil 值。
	TaskRuns port.TaskRunStore
```

`Runtime` 结构体加 `taskRuns port.TaskRunStore`，`NewRuntime` 里 `taskRuns: cfg.TaskRuns`。

3b. `newSubRuntime`（`delegation.go:190-205` 一带，与 `audit: r.audit, events: r.events` 并列）加一行 `taskRuns: r.taskRuns`。

> **子运行时必须继承它**，否则后台子任务的 `RunTask` 拿到的是 nil，落盘在子任务这条路上整条消失——而那正是本设计的目标场景。

3c. `RunTask` 顶部（`defer end()` 之后、`started := time.Now()` 之前）插入开始写入与收口：

```go
	started := time.Now()

	// 运行记录先落盘，再干活。反过来先跑后记，崩在中间就什么都不剩;而落盘失败
	// 时任务不开始——一个状态没落盘的任务没有资格自称可续（规格第五节）。
	runRecord := domain.TaskRun{
		ID:           task.ID + ":run-1",
		TaskID:       task.ID,
		AgentID:      agent.ID,
		StartedAt:    started,
		Status:       domain.RunStatusRunning,
		ParentTaskID: task.ParentTaskID,
		Background:   task.Background,
		Goal:         task.Goal,
	}
	if r.taskRuns != nil {
		if err := r.taskRuns.StartTaskRun(ctx, runRecord); err != nil {
			return domain.TaskRun{}, fmt.Errorf("record the start of task %s: %w", task.ID, err)
		}
	}

	// 终态写入收口在这一处。RunTask 有二十多条错误出口，逐条去记得写一次终态是
	// 守不住的；这里用具名返回值 + defer，让「无论从哪条路出去都恰好落一次终态」
	// 由控制流本身保证。
	//
	// finished 由成功路径置位：那条路自己写 completed（它手里那份 TaskRun 带着
	// 结果与 usage，这里没有），于是这个 defer 对它是空操作。
	var finished bool
	defer func() {
		if r.taskRuns == nil || finished {
			return
		}
		ending := runRecord
		ending.EndedAt = time.Now()
		ending.Status = domain.RunStatusFailed
		if runErr != nil {
			ending.Error = runErr.Error()
		} else {
			// 没有错误却走到这里，说明有一条出口既没报错也没写终态——一种接线
			// 缺口。记成 failed 并说明，比留下一行 running 强：后者会在下一次
			// 启动被扫成 interrupted，把一个代码缺陷伪装成一次进程消失。
			ending.Error = "task run ended without an error and without a recorded completion"
		}
		if err := r.taskRuns.FinishTaskRun(ctx, ending); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("record the end of task %s: %w", task.ID, err))
		}
	}()
```

同时把 `RunTask` 的签名改成具名返回值：

```go
func (r *Runtime) RunTask(ctx context.Context, agent domain.Agent, task domain.Task) (runResult domain.TaskRun, runErr error) {
```

> 函数体内原有的 `return domain.TaskRun{}, err` 等写法不必改写成裸 `return`——具名返回值同样会被赋值，`defer` 读得到。

3d. 成功路径在 `finishRun` 返回之后写 `completed`（`runtime.go:1235` 一带）：

```go
	run, err := r.finishRun(ctx, requestID, agent, task, st)
	if err != nil {
		r.closeTurnOnError(ctx, task, rec, err)
		return domain.TaskRun{}, err
	}
	if r.taskRuns != nil {
		completion := run
		completion.Status = domain.RunStatusCompleted
		completion.ParentTaskID = runRecord.ParentTaskID
		completion.Background = runRecord.Background
		completion.Goal = runRecord.Goal
		if err := r.taskRuns.FinishTaskRun(ctx, completion); err != nil {
			return domain.TaskRun{}, fmt.Errorf("record the completion of task %s: %w", task.ID, err)
		}
		finished = true
	} else {
		finished = true
	}
```

> **注意**：`FinishTaskRun` 失败时 `finished` 仍为 false，于是上面那个 `defer` 会再写一次 `failed`——这是刻意的：结果没能落盘，这一次运行对外就不是 completed。第二次写入若也失败，两条错误由 `errors.Join` 一起带出。

3e. `domain.Task` 加 `ParentTaskID` / `Background` / `Goal` 三个字段（若 `Task` 尚无它们），在 `internal/domain/types.go` 的 `Task` 结构体里，注释写明「由委派路径填写，直连任务为空」。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ -run TestRunTask -v`
Expected: PASS

Run: `go test ./internal/runtime/`
Expected: ok（既有用例不得因为 `TaskRuns` 为 nil 而失败——nil 是合法形状）

- [ ] **Step 5: 变异实证**

1. 删掉 `defer` 里的 `FinishTaskRun` 调用 → `TestRunTaskRecordsAFailedRunOnEveryErrorExit` 必须红。
2. 开始写入的错误改成忽略（`_ = r.taskRuns.StartTaskRun(...)`）→ `TestRunTaskDoesNotStartWhenTheOpeningWriteFails` 必须红。
3. 成功路径的 `FinishTaskRun` 错误改成只记不返 → `TestRunTaskFailsWhenTheClosingWriteFails` 必须红。
4. `defer` 里的 `ending.Status` 改成 `domain.RunStatusInterrupted` → `TestRunTaskNeverWritesInterrupted` 必须红（`FinishTaskRun` 本身也会拒绝，两道都要在）。
5. `newSubRuntime` 里新加的 `taskRuns: r.taskRuns` 删掉 → 见 Task 5 的用例，那里必须红。

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/runtime.go internal/runtime/delegation.go internal/runtime/task_run_persistence_test.go internal/domain/types.go
git commit -m "feat(runtime): 任务开始即落盘，终态写入收口到唯一一处 defer"
```

---

## Task 5: 后台子任务——UUID、同步插行、可寻址句柄

**Files:**
- Modify: `internal/runtime/delegation.go:74-76`（`SubTaskHandle`）、`:362-385`（`runChild`）、`:447-520`（`RunSubTaskAsync`）、`:527-532`（`nextSubTaskID`）
- Modify: `go.mod`（`github.com/google/uuid` 由 indirect 提为直接依赖）
- Test: `internal/runtime/delegation_persistence_test.go`（新建）

**Interfaces:**
- Consumes: Task 4 的 `Runtime.taskRuns`、`domain.Task` 的新字段。
- Produces: `SubTaskHandle{TaskID string}` 里的 `TaskID` 是 UUID；`RunSubTaskAsync` 在 `go` 之前同步插行。

- [ ] **Step 1: 写失败的测试**

`internal/runtime/delegation_persistence_test.go`：

```go
package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/stardust/legion-agent/internal/domain"
)

// TestSubTaskIDsAreUUIDs：子任务 id 不再由进程内计数器拼出来。
//
// 老形态 "<父>:sub-<n>" 的 n 来自一个内存计数器，重启后从 1 重数——于是重启前后
// 两条不同的子任务会拿到同一个 id，而这份记录的全部意义就是按 id 找回它。
func TestSubTaskIDsAreUUIDs(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := newTestRuntime(t, withTaskRuns(runs), withModelAnswer("OK"), withOrchestratorRole())
	handle, err := rt.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
	})
	if err != nil {
		t.Fatalf("RunSubTaskAsync: %v", err)
	}
	if _, err := uuid.Parse(handle.TaskID); err != nil {
		t.Fatalf("子任务 id %q 不是 UUID：%v", handle.TaskID, err)
	}
	if strings.Contains(handle.TaskID, ":sub-") {
		t.Errorf("子任务 id 仍然编码着父子关系：%q", handle.TaskID)
	}
}

// TestBackgroundSubTaskIsRecordedBeforeItStarts：插行发生在 goroutine 之前。
//
// 断言「RunSubTaskAsync 返回时记录已经在」——放进 goroutine 里写的话，父进程在这
// 之后立刻退出就什么都没留下，而那正是本设计要覆盖的场景。
func TestBackgroundSubTaskIsRecordedBeforeItStarts(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := newTestRuntime(t, withTaskRuns(runs), withSlowModel(), withOrchestratorRole())
	handle, err := rt.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
	})
	if err != nil {
		t.Fatalf("RunSubTaskAsync: %v", err)
	}
	started, _ := runs.snapshot()
	if len(started) != 1 {
		t.Fatalf("返回时已落盘 %d 条, want 1", len(started))
	}
	got := started[0]
	if got.TaskID != handle.TaskID {
		t.Errorf("落盘的 task id = %q, handle 是 %q", got.TaskID, handle.TaskID)
	}
	if got.ParentTaskID != "task-parent" {
		t.Errorf("parent = %q, want task-parent", got.ParentTaskID)
	}
	if !got.Background {
		t.Error("后台子任务没有被标成 background")
	}
	if got.Goal != "把 A 查清楚" {
		t.Errorf("goal = %q", got.Goal)
	}
	if got.Status != domain.RunStatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
}

// TestBackgroundSubTaskDoesNotStartWhenItCannotBeRecorded：插不进去就不起。
func TestBackgroundSubTaskDoesNotStartWhenItCannotBeRecorded(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{failStart: errors.New("disk full")}
	model := newCountingModel("OK")
	rt := newTestRuntime(t, withTaskRuns(runs), withModel(model), withOrchestratorRole())
	if _, err := rt.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
	}); err == nil {
		t.Fatal("记录插不进去，后台子任务却起来了")
	}
	if n := model.calls(); n != 0 {
		t.Errorf("模型被调用了 %d 次", n)
	}
}
```

> 若该包已有等价的「慢模型」「orchestrator 角色」夹具，复用既有的；`withSlowModel` 只需让一次推理阻塞到用例结束（`t.Cleanup` 里放行），目的是让断言发生在子任务仍在飞的时候。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run 'TestSubTaskIDs|TestBackgroundSubTask' -v`
Expected: FAIL，id 不是 UUID / 返回时还没有落盘记录

- [ ] **Step 3: 实现**

3a. `nextSubTaskID` 换成 UUID，`subTaskSeq` 字段与 `ParentTaskIDForSubTask` 的**生产用途**一并退役（函数本身保留，见 Task 6）：

```go
// nextSubTaskID 为一次委派铸一个新 id。
//
// 用 UUID 而不是「父任务 + 序号」：序号来自进程内计数器，重启之后从头数，于是
// 重启前后两条不同的子任务会拿到同一个 id。父子关系改存 domain.Task.ParentTaskID
// 与 task_runs.parent_task_id，那里它是可查询的，而不是要从字符串里解析出来的。
func (r *Runtime) nextSubTaskID() string { return uuid.NewString() }
```

（`nextSubTaskID` 的 `parentTaskID` 形参随之删除，调用点同步改。）

3b. `runChild` 构造的 `domain.Task` 带上新字段：

```go
	task := domain.Task{
		ID:           subTaskID,
		AgentID:      agentID,
		Input:        composeSubTaskInput(spec),
		CreatedAt:    time.Now(),
		ParentTaskID: spec.ParentTaskID,
		Background:   background,
		Goal:         spec.Goal,
	}
```

`runChild` 增加一个 `background bool` 形参；`RunSubTask` / `RunSubTasks` 传 `false`，`RunSubTaskAsync` 传 `true`。

3c. `RunSubTaskAsync` 在取到边界票之后、`go` 之前同步插行：

```go
	endBackground := r.gate.BeginChild()
	// 记录与边界票在同一处取得，理由相同：这一刻父任务确凿还在飞。放进下面的
	// goroutine 里写，父进程紧接着退出就什么都没留下——而那正是这条记录要覆盖的
	// 场景。插不进去就不起这个子任务，边界票也要还回去。
	if r.taskRuns != nil {
		if err := r.taskRuns.StartTaskRun(ctx, domain.TaskRun{
			ID:           subTaskID + ":run-1",
			TaskID:       subTaskID,
			AgentID:      agent.ID,
			StartedAt:    time.Now(),
			Status:       domain.RunStatusRunning,
			ParentTaskID: spec.ParentTaskID,
			Background:   true,
			Goal:         spec.Goal,
		}); err != nil {
			endBackground()
			return SubTaskHandle{}, fmt.Errorf("record the start of background sub-task %s: %w", subTaskID, err)
		}
	}
	go func() {
		defer endBackground()
		...
```

> 子任务自己的 `RunTask` 随后还会再插一次同 id 的行吗？**不会**：`RunTask` 用 `task.ID + ":run-1"` 作 run id，与这里写的是同一条主键，`StartTaskRun` 的 INSERT 会因主键冲突报错。所以 **`RunSubTaskAsync` 这里不自己插行，改为把 `background=true` 经 `domain.Task` 交给 `RunTask` 去插**——但那就回到「写在 goroutine 里」了。两者取其一：
>
> **采用的做法**：`RunSubTaskAsync` 插的这一行就是那一条记录；`runChild` 调 `child.RunTask` 时，`RunTask` 检测到 `task.Background` 为真则**跳过开始插入**（记录已经在了），终态收口照常。在 `RunTask` 的开始写入处加这个分支，并在注释里写明：后台子任务的开始记录由派发那一刻写下，因为只有那一刻还能保证父任务在飞。

3d. `SubTaskHandle` 的文档改写：

```go
// SubTaskHandle 指向一条后台子任务。TaskID 是它的 UUID，外部可以拿它查这条子任务
// 的运行记录——包括进程重启之后（那时它的状态是 interrupted）。
//
// 完成仍然经运行时事件（Type "subtask_completed"）送达。句柄本身不等待、不轮询。
type SubTaskHandle struct {
	TaskID string
}
```

3e. `go.mod`：`github.com/google/uuid v1.6.0` 从 indirect 区移到直接依赖区（`go mod tidy` 会自动完成）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ -run 'TestSubTaskIDs|TestBackgroundSubTask' -v`
Expected: PASS

Run: `go test ./internal/runtime/ ./internal/cli/`
Expected: ok（`internal/cli` 里依赖旧 id 形态的用例若失败，**不要改断言绕过**——那是 Task 6 要处理的真实断裂，先记录，Task 6 修）

- [ ] **Step 5: 变异实证**

1. `RunSubTaskAsync` 的插行整段挪进 `go func(){}` 里 → `TestBackgroundSubTaskIsRecordedBeforeItStarts` 必须红。
2. `Background: true` 改成 `false` → 同一条用例必须红。
3. `nextSubTaskID` 改回 `fmt.Sprintf("%s:sub-%d", ...)` → `TestSubTaskIDsAreUUIDs` 必须红。
4. Task 4 第 5 条变异（`newSubRuntime` 不搬 `taskRuns`）在这里复跑一次 → 本任务用例必须红。

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/delegation.go internal/runtime/delegation_persistence_test.go internal/runtime/runtime.go go.mod go.sum
git commit -m "feat(runtime): 后台子任务用 UUID 并在派发那一刻落盘"
```

---

## Task 6: 子任务结果回注——父任务从事件字段来

**Files:**
- Modify: `internal/domain/types.go:204-226`（`RuntimeEvent`）
- Modify: `internal/runtime/delegation.go`（发布 `subtask_completed` 处）
- Modify: `internal/cli/subtask_reinject.go:39-62`
- Modify: `internal/runtime/delegation.go:534-545`（`ParentTaskIDForSubTask` 的文档）
- Test: `internal/cli/subtask_reinject_test.go`（追加）

**Interfaces:**
- Consumes: Task 5 的 UUID id。
- Produces: `domain.RuntimeEvent.ParentTaskID`；回注的三条分支。

**为什么必须有这个任务：** id 变成 UUID 之后，`subtask_reinject.go:45` 从 id 字符串解析父任务的做法直接失效，而它失效的方式是**父任务永远等不到回复**，不是一个响亮的错误。

- [ ] **Step 1: 写失败的测试**

追加到 `internal/cli/subtask_reinject_test.go`：

```go
// TestReinjectUsesTheEventsParentTaskID：新事件带着 ParentTaskID，回注按它投递。
func TestReinjectUsesTheEventsParentTaskID(t *testing.T) {
	t.Parallel()

	events := newFakeEventBus(domain.RuntimeEvent{
		Type:         "subtask_completed",
		TaskID:       "8f14e45f-ceea-467a-9f1b-8d0f0f0f0f0f",
		ParentTaskID: "task-parent",
		Message:      "查清楚了",
		CreatedAt:    time.Now(),
	})
	store := newFakeAgentMessageStore()
	if err := newSubtaskReinjectionJob(events, store)(context.Background()); err != nil {
		t.Fatalf("job: %v", err)
	}
	msgs := store.saved()
	if len(msgs) != 1 {
		t.Fatalf("回注了 %d 条, want 1", len(msgs))
	}
	if msgs[0].TaskID != "task-parent" || msgs[0].ThreadID != "task-parent" {
		t.Errorf("投递给了 %q/%q, want task-parent", msgs[0].TaskID, msgs[0].ThreadID)
	}
}

// TestReinjectFallsBackToTheLegacyIDShape：老事件没有这个字段。
//
// runtime_events 是落盘的，真实库里存着改造之前发布的事件。按老形态 id 解析它们是
// 显式的老数据兼容，不是兜底——新事件一律走字段。
func TestReinjectFallsBackToTheLegacyIDShape(t *testing.T) {
	t.Parallel()

	events := newFakeEventBus(domain.RuntimeEvent{
		Type:      "subtask_completed",
		TaskID:    "task-parent:sub-3",
		Message:   "查清楚了",
		CreatedAt: time.Now(),
	})
	store := newFakeAgentMessageStore()
	if err := newSubtaskReinjectionJob(events, store)(context.Background()); err != nil {
		t.Fatalf("job: %v", err)
	}
	msgs := store.saved()
	if len(msgs) != 1 || msgs[0].TaskID != "task-parent" {
		t.Fatalf("老事件没有按老形态解析出父任务：%+v", msgs)
	}
}

// TestReinjectRefusesAnEventItCannotAddress：两条路都不成立时报错。
//
// 带阳性对照：上面两条用例证明正常的事件确实回注得了，否则这条断言可能只是因为
// 整个 job 什么都没做而恒真。
func TestReinjectRefusesAnEventItCannotAddress(t *testing.T) {
	t.Parallel()

	events := newFakeEventBus(domain.RuntimeEvent{
		Type:      "subtask_completed",
		TaskID:    "8f14e45f-ceea-467a-9f1b-8d0f0f0f0f0f", // UUID，且没有 ParentTaskID
		Message:   "查清楚了",
		CreatedAt: time.Now(),
	})
	store := newFakeAgentMessageStore()
	err := newSubtaskReinjectionJob(events, store)(context.Background())
	if err == nil {
		t.Fatal("一条无法定位父任务的事件被静默跳过了；父任务会永远等下去")
	}
	if len(store.saved()) != 0 {
		t.Error("无法定位父任务却还是投递了")
	}
}
```

> `newFakeEventBus` / `newFakeAgentMessageStore` 若该包已有等价夹具，复用既有的。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run TestReinject -v`
Expected: FAIL，`unknown field ParentTaskID in struct literal of type domain.RuntimeEvent`

- [ ] **Step 3: 实现**

3a. `domain.RuntimeEvent` 加字段：

```go
	// ParentTaskID 是派出这条子任务的父任务，只在 subtask_completed 事件上填写。
	// 子任务 id 是 UUID，从中解析不出父任务，而回注要把结果送回父任务那条线程。
	// 改造之前发布并落盘的事件没有这个字段，见回注处的老数据分支。
	ParentTaskID string `json:"parent_task_id,omitempty"`
```

3b. `RunSubTaskAsync` 的 `subtask_completed` 事件填上它：

```go
		event := domain.RuntimeEvent{
			Type:         "subtask_completed",
			TaskID:       subTaskID,
			ParentTaskID: spec.ParentTaskID,
			CreatedAt:    time.Now(),
		}
```

3c. `subtask_reinject.go` 的解析改成三分支：

```go
			parentTaskID, err := parentTaskIDOf(event)
			if err != nil {
				return err
			}
```

并在同文件加：

```go
// parentTaskIDOf 回答「这条 subtask_completed 事件的结果该送回哪个父任务」。
//
// 两条来源，顺序是有意的：事件自己带的字段是今天的答案；老形态 id
// "<父>:sub-<n>" 是改造之前发布、并且已经落在 runtime_events 里的那些事件仅剩的
// 线索。两条都不成立时报错——静默跳过会让父任务永远等一个已经算完的结果。
func parentTaskIDOf(event domain.RuntimeEvent) (string, error) {
	if event.ParentTaskID != "" {
		return event.ParentTaskID, nil
	}
	if parent, ok := agentruntime.ParentTaskIDForSubTask(event.TaskID); ok {
		return parent, nil
	}
	return "", fmt.Errorf("reinject subtask result: event %q carries no parent task id and %q is not a "+
		"legacy sub-task id", event.TaskID, event.TaskID)
}
```

3d. `ParentTaskIDForSubTask` 的文档改写，说明它现在只服务老数据：

```go
// ParentTaskIDForSubTask 从老形态的子任务 id（"<父>:sub-<n>"）里解析出父任务。
//
// 今天铸出来的子任务 id 是 UUID，解析不出任何东西；这个函数留着，是因为
// runtime_events 里存着改造之前发布的事件，它们的父任务只剩这一条线索。新代码
// 一律读 domain.RuntimeEvent.ParentTaskID。
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/cli/ -run TestReinject -v`
Expected: PASS

Run: `go test ./internal/cli/`
Expected: ok（Task 5 遗留的失败用例应在此处归零）

- [ ] **Step 5: 变异实证**

1. `parentTaskIDOf` 的第一分支删掉（只留老解析）→ `TestReinjectUsesTheEventsParentTaskID` 必须红。
2. 第二分支删掉 → `TestReinjectFallsBackToTheLegacyIDShape` 必须红。
3. 最后的 `return "", fmt.Errorf(...)` 改成 `return event.TaskID, nil` → `TestReinjectRefusesAnEventItCannotAddress` 必须红。
4. `RunSubTaskAsync` 里事件的 `ParentTaskID` 赋值删掉 → 端到端上第 1 条用例的等价路径必须红（若无端到端用例覆盖，补一条）。

- [ ] **Step 6: 提交**

```bash
git add internal/domain/types.go internal/runtime/delegation.go internal/cli/subtask_reinject.go internal/cli/subtask_reinject_test.go
git commit -m "feat(cli): 子任务结果回注改读事件上的父任务 id，老事件走老解析"
```

---

## Task 7: serve 启动扫描

**Files:**
- Modify: `internal/cli/command.go:2640` 一带（`BuildServeService` 的装配段）
- Test: `internal/cli/task_run_sweep_test.go`（新建）

**Interfaces:**
- Consumes: Task 3 的 `port.TaskRunStore.SweepRunning`。
- Produces: serve 启动时的一次扫描 + 一条审计 + 一行日志。

- [ ] **Step 1: 写失败的测试**

`internal/cli/task_run_sweep_test.go`：

```go
package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

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

// TestSweepInterruptedRunsRefusesANilStore：serve 装配一定有这个 store，nil 说明
// 接线漏了。
func TestSweepInterruptedRunsRefusesANilStore(t *testing.T) {
	t.Parallel()

	if err := sweepInterruptedRuns(context.Background(), nil, newFakeAuditLog(), testLogger()); err == nil {
		t.Fatal("nil store 被当成了「没什么要扫的」")
	}
}
```

> `newFakeAuditLog` / `testLogger` 复用该包既有夹具。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run TestSweepInterruptedRuns -v`
Expected: FAIL，`undefined: sweepInterruptedRuns`

- [ ] **Step 3: 实现**

`internal/cli/command.go`（放在 `resolvePluginTrustlist` 附近的同层辅助函数区）：

```go
// sweepInterruptedRuns 把上一次进程留下的 running 运行记录摆成 interrupted，并把
// 这一轮的结果记进审计与日志。
//
// 它在 serve 开始接任务之前跑一次，且只跑一次：此刻库里任何一条 running 都不可能
// 属于本进程。扫描失败让 serve 起不来——扫不动意味着接下来每一条 running 记录的
// 含义都是不确定的，而这份记录的全部价值就在于那个含义是确定的。
//
// 零条也记。一个「从来没扫到过东西」的扫描与一个根本没跑起来的扫描，只有日志能
// 把它们分开。
func sweepInterruptedRuns(ctx context.Context, store port.TaskRunStore, audit port.AuditLog, logger *slog.Logger) error {
	if store == nil {
		return errors.New("sweep interrupted task runs: the task run store is nil; serve assembly always " +
			"provides one, so a nil here is a wiring gap rather than a deployment without run records")
	}
	at := time.Now()
	swept, err := store.SweepRunning(ctx, at)
	if err != nil {
		return fmt.Errorf("sweep interrupted task runs: %w", err)
	}
	if err := audit.Append(ctx, domain.AuditEvent{
		ID:          fmt.Sprintf("task-runs-swept:%d", at.UnixNano()),
		RequestID:   "startup",
		SubjectType: "runtime",
		SubjectID:   "task_runs",
		Action:      "task_runs_swept",
		Hash:        strconv.Itoa(swept),
		CreatedAt:   at,
	}); err != nil {
		return fmt.Errorf("record the task run sweep (%d swept): %w", swept, err)
	}
	logger.Info("swept interrupted task runs",
		"component", "cli",
		"swept", swept,
		"consequence", "runs left running by a previous process are now recorded as interrupted; they are "+
			"not restarted")
	return nil
}
```

在 `BuildServeService` 里，拿到持久化仓库之后、启动 HTTP 服务之前调用它，并把同一个仓库经 Runtime 配置的 `TaskRuns` 传下去（与 `Events` / `Audit` 同处）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/cli/ -run TestSweepInterruptedRuns -v`
Expected: PASS

- [ ] **Step 5: 变异实证**

1. `swept == 0` 时跳过审计（加一条 `if swept == 0 { return nil }`）→ `TestSweepInterruptedRunsRecordsEveryRound` 必须红。
2. `SweepRunning` 的错误改成只记日志 → `TestSweepInterruptedRunsFailsLoud` 必须红。
3. nil 判断改成 `return nil` → `TestSweepInterruptedRunsRefusesANilStore` 必须红。
4. **接线变异**：`BuildServeService` 里那次 `sweepInterruptedRuns` 调用删掉 → 必须有用例转红。若没有，说明只测了函数没测接线（这仓反复踩的那个形状），**补一条覆盖装配的用例**。

- [ ] **Step 6: 提交**

```bash
git add internal/cli/command.go internal/cli/task_run_sweep_test.go
git commit -m "feat(cli): serve 启动时把残留的 running 运行记录摆成 interrupted"
```

---

## Task 8: 查询出口改读表

**Files:**
- Modify: `internal/server/http.go:1232-1290`（`handleGetTaskResult` / `taskResult`）、`taskResultResponse` 定义处、`HTTPServer` 结构体与 `Config`
- Test: `internal/server/task_result_test.go`（追加或新建）

**Interfaces:**
- Consumes: Task 3 的 `port.TaskRunStore.ListTaskRuns`。
- Produces: `/v1/tasks/{id}/result` 响应中的 `status` 字段；表优先、事件回落。

- [ ] **Step 1: 写失败的测试**

```go
// TestTaskResultPrefersThePersistedRun：表里有记录就以表为准。
//
// 表是唯一能报出 interrupted 的来源：事件总线里根本没有「中断」这种事件，因为写下
// 它的那个进程已经不在了。
func TestTaskResultPrefersThePersistedRun(t *testing.T) {
	t.Parallel()

	runs := &stubTaskRuns{byTask: map[string][]domain.TaskRun{
		"task-1": {{
			ID: "task-1:run-1", TaskID: "task-1", Status: domain.RunStatusInterrupted,
			Result: "", StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now(),
		}},
	}}
	srv := newTestServer(t, withTaskRunStore(runs), withTask("task-1"))
	resp := srv.getTaskResult(t, "task-1")
	if resp.Status != string(domain.RunStatusInterrupted) {
		t.Errorf("status = %q, want interrupted", resp.Status)
	}
}

// TestTaskResultFallsBackToTheEventLog：表里没有（落盘之前的老任务）就回落事件。
func TestTaskResultFallsBackToTheEventLog(t *testing.T) {
	t.Parallel()

	runs := &stubTaskRuns{}
	srv := newTestServer(t, withTaskRunStore(runs), withTask("task-1"), withCompletedEvent("task-1", "老答案", 42))
	resp := srv.getTaskResult(t, "task-1")
	if resp.Result != "老答案" || resp.TotalTokens != 42 {
		t.Errorf("回落没生效：%+v", resp)
	}
}

// TestTaskResultReportsAStoreFailure：查表失败要报错，不能悄悄回落到事件。
//
// 悄悄回落会把一次数据库故障渲染成「这条任务没有落盘记录」，而那两件事对读的人
// 完全不同。
func TestTaskResultReportsAStoreFailure(t *testing.T) {
	t.Parallel()

	runs := &stubTaskRuns{err: errors.New("db is locked")}
	srv := newTestServer(t, withTaskRunStore(runs), withTask("task-1"), withCompletedEvent("task-1", "老答案", 42))
	code, body := srv.getTaskResultRaw(t, "task-1")
	if code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, body = %s", code, body)
	}
}
```

> `stubTaskRuns` / `newTestServer` / `withTask` / `withCompletedEvent` / `getTaskResult` 按该包既有的 HTTP 测试夹具写法来，复用既有的。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/server/ -run TestTaskResult -v`
Expected: FAIL，`withTaskRunStore undefined`

- [ ] **Step 3: 实现**

3a. `HTTPServer` 加 `taskRuns port.TaskRunStore` 字段，`Config` 同名字段，装配处（`internal/cli/command.go`）传入。

3b. `taskResultResponse` 加 `Status string \`json:"status"\``（**注意**：该响应已有一个 `Status` 承载 `task.Status`；本任务新增的是运行状态，命名为 `RunStatus string \`json:"run_status"\`` 以免与既有字段语义打架——**沿用这个名字，不要覆盖既有 `status`**）。

3c. `handleGetTaskResult` 先查表：

```go
	// 落盘的运行记录优先：它是唯一能报出 interrupted 的来源——事件总线里没有
	// 「中断」这种事件，因为写下它的那个进程已经不在了。查不到再回落到事件，那是
	// 落盘之前跑完的老任务仅剩的出处。
	var runStatus domain.RunStatus
	if s.taskRuns != nil {
		runs, err := s.taskRuns.ListTaskRuns(r.Context(), taskID)
		if err != nil {
			observability.WithRequestID(s.logger, requestIDFromContext(r.Context())).
				Error("read task runs failed", "task_id", taskID, "error", err)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("task runs: %v", err))
			return
		}
		if len(runs) > 0 {
			last := runs[len(runs)-1]
			runStatus = last.Status
			// 结果与 usage 也以表为准；表里那条是这次运行自己写下的。
			result = last.Result
			usage = taskUsage{
				PromptTokens:     last.PromptTokens,
				CompletionTokens: last.CompletionTokens,
				CachedTokens:     last.CachedTokens,
				TotalTokens:      last.TotalTokens,
				ElapsedMs:        last.EndedAt.Sub(last.StartedAt).Milliseconds(),
			}
			generatedFiles = last.GeneratedFiles
		}
	}
```

（把现有的 `result, usage, generatedFiles, err := s.taskResult(taskID)` 调整成先声明、再按上面的顺序覆盖；事件回落保持原样，只在表里没有记录时使用。）

3d. `taskResult` 的注释里删掉 "because TaskRun is not persisted"，改成说明它现在是老数据的回落路径。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/server/ -run TestTaskResult -v`
Expected: PASS

Run: `go build ./... && go vet ./... && go test ./...`
Expected: 全绿

- [ ] **Step 5: 变异实证**

1. 表优先改成事件优先（先取事件，表里有也不覆盖）→ `TestTaskResultPrefersThePersistedRun` 必须红。
2. `ListTaskRuns` 的错误改成忽略后回落事件 → `TestTaskResultReportsAStoreFailure` 必须红。
3. `len(runs) > 0` 改成 `len(runs) >= 0` → `TestTaskResultFallsBackToTheEventLog` 必须红。

- [ ] **Step 6: 提交**

```bash
git add internal/server/http.go internal/server/task_result_test.go internal/cli/command.go
git commit -m "feat(server): 任务结果以落盘的运行记录为准，老任务回落事件日志"
```

---

## 收尾

- [ ] **整分支复审**：拿规格逐节点名，问「这一条有没有任何代码实现」。重点查规格第五节「数清所有出口」与第九节的九条测试是否都有对应用例——**「规格明写、实现为零」只有整分支最终复审抓得到，因为那种缺口不会让任何测试变红**。
- [ ] **更新接续文档** `docs/superpowers/plans/2026-09-09-open-items-handoff.md`：§〇 B（`TaskRun` 不落盘）与 §一（Spec C 未开工）已消除；补记本次的遗留（若有）。
- [ ] `go build ./... && go vet ./... && go test ./...` 全绿，`gofmt -l .` 为空。
