# 插件生命周期内核（P0）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 legionAgent 补上「插件出口」——资源所有权账本、注册即效果、调用时解析的工具视图，使任何注册都能被干净撤销。

**Architecture:** 新增 `internal/lifecycle` 包持有 owner → disposer 账本（逆序撤销、失败不互相阻断）；改造 `internal/tool.Registry`：注册返回撤销函数、加读写锁、重名 fail-loud；把 `Subset`/`Without` 从快照拷贝改为持父引用的过滤视图，父级撤销后子视图立即失效。本期**不引入 WASM**，纯 Go，行为对现有调用点保持兼容。

**Tech Stack:** Go 1.26.0，module `github.com/stardust/legion-agent`，标准库 `sync` / `errors` / `fmt`，测试用 `go test` + `-race`。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：禁止 fallback、禁止静默吞错、禁止零值假装正常；错误用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装；不变量违反用 `panic`。
- 公开 API 必须有 Go doc 风格注释，以标识符名开头。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿，`gofmt -l .` 输出为空。
- 涉及并发的任务额外跑 `go test -race`。
- 错误路径必须有测试断言「确实返回 error」，不得只测 happy path。
- 设计依据：`docs/design/architecture/legion-plugin-system.md`（Legion docs 仓）§5.1–§5.3。

---

### Task 1: `lifecycle.Ledger` —— owner 资源账本

**Files:**
- Create: `internal/lifecycle/ledger.go`
- Test: `internal/lifecycle/ledger_test.go`

**Interfaces:**
- Consumes: 无（本包零内部依赖）
- Produces:
  - `type Owner string`
  - `func NewLedger() *Ledger`
  - `func (l *Ledger) Add(owner Owner, label string, dispose func() error) func() error`
  - `func (l *Ledger) DisposeOwner(owner Owner) error`
  - `func (l *Ledger) Snapshot() map[Owner][]string`

- [ ] **Step 1: 写失败测试**

创建 `internal/lifecycle/ledger_test.go`：

```go
package lifecycle

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestLedgerDisposesInReverseOrder(t *testing.T) {
	l := NewLedger()
	var order []string
	l.Add("plugin:a", "first", func() error { order = append(order, "first"); return nil })
	l.Add("plugin:a", "second", func() error { order = append(order, "second"); return nil })

	if err := l.DisposeOwner("plugin:a"); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}
	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Fatalf("want [second first], got %v", order)
	}
}

func TestLedgerRunsEveryDisposerDespiteFailure(t *testing.T) {
	l := NewLedger()
	ran := 0
	l.Add("plugin:a", "ok-1", func() error { ran++; return nil })
	l.Add("plugin:a", "boom", func() error { ran++; return errors.New("close failed") })
	l.Add("plugin:a", "ok-2", func() error { ran++; return nil })

	err := l.DisposeOwner("plugin:a")
	if err == nil {
		t.Fatal("want joined error, got nil")
	}
	if ran != 3 {
		t.Fatalf("want all 3 disposers to run, ran %d", ran)
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("error must name the failing entry and cause, got %q", err)
	}
}

func TestLedgerHandleIsIdempotentAndRemovesEntry(t *testing.T) {
	l := NewLedger()
	calls := 0
	revoke := l.Add("plugin:a", "one", func() error { calls++; return nil })

	if err := revoke(); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := revoke(); err != nil {
		t.Fatalf("second revoke must be a no-op, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("want dispose called once, got %d", calls)
	}
	if got := l.Snapshot(); len(got) != 0 {
		t.Fatalf("want empty ledger after revoke, got %v", got)
	}
}

func TestLedgerDisposeOwnerSkipsAlreadyRevoked(t *testing.T) {
	l := NewLedger()
	calls := 0
	revoke := l.Add("plugin:a", "one", func() error { calls++; return nil })
	l.Add("plugin:a", "two", func() error { calls++; return nil })

	if err := revoke(); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := l.DisposeOwner("plugin:a"); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}
	if calls != 2 {
		t.Fatalf("want 2 total dispose calls, got %d", calls)
	}
}

func TestLedgerDisposeUnknownOwnerIsNoOp(t *testing.T) {
	l := NewLedger()
	if err := l.DisposeOwner("plugin:missing"); err != nil {
		t.Fatalf("want nil for unknown owner, got %v", err)
	}
}

func TestLedgerSnapshotReportsLabelsPerOwner(t *testing.T) {
	l := NewLedger()
	l.Add("plugin:a", "tool:foo", func() error { return nil })
	l.Add("plugin:a", "tool:bar", func() error { return nil })
	l.Add("plugin:b", "section:x", func() error { return nil })

	snap := l.Snapshot()
	if len(snap["plugin:a"]) != 2 || len(snap["plugin:b"]) != 1 {
		t.Fatalf("unexpected snapshot: %v", snap)
	}
	snap["plugin:a"][0] = "mutated"
	if l.Snapshot()["plugin:a"][0] == "mutated" {
		t.Fatal("Snapshot must return a copy, not internal state")
	}
}

func TestLedgerAddNilDisposePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic on nil dispose")
		}
	}()
	NewLedger().Add("plugin:a", "bad", nil)
}

func TestLedgerConcurrentAddAndDispose(t *testing.T) {
	l := NewLedger()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			revoke := l.Add("plugin:a", "n", func() error { return nil })
			_ = revoke()
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); _ = l.DisposeOwner("plugin:a") }()
	wg.Wait()
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/lifecycle/... -v
```

