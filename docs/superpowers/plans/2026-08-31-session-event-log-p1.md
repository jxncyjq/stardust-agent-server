# 会话事件日志 P1 —— 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地会话事件日志的存储层——表、`SessionEventStore` 接口与 SQLite 实现，以及 spec §4.3 的六条不变量，其中包括崩溃恢复。

**Architecture:** 一行一事件的 `session_events` 表（`PRIMARY KEY(session_id, seq)`）。`Append` 在一个事务里断言首个 seq 等于库里的 next-seq；`ReadFrom` 只读后缀不改库；`Load` 读全量并在同一事务里为半个 turn 补出合成关闭事件，使重建出的历史是合法的 provider transcript。每会话一把写锁串行化写入。

**Tech Stack:** Go 1.26、`database/sql` + 现有 SQLite 仓储（`internal/storage/sqlite.go`）、`encoding/json`。不引入新依赖。

## Global Constraints

- Spec：`docs/superpowers/specs/2026-08-31-session-event-log-and-trajectory-design.md`（master `b311335`）。本计划只做其中的 **P1**。
- **fail-loud 铁律**（`legionAgent/CLAUDE.md` §0）：禁止兜底/静默跳过/零值假装正常；错误一律 `fmt.Errorf("<动作> <标识>: %w", err)` 包装。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿，`gofmt -l .` 为空。
- 每条不变量都要有断言，且**变异可验红**——每个任务的最后一步会写明「删掉什么会让哪条测试红」，实现者必须真的跑一遍。
- 事件类型与取值取自 spec §4.1：`turn` 每会话单调（不随任务/进程重置），`step` 每 turn 内从 0 重置；`step/end` 的 reason ∈ `completed|failed|cancelled|max_tokens`；`turn/end` 的 reason ∈ `completed|failed|cancelled|interrupted`，`interrupted` 只由崩溃恢复补出。
- **P1 不包括**：发射点接入、三个屏障、投影、删 `conversation_turns`、FTS5、`/events` 端点、前端。本计划不得触碰这些。

### 与 spec 的一处有意偏离（实现者请照此做，不要改回去）

spec §4.4 写的是「写锁 + **内存 next-seq 游标**」。本计划用「写锁 + **在事务内查 `MAX(seq)`**」实现同一个不变量：库是唯一权威，不存在内存游标与库不一致的可能。少一个可漂移的状态，代价是每次 `Append` 多一次索引查询（主键覆盖，代价可忽略）。真测出慢再引入游标。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/domain/session_event.go`（新建） | `SessionEvent` 类型、事件类型闭集、reason 闭集、类型校验 |
| `internal/domain/session_event_test.go`（新建） | 闭集与校验的断言 |
| `internal/port/session_events.go`（新建） | `SessionEventStore` 接口 —— 领域侧看到的契约 |
| `internal/storage/sqlite.go`（修改，`schemaStatements`） | 建 `session_events` 表 |
| `internal/storage/session_events.go`（新建） | SQLite 实现：`Append` / `ReadFrom` / `Load` / 崩溃恢复 / per-session 串行 |
| `internal/storage/session_events_test.go`（新建） | 六条不变量与并发的断言 |

存储实现单独成文件而不是塞进 `sqlite.go`：那个文件已经两千多行，而这块逻辑（恢复、串行、连续性）是自成一体的一组不变量，放一起才读得懂。

---

### Task 1: 事件类型与取值闭集

**Files:**
- Create: `internal/domain/session_event.go`
- Test: `internal/domain/session_event_test.go`

**Interfaces:**
- Consumes: 无
- Produces: `domain.SessionEvent{Seq int64; Type SessionEventType; Time time.Time; Data json.RawMessage}`；常量 `SessionEventTurnStart/UserMessage/StepStart/AssistantMessage/ToolCall/ToolResult/StepEnd/TurnEnd`；`domain.ValidateSessionEventType(SessionEventType) error`；`domain.StepEndReason*` 与 `domain.TurnEndReason*` 常量。

- [ ] **Step 1: 写失败的测试**

`internal/domain/session_event_test.go`：

```go
package domain

import (
	"strings"
	"testing"
)

// 事件类型是**闭集**：未知类型必须被拒绝，并且错误里要指名是哪一个。
//
// 这条守的是 spec §4.3 不变量 4。它防的不是手滑，而是**版本漂移**：一个新版本
// 写进去的事件类型，被旧版本读到时如果被静默忽略，那条会话的历史就在旧版本眼里
// 少了一段——而少的那段恰好是新功能产生的。
func TestAnUnknownEventTypeIsRefusedByName(t *testing.T) {
	err := ValidateSessionEventType("tool/telepathy")
	if err == nil {
		t.Fatal("未知事件类型被接受了")
	}
	if !strings.Contains(err.Error(), "tool/telepathy") {
		t.Errorf("错误里没有指名那个类型：%v", err)
	}
}

func TestEveryKnownEventTypeIsAccepted(t *testing.T) {
	for _, typ := range []SessionEventType{
		SessionEventTurnStart, SessionEventUserMessage,
		SessionEventStepStart, SessionEventAssistantMessage,
		SessionEventToolCall, SessionEventToolResult,
		SessionEventStepEnd, SessionEventTurnEnd,
	} {
		if err := ValidateSessionEventType(typ); err != nil {
			t.Errorf("ValidateSessionEventType(%q) = %v, want nil", typ, err)
		}
	}
}

