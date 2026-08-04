# 中断正在运行的 task（需求2）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** GUI 等待回复时可停止——真中断正在跑的 task 的 tool-loop（省 token），task 转 cancelled，保留已生成部分。

**Architecture:** 跨三层两仓库。legionAgent：domain 加 `TaskCancelled`；coordinator 给每 task 派生 cancellable ctx + `task_id→cancel` map + `Interrupt` 方法，`runAssigned` 把 `context.Canceled` 分流为 Cancelled（非 Failed）；serve 加 `POST /v1/tasks/{id}/interrupt`。legionAgentGUI：`App.InterruptTask`（HTTP POST）+ 发送按钮 sending 时变"停止" + `waitForTaskOutcome` 响应 cancelled。runtime tool-loop 的 ctx cancel 中途停已支持。

**Tech Stack:** Go（domain/coordinator/serve + Wails App）；React/TS。

**参考 spec:** `legionAgent/docs/superpowers/specs/2026-08-03-task-interrupt-design.md`

**门槛:**
- legionAgent：`go build/vet/test`（coordinator 加 `-race`）全绿、`gofmt -l .` 空。
- legionAgentGUI：`go build/test` + 前端 `tsc --noEmit` + vitest。

---

## 文件结构

| 文件 | 仓库 | 动作 |
|------|------|------|
| `internal/domain/types.go` | legionAgent | 加 `TaskCancelled` |
| `internal/runtime/coordinator.go` | legionAgent | per-task ctx + cancels map + Interrupt + Cancelled 分流 |
| `internal/server/http.go` (+ handler 文件) | legionAgent | interrupt 路由 + handler |
| `app.go` | legionAgentGUI | `InterruptTask` + wails 绑定 |
| `frontend/src/stores/runStore.ts` | legionAgentGUI | run 加 taskID |
| `frontend/src/components/ChatPanel.tsx` | legionAgentGUI | 停止按钮 + wait 响应 cancelled |
| `frontend/src/components/icons.tsx` | legionAgentGUI | StopIcon（若无） |

---

## Task 1（legionAgent）: domain 加 TaskCancelled

**Files:** `internal/domain/types.go`, `internal/domain/types_test.go`（或现有 test）

- [ ] **Step 1: 写测试**

在 domain 测试追加（若无 types_test.go 则新建，package domain）：
```go
func TestTaskCancelledStatus(t *testing.T) {
	if TaskCancelled != "cancelled" {
		t.Fatalf("TaskCancelled = %q, want cancelled", TaskCancelled)
	}
}
```

- [ ] **Step 2: 跑失败** — `go test ./internal/domain/ -run TestTaskCancelledStatus -v`（编译失败）

- [ ] **Step 3: 加常量**

`internal/domain/types.go:23`（`TaskSuspended` 后）加：
```go
	TaskCancelled TaskStatus = "cancelled"
```

- [ ] **Step 4: 跑通过 + 全量** — `go test ./internal/domain/ -run TestTaskCancelledStatus -v ; go build ./... ; go test ./... ; gofmt -l .`

> 若 scheduler 有状态白名单/流转校验拒绝 running→cancelled，同步放行（grep `TaskSuspended` 在 scheduler/task 包看流转是否枚举了合法目标状态；cancelled 与 suspended 同为 running 的合法终态）。

- [ ] **Step 5: Commit**
```bash
git add internal/domain/types.go internal/domain/types_test.go
git commit -m "feat(domain): 加 TaskCancelled 状态（任务中断）"
```

---

## Task 2（legionAgent）: coordinator per-task ctx + Interrupt + Cancelled 分流

**Files:** `internal/runtime/coordinator.go`, `internal/runtime/coordinator_interrupt_test.go`（新建）

- [ ] **Step 1: 写失败测试**

