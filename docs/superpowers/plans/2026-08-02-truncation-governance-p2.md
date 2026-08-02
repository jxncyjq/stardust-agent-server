# 工具截断治理 P2（循环熔断）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 补上按**工具名（不看参数）**的熔断维度：单工具每 task 总调用达 loop cap(30) 硬顶即终止工具循环；单工具累计失败达 warn(3) 即提示模型停手。填补现有 `callsKey`(name+arguments) 签名熔断绕不过"同工具换参数重试"的缺口——那正是把 session-…955100 拖到 60 次 fetch_url 调用的失败模式。

**Architecture:** 复用现有 `repeatGuard`（`internal/runtime/messages.go`，一个 `map[string]int` 计数器 + `record()`）。`loopState` 新增两个 per-tool-name 计数器 `toolNameGuard`/`toolFailGuard`，在 `runToolLoop`（`internal/runtime/runtime.go`）与现有签名熔断并排接线：loop cap 命中走现有 `loopCut`→closing 路径终止；same-tool 失败达阈值 `appendUser` 警告。全用常量（YAGNI：不新增 config；spec §7 的 halt@8+hard_stop_enabled 与预算窗口缩放留后续）。

**Tech Stack:** Go；复用 `repeatGuard`；单测 `internal/runtime/messages_test.go` + 集成测 `internal/runtime/multiturn_test.go` 风格。

**参考 spec:** `docs/superpowers/specs/2026-08-01-tool-result-truncation-governance-design.md`（§7 P2）
**前置:** P0（截断自我描述）、P1（统一落盘分页）已合入 master。

**现有机制（不改，P2 与之并排）:** `runToolLoop:502-544` 已有 `repeatGuard.record(callsKey)`（name+arguments 签名）+ `repeatedCallStreak`，常量 `repeatWarnStreak=3`/`repeatAbortStreak=8`/`repeatWarnCount=4`/`repeatAbortCount=6`（messages.go:155-166），命中 abort → `loopCut=true; break` → 循环退出后 `:567` 的 closing（final inference，指示模型直接作答）。P2 loop cap 复用这条 loopCut→closing 路径。

**全局门槛（commit 前）：** 在 `legion/legionAgent` 目录 `go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 空。

---

## 文件结构

| 文件 | 动作 | 职责 |
|------|------|------|
| `internal/runtime/messages.go` | 修改 | 加常量 `toolLoopCap`/`toolSameFailWarn`（现有 repeat 常量附近） |
| `internal/runtime/runtime.go` | 修改 | `loopState` 加 `toolNameGuard`/`toolFailGuard` 字段 + 两处初始化；`runToolLoop` 接线 loop cap 与 same-tool 失败 warn |
| `internal/runtime/messages_test.go` | 修改 | 单测 guard 计数语义 |
| `internal/runtime/toolguard_test.go` | 新建 | 集成测 loop cap 终止 + same-tool 失败 warn |

---

## Task 1: 常量 + loopState 计数器字段

只加数据结构与初始化，行为不变。

**Files:**
- Modify: `internal/runtime/messages.go`（常量）
- Modify: `internal/runtime/runtime.go`（loopState 字段 + 两处初始化）
- Test: `internal/runtime/messages_test.go`

- [ ] **Step 1: 写测试——per-tool-name guard 计数（复用 repeatGuard）**

在 `internal/runtime/messages_test.go` 追加（验证同一 repeatGuard 类型按 tool name 计数的语义，供 P2 复用）：

```go
func TestRepeatGuardCountsPerToolName(t *testing.T) {
	g := newRepeatGuard()
	// Same tool "fetch_url" with different arguments still increments the same
	// per-name counter — this is what the callsKey(name+args) guard misses.
	if n := g.record("fetch_url"); n != 1 {
		t.Fatalf("first record = %d, want 1", n)
	}
	if n := g.record("fetch_url"); n != 2 {
		t.Fatalf("second record = %d, want 2", n)
	}
	if n := g.record("read_file"); n != 1 {
		t.Fatalf("different tool name resets its own count, got %d, want 1", n)
	}
	if n := g.record("fetch_url"); n != 3 {
		t.Fatalf("third fetch_url = %d, want 3", n)
	}
}
```

- [ ] **Step 2: 跑测试确认通过（repeatGuard 已有 record，这测的是复用语义）**

Run: `go test ./internal/runtime/ -run TestRepeatGuardCountsPerToolName -v`
Expected: PASS（`repeatGuard.record` 已存在；此测锁定"按 name 计数"语义，无需新代码）

- [ ] **Step 3: 加常量**

在 `internal/runtime/messages.go` 现有 repeat 常量块（messages.go:146-167，`repeatAbortCount=6` 之后、`)` 之前）加：

```go
	// toolLoopCap bounds how many times ONE tool (keyed by NAME ONLY, ignoring
	// arguments) may be called across a whole task. The repeatWarn/Abort guards
	// key on callsKey (name+arguments), so "same tool, different args" retries
	// slip past them — the failure that ran task …955100 to 60 fetch_url calls.
	// This caps the tool regardless of argument variation; hitting it cuts the
	// loop via the same loopCut→closing path as the signature guard. It is a hard
	// ceiling (no config toggle): a runaway loop must stop.
	toolLoopCap = 30
	// toolSameFailWarn is how many times ONE tool (by NAME) may FAIL across a task
	// before the model is warned to stop retrying it and answer with what it has.
	// Warning only — no hard halt (spec §7 leaves halt@8+hard_stop for later).
	toolSameFailWarn = 3
