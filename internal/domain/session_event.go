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

// SessionEventTurnStart 常量及其他事件类型常量定义会话事件日志中每个事件的类型标识。
// 见 spec §4.1 了解每个事件的生命周期与事件顺序。
const (
	// SessionEventTurnStart 记录收到一条用户输入时开启一个轮次。
	SessionEventTurnStart SessionEventType = "turn/start"
	// SessionEventUserMessage 记录用户那条消息本身。
	SessionEventUserMessage SessionEventType = "user/message"
	// SessionEventStepStart 记录准备发一次模型请求时一步的开始。
	SessionEventStepStart SessionEventType = "step/start"
	// SessionEventAssistantMessage 记录模型响应装配完成（含 usage）。
	SessionEventAssistantMessage SessionEventType = "assistant/message"
	// SessionEventToolCall 记录一次工具调用被派发之前。
	SessionEventToolCall SessionEventType = "tool/call"
	// SessionEventToolResult 记录工具返回（含预览与 spill 定位符）。
	SessionEventToolResult SessionEventType = "tool/result"
	// SessionEventStepEnd 记录一步结束（含失败与取消）。
	SessionEventStepEnd SessionEventType = "step/end"
	// SessionEventTurnEnd 记录轮次结束。
	SessionEventTurnEnd SessionEventType = "turn/end"
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

// StepEndReasonCompleted 标记一步成功完成（spec §4.1）。
const StepEndReasonCompleted = "completed"

// StepEndReasonFailed 标记一步执行失败（spec §4.1）。
const StepEndReasonFailed = "failed"

// StepEndReasonCancelled 标记一步被取消（spec §4.1）。
const StepEndReasonCancelled = "cancelled"

// StepEndReasonMaxTokens 标记一步因达到 token 上限而结束（spec §4.1）。
const StepEndReasonMaxTokens = "max_tokens"

// TurnEndReasonCompleted 标记一个轮次成功完成（spec §4.1）。
const TurnEndReasonCompleted = "completed"

// TurnEndReasonFailed 标记一个轮次执行失败（spec §4.1）。
const TurnEndReasonFailed = "failed"

// TurnEndReasonCancelled 标记一个轮次被取消（spec §4.1）。
const TurnEndReasonCancelled = "cancelled"

// TurnEndReasonInterrupted 标记一个轮次因外部中断（如崩溃恢复）而结束。
// 仅由崩溃恢复补出，正常路径不得使用它——它是「这段历史不是自己结束的」的唯一记号（spec §4.1）。
const TurnEndReasonInterrupted = "interrupted"

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