新建 `internal/runtime/coordinator_interrupt_test.go`（参照现有 coordinator 测试如何构造 Coordinator + fake runner/scheduler；下面是意图，按现有测试基础设施落地）：
```go
// 意图：一个 fake runner 的 RunTask 阻塞直到 ctx 被取消，返回 ctx.Err()。
// 启动 task → Interrupt(taskID) → runner 因 ctx cancel 返回 → task 转 Cancelled。
func TestCoordinatorInterruptCancelsRunningTask(t *testing.T) {
	// ... 构造 Coordinator with fake runner that: <-ctx.Done(); return ctx.Err()
	// dispatch a task; wait until it's registered running; Interrupt(taskID);
	// assert scheduler transitioned it to domain.TaskCancelled (not Failed).
}

func TestCoordinatorInterruptUnknownTaskErrors(t *testing.T) {
	// Interrupt("nonexistent") → error (not in cancels map).
}
```
> 先读现有 `coordinator_*_test.go`（如 coordinator_test.go / coordinator_concurrency_test.go）看它们怎么建 Coordinator（NewCoordinator + CoordinatorConfig 的 fake scheduler/runner/locks）、怎么触发 dispatch（Heartbeat）、怎么断言 scheduler 状态。用同样手法写这两个测试到能编译运行的程度。fake runner 的 RunTask 需 `select { case <-ctx.Done(): return domain.TaskRun{}, ctx.Err() }`。

- [ ] **Step 2: 跑失败** — `go test ./internal/runtime/ -run TestCoordinatorInterrupt -v`

- [ ] **Step 3: Coordinator struct 加 cancels map**

`coordinator.go:71 type Coordinator struct` 里（`resuming map[string]bool` @100 附近）加：
```go
	// cancels holds the cancel func of every currently-running task, keyed by
	// task ID, so Interrupt can stop one task's tool-loop mid-flight. Guarded by
	// mu (shared with any other coordinator mutable maps' own locks — use a
	// dedicated mutex here).
	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc
```
`NewCoordinator`（coordinator.go:103）里初始化：`cancels: make(map[string]context.CancelFunc)`（在返回的 `&Coordinator{...}` 字面量中加）。确认 `sync`/`context` 已 import。

- [ ] **Step 4: dispatch goroutine 派生 per-task ctx + 登记**

`coordinator.go:178-188` 的 dispatch goroutine 改为：
```go
		c.wg.Add(1)
		taskCtx, cancel := context.WithCancel(ctx)
		c.cancelMu.Lock()
		c.cancels[taskToRun.ID] = cancel
		c.cancelMu.Unlock()
		go func(t domain.Task, taskCtx context.Context, cancel context.CancelFunc) {
			defer c.wg.Done()
			defer func() { <-c.sem }()
			defer func() {
				c.cancelMu.Lock()
				delete(c.cancels, t.ID)
				c.cancelMu.Unlock()
				cancel()
			}()
			if _, _, err := c.runAssigned(taskCtx, t); err != nil {
				// 用外层 ctx 记录（taskCtx 可能已被中断取消）。
				c.reportRunFailure(ctx, t.ID, err)
			}
		}(taskToRun, taskCtx, cancel)
		dispatched = true
```

- [ ] **Step 5: 新 Interrupt 方法**

`coordinator.go` 加（`reportRunFailure` 附近）：
```go
// Interrupt cancels the running task's context, stopping its tool-loop
// mid-flight. Returns an error when the task is not currently running (already
// finished / never started) — the caller surfaces it rather than pretending an
// interrupt happened.
func (c *Coordinator) Interrupt(taskID string) error {
	c.cancelMu.Lock()
	cancel, ok := c.cancels[taskID]
	c.cancelMu.Unlock()
	if !ok {
		return fmt.Errorf("task %q is not running; cannot interrupt", taskID)
	}
	cancel()
	return nil
}
```

- [ ] **Step 6: runAssigned 分流 context.Canceled → Cancelled**