```

- [ ] **Step 4: loopState 加两个计数器字段**

在 `internal/runtime/runtime.go` 的 `loopState` struct（runtime.go:189，`repeatGuard *repeatGuard` 字段附近 :200）加：

```go
	// toolNameGuard counts calls per tool NAME (ignoring arguments) across the
	// task, backing the toolLoopCap runaway guard that the name+arguments
	// repeatGuard cannot see. toolFailGuard counts per-name FAILURES for the
	// same-tool-failure warning. Both one per RunTask, like repeatGuard.
	toolNameGuard *repeatGuard
	toolFailGuard *repeatGuard
```

- [ ] **Step 5: 两处 loopState 初始化补 guard**

`runtime.go` 有两处 `loopState{...}` 初始化，均含 `repeatGuard: newRepeatGuard(),`：
- fresh 路径（runtime.go:459-473，`repeatGuard: newRepeatGuard()` @471）
- resume 路径（runtime.go:405-430 区，`repeatGuard: newRepeatGuard()` @427）

在**两处**的 `repeatGuard: newRepeatGuard(),` 行下方各加：

```go
			toolNameGuard:    newRepeatGuard(),
			toolFailGuard:    newRepeatGuard(),
```

> 缩进对齐所在字面量（fresh 处是 2 个 tab，resume 处 4 个 tab——照该块现有字段对齐）。grep `repeatGuard:\s*newRepeatGuard` 确认改全两处。

- [ ] **Step 6: 跑测试 + 全量（行为未变）**

Run: `go test ./internal/runtime/ -run TestRepeatGuard -v ; go build ./... ; go test ./...`
Expected: PASS + 构建成功 + 全绿（字段加了但还没接线，行为不变）

- [ ] **Step 7: Commit**

```bash
git add internal/runtime/messages.go internal/runtime/runtime.go internal/runtime/messages_test.go
git commit -m "feat(runtime): 加 per-tool-name 计数器与 loop-cap 常量（P2 前置）"
```

---

## Task 2: loop cap 接线（终止失控循环）

**Files:**
- Modify: `internal/runtime/runtime.go`（`runToolLoop`）
- Test: `internal/runtime/toolguard_test.go`（新建）

- [ ] **Step 1: 写失败测试——同工具换参数调用达 cap 后循环被切**

创建 `internal/runtime/toolguard_test.go`。测试用一个每轮都请求 `fetch_url`（每次参数不同，绕过签名熔断）的假模型，断言模型被调用次数被 loop cap 限制住（不会到 60 次）：

```go
package runtime