// 空类型单独一条：它是「字段忘了填」的形状，与「填了个没见过的」是两回事，
// 错误信息也该不同——否则排查的人会去找一个根本不存在的类型名。
func TestAnEmptyEventTypeSaysItIsEmpty(t *testing.T) {
	err := ValidateSessionEventType("")
	if err == nil {
		t.Fatal("空事件类型被接受了")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("错误没说它是空的：%v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/domain/ -run EventType -count=1`
Expected: FAIL，`undefined: ValidateSessionEventType`

（`-run` 匹配的是**测试函数名**：三个测试都含 `EventType`。写成 `-run SessionEvent` 会一条都不跑却打印 `ok`——「no tests to run」是绿得不是地方的经典形状。）

- [ ] **Step 3: 写实现**

`internal/domain/session_event.go`：

```go
package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// SessionEventType 是会话事件日志的类型闭集（见 spec §4.1）。
//
// 闭集而不是自由字符串：一个读不懂的类型意味着这条日志是另一个版本写的，
// 而把它当作「可以跳过的一行」会让重建出的历史悄悄缺一段。
type SessionEventType string

const (
	SessionEventTurnStart        SessionEventType = "turn/start"
	SessionEventUserMessage      SessionEventType = "user/message"
	SessionEventStepStart        SessionEventType = "step/start"
	SessionEventAssistantMessage SessionEventType = "assistant/message"
	SessionEventToolCall         SessionEventType = "tool/call"
	SessionEventToolResult       SessionEventType = "tool/result"
	SessionEventStepEnd          SessionEventType = "step/end"
	SessionEventTurnEnd          SessionEventType = "turn/end"
)

// knownSessionEventTypes 是上面那组常量的集合形式，供 ValidateSessionEventType 查。
var knownSessionEventTypes = map[SessionEventType]struct{}{
	SessionEventTurnStart:        {},
	SessionEventUserMessage:      {},
	SessionEventStepStart:        {},
	SessionEventAssistantMessage: {},
	SessionEventToolCall:         {},
	SessionEventToolResult:       {},
	SessionEventStepEnd:          {},
	SessionEventTurnEnd:          {},
}

// step/end 的 reason 闭集（spec §4.1）。
const (
	StepEndReasonCompleted = "completed"
	StepEndReasonFailed    = "failed"
	StepEndReasonCancelled = "cancelled"
	StepEndReasonMaxTokens = "max_tokens"
)

// turn/end 的 reason 闭集（spec §4.1）。interrupted 只由崩溃恢复补出，
// 正常路径不得使用它——它是「这段历史不是自己结束的」这个事实的唯一记号。
const (
	TurnEndReasonCompleted   = "completed"
	TurnEndReasonFailed      = "failed"
	TurnEndReasonCancelled   = "cancelled"
	TurnEndReasonInterrupted = "interrupted"
)

// SessionEvent 是会话事件日志里的一行。
//
// Data 保持 JSON 原文（json.RawMessage）而不是解成具体结构：这一层只负责
// 「按 seq 存取一串不可变的事件」，各事件载荷的形状归它们的生产者与消费者管。
// 存储层去解载荷，就等于每加一种事件都要改存储层。
type SessionEvent struct {
	// Seq 每会话单调、连续，从 0 起。
	Seq  int64            `json:"seq"`
	Type SessionEventType `json:"type"`
	Time time.Time        `json:"time"`
	Data json.RawMessage  `json:"data"`
}

// ValidateSessionEventType 判定一个事件类型是否属于这个构建认得的闭集。
//
// 空与未知分开报：空是「字段忘了填」，未知是「另一个版本写的」，排查方向不同。
func ValidateSessionEventType(typ SessionEventType) error {
	if typ == "" {
		return fmt.Errorf("session event type is empty")
	}
	if _, ok := knownSessionEventTypes[typ]; !ok {
		return fmt.Errorf("unknown session event type %q; this build does not understand it, "+
			"which usually means the log was written by a newer version", typ)
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/domain/ -run EventType -count=1 -v`
Expected: 三条全 PASS（确认输出里真的有三行 `--- PASS`，而不是 `no tests to run`）

- [ ] **Step 5: 变异验证**

把 `ValidateSessionEventType` 里的 `if _, ok := ...; !ok {` 分支整段删掉（变成永远返回 nil），跑：

Run: `go test ./internal/domain/ -run EventType -count=1`
Expected: `TestAnUnknownEventTypeIsRefusedByName` FAIL。**恢复代码**再往下走。

- [ ] **Step 6: 提交**

```bash
git add internal/domain/session_event.go internal/domain/session_event_test.go
git commit -m "feat(domain): 会话事件的类型闭集与取值约定"
```

---

### Task 2: 端口与建表

**Files:**
- Create: `internal/port/session_events.go`
- Modify: `internal/storage/sqlite.go`（`schemaStatements`，在 `runtime_events` 那条之后加一条）
- Test: `internal/storage/session_events_test.go`

**Interfaces:**
- Consumes: Task 1 的 `domain.SessionEvent`
- Produces: `port.SessionEventStore` 接口（`Append` / `ReadFrom` / `Load`）；`session_events` 表

- [ ] **Step 1: 写失败的测试**

`internal/storage/session_events_test.go`：

```go
package storage

import (
	"context"
	"path/filepath"
	"testing"
)

// 建表这一条不是形式主义：表名/主键写错的症状是「一切正常，直到两条事件撞了 seq」，
// 而那时错误信息指向的是 UNIQUE 约束，不是这次改动。
func TestTheSessionEventsTableExistsWithACompositeKey(t *testing.T) {
	ctx := context.Background()
	repo, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()

	var count int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='session_events'`,
	).Scan(&count); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if count != 1 {
		t.Fatalf("session_events 表不存在")
	}

	// 主键必须是 (session_id, seq) 两列：只按 seq 建键会让不同会话互相挤号。
	rows, err := repo.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('session_events') WHERE pk > 0 ORDER BY pk`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer rows.Close()
	var pk []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan pk column: %v", err)
		}
		pk = append(pk, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pk columns: %v", err)
	}
	if len(pk) != 2 || pk[0] != "session_id" || pk[1] != "seq" {
		t.Errorf("主键 = %v, want [session_id seq]", pk)
	}
}
```

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/storage/ -run SessionEventsTable -count=1`
Expected: FAIL，`session_events 表不存在`

- [ ] **Step 3: 建表 + 写端口**

在 `internal/storage/sqlite.go` 的 `schemaStatements` 里，紧跟 `runtime_events` 那条之后插入：

```go
	`CREATE TABLE IF NOT EXISTS session_events (
		session_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		time INTEGER NOT NULL,
		data TEXT NOT NULL,
		PRIMARY KEY (session_id, seq)
	)`,
```

新建 `internal/port/session_events.go`：

```go
package port

import (
	"context"

	"github.com/stardust/legion-agent/internal/domain"
)

// SessionEventStore 是会话事件日志的持久化契约（spec §4.4）。
//
// 三个方法的分工是有意的，不要合并：
//
//   - Append 只追加，且**整批一个事务**——半批写入会留下一个谁也修不好的日志。
//   - ReadFrom **不改库**，只读 seq >= fromSeq 的后缀。轨迹的翻页与增量拉取走它，
//     因为一次「看一眼」不该改变被看的东西。
//   - Load **会改库**：它把崩溃留下的半个 turn 补成合法的 provider transcript
//     （见 spec §4.3 不变量 2）。会话要被重新使用时才调它。
type SessionEventStore interface {
	// Append 追加一批事件。首个事件的 Seq 必须等于该会话当前的 next-seq，
	// 否则拒绝并指出期望值与实际值。
	Append(ctx context.Context, sessionID string, events []domain.SessionEvent) error

	// ReadFrom 返回 seq >= fromSeq 的事件，按 seq 升序。fromSeq 越过末尾返回空切片。
	ReadFrom(ctx context.Context, sessionID string, fromSeq int64) ([]domain.SessionEvent, error)

	// Load 返回该会话的全部事件，必要时先补出崩溃恢复的关闭事件并落盘。
	Load(ctx context.Context, sessionID string) ([]domain.SessionEvent, error)
}
```

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/storage/ -run SessionEventsTable -count=1 -v`
Expected: PASS

- [ ] **Step 5: 变异验证**

把建表语句里的 `PRIMARY KEY (session_id, seq)` 改成 `PRIMARY KEY (seq)`，跑同一条测试：
Expected: FAIL（主键列断言）。**恢复**再往下。

- [ ] **Step 6: 提交**

```bash
git add internal/port/session_events.go internal/storage/sqlite.go internal/storage/session_events_test.go
git commit -m "feat(storage): session_events 表与 SessionEventStore 端口"
```

---

### Task 3: Append —— 连续 seq、整批事务、懒物化

**Files:**
- Create: `internal/storage/session_events.go`
- Modify: `internal/storage/session_events_test.go`（追加测试）

**Interfaces:**
- Consumes: Task 1 的 `domain.SessionEvent`、Task 2 的表与接口
- Produces: `func (r *SQLiteRepository) Append(ctx context.Context, sessionID string, events []domain.SessionEvent) error`；未导出的 `func nextSeqTx(ctx context.Context, tx *sql.Tx, sessionID string) (int64, error)`（自由函数，不挂在仓储上——它只需要那个事务）

- [ ] **Step 1: 写失败的测试**

追加到 `internal/storage/session_events_test.go`：

```go
// 首个 seq 必须等于 next-seq（spec §4.3 不变量 1）。
//
// 这条挡的是「两个写入方各按各的计数往里写」：一旦 seq 出现重叠或跳号，
// 日志就不再能重建出唯一的历史，而错误会在很久之后以「读的时候少一段」出现。
func TestAppendRefusesASeqThatDoesNotContinueTheLog(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	if err := repo.Append(ctx, "s1", []domain.SessionEvent{ev(0, domain.SessionEventTurnStart)}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// 库里的 next-seq 现在是 1；递交一个从 2 开始的批次必须被拒。
	err := repo.Append(ctx, "s1", []domain.SessionEvent{ev(2, domain.SessionEventStepStart)})
	if err == nil {
		t.Fatal("跳号的批次被接受了")
	}
	if !strings.Contains(err.Error(), "2") || !strings.Contains(err.Error(), "1") {
		t.Errorf("错误没同时给出实际与期望的 seq：%v", err)
	}
}

// 批中途失败要整批回滚（spec §4.3 不变量 1）。
//
// 半批写入留下的是一个「seq 连续但内容缺了后半段」的日志——它读得出来、也验得过，
// 却与真实发生的事不符。那种损坏比读不出来更难发现。
func TestAFailedBatchLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	// 第二条与第一条撞 seq：INSERT 到它时违反主键，整批必须回滚。
	batch := []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(0, domain.SessionEventUserMessage),
	}
	if err := repo.Append(ctx, "s1", batch); err == nil {
		t.Fatal("批内重复 seq 被接受了")
	}

	got, err := repo.ReadFrom(ctx, "s1", 0)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("回滚之后还剩 %d 条事件：半批写入留下的日志读得出来却与真实发生的事不符", len(got))
	}
}

// 懒物化（spec §4.3 不变量 5）：没有事件的会话不在事件表里留任何痕迹。
func TestASessionWithNoEventsLeavesNoTrace(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	if err := repo.Append(ctx, "s1", nil); err != nil {
		t.Fatalf("Append(nil) = %v, want nil：空批次是合法的无操作", err)
	}
	var count int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_events`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Errorf("空批次写进了 %d 行", count)
	}
}

