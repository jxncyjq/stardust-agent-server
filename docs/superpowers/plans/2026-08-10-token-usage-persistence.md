# token 消耗持久化 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 token 消耗量（prompt/completion/cached/total）持久化到 `audit_events` 与 `conversation_turns` 两表，使会话 token 消耗可事后查询。

**Architecture:** token 数在 `runtime.finishRun` 与 `server` 的任务结果流里均已就绪，只是无列可写。给两表各加 4 个 `INTEGER NOT NULL DEFAULT 0` 列（CREATE 覆盖新库 + 幂等 ALTER 迁移覆盖老库），domain 结构各加 4 int 字段，写点填值、读点带出。纯 legionAgent 后端，无 API 破坏。

**Tech Stack:** Go + SQLite（database/sql）。守 legionAgent CLAUDE.md fail-loud 铁律。

**Spec:** `docs/superpowers/specs/2026-08-10-token-usage-persistence-design.md`

**仓库根：** 所有路径相对 `legion/legionAgent/`。命令在该目录跑：`go build ./... && go vet ./... && go test ./...`、`gofmt -l .`。

---

## 文件结构

| 文件 | 改动 |
|---|---|
| `internal/domain/types.go` | `AuditEvent`（~112）、`ConversationTurn`（~170）各加 4 字段 |
| `internal/storage/sqlite.go` | 两表 CREATE（1892/1946）加列；`columnMigrations`（1640）加 8 条；audit INSERT（948）/SELECT（961+scan 974）；turn INSERT×2（400/435）；`ListConversationTurns` SELECT（467）；`scanConversationTurn`（1757） |
| `internal/runtime/runtime.go` | `finishRun`（696）audit 字面量填 `st.*Tokens` |
| `internal/server/http.go` | `recordAssistantTurn`（1071）加 usage 参；调用点（1050）传 `usage` |
| `internal/storage/*_test.go`, `internal/runtime/*_test.go` | 测试 |

> 关键一致性：`scanConversationTurn`（1757）只被 `ListConversationTurns`（467 SELECT）喂；两者列顺序必须同步。1613 的 `FROM conversation_turns t` 是 FTS backfill，列不同、不涉及本次。

---

### Task 1: Schema — 两表加 4 列 + 迁移

**Files:** `internal/storage/sqlite.go`；`internal/storage/sqlite_migration_test.go`（新建或追加到现有 migration 测试）

**Context:** CREATE 语句覆盖新库；`columnMigrations` 列表（1640）覆盖老库（`applyColumnMigrations` 幂等，容忍 "duplicate column name"）。两处都要加，否则新库缺迁移路径/老库缺列。

- [ ] **Step 1: 失败测试** — `internal/storage/sqlite_migration_test.go`（若已有 migration 测试文件则追加）。测「新建库两表含 4 新列」+「迁移幂等」：
```go
package storage

import (
	"context"
	"testing"
)

func columnExists(t *testing.T, r *SQLiteRepository, table, col string) bool {
	t.Helper()
	rows, err := r.db.QueryContext(context.Background(), "PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == col {
			return true
		}
	}
	return false
}

func TestTokenColumnsExistAfterInit(t *testing.T) {
	r := newTestRepo(t) // use whatever the package's existing test helper is; see other _test.go
	for _, tc := range []struct{ table, col string }{
		{"audit_events", "prompt_tokens"}, {"audit_events", "completion_tokens"},
		{"audit_events", "cached_tokens"}, {"audit_events", "total_tokens"},
		{"conversation_turns", "prompt_tokens"}, {"conversation_turns", "completion_tokens"},
		{"conversation_turns", "cached_tokens"}, {"conversation_turns", "total_tokens"},
	} {
		if !columnExists(t, r, tc.table, tc.col) {
			t.Errorf("missing column %s.%s", tc.table, tc.col)
		}
	}
}

func TestApplyColumnMigrationsIdempotent(t *testing.T) {
	r := newTestRepo(t)
	if err := r.applyColumnMigrations(context.Background()); err != nil {
		t.Fatalf("re-running migrations must be idempotent: %v", err)
	}
}
```
> 先看 `internal/storage` 现有 `_test.go` 里如何建测试库（helper 名，如 `newTestRepo`/`openTestDB`），把 `newTestRepo(t)` 换成实际 helper。若无 helper，用 `NewSQLiteRepository(ctx, ":memory:")` 之类现有构造（照现有测试）。

