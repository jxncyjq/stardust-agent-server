# Legion IM 网关 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 Legion 落地独立 `legion-gateway` 进程，经现有 HTTP API + SSE 把 IM 平台（样板 Telegram，长轮询）双向接入 agent 运行时，核心零改动。

**Architecture:** 新包 `internal/gateway` 提供平台无关框架（`ChannelAdapter` 自驱动接口 + `PlatformRegistry` 工厂 + `DeliveryRouter` + `SessionBinder` + `DeliveryTracker` + `CoreClient` + `GatewayRunner`）。Telegram 适配器跑 `getUpdates` 长轮询入站、`sendMessage` 出站。网关经 `POST /v1/sessions`、`POST /v1/tasks` 提交，**轮询 `GET /v1/tasks/{id}/result`** 拿完成结果回投。原始平台 id 只留网关，Legion 只见 `HashID` 脱敏值。

> **设计修订（Task 1 验证后，用户确认）**：完成信号**改为轮询 `GET /v1/tasks/{id}/result`**，不用 SSE `/v1/events`——后者是 observability `EventEnvelope` 流，**不携带** `task_completed`。网关只轮询自己提交的 task_id（有界集合），无 SSE 断线丢事件风险。影响 Task 4（加 `Pending()`）、Task 8（`TaskResult` 取代 `StreamEvents`）、Task 10（轮询环取代 SSE 环）。

**Tech Stack:** Go 1.26、标准库 `net/http`/`database/sql`/`bufio`、`modernc.org/sqlite`（已在 go.mod）、`log/slog`。不引入新第三方依赖。

## Global Constraints

- Go 版本 `go 1.26.0`（与 `go.mod` 一致）；不新增第三方依赖，SQLite 用 `modernc.org/sqlite`（已存在）。
- **Fail-Loud 铁律**（`legionAgent/CLAUDE.md`）：错误一律 `return fmt.Errorf("<动作> <标识>: %w", err)` 包装；禁止返回零值假装正常、禁止 `_ = err`、禁止静默 `continue`/`return`。错误路径必须有测试断言。
- 装配期不可恢复错误（缺配置/未知平台）用 `log.Fatal`；goroutine 顶层 / 投递边界用 `slog` 结构化记录（Error/Warn），带 `platform`/`chat`(hash)/`task` 字段，不得 `fmt.Println`。
- 公开 API 必须有 Go doc 注释（以标识符名开头）。
- 验证门（每个 Task 完成后必跑，全绿 + gofmt 空方算完成）：
  ```bash
  cd legion/legionAgent && go build ./... && go vet ./... && go test ./... && gofmt -l .
  ```
- `.gitattributes` 固定 `*.go` 为 LF；Windows 上编辑保持 LF。
- PII：任何进入 Legion（session/task/turn）的平台标识必须先经 `gateway.HashID`；原始 chat/user id 只存网关绑定库，仅用于出站投递。

---

## 文件结构

```
cmd/gateway/main.go                       # legion-gateway 入口：装配 + 运行
internal/gateway/
  adapter.go        # InboundMessage / DeliveryTarget / PlatformConfig / ChannelAdapter 接口
  registry.go       # PlatformEntry / PlatformRegistry（工厂）
  hash.go           # HashID（PII 脱敏）
  tracker.go        # DeliveryTracker（task_id → DeliveryTarget，内存）
  router.go         # DeliveryRouter（选 adapter + 切分 + Send）
  binder.go         # SessionBinder 接口 + SQLiteBinder（持久绑定）
  config.go         # GatewayConfig + Load（JSON + env 密钥 + 校验）
  core_client.go    # CoreClient 接口 + HTTPCoreClient（/v1/sessions、/v1/tasks、SSE /v1/events）
  runner.go         # GatewayRunner（编排：入站流 + 投递环）
  platforms/telegram/adapter.go   # Telegram getUpdates 轮询 + sendMessage
```

**核心 API 契约（已核对 `internal/server/http.go`，各 Task 依赖）**：
- `POST /v1/sessions`，body `{"company_id","agent_id","project","title"}`，返回 `200/201` JSON 含 `"id"`（形如 `session-<unixnano>`）。
- `POST /v1/tasks`，body `{"id"(必填),"input"(必填),"company_id","agent_id","session_id","images"}`，返回 `201` JSON 含 `"id"`。**task id 由网关自铸**。
- `GET /v1/tasks/{id}/result`：返回 `{"task_id","status","result","total_tokens","elapsed_ms"}`。`status` 为 `domain.TaskStatus`：`pending`/`assigned`/`running`/`quality_review`/`done`/`failed`/`suspended`；**终态** = `done`/`failed`/`suspended`，`done` 时 `result` 为模型答复文本。**勿用 `/v1/events` SSE**（observability `EventEnvelope` 流，不含 task_completed）。
- 鉴权：所有 `/v1/*` 需 `Authorization: Bearer <adminToken>`。

---

## Task 1：核对核心「任务提交→完成事件」契约（去风险）

**背景**：整个异步链路依赖「经 `POST /v1/tasks` 提交的任务会被核心执行并发 `task_completed` 事件到 `/v1/events`」。先用测试锁死此契约，避免后续基于臆测。

**Files:**
- Read: `internal/cli/command.go`（serve 装配：`coordinator`、`background` heartbeat、`workflowEvents`、`/v1/events` 接线，约 1670–1820 行）
- Read: `internal/server/http.go`（`handleCreateTask`、`handleCreateSession`、`/v1/events`）、`internal/server/events.go`
- Read: `internal/cli/command_test.go`（既有 serve/task 端到端测试范式，参照其启动方式）
- Test: `internal/cli/gateway_contract_test.go`（新）

- [ ] **Step 1: 读上述文件**，确认：API 提交的任务经 live scheduler + coordinator heartbeat 执行，完成后 `task_completed{TaskID, Message=结果}` 发到 `workflowEvents`，且 `/v1/events` 从该事件源流式输出。记录 `createTaskRequest`/`createSessionRequest` 字段与返回体结构（若与本计划「核心 API 契约」有出入，以代码为准并同步更新后续 Task 的 CoreClient 实现）。

- [ ] **Step 2: 写契约测试**（参照 `command_test.go` 现有 serve-boot 范式；用 fake/recording MaaS 让任务立即产出结果）— `internal/cli/gateway_contract_test.go`

```go
package cli

// TestTaskSubmitEmitsCompletedEvent verifies the core contract the IM gateway
// depends on: a task submitted through the serve stack executes and surfaces a
// task_completed runtime event carrying the result text, keyed by the task id.
//
// It reuses the existing serve boot pattern in command_test.go. The exact helper
// calls below MUST be adapted to whatever harness command_test.go already uses to
// stand up a coordinator + event bus (do not invent a new one). The ASSERTIONS
// are the deliverable: (a) the task runs, (b) a task_completed event exists for
// its id, (c) event.Message equals the model result.
func TestTaskSubmitEmitsCompletedEvent(t *testing.T) {
	// Arrange: boot the same coordinator + workflowEvents + task store the serve
	// path wires (mirror command_test.go's existing runTUITask / serve harness).
	// Submit a task (recording MaaS returns "hello from model").
	// Act: run the coordinator heartbeat until the task completes.
	// Assert:
	//   found := false
	//   for _, e := range workflowEvents.Events() {
	//       if e.Type == "task_completed" && e.TaskID == taskID {
	//           found = true
	//           if e.Message != "hello from model" {
	//               t.Fatalf("task_completed message = %q, want model result", e.Message)
	//           }
	//       }
	//   }
	//   if !found { t.Fatalf("no task_completed event for %q", taskID) }
	t.Skip("fill in using command_test.go serve harness; assertions above are required")
}
```

- [ ] **Step 3: 落实测试**（去掉 `t.Skip`，按 `command_test.go` 实际 harness 填充 Arrange/Act，保留 Assert）。

- [ ] **Step 4: 跑测试确认通过**
Run: `cd legion/legionAgent && go test ./internal/cli/ -run TestTaskSubmitEmitsCompletedEvent -v`
Expected: PASS（证明契约成立）。若 FAIL 且原因是「API 任务不自动执行」，**停止并上报**——需先补触发路径，本计划其余 Task 依赖此契约。

- [ ] **Step 5: 验证门 + Commit**
```bash
cd legion/legionAgent && go build ./... && go vet ./... && go test ./... && gofmt -l .
git add internal/cli/gateway_contract_test.go
git commit -m "test(gateway): pin core task-submit -> task_completed contract"
```

---

## Task 2：框架核心类型 + PlatformRegistry

**Files:**
- Create: `internal/gateway/adapter.go`
- Create: `internal/gateway/registry.go`
- Test: `internal/gateway/registry_test.go`

**Interfaces:**
- Produces: `InboundMessage`, `DeliveryTarget`, `PlatformConfig`, `ChannelAdapter`（接口）, `PlatformEntry`, `*PlatformRegistry`（`Register`/`Get`/`Build`）。

- [ ] **Step 1: 写失败测试** — `internal/gateway/registry_test.go`