import (
	"context"
	"fmt"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

// alwaysFetchDifferentArgsMaas asks for fetch_url every round with a DIFFERENT
// url each time, so the callsKey(name+args) signature guard never trips — only
// the per-tool-name loop cap can stop it.
type alwaysFetchDifferentArgsMaas struct{ calls int }

func (m *alwaysFetchDifferentArgsMaas) Generate(_ context.Context, _ port.InferenceRequest) (port.InferenceResponse, error) {
	m.calls++
	return port.InferenceResponse{
		Text: "fetching",
		ToolCalls: []domain.ToolCall{{
			ID:        fmt.Sprintf("c%d", m.calls),
			Name:      "fetch_url",
			Arguments: map[string]string{"url": fmt.Sprintf("https://ex.example/%d", m.calls)},
		}},
	}, nil
}

func TestLoopCapStopsSameToolDifferentArgs(t *testing.T) {
	maas := &alwaysFetchDifferentArgsMaas{}
	// A registry whose fetch_url always "succeeds" with some output, so only the
	// loop cap (not failure) stops the runaway. Use an allow-all stub tool.
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow), nil, tool.NoopGuardrails{})
	reg.RegisterDescriptor(
		tool.Descriptor{Name: "fetch_url", Group: "web"},
		tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{CallID: call.ID, Success: true, Output: "ok"}, nil
		}),
	)
	// maxToolRounds high enough that the loop cap (30), not the round budget,
	// is what stops it.
	rt := NewRuntime(Config{Maas: maas, Tools: reg, MaxToolRounds: 1000})
	_, err := rt.RunTask(context.Background(), domain.Agent{ID: "a", Role: "developer"},
		domain.Task{ID: "t1", Status: domain.TaskRunning, Input: "go"})
	if err != nil {
		t.Fatalf("RunTask err = %v", err)
	}
	// The loop must be cut around toolLoopCap (30), NOT run to the 1000 round
	// budget. Allow a small margin for the final closing inference.
	if maas.calls > toolLoopCap+3 {
		t.Fatalf("model called %d times; loop cap (%d) should have cut it", maas.calls, toolLoopCap)
	}
	if maas.calls < toolLoopCap {
		t.Fatalf("model called only %d times; expected to reach loop cap (%d) first", maas.calls, toolLoopCap)
	}
}
```

> 先确认 `adapter`/`port.InferenceRequest`/`InferenceResponse` 与 `port.MaasInferenceClient` 接口的真实方法名与签名（grep `type MaasInferenceClient` in internal/port）。上面的 `Infer` 方法名/请求响应字段（`ToolCalls`/`Text`/`Arguments`）按现有 runtime 用法写——若接口方法名不同（如 `Generate`），改成实际的。`adapter` import 若未用则删。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run TestLoopCapStopsSameToolDifferentArgs -v`
Expected: FAIL（无 loop cap，模型被调用约 1000 次直到 round budget）

- [ ] **Step 3: 在 runToolLoop 接线 loop cap**

在 `internal/runtime/runtime.go` 的 `runToolLoop` 里，`repeatCount := st.repeatGuard.record(callsKey(calls))`（runtime.go:509）之后、`st.convo.appendAssistant(...)`（:510）之前，插入 per-tool-name 计数：

```go
		// P2: per-tool-name loop cap. repeatGuard/streak key on callsKey
		// (name+arguments) and so miss "same tool, different args" runaways;
		// this counts by tool NAME only. Recorded before executing this round so
		// the count reflects every call the model has made, including now.
		capHit := ""
		for _, c := range calls {
			if st.toolNameGuard.record(c.Name) >= toolLoopCap {
				capHit = c.Name
			}
		}
```

然后在现有 abort 分支（runtime.go:521 `if streak >= repeatAbortStreak || repeatCount >= repeatAbortCount {`）**之前**加一个并排的 loop-cap 终止分支（放在 `st.convo.syncLoaded(...)` @520 之后、:521 之前）：

