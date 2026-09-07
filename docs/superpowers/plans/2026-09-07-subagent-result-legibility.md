# 委派结果的可判读性 实施计划（Spec A）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让「任务为什么停下来」成为一等信息（记录、落盘、出到委派结果），并让非法的委派请求在启动任何子代理之前硬拒。

**Architecture:** 新增 `domain.StopReason` 四值枚举，取代 `runToolLoop` 里那个只区分两种情况的 `loopCut bool`；`finishRun` 是成功 `TaskRun` 的唯一出口，在那里填写并断言非空（零值即编程错误）；落 `task_runs` 一列；出到 `SubTaskResult` 与 `delegate_task` 输出。委派侧抽一个共用纯校验函数，三个入口共用，批量整批预检。

**Tech Stack:** Go；SQLite（`internal/storage/sqlite.go` 的 `columnMigrations` 幂等 ALTER）；标准 `testing`。

规格：`docs/superpowers/specs/2026-09-07-subagent-result-legibility-design.md`

## Global Constraints

- **fail-loud 铁律**（本仓 CLAUDE.md 最高优先级）：不许回落零值、不许吞错误、不许静默跳过、不许「拿不到就当没配置」。错误用 `fmt.Errorf("...: %w", err)` 包装，**错误点必须可定位**。
- **注释是契约**：注释里每条事实陈述必须与代码一致，且**不得出现关于「谁调用它 / 有没有调用方 / 某个东西落没落地」的句子**。凡是提到别的文件 / 常量 / 包行为的句子，**亲自去那个文件确认再写**。上一期在这上面被抓到七条以上 finding。
- **不得引入包级可变量。**
- **不改变任何一条既有控制流分支**：哪条 `break`、什么时候 `break`，一律不动。唯一允许的行为变更是 Task 4 的收尾话术。
- **零值绝不许被当作 `completed`。**
- 四个常量的确切值：`completed` / `max_rounds` / `tool_loop_cap` / `repeat_loop_broken`。
- 本次**不碰** `agent_id` / `agentregistry` / `toolauth`（Spec B），**不做**后台子任务持久化（Spec C），**不建** provider/descriptor 抽象，**不动** `Registry.Without` 的语义。
- 全绿判据：`go test ./... -count=1 -p 1 -timeout 900s`；`gofmt -l .` 为空；`go vet ./...` 干净。**测退出码不要接管道**（管道会让 `$?` 取到 `tail` 的状态）。
- 只按**显式路径** `git add`，**不用 `git add -A`**。提交信息正文中文，结尾一行 `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`。

## 本期最该防的那个形状

本仓「**接缝在，但没人测那条接缝**」上一期复发**八次**，**每一次都靠变异实证抓出来，静态审查一次都没抓住**。

**每个任务都必须做变异验证**：把它要守的规则在实现里失效掉（**`go vet` 必须干净，不能只造成编译失败**），确认对应用例 FAIL，再改回来确认 PASS。**哪条变异之后测试仍然全绿，就说明那条规则没人守着，必须补测试而不是放过。**

本期尤其要盯这三条接线：
1. `StopReason` 真的**落进了库、又读得回来**（不是只在内存里传）
2. 共用校验函数**三个入口真的都调了**（不是只有一个入口调、另两个各写一遍）
3. 批量预检真的**一个子代理都没起**（不是只返回了 error）

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/domain/types.go` | `StopReason` 类型与四常量；`TaskRun.StopReason` 字段 |
| `internal/runtime/runtime.go` | `loopState.stopReason` 字段；四个赋值点；`finishRun` 填写与断言；收尾话术分派 |
| `internal/storage/sqlite.go` | `columnMigrations` 加一条；`SaveTaskRun` / `ListTaskRuns` 补列 |
| `internal/runtime/delegation.go` | `SubTaskResult.StopReason`；共用校验函数 `validateSubTaskSpec`；`RunSubTasks` 整批预检 |
| `internal/runtime/delegation_tool.go` | `background` 严格解析；输出带 `stop_reason` |

## 既有代码的关键事实（实施者必读）

- `runToolLoop`（`internal/runtime/runtime.go:897`）签名：`func (r *Runtime) runToolLoop(ctx context.Context, requestID string, agent domain.Agent, task domain.Task, st loopState) (domain.TaskRun, error)`。`st` **按值传递**。
- 主循环在 `:927`：`for st.round < r.maxToolRounds && len(st.resp.ToolCalls) > 0 {`
- `loopCut := false` 在 `:906`；两处置真：`:1064`（单工具熔断，`capHit != ""`）与 `:1088`（重复调用熔断）。两处都 `break`。
- 循环之后是 `if len(st.resp.ToolCalls) > 0 {`（`:1121`）。**轮数耗尽**与**两种熔断 break** 都进这个块；块内注释（`:1128-1139`）说明两者的区别在于有没有未关闭的 step。
- 收尾话术在 `:1152-1155`，今天只有两句，`loopCut` 二选一。
- `finishRun`（`:1240`）是成功 `TaskRun` 的**唯一**出口，`:1318` `return run, nil`；唯一调用点 `:1184`。挂起走 `ErrSuspended`；其余 31 处 `return domain.TaskRun{}` 全是零值 + 错误。
- `loopState`（`:254`）已经承载 `round`、`promptTokens` 等「累积事实」，`finishRun` 读 `st.*Tokens`。新字段跟随这个既有模式。
- `domain.TaskRun`（`internal/domain/types.go:85`）。
- `task_runs` 建表在 `internal/storage/sqlite.go:2168`，六列；`SaveTaskRun` 在 `:1131`；`ListTaskRuns` 在 `:1148`。
- `columnMigrations` 在 `internal/storage/sqlite.go:1903`，条目结构 `{table, column, stmt}`。
- `Registry.HasTool(name string) bool` 在 `internal/tool/registry.go:224`。`Registry.Subset`（`:240`）对未知名字**静默丢弃**。
- 测试夹具（`internal/runtime/multiturn_test.go`）：`loopingMaas`（永远回同一个 call）、`recordingRoundsMaas{responses: []port.InferenceResponse}`、`unchangingReadRegistry(t)`。构造：`NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: N})`。`MaxToolRounds: 0` 表示不限。

---

### Task 1: 四个终止原因在主循环被正确判定

**Files:**
- Modify: `internal/domain/types.go:85-100`
- Modify: `internal/runtime/runtime.go:254`（`loopState`）、`:906`、`:1064`、`:1088`、`:1121`
- Test: `internal/runtime/stopreason_test.go`（新建）

**Interfaces:**
- Consumes: 无
- Produces: `domain.StopReason`（底层 `string`）；常量 `domain.StopReasonCompleted` / `StopReasonMaxRounds` / `StopReasonToolLoopCap` / `StopReasonRepeatLoopBroken`；字段 `domain.TaskRun.StopReason StopReason`；字段 `loopState.stopReason domain.StopReason`

- [ ] **Step 1: 写失败的测试**

新建 `internal/runtime/stopreason_test.go`：

```go
package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// 模型第一轮就给最终答案，没有任何工具调用：正常结束。
func TestStopReasonCompletedWhenTheModelAnswersWithoutTools(t *testing.T) {
	t.Parallel()
	maas := &recordingRoundsMaas{responses: []port.InferenceResponse{{Text: "done"}}}
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: 4})

	run, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "answer"})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if run.StopReason != domain.StopReasonCompleted {
		t.Errorf("run.StopReason = %q, want %q: the model answered with no pending tool calls",
			run.StopReason, domain.StopReasonCompleted)
	}
}

