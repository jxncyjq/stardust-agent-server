---
title: 中断正在运行的 task（需求2）设计
date: 2026-08-03
status: draft
tags: [runtime, coordinator, serve, gui, interrupt, cancel]
related:
  - "[[2026-08-03-gui-model-context-badge-design]]"
---

# 中断正在运行的 task（需求2）设计

## 1. 背景

GUI 发送消息后进入"发送中"等待，无法停止。用户需要在等待服务器回复时**真中断正在跑的 task**（停正在烧 token 的 tool-loop），不是只停前端等待。

**现状（已探证）**：
- **coordinator**（`internal/runtime/coordinator.go:178-188`）：task 跑在 goroutine，用**共享的 coordinator ctx**，无 per-task cancellable ctx、无 `task_id→cancel` 映射。
- **serve**（`internal/server/http.go`）：路由是 switch-case，有 POST /v1/tasks、GET result，**无中断 endpoint**。
- **runtime tool-loop**：`generate(ctx)`/`executeToolCalls(ctx)` 因 ctx cancel 返 error → 循环退出，**per-task ctx cancel 能中途停 tool-loop**（已支持）。
- **domain**：`TaskStatus` 有 pending/assigned/running/quality_review/done/failed/suspended（`types.go:17-23`），**无 cancelled**（config done_statuses 已含 "cancelled" 字符串，但缺 domain 常量）。
- **GUI**：`App` 经内嵌 serve 的 HTTP（`a.client.Post(a.BaseURL()+"/v1/tasks", ...)`，app.go:287）通信；`waitForTaskOutcome`（ChatPanel.tsx）只等 terminal event / 30min timeout，无 abort。

## 2. 目标与范围

- 真中断：停正在跑的单个 task 的 tool-loop（省 token/时间），task 转 **cancelled** 状态。
- GUI：发送按钮 sending 时变"停止"，点击中断；中断后**保留已生成的 streamed 部分文本**。
- 跨三层：coordinator（per-task ctx + Interrupt）→ serve（interrupt endpoint）→ GUI（停止按钮 + wait 响应）。

### 不做（YAGNI）
- 不做整 session / 批量中断（GUI 一次跑一个 task，中断当前 task）。
- 不做中断后自动重试 / 断点续跑。

## 3. 跨仓库分层

| 层 | 改动 | 仓库 |
|----|------|------|
| domain 状态 | 加 `TaskCancelled` | legionAgent |
| coordinator 中断 | per-task ctx + cancel map + Interrupt + Cancelled 分支 | legionAgent |
| serve endpoint | POST /v1/tasks/{id}/interrupt | legionAgent |
| GUI 触发 + UX | InterruptTask 绑定 + 停止按钮 + wait 响应 | legionAgentGUI |

## 4. 组件

### ① domain（`internal/domain/types.go`）
`types.go:23` 后加：
```go
	TaskCancelled TaskStatus = "cancelled"
```
scheduler 的状态流转需接受 running → cancelled（与 failed 平级的终态）。

### ② coordinator（`internal/runtime/coordinator.go`）— 核心
- Coordinator struct 加：
  ```go
  	mu      sync.Mutex
  	cancels map[string]context.CancelFunc // 运行中 task 的取消函数，按 task_id
  ```
  （构造处初始化 `cancels: make(map[string]context.CancelFunc)`。）
- dispatch goroutine（coordinator.go:178-188）改为派生 per-task ctx 并登记：
  ```go
  	taskCtx, cancel := context.WithCancel(ctx)
  	c.mu.Lock(); c.cancels[t.ID] = cancel; c.mu.Unlock()
  	go func(t domain.Task) {
  		defer c.wg.Done()
  		defer func() { <-c.sem }()
  		defer func() { c.mu.Lock(); delete(c.cancels, t.ID); c.mu.Unlock(); cancel() }()
  		if _, _, err := c.runAssigned(taskCtx, t); err != nil {
  			c.reportRunFailure(ctx, t.ID, err) // 用外层 ctx 记录，避免用已取消的 taskCtx
  		}
  	}(taskToRun)
  ```
- 新方法：
  ```go
  // Interrupt cancels the running task's context, stopping its tool-loop
  // mid-flight. Returns an error when the task is not currently running (already
  // finished / never started) — the caller (serve → GUI) surfaces it rather than
  // pretending an interrupt happened.
  func (c *Coordinator) Interrupt(taskID string) error {
  	c.mu.Lock()
  	cancel, ok := c.cancels[taskID]
  	c.mu.Unlock()
  	if !ok {
  		return fmt.Errorf("task %q is not running; cannot interrupt", taskID)
  	}
  	cancel()
  	return nil
  }
  ```
- `runAssigned` 的错误处理（当前 ctx.Canceled 会走 Failed 分支）加区分：`errors.Is(err, context.Canceled)` → transition `TaskCancelled` + emit `task_cancelled` 事件；否则保持 Failed。**位置**：runAssigned 里把 runner 错误转 Failed 的那处（+ reportRunFailure 路径）需分流——canceled 不是失败。

### ③ serve（`internal/server/http.go`）
- 路由（http.go:272-280 附近，与其它 /v1/tasks/ case 并列）加：
  ```go
  case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/tasks/") && strings.HasSuffix(r.URL.Path, "/interrupt"):
  	h.handleInterruptTask(w, r)
  ```
- `handleInterruptTask`：从 path 解析 taskID → `coordinator.Interrupt(taskID)` → 成功 200/204；task 不在跑（Interrupt 返 error）→ 404 + 错误体。serve 装配已持有 coordinator（BuildService）——handler 经该引用调用。

