package runtime

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// conversation accumulates the multi-turn exchange of one tool loop.
//
// It is append-only by design. The previous context was a single re-sent user
// message whose tool results were deduplicated by (name, arguments), so a model
// that called read_file twice on the same path saw one entry, not two — every
// round's prompt came out byte-identical and the model kept re-issuing the same
// call. On 2026-07-23 that cost one task 152 identical reads over 554s. Keeping
// each turn distinct is what makes repetition visible to the model.
type conversation struct {
	messages []port.InferenceMessage
	// lastLoaded is the loaded-capability block as it was last shown to the
	// model, so syncLoaded only spends a turn when the block actually changed.
	lastLoaded string
}

// newConversation starts an exchange whose first turn is the task framing, with
// the task's images attached to it — the same placement the single-turn
// contract used.
func newConversation(basePrompt string, images []string) *conversation {
	return &conversation{messages: []port.InferenceMessage{{
		Role:    port.RoleUser,
		Content: basePrompt,
		Images:  images,
	}}}
}

// appendAssistant records the model's turn. calls may be empty (a plain textual
// answer) and text may be empty (a pure tool-call turn).
func (c *conversation) appendAssistant(text string, calls []domain.ToolCall) {
	c.messages = append(c.messages, port.InferenceMessage{
		Role:      port.RoleAssistant,
		Content:   text,
		ToolCalls: calls,
	})
}

// appendToolResults records one tool turn per executed call, paired by call ID.
// A failed call is reported to the model as its own tool turn rather than being
// dropped: the model needs to see the failure to recover, and a provider
// rejects an assistant tool call left unanswered.
func (c *conversation) appendToolResults(calls []domain.ToolCall, results []domain.ToolResult, maxResultChars int) {
	byID := make(map[string]domain.ToolResult, len(results))
	for _, res := range results {
		byID[res.CallID] = res
	}
	for _, call := range calls {
		res, ok := byID[call.ID]
		if !ok {
			continue
		}
		content := res.Output
		if !res.Success {
			content = "failed: " + res.Error
		}
		c.messages = append(c.messages, port.InferenceMessage{
			Role:       port.RoleTool,
			ToolCallID: call.ID,
			Content:    truncateText(content, maxResultChars),
		})
	}
}

// appendUser adds an out-of-band instruction turn: the loaded-capability block,
// a repeat warning, or the final answer-now nudge.
func (c *conversation) appendUser(text string) {
	c.messages = append(c.messages, port.InferenceMessage{Role: port.RoleUser, Content: text})
}

// syncLoaded shows the loaded-capability block to the model when it changed.
// The block is pinned state rather than a turn, but re-sending it every round
// would be the very thing that made rounds indistinguishable; emitting it only
// on change keeps the exchange append-only and still current.
func (c *conversation) syncLoaded(rendered string) {
	if rendered == "" || rendered == c.lastLoaded {
		return
	}
	c.lastLoaded = rendered
	c.appendUser(rendered)
}

// render returns the messages to send, folding the oldest tool outputs first
// once the exchange exceeds maxChars.
//
// It never drops a message: a provider rejects a tool message whose assistant
// tool_call is absent, so the turn structure is load-bearing. The first user
// turn (task framing) is pinned as well — trimming it would silently delete the
// instructions the run is judged against. maxChars <= 0 disables folding.
func (c *conversation) render(maxChars int) []port.InferenceMessage {
	out := slices.Clone(c.messages)
	if maxChars <= 0 || totalChars(out) <= maxChars {
		return out
	}
	for i := range out {
		if out[i].Role != port.RoleTool {
			continue
		}
		dropped := len([]rune(out[i].Content))
		if dropped == 0 {
			continue
		}
		out[i].Content = fmt.Sprintf("[older tool output trimmed: %d chars]", dropped)
		if totalChars(out) <= maxChars {
			break
		}
	}
	return out
}

const (
	// repeatWarnStreak is how many consecutive identical tool-call rounds (same
	// names and arguments) earn an explicit warning turn, and repeatAbortStreak
	// how many end the tool loop.
	//
	// Multi-turn context makes repetition visible to the model, but visible is
	// not the same as stopped: with max_tool_rounds unlimited, a model that keeps
	// re-reading the same file has nothing else to stop it. Warning first keeps a
	// legitimately repeated call — polling a file expected to change — workable.
	repeatWarnStreak  = 3
	repeatAbortStreak = 8
)

// repeatedCallStreak reports how many consecutive rounds requested exactly the
// same tool calls, counting the pending calls as the newest round. It returns 1
// when the pending calls differ from the previous round, and 0 when there are
// no pending calls.
func repeatedCallStreak(msgs []port.InferenceMessage, calls []domain.ToolCall) int {
	if len(calls) == 0 {
		return 0
	}
	want := callsKey(calls)
	streak := 1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != port.RoleAssistant || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		if callsKey(msgs[i].ToolCalls) != want {
			break
		}
		streak++
	}
	return streak
}

// callsKey identifies a whole round's tool calls by name and arguments. Call
// IDs are excluded on purpose: they are fresh every round and would make every
// comparison unequal.
func callsKey(calls []domain.ToolCall) string {
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, dedupKey(call))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// totalChars is the rune length of every message's content: the unit render
// budgets in.
func totalChars(msgs []port.InferenceMessage) int {
	total := 0
	for _, msg := range msgs {
		total += len([]rune(msg.Content))
	}
	return total
}
