package runtime

import (
	"fmt"
	"log/slog"
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
// rejects an assistant tool call left unanswered. Oversized successful results
// are cached to toolRoot/cacheDir and replaced with a preview + read_file footer
// (renderToolResultContent); an empty toolRoot or a read_file result degrades to
// plain self-describing truncation.
func (c *conversation) appendToolResults(calls []domain.ToolCall, results []domain.ToolResult, maxResultChars int, toolRoot, cacheDir string, logger *slog.Logger) {
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
			Content:    renderToolResultContent(call.Name, content, maxResultChars, toolRoot, cacheDir, logger),
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