// 未知事件类型在写入时就被拒（不变量 4）：让它进库，读的那一方就只剩两个坏选择
// ——静默跳过（历史缺一段）或整条会话读不出来。
func TestAppendRefusesAnUnknownEventType(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	bad := domain.SessionEvent{Seq: 0, Type: "tool/telepathy", Time: time.UnixMilli(1), Data: []byte(`{}`)}
	err := repo.Append(ctx, "s1", []domain.SessionEvent{bad})
	if err == nil {
		t.Fatal("未知事件类型被写进库了")
	}
	if !strings.Contains(err.Error(), "tool/telepathy") {
		t.Errorf("错误没指名类型：%v", err)
	}
}

// 大载荷不进事件（不变量 6）：事件表的增长必须与**调用次数**成正比，
// 而不与工具输出体积成正比。超限的输出走 spill，事件里只留预览 + 定位符。
//
// 这条守在存储层，是为了让 P2 接发射点的人在**写错的当场**就看见，
// 而不是在库涨到几个 G 之后。
func TestAnOversizedPayloadIsRefused(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	huge := make([]byte, maxSessionEventDataBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}
	payload, err := json.Marshal(map[string]string{"preview": string(huge)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	err = repo.Append(ctx, "s1", []domain.SessionEvent{{
		Seq: 0, Type: domain.SessionEventToolResult, Time: time.UnixMilli(1), Data: payload,
	}})
	if err == nil {
		t.Fatal("超限载荷被写进事件了：那会让事件表随工具输出体积膨胀")
	}
	if !strings.Contains(err.Error(), "spill") {
		t.Errorf("错误没告诉写的人该走 spill：%v", err)
	}
}
```

在同一文件顶部加测试辅助（与既有 `adapters_test.go` 的建库方式一致）：

```go
func newEventRepo(t *testing.T) *SQLiteRepository {
	t.Helper()
	repo, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// ev 造一个最小的合法事件；载荷内容与这些测试无关。
func ev(seq int64, typ domain.SessionEventType) domain.SessionEvent {
	return domain.SessionEvent{
		Seq:  seq,
		Type: typ,
		Time: time.UnixMilli(1_700_000_000_000 + seq),
		Data: []byte(`{}`),
	}
}
```

并把 import 补齐：`encoding/json`、`strings`、`time`、`github.com/stardust/legion-agent/internal/domain`。

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/storage/ -run "Append|FailedBatch|NoEventsLeavesNoTrace|OversizedPayload" -count=1`
Expected: FAIL，`repo.Append undefined`

- [ ] **Step 3: 写实现**

新建 `internal/storage/session_events.go`：

```go
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/stardust/legion-agent/internal/domain"
)

// maxSessionEventDataBytes 是单个事件载荷的上限。
//
// 它守的是 spec §4.3 不变量 6：事件表的增长与**调用次数**成正比，不与工具输出
// 体积成正比。超限的输出走 spill（internal/runtime/toolcache.go），事件里只留
// 预览与定位符。把这条守在存储层，是为了让写错的人在当场看见，而不是在库涨到
// 几个 G 之后才发现。
//
// 64 KiB 的取法：一条预览按现有截断治理是几 KiB 量级，assistant 消息含 tool_calls
// 时可能到几十 KiB。64 KiB 给了足够余量，同时离「一份完整工具输出」还差一个量级。
const maxSessionEventDataBytes = 64 << 10

// sessionWriteLocks 串行化同一会话的写入。
//
// 同会话的并发写是常态（并行工具返回、审批恢复与新消息相撞），而 seq 的连续性
// 是一个「读-改-写」不变量：两个写入方同时读到同一个 next-seq，就会一个成功、
// 一个撞主键失败。锁按会话切分，不同会话互不阻塞。
type sessionWriteLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (s *sessionWriteLocks) get(sessionID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = make(map[string]*sync.Mutex)
	}
	lock, ok := s.locks[sessionID]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[sessionID] = lock
	}
	return lock
}

// Append 追加一批事件（spec §4.3 不变量 1、4、5、6）。
//
// 整批在一个事务里：中途失败整批回滚，不留半批——半批写入留下的日志读得出来、
// 也验得过，却与真实发生的事不符，那种损坏比读不出来更难发现。
//
// 空批次是合法的无操作（懒物化：没有事件的会话不在表里留痕）。
func (r *SQLiteRepository) Append(ctx context.Context, sessionID string, events []domain.SessionEvent) error {
	if len(events) == 0 {
		return nil
	}
	for _, event := range events {
		if err := domain.ValidateSessionEventType(event.Type); err != nil {
			return fmt.Errorf("append session events for %q: %w", sessionID, err)
		}
		if len(event.Data) > maxSessionEventDataBytes {
			return fmt.Errorf("append session events for %q: event %d (%s) carries %d bytes, "+
				"over the %d-byte limit; large tool output belongs in spill with only a preview "+
				"and locator in the event",
				sessionID, event.Seq, event.Type, len(event.Data), maxSessionEventDataBytes)
		}
	}

	lock := r.sessionEventLocks.get(sessionID)
	lock.Lock()
	defer lock.Unlock()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin append session events for %q: %w", sessionID, err)
	}
	defer tx.Rollback()

	next, err := nextSeqTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	if events[0].Seq != next {
		return fmt.Errorf("append session events for %q: first seq is %d but the log continues at %d; "+
			"the log is append-only and its seq must stay contiguous",
			sessionID, events[0].Seq, next)
	}

	for _, event := range events {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO session_events (session_id, seq, type, time, data)
			VALUES (?, ?, ?, ?, ?)
		`, sessionID, event.Seq, string(event.Type), event.Time.UnixMilli(), string(event.Data)); err != nil {
			return fmt.Errorf("append session event %d (%s) for %q: %w", event.Seq, event.Type, sessionID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session events for %q: %w", sessionID, err)
	}
	return nil
}

// nextSeqTx 在事务内读该会话的下一个 seq。
//
// 权威值来自库而不是内存游标：少一个可能与库不一致的状态。主键覆盖这次查询，
// 代价可忽略。
func nextSeqTx(ctx context.Context, tx *sql.Tx, sessionID string) (int64, error) {
	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq)+1, 0) FROM session_events WHERE session_id = ?`, sessionID,
	).Scan(&next); err != nil {
		return 0, fmt.Errorf("read next seq for %q: %w", sessionID, err)
	}
	return next, nil
}

// decodeSessionEvent 把一行还原成事件。类型在这里再验一次：读到未知类型说明这条
// 日志是另一个版本写的，静默跳过会让重建出的历史缺一段（spec §4.3 不变量 4）。
func decodeSessionEvent(sessionID string, seq int64, typ string, millis int64, data string) (domain.SessionEvent, error) {
	eventType := domain.SessionEventType(typ)
	if err := domain.ValidateSessionEventType(eventType); err != nil {
		return domain.SessionEvent{}, fmt.Errorf("read session event %d for %q: %w", seq, sessionID, err)
	}
	if !json.Valid([]byte(data)) {
		return domain.SessionEvent{}, fmt.Errorf("read session event %d (%s) for %q: data is not valid JSON",
			seq, typ, sessionID)
	}
	return domain.SessionEvent{
		Seq:  seq,
		Type: eventType,
		Time: time.UnixMilli(millis),
		Data: json.RawMessage(data),
	}, nil
}
```

在 `internal/storage/sqlite.go` 的 `SQLiteRepository` 结构体里加一个字段：

```go
	// sessionEventLocks 串行化同一会话的事件追加（见 session_events.go）。
	sessionEventLocks sessionWriteLocks
```

并在 `session_events.go` 的 import 里补 `"time"`。

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/storage/ -run "Append|NoEventsLeavesNoTrace|OversizedPayload|SessionEventsTable" -count=1 -v`
Expected: 全 PASS。注意 `TestAFailedBatchLeavesNothingBehind` 用到 Task 4 的 `ReadFrom`，本任务里整个包会编译不过——**按计划先把那一条测试写进文件但用 `t.Skip("waits for ReadFrom in Task 4")` 占位**，Task 4 实现 `ReadFrom` 后删掉那行 Skip 并真正跑它。本步跑：`go test ./internal/storage/ -run "RefusesASeq|NoEventsLeavesNoTrace|OversizedPayload|UnknownEventType|SessionEventsTable" -count=1 -v`

- [ ] **Step 5: 变异验证（三条）**

1. 删掉连续性检查（`if events[0].Seq != next { ... }` 整段）→ `TestAppendRefusesASeqThatDoesNotContinueTheLog` FAIL。
2. 把 `defer tx.Rollback()` 删掉并把每条 INSERT 的错误改成 `continue` → `TestAFailedBatchLeavesNothingBehind` FAIL（**Task 4 之后再验这条**）。
3. 删掉载荷上限那段 → `TestAnOversizedPayloadIsRefused` FAIL。

每条验完**恢复代码**。

- [ ] **Step 6: 提交**

```bash
git add internal/storage/session_events.go internal/storage/session_events_test.go internal/storage/sqlite.go
git commit -m "feat(storage): 事件追加——连续 seq、整批事务、载荷上限"
```

---

### Task 4: ReadFrom —— 只读后缀，不改库

**Files:**
- Modify: `internal/storage/session_events.go`
- Modify: `internal/storage/session_events_test.go`

**Interfaces:**
- Consumes: Task 3 的 `Append`、`decodeSessionEvent`
- Produces: `func (r *SQLiteRepository) ReadFrom(ctx context.Context, sessionID string, fromSeq int64) ([]domain.SessionEvent, error)`

- [ ] **Step 1: 写失败的测试**

```go
func TestReadFromReturnsOnlyTheSuffix(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(1, domain.SessionEventUserMessage),
		ev(2, domain.SessionEventStepStart),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := repo.ReadFrom(ctx, "s1", 1)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("ReadFrom(1) 返回 %d 条（seq %v），want seq [1 2]", len(got), seqsOf(got))
	}
}