```go
package gateway

import (
	"context"
	"testing"
)

type stubAdapter struct{ name string }

func (a stubAdapter) Platform() string                                                 { return a.name }
func (a stubAdapter) Start(context.Context, chan<- InboundMessage) error               { return nil }
func (a stubAdapter) Send(context.Context, DeliveryTarget, string) error               { return nil }
func (a stubAdapter) Close() error                                                     { return nil }

func TestPlatformRegistryBuildAndUnknown(t *testing.T) {
	reg := NewPlatformRegistry()
	if err := reg.Register(PlatformEntry{
		Platform:         "telegram",
		MaxMessageLength: 4096,
		Factory:          func(PlatformConfig) (ChannelAdapter, error) { return stubAdapter{name: "telegram"}, nil },
	}); err != nil {
		t.Fatalf("Register() error = %v, want nil", err)
	}
	// Duplicate registration fails loud.
	if err := reg.Register(PlatformEntry{Platform: "telegram", Factory: func(PlatformConfig) (ChannelAdapter, error) { return nil, nil }}); err == nil {
		t.Fatalf("Register(dup) error = nil, want non-nil")
	}
	adapter, err := reg.Build("telegram", PlatformConfig{})
	if err != nil || adapter.Platform() != "telegram" {
		t.Fatalf("Build(telegram) = %v, %v, want telegram adapter", adapter, err)
	}
	if _, err := reg.Build("discord", PlatformConfig{}); err == nil {
		t.Fatalf("Build(unknown) error = nil, want non-nil")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestPlatformRegistry -v`
Expected: FAIL（包/类型未定义）

- [ ] **Step 3: 实现 adapter.go**

```go
// Package gateway is Legion's IM multi-channel gateway framework: a
// platform-agnostic adapter model plus routing, session binding, and a core
// API/SSE client, so messaging platforms can be bridged to the agent runtime
// without changing the core.
package gateway

import (
	"context"
	"time"
)

// InboundMessage is a platform-agnostic incoming message pushed by an adapter.
type InboundMessage struct {
	Platform   string
	ChatID     string
	UserID     string
	Text       string
	Images     []string
	ReceivedAt time.Time
}

// DeliveryTarget addresses an outbound reply. Its string form mirrors hermes:
// "<platform>:<chatID>[:<thread>]".
type DeliveryTarget struct {
	Platform string
	ChatID   string
	Thread   string
}

// PlatformConfig is the per-platform runtime configuration handed to an adapter
// factory. Token is the platform credential; PollTimeoutSeconds bounds a long
// poll where applicable.
type PlatformConfig struct {
	Token              string
	PollTimeoutSeconds int
}

// ChannelAdapter is one messaging-platform integration. It is self-driven: Start
// runs its own ingress loop until ctx is cancelled, pushing InboundMessage to
// inbound; Send delivers a reply; Close releases resources.
type ChannelAdapter interface {
	Platform() string
	Start(ctx context.Context, inbound chan<- InboundMessage) error
	Send(ctx context.Context, target DeliveryTarget, text string) error
	Close() error
}
```

- [ ] **Step 4: 实现 registry.go**

```go
package gateway

import "fmt"

// PlatformEntry describes a registered platform: a factory plus metadata used by
// routing (MaxMessageLength) and safety (PIISafe).
type PlatformEntry struct {
	Platform         string
	Factory          func(cfg PlatformConfig) (ChannelAdapter, error)
	MaxMessageLength int
	PIISafe          bool
}

// PlatformRegistry maps a platform name to its entry using the factory pattern,
// so adding a platform is a Register call rather than a change to core logic.
type PlatformRegistry struct {
	entries map[string]PlatformEntry
}

// NewPlatformRegistry returns an empty registry.
func NewPlatformRegistry() *PlatformRegistry {
	return &PlatformRegistry{entries: make(map[string]PlatformEntry)}
}

// Register adds a platform entry. A duplicate platform or a missing factory is a
// programming error reported loudly rather than silently overwritten.
func (r *PlatformRegistry) Register(entry PlatformEntry) error {
	if entry.Platform == "" {
		return fmt.Errorf("register platform: name is required")
	}
	if entry.Factory == nil {
		return fmt.Errorf("register platform %q: factory is required", entry.Platform)
	}
	if _, exists := r.entries[entry.Platform]; exists {
		return fmt.Errorf("register platform %q: already registered", entry.Platform)
	}
	r.entries[entry.Platform] = entry
	return nil
}

// Get returns a platform entry and whether it exists.
func (r *PlatformRegistry) Get(name string) (PlatformEntry, bool) {
	entry, ok := r.entries[name]
	return entry, ok
}

// Build constructs an adapter for a registered platform. An unknown platform is
// an error, not a nil adapter.
func (r *PlatformRegistry) Build(name string, cfg PlatformConfig) (ChannelAdapter, error) {
	entry, ok := r.entries[name]
	if !ok {
		return nil, fmt.Errorf("build platform %q: not registered", name)
	}
	adapter, err := entry.Factory(cfg)
	if err != nil {
		return nil, fmt.Errorf("build platform %q: %w", name, err)
	}
	return adapter, nil
}
```

- [ ] **Step 5: 跑测试确认通过**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestPlatformRegistry -v`
Expected: PASS

- [ ] **Step 6: 验证门 + Commit**
```bash
cd legion/legionAgent && go build ./... && go vet ./... && go test ./... && gofmt -l .
git add internal/gateway/adapter.go internal/gateway/registry.go internal/gateway/registry_test.go
git commit -m "feat(gateway): channel adapter framework + platform registry"
```

---

## Task 3：HashID（PII 脱敏）

**Files:**
- Create: `internal/gateway/hash.go`
- Test: `internal/gateway/hash_test.go`

**Interfaces:**
- Produces: `HashID(raw string) string`。

- [ ] **Step 1: 写失败测试** — `internal/gateway/hash_test.go`

```go
package gateway

import "testing"

func TestHashIDStableAndAnonymized(t *testing.T) {
	a := HashID("123456789")
	b := HashID("123456789")
	if a != b {
		t.Fatalf("HashID not stable: %q vs %q", a, b)
	}
	if a == "123456789" || a == "" {
		t.Fatalf("HashID did not anonymize: %q", a)
	}
	if HashID("123456789") == HashID("987654321") {
		t.Fatalf("HashID collided on distinct inputs")
	}
	if len(a) != 16 {
		t.Fatalf("HashID len = %d, want 16", len(a))
	}
}
```

- [ ] **Step 2: 跑测试确认失败**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestHashID -v`
Expected: FAIL（`HashID` 未定义）

- [ ] **Step 3: 实现 hash.go**

```go
package gateway

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashID anonymizes a raw platform identifier (chat/user id) into a stable,
// non-reversible 16-hex-char token. Only the hash is ever handed to the core, so
// the runtime never sees raw platform ids; the raw value stays in the gateway's
// private binding store for outbound delivery.
func HashID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:16]
}
```

- [ ] **Step 4: 跑测试确认通过**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestHashID -v`
Expected: PASS

- [ ] **Step 5: 验证门 + Commit**
```bash
cd legion/legionAgent && go build ./... && go vet ./... && go test ./... && gofmt -l .
git add internal/gateway/hash.go internal/gateway/hash_test.go
git commit -m "feat(gateway): stable PII-hashing of platform ids"
```

---

## Task 4：DeliveryTracker（task_id → 投递目标）

**Files:**
- Create: `internal/gateway/tracker.go`
- Test: `internal/gateway/tracker_test.go`

**Interfaces:**
- Produces: `*DeliveryTracker`（`NewDeliveryTracker`、`Track(taskID string, target DeliveryTarget)`、`Take(taskID string) (DeliveryTarget, bool)`、`Pending() []string`）。`Take` 命中即移除；`Pending` 返回当前在途 task_id 快照，供轮询环遍历。

- [ ] **Step 1: 写失败测试** — `internal/gateway/tracker_test.go`

```go
package gateway

import (
	"sync"
	"testing"
)

func TestDeliveryTrackerTrackTakeOnce(t *testing.T) {
	tr := NewDeliveryTracker()
	tr.Track("task-1", DeliveryTarget{Platform: "telegram", ChatID: "42"})
	got, ok := tr.Take("task-1")
	if !ok || got.ChatID != "42" {
		t.Fatalf("Take(task-1) = %v, %v, want telegram/42", got, ok)
	}
	// Consumed: a second Take misses.
	if _, ok := tr.Take("task-1"); ok {
		t.Fatalf("Take(task-1) second = ok, want miss after consume")
	}
	// Unknown task misses (legitimate: not a gateway-submitted task).
	if _, ok := tr.Take("other"); ok {
		t.Fatalf("Take(other) = ok, want miss")
	}
}

func TestDeliveryTrackerPendingSnapshot(t *testing.T) {
	tr := NewDeliveryTracker()
	tr.Track("a", DeliveryTarget{Platform: "telegram", ChatID: "1"})
	tr.Track("b", DeliveryTarget{Platform: "telegram", ChatID: "2"})
	pending := tr.Pending()
	if len(pending) != 2 {
		t.Fatalf("Pending() = %v, want 2 ids", pending)
	}
	tr.Take("a")
	if got := tr.Pending(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("Pending() after take = %v, want [b]", got)
	}
}

