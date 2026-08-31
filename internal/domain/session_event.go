package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// SessionEventType 是会话事件日志的类型闭集（见 spec §4.1）。
//
// 闭集而不是自由字符串：一个读不懂的类型意味着这条日志是另一个版本写的，
// 而把它当作「可以跳过的一行」会让重建出的历史悄悄缺一段。
type SessionEventType string

const (
	SessionEventTurnStart        SessionEventType = "turn/start"
	SessionEventUserMessage      SessionEventType = "user/message"
	SessionEventStepStart        SessionEventType = "step/start"
	SessionEventAssistantMessage SessionEventType = "assistant/message"
	SessionEventToolCall         SessionEventType = "tool/call"
	SessionEventToolResult       SessionEventType = "tool/result"
	SessionEventStepEnd          SessionEventType = "step/end"
	SessionEventTurnEnd          SessionEventType = "turn/end"
)

// knownSessionEventTypes 是上面那组常量的集合形式，供 ValidateSessionEventType 查。
var knownSessionEventTypes = map[SessionEventType]struct{}{
	SessionEventTurnStart:        {},
	SessionEventUserMessage:      {},
	SessionEventStepStart:        {},
	SessionEventAssistantMessage: {},
	SessionEventToolCall:         {},
	SessionEventToolResult:       {},
	SessionEventStepEnd:          {},
	SessionEventTurnEnd:          {},
}

// step/end 的 reason 闭集（spec §4.1）。
const (
	StepEndReasonCompleted = "completed"
	StepEndReasonFailed    = "failed"
	StepEndReasonCancelled = "cancelled"
	StepEndReasonMaxTokens = "max_tokens"
)

// turn/end 的 reason 闭集（spec §4.1）。interrupted 只由崩溃恢复补出，
// 正常路径不得使用它——它是「这段历史不是自己结束的」这个事实的唯一记号。
const (
	TurnEndReasonCompleted   = "completed"
	TurnEndReasonFailed      = "failed"
	TurnEndReasonCancelled   = "cancelled"
	TurnEndReasonInterrupted = "interrupted"
)

// SessionEvent 是会话事件日志里的一行。
//
// Data 保持 JSON 原文（json.RawMessage）而不是解成具体结构：这一层只负责
// 「按 seq 存取一串不可变的事件」，各事件载荷的形状归它们的生产者与消费者管。
// 存储层去解载荷，就等于每加一种事件都要改存储层。
type SessionEvent struct {
	// Seq 每会话单调、连续，从 0 起。
	Seq  int64            `json:"seq"`
	Type SessionEventType `json:"type"`
	Time time.Time        `json:"time"`
	Data json.RawMessage  `json:"data"`
}

// ValidateSessionEventType 判定一个事件类型是否属于这个构建认得的闭集。
//
// 空与未知分开报：空是「字段忘了填」，未知是「另一个版本写的」，排查方向不同。
func ValidateSessionEventType(typ SessionEventType) error {
	if typ == "" {
		return fmt.Errorf("session event type is empty")
	}
	if _, ok := knownSessionEventTypes[typ]; !ok {
		return fmt.Errorf("unknown session event type %q; this build does not understand it, "+
			"which usually means the log was written by a newer version", typ)
	}
	return nil
}