// 每轮都要求一个不同的工具调用，轮数先耗尽：max_rounds。
// 这条路径今天在代码里没有任何痕迹，是纯新增。
func TestStopReasonMaxRoundsWhenTheRoundBudgetRunsOut(t *testing.T) {
	t.Parallel()
	const rounds = 3
	responses := make([]port.InferenceResponse, 0, rounds+2)
	for i := range rounds + 2 {
		args, err := json.Marshal(i)
		if err != nil {
			t.Fatalf("Marshal(round index) error = %v, want nil", err)
		}
		responses = append(responses, port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "c" + string(args),
			Name:      "read_file",
			Arguments: map[string]string{"path": "file-" + string(args) + ".txt"},
		}}})
	}
	maas := &recordingRoundsMaas{responses: responses}
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: rounds})

	run, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "keep reading"})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if run.StopReason != domain.StopReasonMaxRounds {
		t.Errorf("run.StopReason = %q, want %q: the loop exited because round %d reached the limit",
			run.StopReason, domain.StopReasonMaxRounds, rounds)
	}
}

// 模型死抠同一个调用：重复守卫先于轮数耗尽切断循环。
func TestStopReasonRepeatLoopBrokenWhenTheModelRepeatsOneCall(t *testing.T) {
	t.Parallel()
	maas := &loopingMaas{call: domain.ToolCall{Name: "read_file", Arguments: map[string]string{"path": "hello.txt"}}}
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: 0})

	run, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read it"})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if run.StopReason != domain.StopReasonRepeatLoopBroken {
		t.Errorf("run.StopReason = %q, want %q: the repeat guard cut the loop",
			run.StopReason, domain.StopReasonRepeatLoopBroken)
	}
}

// 同一个工具名、每轮不同参数：名字级熔断（toolLoopCap）先于重复守卫命中。
func TestStopReasonToolLoopCapWhenOneToolNameExhaustsItsAllowance(t *testing.T) {
	t.Parallel()
	responses := make([]port.InferenceResponse, 0, toolLoopCap+2)
	for i := range toolLoopCap + 2 {
		args, err := json.Marshal(i)
		if err != nil {
			t.Fatalf("Marshal(round index) error = %v, want nil", err)
		}
		responses = append(responses, port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "c" + string(args),
			Name:      "read_file",
			Arguments: map[string]string{"path": "file-" + string(args) + ".txt"},
		}}})
	}
	maas := &recordingRoundsMaas{responses: responses}
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: 0})

	run, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read many files"})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if run.StopReason != domain.StopReasonToolLoopCap {
		t.Errorf("run.StopReason = %q, want %q: read_file exhausted its per-name allowance (%d)",
			run.StopReason, domain.StopReasonToolLoopCap, toolLoopCap)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run TestStopReason -count=1`
Expected: 编译失败，`undefined: domain.StopReasonCompleted`（`StopReason` 尚不存在）。

- [ ] **Step 3: 加类型与字段**

在 `internal/domain/types.go` 的 `TaskRun` 定义**之前**加：

```go
// StopReason says why a task's tool loop stopped. It is recorded at the one
// place a successful TaskRun is assembled, so every finished run carries one.
//
// The empty string is NOT a reason: it means nobody set one, which is a
// programming error rather than a kind of ending. It must never be read as
// StopReasonCompleted — a run that stopped for an unrecorded reason is
// indistinguishable from one that finished, and that is exactly the confusion
// this type exists to remove.
type StopReason string