### ④ runtime
tool-loop ctx cancel 已支持中途停（`runToolLoop` 的 `generate`/`executeToolCalls` 因 ctx.Canceled 返 error → 退出）。**验证点**：确认该 error 是 `context.Canceled`（或 `errors.Is` 可识别），一路传到 runAssigned 供 ② 分流。若中途被 checkSuspend 等吞掉，需修正传播。

### ⑤ GUI（legionAgentGUI）
- `App.InterruptTask(taskID string) error`（app.go，照 SubmitTask 的 HTTP 模式）：
  ```go
  resp, err := a.client.Post(a.BaseURL()+"/v1/tasks/"+taskID+"/interrupt", "application/json", nil)
  ```
  非 2xx（如 404 task 已结束）→ 返 error（fail-loud）。加 Wails 绑定（wails generate）。
- `runStore`（`frontend/src/stores/runStore.ts`）：run 记录加 `taskID`；`startRun` / sendMessage 拿到 taskID 后存入当前 run（供停止按钮读取）。
- `ChatPanel.tsx`：发送按钮（ChatPanel.tsx:864-872）sending 时**变"停止"**（红色/方块 StopIcon，可点击、非禁用），`onClick` → `InterruptTask(currentRun.taskID)`；失败（任务已结束）→ addSystem 提示。
- `waitForTaskOutcome`（ChatPanel.tsx）：`TERMINAL_STATUSES` 加 `'cancelled'`；`TERMINAL_EVENT_TYPES` 加 `'task_cancelled'`；settle 时 `status==='cancelled'` → 回复内容显示"（已中断）"，streamed bubble 用 `finalizeMessage` 保留已生成文本（不清空）。

## 5. 数据流

```
停止按钮 onClick
 → App.InterruptTask(taskID)
   → POST /v1/tasks/{id}/interrupt
     → serve.handleInterruptTask → coordinator.Interrupt(taskID)
       → cancel per-task ctx
         → runToolLoop 下次 generate/executeToolCalls 返 context.Canceled → 退出
           → runAssigned: errors.Is(context.Canceled) → transition TaskCancelled + emit task_cancelled
 → GUI waitForTaskOutcome 收 task_cancelled 事件（或 poll 到 status=cancelled）
   → settle cancelled → 显示"已中断"，保留已 stream 的部分文本
```

## 6. 错误处理（fail-loud 铁律）

- `Interrupt(taskID)`：task 不在 `cancels` map（已结束/从未运行）→ 返 error；serve 转 404；GUI 提示"任务已结束，无法中断"。不静默假装成功。
- `context.Canceled` → `TaskCancelled`（**严格区别** Failed）；真正的运行错误仍走 Failed，不混。
- `reportRunFailure` 对 canceled 不应记为"失败"——中断是用户主动行为，记 info/audit（task_cancelled）而非 error 级失败日志。
- 中断保留已生成部分（streamed 文本 + cancelled 状态）；`GetTaskResult` 返 status=cancelled + 已有 result。
- coordinator `cancels` map 的并发访问全程 `mu` 保护（dispatch 登记、Interrupt 读、goroutine 清除）。

## 7. 测试

### coordinator（internal/runtime）
- Interrupt 运行中 task → 其 ctx 被 cancel → task 转 `TaskCancelled`（非 Failed）。
- Interrupt 不存在/已结束 task → 返 error。
- per-task 隔离：中断 task A 不取消 task B（各自 ctx）。
- `context.Canceled` 分流：runAssigned 遇 ctx.Canceled → Cancelled；遇其它 error → Failed。
- `cancels` map 并发安全（-race）。

### serve（internal/server）
- POST /v1/tasks/{id}/interrupt 路由命中 → 调 coordinator.Interrupt；运行中 → 2xx；不存在 → 404。

### runtime
- ctx cancel 中途停 tool-loop（generate/executeToolCalls 返 context.Canceled → runToolLoop 退出）回归。

### GUI（legionAgentGUI）
- `InterruptTask`：2xx → nil；404 → error。
- 停止按钮：sending 时渲染"停止"可点击；onClick 调 InterruptTask(taskID)。
- `waitForTaskOutcome`：收 task_cancelled / status=cancelled → settle；streamed 部分保留。

### 门槛
- legionAgent：`go build/vet/test`（含 -race for coordinator）全绿、`gofmt -l .` 空。
- legionAgentGUI：`go build/test` + 前端 `tsc --noEmit` + vitest。

## 8. 实现锚点

- `internal/domain/types.go:17-23`（TaskStatus，加 TaskCancelled）。
- `internal/runtime/coordinator.go:178-188`（dispatch goroutine，加 per-task ctx + cancel 登记）、`coordinator.go:203 runAssigned`（错误分流 Cancelled/Failed）、`reportRunFailure`。
- `internal/server/http.go:272-280`（路由 switch，加 interrupt case）+ serve BuildService（coordinator 引用、handler 落点）。
- `internal/runtime/runtime.go runToolLoop`（ctx.Canceled 传播验证）。
- legionAgentGUI：`app.go:287 SubmitTask`（HTTP 模式参照）、新 `InterruptTask` + wails 绑定；`frontend/src/stores/runStore.ts`（run 加 taskID）；`frontend/src/components/ChatPanel.tsx:864-872`（发送按钮→停止）、`waitForTaskOutcome`（TERMINAL_STATUSES/EVENT_TYPES + cancelled 处理）；`icons.tsx`（StopIcon，若无则加）。

## 9. 开放问题

无（中断范围=真中断、状态=TaskCancelled、按钮=发送切停止、保留部分文本 均已确认）。
