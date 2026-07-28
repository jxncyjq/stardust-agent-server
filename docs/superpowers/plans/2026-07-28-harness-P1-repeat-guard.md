# Harness 加固 P1：非连续重复守卫 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Steps use checkbox (`- [ ]`).

**Goal:** 给 tool-loop 加任务级「相同调用非连续重复」守卫（ToolCallSignature 计数），补现有 `repeatedCallStreak` 只拦连续重复的盲区，达阈值先警告后硬停。

**Architecture:** 复用现有 `callsKey`(name+args) 作轮签名，加任务级 `repeatGuard`(map[签名]次数) 挂在 `loopState`；`runToolLoop` 每轮 `record` 一次，与现有连续 streak 并列判定 warn/abort，复用现有 `appendUser` 警告轮 + `tool_loop_broken` 断循环路径。

**Tech Stack:** Go 标准库。

## Global Constraints

- Fail-loud：纯计数无 error 路径；硬停必 publish `tool_loop_broken` 事件（复用现有 fail-loud 路径），不静默停。
- 守 152-incident：只增强重复可见性，不折叠对话、不破坏 append-only。
- 与现有连续 `repeatedCallStreak`/`repeatWarnStreak(3)`/`repeatAbortStreak(8)` 并存不冲突（两条独立判定）。
- `go build/vet/test ./...` 全绿、`gofmt -l .` 空。非导出符号 Go doc；错误/边界路径有测试断言。

## 现状 seam（实测）

- `internal/runtime/messages.go`：`repeatWarnStreak=3`/`repeatAbortStreak=8`；`repeatedCallStreak(msgs,calls)` 只数**连续**；`callsKey(calls)` = 排序后 join 的轮签名（name+args）。
- `internal/runtime/runtime.go`：`loopState` 结构（无 guard 字段）；`runToolLoop` 循环内 `streak := repeatedCallStreak(...)` 后有 `streak>=repeatAbortStreak→loopCut=true;break` 与 `streak>=repeatWarnStreak→appendUser(...)` 两分支；`loopState` 在 RunTask 里两处创建（resume 约 :367、normal 约 :409）。

---

### Task 1: repeatGuard 类型（纯逻辑）

**Files:**
- Modify: `internal/runtime/messages.go`
- Test: `internal/runtime/messages_test.go`（无则新建）

**Interfaces:**
- Produces:
  - 常量 `repeatWarnCount = 4`、`repeatAbortCount = 6`（任务内**累计**出现次数阈值，区别于连续 streak）。
  - `type repeatGuard struct { seen map[string]int }`
  - `func newRepeatGuard() *repeatGuard`
  - `func (g *repeatGuard) record(signature string) int` — 累加该签名出现次数并返回新值（首次返回 1）。

- [ ] **Step 1: Write the failing test**

```go
// messages_test.go
func TestRepeatGuardCountsNonConsecutive(t *testing.T) {
	g := newRepeatGuard()
	// A, B, A, B alternating: A 的累计次数应递增，不受 B 打断
	if n := g.record("A"); n != 1 { t.Fatalf("A#1=%d want 1", n) }
	if n := g.record("B"); n != 1 { t.Fatalf("B#1=%d want 1", n) }
	if n := g.record("A"); n != 2 { t.Fatalf("A#2=%d want 2", n) }
	if n := g.record("B"); n != 2 { t.Fatalf("B#2=%d want 2", n) }
	if n := g.record("A"); n != 3 { t.Fatalf("A#3=%d want 3", n) }
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/runtime/ -run TestRepeatGuardCountsNonConsecutive -v`
Expected: FAIL — `undefined: newRepeatGuard`。

- [ ] **Step 3: Implement**

在 `messages.go` 的 const 块（`repeatWarnStreak`/`repeatAbortStreak` 附近）加：
```go
	// repeatWarnCount and repeatAbortCount bound how many times ONE tool-call
	// signature (callsKey: names+arguments) may recur across a whole task,
	// regardless of whether the recurrences are consecutive. They complement
	// repeatWarnStreak/repeatAbortStreak (which only see consecutive repeats):
	// an A→B→A→B loop never trips the streak guard but does accumulate here.
	// At the warn count the loop injects a steering turn; at the abort count it
	// stops the tool loop (the non-consecutive analogue of the 152-round abort).
	repeatWarnCount  = 4
	repeatAbortCount = 6
```
并新增：
```go
// repeatGuard counts, per task, how many times each tool-call signature
// (callsKey) has been requested — including non-consecutive recurrences. One
// guard lives for the whole tool loop (loopState.repeatGuard); it is the
// non-consecutive counterpart to repeatedCallStreak.
type repeatGuard struct {
	seen map[string]int
}

// newRepeatGuard returns an empty guard.
func newRepeatGuard() *repeatGuard {
	return &repeatGuard{seen: make(map[string]int)}
}

// record increments the running count for signature and returns the new count
// (1 on first sighting).
func (g *repeatGuard) record(signature string) int {
	g.seen[signature]++
	return g.seen[signature]
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/runtime/ -run TestRepeatGuardCountsNonConsecutive -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/messages.go internal/runtime/messages_test.go
git commit -m "feat(runtime): repeatGuard 任务级非连续重复调用计数"
```

---