```go
		if capHit != "" {
			if err := r.events.Publish(ctx, domain.RuntimeEvent{
				Type:      "tool_loop_broken",
				TaskID:    task.ID,
				Message:   fmt.Sprintf("工具 %s 调用次数达上限(%d)，已停止工具循环", capHit, toolLoopCap),
				CreatedAt: time.Now(),
			}); err != nil {
				return domain.TaskRun{}, fmt.Errorf("publish tool loop cap event: %w", err)
			}
			r.logger.Warn("tool loop broken: per-tool call cap reached",
				"task_id", task.ID, "tool", capHit, "cap", toolLoopCap)
			loopCut = true
			break
		}
```

> 放在 abort 分支前后均可（互斥语义无所谓），但必须在 `appendToolResults`（:516）之后——本轮结果已进 conversation，模型在 closing 时能用到它们。当前建议位置（:520 syncLoaded 之后）满足此约束。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ -run TestLoopCapStopsSameToolDifferentArgs -v`
Expected: PASS（模型调用次数被切在 ~30，不到 1000）

- [ ] **Step 5: 全量门槛**

Run: `go build ./... ; go vet ./... ; go test ./... ; gofmt -l .`
Expected: 构建成功、vet 无告警、全绿、gofmt 空

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/runtime.go internal/runtime/toolguard_test.go
git commit -m "feat(runtime): per-tool-name loop cap 熔断失控循环（P2）"
```

---

## Task 3: same-tool 失败 warn

**Files:**
- Modify: `internal/runtime/runtime.go`（`runToolLoop`）
- Test: `internal/runtime/toolguard_test.go`

- [ ] **Step 1: 写失败测试——同工具反复失败后模型收到警告**

在 `internal/runtime/toolguard_test.go` 追加。假模型每轮请求一个总是失败的 `fetch_url`（不同参数），断言几轮后 conversation 里出现"停止重试"的系统警告：

```go
// alwaysFailFetchMaas asks fetch_url every round (different args); the tool
// always fails. After toolSameFailWarn failures the model should be warned.
type alwaysFailFetchMaas struct {
	calls int
}

func (m *alwaysFailFetchMaas) Generate(_ context.Context, _ port.InferenceRequest) (port.InferenceResponse, error) {
	m.calls++
	// Stop asking after a handful of rounds so the task ends naturally once the
	// warning has been issued (avoid relying on loop cap here).
	if m.calls > toolSameFailWarn+2 {
		return port.InferenceResponse{Text: "giving up"}, nil
	}
	return port.InferenceResponse{
		Text: "retry",
		ToolCalls: []domain.ToolCall{{
			ID:        fmt.Sprintf("c%d", m.calls),
			Name:      "fetch_url",
			Arguments: map[string]string{"url": fmt.Sprintf("https://ex.example/%d", m.calls)},
		}},
	}, nil
}

func TestSameToolFailureWarnsModel(t *testing.T) {
	maas := &alwaysFailFetchMaas{}
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow), nil, tool.NoopGuardrails{})
	reg.RegisterDescriptor(
		tool.Descriptor{Name: "fetch_url", Group: "web"},
		tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{CallID: call.ID, Success: false, Error: "boom"}, nil
		}),
	)
	rt := NewRuntime(Config{Maas: maas, Tools: reg, MaxToolRounds: 1000})
	// Capture the conversation by inspecting the request the maas receives on the
	// LAST tool round: after toolSameFailWarn failures the prompt must carry the
	// warning. Simplest: assert via a recording maas that stores the last request.
	_, err := rt.RunTask(context.Background(), domain.Agent{ID: "a", Role: "developer"},
		domain.Task{ID: "t1", Status: domain.TaskRunning, Input: "go"})
	if err != nil {
		t.Fatalf("RunTask err = %v", err)
	}
	// The recording maas below captures prompts; assert the warning appeared.
}
```

