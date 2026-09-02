# 会话事件日志 P4a —— 实现计划（events 端点、SSE 帧、spill 定位符）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把事件日志开成轨迹前端能用的两个口子——一个拉历史的 HTTP 端点、一条实时追加的 SSE 帧——并把工具结果的全文定位符真正带进事件。

**Architecture:** `GET /v1/sessions/{id}/events` 走 P1 的 `ReadFrom`（只读后缀、不触发恢复），是轨迹首屏与翻页的来源。实时追加不新开流：在现有 `/v1/events` SSE 上加一类 `session_event` 帧，帧带 `session_id` 与 `seq`，前端按 seq 连续性判断漏帧、漏了从断点回到 HTTP 端点补拉。`tool/result` 事件补上 `spill_locator`，前端展开全文时用它走既有的 `/v1/files`。

**Tech Stack:** Go 1.26；复用 P1 的 `port.SessionEventStore`、P2 的 `eventRecorder`、既有的 `port.EventBus` 与 SSE handler；无新依赖。

## Global Constraints

- Spec：`docs/superpowers/specs/2026-08-31-session-event-log-and-trajectory-design.md`（master）。本计划做 spec §7 的前两行与「详情读取」那段。
- P1（`a431ce9`）、P2（`678054d`）、P3（`7e93cb0`）已合入 master。`conversation_turns` 已退役，事件日志是唯一真相源。**不要改这三期定下的存储语义。**
- **fail-loud 铁律**（`legionAgent/CLAUDE.md` §0）：禁止兜底/fallback、禁止丢弃错误、禁止静默跳过、禁止零值假装正常；错误 `fmt.Errorf("<动作> <标识>: %w", err)` 包装。唯一豁免是契约显式声明的「可选」。
- 完成判据：`go build ./... && go vet ./... && go test ./... -count=1` 全绿，`gofmt -l $(git ls-files '*.go')` 为空。
- 每条不变量都要有断言且**变异可验红**——每个任务最后一步写明「删掉什么会让哪条测试红」，实现者必须真跑并把输出贴进报告。
- 每个 `go test` 带 `-timeout`；`cli`/`plugin`/`server` 包用 `-p 1`。
- **P4a 不做**：轨迹前端（P4b，在 `legionAgentGUI` 仓）；G3 开关（P5）；投影缓存。本计划不得触碰这些。

### 前三期用实证换来的硬约束（P4a 必须守住）

1. **读路径一律用 `ReadFrom`，绝不调用 `Load`**（spec §4.3.1 第 3 条）。`Load` 只能对「确定没有活跃写入者」的会话调用，而 `/events` 端点在任务执行期间也会被调用。
2. 事件 append-only、`seq` 每会话从 0 起连续；`ReadFrom` 只读后缀，中间断裂即报损坏。
3. P3 定下的投影语义不变：按 `(task_id, role)` 折叠、`ID = task_id + ":" + role`。
4. `agent_id` 是**契约允许为空的可选**（默认 agent 路径），`turn_id`/`task_id` 必填。这是 P3 用一条 Critical 换来的裁决。

### 鉴权：新增的帧不得绕开既有失效信号

`internal/server/events.go` 的 SSE 循环里有一路 `revoked := s.tokens.Changed()`——token 轮换时这条流要断。新加的 `session_event` 帧走的是同一个循环，**不要为它另起一条不看 `revoked` 的路径**。

---

## 本期开工前必须知道的：`spill_locator` 根本没有被写进事件

写这个计划时做 spec coverage 检查，发现 spec §4.1 声明的 `spill_locator` 字段**在代码里零命中**：

```
$ grep -rn "spill_locator\|spillLocator" --include=*.go internal/ | grep -v _test
（无输出）
```

现状是：

- `internal/runtime/toolcache.go:55` 的 `writeToolResultCache(toolRoot, cacheDir, toolName, content)` **确实产出了** `relPath`（工具根相对路径，形如 `<cacheDir>/<tool>-<hash>.md`）；
- 但它只被拼进**给模型看的那段文本**（`renderToolResultContent` 的返回值里那句「已保存全文，可用 read_file 翻页续读」），**没有回传给调用方**；
- `internal/runtime/eventlog.go:397` 的 `recordToolResult(callID, preview, isError, dur)` 签名里没有定位符，载荷只有 `call_id`/`preview`/`is_error`/`duration_ms`。

后果：轨迹里点开一条被截断的工具结果，**没有任何东西能告诉前端全文在哪**。spec §7 的「详情读取」整段因此无法实现。Task 3 就是补这个。

### 另一半好消息：根是同源的，不需要补

spec §7 说「spill 落在 `toolRoot/cacheDir` 下，**能否直接被 `/v1/files` 服务需在实现时确认——根不同源就要补**」。我查过了，**在 serve 路径上同源**：

| 端 | 根从哪来 |
|---|---|
| spill 写入 | `toolRoot`（`writeToolResultCache` 的第一个参数） |
| `/v1/files` 读取 | `session.WorkingDir`（`internal/server/http.go:744` 的 `root := strings.TrimSpace(session.WorkingDir)`） |
| 两者的桥 | `internal/server/http.go:1070` `taskWorkingDir = session.WorkingDir`，再 `:1085` `WorkingDir: taskWorkingDir` —— 任务的工作目录**继承自会话** |