`coordinator.go:262-282` 的 `run, err := runner.RunTask(...)` 错误处理，在 `errors.Is(err, ErrSuspended)` 分支之后、`c.failTask` 之前，加：
```go
		if errors.Is(err, context.Canceled) {
			// 用户中断：转 Cancelled 而非 Failed。用 context.Background() 派生的
			// 短 ctx 做收尾（taskCtx 已取消，用它做 scheduler/audit 会立刻失败）。
			finishCtx := context.WithoutCancel(ctx) // Go 1.21+；或 context.Background()
			if txErr := c.scheduler.Transition(finishCtx, taskToRun.ID, domain.TaskCancelled); txErr != nil {
				return domain.Task{}, false, fmt.Errorf("cancel task %s: %w", taskToRun.ID, txErr)
			}
			if auErr := c.appendAudit(finishCtx, taskToRun.ID, "task_cancelled"); auErr != nil {
				return domain.Task{}, false, auErr
			}
			if _, unlockErr := c.locks.Unlock(finishCtx, taskToRun.ID, c.agent.ID); unlockErr != nil {
				return domain.Task{}, false, fmt.Errorf("release lock on cancel for task %s: %w", taskToRun.ID, unlockErr)
			}
			return c.currentTask(finishCtx, taskToRun.ID)
		}
		c.failTask(ctx, taskToRun.ID)
		return domain.Task{}, false, fmt.Errorf("run task: %w", err)
```
> 关键：中断后的收尾（transition/audit/unlock）不能用已取消的 taskCtx，否则每步立刻 ctx.Canceled 失败。用 `context.WithoutCancel(ctx)`（Go 1.21+，剥离取消但保留值）或 `context.Background()`。确认 go.mod 的 Go 版本支持 WithoutCancel，否则用 Background。同时确认 `RunTask` 在 ctx 取消时返回的 error 满足 `errors.Is(err, context.Canceled)`（若被 `%w` 包了 context.Canceled 仍 OK；若返回别的 sentinel 需在 runtime 侧确保传播 —— 见 Task 2b 验证）。

- [ ] **Step 6b: 验证 runtime tool-loop 传播 context.Canceled**

Run: `grep -n "ctx.Err()\|context.Canceled\|ErrInterrupted" internal/runtime/runtime.go` —— 确认 runToolLoop 里 `generate`/`executeToolCalls` 的 ctx 错误一路 `%w` 包装或原样返回（`errors.Is(err, context.Canceled)` 在 coordinator 能命中）。若 runtime 把 ctx.Canceled 转成了自有 sentinel 而丢了链，在返回处用 `fmt.Errorf("...: %w", ctx.Err())` 保链。加一个 runtime 测试断言 ctx 取消时 RunTask 返回的 err `errors.Is(context.Canceled)`（若现有测试已覆盖则复用）。

- [ ] **Step 7: 跑测试 + 全量（-race）**

Run: `go test ./internal/runtime/ -run TestCoordinatorInterrupt -race -v ; go build ./... ; go vet ./... ; go test ./... ; gofmt -l .`
Expected: PASS + build/vet 净 + 全绿 + gofmt 空

- [ ] **Step 8: Commit**
```bash
git add internal/runtime/coordinator.go internal/runtime/coordinator_interrupt_test.go
git commit -m "feat(runtime): coordinator per-task 取消 + Interrupt（中断转 Cancelled）"
```

---

## Task 3（legionAgent）: serve interrupt endpoint

**Files:** `internal/server/http.go`（+ handler；grep 现有 handleXXX 落点/handler 结构怎么持 coordinator）, `internal/server/*_test.go`

- [ ] **Step 1: 写失败测试**

参照现有 server handler 测试（如 tasks 相关 handler test）写：POST /v1/tasks/{id}/interrupt → 命中路由 → 调 coordinator.Interrupt。运行中 task → 2xx；不存在 → 404。
> 先 grep `handleSubmitTask`/`/v1/tasks` handler in internal/server 看 handler 结构体（`h`）持有什么、coordinator（或其 Interrupt 能力）如何注入。若 handler 结构未持 coordinator，需在 BuildService 装配时把 coordinator（或一个 `Interrupter interface { Interrupt(string) error }`）传入 handler。用 interface 更可测。

- [ ] **Step 2: 跑失败**

- [ ] **Step 3: 加路由 + handler**

`internal/server/http.go`（:272-280 的 /v1/tasks/ case 群里）加：
```go
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/tasks/") && strings.HasSuffix(r.URL.Path, "/interrupt"):
		h.handleInterruptTask(w, r)
```
handler（新，在合适的 handler 文件）：
```go
// handleInterruptTask cancels a running task's tool-loop. 2xx on interrupt, 404
// when the task is not running (already finished / unknown).
func (h *Handler) handleInterruptTask(w http.ResponseWriter, r *http.Request) {
	// path: /v1/tasks/{id}/interrupt
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "/interrupt")
	id = strings.Trim(id, "/")
	if id == "" {
		http.Error(w, "task id required", http.StatusBadRequest)
		return
	}
	if err := h.interrupter.Interrupt(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```