### Task 2: 接入 loopState + runToolLoop

**Files:**
- Modify: `internal/runtime/runtime.go`（`loopState` 结构、两处创建、`runToolLoop` 循环体）
- Test: `internal/runtime/runtime_test.go`（复用既有 harness；无则按既有测试文件模式加）

**Interfaces:**
- Consumes: `newRepeatGuard`/`record`/`callsKey`/`repeatWarnCount`/`repeatAbortCount`（Task 1 + 现有）。
- Produces: `loopState` 加字段 `repeatGuard *repeatGuard`；`runToolLoop` 每轮记录轮签名并在累计阈值触发 warn/abort。

- [ ] **Step 1: Write the failing test**

```go
// runtime_test.go — 用既有构造 Runtime 的 harness（RecordingMaas/MemoryEventBus 等）。
// 造一个 mock maas，让它交替请求两组不同 tool_calls（A,B,A,B,...），断言：
// 累计到 repeatAbortCount 时循环停止（publish 了 tool_loop_broken 事件），
// 而连续 streak 从未达到 repeatAbortStreak(8)（证明是非连续守卫起的作用）。
func TestRunToolLoopAbortsOnNonConsecutiveRepeat(t *testing.T) {
	// 安排：mock maas.Generate 依次返回 tool_calls: A,B,A,B,A,B,...（各不连续重复）
	// 每次 A/B 是固定 name+args，使 callsKey(A) 稳定。
	// 期望：events 里出现 type=="tool_loop_broken"；且循环在 A 累计达 repeatAbortCount 后结束。
	// 断言 emitted events 含 tool_loop_broken，且最终返回的 TaskRun 有 finalize（走 generateNoTools）。
	// 具体 mock 按 internal/runtime 既有测试的 fake maas 模式编写。
}
```
（按 `internal/runtime` 既有单测的 fake `port.MaasInferenceClient` 模式实现 mock；核心断言 = `tool_loop_broken` 事件出现 + 非连续路径触发。）

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/runtime/ -run TestRunToolLoopAbortsOnNonConsecutiveRepeat -v`
Expected: FAIL（当前无非连续守卫，A→B→A 不触发 abort；`loopState` 无 repeatGuard 字段）。

- [ ] **Step 3: Implement**

1. `loopState` 结构加字段（放在 `convo` 附近）：
```go
	// repeatGuard counts non-consecutive repeats of each tool-call signature
	// across the whole task (see messages.go). One per RunTask.
	repeatGuard *repeatGuard
```
2. `RunTask` 两处 `st := loopState{...}` 都加 `repeatGuard: newRepeatGuard(),`（resume 路径 :367 与 normal 路径 :409 均加——resume 重建的 guard 从空开始，历史重复不追溯，可接受）。
3. `runToolLoop` 循环体内，在现有 `streak := repeatedCallStreak(st.convo.messages, calls)` 之后加：
```go
		repeatCount := st.repeatGuard.record(callsKey(calls))
```
4. 把现有 abort 判定 `if streak >= repeatAbortStreak {` 改为并含非连续：
```go
		if streak >= repeatAbortStreak || repeatCount >= repeatAbortCount {
```
（分支体不变——仍 publish `tool_loop_broken` + Warn + `loopCut = true; break`。消息文案保持通用「工具调用重复，已停止工具循环」即可，无需区分连续/非连续。）
5. 把现有 warn 判定 `if streak >= repeatWarnStreak {` 改为：
```go
		if streak >= repeatWarnStreak || repeatCount >= repeatWarnCount {
```
（警告文案沿用现有——「你已连续/多次以相同参数调用同一工具，结果没有变化，改用其他工具或直接作答」。可在文案里把「连续」改为「多次」以同时覆盖两种情形。）

- [ ] **Step 4: Run to verify pass + 全量**

Run: `go test ./internal/runtime/ -run TestRunToolLoopAbortsOnNonConsecutiveRepeat -count=1 -v && go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l .`
Expected: 目标测试 PASS；全包全绿；gofmt 空。（注意：既有连续 streak 测试须仍绿——两条判定 OR 并列不应破坏原行为。）

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/runtime.go internal/runtime/runtime_test.go
git commit -m "feat(runtime): tool-loop 接入非连续重复守卫(警告→硬停)"
```

---

## Self-Review

- **Spec 覆盖**（组件1）：任务级签名计数=Task1 repeatGuard；warn 阈值注入=Task2 Step3.5；abort 硬停+事件=Task2 Step3.4；复用 callsKey 签名=Task2；与连续 streak 并存=OR 判定。均覆盖。
- **占位**：Task2 测试体给了断言目标与 mock 方式说明（「按既有 fake maas 模式」），因 runtime 测试 harness 因文件而异，执行时对齐——非逻辑占位；核心断言明确（tool_loop_broken 事件 + 非连续触发）。
- **类型一致**：`repeatGuard`/`newRepeatGuard`/`record`/`repeatWarnCount`/`repeatAbortCount`/`loopState.repeatGuard`/`callsKey` 跨任务一致。

## 后续（本 spec 其余组件，各自 plan）
- P2 对话 LLM compaction、P3 迭代上限有限化、P4 只读工具并发 —— 见 `2026-07-28-harness-hardening-design.md`，各自 plan+PR。