const (
	// StopReasonCompleted is the model answering with no pending tool calls.
	StopReasonCompleted StopReason = "completed"
	// StopReasonMaxRounds is the round budget running out with calls still pending.
	StopReasonMaxRounds StopReason = "max_rounds"
	// StopReasonToolLoopCap is one tool NAME exhausting its per-task allowance.
	StopReasonToolLoopCap StopReason = "tool_loop_cap"
	// StopReasonRepeatLoopBroken is the model repeating one identical call until
	// the repeat guard cut the loop.
	StopReasonRepeatLoopBroken StopReason = "repeat_loop_broken"
)
```

在 `TaskRun` 结构体里，`Result` 之后加：

```go
	// StopReason is why this run's tool loop stopped. Always set on a run that
	// was assembled successfully; see StopReason for why empty is not a value.
	StopReason StopReason `json:"stop_reason,omitempty"`
```

- [ ] **Step 4: 在 `loopState` 上加字段，删掉 `loopCut`**

在 `internal/runtime/runtime.go` 的 `loopState`（`:254`）里，`round` 字段之后加：

```go
	// stopReason is why the tool loop ended. It is set at each of the loop's
	// terminal points and read by finishRun, alongside the token counters that
	// accumulate the same way. It replaced a loopCut bool that could only tell
	// "the model repeated itself" from "everything else".
	stopReason domain.StopReason
```

删掉 `:906` 的 `loopCut := false` 及其上方那段说明它的注释（`:903-905`）。

- [ ] **Step 5: 在四个终点写入原因**

`:1064` 单工具熔断分支，把 `loopCut = true` 换成：

```go
			st.stopReason = domain.StopReasonToolLoopCap
```

`:1088` 重复调用熔断分支，把 `loopCut = true` 换成：

```go
			st.stopReason = domain.StopReasonRepeatLoopBroken