- [ ] **Step 2: 确认失败** `go test ./internal/storage/ -run 'TokenColumns|ColumnMigrationsIdempotent'` → 缺列失败。

- [ ] **Step 3: 实现**
  1. CREATE `conversation_turns`（1892）：在 `created_at` 列后加 4 列：
```sql
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens INTEGER NOT NULL DEFAULT 0
```
  2. CREATE `audit_events`（1946）：同样加这 4 列。
  3. `columnMigrations`（1640 slice）追加 8 条：
```go
	{table: "audit_events", column: "prompt_tokens", stmt: `ALTER TABLE audit_events ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`},
	{table: "audit_events", column: "completion_tokens", stmt: `ALTER TABLE audit_events ADD COLUMN completion_tokens INTEGER NOT NULL DEFAULT 0`},
	{table: "audit_events", column: "cached_tokens", stmt: `ALTER TABLE audit_events ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`},
	{table: "audit_events", column: "total_tokens", stmt: `ALTER TABLE audit_events ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`},
	{table: "conversation_turns", column: "prompt_tokens", stmt: `ALTER TABLE conversation_turns ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`},
	{table: "conversation_turns", column: "completion_tokens", stmt: `ALTER TABLE conversation_turns ADD COLUMN completion_tokens INTEGER NOT NULL DEFAULT 0`},
	{table: "conversation_turns", column: "cached_tokens", stmt: `ALTER TABLE conversation_turns ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`},
	{table: "conversation_turns", column: "total_tokens", stmt: `ALTER TABLE conversation_turns ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`},
```

- [ ] **Step 4: 确认通过** `go test ./internal/storage/ -run 'TokenColumns|ColumnMigrationsIdempotent' -v` → 过；`gofmt -l internal/storage/sqlite.go`（空）。

- [ ] **Step 5: 提交**
```bash
git add internal/storage/sqlite.go internal/storage/sqlite_migration_test.go
git commit -m "feat(storage): add token columns to audit_events + conversation_turns schema"
```

---

### Task 2: Domain — 结构加 4 字段

**Files:** `internal/domain/types.go`

**Context:** 纯字段增补，无逻辑。加完 `go build` 应仍绿（未使用的新字段不报错）。

- [ ] **Step 1: 实现** — `AuditEvent`（~112）在 `CreatedAt` 前/后加：
```go
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
```
`ConversationTurn`（~170）加同样 4 字段。

- [ ] **Step 2: 确认编译** `go build ./... && gofmt -l internal/domain/types.go`（空）。

- [ ] **Step 3: 提交**
```bash
git add internal/domain/types.go
git commit -m "feat(domain): add token fields to AuditEvent + ConversationTurn"
```

---

### Task 3: 存储 — audit INSERT/SELECT 带 token

**Files:** `internal/storage/sqlite.go`；`internal/storage/sqlite_audit_test.go`（新建或追加）

- [ ] **Step 1: 失败测试** — 往返：写一条带 token 的 `model_inference_completed`，`ListAuditEvents` 读回一致；一条无 token 的（如 `task_completed`）读回 0：
```go
func TestAuditEventTokenRoundTrip(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	in := domain.AuditEvent{
		ID: "t1:model-audit-1", RequestID: "t1:run", SubjectType: "model", SubjectID: "t1",
		Action: "model_inference_completed", Hash: "memory",
		PromptTokens: 1200, CompletionTokens: 340, CachedTokens: 800, TotalTokens: 1540,
		CreatedAt: time.Now(),
	}
	if err := r.AppendAuditEvent(ctx, in); err != nil {
		t.Fatal(err)
	}
	events, err := r.ListAuditEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got *domain.AuditEvent
	for i := range events {
		if events[i].ID == in.ID {
			got = &events[i]
		}
	}
	if got == nil {
		t.Fatal("event not found")
	}
	if got.PromptTokens != 1200 || got.CompletionTokens != 340 || got.CachedTokens != 800 || got.TotalTokens != 1540 {
		t.Fatalf("token mismatch: %+v", got)
	}
}

func TestAuditEventWithoutTokensReadsZero(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	if err := r.AppendAuditEvent(ctx, domain.AuditEvent{ID: "t1:audit-1", RequestID: "t1:run", SubjectType: "task", SubjectID: "t1", Action: "task_completed", Hash: "memory", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	events, _ := r.ListAuditEvents(ctx)
	for _, e := range events {
		if e.ID == "t1:audit-1" && e.TotalTokens != 0 {
			t.Fatalf("want 0 tokens, got %d", e.TotalTokens)
		}
	}
}
```