// 越过末尾返回空，不是错误：轨迹的增量拉取会不断问「有没有比我这条更新的」，
// 「暂时没有」是正常答案。
func TestReadFromPastTheEndIsEmptyNotAnError(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{ev(0, domain.SessionEventTurnStart)}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := repo.ReadFrom(ctx, "s1", 99)
	if err != nil {
		t.Fatalf("ReadFrom(99) = %v, want nil error", err)
	}
	if len(got) != 0 {
		t.Errorf("ReadFrom(99) 返回了 %d 条", len(got))
	}
}

// ReadFrom 不改库：一次「看一眼」不该改变被看的东西。轨迹在翻页，
// 而 Load 的崩溃恢复会写入——两者混在一起，翻页就会静默地改写历史。
func TestReadFromDoesNotWriteAnything(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	// 半个 turn：有 tool/call 没有 tool/result。Load 会为它补事件，ReadFrom 不该。
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(1, domain.SessionEventStepStart),
		toolCall(2, "call-1"),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	before := countEvents(t, repo, "s1")
	if _, err := repo.ReadFrom(ctx, "s1", 0); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if after := countEvents(t, repo, "s1"); after != before {
		t.Errorf("ReadFrom 之后行数从 %d 变成 %d：它不该写任何东西", before, after)
	}
}

