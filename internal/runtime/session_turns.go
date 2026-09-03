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
// sessionCfg.ToolTranscriptEnabled selects. It is the ONE place that selection
// is made for a runner that has a domain.Task, and there are exactly two of
// those: AgentRuntimeResolver (per-agent runtimes) and the CLI's
// defaultTaskRunner (every task whose AgentID is absent from the agent registry,
// the path the GUI's default agent takes).
//
// There is a THIRD production path that reads session history and cannot call
// this function: `legion tui` loads it from a session id, before any
// domain.Task exists (tuiSessionController.SessionHistory, which makes the same
// selection and shares SessionTranscript for the transcript half). It was the
// path this switch originally missed — the switch worked under serve and did
// nothing under tui, with no error anywhere.
//
// That shape — the seam is here but a caller silently stays on the old path —
// is how the GUI once ended up with no cross-turn memory at all, so every one
// of the three call sites is pinned by an assertion on what the model actually
// received: TestDefaultTaskRunnerSendsHistoryAsATranscript (internal/cli),
// TestTheResolverSendsHistoryAsATranscript (this package) and
// TestTheTUIPathSendsHistoryAsATranscript (internal/cli). Deleting any one
// assignment turns exactly one of them red.
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
	// The task's own user turn needs no filtering here, the way RecentTurnsForTask
	// drops it by TaskID: this call happens BEFORE the task runs, and both writers
	// of a user/message event record it later — RunTask's own recorder (opened
	// further down RunTask) and the TUI controller's RecordExchange (which writes
	// the question together with the answer, after the exchange). The projected
	// messages carry no task_id anyway, so filtering here is not available; if a
	// third writer ever records a task's question up front, this is where the
	// duplicate would show up.
	msgs, err := SessionTranscript(ctx, lister, sessionCfg, task.SessionID)
	if err != nil {
		return SessionHistory{}, err
	}
	return SessionHistory{Transcript: msgs}, nil
}

// SessionTranscript loads sessionID's history in G3's transcript shape and
// applies BOTH budgets the session config declares, so the switch changes the
// history's SHAPE and not the limits a deployment configured.
//
// It exists as its own exported function because the transcript branch has two
// callers that cannot share SessionHistoryForTask: the serve-side runners have a
// domain.Task, the TUI's session controller has only a session id (see
// tuiSessionController.SessionHistory). Duplicating the budget arithmetic in the
// TUI is how one path silently ends up unbounded.
//
//   - Session.DefaultRecentTurns caps how MANY messages come back, converted into
//     the transcript's unit by transcriptMessagesPerTurn.
//   - Session.MaxTurnChars caps how LONG each one is. Without this the two paths
//     disagree by two orders of magnitude: RecentTurnsForTask truncates every turn
//     at MaxTurnChars (6000 by default), while the projection passes user/assistant
//     content through verbatim and those events are documented to store the FULL
//     text, bounded only by P1's 64 KiB per event. A configured
//     session.max_turn_chars would then silently do nothing the moment G3 is
//     turned on — and G3's whole cost (spec §3) is request size, "the one thing it
//     must keep measurable". Tool messages are already bounded on the write side
//     (maxEventPreviewRunes = 2000), so in practice this only bites user/assistant
//     bodies; it is applied uniformly anyway rather than by role, because "which
//     roles happen to be pre-truncated today" is not something this budget should
//     depend on.
//
// A nil lister or an empty sessionID is the legitimate "no session history
// configured" state and yields (nil, nil). A lister error is wrapped and
// returned: failing to read history must never be rendered as "there is none".
// 守卫：TestTheTranscriptPathHonoursMaxTurnChars。
func SessionTranscript(ctx context.Context, lister ConversationTurnLister, sessionCfg config.SessionConfig, sessionID string) ([]port.InferenceMessage, error) {
	if lister == nil || strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	limit := sessionCfg.DefaultRecentTurns
	if limit <= 0 {
		limit = defaultRecentTurns
	}
	// truncateText treats maxChars <= 0 as "no truncation", so an explicitly zero
	// MaxTurnChars would inject unbounded bodies here — the same normalization
	// RecentTurnsForTask does, for the same reason.
	maxTurnChars := sessionCfg.MaxTurnChars
	if maxTurnChars <= 0 {
		maxTurnChars = defaultMaxTurnChars
	}
	msgs, err := lister.ListConversationTranscript(ctx, sessionID, limit*transcriptMessagesPerTurn)
	if err != nil {
		return nil, fmt.Errorf("list conversation transcript for session %q: %w", sessionID, err)
	}
	for i := range msgs {
		msgs[i].Content = truncateText(msgs[i].Content, maxTurnChars)
	}
	return msgs, nil
}

// RecentTurnsForTask loads the conversation turns to inject as cross-turn
// memory for task, so a follow-up question can see what the session already
// said. It is the G3-OFF half of SessionHistoryForTask, which is what the two
// runners holding a domain.Task call — AgentRuntimeResolver (per-agent
// runtimes) and the CLI's defaultTaskRunner (every task whose AgentID is absent
// from the agent registry, the path the GUI's default agent takes). `legion tui`
// is the third history-reading path and goes through
// tuiSessionController.SessionHistory instead, because it has only a session id;
// see SessionHistoryForTask for the full list and what pins each one.
//
// Wiring history into only one of these paths is what left the GUI with no
// cross-turn memory at all, so a new runner must go through SessionHistoryForTask
// (or, without a task, SessionTranscript) rather than re-deriving the rules below.
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
