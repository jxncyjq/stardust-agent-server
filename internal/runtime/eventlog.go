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
//
// 接收者契约：本类型的所有方法都假设接收者非 nil。字面 nil 接收者是编程错误，不是本
// 类型承诺处理的状态，panic 是预期行为，不需要也不应该把它做成「安全返回」。
// newEventRecorder 保证它返回的 recorder 永不是字面 nil（拿不到 session id 就直接
// panic），所以持有 recorder 的调用方必须保证字段被赋过值：某次部署没有配置 store 时，
// 正确做法是持有一个 store 字段为 nil 的 *eventRecorder（走 enabled() 判断的可选路径），
// 而不是让持有 recorder 的字段保持零值 nil。
//
// 这与 enabled() 是两回事，不要混为一谈：enabled() 处理的是「Runtime 没配 store」——
// 这是契约显式声明的可选部署形态（见 enabled() 的文档注释），不是对错误状态的兜底。
// enabled() 方法体里的 e != nil 只是让这一个方法本身能在 nil 接收者上求值，不代表
// 其余方法对 nil 接收者也是安全的：recordUserMessage/recordStepStart/
// recordAssistantMessage/recordToolCall/recordToolResult/recordTurnEnd 六个方法在
// 构造 e.append 的参数时会先经由 currentTurn()/currentStep() 直接 e.mu.Lock()，
// 在字面 nil 接收者上仍会 panic；不要把 enabled() 的 e != nil 读成「本类型对 nil
// 接收者是安全的」承诺。
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
//
// 方法体里的 e != nil 只是让 enabled() 自己能在 nil 接收者上求值（append/flush/
// recordTurnStart/recordStepEnd 都先调它），不是本类型「nil 接收者安全」的承诺——
// 其余六个 record* 方法在构造参数时会先经由 currentTurn()/currentStep() 直接解引用
// e.mu，enabled() 的判空来不及保护它们。接收者契约见 eventRecorder 类型文档。
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
//
// 先过 enabled() 再碰 e.mu：目的是在「没配 store」这个契约允许的可选部署形态下，不必为
// 丢弃一次记录而白白加锁（见 enabled() 的文档注释）。这**不代表**本方法与其余六个
// record* 方法在 nil 接收者上的行为一致——那六个方法会先经由 currentTurn()/
// currentStep() 直接解引用 e.mu，在字面 nil 接收者上仍会 panic；本方法把 enabled()
// 判断放在任何解引用之前，只是这一个方法自己的写法。真正的接收者契约见 eventRecorder
// 类型文档：所有方法都假设接收者非 nil，nil 接收者是编程错误，panic 是预期行为。
func (e *eventRecorder) recordTurnStart(turn int) {
	if !e.enabled() {
		return
	}
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
//
// tool_calls 摘要数组**故意不截断**，这是权衡过的决定，不是漏掉的截断：
// spec §4.3.1 第 2 条要求 P3 投影按 call_id 配对，而这个数组正是配对时要用的清单。
// 截掉其中任意一项，恢复/投影就会缺项——那是比容量风险更重的正确性缺陷，用一个
// 换另一个不划算。content/arguments/preview 三处按 maxEventPreviewRunes 截断是因为
// 它们是自由文本，截了不影响可配对性；tool_calls 每项只是 call_id+name，本身已经
// 很小，风险来自「条目数」而不是「单项体积」。
//
// 真的超过 P1 的 64 KiB/事件硬上限（internal/storage.maxSessionEventDataBytes）时，
// flush → Append 会整批失败并返回包装过的错误，由 Task 4 的屏障感知并阻断执行——
// 这是 fail-loud 的正确表现，不是数据损坏（不静默丢事件、不裁剪配对信息）。
// TestRecordAssistantMessageFlushesWithManyToolCalls 用一个远超真实单步工具调用数量
// 的上界证明：在这个上界内，flush 确实能落盘、不会撞到那个上限。
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
//
// 同 recordTurnStart：先过 enabled() 再碰 e.mu，避免在「没配 store」时白白加锁。这不
// 意味着与其余六个 record* 方法在 nil 接收者上的行为一致——见 recordTurnStart 与
// eventRecorder 类型文档对接收者契约的说明。
func (e *eventRecorder) recordStepEnd(reason string) {
	if !e.enabled() {
		return
	}
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

// barrier 是一个 fail-closed 的落盘点（spec §5）。
//
// 三处调用：模型请求前、进工具体前、开下一步前。刷不动就返回错误，调用方**不做那件事**。
// 这是行为改变——数据库写不动时任务会失败而不是照跑——理由在屏障 2：tool/call 必须
// 先落盘，否则崩在工具体里就成了「工具真的执行过、但日志里没有这次调用」，恢复时补不出
// 合成结果，而工具正是有外部副作用的那一端。「先记录再执行」保证任何真发生过的副作用
// 在日志里都有它的调用。
//
// at 是这个屏障的位置，进错误信息——排查的人要立刻知道是哪一处挡住了。
func (e *eventRecorder) barrier(ctx context.Context, at string) error {
	if err := e.flush(ctx); err != nil {
		return fmt.Errorf("session event barrier %s: %w", at, err)
	}
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