- [ ] **Step 2: 确认失败** `go test ./internal/storage/ -run TestAuditEvent` → token 读回 0（列没写）失败。

- [ ] **Step 3: 实现**
  - `AppendAuditEvent`（948 INSERT）→ 列/占位/值都加 4：
```go
		INSERT INTO audit_events (
			id, request_id, subject_type, subject_id, action, hash, created_at,
			prompt_tokens, completion_tokens, cached_tokens, total_tokens
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, event.ID, event.RequestID, event.SubjectType, event.SubjectID, event.Action, event.Hash, formatTime(event.CreatedAt),
		event.PromptTokens, event.CompletionTokens, event.CachedTokens, event.TotalTokens)
```
  - `ListAuditEvents`（961 SELECT + 974 Scan）→ SELECT 加 4 列，Scan 加 4 目标：
```go
		SELECT id, request_id, subject_type, subject_id, action, hash, created_at,
			prompt_tokens, completion_tokens, cached_tokens, total_tokens
		FROM audit_events
		ORDER BY created_at, id
```
```go
		if err := rows.Scan(&event.ID, &event.RequestID, &event.SubjectType, &event.SubjectID, &event.Action, &event.Hash, &createdAt,
			&event.PromptTokens, &event.CompletionTokens, &event.CachedTokens, &event.TotalTokens); err != nil {
```

- [ ] **Step 4: 确认通过** `go test ./internal/storage/ -run TestAuditEvent -v`；`gofmt -l internal/storage/sqlite.go`（空）。

- [ ] **Step 5: 提交**
```bash
git add internal/storage/sqlite.go internal/storage/sqlite_audit_test.go
git commit -m "feat(storage): persist + read token counts on audit events"
```

---

### Task 4: 存储 — conversation_turns INSERT/SELECT 带 token

**Files:** `internal/storage/sqlite.go`；`internal/storage/sqlite_turns_test.go`（新建或追加）

**Context:** 改两个 INSERT（`AppendConversationTurn` 400、`AppendConversationTurnIfAbsent` 435）、`ListConversationTurns` SELECT（467）、共享 `scanConversationTurn`（1757）。SELECT 列顺序与 scan 目标必须同步。

- [ ] **Step 1: 失败测试**：
```go
func TestConversationTurnTokenRoundTrip(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	// seed the session row if the schema requires it (mirror existing turn tests).
	turn := domain.ConversationTurn{
		ID: "t1:assistant", SessionID: "s1", TaskID: "t1", AgentID: "a1",
		Role: domain.ConversationRoleAssistant, Content: "hi",
		PromptTokens: 1200, CompletionTokens: 340, CachedTokens: 800, TotalTokens: 1540,
		CreatedAt: time.Now(),
	}
	if err := r.AppendConversationTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	turns, err := r.ListConversationTurns(ctx, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].PromptTokens != 1200 || turns[0].TotalTokens != 1540 {
		t.Fatalf("token mismatch: %+v", turns)
	}
}

func TestConversationTurnIfAbsentPersistsTokens() // 同理，用 AppendConversationTurnIfAbsent 写 role=user 的 0-token turn，读回 0
```
> 若建 session/turn 需要前置行（外键或 FTS），照 `internal/storage` 现有 turn 测试的 seed 方式写。第二个测试请补全成完整函数（仿第一个），断言 user turn 4 值皆 0。

- [ ] **Step 2: 确认失败** `go test ./internal/storage/ -run TestConversationTurn`.

- [ ] **Step 3: 实现**
  - `AppendConversationTurn`（400 INSERT）列/占位/值加 4：
```go
			INSERT INTO conversation_turns (
				id, session_id, task_id, agent_id, model_profile, role, content, created_at,
				prompt_tokens, completion_tokens, cached_tokens, total_tokens
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, turn.ID, turn.SessionID, turn.TaskID, turn.AgentID, turn.ModelProfile, string(turn.Role), turn.Content, formatTime(turn.CreatedAt),
		turn.PromptTokens, turn.CompletionTokens, turn.CachedTokens, turn.TotalTokens)
```
  - `AppendConversationTurnIfAbsent`（435 `INSERT OR IGNORE`）同样加 4 列/占位/值。
  - `ListConversationTurns` SELECT（467）加 4 列（顺序接在 `created_at` 后）：
