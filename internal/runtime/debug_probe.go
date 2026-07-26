package runtime

import (
	"fmt"
	"strings"

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
	Images    int
	Preview   string
}

// debugPreviewRunes is how many leading runes of a message's content the probe
// shows. Enough to identify a block (persona, catalog, history) without dumping
// the whole prompt into the log.
const debugPreviewRunes = 200

// inferenceRequestDebug computes the debug breakdown of an outgoing inference
// request: the total content size across all messages and a per-message
// summary. It is pure (no logging) so it can be unit-tested and so the caller
// controls whether and how to emit it.
func inferenceRequestDebug(req port.InferenceRequest) (totalChars int, msgs []inferenceMessageDebug) {
	msgs = make([]inferenceMessageDebug, len(req.Messages))
	for i, m := range req.Messages {
		n := len([]rune(m.Content))
		totalChars += n
		msgs[i] = inferenceMessageDebug{
			Index:     i,
			Role:      m.Role,
			Chars:     n,
			ToolCalls: len(m.ToolCalls),
			Images:    len(m.Images),
			Preview:   previewRunes(m.Content, debugPreviewRunes),
		}
	}
	return totalChars, msgs
}

// logInferenceRequest emits the debug probe for one outgoing request: a summary
// line (message count, tool count, total content size) followed by one line per
// message. Gated by the caller on r.debug, so it is silent unless the config
// file's runtime.debug toggle is on.
func (r *Runtime) logInferenceRequest(taskID string, req port.InferenceRequest) {
	total, msgs := inferenceRequestDebug(req)
	r.logger.Info("debug inference request",
		"task_id", taskID,
		"messages", len(req.Messages),
		"tools", len(req.Tools),
		"total_content_chars", total,
	)
	for _, m := range msgs {
		r.logger.Info("debug inference message",
			"task_id", taskID,
			"index", m.Index,
			"role", m.Role,
			"chars", m.Chars,
			"tool_calls", m.ToolCalls,
			"images", m.Images,
			"preview", m.Preview,
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