预期：FAIL，`no required module provides package .../internal/lifecycle` 或 `undefined: NewLedger`。

- [ ] **Step 3: 实现 `ledger.go`**

创建 `internal/lifecycle/ledger.go`：

```go
// Package lifecycle owns the answer to one question: when something must be
// torn down, who is responsible for revoking it.
//
// A Ledger records revocation actions grouped by owner. It never decides WHAT a
// disposer does — the creator supplies that — only who must run it and in which
// order. Ownership follows the creator, not the place the resource ends up: a
// plugin that registers a handler into someone else's map still owns the
// revocation, so no central component has to maintain a directory of who came,
// who left, and what they left behind.
package lifecycle

import (
	"errors"
	"fmt"
	"sync"
)

// Owner identifies whoever created a resource: a plugin instance id, an agent
// session id, or a static assembly name. Compared by value.
type Owner string

// entry is one revocable resource. done guards against running a disposer
// twice when a handle and DisposeOwner race.
type entry struct {
	label   string
	dispose func() error
	done    bool
}

// Ledger maps owners to the revocation actions they are responsible for.
// The zero value is not usable; call NewLedger.
type Ledger struct {
	mu      sync.Mutex
	entries map[Owner][]*entry
}

// NewLedger returns an empty Ledger.
func NewLedger() *Ledger {
	return &Ledger{entries: make(map[Owner][]*entry)}
}

// Add registers dispose under owner and returns a one-shot handle that runs it
// immediately and removes it from the ledger. label names the resource in
// diagnostics and in wrapped errors, so it should identify the thing being
// revoked ("tool:read_file", "wasm-instance"), not the action.
//
// A nil dispose is a programming error: registering a resource with no way to
// revoke it defeats the ledger's only purpose.
func (l *Ledger) Add(owner Owner, label string, dispose func() error) func() error {
	if dispose == nil {
		panic(fmt.Sprintf("lifecycle: Add(%s, %s) requires a dispose func", owner, label))
	}
	e := &entry{label: label, dispose: dispose}
	l.mu.Lock()
	l.entries[owner] = append(l.entries[owner], e)
	l.mu.Unlock()
	return func() error { return l.revoke(owner, e) }
}

// revoke runs one entry if it has not run yet and detaches it from its owner.
func (l *Ledger) revoke(owner Owner, target *entry) error {
	l.mu.Lock()
	if target.done {
		l.mu.Unlock()
		return nil
	}
	target.done = true
	list := l.entries[owner]
	for i, e := range list {
		if e == target {
			l.entries[owner] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(l.entries[owner]) == 0 {
		delete(l.entries, owner)
	}
	l.mu.Unlock()

	if err := target.dispose(); err != nil {
		return fmt.Errorf("dispose %s/%s: %w", owner, target.label, err)
	}
	return nil
}

// DisposeOwner runs every disposer registered under owner in reverse
// registration order — last created, first closed — and clears the owner.
//
// Every remaining disposer runs even when an earlier one fails: stopping at the
// first failure would leave more ghosts behind than it prevents. Failures are
// joined and returned; callers at a teardown boundary must log the result at
// Error level rather than discarding it.
func (l *Ledger) DisposeOwner(owner Owner) error {
	l.mu.Lock()
	list := l.entries[owner]
	delete(l.entries, owner)
	pending := make([]*entry, 0, len(list))
	for _, e := range list {
		if !e.done {
			e.done = true
			pending = append(pending, e)
		}
	}
	l.mu.Unlock()

	var errs []error
	for i := len(pending) - 1; i >= 0; i-- {
		if err := pending[i].dispose(); err != nil {
			errs = append(errs, fmt.Errorf("dispose %s/%s: %w", owner, pending[i].label, err))
		}
	}
	return errors.Join(errs...)
}

// Snapshot reports the live entry labels per owner, in registration order. The
// returned maps and slices are copies; mutating them does not affect the
// ledger. It exists for diagnostics: "plugin loaded but nothing happened" must
// be answerable without reading code.
func (l *Ledger) Snapshot() map[Owner][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[Owner][]string, len(l.entries))
	for owner, list := range l.entries {
		labels := make([]string, 0, len(list))
		for _, e := range list {
			if !e.done {
				labels = append(labels, e.label)
			}
		}
		if len(labels) > 0 {
			out[owner] = labels
		}
	}
	return out
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/lifecycle/... -v -race
```