```sql
			SELECT id, session_id, task_id, agent_id, model_profile, role, content, created_at,
				prompt_tokens, completion_tokens, cached_tokens, total_tokens
			FROM conversation_turns
```
  - `scanConversationTurn`（1757）Scan 加 4 目标（顺序对齐）：
```go
	if err := row.Scan(&turn.ID, &turn.SessionID, &turn.TaskID, &turn.AgentID, &turn.ModelProfile, &role, &turn.Content, &createdAt,
		&turn.PromptTokens, &turn.CompletionTokens, &turn.CachedTokens, &turn.TotalTokens); err != nil {
```
  > 检查是否有别的 SELECT 也调用 `scanConversationTurn`（`grep -n scanConversationTurn internal/storage/sqlite.go`）；若有，其 SELECT 列表必须同步加这 4 列，否则 scan 错列。已知仅 `ListConversationTurns` 使用。

- [ ] **Step 4: 确认通过** `go test ./internal/storage/ -run TestConversationTurn -v` + 现有 turns 测试回归 `go test ./internal/storage/`；`gofmt -l internal/storage/sqlite.go`（空）。

- [ ] **Step 5: 提交**
```bash
git add internal/storage/sqlite.go internal/storage/sqlite_turns_test.go
git commit -m "feat(storage): persist + read token counts on conversation turns"
```

---

### Task 5: Runtime — finishRun audit 填 token

**Files:** `internal/runtime/runtime.go`；`internal/runtime/*_test.go`（追加/新建断言）

**Context:** `finishRun`（696）的 `model_inference_completed` `domain.AuditEvent` 字面量当前不含 token；`st.promptTokens/completionTokens/cachedTokens/totalTokens` 已在作用域（同函数下方写进 `domain.TaskRun`）。

- [ ] **Step 1: 实现** — `finishRun` 里 `r.audit.Append(ctx, domain.AuditEvent{...})`（696）加 4 字段：
```go
	if err := r.audit.Append(ctx, domain.AuditEvent{
		ID:               task.ID + ":model-audit-1",
		RequestID:        requestID,
		SubjectType:      "model",
		SubjectID:        task.ID,
		Action:           "model_inference_completed",
		Hash:             "memory",
		PromptTokens:     st.promptTokens,
		CompletionTokens: st.completionTokens,
		CachedTokens:     st.cachedTokens,
		TotalTokens:      st.totalTokens,
		CreatedAt:        time.Now(),
	}); err != nil {
```

- [ ] **Step 2: 测试** — 在现有 runtime 测试里（找测 `finishRun`/完整 run 的测试，或加一个），用一个记录 append 的 fake audit（现有测试应有 audit stub），断言捕获到的 `model_inference_completed` 事件带非零 token（当 loop state 有 usage 时）。若现有测试基础设施不便断言，最小新增一个针对 `finishRun` 的窄测试，构造带 `st.*Tokens` 的 `loopState` 并断言 fake audit 收到对应值。
  - 先 `grep -rn "model_inference_completed\|fakeAudit\|auditRecorder\|audit.Append" internal/runtime/*_test.go` 找现有断言点复用。

- [ ] **Step 3: 确认通过** `go test ./internal/runtime/ -v`（相关用例）；`gofmt -l internal/runtime/runtime.go`（空）。

- [ ] **Step 4: 提交**
```bash
git add internal/runtime/runtime.go internal/runtime/
git commit -m "feat(runtime): record token counts on model_inference_completed audit"
```

---

### Task 6: Server — recordAssistantTurn 填 token

**Files:** `internal/server/http.go`；`internal/server/*_test.go`

**Context:** `recordAssistantTurn`（1071）签名 `(ctx, task, result)`，不接 token。调用点（1050）已有 `usage`（`result, usage, err := s.taskResult(taskID)`，1039），`usage` 含 `PromptTokens/CompletionTokens/CachedTokens/TotalTokens`（见 taskResultResponse 映射 1059-1062）。

