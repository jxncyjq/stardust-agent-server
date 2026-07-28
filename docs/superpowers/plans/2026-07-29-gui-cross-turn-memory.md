# 修 GUI 跨轮记忆 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Steps use checkbox (`- [ ]`).

**Goal:** GUI serve 路径按 `task.SessionID` 加载 session 最近轮（排除当前轮）注入 `Config.ConversationTurns`，让模型看到前几轮对话。

**Architecture:** `AgentRuntimeResolver` 加最小接口 `ConversationTurnLister`；`ResolveTaskRunner` 加载→过滤当前轮→截断→取最近 N→塞进 `NewRuntime(Config{ConversationTurns: ...})`；serve 装配传 sqlite repo。

**Tech Stack:** Go 标准库 + 现有 `domain.ConversationTurn`。

## Global Constraints

- **Fail-loud**：`ListConversationTurns` 出错 → `ResolveTaskRunner` 返回 error（`fmt.Errorf("...%w")`），**绝不**静默注入空历史假装无前文。
- 合法可选（非兜底）：`lister == nil`（未装配）或 `task.SessionID == ""`（无会话）→ 不注入、不报错。
- 不改 CLI/TUI 路径（已正常）、不改 `cognitive.conversationBlock` 渲染。
- `go build/vet/test ./...` 全绿、`gofmt -l .` 空；公开/非导出符号 Go doc；错误路径有测试断言。

## 现状 seam（实测）

- `internal/runtime/agent_resolver.go`：`AgentRuntimeResolver`/`AgentRuntimeResolverConfig` 无 session 相关字段；`ResolveTaskRunner(ctx, task)(domain.Agent, TaskRunner, bool, error)`；末尾 `NewRuntime(Config{Maas:..., DisabledTools: agentCfg.DisabledTools})`（约 :209-224）。
- `internal/runtime/runtime.go:77`：`Config.ConversationTurns []domain.ConversationTurn`（已存在，只是没人填）。
- `internal/runtime/runtime.go`：包内有 `truncateText(text string, maxChars int) string`（可复用于按 MaxTurnChars 截断）。
- serve 装配：`internal/cli/command.go:2207` `agentruntime.NewAgentRuntimeResolver(agentruntime.AgentRuntimeResolverConfig{...})`。
- `internal/storage/sqlite.go:460`：`(*SQLiteRepository).ListConversationTurns(ctx, sessionID, limit) ([]domain.ConversationTurn, error)`。
- 当前轮已在任务入队前写库：`internal/server/http.go:900` `recordUserTurn`，turn ID = `<taskID>:user`，`TaskID == task.ID`。
- 配置：`cfg.Session.DefaultRecentTurns`（=6）、`cfg.Session.MaxTurnChars`（=6000）。

---

### Task 1: resolver 加 ConversationTurnLister + 加载注入

**Files:**
- Modify: `internal/runtime/agent_resolver.go`
- Test: `internal/runtime/agent_resolver_test.go`

**Interfaces:**
- Produces:
  - `type ConversationTurnLister interface { ListConversationTurns(ctx context.Context, sessionID string, limit int) ([]domain.ConversationTurn, error) }`
  - `AgentRuntimeResolverConfig` 加字段 `ConversationTurns ConversationTurnLister`；`AgentRuntimeResolver` 加同名字段（`NewAgentRuntimeResolver` 里赋值）。
  - `func (r *AgentRuntimeResolver) recentTurnsForTask(ctx context.Context, task domain.Task) ([]domain.ConversationTurn, error)` —— 加载→过滤当前轮→截断→取最近 N。

- [ ] **Step 1: Write the failing test**

