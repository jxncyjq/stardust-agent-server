package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

type fakeSearcher struct {
	turns    []domain.ConversationTurn
	sessions []domain.AgentSession
	err      error

	lastQuery  string
	lastLimit  int
	lastAround string
	lastWindow int
}

func (f *fakeSearcher) SearchMessages(_ context.Context, query string, limit int) ([]domain.ConversationTurn, error) {
	f.lastQuery, f.lastLimit = query, limit
	return f.turns, f.err
}

func (f *fakeSearcher) ScrollMessages(_ context.Context, _ string, aroundID string, window int) ([]domain.ConversationTurn, error) {
	f.lastAround, f.lastWindow = aroundID, window
	return f.turns, f.err
}

func (f *fakeSearcher) BrowseSessions(_ context.Context, limit int) ([]domain.AgentSession, error) {
	f.lastLimit = limit
	return f.sessions, f.err
}

func decodeSessionSearch(t *testing.T, result domain.ToolResult) map[string]any {
	t.Helper()
	if !result.Success {
		t.Fatalf("session_search result unsuccessful: %q", result.Error)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(result.Output), &payload); err != nil {
		t.Fatalf("decode session_search output %q: %v", result.Output, err)
	}
	return payload
}

func TestSessionSearchDiscoveryMode(t *testing.T) {
	searcher := &fakeSearcher{turns: []domain.ConversationTurn{
		{ID: "t2", SessionID: "s1", Role: domain.ConversationRoleAssistant, Content: "prompt cache", CreatedAt: time.Unix(3, 0)},
	}}
	result, err := handleSessionSearch(context.Background(), searcher, domain.ToolCall{
		ID: "c1", Name: "session_search", Arguments: map[string]string{"query": "prompt", "limit": "7"},
	})
	if err != nil {
		t.Fatalf("handleSessionSearch(discovery) error = %v, want nil", err)
	}
	payload := decodeSessionSearch(t, result)
	if payload["mode"] != "discovery" {
		t.Fatalf("mode = %v, want discovery", payload["mode"])
	}
	if searcher.lastQuery != "prompt" || searcher.lastLimit != 7 {
		t.Fatalf("searcher got query=%q limit=%d, want prompt/7", searcher.lastQuery, searcher.lastLimit)
	}
	if turns, ok := payload["turns"].([]any); !ok || len(turns) != 1 {
		t.Fatalf("turns = %v, want one turn", payload["turns"])
	}
}

func TestSessionSearchScrollMode(t *testing.T) {
	searcher := &fakeSearcher{turns: []domain.ConversationTurn{{ID: "t1"}, {ID: "t2"}}}
	result, err := handleSessionSearch(context.Background(), searcher, domain.ToolCall{
		ID: "c1", Arguments: map[string]string{"session_id": "s1", "around_message_id": "t1", "window": "2"},
	})
	if err != nil {
		t.Fatalf("handleSessionSearch(scroll) error = %v, want nil", err)
	}
	payload := decodeSessionSearch(t, result)
	if payload["mode"] != "scroll" {
		t.Fatalf("mode = %v, want scroll", payload["mode"])
	}
	if searcher.lastAround != "t1" || searcher.lastWindow != 2 {
		t.Fatalf("searcher got around=%q window=%d, want t1/2", searcher.lastAround, searcher.lastWindow)
	}
}

func TestSessionSearchBrowseMode(t *testing.T) {
	searcher := &fakeSearcher{sessions: []domain.AgentSession{{ID: "s1", Title: "work"}}}
	result, err := handleSessionSearch(context.Background(), searcher, domain.ToolCall{ID: "c1", Arguments: map[string]string{}})
	if err != nil {
		t.Fatalf("handleSessionSearch(browse) error = %v, want nil", err)
	}
	payload := decodeSessionSearch(t, result)
	if payload["mode"] != "browse" {
		t.Fatalf("mode = %v, want browse", payload["mode"])
	}
	if sessions, ok := payload["sessions"].([]any); !ok || len(sessions) != 1 {
		t.Fatalf("sessions = %v, want one session", payload["sessions"])
	}
}

func TestSessionSearchScrollRequiresBothArgs(t *testing.T) {
	searcher := &fakeSearcher{}
	result, err := handleSessionSearch(context.Background(), searcher, domain.ToolCall{
		ID: "c1", Arguments: map[string]string{"session_id": "s1"},
	})
	if err != nil {
		t.Fatalf("handleSessionSearch(partial scroll) error = %v, want nil", err)
	}
	if result.Success {
		t.Fatalf("partial scroll result success = true, want failure")
	}
}

func TestSessionSearchPropagatesSearcherError(t *testing.T) {
	searcher := &fakeSearcher{err: errors.New("boom")}
	_, err := handleSessionSearch(context.Background(), searcher, domain.ToolCall{
		ID: "c1", Arguments: map[string]string{"query": "prompt"},
	})
	if err == nil {
		t.Fatalf("handleSessionSearch(searcher error) error = nil, want propagated error")
	}
}