func TestDeliveryTrackerConcurrent(t *testing.T) {
	tr := NewDeliveryTracker()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "t" + string(rune('a'+i%26))
			tr.Track(id, DeliveryTarget{Platform: "telegram", ChatID: id})
			tr.Take(id)
		}(i)
	}
	wg.Wait()
}
```

- [ ] **Step 2: 跑测试确认失败**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestDeliveryTracker -v`
Expected: FAIL

- [ ] **Step 3: 实现 tracker.go**

```go
package gateway

import "sync"

// DeliveryTracker maps an in-flight task id to the outbound target that should
// receive its result. It is process-local and short-lived: an entry is created
// when a task is submitted and removed when its result is delivered. A restart
// loses in-flight entries (documented tradeoff).
type DeliveryTracker struct {
	mu      sync.Mutex
	targets map[string]DeliveryTarget
}

// NewDeliveryTracker returns an empty tracker.
func NewDeliveryTracker() *DeliveryTracker {
	return &DeliveryTracker{targets: make(map[string]DeliveryTarget)}
}

// Track records the delivery target for a submitted task id.
func (t *DeliveryTracker) Track(taskID string, target DeliveryTarget) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.targets[taskID] = target
}

// Take returns and removes the target for a task id. ok is false when the id is
// not tracked, which is a legitimate case (the completed task was not submitted
// by this gateway) the caller skips rather than treats as an error.
func (t *DeliveryTracker) Take(taskID string) (DeliveryTarget, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	target, ok := t.targets[taskID]
	if ok {
		delete(t.targets, taskID)
	}
	return target, ok
}

// Pending returns a snapshot of the currently in-flight task ids, so the
// completion poller can iterate them without holding the lock during I/O.
func (t *DeliveryTracker) Pending() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]string, 0, len(t.targets))
	for id := range t.targets {
		ids = append(ids, id)
	}
	return ids
}
```

- [ ] **Step 4: 跑测试确认通过**（含 `-race`）
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestDeliveryTracker -race -v`
Expected: PASS

- [ ] **Step 5: 验证门 + Commit**
```bash
cd legion/legionAgent && go build ./... && go vet ./... && go test ./... && gofmt -l .
git add internal/gateway/tracker.go internal/gateway/tracker_test.go
git commit -m "feat(gateway): in-flight task -> delivery target tracker"
```

---

## Task 5：DeliveryRouter（选 adapter + 切分 + Send）

**Files:**
- Create: `internal/gateway/router.go`
- Test: `internal/gateway/router_test.go`

**Interfaces:**
- Consumes: `ChannelAdapter`, `DeliveryTarget`。
- Produces: `*DeliveryRouter`（`NewDeliveryRouter`、`RegisterAdapter(a ChannelAdapter, maxMessageLength int)`、`Route(ctx, target DeliveryTarget, text string) error`）；`splitMessage(text string, max int) []string`。

- [ ] **Step 1: 写失败测试** — `internal/gateway/router_test.go`

```go
package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recordingAdapter struct {
	name string
	sent []string
	err  error
}

func (a *recordingAdapter) Platform() string                                   { return a.name }
func (a *recordingAdapter) Start(context.Context, chan<- InboundMessage) error { return nil }
func (a *recordingAdapter) Close() error                                       { return nil }
func (a *recordingAdapter) Send(_ context.Context, _ DeliveryTarget, text string) error {
	if a.err != nil {
		return a.err
	}
	a.sent = append(a.sent, text)
	return nil
}

func TestDeliveryRouterSplitsAndRoutes(t *testing.T) {
	adapter := &recordingAdapter{name: "telegram"}
	router := NewDeliveryRouter()
	router.RegisterAdapter(adapter, 5) // tiny max to force splitting

	if err := router.Route(context.Background(), DeliveryTarget{Platform: "telegram", ChatID: "1"}, "abcdefghij"); err != nil {
		t.Fatalf("Route() error = %v, want nil", err)
	}
	if len(adapter.sent) != 2 || adapter.sent[0] != "abcde" || adapter.sent[1] != "fghij" {
		t.Fatalf("sent = %v, want two 5-char chunks", adapter.sent)
	}
}

func TestDeliveryRouterUnknownPlatformFailsLoud(t *testing.T) {
	router := NewDeliveryRouter()
	if err := router.Route(context.Background(), DeliveryTarget{Platform: "discord", ChatID: "1"}, "hi"); err == nil {
		t.Fatalf("Route(unknown) error = nil, want non-nil")
	}
}

func TestDeliveryRouterPropagatesSendError(t *testing.T) {
	adapter := &recordingAdapter{name: "telegram", err: errors.New("boom")}
	router := NewDeliveryRouter()
	router.RegisterAdapter(adapter, 4096)
	if err := router.Route(context.Background(), DeliveryTarget{Platform: "telegram", ChatID: "1"}, "hi"); err == nil {
		t.Fatalf("Route(send error) error = nil, want propagated")
	}
}