```go
// agent_resolver_test.go
type fakeTurnLister struct {
	turns []domain.ConversationTurn
	err   error
	gotLimit int
}

func (f *fakeTurnLister) ListConversationTurns(ctx context.Context, sessionID string, limit int) ([]domain.ConversationTurn, error) {
	f.gotLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.turns, nil
}

func TestRecentTurnsForTaskExcludesCurrentTurn(t *testing.T) {
	task := domain.Task{ID: "task-9", SessionID: "s1", AgentID: "a"}
	lister := &fakeTurnLister{turns: []domain.ConversationTurn{
		{ID: "t1:user", TaskID: "task-1", Role: domain.ConversationRoleUser, Content: "OLD-1"},
		{ID: "t1:assistant", TaskID: "task-1", Role: domain.ConversationRoleAssistant, Content: "OLD-2"},
		{ID: "task-9:user", TaskID: "task-9", Role: domain.ConversationRoleUser, Content: "CURRENT-TURN"},
	}}
	r := &AgentRuntimeResolver{conversationTurns: lister, rootConfig: config.Config{
		Session: config.SessionConfig{DefaultRecentTurns: 6, MaxTurnChars: 6000},
	}}
	got, err := r.recentTurnsForTask(t.Context(), task)
	if err != nil {
		t.Fatalf("recentTurnsForTask err = %v, want nil", err)
	}
	for _, turn := range got {
		if turn.TaskID == task.ID {
			t.Fatalf("current turn must be excluded, got %+v", got)
		}
	}
	if len(got) != 2 || got[0].Content != "OLD-1" {
		t.Fatalf("got %+v, want the 2 older turns", got)
	}
}

func TestRecentTurnsForTaskTruncatesAndFailsLoud(t *testing.T) {
	task := domain.Task{ID: "task-9", SessionID: "s1", AgentID: "a"}
	long := strings.Repeat("x", 100)
	r := &AgentRuntimeResolver{
		conversationTurns: &fakeTurnLister{turns: []domain.ConversationTurn{
			{ID: "t1:user", TaskID: "task-1", Role: domain.ConversationRoleUser, Content: long},
		}},
		rootConfig: config.Config{Session: config.SessionConfig{DefaultRecentTurns: 6, MaxTurnChars: 10}},
	}
	got, err := r.recentTurnsForTask(t.Context(), task)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len([]rune(got[0].Content)) > 60 { // 10 + 截断标记裕量
		t.Fatalf("content not truncated: %d runes", len([]rune(got[0].Content)))
	}

	// fail-loud：lister 出错必须返回 error，不得静默空注入
	rErr := &AgentRuntimeResolver{
		conversationTurns: &fakeTurnLister{err: fmt.Errorf("db down")},
		rootConfig:        config.Config{Session: config.SessionConfig{DefaultRecentTurns: 6}},
	}
	if _, err := rErr.recentTurnsForTask(t.Context(), task); err == nil {
		t.Fatalf("lister error must propagate (fail-loud), got nil")
	}
}

func TestRecentTurnsForTaskSkipsWhenNoSessionOrLister(t *testing.T) {
	task := domain.Task{ID: "task-9", AgentID: "a"} // SessionID 空
	r := &AgentRuntimeResolver{
		conversationTurns: &fakeTurnLister{turns: []domain.ConversationTurn{{ID: "x", TaskID: "t"}}},
		rootConfig:        config.Config{Session: config.SessionConfig{DefaultRecentTurns: 6}},
	}
	got, err := r.recentTurnsForTask(t.Context(), task)
	if err != nil || len(got) != 0 {
		t.Fatalf("no SessionID → (nil,nil), got (%v,%v)", got, err)
	}
	// lister == nil 同样不注入、不 panic
	rNil := &AgentRuntimeResolver{rootConfig: config.Config{Session: config.SessionConfig{DefaultRecentTurns: 6}}}
	got, err = rNil.recentTurnsForTask(t.Context(), domain.Task{ID: "t", SessionID: "s1"})
	if err != nil || len(got) != 0 {
		t.Fatalf("nil lister → (nil,nil), got (%v,%v)", got, err)
	}
}
```
（import 需 `strings`/`fmt`/`config`/`domain`；`AgentRuntimeResolver` 的字段名以实现为准——本 plan 用 `conversationTurns`。）

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/runtime/ -run TestRecentTurnsForTask -v`
Expected: FAIL — `unknown field conversationTurns` / `undefined: recentTurnsForTask` / `undefined: ConversationTurnLister`。

- [ ] **Step 3: Implement**

在 `agent_resolver.go`：
```go
// ConversationTurnLister loads a session's recent conversation turns. It is the
// one method the resolver needs from the session store, kept as its own
// interface so the runtime package does not depend on the whole store.
type ConversationTurnLister interface {
	ListConversationTurns(ctx context.Context, sessionID string, limit int) ([]domain.ConversationTurn, error)
}
```
`AgentRuntimeResolverConfig` 加：
```go
	// ConversationTurns loads the session history injected into each task's
	// prompt (the "Recent conversation" block). Nil disables injection — the
	// serve path without a session store, and tests. Without it a GUI task runs
	// with no cross-turn memory at all.
	ConversationTurns ConversationTurnLister
