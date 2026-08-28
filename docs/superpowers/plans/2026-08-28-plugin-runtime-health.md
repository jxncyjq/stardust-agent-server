# 插件运行期健康度 实施计划（G1）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让一个反复失败的插件被自己的失败记录下来并自动卸载，让一次没收敛干净的卸载留下事件——今天两件事都是无声的。

**Architecture:** 三段。①`internal/plugin/host` 给 guest 调用失败挂上可 `errors.Is` 的分类哨兵（trap / abi），调用方按 ctx 状态另判 timeout；②`host.Deps` 增加 `OnFault` 回调，`pluginToolHandler` 在失败时既发 `plugin/call_failed`（带 category）又报告给回调，`Loader` 用它维护**连续**故障计数，超阈值卸载；③`pool.drain` 暴露在途数，`Loader.unload` 在 drain 超时时发 `plugin/unload_leaked`。

**Tech Stack:** Go 1.26.0，module `github.com/stardust/legion-agent`；wazero；**不新增第三方依赖**。

**上游依据:** 设计文档 `legion-plugin-system.md` §6.9（错误分类与健康度）、§8（事件表）；路线 `plans/2026-08-28-plugin-gap-closure-roadmap.md` 的 G1。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：禁止 fallback、禁止静默吞错、禁止零值假装正常；Go 错误用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装。
- 公开标识符必须有 Go doc 风格注释，以标识符名开头，且不得与代码矛盾。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空。
- **每个 task 至少跑一次 `go test ./...`**（不是包子集）——`TestOpenAPIGolden` 住在 `internal/compat` 却覆盖 `internal/server`。
- 错误路径必须有测试断言「确实返回 error / 确实计数」，不得只测 happy path。
- 每个 task 做**变异验证**：把该 task 的核心机制改坏，确认测试确实 FAIL，把失败输出留在报告里，然后还原并 `git status` 核对。
- 并发相关（Task 3、Task 5）额外跑 `go test -race ./internal/plugin/...`。
- 提交只 stage 本 task 自己的文件（显式路径），**永不 `git add -A`**——工作区有无关的运行时产物（`tasks.md`、`agent.db`）。
- 不改 `plugin_example/` 的包，但每个 task 结束时 `go test ./plugin_example/...` 必须仍绿。

## 前置事实（已在 master，直接用）

```go
// internal/plugin/host —— hostcall.go
const RuntimeEventCallFailed = "plugin/call_failed"      // :50
const CategoryDenied = "denied"                          // :57
func formatCallFailedMessage(category, plugin, hostFunc, reason string) string  // :74
func EventHasCategory(event domain.RuntimeEvent, category string) bool          // :84

// internal/plugin/host —— instance.go
func (i *Instance) Invoke(ctx context.Context, op int32, in []byte) (out []byte, err error)  // :165

// internal/plugin/host —— contribute.go
func contributeTools(ledger, owner, spec Spec, guest guestCaller, keep func(func() error))  // :90
func pluginToolHandler(pluginName, toolName string, guest guestCaller) tool.Handler         // :127

// internal/plugin/host —— hostfunc.go
type Deps struct { PluginName string; Logger *slog.Logger; Config json.RawMessage;
                   KV KVStore; HTTP *http.Client; FS port.WorkspacePathGuard;
                   Events port.EventBus; Tools *tool.Registry; Agent domain.Agent }  // :82

// internal/plugin/loader —— loader.go
type instance struct { name, version string; owner lifecycle.Owner; spec host.Spec;
                       fingerprint, sha256 string; tools []string; lastError string;
                       plugin *host.Plugin }                       // :368
func (l *Loader) unload(ctx context.Context, inst *instance, reason string) (int, error)  // :1249
func (l *Loader) publish(ctx context.Context, eventType, message string)                  // :1283
func (l *Loader) Status() []InstanceStatus                                                // :922

// internal/plugin/host —— pool.go
func (p *pool) drain(ctx context.Context) error   // :400；ctx 超时返回错误，在途 goroutine 继续跑
```

---

### Task 1: 给 guest 调用失败挂上可判别的分类

**Files:**
- Modify: `internal/plugin/host/instance.go`（`Invoke` 的四条失败路径）
- Modify: `internal/plugin/host/hostcall.go`（分类常量放在既有 `CategoryDenied` 旁边）
- Create: `internal/plugin/host/fault.go`
- Test: `internal/plugin/host/fault_test.go`

**Interfaces:**
- Produces:
  - `var host.ErrGuestTrap error`、`var host.ErrGuestABI error`
  - `const host.CategoryTimeout = "timeout"`、`host.CategoryTrap = "trap"`、`host.CategoryABI = "abi"`
  - `func host.ClassifyCallFault(ctx context.Context, err error) (category string, isFault bool)`

今天 `Invoke` 的失败全是 `fmt.Errorf` 字符串包装，`errors.Is` 认不出任何一条。健康度要按类别计数，类型必须先能表达这个区别。

**分类规则**（`ClassifyCallFault`，顺序即优先级）：

| 条件 | category | 计入故障 |
|---|---|---|
| `err == nil` | `""` | 否 |
| `ctx.Err() == context.DeadlineExceeded`（或 `errors.Is(err, context.DeadlineExceeded)`） | `timeout` | 是 |
| `ctx.Err() == context.Canceled`（或 `errors.Is(err, context.Canceled)`） | `""` | **否**——调用方取消不是插件的错 |
| `errors.Is(err, ErrGuestABI)` | `abi` | 是 |
| `errors.Is(err, ErrGuestTrap)` | `trap` | 是 |
| 其它 | `trap` | 是 |

