package storage

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// projectTranscript 把一条会话的事件流投影成 provider transcript——assistant
// 消息带 tool_calls，其后跟与之 call_id 配对的 tool 消息（spec §6 的 G3）。
//
// 它与 projectTurns 并列而不是取代它：turns 是「谁说了什么」的视图（G3 关闭时
// 走它），transcript 是「模型看到的完整往返」（G3 打开时走它）。两者读同一批
// 事件，谁也不改谁。
//
// events 必须按 seq 升序传入——这正是 ReadFrom 的返回契约（session_events.go：
// `ORDER BY seq`）。本函数不重新排序，理由与 projectTurns 相同：重新排序既是不
// 必要的工作，也会掩盖「调用方传错顺序」这类编程错误。**但它也不依赖顺序来配对**，
// 见下。
//
// # 按 call_id 配对，不按位置（spec §4.3.1 第 2 条）
//
// 崩溃恢复补出的 tool/result 排在日志尾部，可能排在自己那次调用的 step/end 之后、
// 且顺序与调用相反。按位置配会把结果配到错误的调用上——而 port.InferenceMessage
// 的文档注释写死了「an OpenAI-compatible provider rejects a tool message it
// cannot pair with a preceding tool call」，配错的后果是整个请求被 provider 拒收，
// 不是显示得难看一点。所以这里先扫一遍把结果按 call_id 建成表，再由 assistant 的
// tool_calls 清单去查表，全程不看任何事件的相对位置。
// 守卫：TestResultsPairByCallIDEvenWhenTheyTrailAtTheEnd。
//
// # 只宣告有结果的调用
//
// 一条被记录过却没有结果事件的 tool/call 只可能来自进程硬崩（spec §4.3.1 第 1 条；
// 「记了一次根本没发生的调用」这个反面由 runtime 的 dropBufferedToolCall 挡住）。
// 把它放进 tool_calls 会让 provider 等一条永远不来的 tool 消息，同样拒收。所以
// 必须**先扫完结果、再决定宣告什么**——边走边配做不到这件事，因为决定「c2 没有
// 结果」需要看完整条日志。守卫：TestAnUnansweredCallIsNotAnnouncedInTheTranscript。
//
// 「未答」是崩溃后合法的残留状态，跳过它是在产出一份**合法**的 transcript，不是
// 在给错误兜底；真正的坏数据（空 call_id）在下面被显式拒绝，不与它混为一谈。
//
// 未知事件类型一律报错并指名，不静默跳过——server 侧的类型是闭集但它会长，静默
// 跳过意味着加了新类型之后 transcript 会悄悄少东西而没人发现。
//
// 纯函数：不碰数据库、不读文件、不看时钟。同一批事件永远投影出同一个结果。
func projectTranscript(sessionID string, events []domain.SessionEvent) ([]port.InferenceMessage, error) {
	// 第一遍：把所有结果按 call_id 收起来。必须先扫完，因为恢复补出的结果排在尾部。
	results, err := collectToolResults(sessionID, events)
	if err != nil {
		return nil, err
	}

	msgs := make([]port.InferenceMessage, 0, len(events)/3)
	for _, event := range events {
		switch event.Type {
		case domain.SessionEventUserMessage:
			var payload struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, fmt.Errorf("project transcript for %q: decode user/message at seq %d: %w",
					sessionID, event.Seq, err)
			}
			msgs = append(msgs, port.InferenceMessage{
				Role:    port.RoleUser,
				Content: payload.Content,
			})

		case domain.SessionEventAssistantMessage:
			var payload struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					CallID string `json:"call_id"`
					Name   string `json:"name"`
				} `json:"tool_calls"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, fmt.Errorf("project transcript for %q: decode assistant/message at seq %d: %w",
					sessionID, event.Seq, err)
			}
			calls := make([]domain.ToolCall, 0, len(payload.ToolCalls))
			for _, c := range payload.ToolCalls {
				// 空 call_id 只可能来自坏掉的写入方：recordAssistantMessage 原样抄
				// domain.ToolCall.ID，而它是 provider 给的调用标识。不拒的话它会掉进
				// 下面那条「未答就不宣告」的 continue——因为 results 里永远不会有空键
				// （collectToolResults 已经拒绝了空 call_id 的结果）——于是「写入方坏了」
				// 与「硬崩留下未答调用」这两件性质完全不同的事被折叠成同一种无声后果。
				// 守卫：TestAnAnnouncedCallWithAnEmptyCallIDIsRefused。
				if strings.TrimSpace(c.CallID) == "" {
					return nil, fmt.Errorf("project transcript for %q: assistant/message at seq %d announces a tool call with no call_id, so it can never be paired",
						sessionID, event.Seq)
				}
				// 只宣告有结果的调用——见函数头那一节。
				if _, answered := results[c.CallID]; !answered {
					continue
				}
				calls = append(calls, domain.ToolCall{ID: c.CallID, Name: c.Name})
			}
			msg := port.InferenceMessage{Role: port.RoleAssistant, Content: payload.Content}
			if len(calls) > 0 {
				msg.ToolCalls = calls
			}
			msgs = append(msgs, msg)
			// 紧跟着把这批调用的结果按 call_id 取出来铺在后面——provider 要求
			// tool 消息紧随宣告它的 assistant。
			for _, c := range calls {
				msgs = append(msgs, port.InferenceMessage{
					Role:       port.RoleTool,
					ToolCallID: c.ID,
					Content:    results[c.ID],
				})
			}

		case domain.SessionEventTurnStart, domain.SessionEventTurnEnd,
			domain.SessionEventStepStart, domain.SessionEventStepEnd,
			domain.SessionEventToolCall, domain.SessionEventToolResult:
			// 这六类不直接产出消息：边界事件没有模型可读的内容；tool/call 的信息
			// 已经在 assistant 的 tool_calls 里；tool/result 已在第一遍收进 results。
			//
			// 逐条列出而不是让它们落进 default，是为了让「加了新事件类型却忘了决定
			// 它怎么投影」落到下面那条 default 的运行期报错上，而不是悄悄少算。

		default:
			return nil, fmt.Errorf("project transcript for %q: unknown event type %q at seq %d",
				sessionID, event.Type, event.Seq)
		}
	}
	return msgs, nil
}

// collectToolResults 扫一遍事件，把每条 tool/result 的模型可读内容按 call_id 收起来。
//
// 单独一遍是必须的：恢复补出的结果排在日志尾部，边走边配会漏掉它们。
//
// 同一个 call_id 出现多条结果时后者覆盖前者：恢复补出的合成结果是对那次调用的
// 最终裁定，它排在尾部，覆盖正是想要的语义。
func collectToolResults(sessionID string, events []domain.SessionEvent) (map[string]string, error) {
	results := make(map[string]string)
	for _, event := range events {
		if event.Type != domain.SessionEventToolResult {
			continue
		}
		var payload struct {
			CallID       string `json:"call_id"`
			Preview      string `json:"preview"`
			IsError      bool   `json:"is_error"`
			SpillLocator string `json:"spill_locator"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil, fmt.Errorf("project transcript for %q: decode tool/result at seq %d: %w",
				sessionID, event.Seq, err)
		}
		// 空 call_id 的结果配不上任何调用。放过它不只是丢一条 tool 消息：它对应的
		// 那次调用会因此被判成「未答」而整个从 transcript 里消失，无声无息。
		// 守卫：TestAToolResultWithAnEmptyCallIDIsRefused。
		if strings.TrimSpace(payload.CallID) == "" {
			return nil, fmt.Errorf("project transcript for %q: tool/result at seq %d has no call_id, so it cannot be paired",
				sessionID, event.Seq)
		}
		results[payload.CallID] = renderTranscriptToolContent(payload.Preview, payload.IsError, payload.SpillLocator)
	}
	return results, nil
}

// renderTranscriptToolContent 把一条结果渲染成模型看到的文本。
//
// 只放**预览**，不放全文（spec §6：全文仍靠 read_file 按定位符取）——这正是 G3
// 的成本上界所在：预览在写入侧已按 maxEventPreviewRunes 截过，这里不再展开。
// 定位符会一并给出，让模型知道去哪取全文；出错的结果显式标出，否则模型会把一条
// 错误当成数据。
func renderTranscriptToolContent(preview string, isError bool, spillLocator string) string {
	var b strings.Builder
	if isError {
		b.WriteString("[错误] ")
	}
	b.WriteString(preview)
	if strings.TrimSpace(spillLocator) != "" {
		b.WriteString("\n[全文见 ")
		b.WriteString(spillLocator)
		b.WriteString("，用 read_file 按需翻页]")
	}
	return b.String()
}