```
`AgentRuntimeResolver` 结构加 `conversationTurns ConversationTurnLister`，`NewAgentRuntimeResolver` 里 `conversationTurns: cfg.ConversationTurns`。

新增方法：
```go
// recentTurnsForTask loads the session turns to inject into this task's prompt:
// the most recent DefaultRecentTurns turns of task.SessionID, excluding the
// task's own user turn (the HTTP layer records it before enqueuing, so it would
// otherwise be duplicated alongside task.Input), each truncated to
// Session.MaxTurnChars.
//
// A nil lister or an empty task.SessionID is a legitimate "no session history"
// state and yields (nil, nil). A store failure is NOT: it returns an error, so
// a lost history is never mistaken for an empty one (CLAUDE.md fail-loud).
func (r *AgentRuntimeResolver) recentTurnsForTask(ctx context.Context, task domain.Task) ([]domain.ConversationTurn, error) {
	if r.conversationTurns == nil || strings.TrimSpace(task.SessionID) == "" {
		return nil, nil
	}
	limit := r.rootConfig.Session.DefaultRecentTurns
	if limit <= 0 {
		limit = 6
	}
	// +1: the task's own user turn is already persisted and will be filtered out.
	turns, err := r.conversationTurns.ListConversationTurns(ctx, task.SessionID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("list conversation turns for session %q: %w", task.SessionID, err)
	}
	out := make([]domain.ConversationTurn, 0, len(turns))
	for _, turn := range turns {
		if turn.TaskID == task.ID {
			continue
		}
		turn.Content = truncateText(turn.Content, r.rootConfig.Session.MaxTurnChars)
		out = append(out, turn)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}
```
在 `ResolveTaskRunner` 里、`NewRuntime(Config{...})` **之前**加载：
```go
	recentTurns, err := r.recentTurnsForTask(ctx, task)
	if err != nil {
		return domain.Agent{}, nil, false, err
	}
```
并在 `NewRuntime(Config{...})` 里加一行：
```go
		ConversationTurns:     recentTurns,
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/runtime/ -run TestRecentTurnsForTask -count=1 -v && go build ./... && go vet ./...`
Expected: PASS + build/vet 绿。

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/agent_resolver.go internal/runtime/agent_resolver_test.go
git commit -m "fix(runtime): resolver 按 session 加载最近轮注入(排除当前轮, fail-loud)"
```

---

### Task 2: serve 装配传入 session store + 端到端断言

**Files:**
- Modify: `internal/cli/command.go`（约 :2207 `AgentRuntimeResolverConfig{...}`）
- Test: `internal/runtime/agent_resolver_test.go`

**Interfaces:**
- Consumes: `ConversationTurnLister`、`recentTurnsForTask`（Task 1）。
- Produces: serve 构建的 resolver 带上实现了 `ListConversationTurns` 的 store（`*storage.SQLiteRepository` 满足该方法，见 sqlite.go:460）。

- [ ] **Step 1: Write the failing test**

```go
// 端到端：resolver 构建出的 runner 其 prompt 含 "Recent conversation" 与历史内容。
// 按 agent_resolver_test.go 既有 harness 构造 resolver（registry/maasFactory/audit/events 等），
// 额外传 ConversationTurns: &fakeTurnLister{turns: [...含 "HISTORY-MARKER" 的历史轮...]}，
// 然后 ResolveTaskRunner 得到 runner，触发一次 RunTask（既有 fake maas），
// 断言发给模型的 prompt 含 "Recent conversation" 且含 "HISTORY-MARKER"。
func TestResolveTaskRunnerInjectsSessionHistory(t *testing.T) {
	// 按既有 harness 编写；核心断言：prompt 含 "Recent conversation" + "HISTORY-MARKER"
}
```
（严格按 `internal/runtime/agent_resolver_test.go` 既有构造方式写，别另起 harness。）

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/runtime/ -run TestResolveTaskRunnerInjectsSessionHistory -v`
Expected: FAIL（未装配前 prompt 无 "Recent conversation"；Task 1 已实现则应在 resolver 传了 lister 时通过——若已通过，说明 Task 1 覆盖，本步改为验证 serve 装配 build 正确）。

- [ ] **Step 3: Implement**

`internal/cli/command.go` 约 :2207 的 `agentruntime.AgentRuntimeResolverConfig{...}` 里加一行：
```go
		ConversationTurns: <serve 已有的 session store 变量>,
```
**执行者**：在该函数内确认已有的、实现了 `ListConversationTurns(ctx, sessionID, limit)` 的 store 变量名（serve 里构建 HTTP server 时传给 `SessionStore` 的那个，通常是 sqlite repository / store 变量）。若该变量在 resolver 构建之后才出现，把 resolver 构建移到其后，或提前构建 store —— **不要**新建第二个 store 实例（会导致两套连接）。

- [ ] **Step 4: Run + 全量门禁**

Run: `go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l .`
Expected: 全绿；gofmt 空。

- [ ] **Step 5: Commit**

```bash
git add internal/cli/command.go internal/runtime/agent_resolver_test.go
git commit -m "fix(cli): serve 装配把 session store 传给 resolver，打通 GUI 跨轮记忆"
```

---

## Self-Review

- **Spec 覆盖**：最小接口=Task1 `ConversationTurnLister`；加载/过滤当前轮/截断/取最近 N=Task1 `recentTurnsForTask`；注入 Config=Task1 Step3；serve 装配=Task2；fail-loud=Task1 测试断言；nil/空 SessionID 可选=Task1 测试。均覆盖。
- **占位**：Task2 测试体与 store 变量名标注「按既有 harness / 就地确认」——因 serve 函数变量名与测试 harness 因文件而异，执行时对齐；核心断言与约束（不新建第二 store）已明确，非逻辑占位。
- **类型一致**：`ConversationTurnLister`/`AgentRuntimeResolverConfig.ConversationTurns`/`AgentRuntimeResolver.conversationTurns`/`recentTurnsForTask`/`Config.ConversationTurns`/`truncateText` 跨任务一致。