（`h.interrupter` = 注入的 coordinator/Interrupter interface。BuildService 装配时传入。handler 结构体名/字段按现有代码对齐。）

- [ ] **Step 4: BuildService 接线 coordinator → handler**

grep `BuildService` 装配 handler 处，把 coordinator（已存在的实例）作为 `interrupter` 传给 handler。若 handler 用一个 deps 结构，加字段。

- [ ] **Step 5: 跑测试 + 全量** — `go test ./internal/server/ -v ; go build ./... ; go test ./... ; gofmt -l .`

- [ ] **Step 6: Commit**
```bash
git add internal/server/ 
git commit -m "feat(serve): POST /v1/tasks/{id}/interrupt 中断端点"
```

---

## Task 4（legionAgentGUI）: App.InterruptTask + wails 绑定

**Files:** `app.go`, `app_test.go`, `frontend/wailsjs/go/main/App.{d.ts,js}`

- [ ] **Step 1: 写失败测试**

参照现有 App HTTP 绑定测试（如 SubmitTask 的测试用 httptest server 冒充 serve）：InterruptTask 对运行中 → nil；对 404 → error。
> grep App 测试怎么用 httptest.Server 冒充内嵌 serve（设 a.serve.port / BaseURL）。照写。

- [ ] **Step 2: 跑失败**

- [ ] **Step 3: 实现 InterruptTask（app.go）**

照 `SubmitTask`（app.go:287）的 HTTP 模式：
```go
// InterruptTask cancels a running task on the embedded serve. A non-2xx status
// (e.g. 404 when the task already finished) is returned as an error rather than
// silently ignored. Called by React via the Wails bindings.
func (a *App) InterruptTask(taskID string) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return fmt.Errorf("task id is required")
	}
	resp, err := a.client.Post(a.BaseURL()+"/v1/tasks/"+taskID+"/interrupt", "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("interrupt task %q failed: status %d: %s", taskID, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
```

- [ ] **Step 4: 跑测试 + build** — `go test . -run TestInterruptTask -v ; go build ./... ; gofmt -l .`

- [ ] **Step 5: 生成 wails 绑定**

`wails generate module`（若 CLI 可用）；否则手加到 `frontend/wailsjs/go/main/App.d.ts`：
```ts
export function InterruptTask(arg1:string):Promise<void>;
```
和 `App.js`：
```js
export function InterruptTask(arg1) {
  return window['go']['main']['App']['InterruptTask'](arg1);
}
```

- [ ] **Step 6: Commit**
```bash
git add app.go app_test.go frontend/wailsjs/go/main/App.d.ts frontend/wailsjs/go/main/App.js
git commit -m "feat(gui): InterruptTask 绑定——中断运行中的 task"
```

---

## Task 5（legionAgentGUI frontend）: 停止按钮 + wait 响应 cancelled

**Files:** `frontend/src/stores/runStore.ts`, `frontend/src/components/ChatPanel.tsx`, `frontend/src/components/icons.tsx`, `frontend/src/components/ChatPanel.test.tsx`

- [ ] **Step 1: runStore 记 taskID**

`frontend/src/stores/runStore.ts`：run 记录结构加 `taskID?: string`；提供设置它的方式（`startRun` 或新 `setRunTask(sessionID, taskID)`）。grep runStore 现有 run 结构 + startRun/updateRun 签名，按现有模式加。

- [ ] **Step 2: sendMessage 存 taskID**

`ChatPanel.tsx` `sendMessage`：`const taskID = await SubmitTask(...)`（:675）之后，把 taskID 存进当前 run（`setRunTask(sessionID, taskID)` 或 updateRun）。

- [ ] **Step 3: 发送按钮 sending 时变"停止"**