最后一条是刻意的：一个分不出类的失败仍然是插件没答上来，宁可记成 trap 也不要漏掉——漏掉才是健康度失效的方式。

- [ ] **Step 1: 写失败测试**

新建 `internal/plugin/host/fault_test.go`：

```go
package host

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestClassifyCallFaultNilErrorIsNotAFault(t *testing.T) {
	category, isFault := ClassifyCallFault(context.Background(), nil)
	if category != "" || isFault {
		t.Errorf("ClassifyCallFault(ctx, nil) = (%q, %t), want (\"\", false)", category, isFault)
	}
}

func TestClassifyCallFaultDeadlineIsTimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), timeInThePast())
	defer cancel()
	category, isFault := ClassifyCallFault(ctx, fmt.Errorf("invoke op=1: %w", context.DeadlineExceeded))
	if category != CategoryTimeout || !isFault {
		t.Errorf("ClassifyCallFault on a deadline = (%q, %t), want (%q, true)", category, isFault, CategoryTimeout)
	}
}

func TestClassifyCallFaultCancellationIsNotThePluginsFault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	category, isFault := ClassifyCallFault(ctx, fmt.Errorf("invoke op=1: %w", context.Canceled))
	if isFault {
		t.Errorf("ClassifyCallFault on a caller cancellation = (%q, true), want isFault=false: "+
			"a caller who walked away has not broken the plugin", category)
	}
}

func TestClassifyCallFaultABISentinel(t *testing.T) {
	err := fmt.Errorf("alloc 8 bytes: %w", ErrGuestABI)
	category, isFault := ClassifyCallFault(context.Background(), err)
	if category != CategoryABI || !isFault {
		t.Errorf("ClassifyCallFault(%v) = (%q, %t), want (%q, true)", err, category, isFault, CategoryABI)
	}
}

func TestClassifyCallFaultTrapSentinel(t *testing.T) {
	err := fmt.Errorf("invoke op=1: %w", ErrGuestTrap)
	category, isFault := ClassifyCallFault(context.Background(), err)
	if category != CategoryTrap || !isFault {
		t.Errorf("ClassifyCallFault(%v) = (%q, %t), want (%q, true)", err, category, isFault, CategoryTrap)
	}
}

func TestClassifyCallFaultUnrecognizedCountsAsTrap(t *testing.T) {
	category, isFault := ClassifyCallFault(context.Background(), errors.New("something nobody classified"))
	if category != CategoryTrap || !isFault {
		t.Errorf("ClassifyCallFault on an unclassified error = (%q, %t), want (%q, true): "+
			"an unclassifiable failure is still a failure", category, isFault, CategoryTrap)
	}
}
```

`timeInThePast` 放在同文件底部：

```go
func timeInThePast() time.Time { return time.Now().Add(-time.Hour) }
```

（记得 import `"time"`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/plugin/host/ -run TestClassifyCallFault -v`
Expected: FAIL，`undefined: ClassifyCallFault`

- [ ] **Step 3: 实现分类**

新建 `internal/plugin/host/fault.go`：

```go
package host

import (
	"context"
	"errors"
)

// ErrGuestABI marks a failure in the ABI mechanics themselves: the guest's
// allocator returned a null pointer, a result slot pointed outside linear
// memory, or a body could not be decoded. It is a fault of the plugin, not of
// the request, which is why it is counted.
var ErrGuestABI = errors.New("guest abi violation")

// ErrGuestTrap marks a wasm trap: an out-of-bounds access, unreachable, a
// division by zero — anything wazero aborted the module for.
var ErrGuestTrap = errors.New("guest trap")

