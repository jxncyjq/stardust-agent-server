package runtime

import (
	"slices"

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

// render returns the messages to send.
func (c *conversation) render(maxChars int) []port.InferenceMessage {
	return slices.Clone(c.messages)
}