`ChatPanel.tsx` 发送按钮（:864-872）改为：sending 时渲染"停止"（红色 + StopIcon，**不禁用**），onClick 调中断：
```tsx
          <button
            className={sending
              ? 'interactive flex items-center gap-1.5 px-4 py-2 bg-destructive text-destructive-foreground rounded-md text-sm hover:opacity-90'
              : 'interactive flex items-center gap-1.5 px-4 py-2 bg-primary text-primary-foreground rounded-md text-sm hover:opacity-90 disabled:opacity-50'}
            onClick={sending ? onStop : sendMessage}
            disabled={false}
            aria-label={sending ? '停止' : '发送消息'}
          >
            {sending ? <StopIcon /> : <SendIcon />}
            <span>{sending ? '停止' : '发送'}</span>
          </button>
```
`onStop`：
```tsx
  async function onStop() {
    const taskID = currentRun?.taskID
    if (!taskID) return
    try {
      await InterruptTask(taskID)
    } catch (err) {
      addSystem(`中断失败: ${errText(err)}`)
    }
  }
```
（import `InterruptTask` from wailsjs、`StopIcon` from icons。textarea 的 `disabled={sending}` 保持——发送中不让输入，但停止按钮可点。）

- [ ] **Step 4: StopIcon（若无）**

grep `StopIcon` in `icons.tsx`；无则加一个方块 stop 图标（照现有 icon 组件写法，如一个 `<rect>` 的 svg）。

- [ ] **Step 5: waitForTaskOutcome 响应 cancelled**

`ChatPanel.tsx`：
- `TERMINAL_STATUSES`（:82）加 `'cancelled'`。
- `TERMINAL_EVENT_TYPES`（:85）加 `'task_cancelled'`。
- settle 后 `sendMessage` 里组装内容处（:683-688）：`status==='cancelled'` → content 用已 stream 文本 + "（已中断）"标注；streamed bubble 走 `finalizeMessage`/`updateMessage(streaming:false)` 保留部分文本，不清空。

- [ ] **Step 6: tsc + 前端测试**

Run（frontend）: `npx tsc --noEmit ; npx vitest run`
Expected: 无类型错、全绿。（若 ChatPanel.test.tsx 需 mock InterruptTask，加 mock。）

- [ ] **Step 7: Commit**
```bash
git add frontend/src/stores/runStore.ts frontend/src/components/ChatPanel.tsx frontend/src/components/icons.tsx frontend/src/components/ChatPanel.test.tsx
git commit -m "feat(gui): 发送按钮 sending 时变停止，中断并保留已生成部分"
```

---

## 自检结论（写计划者已核对）

- **Spec 覆盖**：TaskCancelled（T1）、coordinator per-task ctx+cancel map+Interrupt+Cancelled 分流（T2）、runtime ctx 传播验证（T2b）、serve endpoint（T3）、App.InterruptTask+wails（T4）、runStore taskID + 停止按钮 + wait cancelled + 保留部分（T5）。均覆盖。
- **Placeholder 扫描**：无 TBD/TODO。多处"grep 现有测试基础设施/handler 结构/runStore 模式"是明确的对齐现有代码指引（coordinator/server/App/前端测试的既有构造手法我未逐字确认，指向现有文件让实现者对齐），非留白。
- **类型/签名一致**：`Coordinator.Interrupt(taskID string) error`（T2 定义）↔ serve `interrupter.Interrupt`（T3）↔ App.InterruptTask HTTP（T4）↔ 前端 InterruptTask（T5）链路一致；`TaskCancelled`（T1）在 coordinator 分流（T2）、GUI TERMINAL_STATUSES（T5）一致（"cancelled"）；`task_cancelled` 事件（T2 emit）↔ TERMINAL_EVENT_TYPES（T5）一致。
- **跨仓库 commit**：T1-T3 legionAgent；T4-T5 legionAgentGUI。
- **关键风险点（实现者重点验证）**：(a) 中断收尾用非取消 ctx（`context.WithoutCancel`/Background），否则 transition/audit/unlock 立刻 ctx.Canceled 失败——T2 Step6 已强调；(b) runtime tool-loop 的 ctx.Canceled 真传播到 coordinator（`errors.Is` 能命中）——T2b 验证；(c) serve handler 拿到 coordinator 引用（BuildService 接线）——T3 Step4。
