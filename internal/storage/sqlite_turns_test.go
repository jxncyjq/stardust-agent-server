package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestConversationTurnTokenRoundTrip(t *testing.T) {
	r := openTestRepo(t)
	ctx := context.Background()
	turn := domain.ConversationTurn{
		ID: "t1:assistant", SessionID: "s1", TaskID: "t1", AgentID: "a1",
		Role: domain.ConversationRoleAssistant, Content: "hi",
		PromptTokens: 1200, CompletionTokens: 340, CachedTokens: 800, TotalTokens: 1540,
		CreatedAt: time.Now(),
	}
	if err := r.AppendConversationTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	turns, err := r.ListConversationTurns(ctx, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	if turns[0].PromptTokens != 1200 || turns[0].CompletionTokens != 340 || turns[0].CachedTokens != 800 || turns[0].TotalTokens != 1540 {
		t.Fatalf("token mismatch: %+v", turns[0])
	}
}

func TestConversationTurnIfAbsentUserZeroTokens(t *testing.T) {
	r := openTestRepo(t)
	ctx := context.Background()
	turn := domain.ConversationTurn{
		ID: "t2:user", SessionID: "s2", TaskID: "t2", AgentID: "a1",
		Role: domain.ConversationRoleUser, Content: "q",
		CreatedAt: time.Now(),
	}
	if _, err := r.AppendConversationTurnIfAbsent(ctx, turn); err != nil {
		t.Fatal(err)
	}
	turns, err := r.ListConversationTurns(ctx, "s2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].TotalTokens != 0 || turns[0].PromptTokens != 0 {
		t.Fatalf("want zero-token user turn, got %+v", turns)
	}
}