// ClassifyCallFault decides which plugin/call_failed category one guest call
// failure belongs to, and whether it counts toward the plugin's health.
//
// A caller's cancellation is deliberately NOT a fault: the plugin never got to
// answer, and counting it would let a busy operator unload a healthy plugin by
// pressing Ctrl-C. A deadline IS one: the plugin had its whole configured
// budget and did not answer within it.
//
// An error that matches nothing is counted as a trap rather than ignored. The
// alternative — "unclassified means not a fault" — is exactly how a health
// counter quietly stops counting.
func ClassifyCallFault(ctx context.Context, err error) (category string, isFault bool) {
	if err == nil {
		return "", false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return CategoryTimeout, true
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return "", false
	}
	if errors.Is(err, ErrGuestABI) {
		return CategoryABI, true
	}
	return CategoryTrap, true
}
```

在 `internal/plugin/host/hostcall.go` 的 `CategoryDenied` 下方补三个常量：

```go
// CategoryTimeout / CategoryTrap / CategoryABI are the remaining
// plugin/call_failed categories from the design doc's error taxonomy. Unlike
// CategoryDenied — which means the plugin overstepped — all three mean the
// plugin failed to answer, and all three count toward its health.
const (
	CategoryTimeout = "timeout"
	CategoryTrap    = "trap"
	CategoryABI     = "abi"
)
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/plugin/host/ -run TestClassifyCallFault -v`
Expected: PASS（6 个）

- [ ] **Step 5: 把 ABI 哨兵挂到 Invoke 的失败路径上**

`internal/plugin/host/instance.go` 的 `Invoke` 里，四处**属于 ABI 的**失败改为包 `ErrGuestABI`（保留原有措辞，只加 `%w`）：

```go
// alloc 返回 0
return nil, fmt.Errorf("alloc %d bytes: guest returned null pointer: %w", len(in), ErrGuestABI)
// 写入参越界
return nil, fmt.Errorf("write %d bytes at %d: out of range: %w", len(in), ptr, ErrGuestABI)
// 读结果越界
rerr := fmt.Errorf("read result at %d len %d: out of range: %w", outPtr, outLen, ErrGuestABI)
// 释放结果失败
return nil, fmt.Errorf("free result %d bytes at %d: %w: %w", outLen, outPtr, ferr, ErrGuestABI)
```

`invoke.Call` 的失败包 `ErrGuestTrap`：

```go
return nil, fmt.Errorf("invoke op=%d: %w: %w", op, ierr, ErrGuestTrap)
```

- [ ] **Step 6: 补一条端到端分类测试**

在 `internal/plugin/host/fault_test.go` 追加（`plugin.wasm` 夹具的 op 93 返回一个越界指针，op 99 是死循环，见 `testdata/README.md`）：

```go
func TestInvokeBogusResultIsAnABIFault(t *testing.T) {
	ctx := context.Background()
	inst := newFixtureInstance(t, ctx) // 既有夹具助手，见 instance_test.go
	_, err := inst.Invoke(ctx, 93, nil)
	if err == nil {
		t.Fatal("Invoke(op=93) = nil error, want an ABI failure: the fixture returns a pointer outside memory")
	}
	category, isFault := ClassifyCallFault(ctx, err)
	if category != CategoryABI || !isFault {
		t.Errorf("ClassifyCallFault(%v) = (%q, %t), want (%q, true)", err, category, isFault, CategoryABI)
	}
}
```

先读 `internal/plugin/host/instance_test.go`，用它已有的夹具构造方式（函数名以那里为准，不要新造一个平行夹具）。

- [ ] **Step 7: 全量测试 + 变异验证**

Run: `go test ./...`
Expected: 全绿

变异：把 `ClassifyCallFault` 最后一行改成 `return "", false`，重跑
`go test ./internal/plugin/host/ -run TestClassifyCallFault`，确认
`TestClassifyCallFaultUnrecognizedCountsAsTrap` FAIL，贴失败输出，然后还原。

- [ ] **Step 8: 提交**

```bash
git add internal/plugin/host/fault.go internal/plugin/host/fault_test.go internal/plugin/host/hostcall.go internal/plugin/host/instance.go
git commit -m "feat(plugin): 给 guest 调用失败挂上可判别的分类哨兵"
```

---

### Task 2: 工具调用失败时发出带 category 的 plugin/call_failed

**Files:**
- Modify: `internal/plugin/host/contribute.go`（`contributeTools`、`pluginToolHandler` 签名与实现）
- Test: `internal/plugin/host/contribute_test.go`

**Interfaces:**
- Consumes: `host.ClassifyCallFault`、`host.CategoryABI/CategoryTrap/CategoryTimeout`（Task 1）
- Produces:
  - `func pluginToolHandler(pluginName, toolName string, guest guestCaller, report faultReporter) tool.Handler`
  - `type faultReporter func(ctx context.Context, category, toolName, reason string)`

今天 `plugin/call_failed` 只在 host 函数**拒绝**时发（`hostcall.go` 的 `publishDenial`）。guest 自己 trap、超时、ABI 违规，一个事件都没有——排查时只能看到工具返回了 error。

- [ ] **Step 1: 写失败测试**

在 `internal/plugin/host/contribute_test.go` 追加。夹具沿用该文件既有的 `guestCaller` 假实现（先读一遍它现在怎么造）：

```go
func TestPluginToolHandlerReportsATrapAsACallFailedFault(t *testing.T) {
	var got []string
	report := func(_ context.Context, category, toolName, reason string) {
		got = append(got, category+":"+toolName+":"+reason)
	}
	guest := failingGuest{err: fmt.Errorf("invoke op=1: %w", ErrGuestTrap)} // 既有假 guest，字段名以文件现状为准
	handler := pluginToolHandler("legion-demo", "demo_echo", guest, report)

	_, err := handler.Handle(context.Background(), domain.ToolCall{ID: "c1", Name: "demo_echo"})
	if err == nil {
		t.Fatal("handler on a trapping guest = nil error, want the trap to propagate")
	}
	if len(got) != 1 {
		t.Fatalf("fault reports = %v, want exactly one", got)
	}
	if !strings.HasPrefix(got[0], CategoryTrap+":demo_echo:") {
		t.Errorf("fault report = %q, want it to start with %q", got[0], CategoryTrap+":demo_echo:")
	}
}