预期：全部 PASS（8 个测试）。

- [ ] **Step 5: 提交**

```bash
git add internal/lifecycle/ledger.go internal/lifecycle/ledger_test.go
git commit -m "feat(lifecycle): add owner-scoped disposer ledger"
```

---

### Task 2: `tool.Registry` 注册即效果

**Files:**
- Modify: `internal/tool/registry.go:49-76`（结构体 + `Register` + `RegisterDescriptor`）
- Modify: `internal/tool/registry.go:118-141`（`Descriptors` / `SafeToolNames` 加读锁）
- Modify: `internal/tool/registry.go:153-156`（`Execute` 查表加读锁）
- Test: `internal/tool/registry_revoke_test.go`（新建）

**Interfaces:**
- Consumes: 无
- Produces:
  - `func (r *Registry) Register(name string, handler Handler) func()`
  - `func (r *Registry) RegisterDescriptor(descriptor Descriptor, handler Handler) func()`
  - `func (r *Registry) Replace(descriptor Descriptor, handler Handler) func()`

- [ ] **Step 1: 写失败测试**

创建 `internal/tool/registry_revoke_test.go`：

```go
package tool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

func okHandler() Handler {
	return HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Output: "ok"}, nil
	})
}

func TestRegisterReturnsWorkingRevoke(t *testing.T) {
	r := NewRegistry(nil, nil, nil)
	revoke := r.Register("echo", okHandler())

	if _, err := r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "echo"}); err != nil {
		t.Fatalf("Execute before revoke: %v", err)
	}

	revoke()

	_, err := r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "echo"})
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("want ErrToolNotFound after revoke, got %v", err)
	}
	for _, d := range r.Descriptors() {
		if d.Name == "echo" {
			t.Fatal("revoked tool must not appear in Descriptors")
		}
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	r := NewRegistry(nil, nil, nil)
	revoke := r.Register("echo", okHandler())
	revoke()
	revoke()
	r.Register("echo", okHandler()) // name must be free again
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic on duplicate tool name")
		}
	}()
	r := NewRegistry(nil, nil, nil)
	r.Register("echo", okHandler())
	r.Register("echo", okHandler())
}

func TestReplaceOverridesAndRestoresNothing(t *testing.T) {
	r := NewRegistry(nil, nil, nil)
	r.Register("echo", okHandler())
	revoke := r.Replace(Descriptor{Name: "echo"}, HandlerFunc(
		func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{Output: "replaced"}, nil
		}))

	res, err := r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "echo"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output != "replaced" {
		t.Fatalf("want replaced handler, got %q", res.Output)
	}

	revoke()
	if _, err := r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "echo"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("Replace revoke removes the name outright, got %v", err)
	}
}

func TestConcurrentRegisterRevokeExecute(t *testing.T) {
	r := NewRegistry(nil, nil, nil)
	r.Register("stable", okHandler())

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "stable"})
		}()
		// 每个 goroutine 用唯一名：重名注册是 fail-loud 的 panic，
		// 并发测试要压的是锁，不是重名契约。
		go func(i int) {
			defer wg.Done()
			revoke := r.Register(fmt.Sprintf("churn-%d", i), okHandler())
			revoke()
		}(i)
	}
	wg.Wait()
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/tool/... -run "TestRegisterReturnsWorkingRevoke|TestDuplicateRegistrationPanics|TestReplace" -v
```

