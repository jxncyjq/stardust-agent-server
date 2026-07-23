package runtime

import (
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestSessionKeyForTaskPrefersSessionID(t *testing.T) {
	if got := sessionKeyForTask(domain.Task{ID: "t1", SessionID: "s1"}); got != "s1" {
		t.Errorf("sessionKeyForTask = %q, want s1", got)
	}
	if got := sessionKeyForTask(domain.Task{ID: "t1"}); got != "t1" {
		t.Errorf("sessionKeyForTask (no session) = %q, want t1", got)
	}
}

func TestMessageSnapshotRoundTrip(t *testing.T) {
	convo := newConversation("base", []string{"data:image/png;base64,AA"})
	calls := []domain.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": "a.txt"}}}
	convo.appendAssistant("thinking", calls)
	convo.appendToolResults(calls, []domain.ToolResult{{CallID: "c1", Success: true, Output: "hi"}}, 0)

	restored := restoreConversation(snapshotMessages(convo))

	if len(restored.messages) != len(convo.messages) {
		t.Fatalf("restored len = %d, want %d", len(restored.messages), len(convo.messages))
	}
	if restored.messages[1].ToolCalls[0].Name != "read_file" {
		t.Errorf("assistant turn lost its tool call: %+v", restored.messages[1])
	}
	if restored.messages[2].ToolCallID != "c1" || restored.messages[2].Content != "hi" {
		t.Errorf("tool turn = %+v, want the paired result", restored.messages[2])
	}
	if len(restored.messages[0].Images) != 1 {
		t.Errorf("first turn lost its images: %+v", restored.messages[0])
	}
}

func TestLoadedSnapshotRoundTrip(t *testing.T) {
	entries := []loadedEntry{
		{name: "read_file", detail: `{"name":"read_file"}`},
		{name: "curator", detail: "skill body text"},
	}
	restored := restoreLoaded(snapshotLoaded(entries))
	if len(restored) != len(entries) {
		t.Fatalf("restored len = %d, want %d", len(restored), len(entries))
	}
	for i := range entries {
		if restored[i].name != entries[i].name || restored[i].detail != entries[i].detail {
			t.Errorf("restored[%d] = %+v, want %+v", i, restored[i], entries[i])
		}
	}
}

func TestLoadedSnapshotRoundTripEmptyIsEmpty(t *testing.T) {
	if got := restoreLoaded(snapshotLoaded(nil)); len(got) != 0 {
		t.Fatalf("restoreLoaded(snapshotLoaded(nil)) = %#v, want empty", got)
	}
}