所以 `spill_locator` 作为工具根相对路径，可以原样交给 `/v1/files?path=<locator>&session_id=<sid>`。**但这是从代码读出来的结论，Task 3 必须用一条端到端测试把它钉住**——本仓在「接缝看起来对、实际没接」上栽过两次。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/server/session_events.go`（新建） | `GET /v1/sessions/{id}/events` 的 handler：解析 `from_seq`/`limit`、调 `ReadFrom`、序列化 |
| `internal/server/session_events_test.go`（新建） | 端点的参数校验、只读后缀、不调 `Load`、分页语义 |
| `internal/server/http.go`（修改） | 路由分派加一条；`SessionStore` 接口加 `ReadFrom` |
| `internal/server/events.go`（修改） | SSE 循环里放行 `session_event` 帧 |
| `internal/server/events_session_frame_test.go`（新建） | 帧带 `session_id`/`seq`、`?type=` 过滤、token 轮换仍会断流 |
| `internal/runtime/toolcache.go`（修改） | `renderToolResultContent` 把 `relPath` 回传给调用方 |
| `internal/runtime/eventlog.go`（修改） | `recordToolResult` 增加 `spillLocator` 参数并写进载荷 |
| `internal/runtime/runtime.go`（修改） | 发射点把定位符传下去 |
| `internal/server/spill_locator_test.go`（新建） | 端到端：事件里的 `spill_locator` 能被 `/v1/files` 直接服务 |

`session_events.go` 单独成文件：`http.go` 已经很大，而「事件端点怎么分页、怎么拒绝坏参数」是一组自成一体的规则。

---

## Task 1: `GET /v1/sessions/{id}/events` 端点

**Files:**
- Create: `internal/server/session_events.go`
- Create: `internal/server/session_events_test.go`
- Modify: `internal/server/http.go`（路由 switch + `SessionStore` 接口）

**Interfaces:**
- Consumes: P1 的 `ReadFrom(ctx, sessionID string, fromSeq int64) ([]domain.SessionEvent, error)`
- Produces: `GET /v1/sessions/{id}/events?from_seq=&limit=` 返回 `{"events":[{"seq":0,"type":"turn/start","time":"...","data":{...}}],"next_seq":11}`

**为什么返回 `next_seq`**：前端翻页要知道「下一页从哪开始」，而它不能靠 `events[len-1].seq + 1` 自己算——那会把「这一页恰好读完」和「还有下一页」两种情况混为一谈。服务端明确给出。

- [ ] **Step 1: 写失败的测试**

`internal/server/session_events_test.go`：

```go
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 端点是轨迹首屏与翻页的来源。这条断言的是它把 ReadFrom 的结果原样开出来，
// 且 next_seq 指向下一页的起点。
func TestTheEventsEndpointReturnsTheEventsAndTheNextSeq(t *testing.T) {
	srv := newTestServerWithSessionEvents(t, "sess-1", []storedEvent{
		{Seq: 0, Type: "turn/start", Data: `{"turn":0}`},
		{Seq: 1, Type: "user/message", Data: `{"turn":0,"content":"你好"}`},
		{Seq: 2, Type: "turn/end", Data: `{"turn":0,"reason":"completed"}`},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-1/events", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，要 200：%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Events []struct {
			Seq  int64           `json:"seq"`
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		} `json:"events"`
		NextSeq int64 `json:"next_seq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应：%v，原文 %s", err, rec.Body.String())
	}
	if len(got.Events) != 3 {
		t.Fatalf("返回 %d 条事件，要 3 条", len(got.Events))
	}
	if got.Events[0].Seq != 0 || got.Events[2].Seq != 2 {
		t.Errorf("seq 不对：%d..%d", got.Events[0].Seq, got.Events[2].Seq)
	}
	if got.Events[1].Type != "user/message" {
		t.Errorf("type = %q", got.Events[1].Type)
	}
	if got.NextSeq != 3 {
		t.Errorf("next_seq = %d，要 3（最后一条 seq 2 的下一格）", got.NextSeq)
	}
}

// from_seq 是翻页的续读点：只回它之后的后缀。
func TestFromSeqReturnsOnlyTheSuffix(t *testing.T) {
	srv := newTestServerWithSessionEvents(t, "sess-1", []storedEvent{
		{Seq: 0, Type: "turn/start", Data: `{"turn":0}`},
		{Seq: 1, Type: "user/message", Data: `{"turn":0,"content":"你好"}`},
		{Seq: 2, Type: "turn/end", Data: `{"turn":0,"reason":"completed"}`},
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-1/events?from_seq=2", nil))

	var got struct {
		Events []struct {
			Seq int64 `json:"seq"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if len(got.Events) != 1 || got.Events[0].Seq != 2 {
		t.Fatalf("from_seq=2 返回了 %d 条（首条 seq 见下），要只回 seq 2 那一条：%+v", len(got.Events), got.Events)
	}
}

// limit 压住每屏事件数（spec §7：虚拟滚动先不做，靠 limit 分页压住）。
// 截断时 next_seq 必须指向**被截掉的第一条**，否则前端翻页会跳过事件。
func TestLimitTruncatesAndNextSeqPointsAtTheFirstDroppedEvent(t *testing.T) {
	srv := newTestServerWithSessionEvents(t, "sess-1", []storedEvent{
		{Seq: 0, Type: "turn/start", Data: `{"turn":0}`},
		{Seq: 1, Type: "user/message", Data: `{"turn":0,"content":"你好"}`},
		{Seq: 2, Type: "turn/end", Data: `{"turn":0,"reason":"completed"}`},
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-1/events?limit=2", nil))

	var got struct {
		Events []struct {
			Seq int64 `json:"seq"`
		} `json:"events"`
		NextSeq int64 `json:"next_seq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if len(got.Events) != 2 {
		t.Fatalf("limit=2 返回了 %d 条", len(got.Events))
	}
	if got.NextSeq != 2 {
		t.Errorf("next_seq = %d，要 2（被截掉的第一条）：前端下一页从这里续读，指错会漏事件", got.NextSeq)
	}
}

// 坏参数是调用方的错，必须 400 并说清楚哪个参数坏了——不要悄悄当成默认值。
func TestBadPagingParametersAreRefusedByName(t *testing.T) {
	srv := newTestServerWithSessionEvents(t, "sess-1", nil)

	for _, tc := range []struct{ query, wants string }{
		{"?from_seq=-1", "from_seq"},
		{"?from_seq=abc", "from_seq"},
		{"?limit=-5", "limit"},
		{"?limit=abc", "limit"},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-1/events"+tc.query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s → 状态码 %d，要 400", tc.query, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.wants) {
			t.Errorf("%s → 错误信息没指名 %q：%s", tc.query, tc.wants, rec.Body.String())
		}
	}
}

// 不存在的会话返回 404，而不是空事件列表——「这条会话没有事件」和
// 「这条会话不存在」是两件事，前端要能区分。
func TestAMissingSessionIsNotFound(t *testing.T) {
	srv := newTestServerWithSessionEvents(t, "sess-1", nil)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-nope/events", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("状态码 %d，要 404：%s", rec.Code, rec.Body.String())
	}
}
```

> **实现者注意（夹具是本仓栽过五次的地方）**：`newTestServerWithSessionEvents` 与 `storedEvent` 需要你自己建。**它的 `ReadFrom` 必须与真 store 语义一致**——真 store（`internal/storage/session_events.go`）的 `ReadFrom` 只读后缀、遇到中间断裂报损坏、`fromSeq < 0` 报错。P2 就栽在「夹具不校验 seq 而真 store 校验」上，让一条功能回归对整套测试隐形。**照 `internal/server` 里既有的 session 夹具写法来**（`http_test.go` 里有现成的 `SessionStore` 假实现），不要新造一套语义不同的。

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/server/ -run "EventsEndpoint|FromSeq|LimitTruncates|BadPagingParameters|MissingSessionIsNotFound" -count=1 -p 1 -timeout 10m`

Expected: 编译失败（`newTestServerWithSessionEvents` 未定义），或路由不存在导致 404/405。**把真实输出记进报告。**

- [ ] **Step 3: 写实现**

`internal/server/session_events.go`：

```go
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultSessionEventsLimit 是一页事件的默认上限。
//
// spec §7 定了「虚拟滚动先不做：靠 limit 分页压住每屏事件数」，所以这个默认值就是
// 前端首屏的规模。取 500：一条典型会话的一轮对话约 6-10 条事件，500 足够铺满首屏
// 而不会让单个响应大到需要流式。
const defaultSessionEventsLimit = 500

// sessionEventsResponse 是 GET /v1/sessions/{id}/events 的响应体。
//
// NextSeq 由服务端给出而不是让前端从 events 末尾自己算：那样会把「这一页恰好读完」
// 与「还有下一页」混为一谈。截断时它指向**被截掉的第一条**，前端据此续读不会漏。
type sessionEventsResponse struct {
	Events  []sessionEventDTO `json:"events"`
	NextSeq int64             `json:"next_seq"`
}

type sessionEventDTO struct {
	Seq int64 `json:"seq"`
	// Type 是 domain.SessionEventType 的字符串形式（闭集，见 internal/domain/session_event.go）。
	Type string `json:"type"`
	// Time 用 time.Time 直接序列化，与本包既有 DTO 一致（见 conversationTurnResponse.CreatedAt，
	// internal/server/http.go:523），由 encoding/json 产出 RFC3339Nano。
	Time time.Time       `json:"time"`
	Data json.RawMessage `json:"data"`
}

// handleSessionEvents 开出一条会话的原始事件，供轨迹首屏与翻页使用。
//
// 走 ReadFrom 而不是 Load：spec §4.3.1 第 3 条要求 Load 只对「确定没有活跃写入者」的
// 会话调用，而这个端点在任务执行期间也会被前端调用。ReadFrom 只读后缀、不触发崩溃恢复，
// 正是这里要的。
func (s *HTTPServer) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "session store is unavailable")
		return
	}
	sessionID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/events")
	if strings.TrimSpace(sessionID) == "" {
		writeError(w, http.StatusBadRequest, "session id is required")
		return
	}

	// 会话不存在与「会话存在但没有事件」是两件事，前端要能区分。
	if _, ok, err := s.sessions.GetAgentSession(r.Context(), sessionID); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load session %q: %v", sessionID, err))
		return
	} else if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", sessionID))
		return
	}

	fromSeq, err := parseNonNegativeQueryInt(r, "from_seq", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := parseNonNegativeQueryInt(r, "limit", defaultSessionEventsLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if limit == 0 {
		limit = defaultSessionEventsLimit
	}

	events, err := s.sessions.ReadFrom(r.Context(), sessionID, fromSeq)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("read session events for %q: %v", sessionID, err))
		return
	}

	// nextSeq 的两种情形：截断了就指向被截掉的第一条；没截断就指向最后一条的下一格。
	// 空结果时保持调用方给的 fromSeq——「从这里继续等」。
	nextSeq := fromSeq
	truncated := int64(len(events)) > limit
	if truncated {
		events = events[:limit]
	}
	if len(events) > 0 {
		nextSeq = events[len(events)-1].Seq + 1
	}

	out := sessionEventsResponse{Events: make([]sessionEventDTO, 0, len(events)), NextSeq: nextSeq}
	for _, e := range events {
		out.Events = append(out.Events, sessionEventDTO{
			Seq:  e.Seq,
			Type: string(e.Type),
			Time: e.Time.UTC(),
			Data: e.Data,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// parseNonNegativeQueryInt 读一个非负整数查询参数。
//
// 坏值一律报错并**指名是哪个参数**，不悄悄当成默认值：调用方拼错了参数名或传了负数，
// 静默用默认值会让它以为自己的分页生效了（fail-loud 铁律，CLAUDE.md §0）。
// 参数缺席是合法的可选，返回 fallback。
func parseNonNegativeQueryInt(r *http.Request, name string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", name, raw)
	}
	if v < 0 {
		return 0, fmt.Errorf("%s must not be negative, got %d", name, v)
	}
	return v, nil
}
```

`internal/server/http.go` 两处改动：

```go
// 1) 路由 switch 里加一条。注意它必须排在既有的
//    `strings.HasPrefix(r.URL.Path, "/v1/sessions/") && strings.HasSuffix(..., "/turns")`
//    那几条**同族分支旁边**，且不能被 PATCH/DELETE 那两条 `!strings.HasSuffix(..., "/turns")`
//    的分支抢走——那两条today 用「不是 /turns」来判定「是会话本体」，加了 /events 之后
//    这个判定就不成立了。请自己读那几行现状再决定插在哪、要不要一并改它们的条件。
case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sessions/") && strings.HasSuffix(r.URL.Path, "/events"):
	s.handleSessionEvents(rec, r)

// 2) SessionStore 接口加一个方法（它今天已经有 ListConversationTurns、GetAgentSession 等）
ReadFrom(ctx context.Context, sessionID string, fromSeq int64) ([]domain.SessionEvent, error)
```

> **实现者注意**：上面那条「PATCH/DELETE 用『不是 /turns』判定会话本体」是我读路由时发现的真实风险（`http.go:341`、`:343`）。**动手前自己核实**，如果现状确实如此，`DELETE /v1/sessions/{id}/events` 这种请求会被路由成「删除会话」——那是数据损坏级的错误。把你的核实结论写进报告。

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/server/ -run "EventsEndpoint|FromSeq|LimitTruncates|BadPagingParameters|MissingSessionIsNotFound" -count=1 -p 1 -timeout 10m -v`

Expected: 五条都 `--- PASS`。核对 `-run` 模式确实匹配到了这五个函数名（`grep -n "^func Test" internal/server/session_events_test.go`），不要接受 `[no tests to run]` 却报 `ok`。

再跑全量：`go test ./... -count=1 -p 1 -timeout 40m`

- [ ] **Step 5: 变异验证（四条）**

| # | 变异 | 期望红在哪 |
|---|---|---|
| 1 | `nextSeq` 截断分支去掉，恒返回 `events[len-1].Seq + 1` | `TestLimitTruncatesAndNextSeqPointsAtTheFirstDroppedEvent` |
| 2 | `parseNonNegativeQueryInt` 的 `v < 0` 检查删掉 | `TestBadPagingParametersAreRefusedByName` |
| 3 | 会话存在性检查删掉（不存在也返回 200 空列表） | `TestAMissingSessionIsNotFound` |
| 4 | `ReadFrom(ctx, sessionID, fromSeq)` 改成 `ReadFrom(ctx, sessionID, 0)` | `TestFromSeqReturnsOnlyTheSuffix` |

每条：改 → 跑 → 贴真实 FAIL 输出 → `git checkout --` 还原 → `git status --short` 确认为空。

**注意**：变异只造成编译失败不算变异验证——改成能编译但行为错的形式再跑。

- [ ] **Step 6: 提交**

```bash
git add internal/server/session_events.go internal/server/session_events_test.go internal/server/http.go
git commit -m "feat(server): 会话事件的分页读取端点"
```

---

## Task 2: SSE 的 `session_event` 帧

**Files:**
- Modify: `internal/server/events.go`
- Create: `internal/server/events_session_frame_test.go`
- Modify: 事件发布侧（见 Step 3 的说明——**实现者需要自己定位**）

**Interfaces:**
- Consumes: 既有的 `s.platformEvents.Subscribe(ctx)`、`domain.RuntimeEvent`
- Produces: SSE 上一类新帧，`type` 为 `session_event`，payload 至少带 `session_id`、`seq`、`event_type`

**为什么不新开流**（spec §7 原文）：GUI 已有 SSE 桥（`sse_bridge.go` → Wails 事件），再加一条通道等于多一套重连/鉴权/断线语义。

**前端的漏帧策略**（spec §7 原文）：帧带 `session_id` 与 `seq`，前端按 seq 连续性判断是否漏帧，**漏了从断点补拉**（回到 Task 1 的端点）而不是猜。所以这条帧**不需要保证送达**，但**必须带准确的 seq**——seq 错了比漏帧更糟，前端会以为自己没漏。

- [ ] **Step 1: 写失败的测试**

`internal/server/events_session_frame_test.go`：

```go
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/observability"
)

// 帧必须带 session_id 与 seq：前端靠 seq 连续性判断漏帧，漏了回到
// /v1/sessions/{id}/events 从断点补拉。seq 错了比漏帧更糟——前端会以为自己没漏。
//
// 这里断言的是**帧的内容**，不是「代码里有那一行」。
func TestASessionEventFrameCarriesTheSessionIDAndSeq(t *testing.T) {
	bus := observability.NewEventBus(8)
	srv := NewHTTPServer(Config{AdminToken: "token", PlatformEvents: bus})
	req := httptest.NewRequest(http.MethodGet, "/v1/events?type=session_event", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	go func() {
		time.Sleep(10 * time.Millisecond)
		if err := bus.Publish(context.Background(), observability.EventEnvelope{
			Type:      "session_event",
			SubjectID: "sess-1",
			Data: map[string]any{
				"session_id": "sess-1",
				"seq":        7,
				"event_type": "tool/call",
			},
		}); err != nil {
			t.Errorf("Publish(session_event) error = %v", err)
		}
		if err := bus.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "event: session_event") {
		t.Fatalf("响应里没有 session_event 帧：%q", body)
	}
	for _, want := range []string{`"session_id":"sess-1"`, `"seq":7`, `"event_type":"tool/call"`} {
		if !strings.Contains(body, want) {
			t.Errorf("帧里缺 %s：前端靠 session_id+seq 判断漏帧
完整响应：%s", want, body)
		}
	}
}

// ?type= 过滤是既有契约：只订阅 session_event 的客户端不该收到别的帧，
// 新增一类帧也不能把老客户端的过滤打乱。
func TestTypeFilterStillSelectsOnlyTheRequestedFrames(t *testing.T) {
	bus := observability.NewEventBus(8)
	srv := NewHTTPServer(Config{AdminToken: "token", PlatformEvents: bus})
	req := httptest.NewRequest(http.MethodGet, "/v1/events?type=session_event", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	go func() {
		time.Sleep(10 * time.Millisecond)
		if err := bus.Publish(context.Background(), observability.EventEnvelope{
			Type:      "session_event",
			SubjectID: "sess-1",
			Data:      map[string]any{"session_id": "sess-1", "seq": 0, "event_type": "turn/start"},
		}); err != nil {
			t.Errorf("Publish(session_event) error = %v", err)
		}
		if err := bus.Publish(context.Background(), observability.EventEnvelope{
			Type: "task.completed",
			Data: map[string]any{"task_id": "task-1"},
		}); err != nil {
			t.Errorf("Publish(task.completed) error = %v", err)
		}
		if err := bus.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "event: session_event") {
		t.Fatalf("订阅 session_event 却没收到它：%q", body)
	}
	if strings.Contains(body, "task.completed") {
		t.Errorf("?type=session_event 却收到了 task.completed：过滤被新帧打乱了
%s", body)
	}
}

// token 轮换要断流。新帧走的是同一个 select 循环，不能为它另起一条不看
// revoked 的路径——那等于给 SSE 开了个绕过鉴权失效的后门。
func TestSessionEventFramesStopWhenTheTokenIsRotated(t *testing.T) {
	// 实现者：本包已有覆盖 token 轮换断流的测试（grep "tokens\|Changed\|rotate"
	// 找到它，internal/server/tokenrevoke_test.go 是个起点）。照它的写法建 server、
	// 订阅 ?type=session_event、在流上轮换 token，断言 ServeHTTP 返回（流被关闭）。
	//
	// 断言的是**流真的断了**，不是「代码里引用了 revoked」。
	t.Fatal("实现者：按上面的说明写出真实断言，然后删掉这一行")
}
```

> **实现者注意**：前两条测试可以原样用（`observability.NewEventBus` + `EventEnvelope` + `bus.Close()` 终止流，是本包既有 SSE 测试的写法，见 `events_test.go:15`）。第三条留了 `t.Fatal` 占位——**它必须被真实断言替换掉，不能留着**（`t.Fatal` 会让这条测试永远红，正好逼你写完；本仓有过用 `t.Skip` 占位的前科，而 `t.Skip` 挡不住编译期检查、留着毫无意义）。它需要照抄本包既有的 token 轮换测试搭法，而那套夹具我不能凭空写对。

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/server/ -run "SessionEventFrame|TypeFilterStillSelects" -count=1 -p 1 -timeout 10m`

Expected: 三条 FAIL（帧类型还不存在）。**把真实输出记进报告。**

- [ ] **Step 3: 写实现**

两件事：

**(a) 发布侧**——事件写进日志后要往总线上发一条。**发布点在哪由你定位**：`eventRecorder.flush`（`internal/runtime/eventlog.go`）是事件真正落盘的地方，而 `port.EventBus` 只有 `Publish(ctx, domain.RuntimeEvent)`。请自己判断：

- 是在 `flush` 成功后逐条发布，还是在 `Runtime` 那一层发布？
- `eventRecorder` 今天**没有** `EventBus`，加进去会不会让它多一个依赖、违反它「只管把事件写进 store」的职责？
- 发布失败要怎么办？**这条特别重要**：屏障是 fail-closed 的（P2 定的），但「SSE 通知发不出去」和「事件落盘失败」是两件事——前者前端可以靠 seq 连续性补拉，后者不能。想清楚再写，并把理由写进报告。

**(b) SSE 侧**——`internal/server/events.go` 的循环里放行这类帧。既有循环已经处理了 `?type=` 过滤与 `revoked`，新帧走同一条路。

**约束**：
- 帧的 payload 至少要有 `session_id`、`seq`、`event_type`。
- **不要在帧里塞事件的完整 `data`**：spec §7 的设计是「帧只做通知，内容从端点拉」。塞进去会让一条大事件把 SSE 流撑爆，而这条流还承载着别的帧。
- 不要为新帧另起一条不看 `revoked` 的路径。

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/server/ -run "SessionEventFrame|TypeFilterStillSelects" -count=1 -p 1 -timeout 10m -v`

再跑全量：`go test ./... -count=1 -p 1 -timeout 40m`

- [ ] **Step 5: 变异验证（三条）**

| # | 变异 | 期望红在哪 |
|---|---|---|
| 1 | 帧的 payload 里不填 `seq`（填 0） | `TestASessionEventFrameCarriesTheSessionIDAndSeq` |
| 2 | 发布点删掉（事件落盘但不发帧） | `TestASessionEventFrameCarriesTheSessionIDAndSeq` |
| 3 | SSE 循环里让 `session_event` 绕开 `?type=` 过滤 | `TestTypeFilterStillSelectsOnlyTheRequestedFrames` |

变异 2 是本任务的**接线守卫**——本仓栽过两次「接缝在但没人调用它」。若它不红，说明发布点没被任何测试覆盖，**补测试再继续**。

- [ ] **Step 6: 提交**

```bash
git add internal/server/events.go internal/server/events_session_frame_test.go
git commit -m "feat(server): SSE 增加 session_event 帧"
```

（若发布侧改到了 `internal/runtime`，一并显式 `git add` 那些文件。）

---

## Task 3: `spill_locator` 进事件，并验它能被 `/v1/files` 服务

**Files:**
- Modify: `internal/runtime/toolcache.go`（`renderToolResultContent` 回传 `relPath`）
- Modify: `internal/runtime/eventlog.go`（`recordToolResult` 增参并写载荷）
- Modify: `internal/runtime/runtime.go`（发射点传值）
- Create: `internal/server/spill_locator_test.go`（端到端）
- Modify: `internal/runtime/eventlog_test.go`（载荷断言）

**Interfaces:**
- Consumes: `writeToolResultCache(toolRoot, cacheDir, toolName, content) (string, error)` 的返回值——工具根相对路径
- Produces: `tool/result` 事件载荷新增 `spill_locator` 字段（无 spill 时为空串，**这是契约允许的可选**：结果没超长就没有全文文件）

**这一任务补的是 spec §4.1 声明、但代码里零命中的字段**（见本计划开头那节）。没有它，轨迹点开一条被截断的工具结果，没有任何东西能告诉前端全文在哪。

- [ ] **Step 1: 写失败的测试**

先加载荷断言到 `internal/runtime/eventlog_test.go`：

```go
// spec §4.1 声明了 spill_locator：tool/result 只存预览，全文落在工具根下的缓存文件里，
// 定位符是取回全文的唯一线索。没有它，轨迹里点开一条被截断的结果就没有下文。
func TestAToolResultEventCarriesTheSpillLocator(t *testing.T) {
	store := newCaptureEventStore()
	rec := newEventRecorder(store, "sess-1", "task-7", "agent-a")
	rec.recordTurnStart()
	rec.recordStepStart()
	rec.recordToolResult("c1", "预览片段", false, 12*time.Millisecond, ".stardust/cache/read_file-abc123.md")
	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	data := payloadOfType(t, store.events, domain.SessionEventToolResult)
	if got := data["spill_locator"]; got != ".stardust/cache/read_file-abc123.md" {
		t.Errorf("spill_locator = %v，要 .stardust/cache/read_file-abc123.md", got)
	}
}

// 没有 spill 时定位符为空串——结果没超长就没有全文文件。
// 这是契约显式声明的可选，不是兜底。
func TestAToolResultWithoutSpillCarriesAnEmptyLocator(t *testing.T) {
	store := newCaptureEventStore()
	rec := newEventRecorder(store, "sess-1", "task-7", "agent-a")
	rec.recordTurnStart()
	rec.recordStepStart()
	rec.recordToolResult("c1", "短结果", false, time.Millisecond, "")
	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	data := payloadOfType(t, store.events, domain.SessionEventToolResult)
	if got, ok := data["spill_locator"].(string); !ok || got != "" {
		t.Errorf("spill_locator = %v，要空串", data["spill_locator"])
	}
}
```

> `payloadOfType` 与 `newCaptureEventStore` 是 P2/P3 已有的辅助函数，同包直接用。

再加端到端测试 `internal/server/spill_locator_test.go`：

```go
package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// spec §7 的「详情读取」：tool/result 只有预览 + 定位符，展开时按定位符取全文。
// 这条测试钉的是**两个根真的同源**——spill 写在 toolRoot 下，/v1/files 读的是
// session.WorkingDir，而任务的 WorkingDir 继承自会话（http.go:1070）。
// spec 原话：「能否直接被它服务需在实现时确认——根不同源就要补」。
//
// 它断言的是端到端的结果（HTTP 真的把全文吐出来了），不是「路径字符串看起来一样」。
func TestASpillLocatorCanBeServedByTheFilesEndpoint(t *testing.T) {
	workdir := t.TempDir()
	// 造一个 spill 文件，位置与 writeToolResultCache 的产物一致：toolRoot/<locator>
	locator := filepath.Join(".stardust", "cache", "read_file-abc123.md")
	full := filepath.Join(workdir, locator)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const wholeText = "这是被截断的工具结果的全文。"
	if err := os.WriteFile(full, []byte(wholeText), 0o644); err != nil {
		t.Fatalf("write spill: %v", err)
	}

	// 会话的 WorkingDir 就是 toolRoot——这正是被测的那条同源关系。
	srv := newTestServerWithSessionWorkingDir(t, "sess-1", workdir)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/v1/files?session_id=sess-1&path="+url.QueryEscape(filepath.ToSlash(locator)), nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，要 200：%s\n"+
			"这说明 spill 的根与 /v1/files 的根不同源——spec §7 说过「根不同源就要补」",
			rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != wholeText {
		t.Errorf("取回的全文 = %q，要 %q", got, wholeText)
	}
}
```

> **实现者注意**：`newTestServerWithSessionWorkingDir` 要你自己建（或复用本包既有的 server 夹具再设 `WorkingDir`）。**关键是那个会话的 `WorkingDir` 必须真的等于你写 spill 文件的目录**——如果你为了让测试通过而把两者设成不同再手工拼路径，这条测试就什么都没验到。本仓在这种「让生产上不可能成立的前置条件在测试里成立」上栽过（P3 的 Task 3）。

- [ ] **Step 2: 跑测试确认它红**

Run: `go test ./internal/runtime/ -run "SpillLocator|WithoutSpill" -count=1 -timeout 10m` → 编译失败（`recordToolResult` 参数数量不对）
Run: `go test ./internal/server/ -run "SpillLocatorCanBeServed" -count=1 -p 1 -timeout 10m` → 编译失败或 404

**把两条的真实输出都记进报告。**

- [ ] **Step 3: 写实现**

**(a) `internal/runtime/toolcache.go`** —— `renderToolResultContent` 现在只返回拼好的文本，把 `relPath` 一并回传：

```go
// renderToolResultContent 渲染给模型看的工具结果，并回传全文的定位符。
//
// 第二个返回值是**工具根相对路径**（spec §4.1 的 spill_locator）：内容超长时全文被写到
// toolRoot/cacheDir 下，这个路径是取回它的唯一线索。没有发生 spill 时返回空串——那是
// 契约允许的可选（结果没超长就没有全文文件），不是兜底。
//
// 在这之前这个路径只被拼进给模型的那段提示文本里，调用方拿不到，于是事件日志里也就
// 没有它——轨迹点开一条被截断的结果就没有下文。
func renderToolResultContent(toolName, content string, maxResultChars int, toolRoot, cacheDir string, logger *slog.Logger) (string, string) {
	// ……保留现有全部逻辑，只把返回值从一个改成两个：
	//   - 未超长 / toolRoot 为空 / read_file / 写缓存失败这几条早返回路径，第二个值返回 ""
	//   - 成功写了缓存那条路径，第二个值返回 relPath
}
```

> 实现者：这个函数今天有五条返回路径（`maxResultChars<=0`、未超长、`toolRoot` 空或 `read_file`、写缓存失败、成功）。**逐条决定第二个返回值**，别漏。写缓存失败那条尤其要注意：它已经在 fail-loud 地记日志了，定位符返回空串是对的（全文确实不存在）。

**(b) `internal/runtime/eventlog.go`** —— `recordToolResult` 增参：

```go
// recordToolResult 记一次工具调用的结果。
//
// spillLocator 是全文的工具根相对路径（spec §4.1）；结果没超长时为空串，那是契约允许的
// 可选。前端展开全文时把它交给 /v1/files —— 两个根同源（任务的 WorkingDir 继承自会话，
// 而 /v1/files 的根就是会话的 WorkingDir），所以定位符可以原样使用。
func (e *eventRecorder) recordToolResult(callID string, preview string, isError bool, dur time.Duration, spillLocator string) {
	e.append(domain.SessionEventToolResult, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(),
		"call_id": callID, "preview": truncateRunes(preview, maxEventPreviewRunes),
		"is_error": isError, "duration_ms": dur.Milliseconds(),
		"spill_locator": spillLocator,
	})
}
```

**(c) `internal/runtime/runtime.go`** —— 发射点把定位符传下去。`renderToolResultContent` 的调用点现在多了一个返回值，顺着它传到 `recordToolResult`。

> **实现者注意**：`renderToolResultContent` 可能有多个调用点。**全部找出来**（`grep -rn "renderToolResultContent" --include=*.go internal/`），逐个处理。本仓栽过两次「只改了一处调用点」。

- [ ] **Step 4: 跑测试确认它绿**

Run: `go test ./internal/runtime/ -run "SpillLocator|WithoutSpill" -count=1 -timeout 10m -v`
Run: `go test ./internal/server/ -run "SpillLocatorCanBeServed" -count=1 -p 1 -timeout 10m -v`

Expected: 三条都 `--- PASS`。核对 `-run` 模式真的匹配到了这三个函数名。

再跑全量：`go build ./... && go vet ./... && go test ./... -count=1 -p 1 -timeout 40m`

- [ ] **Step 5: 变异验证（三条）**

| # | 变异 | 期望红在哪 |
|---|---|---|
| 1 | `recordToolResult` 载荷里不填 `spill_locator` | `TestAToolResultEventCarriesTheSpillLocator` |
| 2 | `renderToolResultContent` 成功路径的第二个返回值改成 `""` | `TestAToolResultEventCarriesTheSpillLocator`（若不红，说明发射点没接上——**补测试**） |
| 3 | 端到端测试里把会话的 `WorkingDir` 改成另一个目录 | `TestASpillLocatorCanBeServedByTheFilesEndpoint` |

变异 3 验的是「这条端到端测试真的在验同源关系」——如果改了根它还绿，说明它没验到东西。

- [ ] **Step 6: 提交**

```bash
git add internal/runtime/toolcache.go internal/runtime/eventlog.go internal/runtime/runtime.go internal/runtime/eventlog_test.go internal/server/spill_locator_test.go
git commit -m "feat(runtime): tool/result 带上全文定位符"
```

---

## 完成判据（P4a 全部做完时逐条核对）

- [ ] `GET /v1/sessions/{id}/events` 能分页读出事件，`from_seq`/`limit` 语义正确，坏参数 400 并指名，不存在的会话 404
- [ ] 该端点走 `ReadFrom`，**全条路径上没有 `Load`**（spec §4.3.1 第 3 条）
- [ ] `next_seq` 在截断时指向**被截掉的第一条**——前端据此续读不会漏事件
- [ ] SSE 上有 `session_event` 帧，带 `session_id` 与 `seq`，且**不夹带完整 data**
- [ ] `?type=` 过滤对新帧仍准确；token 轮换仍会断流
- [ ] `tool/result` 事件带 `spill_locator`，无 spill 时为空串
- [ ] **一条端到端测试证明定位符能被 `/v1/files` 直接服务**（spec §7 明写要验的接缝）
- [ ] `PATCH`/`DELETE /v1/sessions/{id}` 那两条「不是 /turns 就是会话本体」的路由分支已核实，不会把 `/events` 请求误判
- [ ] `go build`/`go vet`/`go test ./...` 全绿，`gofmt -l` 为空
- [ ] **P4a 没有碰**：轨迹前端（P4b）、G3 开关（P5）、投影缓存

## 交给 P4b（GUI 仓）的东西

- 首屏与翻页：`GET /v1/sessions/{id}/events?from_seq=&limit=`，响应带 `next_seq`
- 实时追加：SSE 的 `session_event` 帧，带 `session_id` + `seq`；**按 seq 连续性判断漏帧，漏了回到上面那个端点从断点补拉**，不要猜
- 工具结果全文：事件里的 `spill_locator` 直接交给 `/v1/files?session_id=<sid>&path=<locator>`
- **这条同源关系仅当会话绑定了 `working_dir` 时成立**：未绑定 `working_dir` 的会话，`spill_locator` 仍可能非空（工具根回退到 `ContextFiles.Root`），但 `/v1/files` 对空 `WorkingDir` 直接 404「session has no working directory」，取不回全文。P4b 必须把这个 404 当成「全文不可得」的合法结果来渲染（例如「全文不可用」的占位态），**不要**当成错误弹窗或网络故障处理。
- GUI 已有 SSE 桥（`sse_bridge.go` → Wails 事件），新帧走同一条桥，不要新开流

## 本期已知、不在范围内的事

- 投影缓存不做（spec §6：「投影在真实会话长度上测出慢时」再加）
- 虚拟滚动不做（spec §7：靠 `limit` 分页压住每屏事件数，测出卡顿再加）
- `assistant/chunk` 不入库（spec §10：需要事后逐 token 回放流式过程时再考虑）