```

`:1121` 的 `if len(st.resp.ToolCalls) > 0 {` 改成：

```go
	if len(st.resp.ToolCalls) > 0 {
		// Both breaks above and a plain round-budget exhaustion arrive here.
		// The breaks already named their reason; an unnamed one is the budget.
		if st.stopReason == "" {
			st.stopReason = domain.StopReasonMaxRounds
		}
```

并在该 `if` 块的**结尾之后**加一个 `else` 分支（块内既有代码一行不动）：

```go
	} else {
		// The loop ended with nothing pending: the model gave its answer.
		st.stopReason = domain.StopReasonCompleted
	}
```

- [ ] **Step 6: 让 `finishRun` 把它带进 `TaskRun`**

在 `finishRun`（`:1240`）组装 `run` 的地方，给 `domain.TaskRun{...}` 字面量加：

```go
		StopReason: st.stopReason,
```

- [ ] **Step 7: 跑测试确认通过**

Run: `go test ./internal/runtime/ -run TestStopReason -count=1`
Expected: PASS，四个用例全过。

- [ ] **Step 8: 变异验证**

依次做这四条，每条：改坏 → 跑 `go vet ./internal/runtime/`（必须干净）→ 跑 `go test ./internal/runtime/ -count=1`（必须有用例 FAIL）→ 还原 → 确认 PASS。

1. `:1121` 的 `if st.stopReason == "" { st.stopReason = domain.StopReasonMaxRounds }` 整段删掉 → `TestStopReasonMaxRounds...` 必红
2. `else` 分支里换成 `st.stopReason = domain.StopReasonMaxRounds` → `TestStopReasonCompleted...` 必红
3. 单工具熔断分支改成写 `domain.StopReasonRepeatLoopBroken` → `TestStopReasonToolLoopCap...` 必红
4. 重复熔断分支改成写 `domain.StopReasonToolLoopCap` → `TestStopReasonRepeatLoopBroken...` 必红

**任何一条之后测试仍然全绿，就补用例，不要放过。**

- [ ] **Step 9: 提交**

```bash
git add internal/domain/types.go internal/runtime/runtime.go internal/runtime/stopreason_test.go
git commit -m "feat(runtime): 主循环记录终止原因，取代只分两种情况的 loopCut"
```

---

### Task 2: `finishRun` 断言终止原因非空

**Files:**
- Modify: `internal/runtime/runtime.go:1240`（`finishRun`）
- Test: `internal/runtime/stopreason_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `loopState.stopReason`、`domain.StopReason`
- Produces: 无新符号

- [ ] **Step 1: 写失败的测试**

追加到 `internal/runtime/stopreason_test.go`：

```go
// 零值不是一种结局，是「没人填」。成功的 TaskRun 只从 finishRun 出来，所以
// 断言放在那里；它炸掉的是「加了字段但某条路径忘了填」这个形状。
func TestFinishRunPanicsWhenNoStopReasonWasRecorded(t *testing.T) {
	t.Parallel()
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: &recordingRoundsMaas{
		responses: []port.InferenceResponse{{Text: "done"}},
	}, Tools: unchangingReadRegistry(t)})

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("finishRun() did not panic on an unrecorded stop reason; an empty reason must never pass for completed")
		}
		message, ok := recovered.(string)
		if !ok || !strings.Contains(message, "stop reason") {
			t.Fatalf("panic value = %v, want a message naming the missing stop reason", recovered)
		}
	}()

	//nolint:errcheck // the call panics before it can return
	_, _ = rt.finishRun(context.Background(), "req-1", domain.Agent{ID: "a"},
		domain.Task{ID: "t1"}, loopState{started: time.Now()})
}
```

同时把 `"strings"` 与 `"time"` 加进该文件的 import 块。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run TestFinishRunPanics -count=1`
Expected: FAIL，`finishRun() did not panic on an unrecorded stop reason`。

- [ ] **Step 3: 加断言**

在 `finishRun`（`:1240`）函数体**最前面**加：

```go
	// A successful TaskRun is assembled only here, so this is the one place the
	// invariant can be checked. An empty reason is not a kind of ending; it is a
	// terminal path that forgot to name itself, and letting it through would
	// report that run as a clean completion.
	if st.stopReason == "" {
		panic("runtime: finishRun reached with no stop reason recorded for task " + task.ID)
	}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ -run "TestFinishRunPanics|TestStopReason" -count=1`
Expected: PASS，五个用例全过。

- [ ] **Step 5: 变异验证**

把断言换成 `if st.stopReason == "" { st.stopReason = domain.StopReasonCompleted }`（即「零值当作 completed」这个正被禁止的形状）→ `go vet` 干净 → `TestFinishRunPanicsWhenNoStopReasonWasRecorded` 必红 → 还原 → 绿。

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/runtime.go internal/runtime/stopreason_test.go
git commit -m "feat(runtime): finishRun 断言终止原因非空，零值绝不当作 completed"
```

---

### Task 3: 终止原因落盘

**Files:**
- Modify: `internal/storage/sqlite.go:1903`（`columnMigrations`）、`:1131`（`SaveTaskRun`）、`:1148`（`ListTaskRuns`）
- Test: `internal/storage/taskrun_stopreason_test.go`（新建）

**Interfaces:**
- Consumes: `domain.TaskRun.StopReason`（Task 1）
- Produces: `task_runs.stop_reason` 列

- [ ] **Step 1: 写失败的测试**

新建 `internal/storage/taskrun_stopreason_test.go`。仓库用既有的 `openTestSQLiteRepository(t)`（`internal/storage/sqlite_test.go:21` 在用），迁移函数是 `(*SQLiteRepository).applyColumnMigrations(ctx)`（`sqlite.go:1943`）：

```go
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
```

`repo.db` 是 `SQLiteRepository` 的未导出字段，同包测试可直接用。**不要新建 helper。**

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/storage/ -run "TaskRunStopReason|StopReasonColumn" -count=1`
Expected: FAIL，往返用例读回来是空串（列还不存在，`SaveTaskRun` 没写它）。

- [ ] **Step 3: 加迁移**

在 `internal/storage/sqlite.go` 的 `columnMigrations`（`:1903`）切片**末尾**追加：

```go
	{
		table:  "task_runs",
		column: "stop_reason",
		stmt:   `ALTER TABLE task_runs ADD COLUMN stop_reason TEXT NOT NULL DEFAULT ''`,
	},
```

- [ ] **Step 4: 写侧补列**

`SaveTaskRun`（`:1131`）改成：

```go
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_runs (id, task_id, agent_id, started_at, ended_at, result, stop_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			task_id = excluded.task_id,
			agent_id = excluded.agent_id,
			started_at = excluded.started_at,
			ended_at = excluded.ended_at,
			result = excluded.result,
			stop_reason = excluded.stop_reason
	`, run.ID, run.TaskID, run.AgentID, formatTime(run.StartedAt), formatTime(run.EndedAt), run.Result, string(run.StopReason))
```

- [ ] **Step 5: 读侧补列**

`ListTaskRuns`（`:1148`）的 SELECT 改成：

```go
		SELECT id, task_id, agent_id, started_at, ended_at, result, stop_reason
		FROM task_runs
		WHERE task_id = ?
		ORDER BY started_at, id
```

并在该函数的 `rows.Scan(...)` 里，按同样的顺序**在末尾**多接一个目标。扫进一个 `string` 局部变量再转成 `domain.StopReason` 赋给该行的 `StopReason` 字段——**照抄该函数既有的逐行组装写法**（它已经在把时间字符串转回 `time.Time`，跟着那个模式走）。

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/storage/ -count=1`
Expected: PASS，`internal/storage` 全绿。

- [ ] **Step 7: 变异验证**

1. `SaveTaskRun` 的 INSERT 去掉 `stop_reason` 列（连同 VALUES 的 `?` 与参数）→ `go vet` 干净 → 往返用例必红
2. `ListTaskRuns` 的 SELECT 保留 `stop_reason` 但 Scan 出来**不赋给** `StopReason` 字段（扫进一个丢弃变量）→ `go vet` 干净 → 往返用例必红。**这条专抓「写得出但读不回」**
3. 迁移条目整条删掉 → 往返用例必红

- [ ] **Step 8: 提交**

```bash
git add internal/storage/sqlite.go internal/storage/taskrun_stopreason_test.go
git commit -m "feat(storage): task_runs 落盘终止原因，老行读回空值"
```

---

### Task 4: 收尾话术按终止原因分派

**Files:**
- Modify: `internal/runtime/runtime.go:1152-1155`
- Test: `internal/runtime/stopreason_closing_test.go`（新建）

**Interfaces:**
- Consumes: `loopState.stopReason`（Task 1）
- Produces: 无新符号

**背景**：今天 `:1064`（单工具熔断）与 `:1088`（重复调用熔断）**都**置 `loopCut = true`，于是 `:1153` 给两者同一句「检测到你在重复同一个工具调用」——**撞了单工具上限的任务，被告知的却是「你在重复调用」**。这是一句假话，本仓把陈述准确性当契约。本任务改正它。

这是**模型可见的 prompt 变更**，所以它有自己的用例，**且不与 Task 1 的原因判定共用用例**——否则一个变异同时打两处规则，分不清是哪条在守。

- [ ] **Step 1: 写失败的测试**

新建 `internal/runtime/stopreason_closing_test.go`。用 Task 1 里那两个场景的构造方式（重复调用用 `loopingMaas`，单工具熔断用每轮不同参数的 `recordingRoundsMaas`），断言模型最后收到的那条系统提示：

```go
package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// 撞单工具上限的任务，收尾话术必须说的是上限，不是「你在重复调用」。
func TestClosingInstructionForAToolNameCapNamesTheCap(t *testing.T) {
	t.Parallel()
	responses := make([]port.InferenceResponse, 0, toolLoopCap+2)
	for i := range toolLoopCap + 2 {
		args, err := json.Marshal(i)
		if err != nil {
			t.Fatalf("Marshal(round index) error = %v, want nil", err)
		}
		responses = append(responses, port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "c" + string(args),
			Name:      "read_file",
			Arguments: map[string]string{"path": "file-" + string(args) + ".txt"},
		}}})
	}
	maas := &recordingRoundsMaas{responses: responses}
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: 0})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read many"}); err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if maas.sawText("重复同一个工具调用") {
		t.Error("a per-tool-name cap was explained to the model as repeating one call; that is not what happened")
	}
	if !maas.sawText("调用次数已达上限") {
		t.Error("the model was never told which limit it hit")
	}
}