func TestSplitMessagePrefersNewline(t *testing.T) {
	chunks := splitMessage("aaa\nbbbbb", 5)
	if len(chunks) != 2 || chunks[0] != "aaa" || chunks[1] != "bbbbb" {
		t.Fatalf("splitMessage = %v, want [aaa bbbbb] (split at newline)", chunks)
	}
	if strings.Join(splitMessage("", 5), "") != "" {
		t.Fatalf("splitMessage(empty) non-empty")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run 'TestDeliveryRouter|TestSplitMessage' -v`
Expected: FAIL

- [ ] **Step 3: 实现 router.go**

```go
package gateway

import (
	"context"
	"fmt"
)

// DeliveryRouter sends an outbound reply through the adapter for its target
// platform, splitting over-length text into platform-sized chunks.
type DeliveryRouter struct {
	adapters map[string]ChannelAdapter
	maxLen   map[string]int
}

// NewDeliveryRouter returns an empty router.
func NewDeliveryRouter() *DeliveryRouter {
	return &DeliveryRouter{
		adapters: make(map[string]ChannelAdapter),
		maxLen:   make(map[string]int),
	}
}

// RegisterAdapter binds an adapter and its max message length to its platform.
// maxMessageLength <= 0 disables splitting for that platform.
func (r *DeliveryRouter) RegisterAdapter(a ChannelAdapter, maxMessageLength int) {
	r.adapters[a.Platform()] = a
	r.maxLen[a.Platform()] = maxMessageLength
}

// Route delivers text to target. An unknown platform is a loud error. Over-length
// text is split; the first Send error aborts and is returned wrapped.
func (r *DeliveryRouter) Route(ctx context.Context, target DeliveryTarget, text string) error {
	adapter, ok := r.adapters[target.Platform]
	if !ok {
		return fmt.Errorf("route delivery: platform %q has no adapter", target.Platform)
	}
	for _, chunk := range splitMessage(text, r.maxLen[target.Platform]) {
		if err := adapter.Send(ctx, target, chunk); err != nil {
			return fmt.Errorf("route delivery to %s:%s: %w", target.Platform, target.ChatID, err)
		}
	}
	return nil
}

// splitMessage breaks text into chunks no longer than max runes, preferring to
// break at the last newline within a chunk so replies are not cut mid-line. max
// <= 0 returns text as a single chunk. Empty text yields no chunks.
func splitMessage(text string, max int) []string {
	if text == "" {
		return nil
	}
	if max <= 0 {
		return []string{text}
	}
	runes := []rune(text)
	var chunks []string
	for len(runes) > 0 {
		end := max
		if end > len(runes) {
			end = len(runes)
		}
		if end < len(runes) {
			// Prefer a newline break within [0, end).
			for i := end - 1; i > 0; i-- {
				if runes[i] == '\n' {
					end = i
					break
				}
			}
		}
		chunk := string(runes[:end])
		chunks = append(chunks, chunk)
		// Skip a single boundary newline so it is not re-emitted as a blank chunk.
		if end < len(runes) && runes[end] == '\n' {
			end++
		}
		runes = runes[end:]
	}
	return chunks
}
```

- [ ] **Step 4: 跑测试确认通过**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run 'TestDeliveryRouter|TestSplitMessage' -v`
Expected: PASS

- [ ] **Step 5: 验证门 + Commit**
```bash
cd legion/legionAgent && go build ./... && go vet ./... && go test ./... && gofmt -l .
git add internal/gateway/router.go internal/gateway/router_test.go
git commit -m "feat(gateway): delivery router with length-aware splitting"
```

---

## Task 6：SessionBinder + SQLiteBinder（持久绑定）

**Files:**
- Create: `internal/gateway/binder.go`
- Test: `internal/gateway/binder_test.go`

**Interfaces:**
- Produces: `SessionBinder`（接口：`Resolve(ctx, platformKey) (sessionID, rawChatID string, ok bool, err error)`、`Bind(ctx, platformKey, sessionID, rawChatID string) error`）；`*SQLiteBinder`（`OpenSQLiteBinder(ctx, path) (*SQLiteBinder, error)`、实现接口、`Close() error`）。

- [ ] **Step 1: 写失败测试** — `internal/gateway/binder_test.go`

```go
package gateway

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteBinderBindResolvePersists(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")

	binder, err := OpenSQLiteBinder(ctx, path)
	if err != nil {
		t.Fatalf("OpenSQLiteBinder() error = %v, want nil", err)
	}
	// Miss before bind.
	if _, _, ok, err := binder.Resolve(ctx, "telegram:42"); err != nil || ok {
		t.Fatalf("Resolve(before) = ok %v err %v, want miss", ok, err)
	}
	if err := binder.Bind(ctx, "telegram:42", "session-1", "42"); err != nil {
		t.Fatalf("Bind() error = %v, want nil", err)
	}
	sid, raw, ok, err := binder.Resolve(ctx, "telegram:42")
	if err != nil || !ok || sid != "session-1" || raw != "42" {
		t.Fatalf("Resolve(after) = %q %q %v %v, want session-1/42/true", sid, raw, ok, err)
	}
	if err := binder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	// Reopen: binding survives (persistence).
	reopened, err := OpenSQLiteBinder(ctx, path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	sid, _, ok, err = reopened.Resolve(ctx, "telegram:42")
	if err != nil || !ok || sid != "session-1" {
		t.Fatalf("Resolve(reopened) = %q %v %v, want session-1/true", sid, ok, err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestSQLiteBinder -v`
Expected: FAIL

- [ ] **Step 3: 实现 binder.go**

```go
package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// SessionBinder maps a platform conversation key ("<platform>:<chatID>") to the
// Legion session it drives, plus the raw chat id needed for outbound delivery.
// The raw id lives only here, never in the core.
type SessionBinder interface {
	Resolve(ctx context.Context, platformKey string) (sessionID string, rawChatID string, ok bool, err error)
	Bind(ctx context.Context, platformKey string, sessionID string, rawChatID string) error
}

// SQLiteBinder is a SQLite-backed SessionBinder so bindings (and thus per-chat
// conversation continuity) survive a gateway restart.
type SQLiteBinder struct {
	db *sql.DB
}

// OpenSQLiteBinder opens (creating if needed) the binding database at path.
func OpenSQLiteBinder(ctx context.Context, path string) (*SQLiteBinder, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open gateway sqlite %q: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping gateway sqlite %q: %w", path, err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS channel_bindings (
			platform_key TEXT PRIMARY KEY,
			session_id   TEXT NOT NULL,
			raw_chat_id  TEXT NOT NULL,
			created_at   TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate channel_bindings: %w", err)
	}
	return &SQLiteBinder{db: db}, nil
}

// Resolve returns the binding for platformKey. ok is false with a nil error when
// no binding exists yet — a legitimate first-contact state the caller handles by
// creating a session.
func (b *SQLiteBinder) Resolve(ctx context.Context, platformKey string) (string, string, bool, error) {
	var sessionID, rawChatID string
	err := b.db.QueryRowContext(ctx, `
		SELECT session_id, raw_chat_id FROM channel_bindings WHERE platform_key = ?
	`, platformKey).Scan(&sessionID, &rawChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("resolve binding %q: %w", platformKey, err)
	}
	return sessionID, rawChatID, true, nil
}

// Bind stores (or replaces) the binding for platformKey.
func (b *SQLiteBinder) Bind(ctx context.Context, platformKey, sessionID, rawChatID string) error {
	if _, err := b.db.ExecContext(ctx, `
		INSERT INTO channel_bindings (platform_key, session_id, raw_chat_id)
		VALUES (?, ?, ?)
		ON CONFLICT(platform_key) DO UPDATE SET
			session_id = excluded.session_id,
			raw_chat_id = excluded.raw_chat_id
	`, platformKey, sessionID, rawChatID); err != nil {
		return fmt.Errorf("bind %q: %w", platformKey, err)
	}
	return nil
}

// Close releases the database.
func (b *SQLiteBinder) Close() error {
	if err := b.db.Close(); err != nil {
		return fmt.Errorf("close gateway sqlite: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认通过**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestSQLiteBinder -v`
Expected: PASS

- [ ] **Step 5: 验证门 + Commit**
```bash
cd legion/legionAgent && go build ./... && go vet ./... && go test ./... && gofmt -l .
git add internal/gateway/binder.go internal/gateway/binder_test.go
git commit -m "feat(gateway): sqlite-backed session binder"
```

---

## Task 7：GatewayConfig + Load（JSON + env 密钥 + 校验）

**Files:**
- Create: `internal/gateway/config.go`
- Test: `internal/gateway/config_test.go`

**Interfaces:**
- Produces: `GatewayConfig`（含 `Core{BaseURL, Token}`、`Identity{AgentID, CompanyID}`、`Binding{SQLitePath}`、`Delivery{Retries, BackoffMS}`、`Platforms map[string]PlatformSettings`，`PlatformSettings{Enabled bool, Token string, PollTimeoutSeconds int}`）；`Load(path string) (GatewayConfig, error)`。Token 字段在 Load 后已从 env 解析出明文。

- [ ] **Step 1: 写失败测试** — `internal/gateway/config_test.go`

```go
package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadResolvesEnvSecretsAndValidates(t *testing.T) {
	t.Setenv("TEST_ADMIN", "admintok")
	t.Setenv("TEST_TG", "bottok")
	path := writeConfig(t, `{
		"core":     {"base_url":"http://x:8080","token_env":"TEST_ADMIN"},
		"identity": {"agent_id":"im-agent","company_id":"default"},
		"binding":  {"sqlite_path":"g.db"},
		"delivery": {"retries":3,"backoff_ms":500},
		"platforms": {"telegram":{"enabled":true,"token_env":"TEST_TG","poll_timeout_s":30}}
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.Core.Token != "admintok" || cfg.Platforms["telegram"].Token != "bottok" {
		t.Fatalf("secrets not resolved from env: core=%q tg=%q", cfg.Core.Token, cfg.Platforms["telegram"].Token)
	}
	if cfg.Identity.AgentID != "im-agent" {
		t.Fatalf("identity.agent_id = %q, want im-agent", cfg.Identity.AgentID)
	}
}

func TestLoadFailsLoudOnMissingEnvSecret(t *testing.T) {
	path := writeConfig(t, `{
		"core":{"base_url":"http://x","token_env":"MISSING_ENV"},
		"identity":{"agent_id":"a","company_id":"c"},
		"binding":{"sqlite_path":"g.db"},
		"platforms":{}
	}`)
	if _, err := Load(path); err == nil {
		t.Fatalf("Load(missing env) error = nil, want non-nil")
	}
}

func TestLoadFailsLoudOnMissingCoreURL(t *testing.T) {
	t.Setenv("TEST_ADMIN", "x")
	path := writeConfig(t, `{
		"core":{"base_url":"","token_env":"TEST_ADMIN"},
		"identity":{"agent_id":"a","company_id":"c"},
		"binding":{"sqlite_path":"g.db"},
		"platforms":{}
	}`)
	if _, err := Load(path); err == nil {
		t.Fatalf("Load(no core url) error = nil, want non-nil")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestLoad -v`
Expected: FAIL

- [ ] **Step 3: 实现 config.go**

```go
package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// GatewayConfig is the gateway's runtime configuration, loaded from a JSON file
// with secrets resolved from environment variables (never stored in the file).
type GatewayConfig struct {
	Core      CoreConfig                  `json:"core"`
	Identity  IdentityConfig              `json:"identity"`
	Binding   BindingConfig               `json:"binding"`
	Delivery  DeliveryConfig              `json:"delivery"`
	Platforms map[string]PlatformSettings `json:"platforms"`
}

// CoreConfig points at the Legion core API. Token is resolved from TokenEnv.
type CoreConfig struct {
	BaseURL  string `json:"base_url"`
	TokenEnv string `json:"token_env"`
	Token    string `json:"-"`
}

// IdentityConfig is the agent/company the gateway submits IM tasks as.
type IdentityConfig struct {
	AgentID   string `json:"agent_id"`
	CompanyID string `json:"company_id"`
}

// BindingConfig configures the binding store.
type BindingConfig struct {
	SQLitePath string `json:"sqlite_path"`
}

// DeliveryConfig bounds outbound retry behavior.
type DeliveryConfig struct {
	Retries   int `json:"retries"`
	BackoffMS int `json:"backoff_ms"`
}

// PlatformSettings is one platform's config. Token is resolved from TokenEnv.
type PlatformSettings struct {
	Enabled            bool   `json:"enabled"`
	TokenEnv           string `json:"token_env"`
	PollTimeoutSeconds int    `json:"poll_timeout_s"`
	Token              string `json:"-"`
}

// Load reads the gateway config file, resolves env secrets, and validates
// required fields. Missing file, malformed JSON, an unset secret env var, or a
// missing core URL/identity is a fatal configuration error reported loudly.
func Load(path string) (GatewayConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return GatewayConfig{}, fmt.Errorf("read gateway config %q: %w", path, err)
	}
	var cfg GatewayConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return GatewayConfig{}, fmt.Errorf("parse gateway config %q: %w", path, err)
	}
	if strings.TrimSpace(cfg.Core.BaseURL) == "" {
		return GatewayConfig{}, fmt.Errorf("gateway config: core.base_url is required")
	}
	if strings.TrimSpace(cfg.Identity.AgentID) == "" || strings.TrimSpace(cfg.Identity.CompanyID) == "" {
		return GatewayConfig{}, fmt.Errorf("gateway config: identity.agent_id and company_id are required")
	}
	token, err := readEnvSecret(cfg.Core.TokenEnv)
	if err != nil {
		return GatewayConfig{}, fmt.Errorf("gateway config core token: %w", err)
	}
	cfg.Core.Token = token
	for name, p := range cfg.Platforms {
		if !p.Enabled {
			continue
		}
		token, err := readEnvSecret(p.TokenEnv)
		if err != nil {
			return GatewayConfig{}, fmt.Errorf("gateway config platform %q token: %w", name, err)
		}
		p.Token = token
		cfg.Platforms[name] = p
	}
	return cfg, nil
}

// readEnvSecret returns the value of env var name, failing loud when the name is
// empty or the variable is unset — secrets must never silently default to empty.
func readEnvSecret(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("token_env is required")
	}
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		return "", fmt.Errorf("env %q is not set", name)
	}
	return value, nil
}
```

- [ ] **Step 4: 跑测试确认通过**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestLoad -v`
Expected: PASS