预期：FAIL，`revoke := r.Register(...)` 处报 `assignment mismatch: 1 variable but r.Register returns 0 values`，以及 `r.Replace undefined`。

- [ ] **Step 3: 改造 `registry.go`**

把 `internal/tool/registry.go` 中结构体与注册方法替换为：

```go
type Registry struct {
	policy    Policy
	enforcer  PermissionEnforcer
	guards    Guardrails
	audit     port.AuditLog
	sanitizer port.OutputSanitizer

	// mu guards handlers and describes. Registration used to happen only during
	// assembly; plugins register and revoke while the agent is running, so reads
	// on the execution path now race writes without it.
	mu        sync.RWMutex
	handlers  map[string]Handler
	describes map[string]Descriptor
}

// Register adds a tool under name and returns its revoke function. See
// RegisterDescriptor for the duplicate-name contract.
func (r *Registry) Register(name string, handler Handler) func() {
	return r.RegisterDescriptor(Descriptor{Name: name}, handler)
}

// RegisterDescriptor adds one tool and returns the function that removes it.
// The revoke function is idempotent and frees the name for reuse.
//
// Registering a name that is already registered panics. Two contributors
// fighting over one model-facing name is never a valid state: silently
// overwriting turns which implementation the model reaches into a load-order
// lottery, and the loser's registration would never be revocable. Deliberate
// override goes through Replace.
func (r *Registry) RegisterDescriptor(descriptor Descriptor, handler Handler) func() {
	r.mu.Lock()
	if _, exists := r.handlers[descriptor.Name]; exists {
		r.mu.Unlock()
		panic(fmt.Sprintf("tool: duplicate registration for %q", descriptor.Name))
	}
	r.handlers[descriptor.Name] = handler
	r.describes[descriptor.Name] = descriptor
	r.mu.Unlock()
	return r.revokeFunc(descriptor.Name)
}

// Replace installs handler under descriptor.Name whether or not the name is
// taken, and returns the function that removes it. It does NOT restore the
// previous registration: reinstating a handler whose owner may already be gone
// would resurrect exactly the stale implementation this design removes.
func (r *Registry) Replace(descriptor Descriptor, handler Handler) func() {
	r.mu.Lock()
	r.handlers[descriptor.Name] = handler
	r.describes[descriptor.Name] = descriptor
	r.mu.Unlock()
	return r.revokeFunc(descriptor.Name)
}

// revokeFunc returns an idempotent remover for name.
func (r *Registry) revokeFunc(name string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.handlers, name)
			delete(r.describes, name)
			r.mu.Unlock()
		})
	}
}
```

在文件 import 块补 `"sync"`（`fmt` 已存在）。

`Descriptors` 与 `SafeToolNames` 的循环体外分别加：

```go
	r.mu.RLock()
	defer r.mu.RUnlock()
```

`Execute` 开头的查表改为：

```go
	r.mu.RLock()
	handler, ok := r.handlers[call.Name]
	descriptor := r.describes[call.Name]
	r.mu.RUnlock()
	if !ok {
		return domain.ToolResult{}, fmt.Errorf("%w: %s", ErrToolNotFound, call.Name)
	}
```

