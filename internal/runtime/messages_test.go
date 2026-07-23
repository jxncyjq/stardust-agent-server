package runtime

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

func TestConversationRecordsAssistantToolCallsThenResults(t *testing.T) {
	t.Parallel()
	convo := newConversation("do the thing", nil)
	calls := []domain.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": "a.txt"}}}

	convo.appendAssistant("", calls)
	convo.appendToolResults(calls, []domain.ToolResult{{CallID: "c1", Success: true, Output: "content"}}, 0)

	msgs := convo.render(0)
	if len(msgs) != 3 {
		t.Fatalf("render() = %d messages, want user+assistant+tool", len(msgs))
	}
	if msgs[0].Role != port.RoleUser || msgs[0].Content != "do the thing" {
		t.Fatalf("first turn = %+v, want the task framing as a user message", msgs[0])
	}
	if msgs[1].Role != port.RoleAssistant || len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("second turn = %+v, want the assistant turn carrying its call", msgs[1])
	}
	if msgs[2].Role != port.RoleTool || msgs[2].ToolCallID != "c1" || msgs[2].Content != "content" {
		t.Fatalf("third turn = %+v, want the tool result paired to call c1", msgs[2])
	}
}

// The 152-read incident in one assertion: repeated identical calls must stay
// visible as separate turns. Collapsing them by (name, arguments) — the old
// behaviour — is what made every round look identical to the model.
func TestConversationKeepsRepeatedIdenticalCallsAsDistinctTurns(t *testing.T) {
	t.Parallel()
	convo := newConversation("base", nil)

	for i := range 3 {
		calls := []domain.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "read_file", Arguments: map[string]string{"path": "a.txt"}}}
		convo.appendAssistant("", calls)
		convo.appendToolResults(calls, []domain.ToolResult{{CallID: calls[0].ID, Success: true, Output: "same"}}, 0)
	}

	msgs := convo.render(0)
	if len(msgs) != 7 {
		t.Fatalf("render() = %d messages, want 1 user + 3 assistant + 3 tool", len(msgs))
	}
}

func TestConversationTruncatesOversizedToolResult(t *testing.T) {
	t.Parallel()
	convo := newConversation("base", nil)
	calls := []domain.ToolCall{{ID: "c1", Name: "read_file"}}

	convo.appendAssistant("", calls)
	convo.appendToolResults(calls, []domain.ToolResult{{CallID: "c1", Success: true, Output: strings.Repeat("x", 100)}}, 10)

	msgs := convo.render(0)
	if !strings.Contains(msgs[2].Content, "truncated") {
		t.Fatalf("tool content = %q, want a truncation marker", msgs[2].Content)
	}
}

// A failed tool must reach the model as its own tool turn: it has to see the
// failure to recover from it, and the provider requires every tool call to be
// answered.
func TestConversationRecordsFailedToolAsToolMessage(t *testing.T) {
	t.Parallel()
	convo := newConversation("base", nil)
	calls := []domain.ToolCall{{ID: "c1", Name: "read_file"}}

	convo.appendAssistant("", calls)
	convo.appendToolResults(calls, []domain.ToolResult{{CallID: "c1", Success: false, Error: "no such file"}}, 0)

	msgs := convo.render(0)
	if msgs[2].Role != port.RoleTool || !strings.Contains(msgs[2].Content, "no such file") {
		t.Fatalf("tool turn = %+v, want the failure reported to the model", msgs[2])
	}
}

func TestConversationCarriesImagesOnTheFirstUserTurn(t *testing.T) {
	t.Parallel()
	convo := newConversation("look", []string{"data:image/png;base64,AAAA"})

	msgs := convo.render(0)
	if len(msgs[0].Images) != 1 {
		t.Fatalf("first turn images = %v, want the task's images", msgs[0].Images)
	}
}