- [ ] **Step 1: 失败测试** — 找现有 `recordAssistantTurn` 或任务结果端点测试（`grep -rn "recordAssistantTurn\|taskResult\|:assistant" internal/server/*_test.go`）。加/改断言：完成任务的 assistant turn 落盘带对应 token。若直接测 `recordAssistantTurn` 需要一个 fake session store 捕获 turn——照现有测试的 store stub。示例断言核心：
```go
// after driving the task-result endpoint / calling recordAssistantTurn with a
// usage carrying {Prompt:1200, Completion:340, Cached:800, Total:1540}
// assert the captured ConversationTurn has those 4 values.
```

- [ ] **Step 2: 确认失败** `go test ./internal/server/ -run <相关用例>` → turn token 为 0。

- [ ] **Step 3: 实现**
  - 改签名（`usage` 的具体类型见 `taskResult` 返回值定义；用其类型，或直接传 4 个 int）：
```go
func (s *HTTPServer) recordAssistantTurn(ctx context.Context, task domain.Task, result string, usage <usageType>) error {
	...
	turn := domain.ConversationTurn{
		ID:               task.ID + ":assistant",
		SessionID:        task.SessionID,
		TaskID:           task.ID,
		AgentID:          task.AgentID,
		Role:             domain.ConversationRoleAssistant,
		Content:          result,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		CachedTokens:     usage.CachedTokens,
		TotalTokens:      usage.TotalTokens,
		CreatedAt:        time.Now(),
	}
	...
}
```
  - 调用点（1050）改 `s.recordAssistantTurn(r.Context(), task, result, usage)`。
  > 先 `grep -n "func (s \*HTTPServer) taskResult" internal/server/http.go` 看 `usage` 的返回类型名，`<usageType>` 用真实类型（可能是内部结构或 taskResultResponse 的一部分）。

- [ ] **Step 4: 确认通过** `go test ./internal/server/ -v`（相关用例 + 回归）；`gofmt -l internal/server/http.go`（空）。

- [ ] **Step 5: 提交**
```bash
git add internal/server/http.go internal/server/
git commit -m "feat(server): persist token counts on assistant conversation turn"
```

---

### Task 7: 全量校验

- [ ] **Step 1** `go build ./... && go vet ./... && go test ./...`（全绿）。
- [ ] **Step 2** `gofmt -l .`（输出为空）。
- [ ] **Step 3（可选真机验证）** 跑一次带真实推理的任务 → 查库确认落盘：
```bash
sqlite3 agent.db "SELECT id, action, prompt_tokens, completion_tokens, cached_tokens, total_tokens FROM audit_events WHERE action='model_inference_completed' ORDER BY created_at DESC LIMIT 3;"
sqlite3 agent.db "SELECT id, role, prompt_tokens, total_tokens FROM conversation_turns WHERE role='assistant' ORDER BY created_at DESC LIMIT 3;"
```
期望：非零 token。
- [ ] **Step 4** 收尾提交（如有微调）。

---

## 范围外（后续）
- GUI 侧读取并展示历史 token（本 plan 只保证后端落盘 + 可查；GUI `app.go` 的 `ListAuditEvents`/turns 若要显示历史 token，另做）。
- assistant turn 的 usage 若因事件消费/过期拿不到 → 改由 runtime 写 turn 时直接携带 token（可靠性优化）。
- 跨会话 token 聚合 / 成本换算。

---

## Self-Review

**Spec 覆盖：** 两表加 4 typed 列（CREATE+迁移）→T1；domain 加字段→T2；audit 写读→T3；turn 写读→T4；finishRun 填 audit token→T5；recordAssistantTurn 填 turn token→T6；全量校验+真机查库→T7。fail-loud（0 属可选、不伪造、error 包装）贯穿。

**类型一致性：** `AuditEvent`/`ConversationTurn` 加的 4 字段名（PromptTokens/CompletionTokens/CachedTokens/TotalTokens，T2）在 T3/T4/T5/T6 一致引用；SQL 列名（prompt_tokens/…）在 CREATE/迁移/INSERT/SELECT 一致；`scanConversationTurn` scan 顺序与 `ListConversationTurns` SELECT 顺序同步（T4 强调）；`st.*Tokens`（T5）与 `usage.*Tokens`（T6）分别是 runtime/server 两侧同源数据。

**占位符：** T5/T6 的测试与 `<usageType>` 因依赖现有测试基础设施/内部类型，给出 grep 定位指令 + 断言契约（非逐行）；schema/domain/INSERT/SELECT 等机械改动含完整代码。无 TBD。