（删除原第 158 行独立的 `descriptor := r.describes[call.Name]`。）

- [ ] **Step 4: 修正既有调用点并跑全量测试**

22 处调用点（`internal/tool/*.go`、`internal/runtime/delegation_tool.go`、`internal/runtime/moa_tool.go`、`internal/app/app.go:157`）无需修改——Go 允许丢弃返回值。直接跑：

```bash
go build ./... && go test ./internal/tool/... ./internal/runtime/... ./internal/app/... -race
```

预期：PASS。若出现 `panic: tool: duplicate registration for "xxx"`，说明该处存在真实的重复注册：如果是测试里刻意覆盖，把那一处改为 `r.Replace(Descriptor{Name: "xxx"}, handler)`；如果是生产代码里两个模块抢同一名字，这是必须修的 bug，按 fail-loud 铁律定位后消除，不要用 `Replace` 掩盖。

- [ ] **Step 5: 提交**

```bash
git add internal/tool/registry.go internal/tool/registry_revoke_test.go
git commit -m "feat(tool): make registration revocable and concurrency-safe"
```

---

### Task 3: `Subset` / `Without` 改为调用时解析的视图

**Files:**
- Modify: `internal/tool/registry.go:78-116`（`Subset` / `Without`）
- Modify: `internal/tool/registry.go`（新增 `filter`、`view`、`resolve`、`visible`；`Descriptors` / `SafeToolNames` / `Execute` 改走解析）
- Test: `internal/tool/registry_view_test.go`（新建）
- Test: `internal/tool/registry_subset_test.go`（既有，可能需按新语义调整）

**Interfaces:**
- Consumes: Task 2 的 `Register(...) func()`、`RegisterDescriptor(...) func()`、`r.mu`；以及 Task 2 在 `registry_revoke_test.go` 里定义的包内测试助手 `okHandler() Handler`（同包，无需重复定义）
- Produces:
  - `func (r *Registry) Subset(names ...string) *Registry`（语义变更：返回视图而非拷贝）
  - `func (r *Registry) Without(names ...string) *Registry`（同上）

- [ ] **Step 1: 写失败测试**

创建 `internal/tool/registry_view_test.go`：

```go
package tool

import (
	"context"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// 幽灵回归测试：父级撤销后，先前派生的子视图必须立即失效。
func TestSubsetViewLosesToolWhenParentRevokes(t *testing.T) {
	parent := NewRegistry(nil, nil, nil)
	revoke := parent.Register("foo", okHandler())
	child := parent.Subset("foo")

	if _, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "foo"}); err != nil {
		t.Fatalf("child Execute before revoke: %v", err)
	}

	revoke()

	_, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "foo"})
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("want ErrToolNotFound in child after parent revoke, got %v", err)
	}
}

func TestSubsetAllowListExcludesLaterUnlistedTools(t *testing.T) {
	parent := NewRegistry(nil, nil, nil)
	parent.Register("foo", okHandler())
	child := parent.Subset("foo")

	parent.Register("bar", okHandler()) // 后注册且未列入 allow

	if _, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "bar"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("allow-list must exclude later unlisted tools, got %v", err)
	}
}

func TestWithoutDenyListAdmitsLaterTools(t *testing.T) {
	parent := NewRegistry(nil, nil, nil)
	parent.Register("foo", okHandler())
	child := parent.Without("foo")

	parent.Register("bar", okHandler()) // 后注册且未被 deny

	if _, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "bar"}); err != nil {
		t.Fatalf("deny-only filter must admit later tools, got %v", err)
	}
	if _, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "foo"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("denied tool must stay invisible, got %v", err)
	}
}

// 被过滤掉的工具，既不出现在 Descriptors 也拒绝执行——与不存在不可区分。
func TestFilteredToolIsInvisibleAndUnexecutable(t *testing.T) {
	parent := NewRegistry(nil, nil, nil)
	parent.Register("foo", okHandler())
	parent.Register("secret", okHandler())
	child := parent.Subset("foo")

	for _, d := range child.Descriptors() {
		if d.Name == "secret" {
			t.Fatal("filtered tool must not appear in Descriptors")
		}
	}
	if _, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "secret"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("filtered tool must refuse execution, got %v", err)
	}
}

// 作用域自有注册豁免过滤：委派出去的子作用域保留它自己应答的工具。
func TestOwnRegistrationBypassesFilter(t *testing.T) {
	parent := NewRegistry(nil, nil, nil)
	parent.Register("foo", okHandler())
	child := parent.Subset("foo")
	child.Register("child_only", okHandler())

	if _, err := child.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "child_only"}); err != nil {
		t.Fatalf("own registration must bypass the filter, got %v", err)
	}
	if _, err := parent.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "child_only"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("child registration must not leak upward, got %v", err)
	}
}

func TestNestedViewsIntersectFilters(t *testing.T) {
	parent := NewRegistry(nil, nil, nil)
	parent.Register("a", okHandler())
	parent.Register("b", okHandler())
	grandchild := parent.Subset("a", "b").Without("b")

	if _, err := grandchild.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "a"}); err != nil {
		t.Fatalf("a must stay visible, got %v", err)
	}
	if _, err := grandchild.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "b"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("b must be denied by the nested filter, got %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/tool/... -run "TestSubsetView|TestWithoutDeny|TestFilteredTool|TestOwnRegistration|TestNestedViews" -v
```