> **实现者注意**：上面的断言不完整——`same-tool warn` 通过 `st.convo.appendUser(...)` 注入，模型下一轮的 `InferenceRequest` 里能看到。最干净的断言方式：用一个**记录每次 `Infer` 收到的 request** 的假 maas（参照 `internal/runtime` 既有测试里如何断言 prompt 内容——grep `multiturn_test.go` / `messages_test.go` 里对 `appendUser` 警告的断言，如现有"[系统] 你已多次"警告是怎么被测的），复用同样手法断言出现子串 `工具 fetch_url` + `失败`。请先读 `multiturn_test.go` 确认现有假 maas 与 prompt 断言写法，据此把本测试写成能真正断言警告文本出现的形式（可用记录型 maas 存最后一次 request，或复用现有 recording maas）。保留测试意图：**toolSameFailWarn(3) 次失败后，conversation 出现 same-tool 失败警告**。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/runtime/ -run TestSameToolFailureWarnsModel -v`
Expected: FAIL（无 same-tool 失败 warn）

- [ ] **Step 3: 在 runToolLoop 接线 same-tool 失败 warn**

在 `internal/runtime/runtime.go` 的 `runToolLoop` 里，`st.convo.appendToolResults(...)`（runtime.go:516）之后加：

```go
		// P2: same-tool failure warning. Count failures by tool NAME (not
		// callsKey) so "same tool, different args" failing repeatedly is caught.
		// Warn only — the loop cap (Task 2) is the hard stop.
		nameByID := make(map[string]string, len(calls))
		for _, c := range calls {
			nameByID[c.ID] = c.Name
		}
		for _, res := range results {
			if res.Success {
				continue
			}
			if st.toolFailGuard.record(nameByID[res.CallID]) == toolSameFailWarn {
				st.convo.appendUser(fmt.Sprintf(
					"[系统] 工具 %s 已累计失败 %d 次。不要再用不同参数反复重试它：检查最近的错误信息、验证假设，改用其他工具，或基于已有信息直接作答。",
					nameByID[res.CallID], toolSameFailWarn))
			}
		}
```

> 用 `== toolSameFailWarn`（恰好第 N 次）而非 `>=`，避免每轮重复追加同一警告。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/runtime/ -run TestSameToolFailureWarnsModel -v`
Expected: PASS

- [ ] **Step 5: 全量门槛**

Run: `go build ./... ; go vet ./... ; go test ./... ; gofmt -l .`
Expected: 构建成功、vet 无告警、全绿、gofmt 空

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/runtime.go internal/runtime/toolguard_test.go
git commit -m "feat(runtime): same-tool 反复失败警告模型停手（P2）"
```

---

## 自检结论（写计划者已核对）

- **Spec 覆盖（§7 P2）**：loop cap（per-tool-name 总调用 30 硬顶终止）= T2；same-tool 失败 warn（by name，≥3）= T3；per-tool-name 计数基建 = T1。偏离（plan header 已注明）：halt@8+hard_stop_enabled 与预算窗口缩放**首版不做**（YAGNI；loop cap 已是硬兜底，halt 默认关价值低，留后续）。
- **Placeholder 扫描**：无 TBD/TODO。T2 Step1 / T3 Step1 关于"确认 MaasInferenceClient 接口方法名"与"参照 multiturn_test.go 写 prompt 断言"是明确的 grep 指引（现有测试基础设施存在，只是我未逐字确认其假 maas 方法签名）——实现者按现有测试写法落地；非留白。
- **类型一致性**：`toolLoopCap`/`toolSameFailWarn` 常量 T1 定义、T2/T3 用；`loopState.toolNameGuard`/`toolFailGuard` T1 定义+初始化、T2/T3 用；复用现有 `repeatGuard.record(string) int`、`loopCut`、`appendUser`、`events.Publish` 模式，签名与现有一致。
- **实现锚点**：常量块 messages.go:146-167（repeatAbortCount@166）；loopState struct runtime.go:189（repeatGuard@200）；两处初始化 runtime.go:471(fresh)/:427(resume)；runToolLoop 接线区 :509(record)/:516(appendToolResults)/:520-521(abort 分支)/:567(closing)；`repeatGuard`/`newRepeatGuard`/`callsKey` @messages.go:173/178/214；`executeToolCalls` @runtime.go:826。