// 中间断裂 = 损坏，拒绝（spec §4.3 不变量 3）。
//
// 静默跳过一个洞，等于把「这段历史缺了一块」变成「这段历史就是这样」——
// 而缺掉的恰好可能是那次出问题的工具调用。
func TestAHoleInTheMiddleIsRefused(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(1, domain.SessionEventStepStart),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 绕过 Append 直接制造一个洞（seq 1 被删掉）。真实成因是行损坏或人工干预。
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM session_events WHERE session_id='s1' AND seq=1`); err != nil {
		t.Fatalf("制造断裂: %v", err)
	}

	_, err := repo.ReadFrom(ctx, "s1", 0)
	if err == nil {
		t.Fatal("seq 有洞却读成功了")
	}
	if !strings.Contains(err.Error(), "1") {
		t.Errorf("错误没指出断在哪里：%v", err)
	}
}
```

辅助函数（追加到测试文件）：

```go
func seqsOf(events []domain.SessionEvent) []int64 {
	out := make([]int64, 0, len(events))
	for _, e := range events {
		out = append(out, e.Seq)
	}
	return out
}

func countEvents(t *testing.T, repo *SQLiteRepository, sessionID string) int {
	t.Helper()
	var count int
	if err := repo.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM session_events WHERE session_id = ?`, sessionID).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return count
}

// toolCall 造一条带 call_id 的 tool/call 事件。
func toolCall(seq int64, callID string) domain.SessionEvent {
	data, err := json.Marshal(map[string]any{"turn": 0, "step": 0, "call_id": callID, "name": "read_file", "arguments": "{}"})
	if err != nil {
		panic(err)
	}
	return domain.SessionEvent{Seq: seq, Type: domain.SessionEventToolCall, Time: time.UnixMilli(1), Data: data}
}
```

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/storage/ -run "ReadFrom|HoleInTheMiddle" -count=1`
Expected: FAIL，`repo.ReadFrom undefined`

- [ ] **Step 3: 写实现**

追加到 `internal/storage/session_events.go`：

```go
// ReadFrom 返回 seq >= fromSeq 的事件，按 seq 升序（spec §4.4）。
//
// **不改库**：轨迹的翻页与增量拉取走这条路，而一次「看一眼」不该改变被看的东西。
// 崩溃恢复只发生在 Load 里。
//
// 返回的这段必须自身连续：中间有洞说明日志损坏，报错而不是把缺口当成「本来就这样」
// （spec §4.3 不变量 3）。
func (r *SQLiteRepository) ReadFrom(ctx context.Context, sessionID string, fromSeq int64) ([]domain.SessionEvent, error) {
	if fromSeq < 0 {
		return nil, fmt.Errorf("read session events for %q: fromSeq %d is negative", sessionID, fromSeq)
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT seq, type, time, data
		FROM session_events
		WHERE session_id = ? AND seq >= ?
		ORDER BY seq
	`, sessionID, fromSeq)
	if err != nil {
		return nil, fmt.Errorf("read session events for %q: %w", sessionID, err)
	}
	defer rows.Close()

	var events []domain.SessionEvent
	var expected int64 = -1
	for rows.Next() {
		var (
			seq    int64
			typ    string
			millis int64
			data   string
		)
		if err := rows.Scan(&seq, &typ, &millis, &data); err != nil {
			return nil, fmt.Errorf("scan session event for %q: %w", sessionID, err)
		}
		if expected >= 0 && seq != expected {
			return nil, fmt.Errorf("session log for %q is broken: seq jumps from %d to %d; "+
				"a gap means the log no longer reconstructs one history", sessionID, expected-1, seq)
		}
		event, err := decodeSessionEvent(sessionID, seq, typ, millis, data)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
		expected = seq + 1
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session events for %q: %w", sessionID, err)
	}
	return events, nil
}
```

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/storage/ -run "ReadFrom|HoleInTheMiddle|Append|FailedBatch|NoEventsLeavesNoTrace|OversizedPayload" -count=1 -v`
Expected: 全 PASS。**先删掉 Task 3 里给 `TestAFailedBatchLeavesNothingBehind` 加的那行 `t.Skip`**，否则它会以 SKIP 混过去——一条永远 skip 的测试等于没有测试。

- [ ] **Step 5: 变异验证（两条）**

1. 删掉断裂检查（`if expected >= 0 && seq != expected { ... }`）→ `TestAHoleInTheMiddleIsRefused` FAIL。
2. 补做 Task 3 的变异 2（去掉事务回滚）→ `TestAFailedBatchLeavesNothingBehind` FAIL。

验完恢复。

- [ ] **Step 6: 提交**

```bash
git add internal/storage/session_events.go internal/storage/session_events_test.go
git commit -m "feat(storage): ReadFrom 只读后缀，断裂即报损坏"
```

