package runtime

import (
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/port"
)

// inferenceMessageDebug is the per-message breakdown the debug probe emits: it
// tells how large each message in the outgoing prompt is (in runes, not bytes,
// so multibyte content is not over-counted) and shows a short single-line
// preview, so a bloated prompt can be traced to the exact message carrying it.
type inferenceMessageDebug struct {
	Index     int
	Role      string
	Chars     int
	ToolCalls int
	// ToolCallChars 是这条消息里 tool call **参数**的体积（rune 数），与 Chars 分开
	// 报而不是并进去。
	//
	// 为什么必须单独有它：#148 之后 session.max_turn_chars 的预算**已经在裁**
	// ToolCalls[].Arguments（internal/runtime/session_turns.go 的 applyTurnBudget），
	// 而这个探针当时只数 Content。于是「被预算管住了却量不到」——G3 唯一必须保持
	// 可度量的东西是请求体积，从 runtime.debug 读到的数会系统性低估它。
	//
	// 分开报而不是并进 Chars：Chars 与 total_content_chars 是既有日志的口径，并进去
	// 会让新旧两段日志不可比，而这个探针存在的意义正是纵向比较「prompt 什么时候涨的」。
	ToolCallChars int
	Images        int
	Preview       string
}

// debugPreviewRunes is how many leading runes of a message's content the probe
// shows. Enough to identify a block (persona, catalog, history) without dumping
// the whole prompt into the log.
const debugPreviewRunes = 200

// inferenceRequestDebug computes the debug breakdown of an outgoing inference
// request: the total content size across all messages and a per-message
// summary. It is pure (no logging) so it can be unit-tested and so the caller
// controls whether and how to emit it.
func inferenceRequestDebug(req port.InferenceRequest) (totalChars, totalToolCallChars int, msgs []inferenceMessageDebug) {
	msgs = make([]inferenceMessageDebug, len(req.Messages))
	for i, m := range req.Messages {
		n := len([]rune(m.Content))
		totalChars += n
		// 参数体积单独累计——见 inferenceMessageDebug.ToolCallChars 的说明。
		// 数的是参数名与参数值本身，不含 JSON 结构字符：这个数要和
		// applyTurnBudget 花掉的额度可比，而那份预算算的也是内容的 rune 数。
		callChars := 0
		for _, c := range m.ToolCalls {
			for name, value := range c.Arguments {
				callChars += len([]rune(name)) + len([]rune(value))
			}
		}
		totalToolCallChars += callChars
		msgs[i] = inferenceMessageDebug{
			Index:         i,
			Role:          m.Role,
			Chars:         n,
			ToolCalls:     len(m.ToolCalls),
			ToolCallChars: callChars,
			Images:        len(m.Images),
			Preview:       previewRunes(m.Content, debugPreviewRunes),
		}
	}
	return totalChars, totalToolCallChars, msgs
}

// logInferenceRequest emits the debug probe for one outgoing request: a summary
// line (message count, tool count, total content size) followed by one line per
// message. Gated by the caller on r.debug, so it is silent unless the config
// file's runtime.debug toggle is on.
func (r *Runtime) logInferenceRequest(taskID string, req port.InferenceRequest) {
	total, totalToolCalls, msgs := inferenceRequestDebug(req)
	r.logger.Info("debug inference request",
		"task_id", taskID,
		"messages", len(req.Messages),
		"tools", len(req.Tools),
		"total_content_chars", total,
		// 与 total_content_chars 并列而不是并入：见 inferenceMessageDebug.ToolCallChars。
		"total_tool_call_chars", totalToolCalls,
	)
	for _, m := range msgs {
		r.logger.Info("debug inference message",
			"task_id", taskID,
			"index", m.Index,
			"role", m.Role,
			"chars", m.Chars,
			"tool_calls", m.ToolCalls,
			"tool_call_chars", m.ToolCallChars,
			"images", m.Images,
			"preview", m.Preview,
		)
	}
}

// logContextBlocks emits the per-section size accounting of the base prompt
// (persona/context files, capability catalog, conversation history, ...). The
// per-message probe only reports each message's total, which is enough to see
// that the base prompt grew but not what grew it — this closes that gap.
// Gated by the caller on r.debug.
func (r *Runtime) logContextBlocks(taskID string, blocks []cognitive.BlockSize) {
	total := 0
	for _, b := range blocks {
		total += b.Chars
	}
	for _, b := range blocks {
		r.logger.Info("debug context block",
			"task_id", taskID,
			"block", b.Name,
			"chars", b.Chars,
			"total_chars", total,
		)
	}
}

// previewRunes renders the first max runes of s as a single line (newlines and
// carriage returns folded to a visible marker), appending a remainder marker
// when content was dropped. It counts by rune so it never splits a multibyte
// character.
func previewRunes(s string, max int) string {
	oneLine := strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", "⏎")
	r := []rune(oneLine)
	if len(r) <= max {
		return oneLine
	}
	return string(r[:max]) + fmt.Sprintf("…(+%d)", len(r)-max)
}
