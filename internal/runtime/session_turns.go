package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
)

// defaultRecentTurns and defaultMaxTurnChars mirror the CLI session defaults
// (normalizeRecentTurns / normalizeMaxTurnChars): a configured 0 means "unset",
// not "inject nothing" or "never truncate".
const (
	defaultRecentTurns  = 6
	defaultMaxTurnChars = 6000
)

// RecentTurnsForTask loads the conversation turns to inject as cross-turn
// memory for task, so a follow-up question can see what the session already
// said. It is shared by every runner that serves a session-bound task —
// AgentRuntimeResolver (per-agent runtimes) and the CLI's defaultTaskRunner
// (every task whose AgentID is absent from the agent registry, which is the
// path the GUI's default-agent takes). Wiring it in only one of them is what
// left the GUI with no cross-turn memory at all, so new runners must call this
// rather than re-deriving the rules below.
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