---

### Task 5: Load —— 崩溃恢复补出合法 transcript

**Files:**
- Modify: `internal/storage/session_events.go`
- Modify: `internal/storage/session_events_test.go`

**Interfaces:**
- Consumes: Task 3 的 `Append`/`nextSeqTx`、Task 4 的 `ReadFrom`
- Produces: `func (r *SQLiteRepository) Load(ctx context.Context, sessionID string) ([]domain.SessionEvent, error)`；未导出的 `planRecovery([]domain.SessionEvent) (recoveryPlan, error)`、`synthesizeClosers(recoveryPlan, int64, time.Time) ([]domain.SessionEvent, error)`、`intField/stringField(json.RawMessage, string) (T, error)`

**这是整个 P1 最要紧的一条**：它是唯一「平时看不出、出事才致命」的部分。日志里留下一个「有调用没结果」的半个 turn，下一次请求就会把一个非法的消息数组发给模型。

- [ ] **Step 1: 写失败的测试**

```go
// 崩溃恢复：半个 turn 要被补成合法的 provider transcript（spec §4.3 不变量 2）。
//
// 判据不是「补了几条」，而是**每个 tool/call 都有与之 call_id 对应的 tool/result**，
// 且 turn 以 interrupted 收尾。少了任何一条，重建出的消息数组发给模型就是非法的。
func TestLoadClosesAnInterruptedTurnIntoAValidTranscript(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	// 崩在两个工具调用之间：call-1 有结果，call-2 没有。
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(1, domain.SessionEventStepStart),
		toolCall(2, "call-1"),
		toolResult(3, "call-1"),
		toolCall(4, "call-2"),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	events, err := repo.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	answered := map[string]bool{}
	for _, e := range events {
		switch e.Type {
		case domain.SessionEventToolCall:
			// 记名但不覆盖已有的 true：自赋值（answered[x] = answered[x]）会被
			// go vet 判为错误，而这个仓要求 vet 全绿。
			if _, seen := answered[callIDOf(t, e)]; !seen {
				answered[callIDOf(t, e)] = false
			}
		case domain.SessionEventToolResult:
			answered[callIDOf(t, e)] = true
		}
	}
	for callID, ok := range answered {
		if !ok {
			t.Errorf("call %q 没有对应的 tool/result：这样重建出的消息数组发给模型是非法的", callID)
		}
	}

	last := events[len(events)-1]
	if last.Type != domain.SessionEventTurnEnd {
		t.Fatalf("最后一条是 %s，want turn/end", last.Type)
	}
	if reason := reasonOf(t, last); reason != domain.TurnEndReasonInterrupted {
		t.Errorf("turn/end 的 reason = %q, want %q——这是「这段历史不是自己结束的」的唯一记号",
			reason, domain.TurnEndReasonInterrupted)
	}
}

// 恢复要落盘：下一次 Append 从补齐后的 seq continue，而不是又撞回半个 turn。
func TestLoadPersistsTheRecoveryItSynthesized(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(1, domain.SessionEventStepStart),
		toolCall(2, "call-1"),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	recovered, err := repo.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 再读一次：补出来的事件必须在库里，而不只是在返回值里。
	again, err := repo.ReadFrom(ctx, "s1", 0)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if len(again) != len(recovered) {
		t.Fatalf("库里有 %d 条，Load 返回了 %d 条：恢复没有落盘，下次打开还会再补一次",
			len(again), len(recovered))
	}
	// 且下一批能接着写。
	next := int64(len(again))
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{ev(next, domain.SessionEventTurnStart)}); err != nil {
		t.Errorf("恢复之后接着写失败：%v", err)
	}
}

// 已经收尾的日志不该被动：Load 是幂等的，读两次不会越补越长。
func TestLoadLeavesABalancedLogAlone(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)
	if err := repo.Append(ctx, "s1", []domain.SessionEvent{
		ev(0, domain.SessionEventTurnStart),
		ev(1, domain.SessionEventStepStart),
		toolCall(2, "call-1"),
		toolResult(3, "call-1"),
		stepEnd(4, domain.StepEndReasonCompleted),
		turnEnd(5, domain.TurnEndReasonCompleted),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	first, err := repo.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	second, err := repo.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if len(first) != 6 || len(second) != 6 {
		t.Errorf("Load 改了一个本来就平衡的日志：%d then %d, want 6", len(first), len(second))
	}
}
```

补充辅助函数：

```go
func toolResult(seq int64, callID string) domain.SessionEvent {
	data, err := json.Marshal(map[string]any{
		"turn": 0, "step": 0, "call_id": callID, "preview": "ok", "is_error": false, "duration_ms": 1,
	})
	if err != nil {
		panic(err)
	}
	return domain.SessionEvent{Seq: seq, Type: domain.SessionEventToolResult, Time: time.UnixMilli(1), Data: data}
}

func stepEnd(seq int64, reason string) domain.SessionEvent {
	data, err := json.Marshal(map[string]any{"turn": 0, "step": 0, "reason": reason})
	if err != nil {
		panic(err)
	}
	return domain.SessionEvent{Seq: seq, Type: domain.SessionEventStepEnd, Time: time.UnixMilli(1), Data: data}
}

func turnEnd(seq int64, reason string) domain.SessionEvent {
	data, err := json.Marshal(map[string]any{"turn": 0, "reason": reason})
	if err != nil {
		panic(err)
	}
	return domain.SessionEvent{Seq: seq, Type: domain.SessionEventTurnEnd, Time: time.UnixMilli(1), Data: data}
}

func callIDOf(t *testing.T, e domain.SessionEvent) string {
	t.Helper()
	var payload struct {
		CallID string `json:"call_id"`
	}
	if err := json.Unmarshal(e.Data, &payload); err != nil {
		t.Fatalf("unmarshal call_id from %s: %v", e.Type, err)
	}
	return payload.CallID
}

func reasonOf(t *testing.T, e domain.SessionEvent) string {
	t.Helper()
	var payload struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(e.Data, &payload); err != nil {
		t.Fatalf("unmarshal reason from %s: %v", e.Type, err)
	}
	return payload.Reason
}
```

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/storage/ -run Load -count=1`
Expected: FAIL，`repo.Load undefined`

- [ ] **Step 3: 写实现**

追加到 `internal/storage/session_events.go`：

```go
// recoveryPlan 是一个未收尾日志需要补上的事件（尚未分配 seq）。
type recoveryPlan struct {
	unansweredCalls []unansweredCall
	needStepEnd     bool
	stepTurn        int
	stepStep        int
	needTurnEnd     bool
	turnNumber      int
}