func TestPluginToolHandlerReportsNothingOnSuccess(t *testing.T) {
	var got []string
	report := func(_ context.Context, category, toolName, reason string) {
		got = append(got, category)
	}
	guest := okGuest{body: []byte(`{"success":true,"output":"fine"}`)} // 既有假 guest
	handler := pluginToolHandler("legion-demo", "demo_echo", guest, report)

	if _, err := handler.Handle(context.Background(), domain.ToolCall{ID: "c1", Name: "demo_echo"}); err != nil {
		t.Fatalf("handler on a healthy guest = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("fault reports on success = %v, want none", got)
	}
}

func TestPluginToolHandlerDoesNotReportAFailedToolResultAsAFault(t *testing.T) {
	// 一个工具说"我没做成"是业务结果，不是插件故障（设计文档 §6.9：plugin_error 不计）。
	var got []string
	report := func(_ context.Context, category, toolName, reason string) { got = append(got, category) }
	guest := okGuest{body: []byte(`{"success":false,"error":"missing argument"}`)}
	handler := pluginToolHandler("legion-demo", "demo_echo", guest, report)

	result, err := handler.Handle(context.Background(), domain.ToolCall{ID: "c1", Name: "demo_echo"})
	if err != nil {
		t.Fatalf("handler on a failed tool result = %v, want nil error: a failed result is an answer", err)
	}
	if result.Success {
		t.Error("result.Success = true, want false")
	}
	if len(got) != 0 {
		t.Errorf("fault reports for a failed tool result = %v, want none", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/plugin/host/ -run TestPluginToolHandler -v`
Expected: FAIL，`too many arguments in call to pluginToolHandler`

- [ ] **Step 3: 实现**

`contribute.go`：

```go
// faultReporter is how a tool handler tells its owner that a call failed in a
// way that counts toward the plugin's health. reason is the error text, kept
// short enough to travel inside an event message.
//
// It is a plain function rather than an interface because there is exactly one
// implementation (the Loader's counter) and one test double.
type faultReporter func(ctx context.Context, category, toolName, reason string)

func pluginToolHandler(pluginName, toolName string, guest guestCaller, report faultReporter) tool.Handler {
	return tool.HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		fail := func(err error) (domain.ToolResult, error) {
			if category, isFault := ClassifyCallFault(ctx, err); isFault && report != nil {
				report(ctx, category, toolName, err.Error())
			}
			return domain.ToolResult{}, err
		}
		// …既有的 encode / guest.call / decodeToolResult 三步不变，
		// 每一处 `return domain.ToolResult{}, fmt.Errorf(...)` 改成 `return fail(fmt.Errorf(...))`
	})
}
```

`contributeTools` 里把 reporter 传下去：

```go
handler := pluginToolHandler(spec.Name, descriptor.Name, guest, spec.Deps.OnFault)
```

（`Deps.OnFault` 在 Task 3 加；本 task 先把参数打通，用 `nil` 也能编译——但**不要**留 `nil`，Task 3 一起完成。若本 task 先落地，`spec.Deps.OnFault` 尚不存在，则先传 `nil` 并在 Task 3 改掉，commit message 里写明。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/plugin/host/ -run TestPluginToolHandler -v`
Expected: PASS（3 个）

- [ ] **Step 5: 事件也要发**

在 `fail` 里，除了 `report`，还要发事件（复用既有的 `formatCallFailedMessage`，`hostFunc` 位置放 `"tool:"+toolName`）：

```go
if deps.Events != nil {
	deps.Events.Publish(ctx, domain.RuntimeEvent{
		Type:    RuntimeEventCallFailed,
		Message: formatCallFailedMessage(category, pluginName, "tool:"+toolName, err.Error()),
	})
}
```

`Deps` 要传进 handler：把 `pluginToolHandler` 的参数从 `pluginName string` 换成整个 `deps Deps`（它已经有 `PluginName`）。相应改 `contributeTools` 的调用点与上面三个测试。

先读 `hostcall.go:703` 的 `publishDenial` 看事件字段怎么填（`TaskID` 从 ctx 取的方式以那里为准），照抄同一种写法。

补一条测试：

```go
func TestPluginToolHandlerPublishesCallFailedWithCategory(t *testing.T) {
	bus := &recordingEventBus{} // 既有测试替身，以文件现状为准
	deps := Deps{PluginName: "legion-demo", Events: bus}
	handler := pluginToolHandler(deps, "demo_echo", failingGuest{err: fmt.Errorf("invoke op=1: %w", ErrGuestTrap)}, nil)

	if _, err := handler.Handle(context.Background(), domain.ToolCall{ID: "c1", Name: "demo_echo"}); err == nil {
		t.Fatal("want the trap to propagate")
	}
	if len(bus.events) != 1 {
		t.Fatalf("published %d events, want 1", len(bus.events))
	}
	if !EventHasCategory(bus.events[0], CategoryTrap) {
		t.Errorf("event %q does not carry category %q", bus.events[0].Message, CategoryTrap)
	}
}
```

- [ ] **Step 6: 全量测试 + 变异验证**

Run: `go test ./... && go test ./plugin_example/...`
Expected: 全绿

变异：把 `fail` 里的 `if isFault` 改成 `if false`，确认
`TestPluginToolHandlerReportsATrapAsACallFailedFault` 与
`TestPluginToolHandlerPublishesCallFailedWithCategory` 双双 FAIL，贴输出后还原。

- [ ] **Step 7: 提交**

```bash
git add internal/plugin/host/contribute.go internal/plugin/host/contribute_test.go
git commit -m "feat(plugin): guest 调用失败发出带 category 的 plugin/call_failed"
```

---

### Task 3: 连续故障计数与阈值配置

**Files:**
- Modify: `internal/plugin/host/hostfunc.go`（`Deps` 加 `OnFault`）
- Modify: `internal/plugin/loader/loader.go`（`instance` 加计数、`prepare` 装配 reporter）
- Modify: `internal/config/config.go`（`PluginsConfig` 加 `Health`、默认值、校验）
- Modify: `internal/cli/plugins_command.go`（把配置传进 loader.Config）
- Test: `internal/plugin/loader/health_test.go`、`internal/config/config_test.go`

**Interfaces:**
- Consumes: Task 2 的 `faultReporter` 形状
- Produces:
  - `Deps.OnFault func(ctx context.Context, category, toolName, reason string)`
  - `config.PluginHealthConfig{ MaxConsecutiveFaults int `json:"max_consecutive_faults"` }`（默认 5）
  - `loader.Config.MaxConsecutiveFaults int`
  - `(*Loader).recordFault(name, category string) (unloadNow bool)`

**计数语义**（写死在测试里，别改）：

| 事件 | 计数 |
|---|---|
| 一次 `timeout` / `trap` / `abi` | +1 |
| 一次成功调用 | **清零** |
| 一次 `denied` | 不变（插件越界，不是坏了；`hostcall` 那条路径不经过本计数器） |
| 失败的 `ToolResult`（`success:false`） | 不变 |

- [ ] **Step 1: 写失败测试（配置）**

`internal/config/config_test.go` 追加：

```go
func TestLoadPluginsHealthDefaultsToFive(t *testing.T) {
	path := writeConfig(t, `{"plugins":{"manifest":"plugins.json","root":"plugins"}}`) // 既有助手
	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Plugins.Health.MaxConsecutiveFaults != 5 {
		t.Errorf("Plugins.Health.MaxConsecutiveFaults = %d, want the default 5",
			cfg.Plugins.Health.MaxConsecutiveFaults)
	}
}

func TestLoadRejectsANonPositiveHealthThreshold(t *testing.T) {
	path := writeConfig(t, `{"plugins":{"manifest":"plugins.json","root":"plugins","health":{"max_consecutive_faults":0}}}`)
	_, err := Load(context.Background(), Options{Path: path})
	if err == nil {
		t.Fatal("Load with max_consecutive_faults=0 = nil error, want a refusal: " +
			"0 is not 'never unload', it is an unstated policy")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/config/ -run TestLoadPluginsHealth -v` 与
`go test ./internal/config/ -run TestLoadRejectsANonPositiveHealth -v`
Expected: FAIL，`cfg.Plugins.Health undefined`

- [ ] **Step 3: 实现配置**

`internal/config/config.go`，`PluginsConfig` 增加字段：

```go
	// Health bounds how many CONSECUTIVE faults (timeout / trap / abi — see
	// internal/plugin/host.ClassifyCallFault) one plugin may produce before the
	// deployment unloads it. A successful call resets the count; a denial does
	// not count at all (the plugin overstepped, it is not broken).
	Health PluginHealthConfig `json:"health"`
```

```go
// PluginHealthConfig is the deployment's tolerance for a misbehaving plugin.
//
// MaxConsecutiveFaults has NO "0 means unlimited" reading: zero is refused by
// validatePlugins whenever a manifest is configured. A deployment that wants a
// high tolerance states a high number; leaving the policy unstated and getting
// "never unload" is exactly the silent degradation this project forbids.
type PluginHealthConfig struct {
	// MaxConsecutiveFaults is the count at which a plugin is unloaded.
	// Default 5.
	MaxConsecutiveFaults int `json:"max_consecutive_faults"`
}
```

`DefaultConfig()` 的 `Plugins` 字面量加 `Health: PluginHealthConfig{MaxConsecutiveFaults: 5}`；
`validatePlugins` 加：

```go
	if cfg.Health.MaxConsecutiveFaults <= 0 {
		return fmt.Errorf("plugins.health.max_consecutive_faults is %d; it must be positive "+
			"(a deployment that tolerates more failures states a larger number; there is no "+
			"'unlimited' reading of zero)", cfg.Health.MaxConsecutiveFaults)
	}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/config/ -run "TestLoadPluginsHealth|TestLoadRejectsANonPositiveHealth" -v`
Expected: PASS

- [ ] **Step 5: 写失败测试（计数）**

新建 `internal/plugin/loader/health_test.go`：

```go
func TestRecordFaultUnloadsAtTheThreshold(t *testing.T) {
	h := newHarness(t)                       // 既有测试夹具，见 loader_test.go
	h.cfg.MaxConsecutiveFaults = 3
	l := h.loader(t)
	h.mountOne(t, l, "plugin-a")             // 既有助手：挂载一个夹具插件

	for i := 1; i < 3; i++ {
		if unload := l.recordFault("plugin-a", host.CategoryTrap); unload {
			t.Fatalf("fault %d of 3 asked for an unload; want it to wait for the threshold", i)
		}
	}
	if unload := l.recordFault("plugin-a", host.CategoryTrap); !unload {
		t.Error("the 3rd consecutive fault did not ask for an unload, want it to")
	}
}

func TestRecordFaultResetsOnSuccess(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxConsecutiveFaults = 2
	l := h.loader(t)
	h.mountOne(t, l, "plugin-a")

	l.recordFault("plugin-a", host.CategoryTimeout)
	l.recordSuccess("plugin-a")
	if unload := l.recordFault("plugin-a", host.CategoryTimeout); unload {
		t.Error("a fault after a success asked for an unload; the counter must have reset")
	}
}

func TestRecordFaultIgnoresDenials(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxConsecutiveFaults = 1
	l := h.loader(t)
	h.mountOne(t, l, "plugin-a")

	if unload := l.recordFault("plugin-a", host.CategoryDenied); unload {
		t.Error("a denial asked for an unload; a denial means the plugin overstepped, not that it is broken")
	}
}
```

- [ ] **Step 6: 跑测试确认失败**

Run: `go test ./internal/plugin/loader/ -run TestRecordFault -v`
Expected: FAIL，`l.recordFault undefined`

- [ ] **Step 7: 实现计数**

`internal/plugin/host/hostfunc.go` 的 `Deps` 加字段：

```go
	// OnFault is called when a call into this plugin fails in a way that counts
	// toward its health (see ClassifyCallFault). It is CONTRACT-DECLARED
	// OPTIONAL: a nil OnFault means nobody is counting, which is what an
	// embedder that mounts a plugin outside the Loader gets.
	OnFault func(ctx context.Context, category, toolName, reason string)
```

`internal/plugin/loader/loader.go`：`instance` 加 `faults int`；`Config` 加
`MaxConsecutiveFaults int`（`newLoader` 校验 > 0）；新增：

```go
// recordFault counts one health-relevant failure of a mounted plugin and
// reports whether the deployment must now unload it.
//
// It takes the instances lock: faults arrive from tool handlers on whatever
// goroutine the model's tool loop runs on, while convergence mutates the same
// map. The counter lives on the instance rather than in a side map so it dies
// with the mount — a replaced plugin starts clean, which is the only reading
// that makes sense for "consecutive failures of THIS instance".
func (l *Loader) recordFault(name, category string) bool {
	if category == host.CategoryDenied {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	inst := l.instances[name]
	if inst == nil {
		return false
	}
	inst.faults++
	return inst.faults >= l.cfg.MaxConsecutiveFaults
}

// recordSuccess clears the fault counter: health is about CONSECUTIVE
// failures, so one answered call means the plugin is answering again.
func (l *Loader) recordSuccess(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if inst := l.instances[name]; inst != nil {
		inst.faults = 0
	}
}
```

（`l.mu` / `l.instances` 的真实字段名以 `loader.go` 现状为准。）

在 `prepare`（`loader.go:1048` 附近）装配 reporter：

```go
	deps := l.deps(entry.Name, entry.Config)
	name := entry.Name
	deps.OnFault = func(ctx context.Context, category, toolName, reason string) {
		if l.recordFault(name, category) {
			l.unloadUnhealthy(ctx, name, category, toolName, reason)
		}
	}
```

`unloadUnhealthy` 在 Task 4 实现；本 task 先写成只记日志的私有方法，Task 4 补齐——**不要**留空函数体，先实现为
`l.publish(ctx, "plugin/unloaded", …)` 之外的一行 `l.logger.Warn(...)`，Task 4 换掉。

`recordSuccess` 的调用点：`pluginToolHandler` 成功返回时也要报一次。为此把 `Deps.OnFault` 之外再加一个
`Deps.OnSuccess func(ctx context.Context, toolName string)`，同样在 `prepare` 里绑到 `l.recordSuccess`。

`internal/cli/plugins_command.go` 的 `newPluginLoader` 把
`cfg.Plugins.Health.MaxConsecutiveFaults` 填进 `loader.Config`。

- [ ] **Step 8: 跑测试确认通过 + -race**

Run: `go test ./internal/plugin/loader/ -run TestRecordFault -v`
Expected: PASS（3 个）

Run: `go test -race ./internal/plugin/...`
Expected: 全绿、无 race 报告

- [ ] **Step 9: 全量测试 + 变异验证**

Run: `go test ./... && go test ./plugin_example/...`

变异：把 `recordFault` 的 `inst.faults++` 删掉，确认
`TestRecordFaultUnloadsAtTheThreshold` FAIL；还原。再把 `recordSuccess` 的清零删掉，确认
`TestRecordFaultResetsOnSuccess` FAIL；还原。两次失败输出都留在报告里。

- [ ] **Step 10: 提交**

```bash
git add internal/config/config.go internal/config/config_test.go internal/plugin/host/hostfunc.go internal/plugin/loader/loader.go internal/plugin/loader/health_test.go internal/cli/plugins_command.go
git commit -m "feat(plugin): 连续故障计数与 plugins.health 阈值"
```

---

### Task 4: 超阈值自动卸载并说清楚原因

**Files:**
- Modify: `internal/plugin/loader/loader.go`（`unloadUnhealthy`、`Status` 的 `lastError`）
- Modify: `internal/server/plugins.go`（`PluginView.Detail` 的健康度措辞）
- Test: `internal/plugin/loader/health_test.go`

**Interfaces:**
- Consumes: `(*Loader).recordFault`（Task 3）、`(*Loader).unload`（既有，`loader.go:1249`）
- Produces: `func (l *Loader) unloadUnhealthy(ctx context.Context, name, category, toolName, reason string)`

**不做**：自动重试、自动重新加载。设计文档写明「重试一个 trap 的插件通常只是重复 trap」。运维要它回来，走 `agent plugins reload`。

- [ ] **Step 1: 写失败测试**

```go
func TestUnhealthyPluginIsUnloadedAndSaysWhy(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxConsecutiveFaults = 1
	l := h.loader(t)
	h.mountOne(t, l, "plugin-a")

	l.unloadUnhealthy(context.Background(), "plugin-a", host.CategoryTrap, "demo_echo",
		"invoke op=1: guest trap")

	statuses := l.Status()
	if len(statuses) != 1 {
		t.Fatalf("Status() = %d rows, want 1", len(statuses))
	}
	if statuses[0].State != StateFailed {
		t.Errorf("state = %q, want %q", statuses[0].State, StateFailed)
	}
	if !strings.Contains(statuses[0].LastError, "health") ||
		!strings.Contains(statuses[0].LastError, host.CategoryTrap) {
		t.Errorf("LastError = %q, want it to name the health policy and the category", statuses[0].LastError)
	}
	if !h.published(t, "plugin/unloaded", "reason=health") {
		t.Error("no plugin/unloaded event with reason=health; an automatic unload must be visible")
	}
}

func TestUnhealthyUnloadDoesNotReloadTheSamePlugin(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxConsecutiveFaults = 1
	l := h.loader(t)
	h.mountOne(t, l, "plugin-a")
	l.unloadUnhealthy(context.Background(), "plugin-a", host.CategoryTrap, "demo_echo", "boom")

	if l.mounted("plugin-a") != nil {
		t.Error("the plugin is still mounted after a health unload; there is no automatic retry by design")
	}
}
```

（`h.published` 是本 task 要在 harness 上补的小助手：扫描测试事件总线里的事件类型与消息子串。若 harness 已有等价物，用既有的。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/plugin/loader/ -run TestUnhealthy -v`
Expected: FAIL，`l.unloadUnhealthy undefined` 或事件断言失败

- [ ] **Step 3: 实现**

```go
// unloadUnhealthy unloads a plugin whose consecutive-fault count crossed the
// deployment's threshold, and leaves behind an explanation that names the
// policy, the category and the tool call that tripped it.
//
// It does NOT reload or retry. A plugin that trapped its way to the threshold
// will usually trap again on the next call, and an automatic retry would turn
// one visible unload into an invisible loop. Bringing it back is an operator
// action: fix the package, then `agent plugins reload`.
func (l *Loader) unloadUnhealthy(ctx context.Context, name, category, toolName, reason string) {
	inst := l.mounted(name)
	if inst == nil {
		return
	}
	detail := fmt.Sprintf("health: %d consecutive faults (last: category=%s tool=%s: %s)",
		l.cfg.MaxConsecutiveFaults, category, toolName, reason)
	revoked, err := l.unload(ctx, inst, "health")
	if err != nil {
		l.logger.Error("unload unhealthy plugin", "plugin", name, "error", err)
		detail += fmt.Sprintf("; unload reported: %v", err)
	}
	l.markFailed(name, detail) // 既有的失败记账路径，函数名以 loader.go 现状为准（见 l.fail）
	l.publish(ctx, "plugin/unloaded",
		fmt.Sprintf("plugin=%s reason=health category=%s revoked=%d", name, category, revoked))
}
```

先读 `loader.go:1139` 的 `fail` 与 `:1249` 的 `unload`，复用它们的记账方式，**不要**新造一套并行的状态写入。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/plugin/loader/ -run TestUnhealthy -v`
Expected: PASS（2 个）

- [ ] **Step 5: 呈现到 HTTP / CLI**

`Status()` 已经把 `lastError` 带出来，`internal/server/plugins.go` 的 `PluginView.Detail` 走的是同一条路，因此
**不需要新字段**。补一条端点层测试确认健康度卸载在 `GET /v1/plugins` 里说得清（放在
`internal/server/plugins_test.go`，沿用该文件既有的假 `PluginConsent`）：

```go
func TestListPluginsSurfacesAHealthUnload(t *testing.T) {
	// consent 假实现返回 state=failed、detail 含 "health: 3 consecutive faults"
	// 断言响应 JSON 的 detail 原样带出该措辞。
}
```

（把注释换成真实断言代码，形状照抄该文件既有的 List 测试。）

- [ ] **Step 6: 全量测试 + 变异验证**

Run: `go test ./... && go test ./plugin_example/...`

变异：把 `unloadUnhealthy` 里的 `l.publish` 那行删掉，确认
`TestUnhealthyPluginIsUnloadedAndSaysWhy` FAIL；还原。

- [ ] **Step 7: 提交**

```bash
git add internal/plugin/loader/loader.go internal/plugin/loader/health_test.go internal/server/plugins_test.go
git commit -m "feat(plugin): 连续故障超阈值自动卸载并说明原因"
```

---

### Task 5: 卸载没收敛干净时发 plugin/unload_leaked

**Files:**
- Modify: `internal/plugin/host/pool.go`（在途计数）
- Modify: `internal/plugin/host/activate.go`（把在途数透出给 Loader）
- Modify: `internal/plugin/loader/loader.go`（`unload` 的 drain 失败分支）
- Test: `internal/plugin/host/pool_test.go`、`internal/plugin/loader/health_test.go`

**Interfaces:**
- Produces:
  - `func (p *pool) inflightCount() int`
  - `func (p *Plugin) InflightCalls() int`
  - 事件 `plugin/unload_leaked`，消息形如 `plugin=<name> inflight=<n> waited=<dur>`

设计文档 §8 的五个事件里，这一个至今没发。今天 drain 超时只会让 `unload` 返回一个 error，被记进 `lastError`——而「有 N 个调用还在里面跑」这个事实没有任何地方说得出来。

- [ ] **Step 1: 写失败测试（pool 计数）**

`internal/plugin/host/pool_test.go` 追加：

```go
func TestPoolInflightCountTracksAcquiredInstances(t *testing.T) {
	p := newTestPool(t, 2) // 既有夹具助手
	if got := p.inflightCount(); got != 0 {
		t.Fatalf("inflightCount on a fresh pool = %d, want 0", got)
	}
	slot, err := p.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := p.inflightCount(); got != 1 {
		t.Errorf("inflightCount after one acquire = %d, want 1", got)
	}
	p.release(slot)
	if got := p.inflightCount(); got != 0 {
		t.Errorf("inflightCount after release = %d, want 0", got)
	}
}
```

（`acquire`/`release` 的真实签名以 `pool.go` 现状为准。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/plugin/host/ -run TestPoolInflightCount -v`
Expected: FAIL，`p.inflightCount undefined`

- [ ] **Step 3: 实现在途计数**

`pool.go`：`inflight sync.WaitGroup` 旁边加 `inflightN atomic.Int64`，在每处
`p.inflight.Add(1)` 后 `p.inflightN.Add(1)`、每处 `p.inflight.Done()` 前 `p.inflightN.Add(-1)`：

```go
// inflightCount reports how many calls are inside this pool right now.
//
// It shadows the WaitGroup rather than replacing it: a WaitGroup cannot be
// read, and the number is only needed for a diagnostic (how much a drain left
// behind), never for a decision. It is therefore allowed to be a moment out of
// date — but it must never go negative, which is why every Add(1)/Done() pair
// updates it in the same place.
func (p *pool) inflightCount() int {
	return int(p.inflightN.Load())
}
```

`activate.go` 的 `Plugin` 加：

```go
// InflightCalls reports how many calls are still inside this plugin. The
// Loader reads it after a drain that timed out, to say how much was left
// behind rather than only that something was.
func (p *Plugin) InflightCalls() int { return p.pool.inflightCount() }
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/plugin/host/ -run TestPoolInflightCount -v` → PASS
Run: `go test -race ./internal/plugin/host/` → 全绿

- [ ] **Step 5: 写失败测试（事件）**

`internal/plugin/loader/health_test.go` 追加：

```go
func TestUnloadPublishesLeakedWhenDrainDoesNotConverge(t *testing.T) {
	h := newHarness(t)
	l := h.loader(t)
	h.mountOne(t, l, "plugin-a")
	h.holdOneCallOpen(t, "plugin-a")   // 本 task 要补的助手：占住一个实例不放，让 drain 超时

	// drain 的等待上限来自 unload 内部的超时；夹具把它调到毫秒级。
	_, _ = l.unload(context.Background(), l.mounted("plugin-a"), "manifest-removed")

	if !h.published(t, "plugin/unload_leaked", "inflight=1") {
		t.Error("no plugin/unload_leaked event; a drain that left calls behind must say how many")
	}
}
```

- [ ] **Step 6: 跑测试确认失败**

Run: `go test ./internal/plugin/loader/ -run TestUnloadPublishesLeaked -v`
Expected: FAIL（事件没发）

- [ ] **Step 7: 实现事件**

`loader.go` 的 `unload`：drain 返回错误时，除既有的错误传播外，加

```go
	// A drain that did not converge is the design doc's plugin/unload_leaked:
	// the wasm runtime stays alive with calls inside it, and the ONLY place
	// that fact is visible is this event. The error still propagates — this is
	// an added report, not a swallow.
	l.publish(ctx, "plugin/unload_leaked",
		fmt.Sprintf("plugin=%s inflight=%d waited=%s", inst.name, inst.plugin.InflightCalls(), waited))
```

`waited` 用 `unload` 里已有的等待上限值（若当前是 ctx 派生的 deadline，取该常量/配置值，**不要**新引入一个）。

- [ ] **Step 8: 跑测试确认通过**

Run: `go test ./internal/plugin/loader/ -run TestUnloadPublishesLeaked -v` → PASS

- [ ] **Step 9: 全量测试 + 变异验证**

Run: `go test ./... && go test -race ./internal/plugin/... && go test ./plugin_example/...`

变异：把 `inflightN.Add(-1)` 删掉，确认 `TestPoolInflightCountTracksAcquiredInstances` FAIL；还原。

- [ ] **Step 10: 提交**

```bash
git add internal/plugin/host/pool.go internal/plugin/host/pool_test.go internal/plugin/host/activate.go internal/plugin/loader/loader.go internal/plugin/loader/health_test.go
git commit -m "feat(plugin): 卸载未收敛时发 plugin/unload_leaked"
```

---

### Task 6: 文档与配置示例

**Files:**
- Modify: `configs/agent.full.example.json`、`configs/agent.complete.example.json`（如含 plugins 段）
- Modify: docs 仓 `agents/reference/reference-legion-agent-plugins-001.md`（§6.1 配置表、§8 状态与收敛、§9 排错表）
- Modify: docs 仓 `design/architecture/legion-plugin-system.md`（§9 路线表把 G1 标记为已交付）

**Interfaces:**
- Consumes: Task 3 的配置键名 `plugins.health.max_consecutive_faults`、Task 4/5 的事件与措辞

- [ ] **Step 1: 配置示例**

在示例配置的 `plugins` 段加：

```json
    "health": { "max_consecutive_faults": 5 }
```

- [ ] **Step 2: 参考手册**

- §6.1 配置表加一行：`health.max_consecutive_faults` / 默认 5 / 「连续故障（timeout·trap·abi）达到此数即自动卸载；成功调用清零；denied 不计。0 不是『不限』，是配置错误」。
- §8 加一小节「健康度」：四类失败与是否计入、自动卸载后状态是 `failed` 且 `detail` 说明原因、**不会自动重试**、要它回来走 `reload`。
- §9 排错表加两行：`detail` 里出现 `health: N consecutive faults` → 插件反复失败被自动卸载；出现 `plugin/unload_leaked` 事件 → 卸载时仍有在途调用。

- [ ] **Step 3: 设计文档路线表**

`design/architecture/legion-plugin-system.md` §9 增加一行 G1 并标 ✅，附本计划路径。

- [ ] **Step 4: 提交（两个仓分别提）**

```bash
# server 仓
git add configs/agent.full.example.json configs/agent.complete.example.json
git commit -m "docs(plugin): 配置示例补上 plugins.health"
```

docs 仓另起分支与 PR（该仓与 server 仓提交分离，见 `2026-08-28-gui-plugin-walkthrough.md` 的教训：从 master 切分支前先确认本地没有未推的文档提交）。

---

## 自检

**范围覆盖**：§6.9 的五类分类 → Task 1（denied 已有）；健康度计数与自动卸载 → Task 3/4；§8 缺的 `plugin/unload_leaked` → Task 5；配置与文档 → Task 3/6。设计文档里「不做自动重试」这条在 Task 4 的实现注释与测试（`TestUnhealthyUnloadDoesNotReloadTheSamePlugin`）里各钉一次。

**类型一致性**：`ClassifyCallFault(ctx, err) (string, bool)` 在 Task 1 定义、Task 2 使用；`faultReporter` 的四个参数顺序 `(ctx, category, toolName, reason)` 在 Task 2 定义、Task 3 的 `Deps.OnFault` 与 `prepare` 装配处一致；`recordFault(name, category) bool` 在 Task 3 定义、Task 4 复用；`InflightCalls() int` 在 Task 5 定义并当场使用。

**已知留白**（**不是** placeholder，是刻意留给实现者按现状对齐的名字）：`newHarness` / `h.mountOne` / `h.published` / `newTestPool` / `failingGuest` / `okGuest` / `recordingEventBus` 这些测试夹具的**确切名字以各文件现状为准**——本仓每个包都有自己的夹具助手，新造一套平行的会比复用差。每处都在正文里点明了「以文件现状为准」。
