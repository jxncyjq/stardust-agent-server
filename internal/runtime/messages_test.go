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

// An OpenAI-compatible provider rejects a tool message whose assistant
// tool_call is missing, so the budget may rewrite content but never drop turns.
func TestRenderFoldsOldestToolOutputWhenOverBudget(t *testing.T) {
	t.Parallel()
	convo := newConversation("base", nil)
	for i := range 5 {
		calls := []domain.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "read_file"}}
		convo.appendAssistant("", calls)
		convo.appendToolResults(calls, []domain.ToolResult{{CallID: calls[0].ID, Success: true, Output: strings.Repeat("x", 1000)}}, 0)
	}

	msgs := convo.render(2000)

	if len(msgs) != 11 {
		t.Fatalf("render() = %d messages, want the turn structure preserved (11)", len(msgs))
	}
	if !strings.Contains(msgs[2].Content, "trimmed") {
		t.Fatalf("oldest tool turn = %q, want it folded first", msgs[2].Content)
	}
	if last := msgs[len(msgs)-1].Content; last != strings.Repeat("x", 1000) {
		t.Fatalf("newest tool turn was folded (%d chars), want it intact", len([]rune(last)))
	}
	if total := totalChars(msgs); total > 2000 {
		t.Fatalf("render() total = %d chars, want <= 2000", total)
	}
}

// The task framing is pinned: trimming it would delete the instructions the run
// is judged against, which is exactly how the previous single-budget bounding
// used to lose the head of the prompt.
func TestRenderNeverFoldsTheFirstUserTurn(t *testing.T) {
	t.Parallel()
	convo := newConversation(strings.Repeat("b", 3000), nil)

	msgs := convo.render(1000)

	if msgs[0].Content != strings.Repeat("b", 3000) {
		t.Fatalf("first turn was folded (%d chars), want the task framing intact", len([]rune(msgs[0].Content)))
	}
}

func TestRenderLeavesExchangeAloneWithinBudget(t *testing.T) {
	t.Parallel()
	convo := newConversation("base", nil)
	calls := []domain.ToolCall{{ID: "c1", Name: "read_file"}}
	convo.appendAssistant("", calls)
	convo.appendToolResults(calls, []domain.ToolResult{{CallID: "c1", Success: true, Output: "small"}}, 0)

	msgs := convo.render(10000)

	if msgs[2].Content != "small" {
		t.Fatalf("tool turn = %q, want it untouched within budget", msgs[2].Content)
	}
}

func TestRepeatedCallStreakCountsIdenticalConsecutiveRounds(t *testing.T) {
	t.Parallel()
	convo := newConversation("base", nil)
	for i := range 3 {
		calls := []domain.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "read_file", Arguments: map[string]string{"path": "a.txt"}}}
		convo.appendAssistant("", calls)
		convo.appendToolResults(calls, []domain.ToolResult{{CallID: calls[0].ID, Success: true, Output: "same"}}, 0)
	}

	pending := []domain.ToolCall{{ID: "c3", Name: "read_file", Arguments: map[string]string{"path": "a.txt"}}}
	if got := repeatedCallStreak(convo.messages, pending); got != 4 {
		t.Fatalf("repeatedCallStreak = %d, want 4 (3 recorded rounds + the pending one)", got)
	}
}

// Call IDs are fresh every round, so the streak must compare name+arguments.
func TestRepeatedCallStreakResetsOnDifferentArguments(t *testing.T) {
	t.Parallel()
	convo := newConversation("base", nil)
	for _, path := range []string{"a.txt", "a.txt", "b.txt"} {
		calls := []domain.ToolCall{{ID: "c-" + path, Name: "read_file", Arguments: map[string]string{"path": path}}}
		convo.appendAssistant("", calls)
		convo.appendToolResults(calls, []domain.ToolResult{{CallID: calls[0].ID, Success: true, Output: "x"}}, 0)
	}

	pending := []domain.ToolCall{{ID: "c-next", Name: "read_file", Arguments: map[string]string{"path": "c.txt"}}}
	if got := repeatedCallStreak(convo.messages, pending); got != 1 {
		t.Fatalf("repeatedCallStreak = %d, want 1 for a call the model has not just made", got)
	}
}

func TestRepeatedCallStreakIsZeroWithoutPendingCalls(t *testing.T) {
	t.Parallel()
	convo := newConversation("base", nil)

	if got := repeatedCallStreak(convo.messages, nil); got != 0 {
		t.Fatalf("repeatedCallStreak(no pending calls) = %d, want 0", got)
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
