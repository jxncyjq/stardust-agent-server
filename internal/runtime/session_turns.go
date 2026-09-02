package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// defaultRecentTurns and defaultMaxTurnChars mirror the CLI session defaults
// (normalizeRecentTurns / normalizeMaxTurnChars): a configured 0 means "unset",
// not "inject nothing" or "never truncate".
const (
	defaultRecentTurns  = 6
	defaultMaxTurnChars = 6000
)

// transcriptMessagesPerTurn converts the history budget from the unit the
// text-block path budgets in (DefaultRecentTurns conversation turns) into the
// unit the transcript path needs (messages).
//
// One conversation turn becomes one user message, or — for an assistant turn —
// one assistant message plus one tool message per call it made. The factor is a
// deliberate over-estimate: G3 exists so the model can see those tool
// round-trips, and a budget that clipped them away would defeat it. It is
// nonetheless a cap, because reading a whole session back would let one long
// conversation grow every request without bound, and G3's cost (spec §3) is
// request size — the one thing it must keep measurable.
const transcriptMessagesPerTurn = 8

// SessionHistory is one task's session history in whichever of the two shapes
// G3 selects. Exactly one of the fields is ever populated (both are empty when
// there is no history to inject), which is why they are returned together
// rather than as two independent loads: a caller cannot end up injecting both
// and sending the same history twice.
type SessionHistory struct {
	// Turns is the G3-off shape: the turns the cognitive Core renders into the
	// prompt's "Recent conversation:" block.
	Turns []domain.ConversationTurn
	// Transcript is the G3-on shape: provider messages appended after
	// message[0], carrying the history's tool round-trips.
	Transcript []port.InferenceMessage
}

// SessionHistoryForTask loads task's session history in the shape
// sessionCfg.ToolTranscriptEnabled selects, and is the ONE place that selection
// is made. Every runner that serves a session-bound task calls it —
// AgentRuntimeResolver (per-agent runtimes) and the CLI's defaultTaskRunner
// (every task whose AgentID is absent from the agent registry, the path the
// GUI's default agent takes). Wiring the switch into only one of them would
// leave that path silently on the old shape; that is how the GUI once ended up
// with no cross-turn memory at all.
//
// Switch off (the default, spec §3: G3 "不该在做轨迹的顺路上悄悄打开") is the
// existing path unchanged, byte for byte: RecentTurnsForTask, and the history
// stays inside the prompt text.
//
// A nil lister or a task with no SessionID is a legitimate "no session history
// configured" state and yields an empty SessionHistory. A lister error is
// returned wrapped: failing to read history must not be rendered as "there is
// none".
func SessionHistoryForTask(ctx context.Context, lister ConversationTurnLister, sessionCfg config.SessionConfig, task domain.Task) (SessionHistory, error) {
	if !sessionCfg.ToolTranscriptEnabled {
		turns, err := RecentTurnsForTask(ctx, lister, sessionCfg, task)
		if err != nil {
			return SessionHistory{}, err
		}
		return SessionHistory{Turns: turns}, nil
	}
	if lister == nil || strings.TrimSpace(task.SessionID) == "" {
		return SessionHistory{}, nil
	}
	limit := sessionCfg.DefaultRecentTurns
	if limit <= 0 {
		limit = defaultRecentTurns
	}
	// The task's own user turn needs no filtering here, the way RecentTurnsForTask
	// drops it by TaskID: this call happens BEFORE the task runs, and both writers
	// of a user/message event record it later — RunTask's own recorder (opened
	// further down RunTask) and the TUI controller's RecordExchange (which writes
	// the question together with the answer, after the exchange). The projected
	// messages carry no task_id anyway, so filtering here is not available; if a
	// third writer ever records a task's question up front, this is where the
	// duplicate would show up.
	msgs, err := lister.ListConversationTranscript(ctx, task.SessionID, limit*transcriptMessagesPerTurn)
	if err != nil {
		return SessionHistory{}, fmt.Errorf("list conversation transcript for session %q: %w", task.SessionID, err)
	}
	return SessionHistory{Transcript: msgs}, nil
}

// RecentTurnsForTask loads the conversation turns to inject as cross-turn
// memory for task, so a follow-up question can see what the session already
// said. It is the G3-OFF half of SessionHistoryForTask, which is what every
// runner serving a session-bound task calls — AgentRuntimeResolver (per-agent
// runtimes) and the CLI's defaultTaskRunner (every task whose AgentID is absent
// from the agent registry, the path the GUI's default agent takes). Wiring
// history into only one of them is what left the GUI with no cross-turn memory
// at all, so new runners must go through SessionHistoryForTask rather than
// re-deriving the rules below.
//
// It asks for one more turn than the limit and then drops the task's own user
// turn: the HTTP layer persists that turn (as "<taskID>:user") before the task
// is enqueued, so it would otherwise be replayed alongside task.Input.
//
// A nil lister or a task with no SessionID is a legitimate "no session history
// configured" state and yields (nil, nil). A lister error is returned wrapped:
// failing to read history must not be silently rendered as "there is none".
func RecentTurnsForTask(ctx context.Context, lister ConversationTurnLister, sessionCfg config.SessionConfig, task domain.Task) ([]domain.ConversationTurn, error) {
	if lister == nil || strings.TrimSpace(task.SessionID) == "" {
		return nil, nil
	}
	limit := sessionCfg.DefaultRecentTurns
	if limit <= 0 {
		limit = defaultRecentTurns
	}
	// truncateText treats maxChars <= 0 as "no truncation", so an explicitly
	// zero MaxTurnChars would inject unbounded turn bodies here while the CLI
	// path capped them at the default.
	maxTurnChars := sessionCfg.MaxTurnChars
	if maxTurnChars <= 0 {
		maxTurnChars = defaultMaxTurnChars
	}
	// +1: the task's own user turn is already persisted and will be filtered out.
	turns, err := lister.ListConversationTurns(ctx, task.SessionID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("list conversation turns for session %q: %w", task.SessionID, err)
	}
	out := make([]domain.ConversationTurn, 0, len(turns))
	for _, turn := range turns {
		if turn.TaskID == task.ID {
			continue
		}
		turn.Content = truncateText(turn.Content, maxTurnChars)
		out = append(out, turn)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}