- [ ] **Step 5: 验证门 + Commit**
```bash
cd legion/legionAgent && go build ./... && go vet ./... && go test ./... && gofmt -l .
git add internal/gateway/config.go internal/gateway/config_test.go
git commit -m "feat(gateway): config loader with env-resolved secrets and validation"
```

---

## Task 8：CoreClient（HTTP：sessions / tasks / result 轮询）

**Files:**
- Create: `internal/gateway/core_client.go`
- Test: `internal/gateway/core_client_test.go`

**Interfaces:**
- Produces: `SessionReq{CompanyID, AgentID, Project, Title string}`；`TaskReq{ID, Input, CompanyID, AgentID, SessionID string; Images []string}`；`CoreClient`（接口：`EnsureSession(ctx, SessionReq) (string, error)`、`SubmitTask(ctx, TaskReq) (string, error)`、`TaskResult(ctx, taskID string) (text string, done bool, err error)`）；`NewHTTPCoreClient(baseURL, token string) *HTTPCoreClient`（实现接口）。
- `TaskResult` 语义：`GET /v1/tasks/{id}/result` → `{status, result}`；`done` = `status ∈ {done, failed, suspended}`（终态），`text` = `result`（仅 `done` 时非空）。

- [ ] **Step 1: 写失败测试** — `internal/gateway/core_client_test.go`

