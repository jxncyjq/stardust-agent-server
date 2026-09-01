package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestConversationTurnGeneratedFilesRoundTrip(t *testing.T) {
	r := openTestRepo(t)
	ctx := context.Background()
	turn := domain.ConversationTurn{
		ID: "t1:assistant", SessionID: "s1", TaskID: "t1", AgentID: "a1",
		Role: domain.ConversationRoleAssistant, Content: "hi",
		GeneratedFiles: []string{"docs/a.html", "out/b.md"},
		CreatedAt:      time.Now(),
	}
	appendTurnEvents(t, r, "s1", turn)
	turns, err := r.ListConversationTurns(ctx, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || len(turns[0].GeneratedFiles) != 2 || turns[0].GeneratedFiles[0] != "docs/a.html" || turns[0].GeneratedFiles[1] != "out/b.md" {
		t.Fatalf("got %+v", turns)
	}
}

func TestConversationTurnNoGeneratedFilesReadsEmpty(t *testing.T) {
	r := openTestRepo(t)
	ctx := context.Background()
	turn := domain.ConversationTurn{
		ID: "t2:user", SessionID: "s2", TaskID: "t2", AgentID: "a1",
		Role: domain.ConversationRoleUser, Content: "q", CreatedAt: time.Now(),
	}
	appendTurnEvents(t, r, "s2", turn)
	turns, err := r.ListConversationTurns(ctx, "s2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || len(turns[0].GeneratedFiles) != 0 {
		t.Fatalf("want empty generated files, got %+v", turns)
	}
}
