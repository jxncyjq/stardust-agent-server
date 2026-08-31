package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// eventRecorder 把一次任务执行写成会话事件日志（spec §5）。
//
// 它是**每次 RunTask 一个**：seq 游标、缓冲区都属于这一次执行，不跨任务共享。
// 真正的串行化与 seq 连续性由 P1 的 store 保证（它在事务内查 next-seq 并持 per-session
// 写锁），这里只负责「发什么、什么时候必须落盘」。
type eventRecorder struct {
	store   port.SessionEventStore
	session string

	mu      sync.Mutex
	pending []domain.SessionEvent
	nextSeq int64
	// seqKnown 表示 nextSeq 已经与库对齐过。第一次 flush 之前不知道库里走到哪，
	// 所以 seq 在 flush 时才最终确定（见 flush）。
	seqKnown bool
	turn     int
	step     int
}

// newEventRecorder 建一次任务执行的记录器。
//
// 会话号取 task.SessionID，为空时退到 task.ID（决定 D-A：单次任务与委派子任务没有
// 会话号，让它们各自成为一条短日志，比加特例分支更简单，轨迹也一样看得到）。
// 两者都空说明这条任务没有任何身份——写出来的事件谁也认不回去，直接 panic：
// 这是编程错误，不是运行期状况。
func newEventRecorder(store port.SessionEventStore, task domain.Task) *eventRecorder {
	session := task.SessionID
	if session == "" {
		session = task.ID
	}
	if session == "" {
		panic("runtime: event recorder needs a session id or a task id; a task with neither cannot own a log")
	}
	return &eventRecorder{store: store, session: session}
}

// sessionID 是这次执行写入的会话日志。
func (e *eventRecorder) sessionID() string { return e.session }

// enabled 说明这个部署是否记录会话事件。
//
// 没有配 store 是**契约允许的可选**（见 Config.SessionEvents），不是错误：内存后端与
// 大量测试构造都不配。它与「配了但写不进去」是两回事——后者由 flush 硬失败。
func (e *eventRecorder) enabled() bool { return e != nil && e.store != nil }

// eventUsage 是一次模型响应的 token 用量。
//
// 本地定义而不是复用某个领域结构：那些结构（TaskRun/RuntimeEvent/ConversationTurn）
// 各自还带着一堆与事件无关的字段，让事件记录去依赖它们会把两件事绑死。
type eventUsage struct {
	Prompt     int
	Completion int
	Cached     int
	Total      int
}

// maxEventPreviewRunes 是事件里预览文本的上限。
//
// 它守的是 spec §4.3 不变量 6 在发射侧的对应物：事件表的增长与调用次数成正比，
// 不与工具输出体积成正比。全文仍由既有的截断治理落盘（toolcache.go），事件里只留
// 这段预览。按 rune 而不是 byte 计，与该仓其余截断口径一致（中文一个字符 3 字节，
// 按 byte 截会把预览砍成三分之一）。
const maxEventPreviewRunes = 2000

// append 把一条事件放进缓冲。seq 在 flush 时统一分配（见 flush）。
//
// 记录器没启用时直接丢弃：没有配 store 是契约允许的部署形态。
func (e *eventRecorder) append(typ domain.SessionEventType, payload map[string]any) {
	if !e.enabled() {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		// 载荷是本文件自己构造的 map，编不出 JSON 属编程错误。
		panic(fmt.Sprintf("runtime: marshal %s payload: %v", typ, err))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending = append(e.pending, domain.SessionEvent{Type: typ, Time: time.Now(), Data: data})
}

// recordTurnStart 记一个轮次的开始，并把 step 计数归零（spec §4.1：step 每 turn 重置）。
func (e *eventRecorder) recordTurnStart(turn int) {
	e.mu.Lock()
	e.turn, e.step = turn, 0
	e.mu.Unlock()
	e.append(domain.SessionEventTurnStart, map[string]any{"turn": turn})
}

// recordUserMessage 记这一轮的用户输入。
func (e *eventRecorder) recordUserMessage(content string) {
	e.append(domain.SessionEventUserMessage, map[string]any{
		"turn": e.currentTurn(), "content": truncateRunes(content, maxEventPreviewRunes),
	})
}

// recordStepStart 记一次模型请求的开始。
func (e *eventRecorder) recordStepStart() {
	e.append(domain.SessionEventStepStart, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(),
	})
}