// 死抠同一个调用的任务，收尾话术说的是重复。
func TestClosingInstructionForARepeatCutNamesTheRepetition(t *testing.T) {
	t.Parallel()
	maas := &loopingMaas{call: domain.ToolCall{Name: "read_file", Arguments: map[string]string{"path": "hello.txt"}}}
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: 0})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read it"}); err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if !maas.sawText("重复同一个工具调用") {
		t.Error("a repeat-guard cut was not explained to the model as repetition")
	}
}
```

**注意**：`recordingRoundsMaas` 是否有 `sawText` 方法，先读 `internal/runtime/multiturn_test.go` 确认——`loopingMaas` 有。若 `recordingRoundsMaas` 没有，给它加一个与 `loopingMaas` **完全同形**的实现（记录所有请求消息、按子串查找），不要另造语义。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run TestClosingInstruction -count=1`
Expected: `TestClosingInstructionForAToolNameCapNamesTheCap` FAIL——今天它拿到的正是「重复同一个工具调用」那句。

- [ ] **Step 3: 按原因分派**

把 `:1152-1155` 那段（`closing := ...` 加 `if loopCut { ... }`）整体换成：

```go
		// Each terminal reason gets its own closing instruction: telling a task
		// that exhausted one tool's allowance that it was "repeating the same
		// call" describes something that did not happen.
		var closing string
		switch st.stopReason {
		case domain.StopReasonRepeatLoopBroken:
			closing = "[系统] 检测到你在重复同一个工具调用，已停止工具循环。请勿再调用、规划或描述任何工具调用，直接基于以上已获取的信息，用自然语言给出对用户问题的最终回答。"
		case domain.StopReasonToolLoopCap:
			closing = "[系统] 单个工具的调用次数已达上限，已停止工具循环。请勿再调用、规划或描述任何工具调用，直接基于以上已获取的信息，用自然语言给出对用户问题的最终回答。"
		case domain.StopReasonMaxRounds:
			closing = "[系统] 工具调用轮数已达上限。请勿再调用、规划或描述任何工具调用，直接基于以上已获取的信息，用自然语言给出对用户问题的最终回答。"
		default:
			// StopReasonCompleted never reaches here (this block runs only with
			// calls still pending), and an unknown reason is a new terminal path
			// that forgot to teach this switch about itself.
			panic("runtime: no closing instruction for stop reason " + string(st.stopReason))
		}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ -count=1`
Expected: PASS，`internal/runtime` 全绿。**若有既有用例断言旧话术而变红，那是它在钉一句假话——把该用例的断言改成新话术，并在提交信息里说明改了哪一条、为什么。**

- [ ] **Step 5: 变异验证**

1. `StopReasonToolLoopCap` 分支的话术换成重复那句 → `TestClosingInstructionForAToolNameCapNamesTheCap` 必红
2. `default` 的 `panic` 换成 `closing = ""` → 新增一个用例：给 `st.stopReason` 塞一个未知值走到这里，必须炸；若无法从外部构造，就直接单测这个分派函数（此时把 switch 抽成一个接收 `domain.StopReason` 返回 `string` 的纯函数，反而更好测）

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/runtime.go internal/runtime/stopreason_closing_test.go
git commit -m "fix(runtime): 收尾话术按终止原因分派，单工具熔断不再谎称是重复调用"
```

---

### Task 5: 终止原因出到委派结果

**Files:**
- Modify: `internal/runtime/delegation.go:38-46`（`SubTaskResult`）、`runChild`
- Modify: `internal/runtime/delegation_tool.go`（`delegateResultsView`、`handleDelegateTask` 的 single 分支）
- Test: `internal/runtime/delegation_stopreason_test.go`（新建）

**Interfaces:**
- Consumes: `domain.TaskRun.StopReason`（Task 1）
- Produces: `runtime.SubTaskResult.StopReason domain.StopReason`；`delegate_task` 输出的 `stop_reason` 键（single 与 batch 每条）

- [ ] **Step 1: 写失败的测试**

新建 `internal/runtime/delegation_stopreason_test.go`。**先读 `internal/runtime/delegation_test.go` 与 `delegation_tool_test.go`**，照抄它们构造可委派 Runtime 的方式（`role`、`depth`、`maxSpawnDepth` 怎么设），不要自造：

```go
package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// 子代理撞了熔断，父必须看得出来 —— 裸 Summary 分不清「答完了」和「被截断了」。
func TestSubTaskResultCarriesTheChildStopReason(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "子任务摘要：完成"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas})
	res, err := parent.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "read it",
	})
	if err != nil {
		t.Fatalf("RunSubTask() error = %v, want nil", err)
	}
	if res.StopReason == "" {
		t.Fatal("SubTaskResult.StopReason is empty: the parent cannot tell a finished child from a cut one")
	}
}