// unansweredCall 是一个发出去、却没有结果的工具调用。
type unansweredCall struct {
	callID string
	turn   int
	step   int
}

// Load 返回该会话的全部事件，必要时先把崩溃留下的半个 turn 补成合法的
// provider transcript 并落盘（spec §4.3 不变量 2）。
//
// **为什么补而不是截断**：截掉半个 turn 会丢掉真实发生过的事——那些工具是真的
// 执行了、副作用是真的产生了。补的做法保留它们，只是把「没等到结果」这件事
// 记成一条 is_error 的结果，让重建出的消息数组仍然合法：每个 tool_call 都有
// 与之对应的 tool 消息。
//
// 幂等：已经平衡的日志原样返回，不追加任何东西。
func (r *SQLiteRepository) Load(ctx context.Context, sessionID string) ([]domain.SessionEvent, error) {
	events, err := r.ReadFrom(ctx, sessionID, 0)
	if err != nil {
		return nil, err
	}
	plan, err := planRecovery(events)
	if err != nil {
		return nil, fmt.Errorf("recover session %q: %w", sessionID, err)
	}
	if len(plan.unansweredCalls) == 0 && !plan.needStepEnd && !plan.needTurnEnd {
		return events, nil
	}

	synthesized, err := synthesizeClosers(plan, int64(len(events)), lastTimeOf(events))
	if err != nil {
		return nil, fmt.Errorf("recover session %q: %w", sessionID, err)
	}
	if err := r.Append(ctx, sessionID, synthesized); err != nil {
		return nil, fmt.Errorf("persist recovery for session %q: %w", sessionID, err)
	}
	return append(events, synthesized...), nil
}

// planRecovery 判断一个日志缺什么收尾事件。
//
// 判据只看事件本身：有 turn/start 而没有对应的 turn/end 就是没收尾；
// tool/call 没有同 call_id 的 tool/result 就是没答。
func planRecovery(events []domain.SessionEvent) (recoveryPlan, error) {
	var plan recoveryPlan
	pending := map[string]unansweredCall{}
	var order []string
	turnOpen, stepOpen := false, false

	for _, event := range events {
		switch event.Type {
		case domain.SessionEventTurnStart:
			turnOpen = true
			turn, err := intField(event.Data, "turn")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			plan.turnNumber = turn
		case domain.SessionEventTurnEnd:
			turnOpen = false
		case domain.SessionEventStepStart:
			stepOpen = true
			turn, err := intField(event.Data, "turn")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			step, err := intField(event.Data, "step")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			plan.stepTurn, plan.stepStep = turn, step
		case domain.SessionEventStepEnd:
			stepOpen = false
		case domain.SessionEventToolCall:
			id, err := stringField(event.Data, "call_id")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			turn, err := intField(event.Data, "turn")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			step, err := intField(event.Data, "step")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			pending[id] = unansweredCall{callID: id, turn: turn, step: step}
			order = append(order, id)
		case domain.SessionEventToolResult:
			id, err := stringField(event.Data, "call_id")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			delete(pending, id)
		}
	}
	for _, id := range order {
		if call, ok := pending[id]; ok {
			plan.unansweredCalls = append(plan.unansweredCalls, call)
		}
	}
	plan.needStepEnd = stepOpen
	plan.needTurnEnd = turnOpen
	return plan, nil
}