```go
package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPCoreClientEnsureSessionAndSubmitTask(t *testing.T) {
	ctx := context.Background()
	var gotAuth, gotSessionPath, gotTaskPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/v1/sessions":
			gotSessionPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"session-99"}`))
		case "/v1/tasks":
			gotTaskPath = r.URL.Path
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"task-1"}`))
		default:
			http.Error(w, "no", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client := NewHTTPCoreClient(server.URL, "admintok")
	sid, err := client.EnsureSession(ctx, SessionReq{CompanyID: "c", AgentID: "a", Project: "telegram", Title: "telegram:hash"})
	if err != nil || sid != "session-99" {
		t.Fatalf("EnsureSession = %q, %v, want session-99", sid, err)
	}
	tid, err := client.SubmitTask(ctx, TaskReq{ID: "task-1", Input: "hi", SessionID: "session-99", CompanyID: "c", AgentID: "a"})
	if err != nil || tid != "task-1" {
		t.Fatalf("SubmitTask = %q, %v, want task-1", tid, err)
	}
	if gotAuth != "Bearer admintok" || gotSessionPath != "/v1/sessions" || gotTaskPath != "/v1/tasks" {
		t.Fatalf("auth/paths = %q %q %q", gotAuth, gotSessionPath, gotTaskPath)
	}
}

func TestHTTPCoreClientTaskResultDoneAndPending(t *testing.T) {
	ctx := context.Background()
	var status string // flip between calls
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/tasks/t1/result" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"task_id":"t1","status":%q,"result":"answer text"}`, status)
			return
		}
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	client := NewHTTPCoreClient(server.URL, "tok")

	// Not terminal yet → done=false.
	status = "running"
	text, done, err := client.TaskResult(ctx, "t1")
	if err != nil || done {
		t.Fatalf("TaskResult(running) = %q,%v,%v, want done=false", text, done, err)
	}
	// Terminal → done=true with result text.
	status = "done"
	text, done, err = client.TaskResult(ctx, "t1")
	if err != nil || !done || text != "answer text" {
		t.Fatalf("TaskResult(done) = %q,%v,%v, want answer text/done", text, done, err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestHTTPCoreClient -v`
Expected: FAIL

- [ ] **Step 3: 实现 core_client.go**

```go
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SessionReq is the body for POST /v1/sessions.
type SessionReq struct {
	CompanyID string `json:"company_id"`
	AgentID   string `json:"agent_id"`
	Project   string `json:"project"`
	Title     string `json:"title"`
}

// TaskReq is the body for POST /v1/tasks. ID is caller-minted and required.
type TaskReq struct {
	ID        string   `json:"id"`
	Input     string   `json:"input"`
	CompanyID string   `json:"company_id"`
	AgentID   string   `json:"agent_id"`
	SessionID string   `json:"session_id"`
	Images    []string `json:"images,omitempty"`
}

// CoreClient talks to the Legion core over its HTTP API.
type CoreClient interface {
	EnsureSession(ctx context.Context, req SessionReq) (sessionID string, err error)
	SubmitTask(ctx context.Context, req TaskReq) (taskID string, err error)
	TaskResult(ctx context.Context, taskID string) (text string, done bool, err error)
}

// HTTPCoreClient is the HTTP/SSE implementation of CoreClient.
type HTTPCoreClient struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPCoreClient builds a client for the core at baseURL authenticating with
// the given bearer token.
func NewHTTPCoreClient(baseURL, token string) *HTTPCoreClient {
	return &HTTPCoreClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *HTTPCoreClient) postJSON(ctx context.Context, path string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode %s body: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned %s: %s", path, resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// EnsureSession creates a session and returns its id.
func (c *HTTPCoreClient) EnsureSession(ctx context.Context, req SessionReq) (string, error) {
	data, err := c.postJSON(ctx, "/v1/sessions", req)
	if err != nil {
		return "", fmt.Errorf("ensure session: %w", err)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode session response: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("ensure session: response had empty id")
	}
	return out.ID, nil
}

// SubmitTask submits a task and returns its id.
func (c *HTTPCoreClient) SubmitTask(ctx context.Context, req TaskReq) (string, error) {
	data, err := c.postJSON(ctx, "/v1/tasks", req)
	if err != nil {
		return "", fmt.Errorf("submit task: %w", err)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode task response: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("submit task: response had empty id")
	}
	return out.ID, nil
}

// TaskResult fetches a task's current result. done is true once the task reaches
// a terminal status (done/failed/suspended); text is the answer, non-empty only
// on a successful completion. A not-yet-terminal task returns done=false with no
// error so the poller retries.
func (c *HTTPCoreClient) TaskResult(ctx context.Context, taskID string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/tasks/"+taskID+"/result", nil)
	if err != nil {
		return "", false, fmt.Errorf("build task result request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("call task result %q: %w", taskID, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false, fmt.Errorf("task result %q returned %s: %s", taskID, resp.Status, strings.TrimSpace(string(data)))
	}
	var out struct {
		Status string `json:"status"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", false, fmt.Errorf("decode task result %q: %w", taskID, err)
	}
	switch out.Status {
	case "done", "failed", "suspended":
		return out.Result, true, nil
	default:
		return "", false, nil
	}
}
```

- [ ] **Step 4: 跑测试确认通过**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestHTTPCoreClient -v`
Expected: PASS

- [ ] **Step 5: 验证门 + Commit**
```bash
cd legion/legionAgent && go build ./... && go vet ./... && go test ./... && gofmt -l .
git add internal/gateway/core_client.go internal/gateway/core_client_test.go
git commit -m "feat(gateway): core HTTP client (sessions, tasks, result polling)"
```

---

## Task 9：Telegram 适配器（getUpdates 轮询 + sendMessage）

**Files:**
- Create: `internal/gateway/platforms/telegram/adapter.go`
- Test: `internal/gateway/platforms/telegram/adapter_test.go`

**Interfaces:**
- Consumes: `gateway.ChannelAdapter`, `gateway.InboundMessage`, `gateway.DeliveryTarget`, `gateway.PlatformConfig`。
- Produces: `New(cfg gateway.PlatformConfig) (gateway.ChannelAdapter, error)`；内部 `Adapter` 有可注入 `baseURL`（测试用 httptest 覆盖 `https://api.telegram.org/bot<token>`）。

- [ ] **Step 1: 写失败测试** — `internal/gateway/platforms/telegram/adapter_test.go`

```go
package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/gateway"
)

func TestTelegramStartReceivesUpdate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "getUpdates") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":10,"message":{"chat":{"id":42},"from":{"id":7},"text":"hi"}}]}`))
			return
		}
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	adapter := newForTest("tok", server.URL, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inbound := make(chan gateway.InboundMessage, 1)
	go func() { _ = adapter.Start(ctx, inbound) }()

	select {
	case msg := <-inbound:
		if msg.Platform != "telegram" || msg.ChatID != "42" || msg.Text != "hi" {
			t.Fatalf("inbound = %+v, want telegram/42/hi", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no inbound message received")
	}
}

func TestTelegramSendPostsMessage(t *testing.T) {
	var gotChatID, gotText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "sendMessage") {
			var body struct {
				ChatID any    `json:"chat_id"`
				Text   string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotChatID, gotText = toStr(body.ChatID), body.Text
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	adapter := newForTest("tok", server.URL, 0)
	if err := adapter.Send(context.Background(), gateway.DeliveryTarget{Platform: "telegram", ChatID: "42"}, "hello"); err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}
	if gotChatID != "42" || gotText != "hello" {
		t.Fatalf("sendMessage got chat=%q text=%q, want 42/hello", gotChatID, gotText)
	}
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strings.TrimSuffix(strings.TrimSuffix(jsonNumber(t), "0"), ".")
	default:
		return ""
	}
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
```

- [ ] **Step 2: 跑测试确认失败**
Run: `cd legion/legionAgent && go test ./internal/gateway/platforms/telegram/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 adapter.go**

```go
// Package telegram is the Telegram ChannelAdapter for the Legion IM gateway. It
// ingests messages via getUpdates long polling and delivers replies via
// sendMessage, keeping the gateway fully outbound (no public webhook needed).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/stardust/legion-agent/internal/gateway"
)

const defaultPollTimeoutSeconds = 30

// Adapter implements gateway.ChannelAdapter for Telegram.
type Adapter struct {
	token       string
	baseURL     string // https://api.telegram.org/bot<token>
	pollTimeout int
	client      *http.Client
	offset      int64
}

// New builds a Telegram adapter from platform config. A missing token is a loud
// error. Satisfies gateway's factory signature.
func New(cfg gateway.PlatformConfig) (gateway.ChannelAdapter, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("telegram: token is required")
	}
	timeout := cfg.PollTimeoutSeconds
	if timeout <= 0 {
		timeout = defaultPollTimeoutSeconds
	}
	return &Adapter{
		token:       cfg.Token,
		baseURL:     "https://api.telegram.org/bot" + cfg.Token,
		pollTimeout: timeout,
		client:      &http.Client{Timeout: time.Duration(timeout+10) * time.Second},
	}, nil
}

// newForTest builds an adapter pointed at a stub base URL for tests.
func newForTest(token, baseURL string, pollTimeout int) *Adapter {
	if pollTimeout <= 0 {
		pollTimeout = 1
	}
	return &Adapter{
		token:       token,
		baseURL:     baseURL,
		pollTimeout: pollTimeout,
		client:      &http.Client{Timeout: 5 * time.Second},
	}
}

// Platform returns "telegram".
func (a *Adapter) Platform() string { return "telegram" }

// Close is a no-op; the HTTP client needs no teardown.
func (a *Adapter) Close() error { return nil }

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Text string `json:"text"`
	} `json:"message"`
}

type tgUpdatesResponse struct {
	OK     bool       `json:"ok"`
	Result []tgUpdate `json:"result"`
}

// Start runs the getUpdates long-poll loop until ctx is cancelled, pushing each
// text message to inbound. The update offset advances past processed updates so
// each is delivered once. A transient HTTP error is not fatal — it is surfaced
// as a returned error only on ctx cancellation; per-iteration failures back off
// briefly and retry.
func (a *Adapter) Start(ctx context.Context, inbound chan<- gateway.InboundMessage) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		updates, err := a.getUpdates(ctx)
		if err != nil {
			// Transient: back off and retry unless cancelled.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
				continue
			}
		}
		for _, u := range updates {
			if u.UpdateID >= a.offset {
				a.offset = u.UpdateID + 1
			}
			if u.Message.Text == "" {
				continue
			}
			msg := gateway.InboundMessage{
				Platform:   "telegram",
				ChatID:     strconv.FormatInt(u.Message.Chat.ID, 10),
				UserID:     strconv.FormatInt(u.Message.From.ID, 10),
				Text:       u.Message.Text,
				ReceivedAt: time.Now(),
			}
			select {
			case inbound <- msg:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func (a *Adapter) getUpdates(ctx context.Context) ([]tgUpdate, error) {
	url := fmt.Sprintf("%s/getUpdates?timeout=%d&offset=%d", a.baseURL, a.pollTimeout, a.offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram getUpdates request: %w", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram getUpdates: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram getUpdates status %s: %s", resp.Status, string(data))
	}
	var parsed tgUpdatesResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("telegram getUpdates decode: %w", err)
	}
	if !parsed.OK {
		return nil, fmt.Errorf("telegram getUpdates: ok=false")
	}
	return parsed.Result, nil
}

// Send delivers text to the target chat via sendMessage.
func (a *Adapter) Send(ctx context.Context, target gateway.DeliveryTarget, text string) error {
	body, err := json.Marshal(map[string]any{"chat_id": target.ChatID, "text": text})
	if err != nil {
		return fmt.Errorf("telegram sendMessage encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram sendMessage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram sendMessage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("telegram sendMessage status %s: %s", resp.Status, string(data))
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认通过**
Run: `cd legion/legionAgent && go test ./internal/gateway/platforms/telegram/ -v`
Expected: PASS

- [ ] **Step 5: 验证门 + Commit**
```bash
cd legion/legionAgent && go build ./... && go vet ./... && go test ./... && gofmt -l .
git add internal/gateway/platforms/telegram/
git commit -m "feat(gateway): telegram adapter (getUpdates poll + sendMessage)"
```

---

## Task 10：GatewayRunner（编排：入站流 + 投递环）

**Files:**
- Create: `internal/gateway/runner.go`
- Test: `internal/gateway/runner_test.go`

**Interfaces:**
- Consumes: `ChannelAdapter`, `*DeliveryRouter`, `SessionBinder`, `*DeliveryTracker`, `CoreClient`（`TaskResult`）, `GatewayConfig`, `HashID`。
- Produces: `*GatewayRunner`（`NewGatewayRunner(cfg, core, binder, router, tracker, adapters, logger)`、`Run(ctx) error`、导出以便测试的 `HandleInbound(ctx, InboundMessage) error` 与 `PollOnce(ctx) error`）。任务 id 由 `mintTaskID(platform, chatID)` 生成。完成投递为**轮询环**：`pollLoop` 定时调 `PollOnce`，`PollOnce` 遍历 `tracker.Pending()`，对每个 `core.TaskResult` 终态者 `Take` + `router.Route`。

- [ ] **Step 1: 写失败测试** — `internal/gateway/runner_test.go`

```go
package gateway

import (
	"context"
	"log/slog"
	"testing"
)

type fakeResult struct {
	text string
	done bool
}

type fakeCore struct {
	ensured   SessionReq
	submitted TaskReq
	sessionID string
	taskID    string
	results   map[string]fakeResult // taskID -> result for TaskResult
}

func (c *fakeCore) EnsureSession(_ context.Context, req SessionReq) (string, error) {
	c.ensured = req
	return c.sessionID, nil
}
func (c *fakeCore) SubmitTask(_ context.Context, req TaskReq) (string, error) {
	c.submitted = req
	return c.taskID, nil
}
func (c *fakeCore) TaskResult(_ context.Context, taskID string) (string, bool, error) {
	r, ok := c.results[taskID]
	if !ok {
		return "", false, nil
	}
	return r.text, r.done, nil
}

type memBinder struct{ m map[string][2]string }

func (b *memBinder) Resolve(_ context.Context, k string) (string, string, bool, error) {
	v, ok := b.m[k]
	return v[0], v[1], ok, nil
}
func (b *memBinder) Bind(_ context.Context, k, sid, raw string) error {
	b.m[k] = [2]string{sid, raw}
	return nil
}

func TestHandleInboundBindsHashedSessionAndTracks(t *testing.T) {
	core := &fakeCore{sessionID: "session-1", taskID: "task-1"}
	binder := &memBinder{m: map[string][2]string{}}
	tracker := NewDeliveryTracker()
	runner := NewGatewayRunner(
		GatewayConfig{Identity: IdentityConfig{AgentID: "im", CompanyID: "co"}},
		core, binder, NewDeliveryRouter(), tracker, nil, slog.Default(),
	)

	msg := InboundMessage{Platform: "telegram", ChatID: "42", UserID: "7", Text: "hi"}
	if err := runner.HandleInbound(context.Background(), msg); err != nil {
		t.Fatalf("HandleInbound() error = %v, want nil", err)
	}
	// Session created with hashed id in the title, never the raw chat id.
	if core.ensured.Title == "" || core.ensured.Title == "telegram:42" {
		t.Fatalf("session title = %q, want hashed (not raw 42)", core.ensured.Title)
	}
	// Task submitted against the session with the message text.
	if core.submitted.SessionID != "session-1" || core.submitted.Input != "hi" {
		t.Fatalf("submitted = %+v, want session-1/hi", core.submitted)
	}
	// Delivery target tracked with the RAW chat id (for outbound).
	target, ok := tracker.Take(core.submitted.ID)
	if !ok || target.ChatID != "42" {
		t.Fatalf("tracked target = %v, %v, want raw chat 42", target, ok)
	}
	// Binding stored under the platform key.
	if _, _, ok, _ := binder.Resolve(context.Background(), "telegram:42"); !ok {
		t.Fatalf("binding not stored for telegram:42")
	}
}

func TestHandleInboundReusesExistingSession(t *testing.T) {
	core := &fakeCore{sessionID: "SHOULD-NOT-BE-USED", taskID: "task-2"}
	binder := &memBinder{m: map[string][2]string{"telegram:42": {"session-existing", "42"}}}
	runner := NewGatewayRunner(
		GatewayConfig{Identity: IdentityConfig{AgentID: "im", CompanyID: "co"}},
		core, binder, NewDeliveryRouter(), NewDeliveryTracker(), nil, slog.Default(),
	)
	if err := runner.HandleInbound(context.Background(), InboundMessage{Platform: "telegram", ChatID: "42", Text: "again"}); err != nil {
		t.Fatalf("HandleInbound() error = %v", err)
	}
	if core.submitted.SessionID != "session-existing" {
		t.Fatalf("submitted session = %q, want reused session-existing", core.submitted.SessionID)
	}
	if core.ensured.AgentID != "" {
		t.Fatalf("EnsureSession called for an already-bound chat")
	}
}

func TestPollOnceDeliversTerminalTasks(t *testing.T) {
	adapter := &recordingAdapter{name: "telegram"}
	router := NewDeliveryRouter()
	router.RegisterAdapter(adapter, 4096)
	tracker := NewDeliveryTracker()
	tracker.Track("task-1", DeliveryTarget{Platform: "telegram", ChatID: "42"})
	tracker.Track("task-2", DeliveryTarget{Platform: "telegram", ChatID: "43"})
	core := &fakeCore{results: map[string]fakeResult{
		"task-1": {text: "answer one", done: true},
		"task-2": {done: false}, // still running
	}}
	runner := NewGatewayRunner(GatewayConfig{}, core, &memBinder{m: map[string][2]string{}}, router, tracker, nil, slog.Default())

	if err := runner.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce() error = %v, want nil", err)
	}
	// task-1 terminal → delivered and removed; task-2 still pending.
	if len(adapter.sent) != 1 || adapter.sent[0] != "answer one" {
		t.Fatalf("sent = %v, want [answer one]", adapter.sent)
	}
	if pending := tracker.Pending(); len(pending) != 1 || pending[0] != "task-2" {
		t.Fatalf("pending = %v, want [task-2]", pending)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run 'TestHandleInbound|TestPollOnce' -v`
Expected: FAIL

- [ ] **Step 3: 实现 runner.go**

```go
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// GatewayRunner wires adapters, session binding, task submission, and outbound
// delivery into the running gateway. It owns three concurrent loops: each
// adapter's ingress, an inbound worker, and the SSE-driven delivery loop.
type GatewayRunner struct {
	cfg      GatewayConfig
	core     CoreClient
	binder   SessionBinder
	router   *DeliveryRouter
	tracker  *DeliveryTracker
	adapters []ChannelAdapter
	logger   *slog.Logger
	seq      uint64
	seqMu    sync.Mutex
}

// NewGatewayRunner assembles a runner from its collaborators.
func NewGatewayRunner(cfg GatewayConfig, core CoreClient, binder SessionBinder, router *DeliveryRouter, tracker *DeliveryTracker, adapters []ChannelAdapter, logger *slog.Logger) *GatewayRunner {
	if logger == nil {
		logger = slog.Default()
	}
	return &GatewayRunner{cfg: cfg, core: core, binder: binder, router: router, tracker: tracker, adapters: adapters, logger: logger}
}

// Run starts every adapter's ingress, an inbound worker, and the delivery loop,
// blocking until ctx is cancelled. A fatal error from any loop cancels the rest
// and is returned wrapped.
func (g *GatewayRunner) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	inbound := make(chan InboundMessage, 64)

	var wg sync.WaitGroup
	errCh := make(chan error, len(g.adapters)+2)

	for _, adapter := range g.adapters {
		wg.Add(1)
		go func(a ChannelAdapter) {
			defer wg.Done()
			if err := a.Start(ctx, inbound); err != nil {
				errCh <- fmt.Errorf("adapter %q start: %w", a.Platform(), err)
				cancel()
			}
		}(adapter)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		g.inboundWorker(ctx, inbound)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		g.pollLoop(ctx)
	}()

	<-ctx.Done()
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		return err
	}
	return nil
}

func (g *GatewayRunner) inboundWorker(ctx context.Context, inbound <-chan InboundMessage) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-inbound:
			if err := g.HandleInbound(ctx, msg); err != nil {
				g.logger.Error("handle inbound message",
					"platform", msg.Platform, "chat", HashID(msg.ChatID), "err", err)
			}
		}
	}
}

// HandleInbound turns one inbound message into a Legion task: resolve-or-create
// the session for its chat (Legion sees only the hashed id), submit the task,
// and track the delivery target keyed by the minted task id.
func (g *GatewayRunner) HandleInbound(ctx context.Context, msg InboundMessage) error {
	key := msg.Platform + ":" + msg.ChatID
	sessionID, _, ok, err := g.binder.Resolve(ctx, key)
	if err != nil {
		return fmt.Errorf("resolve binding %q: %w", key, err)
	}
	if !ok {
		sessionID, err = g.core.EnsureSession(ctx, SessionReq{
			CompanyID: g.cfg.Identity.CompanyID,
			AgentID:   g.cfg.Identity.AgentID,
			Project:   msg.Platform,
			Title:     msg.Platform + ":" + HashID(msg.ChatID),
		})
		if err != nil {
			return fmt.Errorf("ensure session for %q: %w", key, err)
		}
		if err := g.binder.Bind(ctx, key, sessionID, msg.ChatID); err != nil {
			return fmt.Errorf("bind session for %q: %w", key, err)
		}
	}
	taskID := g.mintTaskID(msg.Platform, msg.ChatID)
	if _, err := g.core.SubmitTask(ctx, TaskReq{
		ID:        taskID,
		Input:     msg.Text,
		CompanyID: g.cfg.Identity.CompanyID,
		AgentID:   g.cfg.Identity.AgentID,
		SessionID: sessionID,
		Images:    msg.Images,
	}); err != nil {
		return fmt.Errorf("submit task for %q: %w", key, err)
	}
	g.tracker.Track(taskID, DeliveryTarget{Platform: msg.Platform, ChatID: msg.ChatID})
	return nil
}

// pollInterval bounds how often the gateway checks its in-flight tasks for
// completion. Small enough for responsive replies, large enough to avoid
// hammering the core.
const pollInterval = 2 * time.Second

// pollLoop delivers completed tasks on a fixed interval until ctx is cancelled.
func (g *GatewayRunner) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := g.PollOnce(ctx); err != nil {
				g.logger.Warn("delivery poll pass", "err", err)
			}
		}
	}
}