预期：`TestSubsetViewLosesToolWhenParentRevokes` FAIL（当前是拷贝语义，子视图仍能执行），`TestWithoutDenyListAdmitsLaterTools` FAIL（拷贝看不到后注册的工具）。

- [ ] **Step 3: 实现视图解析**

在 `internal/tool/registry.go` 的 `Registry` 结构体中加入两个字段：

```go
type Registry struct {
	// parent is nil for a root registry. A derived view holds a REFERENCE to
	// its parent and resolves at call time, never a copy of its handlers: a
	// copy keeps answering after the parent revokes the tool.
	parent *Registry
	filter *filter

	// ...（Task 2 已有字段保持不变）
}

// filter is one scope's view over what it inherits. allow == nil means no
// allow-list, so unlisted inherited tools stay visible; deny always removes.
type filter struct {
	allow map[string]bool
	deny  map[string]bool
}

// admits reports whether an inherited name survives this filter.
func (f *filter) admits(name string) bool {
	if f == nil {
		return true
	}
	if f.deny[name] {
		return false
	}
	if f.allow != nil && !f.allow[name] {
		return false
	}
	return true
}
```

替换 `Subset` / `Without`，并新增 `view` / `resolve` / `visible`：

```go
// Subset returns a VIEW over this registry exposing only the named tools plus
// whatever the view registers itself. It shares this registry's policy,
// enforcer, guardrails, audit log and sanitizer. Names with no matching handler
// are ignored, and a tool registered on the parent later is visible only if it
// was named here. It backs delegated sub-agents that must run with a narrowed
// tool set.
//
// The view resolves through its parent on every call, so revoking a tool on the
// parent removes it from every derived view at once.
func (r *Registry) Subset(names ...string) *Registry {
	allow := make(map[string]bool, len(names))
	for _, name := range names {
		allow[name] = true
	}
	return r.view(&filter{allow: allow})
}

// Without returns a VIEW exposing every inherited tool except the named ones,
// including tools registered on the parent after this call. Names with no
// matching tool are ignored: disabling a tool an agent never had is a
// legitimate no-op, not an error. It never mutates the receiver.
func (r *Registry) Without(names ...string) *Registry {
	deny := make(map[string]bool, len(names))
	for _, name := range names {
		deny[name] = true
	}
	return r.view(&filter{deny: deny})
}

// view builds a child registry that inherits through f.
func (r *Registry) view(f *filter) *Registry {
	return &Registry{
		parent:    r,
		filter:    f,
		policy:    r.policy,
		enforcer:  r.enforcer,
		guards:    r.guards,
		audit:     r.audit,
		sanitizer: r.sanitizer,
		handlers:  make(map[string]Handler),
		describes: make(map[string]Descriptor),
	}
}

// resolve returns the handler and descriptor this registry currently exposes
// for name. Own registrations win and bypass the filter — a delegated scope
// keeps the tools it answers itself — while inherited ones must pass it.
func (r *Registry) resolve(name string) (Handler, Descriptor, bool) {
	r.mu.RLock()
	handler, ok := r.handlers[name]
	descriptor := r.describes[name]
	r.mu.RUnlock()
	if ok {
		return handler, descriptor, true
	}
	if r.parent == nil || !r.filter.admits(name) {
		return nil, Descriptor{}, false
	}
	return r.parent.resolve(name)
}

// visible collects every descriptor this registry exposes into out, inherited
// first so own registrations shadow same-named inherited ones.
func (r *Registry) visible(out map[string]Descriptor) {
	if r.parent != nil {
		inherited := make(map[string]Descriptor)
		r.parent.visible(inherited)
		for name, descriptor := range inherited {
			if r.filter.admits(name) {
				out[name] = descriptor
			}
		}
	}
	r.mu.RLock()
	for name, descriptor := range r.describes {
		out[name] = descriptor
	}
	r.mu.RUnlock()
}
```

