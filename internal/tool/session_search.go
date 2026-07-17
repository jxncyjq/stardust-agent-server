package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// MessageSearcher is the storage capability backing the session_search tool. It
// is satisfied by *storage.SQLiteRepository. The tool depends on this narrow
// interface rather than the concrete repository so it stays testable and
// storage-agnostic.
type MessageSearcher interface {
	// SearchMessages runs a full-text query over conversation content (discovery).
	SearchMessages(ctx context.Context, query string, limit int) ([]domain.ConversationTurn, error)
	// ScrollMessages returns a window of turns around an anchor turn (scroll).
	ScrollMessages(ctx context.Context, sessionID string, aroundID string, window int) ([]domain.ConversationTurn, error)
	// BrowseSessions returns the most recently updated sessions (browse).
	BrowseSessions(ctx context.Context, limit int) ([]domain.AgentSession, error)
}

// RegisterSessionSearchTool registers the session_search tool on registry when
// searcher is available. It is a no-op when registry or searcher is nil.
func RegisterSessionSearchTool(registry *Registry, searcher MessageSearcher) {
	if registry == nil || searcher == nil {
		return
	}
	registry.RegisterDescriptor(sessionSearchDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return handleSessionSearch(ctx, searcher, call)
	}))
}

func sessionSearchDescriptor() Descriptor {
	return Descriptor{
		Name: "session_search",
		Description: "Search past conversation history instead of stacking it into context. " +
			"Three modes inferred from arguments: (1) discovery — pass query to full-text search prior turns; " +
			"(2) scroll — pass session_id and around_message_id to read the turns surrounding a specific message; " +
			"(3) browse — pass no query to list the most recent sessions. Optional limit/window bound the result size.",
		RiskLevel: "low",
		Timeout:   5 * time.Second,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":             map[string]any{"type": "string", "description": "Full-text query for discovery mode. When set, discovery mode is used."},
				"session_id":        map[string]any{"type": "string", "description": "Session to scroll within (scroll mode, with around_message_id)."},
				"around_message_id": map[string]any{"type": "string", "description": "Message id to center the scroll window on (scroll mode)."},
				"limit":             map[string]any{"type": "string", "description": "Max results for discovery/browse. Defaults to a sensible cap."},
				"window":            map[string]any{"type": "string", "description": "Turns to include before and after the anchor in scroll mode."},
			},
		},
	}
}

// handleSessionSearch dispatches to discovery, scroll, or browse based on which
// arguments are present. Mode selection is deterministic: a query means
// discovery; a session_id plus around_message_id means scroll; anything else
// means browse.
func handleSessionSearch(ctx context.Context, searcher MessageSearcher, call domain.ToolCall) (domain.ToolResult, error) {
	query := strings.TrimSpace(call.Arguments["query"])
	sessionID := strings.TrimSpace(call.Arguments["session_id"])
	aroundID := strings.TrimSpace(call.Arguments["around_message_id"])

	switch {
	case query != "":
		turns, err := searcher.SearchMessages(ctx, query, sessionSearchInt(call.Arguments["limit"]))
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("session_search discovery: %w", err)
		}
		return sessionSearchJSON(call.ID, map[string]any{"mode": "discovery", "query": query, "turns": turnsView(turns)})
	case sessionID != "" && aroundID != "":
		turns, err := searcher.ScrollMessages(ctx, sessionID, aroundID, sessionSearchInt(call.Arguments["window"]))
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("session_search scroll: %w", err)
		}
		return sessionSearchJSON(call.ID, map[string]any{"mode": "scroll", "session_id": sessionID, "around_message_id": aroundID, "turns": turnsView(turns)})
	case sessionID != "" || aroundID != "":
		return domain.ToolResult{CallID: call.ID, Success: false, Error: "scroll mode requires both session_id and around_message_id"}, nil
	default:
		sessions, err := searcher.BrowseSessions(ctx, sessionSearchInt(call.Arguments["limit"]))
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("session_search browse: %w", err)
		}
		return sessionSearchJSON(call.ID, map[string]any{"mode": "browse", "sessions": sessionsView(sessions)})
	}
}

// sessionSearchInt parses an optional positive integer argument, returning 0
// (meaning "use the storage default") for empty or invalid input.
func sessionSearchInt(raw string) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func turnsView(turns []domain.ConversationTurn) []map[string]any {
	view := make([]map[string]any, 0, len(turns))
	for _, turn := range turns {
		view = append(view, map[string]any{
			"id":         turn.ID,
			"session_id": turn.SessionID,
			"task_id":    turn.TaskID,
			"role":       string(turn.Role),
			"content":    turn.Content,
			"created_at": turn.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return view
}

func sessionsView(sessions []domain.AgentSession) []map[string]any {
	view := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		view = append(view, map[string]any{
			"id":         session.ID,
			"agent_id":   session.AgentID,
			"title":      session.Title,
			"updated_at": session.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	return view
}

func sessionSearchJSON(callID string, payload map[string]any) (domain.ToolResult, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("encode session_search result: %w", err)
	}
	return domain.ToolResult{CallID: callID, Success: true, Output: string(encoded)}, nil
}
