package runtime

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
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

// pinCachePrefix marks the first message's stable cache-prefix rune length, the
// boundary the adapter turns into a prompt-cache breakpoint. No-op on an empty
// exchange or a non-positive length. Kept separate from newConversation so its
// many callers stay unchanged.
func (c *conversation) pinCachePrefix(n int) {
	if n > 0 && len(c.messages) > 0 {
		c.messages[0].StablePrefixLen = n
	}
}

// appendHistory appends the session's history transcript (G3 on) right after
// message[0], VERBATIM.
//
// Verbatim matters in one specific way: it must not touch StablePrefixLen.
// That field is only meaningful on message[0] — the adapter turns it into the
// provider's cache_control breakpoint — so copying message[0]'s value onto a
// history message would place a second breakpoint inside content that changes
// every task. The history the projection produces carries a zero there, and
// this function keeps it that way.
// 守卫：TestTheCacheBreakpointStaysOnTheFirstMessage。
func (c *conversation) appendHistory(history []port.InferenceMessage) {
	c.messages = append(c.messages, history...)
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

// modelFacingToolContent is the raw text one tool result reaches the model as,
// before any truncation or spilling: the output on success, an explicitly
// labelled failure line otherwise. A failed call is reported to the model
// rather than dropped — the model needs to see the failure to recover, and a
// provider rejects an assistant tool call left unanswered.
//
// It lives here, next to appendToolResults, but is applied at DISPATCH time
// (executeToolCalls) because the rendering that consumes it has to happen there:
// see appendToolResults for why. One function so the two sites cannot drift into
// spilling one string and showing the model another.
func modelFacingToolContent(res domain.ToolResult) string {
	if !res.Success {
		return "failed: " + res.Error
	}
	return res.Output
}

// appendToolResults records one tool turn per executed call, paired by call ID,
// from content ALREADY rendered by renderToolResultContent (rendered is keyed by
// call ID).
//
// The rendering deliberately does not happen here any more. It writes the spill
// file whose path is spec §4.1's spill_locator, and the tool/result event that
// must carry that locator is recorded inside executeToolCalls, one call at a
// time, BEFORE this function runs for the round (and, for a multi-call round,
// already flushed to disk by the next call's pre-dispatch barrier). Rendering
// here would mean the locator only came into existence after the event that
// needs it had been written — so the render moved to the dispatch site and the
// text it produced is handed here. Rendering it twice instead was rejected: two
// writes of the same content-addressed file, two chances to disagree.
//
// A call with no rendered entry is a wiring error, not a state to absorb:
// executeToolCalls fills the map in the same loop that fills results, so every
// dispatched call has one. Panicking here (fail-loud 铁律) keeps a future caller
// from silently dropping a tool turn and leaving the provider with an
// unanswered assistant tool call.
func (c *conversation) appendToolResults(calls []domain.ToolCall, rendered map[string]string) {
	for _, call := range calls {
		content, ok := rendered[call.ID]
		if !ok {
			panic(fmt.Sprintf("runtime: no rendered tool result for call %s (%s); "+
				"every dispatched call must be answered back to the model", call.ID, call.Name))
		}
		c.messages = append(c.messages, port.InferenceMessage{
			Role:       port.RoleTool,
			ToolCallID: call.ID,
			Content:    content,
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

// applyCompaction replaces messages[1:preserveStart] with a single summary
// user message, pinning messages[0] (the stable cache prefix / base prompt)
// and keeping the recent tail from preserveStart onward verbatim.
func (c *conversation) applyCompaction(preserveStart int, summary string) {
	tail := append([]port.InferenceMessage(nil), c.messages[preserveStart:]...)
	c.messages = append([]port.InferenceMessage{
		c.messages[0],
		{Role: port.RoleUser, Content: "[对话摘要]\n" + summary},
	}, tail...)
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

	// repeatWarnCount and repeatAbortCount bound how many times ONE tool-call
	// signature (callsKey: names+arguments) may recur across a whole task,
	// regardless of whether the recurrences are consecutive. They complement
	// repeatWarnStreak/repeatAbortStreak (which only see consecutive repeats):
	// an A→B→A→B loop never trips the streak guard but does accumulate here.
	// At the warn count the loop injects a steering turn; at the abort count it
	// stops the tool loop (the non-consecutive analogue of the 152-round abort).
	repeatWarnCount  = 4
	repeatAbortCount = 6

	// toolLoopCap bounds how many times ONE tool (keyed by NAME ONLY, ignoring
	// arguments) may be called across a whole task. The repeatWarn/Abort guards
	// key on callsKey (name+arguments), so "same tool, different args" retries
	// slip past them — the failure that ran task …955100 to 60 fetch_url calls.
	// This caps the tool regardless of argument variation; hitting it cuts the
	// loop via the same loopCut→closing path as the signature guard. It is a hard
	// ceiling (no config toggle): a runaway loop must stop.
	toolLoopCap = 30
	// toolSameFailWarn is how many times ONE tool (by NAME) may FAIL across a task
	// before the model is warned to stop retrying it and answer with what it has.
	// Warning only — no hard halt (spec §7 leaves halt@8+hard_stop for later).
	toolSameFailWarn = 3
)

// repeatGuard counts, per task, how many times each tool-call signature
// (callsKey) has been requested — including non-consecutive recurrences. One
// guard lives for the whole tool loop (loopState.repeatGuard); it is the
// non-consecutive counterpart to repeatedCallStreak.
type repeatGuard struct {
	seen map[string]int
}

// newRepeatGuard returns an empty guard.
func newRepeatGuard() *repeatGuard {
	return &repeatGuard{seen: make(map[string]int)}
}

// record increments the running count for signature and returns the new count
// (1 on first sighting).
func (g *repeatGuard) record(signature string) int {
	g.seen[signature]++
	return g.seen[signature]
}

// sharedToolBudget is the task's per-tool-name counter seen through
// tool.LoopBudget: the seam that lets something the tool loop dispatched into —
// a plugin's call_tool host function — spend the SAME allowance the loop spends.
//
// There is exactly one per RunTask (loopState.toolNameGuard), and the loop's own
// per-name accounting goes through it too, so the map behind it has one writer
// path rather than two. A counter of its own for the plugin would be a channel
// around the task's total budget: toolLoopCap would see a quiet task while a
// contributor drove one tool without limit.
//
// The mutex is not decoration and not a leftover. repeatGuard is a plain map with
// no synchronization of its own, and this budget is handed to every dispatched
// tool call on its context: a handler is free to charge it from a goroutine of its
// own while the loop records the next round, and an unsynchronized map racing that
// way is a fatal "concurrent map writes", not a wrong number.
type sharedToolBudget struct {
	mu    sync.Mutex
	guard *repeatGuard
}

// newSharedToolBudget returns an empty per-task tool budget.
func newSharedToolBudget() *sharedToolBudget {
	return &sharedToolBudget{guard: newRepeatGuard()}
}

// Record counts one call of the tool named name and returns the task's running
// total for that name together with toolLoopCap, the ceiling in force. It
// implements tool.LoopBudget; see that interface for why counting and reporting
// are one call rather than a peek plus an increment.
func (b *sharedToolBudget) Record(name string) (count, limit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.guard.record(name), toolLoopCap
}

// The budget is installed on every dispatched call's context as a
// tool.LoopBudget, so a signature drift must fail here rather than at the
// installation site.
var _ tool.LoopBudget = (*sharedToolBudget)(nil)

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