`Descriptors` 改为走 `visible`：

```go
func (r *Registry) Descriptors() []Descriptor {
	collected := make(map[string]Descriptor)
	r.visible(collected)
	descriptors := make([]Descriptor, 0, len(collected))
	for _, descriptor := range collected {
		descriptors = append(descriptors, descriptor)
	}
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].Name < descriptors[j].Name })
	return descriptors
}
```

`SafeToolNames` 同样改为遍历 `r.Descriptors()` 而非直接读 `r.describes`（保持其原有的过滤条件不变）。

`Execute` 的查表改为：

```go
	handler, descriptor, ok := r.resolve(call.Name)
	if !ok {
		return domain.ToolResult{}, fmt.Errorf("%w: %s", ErrToolNotFound, call.Name)
	}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/tool/... -v -race
```

预期：新测试全 PASS。既有 `registry_subset_test.go` 若断言「父级后续注册对子集不可见」这类拷贝语义，按新语义修正断言并在提交信息里说明行为变更；若断言的是过滤本身，应原样通过。

- [ ] **Step 5: 全量回归**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l .
```

预期：全绿，`gofmt -l .` 无输出。重点关注 `internal/runtime`（委派子 agent 用 `Subset`）与 `internal/toolauth`（用 `Without` 做 per-agent 禁用）的测试。

- [ ] **Step 6: 提交**

```bash
git add internal/tool/registry.go internal/tool/registry_view_test.go internal/tool/registry_subset_test.go
git commit -m "fix(tool): resolve Subset/Without views at call time instead of copying handlers"
```

---

### Task 4: owner 绑定与诊断出口

**Files:**
- Create: `internal/tool/owned.go`
- Test: `internal/tool/owned_test.go`
- Modify: `internal/app/app.go`（装配处创建 ledger 并透出快照）

**Interfaces:**
- Consumes: Task 1 的 `lifecycle.NewLedger()` / `Add` / `DisposeOwner` / `Snapshot`；Task 2 的 `RegisterDescriptor(...) func()` 与包内测试助手 `okHandler() Handler`
- Produces:
  - `func RegisterOwned(ledger *lifecycle.Ledger, owner lifecycle.Owner, r *Registry, d Descriptor, h Handler) func() error`

- [ ] **Step 1: 写失败测试**

创建 `internal/tool/owned_test.go`：

```go
package tool

import (
	"context"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
)

func TestRegisterOwnedRevokesThroughLedger(t *testing.T) {
	ledger := lifecycle.NewLedger()
	r := NewRegistry(nil, nil, nil)
	RegisterOwned(ledger, "plugin:demo", r, Descriptor{Name: "demo_tool"}, okHandler())

	if _, err := r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "demo_tool"}); err != nil {
		t.Fatalf("Execute before dispose: %v", err)
	}
	if labels := ledger.Snapshot()["plugin:demo"]; len(labels) != 1 || labels[0] != "tool:demo_tool" {
		t.Fatalf("want one ledger entry labelled tool:demo_tool, got %v", labels)
	}

	if err := ledger.DisposeOwner("plugin:demo"); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}

	if _, err := r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "demo_tool"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("want ErrToolNotFound after owner disposal, got %v", err)
	}
	if len(ledger.Snapshot()) != 0 {
		t.Fatalf("ledger must be empty after disposal, got %v", ledger.Snapshot())
	}
}