// recordAssistantMessage 记模型响应（含它请求的工具调用与 token 用量）。
func (e *eventRecorder) recordAssistantMessage(content string, calls []domain.ToolCall, usage eventUsage, profile string) {
	names := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		names = append(names, map[string]any{"call_id": c.ID, "name": c.Name})
	}
	e.append(domain.SessionEventAssistantMessage, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(),
		"content": truncateRunes(content, maxEventPreviewRunes), "tool_calls": names,
		"usage": map[string]any{
			"prompt": usage.Prompt, "completion": usage.Completion,
			"cached": usage.Cached, "total": usage.Total,
		},
		"model_profile": profile,
	})
}

// recordToolCall 记一次工具调用**被派发之前**的事实（spec §5 屏障 2 的前提）。
func (e *eventRecorder) recordToolCall(call domain.ToolCall) {
	arguments, err := json.Marshal(call.Arguments)
	if err != nil {
		panic(fmt.Sprintf("runtime: marshal tool call arguments for %s: %v", call.ID, err))
	}
	e.append(domain.SessionEventToolCall, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(),
		"call_id": call.ID, "name": call.Name,
		"arguments": truncateRunes(string(arguments), maxEventPreviewRunes),
	})
}

// recordToolResult 记一次工具调用的结果。
//
// **每条记录过的 tool/call 都必须有它**（spec §4.3.1 第 1 条）：工具失败、取消、被
// 拒绝一样要发，`isError` 为真。少发一条，恢复时会把它当成「崩在工具里」而补一条
// 合成结果，日志就与真实发生的事不符了。
func (e *eventRecorder) recordToolResult(callID string, preview string, isError bool, dur time.Duration) {
	e.append(domain.SessionEventToolResult, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(),
		"call_id": callID, "preview": truncateRunes(preview, maxEventPreviewRunes),
		"is_error": isError, "duration_ms": dur.Milliseconds(),
	})
}

// recordStepEnd 记一步的结束，并把 step 计数推进一格。
func (e *eventRecorder) recordStepEnd(reason string) {
	e.append(domain.SessionEventStepEnd, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(), "reason": reason,
	})
	e.mu.Lock()
	e.step++
	e.mu.Unlock()
}

// recordTurnEnd 记一个轮次的结束。
func (e *eventRecorder) recordTurnEnd(reason string) {
	e.append(domain.SessionEventTurnEnd, map[string]any{
		"turn": e.currentTurn(), "reason": reason,
	})
}

func (e *eventRecorder) currentTurn() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.turn
}

func (e *eventRecorder) currentStep() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.step
}

// flush 把缓冲里的事件落盘（spec §5 的三个屏障调它）。
//
// seq 在这里才分配：P1 的 store 要求首个 seq 等于库里的 next-seq，而库里走到哪只有
// 这一刻才知道（同一会话可能有别的写入者）。第一次 flush 用 ReadFrom 对齐游标，
// 之后按本次执行自己写过的条数递增。
//
// **失败就是失败**：调用方（屏障）据此决定不发请求、不进工具体、不开下一步。
// 缓冲保持不变，让调用方能在重试时不丢事件。
func (e *eventRecorder) flush(ctx context.Context) error {
	if !e.enabled() {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.pending) == 0 {
		return nil
	}
	if !e.seqKnown {
		existing, err := e.store.ReadFrom(ctx, e.session, 0)
		if err != nil {
			return fmt.Errorf("align session event cursor for %q: %w", e.session, err)
		}
		e.nextSeq = int64(len(existing))
		e.seqKnown = true
	}
	batch := make([]domain.SessionEvent, len(e.pending))
	for i, event := range e.pending {
		event.Seq = e.nextSeq + int64(i)
		batch[i] = event
	}
	if err := e.store.Append(ctx, e.session, batch); err != nil {
		return fmt.Errorf("persist session events for %q: %w", e.session, err)
	}
	e.nextSeq += int64(len(batch))
	e.pending = e.pending[:0]
	return nil
}

// truncateRunes 按 rune 截断并标注截断量，使读的人知道自己看的是一段而不是全部。
//
// 截断标注本身也占 rune：直接把内容砍到 limit 再拼标注会让总长度超过 limit
// （截断测试真的抓到过这个：标注比它替换掉的内容还长）。所以先按 limit 估出标注的
// 长度上限，把内容留出这部分空间，再用真实保留量重新生成标注——保留量只会比估计时
// 更小或相等，标注只会更短或相等，总长度因此保证不超过 limit。
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	total := len(runes)
	if total <= limit {
		return s
	}
	budget := limit
	if budget < 0 {
		budget = 0
	}
	estimate := fmt.Sprintf("\n…[truncated: %d of %d runes shown]", budget, total)
	kept := budget - len([]rune(estimate))
	if kept < 0 {
		kept = 0
	}
	suffix := fmt.Sprintf("\n…[truncated: %d of %d runes shown]", kept, total)
	return string(runes[:kept]) + suffix
}