// PollOnce checks every in-flight task once. A task that has reached a terminal
// status has its result delivered to the tracked target and is removed from the
// tracker; non-terminal tasks are left for the next pass. A per-task result-fetch
// or delivery error is logged at the loop boundary and does not abort the pass —
// the task is retried on the next tick.
func (g *GatewayRunner) PollOnce(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, taskID := range g.tracker.Pending() {
		text, done, err := g.core.TaskResult(ctx, taskID)
		if err != nil {
			g.logger.Warn("fetch task result", "task", taskID, "err", err)
			continue
		}
		if !done {
			continue
		}
		target, ok := g.tracker.Take(taskID)
		if !ok {
			continue // taken by a concurrent pass
		}
		if text == "" {
			continue // terminal but no answer (e.g. failed) — nothing to deliver
		}
		if err := g.router.Route(ctx, target, text); err != nil {
			g.logger.Error("deliver reply",
				"platform", target.Platform, "chat", HashID(target.ChatID), "task", taskID, "err", err)
		}
	}
	return nil
}

// mintTaskID builds a process-unique task id from platform + hashed chat + a
// monotonic counter, so submitted tasks never collide and carry no raw id.
func (g *GatewayRunner) mintTaskID(platform, chatID string) string {
	g.seqMu.Lock()
	g.seq++
	seq := g.seq
	g.seqMu.Unlock()
	return fmt.Sprintf("%s-%s-%d", platform, HashID(chatID), seq)
}
```

- [ ] **Step 4: 跑测试确认通过**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run 'TestHandleInbound|TestPollOnce' -v`
Expected: PASS

- [ ] **Step 5: 验证门 + Commit**
```bash
cd legion/legionAgent && go build ./... && go vet ./... && go test ./... && gofmt -l .
git add internal/gateway/runner.go internal/gateway/runner_test.go
git commit -m "feat(gateway): runner orchestrating inbound flow + result-polling delivery"
```