func TestRegisterOwnedHandleIsIdempotent(t *testing.T) {
	ledger := lifecycle.NewLedger()
	r := NewRegistry(nil, nil, nil)
	revoke := RegisterOwned(ledger, "plugin:demo", r, Descriptor{Name: "demo_tool"}, okHandler())

	if err := revoke(); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if err := revoke(); err != nil {
		t.Fatalf("second revoke must be a no-op, got %v", err)
	}
	if err := ledger.DisposeOwner("plugin:demo"); err != nil {
		t.Fatalf("DisposeOwner after manual revoke: %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/tool/... -run TestRegisterOwned -v
```

预期：FAIL，`undefined: RegisterOwned`。

- [ ] **Step 3: 实现 `owned.go`**

创建 `internal/tool/owned.go`：

```go
package tool

import (
	"github.com/stardust/legion-agent/internal/lifecycle"
)

// RegisterOwned registers one tool and files its revocation under owner in
// ledger, returning the same one-shot handle the ledger hands out.
//
// Ownership follows the CREATOR, not the registry the handler lands in: a
// plugin that contributes a tool owns removing it, so the registry never has to
// track who contributed what. Disposing the owner removes every tool it
// contributed, in reverse registration order.
func RegisterOwned(
	ledger *lifecycle.Ledger,
	owner lifecycle.Owner,
	r *Registry,
	descriptor Descriptor,
	handler Handler,
) func() error {
	revoke := r.RegisterDescriptor(descriptor, handler)
	return ledger.Add(owner, "tool:"+descriptor.Name, func() error {
		revoke()
		return nil
	})
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/tool/... -run TestRegisterOwned -v -race
```

预期：两个测试 PASS。

- [ ] **Step 5: 在装配处创建 ledger 并透出快照**

在 `internal/app/app.go` 的 `App` 结构体加字段：

```go
	// ledger records which owner must revoke which runtime resource. Static
	// assembly registers under the "app" owner; plugin instances get their own.
	ledger *lifecycle.Ledger
```

在 `New()` 里初始化 `ledger: lifecycle.NewLedger()`，并新增导出方法：

```go
// PluginResources reports the live revocable resources per owner. It answers
// "the plugin loaded but nothing happened" without reading code.
func (a *App) PluginResources() map[lifecycle.Owner][]string {
	return a.ledger.Snapshot()
}
```

补 import `"github.com/stardust/legion-agent/internal/lifecycle"`。

- [ ] **Step 6: 全量回归**

```bash
go build ./... && go vet ./... && go test ./... -race && gofmt -l .
```

预期：全绿，`gofmt -l .` 无输出。

- [ ] **Step 7: 提交**

```bash
git add internal/tool/owned.go internal/tool/owned_test.go internal/app/app.go
git commit -m "feat(tool): bind tool registration to an owner ledger"
```

---

## 交付后状态

本期完成后：

- 任何工具注册都可被撤销，撤销责任跟随创建者（`lifecycle.Ledger`）。
- 派生视图不再持有 handler 拷贝，父级撤销即刻对所有子视图生效（消灭 §3 缺陷 B 的幽灵）。
- 注册表并发安全，重名注册 fail-loud（缺陷 A、C）。
- `App.PluginResources()` 可回答「谁持有哪些资源」。

**尚未包含**（各自独立 plan）：WASM 宿主与 ABI、`plugin.Loader` 与 `plugins.yaml`、能力白名单、在途调用收敛、策略钩子与 prompt 段插件面、§8 的事件与 `plugins status` 诊断命令。设计见 Legion docs 仓 `docs/design/architecture/legion-plugin-system.md` §6–§9。
