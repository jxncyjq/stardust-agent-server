package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/port"
)

// compactPreserveTail is how many of the most recent messages are kept
// verbatim by compactConversation, before compactionSplit walks that boundary
// back off any orphan RoleTool it lands on.
const compactPreserveTail = 8

// maxCompactionsPerTask caps how many times runToolLoop will compact a single
// task's conversation. st.promptTokens only grows, so without a cap a task
// that stays over compactTokenThreshold would re-summarise every round; the
// cap bounds that spend while still letting the first several compactions
// keep the exchange's token growth in check.
const maxCompactionsPerTask = 3

// compactionSplit computes the range of messages that may be summarised away.
// msgs[0] (the base prompt / stable cache prefix) is always pinned, so
// compactStart is always 1. preserveStart is the index at which the preserved
// recent tail begins: it starts at len-preserveTail and is walked backward until
// it lands on a turn boundary that is NOT a RoleTool message — a RoleTool at the
// tail boundary would be an orphan whose RoleAssistant tool_calls fell into the
// compacted range, which providers reject. ok is false when there is nothing
// worth compacting (fewer than 4 messages, or the preserved tail already covers
// everything after the base prompt).
func compactionSplit(msgs []port.InferenceMessage, preserveTail int) (compactStart, preserveStart int, ok bool) {
	compactStart = 1
	if len(msgs) < 4 || preserveTail < 1 {
		return 0, 0, false
	}
	preserveStart = len(msgs) - preserveTail
	if preserveStart < compactStart {
		preserveStart = compactStart
	}
	// Walk backward off any orphan RoleTool boundary (its assistant is earlier).
	for preserveStart > compactStart && msgs[preserveStart].Role == port.RoleTool {
		preserveStart--
	}
	if preserveStart <= compactStart {
		return 0, 0, false
	}
	return compactStart, preserveStart, true
}

// summarizePrompt renders msgs (the range compactConversation is about to
// compact away) as a single text block for the summarization request: one
// line per message, role-labelled, in order.
func summarizePrompt(msgs []port.InferenceMessage) string {
	var b strings.Builder
	b.WriteString("将以下对话历史压缩成简洁要点，保留关键事实、已获取的信息、未决事项，供后续继续对话参考：\n\n")
	for _, m := range msgs {
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// compactConversation summarises convo's middle range (everything between the
// pinned base prompt and the preserved recent tail) into one visible
// "[对话摘要]" message when the tool loop's accumulated prompt tokens warrant
// it. ok is false, with a nil error, when there is nothing worth compacting
// (compactionSplit found no safe range) — this is not a failure, just a
// no-op.
//
// Fail-loud: if the summarization call itself fails, compactConversation
// returns (false, error) and leaves convo untouched. A failed compaction must
// never be mistaken for cleared history — the 152-incident constraint applies
// here too: losing context because a summarization call errored would be
// exactly the silent-fallback failure mode this codebase forbids.
func (r *Runtime) compactConversation(ctx context.Context, convo *conversation) (bool, error) {
	cs, ps, ok := compactionSplit(convo.messages, compactPreserveTail)
	if !ok {
		return false, nil
	}
	resp, err := r.maas.Generate(ctx, port.InferenceRequest{
		Messages: []port.InferenceMessage{{Role: port.RoleUser, Content: summarizePrompt(convo.messages[cs:ps])}},
	})
	if err != nil {
		return false, fmt.Errorf("compact summarize: %w", err)
	}
	// An empty-but-successful summary (safety filter, truncation, a demo/degraded
	// path returning "") must be treated as a failure, not applied: replacing real
	// history with an empty "[对话摘要]" would lose context exactly like a cleared
	// history — the fail-loud rule forbids passing a zero value off as valid.
	if strings.TrimSpace(resp.Text) == "" {
		return false, fmt.Errorf("compact summarize: empty summary")
	}
	convo.applyCompaction(ps, resp.Text)
	return true, nil
}