// 工具输出里也要有，single 与 batch 都要。
func TestDelegateTaskOutputCarriesTheStopReason(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "子任务摘要：完成"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas})
	out, err := parent.handleDelegateTask(context.Background(), domain.ToolCall{
		ID:        "call-1",
		Name:      "delegate_task",
		Arguments: map[string]string{"goal": "read it"},
	})
	if err != nil {
		t.Fatalf("handleDelegateTask() error = %v, want nil", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out.Output), &payload); err != nil {
		t.Fatalf("Unmarshal(tool output) error = %v, want nil", err)
	}
	if payload["stop_reason"] == nil || payload["stop_reason"] == "" {
		t.Errorf("tool output = %v, want a non-empty stop_reason", payload)
	}
}
```

构造照抄 `internal/runtime/delegation_test.go:42-43`：根 Runtime 默认就是 orchestrator，`recordingSubMaas`（同文件 `:17`）返回固定摘要并记录每个 prompt。import 需要 `taskgate`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run "SubTaskResultCarries|DelegateTaskOutputCarries" -count=1`
Expected: 编译失败（`res.StopReason` 未定义）。

- [ ] **Step 3: 给 `SubTaskResult` 加字段**

`internal/runtime/delegation.go` 的 `SubTaskResult`：

```go
type SubTaskResult struct {
	TaskID  string
	Summary string
	Err     string
	// StopReason is why the child's tool loop ended. Without it a Summary is
	// just text: a child that answered and a child that was cut off mid-work
	// read the same.
	StopReason domain.StopReason
}
```

- [ ] **Step 4: 在 `runChild` 里填它**

`runChild` 结尾的 `return SubTaskResult{TaskID: subTaskID, Summary: run.Result}, nil` 改成：

```go
	return SubTaskResult{TaskID: subTaskID, Summary: run.Result, StopReason: run.StopReason}, nil
```

- [ ] **Step 5: 出到工具输出**

`internal/runtime/delegation_tool.go` 的 `delegateResultsView`，给每条加：

```go
			"stop_reason": string(res.StopReason),
```

`handleDelegateTask` 的 single 分支，把返回改成：

```go
	return delegateJSON(call.ID, map[string]any{
		"mode":        "single",
		"task_id":     res.TaskID,
		"summary":     res.Summary,
		"stop_reason": string(res.StopReason),
	})
```

background 分支**不动**：它返回的是 handle，此刻还没有结果。

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/runtime/ -count=1`
Expected: PASS。

- [ ] **Step 7: 变异验证**

1. `runChild` 不填 `StopReason` → `TestSubTaskResultCarriesTheChildStopReason` 必红
2. `delegateResultsView` 去掉 `stop_reason` 键 → 需要一条 batch 用例守着；若 Step 1 的两条都不覆盖 batch，**补一条 batch 用例**（这正是「哪条变异后仍全绿就补测试」的适用场景）

- [ ] **Step 8: 提交**

```bash
git add internal/runtime/delegation.go internal/runtime/delegation_tool.go internal/runtime/delegation_stopreason_test.go
git commit -m "feat(runtime): 委派结果带上子代理的终止原因"
```

---

### Task 6: 委派请求的共用前置校验

**Files:**
- Modify: `internal/runtime/delegation.go`（新增 `validateSubTaskSpec`；`RunSubTask` / `RunSubTaskAsync` 调用它）
- Modify: `internal/runtime/delegation_tool.go`（`parseDelegateBool` 改为严格解析）
- Test: `internal/runtime/delegation_validation_test.go`（新建）

**Interfaces:**
- Consumes: `SubTaskSpec`（既有）
- Produces: `func (r *Runtime) validateSubTaskSpec(spec SubTaskSpec) error`；`func parseDelegateBool(value string) (bool, error)`（签名从返回单个 `bool` 改为带 error）

**背景**：两处静默降级——`Registry.Subset`（`internal/tool/registry.go:240`）对未知工具名**静默丢弃**（收窄路径上意味着子代理拿到的工具比调用方以为的少，极端情况一个都没有）；`parseDelegateBool` 把任何非 `1/true/yes/y` 的值当 false（拼成 `"ture"` 就悄悄跑成前台）。

`role` 非法、深度超限、`tasks` JSON 解析失败**今天已经 fail-loud**，本任务把前两者**收进共用函数**，语义不变。

- [ ] **Step 1: 写失败的测试**

新建 `internal/runtime/delegation_validation_test.go`：

```go
package runtime

import (
	"context"
	"strings"
	"testing"
)

// 收窄用的工具名拼错了，今天被 Subset 静默丢掉，子代理拿到一个能力莫名其妙的
// 工具集。必须硬拒，并说出是哪个名字。
func TestRunSubTaskRefusesAToolsetNameThatDoesNotExist(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "ok"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t)})
	_, err := parent.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "read it",
		Toolsets:     []string{"read_file", "raed_file"},
	})
	if err == nil {
		t.Fatal("RunSubTask() error = nil for an unknown toolset name, want a refusal")
	}
	if !strings.Contains(err.Error(), "raed_file") {
		t.Errorf("error = %v, want it to name the tool it does not recognise", err)
	}
}

// 三个入口共用同一个判定：async 这条路也必须拒。
func TestRunSubTaskAsyncRefusesAToolsetNameThatDoesNotExist(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "ok"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t)})
	if _, err := parent.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "read it",
		Toolsets:     []string{"raed_file"},
	}); err == nil {
		t.Fatal("RunSubTaskAsync() error = nil for an unknown toolset name, want a refusal")
	}
}