---

## Task 11：cmd/gateway 装配 + 部署清单

**Files:**
- Create: `cmd/gateway/main.go`
- Create: `configs/gateway.example.json`
- Create: `deploy/docker-compose.gateway.yml`
- Test: `internal/gateway/wiring_test.go`

**Interfaces:**
- Consumes: 全部 gateway 单元 + `telegram.New`。
- Produces: `BuildAdapters(cfg GatewayConfig, reg *PlatformRegistry) ([]ChannelAdapter, *DeliveryRouter, error)`（放在 `internal/gateway/wiring.go`，供 main 与测试复用）。

- [ ] **Step 1: 写失败测试** — `internal/gateway/wiring_test.go`

```go
package gateway

import (
	"context"
	"testing"
)

func TestBuildAdaptersRegistersEnabledPlatforms(t *testing.T) {
	reg := NewPlatformRegistry()
	if err := reg.Register(PlatformEntry{
		Platform:         "telegram",
		MaxMessageLength: 4096,
		Factory:          func(PlatformConfig) (ChannelAdapter, error) { return stubAdapter{name: "telegram"}, nil },
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	cfg := GatewayConfig{Platforms: map[string]PlatformSettings{
		"telegram": {Enabled: true, Token: "tok", PollTimeoutSeconds: 30},
		"discord":  {Enabled: false},
	}}
	adapters, router, err := BuildAdapters(cfg, reg)
	if err != nil {
		t.Fatalf("BuildAdapters() error = %v, want nil", err)
	}
	if len(adapters) != 1 || adapters[0].Platform() != "telegram" {
		t.Fatalf("adapters = %v, want only telegram (discord disabled)", adapters)
	}
	// Router can route to the built adapter.
	if err := router.Route(context.Background(), DeliveryTarget{Platform: "telegram", ChatID: "1"}, "hi"); err != nil {
		t.Fatalf("router.Route() error = %v", err)
	}
}

func TestBuildAdaptersFailsLoudOnUnregisteredEnabledPlatform(t *testing.T) {
	reg := NewPlatformRegistry()
	cfg := GatewayConfig{Platforms: map[string]PlatformSettings{"telegram": {Enabled: true, Token: "t"}}}
	if _, _, err := BuildAdapters(cfg, reg); err == nil {
		t.Fatalf("BuildAdapters(unregistered) error = nil, want non-nil")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestBuildAdapters -v`
Expected: FAIL

- [ ] **Step 3: 实现 wiring.go** — `internal/gateway/wiring.go`

```go
package gateway

import "fmt"

// BuildAdapters instantiates an adapter for every enabled platform in cfg using
// the registry, and returns them plus a DeliveryRouter pre-registered with each.
// An enabled platform with no registered entry is a loud configuration error.
func BuildAdapters(cfg GatewayConfig, reg *PlatformRegistry) ([]ChannelAdapter, *DeliveryRouter, error) {
	var adapters []ChannelAdapter
	router := NewDeliveryRouter()
	for name, settings := range cfg.Platforms {
		if !settings.Enabled {
			continue
		}
		entry, ok := reg.Get(name)
		if !ok {
			return nil, nil, fmt.Errorf("build adapters: platform %q enabled but not registered", name)
		}
		adapter, err := reg.Build(name, PlatformConfig{
			Token:              settings.Token,
			PollTimeoutSeconds: settings.PollTimeoutSeconds,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("build adapters: %w", err)
		}
		adapters = append(adapters, adapter)
		router.RegisterAdapter(adapter, entry.MaxMessageLength)
	}
	return adapters, router, nil
}
```

- [ ] **Step 4: 跑测试确认通过**
Run: `cd legion/legionAgent && go test ./internal/gateway/ -run TestBuildAdapters -v`
Expected: PASS

- [ ] **Step 5: 实现 main.go** — `cmd/gateway/main.go`

```go
// Command legion-gateway bridges IM platforms to a running Legion core over its
// HTTP API and SSE event stream. It is fully outbound (long polling), so it needs
// no public URL. Configure via a JSON file (path in LEGION_GATEWAY_CONFIG or the
// first argument); secrets come from environment variables named in that file.
package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/stardust/legion-agent/internal/gateway"
	"github.com/stardust/legion-agent/internal/gateway/platforms/telegram"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	configPath := os.Getenv("LEGION_GATEWAY_CONFIG")
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}
	if configPath == "" {
		log.Fatal("legion-gateway: config path required (LEGION_GATEWAY_CONFIG or arg 1)")
	}
	cfg, err := gateway.Load(configPath)
	if err != nil {
		log.Fatalf("legion-gateway: load config: %v", err)
	}

	reg := gateway.NewPlatformRegistry()
	if err := reg.Register(gateway.PlatformEntry{
		Platform:         "telegram",
		Factory:          telegram.New,
		MaxMessageLength: 4096,
		PIISafe:          true,
	}); err != nil {
		log.Fatalf("legion-gateway: register telegram: %v", err)
	}

	adapters, router, err := gateway.BuildAdapters(cfg, reg)
	if err != nil {
		log.Fatalf("legion-gateway: build adapters: %v", err)
	}
	if len(adapters) == 0 {
		log.Fatal("legion-gateway: no platforms enabled")
	}

	ctx := context.Background()
	binder, err := gateway.OpenSQLiteBinder(ctx, cfg.Binding.SQLitePath)
	if err != nil {
		log.Fatalf("legion-gateway: open binder: %v", err)
	}
	defer func() {
		if err := binder.Close(); err != nil {
			logger.Error("close binder", "err", err)
		}
	}()

	core := gateway.NewHTTPCoreClient(cfg.Core.BaseURL, cfg.Core.Token)
	runner := gateway.NewGatewayRunner(cfg, core, binder, router, gateway.NewDeliveryTracker(), adapters, logger)

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("legion-gateway started", "platforms", len(adapters), "core", cfg.Core.BaseURL)
	if err := runner.Run(ctx); err != nil {
		log.Fatalf("legion-gateway: run: %v", err)
	}
	logger.Info("legion-gateway stopped")
}
```

- [ ] **Step 6: 部署清单** — `configs/gateway.example.json`

```json
{
  "core":     { "base_url": "http://127.0.0.1:8080", "token_env": "LEGION_ADMIN_TOKEN" },
  "identity": { "agent_id": "im-agent", "company_id": "default-company" },
  "binding":  { "sqlite_path": "gateway.db" },
  "delivery": { "retries": 3, "backoff_ms": 500 },
  "platforms": {
    "telegram": { "enabled": true, "token_env": "LEGION_TELEGRAM_BOT_TOKEN", "poll_timeout_s": 30 }
  }
}
```

`deploy/docker-compose.gateway.yml`:

```yaml
services:
  gateway:
    image: legion-agent:latest
    command: ["legion-gateway", "/etc/legion/gateway.json"]
    environment:
      - LEGION_ADMIN_TOKEN=${LEGION_ADMIN_TOKEN}
      - LEGION_TELEGRAM_BOT_TOKEN=${LEGION_TELEGRAM_BOT_TOKEN}
    volumes:
      - ./configs/gateway.example.json:/etc/legion/gateway.json:ro
      - gateway-data:/data
    restart: unless-stopped

volumes:
  gateway-data:
```

- [ ] **Step 7: 验证门（含 build 出二进制）**
```bash
cd legion/legionAgent && go build ./... && go vet ./... && go test ./... && gofmt -l .
go build -o /dev/null ./cmd/gateway
```
Expected: 全绿，二进制可编译。

- [ ] **Step 8: Commit**
```bash
git add cmd/gateway/ internal/gateway/wiring.go internal/gateway/wiring_test.go configs/gateway.example.json deploy/docker-compose.gateway.yml
git commit -m "feat(gateway): legion-gateway binary, wiring, and deploy manifests"
```

---

## 收尾：ADR + 文档回链

- [ ] 在 `docs/architecture/adr-im-gateway.md` 写 ADR（决策：独立进程、长轮询、SSE 完成、每 chat 会话、PII 边界），front matter + `@section` + `related_docs` 指向本计划与 spec；更新 `../../../docs/design/analysis/hermes/06-hermes-insights.md`「Legion 参考」处标注「已实现（IM 网关，Telegram 样板）」。
- [ ] 全量门：`cd legion/legionAgent && go build ./... && go vet ./... && go test ./... && gofmt -l .` 全绿。
- [ ] Commit：`docs(architecture): ADR for IM gateway; mark hermes multi-channel reference implemented`

---

## 依赖与顺序

```
Task1(契约验证) ── 前置去风险，必须先过
Task2(框架类型/注册表) ─┬─> Task5(router)  Task9(telegram) Task11(wiring/main)
Task3(hash) ────────────┤
Task4(tracker) ─────────┤
Task6(binder) ──────────┤
Task7(config) ──────────┤
Task8(core client) ─────┴─> Task10(runner, 依赖 2/3/4/8) ─> Task11
```

建议顺序：**Task1 → 2 → 3 → 4 → 5 → 6 → 7 → 8 → 9 → 10 → 11 → 收尾**（Task 2–9 多数可并行，但按序做最省心）。