// synthesizeClosers 按 spec §4.3 不变量 2 的顺序造出补齐事件：
// 每个未答调用一条 is_error 的 tool/result，然后 step/end，最后 turn/end{interrupted}。
func synthesizeClosers(plan recoveryPlan, nextSeq int64, at time.Time) ([]domain.SessionEvent, error) {
	var out []domain.SessionEvent
	seq := nextSeq
	for _, call := range plan.unansweredCalls {
		data, err := json.Marshal(map[string]any{
			"turn": call.turn, "step": call.step, "call_id": call.callID,
			"preview":     "the agent stopped before this tool returned; its result was never recorded",
			"is_error":    true,
			"duration_ms": 0,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal synthetic tool result for %q: %w", call.callID, err)
		}
		out = append(out, domain.SessionEvent{
			Seq: seq, Type: domain.SessionEventToolResult, Time: at, Data: data,
		})
		seq++
	}
	if plan.needStepEnd {
		data, err := json.Marshal(map[string]any{
			"turn": plan.stepTurn, "step": plan.stepStep, "reason": domain.StepEndReasonCancelled,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal synthetic step end: %w", err)
		}
		out = append(out, domain.SessionEvent{Seq: seq, Type: domain.SessionEventStepEnd, Time: at, Data: data})
		seq++
	}
	if plan.needTurnEnd {
		data, err := json.Marshal(map[string]any{
			"turn": plan.turnNumber, "reason": domain.TurnEndReasonInterrupted,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal synthetic turn end: %w", err)
		}
		out = append(out, domain.SessionEvent{Seq: seq, Type: domain.SessionEventTurnEnd, Time: at, Data: data})
	}
	return out, nil
}

// lastTimeOf 用最后一条真实事件的时间给补出来的事件打时间戳：它们描述的是
// 那一刻发生的中断，而不是「现在」。空日志不会走到恢复路径。
func lastTimeOf(events []domain.SessionEvent) time.Time {
	if len(events) == 0 {
		return time.UnixMilli(0)
	}
	return events[len(events)-1].Time
}

// intField / stringField 从载荷里取恢复所需的字段。
//
// **不吞错**（CLAUDE.md §0）：spec §4.1 规定这些字段必填，取不到说明这条日志
// 本身有问题。返回零值接着走，等于让恢复出的事件带着编造的 turn/step/call_id
// ——那正是「凑个值接着跑」。所以两者都返回 error，由 planRecovery 一路上报。
func intField(data json.RawMessage, name string) (int, error) {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, fmt.Errorf("decode event payload for field %q: %w", name, err)
	}
	value, ok := payload[name].(float64)
	if !ok {
		return 0, fmt.Errorf("event payload has no numeric %q field", name)
	}
	return int(value), nil
}

func stringField(data json.RawMessage, name string) (string, error) {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("decode event payload for field %q: %w", name, err)
	}
	value, ok := payload[name].(string)
	if !ok {
		return "", fmt.Errorf("event payload has no string %q field", name)
	}
	return value, nil
}
```

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/storage/ -run Load -count=1 -v`
Expected: 三条全 PASS

- [ ] **Step 5: 变异验证（三条）**

1. 在 `synthesizeClosers` 里跳过未答调用那段（把 `for _, call := range plan.unansweredCalls` 循环体注释掉）→ `TestLoadClosesAnInterruptedTurnIntoAValidTranscript` FAIL。
2. 把 `Load` 里的 `r.Append(ctx, sessionID, synthesized)` 删掉，直接返回拼好的切片 → `TestLoadPersistsTheRecoveryItSynthesized` FAIL。
3. 把 `planRecovery` 里 `case domain.SessionEventTurnEnd: turnOpen = false` 删掉（让它以为永远没收尾）→ `TestLoadLeavesABalancedLogAlone` FAIL。

每条验完恢复。

- [ ] **Step 6: 提交**

```bash
git add internal/storage/session_events.go internal/storage/session_events_test.go
git commit -m "feat(storage): 崩溃恢复把半个 turn 补成合法 transcript"
```

---

### Task 6: 并发写的 seq 连续性

**Files:**
- Modify: `internal/storage/session_events_test.go`

**Interfaces:**
- Consumes: Task 3 的 `Append`（含 `sessionWriteLocks`）
- Produces: 无新接口——本任务只补上守护 Task 3 那把锁的测试

- [ ] **Step 1: 写失败的测试**

```go
// 同会话并发写：seq 必须仍然连续、无重复（spec §4.4）。
//
// 这条守的是那把 per-session 锁。没有它，两个写入方会读到同一个 next-seq——
// 一个成功、一个撞主键失败，而失败的那次带走的是真实发生过的一段历史。
//
// 每个 goroutine 自己重试：调用方本来就不知道别人在写，它拿到「日志已经走到
// 第 N 条」的错误后重读再写才是正常用法。这里要断言的是**结果**：最终的日志
// 连续、条数正确、无重复。
func TestConcurrentAppendsKeepTheLogContiguous(t *testing.T) {
	ctx := context.Background()
	repo := newEventRepo(t)

	const writers = 8
	const perWriter = 5
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				for {
					existing, err := repo.ReadFrom(ctx, "s1", 0)
					if err != nil {
						t.Errorf("ReadFrom: %v", err)
						return
					}
					err = repo.Append(ctx, "s1", []domain.SessionEvent{
						ev(int64(len(existing)), domain.SessionEventStepStart),
					})
					if err == nil {
						break
					}
					if !strings.Contains(err.Error(), "the log continues at") {
						t.Errorf("并发写失败的原因不是抢号：%v", err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	events, err := repo.ReadFrom(ctx, "s1", 0)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if len(events) != writers*perWriter {
		t.Fatalf("最终 %d 条，want %d：有写入被静默丢掉了", len(events), writers*perWriter)
	}
	for i, e := range events {
		if e.Seq != int64(i) {
			t.Fatalf("第 %d 条的 seq 是 %d：日志不连续", i, e.Seq)
		}
	}
}
```

补 import：`sync`。

- [ ] **Step 2: 跑测试确认它绿（这条是守护性测试，实现已在 Task 3）**

Run: `go test ./internal/storage/ -run ConcurrentAppends -count=1 -race`
Expected: PASS

- [ ] **Step 3: 变异验证——去掉那把锁**

把 `Append` 里这两行删掉：

```go
	lock := r.sessionEventLocks.get(sessionID)
	lock.Lock()
	defer lock.Unlock()
```

Run: `go test ./internal/storage/ -run ConcurrentAppends -count=5 -race`
Expected: FAIL 或 race 报告。**注意**：SQLite 的写锁可能让它偶尔仍然通过，所以用 `-count=5`；若五次都绿，把 `writers` 提到 16 再试，并把这一事实记进提交说明——那意味着这条测试对这个变异不敏感，需要换一种造竞态的方式（例如在 `nextSeqTx` 之后插入一次 `runtime.Gosched()`）。

验完恢复代码。

- [ ] **Step 4: 提交**

```bash
git add internal/storage/session_events_test.go
git commit -m "test(storage): 并发写下 seq 仍然连续"
```

---

### Task 7: 收口——接口一致性与全量校验

**Files:**
- Modify: `internal/storage/session_events.go`（加编译期断言）
- Test: 全量

**Interfaces:**
- Consumes: 前六个任务的全部产物
- Produces: `var _ port.SessionEventStore = (*SQLiteRepository)(nil)` 编译期保证

- [ ] **Step 1: 加编译期断言**

在 `internal/storage/session_events.go` 顶部（import 之后）加：

```go
// 编译期保证 SQLiteRepository 满足端口契约。
//
// 这一行的作用是让「方法签名改了但端口没改」在**编译时**就停下来，而不是等到
// 装配时才发现某个实现悄悄不再满足接口。
var _ port.SessionEventStore = (*SQLiteRepository)(nil)
```

并在 import 里加 `"github.com/stardust/legion-agent/internal/port"`。

- [ ] **Step 2: 全量校验**

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1
```

Expected: 无输出即通过（`gofmt -l` 必须为空）；`go test` 全部 `ok`。

- [ ] **Step 3: 带 race 再跑一次事件相关的**

Run: `go test ./internal/storage/ ./internal/domain/ -count=1 -race`
Expected: PASS

- [ ] **Step 4: 提交**

```bash
git add internal/storage/session_events.go
git commit -m "chore(storage): 编译期钉住 SessionEventStore 契约"
```

---

## 完成判据（P1 全部做完时逐条核对）

- [ ] `session_events` 表存在，主键是 `(session_id, seq)`
- [ ] 六条不变量各有测试，且各自的变异都验过红：
  - 不变量 1（连续 seq + 整批事务）→ Task 3 变异 1、2
  - 不变量 2（崩溃恢复补齐）→ Task 5 变异 1、2、3
  - 不变量 3（中间断裂即损坏）→ Task 4 变异 1
  - 不变量 4（未知类型拒绝并指名）→ Task 1 变异
  - 不变量 5（懒物化）→ `TestASessionWithNoEventsLeavesNoTrace`
  - 不变量 6（大载荷不进事件）→ Task 3 变异 3
- [ ] 并发写 seq 连续，`-race` 干净
- [ ] `go build`/`go vet`/`go test ./...` 全绿，`gofmt -l` 为空
- [ ] **P1 没有碰**发射点、屏障、投影、`conversation_turns`、FTS5、`/events`、前端

## 交给 P2 的东西

- `port.SessionEventStore` 三个方法的语义（Append 只追加、ReadFrom 不改库、Load 会改库）
- `maxSessionEventDataBytes = 64 KiB`：P2 接发射点时，`tool/result` 必须只带预览与 spill 定位符，超限会在写入当场被拒
- `domain.ValidateSessionEventType`：P2 新增事件类型时必须同时登记进闭集，否则写入即失败