// background 拼错了不能悄悄跑成前台。
func TestParseDelegateBoolRefusesAValueItCannotRecognise(t *testing.T) {
	t.Parallel()
	if _, err := parseDelegateBool("ture"); err == nil {
		t.Fatal("parseDelegateBool(\"ture\") error = nil, want a refusal: a typo must not silently run in the foreground")
	}
	for _, value := range []string{"", "true", "false", "1", "0", "yes", "no"} {
		if _, err := parseDelegateBool(value); err != nil {
			t.Errorf("parseDelegateBool(%q) error = %v, want nil", value, err)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run "RefusesAToolset|ParseDelegateBool" -count=1`
Expected: FAIL——今天未知名字被静默接受，`parseDelegateBool` 只返回一个值（编译失败）。

- [ ] **Step 3: 写共用校验函数**

在 `internal/runtime/delegation.go` 加：

```go
// validateSubTaskSpec decides whether one delegation request may be admitted,
// without creating anything. It is the single judgment RunSubTask,
// RunSubTasks and RunSubTaskAsync share: a request rejected here has started
// no child and had no side effect.
//
// It checks only what this deployment actually has. There is no provider
// descriptor to negotiate against because there is one transport.
func (r *Runtime) validateSubTaskSpec(spec SubTaskSpec) error {
	if strings.TrimSpace(spec.Goal) == "" {
		return fmt.Errorf("validate sub task: goal is required")
	}
	switch spec.Role {
	case "", roleLeaf, roleOrchestrator:
	default:
		return fmt.Errorf("validate sub task: role %q is not %q or %q", spec.Role, roleOrchestrator, roleLeaf)
	}
	if r.depth+1 > r.maxSpawnDepth {
		return fmt.Errorf("validate sub task: delegation depth %d exceeds max spawn depth %d", r.depth+1, r.maxSpawnDepth)
	}
	// Subset NARROWS, so a name it does not recognise is dropped in silence and
	// the child ends up with fewer tools than the caller asked for -- possibly
	// none. (Without, which widens, may ignore unknown names: removing a tool an
	// agent never had is a real no-op.)
	for _, name := range spec.Toolsets {
		if r.tools == nil || !r.tools.HasTool(name) {
			return fmt.Errorf("validate sub task: toolset name %q is not a tool this agent has", name)
		}
	}
	return nil
}
```

- [ ] **Step 4: 三个入口都调它**

`RunSubTask`、`RunSubTaskAsync` 里，把各自那句 `if strings.TrimSpace(spec.Goal) == "" { ... }` 换成：

```go
	if err := r.validateSubTaskSpec(spec); err != nil {
		return SubTaskResult{}, err
	}
```

（`RunSubTaskAsync` 里的零值改成 `SubTaskHandle{}`。）

`canDelegate` 的检查**位置不变**，仍在校验之后——它问的是「这个 runtime 有没有资格派」，与「这个请求合不合法」是两件事。

`newSubRuntime` 既有的深度与角色断言**保留**，作为最后一道防线。

- [ ] **Step 5: `background` 严格解析**

`internal/runtime/delegation_tool.go`：

```go
// parseDelegateBool reads the background flag. An unrecognised value is an
// error rather than false: a typo must not quietly turn a background
// delegation into a blocking one.
func parseDelegateBool(value string) (bool, error) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "", "0", "false", "no", "n":
		return false, nil
	case "1", "true", "yes", "y":
		return true, nil
	default:
		return false, fmt.Errorf("background %q is not a recognised boolean", value)
	}
}
```

`handleDelegateTask` 里的调用点改成：

```go
	background, err := parseDelegateBool(call.Arguments["background"])
	if err != nil {
		return domain.ToolResult{CallID: call.ID, Success: false, Error: err.Error()}, nil
	}
	if background {
```

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/runtime/ -count=1`
Expected: PASS。

- [ ] **Step 7: 变异验证**

1. `validateSubTaskSpec` 里的 `HasTool` 循环整段删掉 → 两条 toolset 用例必红
2. **只**把 `RunSubTaskAsync` 的调用改回旧的 goal 检查（`RunSubTask` 保持调共用函数）→ `TestRunSubTaskAsyncRefuses...` 必红。**这条专抓「三个入口共用」这条接线**
3. `parseDelegateBool` 的 `default` 改回 `return false, nil` → `TestParseDelegateBool...` 必红

- [ ] **Step 8: 提交**

```bash
git add internal/runtime/delegation.go internal/runtime/delegation_tool.go internal/runtime/delegation_validation_test.go
git commit -m "feat(runtime): 委派请求的共用前置校验，未知工具名与非法 background 不再静默降级"
```

---

### Task 7: 批量整批预检

**Files:**
- Modify: `internal/runtime/delegation.go`（`RunSubTasks`）
- Test: `internal/runtime/delegation_validation_test.go`（追加）

**Interfaces:**
- Consumes: `validateSubTaskSpec`（Task 6）
- Produces: 无新符号

**背景**：今天 `RunSubTasks` 在每个 goroutine 里才调 `RunSubTask` 做校验，等发现第 5 条非法时，**前 4 个已经带着副作用跑起来了**。子代理有它自己的副作用——`delegate_task` 的描述符自己写着 `Sensitive: true, // spawns sub-agents with side effects of their own`。

**整批拒是调度层失败**，按 `RunSubTasks` 既有契约返回**整体 error**，**不是**塞进每条的 `Err`——`Err` 的既有含义是「这条子任务跑了但失败了」，不能与「这条根本没资格启动」混为一谈。

- [ ] **Step 1: 写失败的测试**

追加到 `internal/runtime/delegation_validation_test.go`：

```go
// 一条非法就整批不启动 —— 断言的是副作用数为 0，不是「返回了 error」。
func TestRunSubTasksStartsNothingWhenOneEntryIsInvalid(t *testing.T) {
	t.Parallel()
	// 每个子代理各自跑 RunTask，都会调一次模型，所以 recorded() 的长度就是
	// 实际启动了几个子代理。
	maas := &recordingSubMaas{summary: "ok"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t)})
	started := func() int { return len(maas.recorded()) }
	specs := []SubTaskSpec{
		{ParentTaskID: "t1", Goal: "one"},
		{ParentTaskID: "t1", Goal: "two"},
		{ParentTaskID: "t1", Goal: "three", Toolsets: []string{"raed_file"}},
		{ParentTaskID: "t1", Goal: "four"},
		{ParentTaskID: "t1", Goal: "five"},
	}

	results, err := parent.RunSubTasks(context.Background(), specs)
	if err == nil {
		t.Fatal("RunSubTasks() error = nil with an invalid entry, want the whole batch refused")
	}
	if results != nil {
		t.Errorf("RunSubTasks() results = %v, want nil: a refused batch produced no results", results)
	}
	if got := started(); got != 0 {
		t.Errorf("%d children were started, want 0: a batch with an invalid entry must start none of them", got)
	}
	if !strings.Contains(err.Error(), "raed_file") {
		t.Errorf("error = %v, want it to name the offending tool", err)
	}
}

// 全部合法时批量照旧跑完，单条失败仍然进该条的 Err（既有契约不变）。
func TestRunSubTasksStillRunsEveryValidEntry(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "ok"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t)})
	started := func() int { return len(maas.recorded()) }
	specs := []SubTaskSpec{
		{ParentTaskID: "t1", Goal: "one"},
		{ParentTaskID: "t1", Goal: "two"},
	}

	results, err := parent.RunSubTasks(context.Background(), specs)
	if err != nil {
		t.Fatalf("RunSubTasks() error = %v, want nil", err)
	}
	if len(results) != 2 {
		t.Fatalf("RunSubTasks() returned %d results, want 2", len(results))
	}
	if got := started(); got != 2 {
		t.Errorf("%d children were started, want 2", got)
	}
}
```

`recordingSubMaas.recorded()`（`internal/runtime/delegation_test.go:33`）返回它见过的全部 prompt，长度即启动过的子代理数。**不要新造 Runtime 构造逻辑。**

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run RunSubTasksStarts -count=1`
Expected: FAIL——今天前两条会先跑起来，`started()` 不是 0。

- [ ] **Step 3: 加整批预检**

`RunSubTasks` 里，在 `if !r.canDelegate() { ... }` 之后、建 `results` 之前插入：

```go
	// Pre-flight the whole batch before starting any of it. A child has side
	// effects of its own, so discovering entry 5 is malformed after entries 1-4
	// are already running is not a refusal, it is a partial execution.
	for i, spec := range specs {
		if err := r.validateSubTaskSpec(spec); err != nil {
			return nil, fmt.Errorf("run sub tasks: entry %d: %w", i, err)
		}
	}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ -count=1`
Expected: PASS。

- [ ] **Step 5: 变异验证**

1. 预检循环整段删掉 → `TestRunSubTasksStartsNothingWhenOneEntryIsInvalid` 必红
2. 预检改成**只记录不返回**（收集 err 但继续往下跑）→ 同一条必红。**这条专抓「校验了但没拦住」**
3. 预检返回改成把错误塞进第 i 条的 `Err` 而不是整体 error → 必红（`results != nil` 那条断言）

- [ ] **Step 6: 全量与提交**

```bash
go test ./... -count=1 -p 1 -timeout 900s
```
退出码单独用 `echo "EXIT=$?"` 取，**不要接管道**。Expected: `EXIT=0`，0 个 FAIL。

```bash
gofmt -l .
go vet ./...
```
Expected: 前者 0 行，后者无输出。

```bash
git add internal/runtime/delegation.go internal/runtime/delegation_validation_test.go
git commit -m "feat(runtime): 批量委派整批预检，任一条目非法则一个子代理都不启动"
```

---

## 自检（写计划时已跑）

**规格覆盖**：spec §3.2 类型→Task 1；§3.3 唯一出口与零值不变量→Task 1 Step 6 + Task 2；§3.4 落盘（含读写不对称）→Task 3；§3.5 出口→Task 5；§3.6 收尾话术→Task 4；§4.1(1) toolsets→Task 6；§4.1(2) background→Task 6；§4.2 共用函数三入口→Task 6（变异 2 专守）；§4.3 校验项→Task 6 Step 3；§4.4 整批预检→Task 7；§4.5 错误可定位→Task 6/7 的断言都查了错误文本；§5 测试的 10 条接缝分别落在 Task 1(1)、2(2)、3(3,4,5)、4(6)、7(7)、6(8,9,10)。**无遗漏。**

**类型一致性**：`domain.StopReason` 在 Task 1 定义，Task 3/5 消费；`loopState.stopReason` 在 Task 1 加，Task 2/4 读；`validateSubTaskSpec` 在 Task 6 定义，Task 7 消费；`parseDelegateBool` 的签名变更只在 Task 6，其唯一调用点同任务内改完。

**夹具全部用既有真名，无占位**：`openTestSQLiteRepository`（`internal/storage/sqlite_test.go:21`）、`(*SQLiteRepository).applyColumnMigrations`（`sqlite.go:1943`）、`recordingSubMaas` 与 `.recorded()`（`internal/runtime/delegation_test.go:17,33`）、`loopingMaas` / `recordingRoundsMaas` / `unchangingReadRegistry`（`internal/runtime/multiturn_test.go`）。唯一要实施者到现场确认的是 Task 4 里 `recordingRoundsMaas` 有没有 `sawText`——`loopingMaas` 有，若前者没有则照同形补一个。
