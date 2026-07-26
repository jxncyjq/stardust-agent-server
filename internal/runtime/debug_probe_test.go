package runtime

import (
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// TestInferenceRequestDebugBreakdown verifies the debug probe reports, per
// message, the rune count, role, tool-call/image counts and a truncated
// single-line preview, plus the total content size across the request — the
// breakdown that localizes which message carries the bulk of the prompt.
func TestInferenceRequestDebugBreakdown(t *testing.T) {
	big := strings.Repeat("你", 500) // 500 runes, multibyte
	req := port.InferenceRequest{
		Messages: []port.InferenceMessage{
			{Role: "system", Content: big + "\nline"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "c1"}, {ID: "c2"}}},
		},
		Tools: []port.InferenceTool{{Name: "a"}, {Name: "b"}},
	}

	total, msgs := inferenceRequestDebug(req)

	if len(msgs) != 3 {
		t.Fatalf("msgs len = %d, want 3", len(msgs))
	}
	// Rune count, not byte count: the multibyte content must not be over-counted.
	if msgs[0].Chars != 505 {
		t.Errorf("msgs[0].Chars = %d, want 505 (500 runes + \\n + 4)", msgs[0].Chars)
	}
	if total != 505+2+0 {
		t.Errorf("total = %d, want %d", total, 505+2)
	}
	if msgs[0].Role != "system" {
		t.Errorf("msgs[0].Role = %q, want system", msgs[0].Role)
	}
	if msgs[2].ToolCalls != 2 {
		t.Errorf("msgs[2].ToolCalls = %d, want 2", msgs[2].ToolCalls)
	}
	// Preview is single-line (newline replaced) and truncated with a remainder marker.
	if strings.Contains(msgs[0].Preview, "\n") {
		t.Errorf("preview must be single-line, got %q", msgs[0].Preview)
	}
	if !strings.Contains(msgs[0].Preview, "(+") {
		t.Errorf("preview of an over-length message must carry a truncation marker, got %q", msgs[0].Preview)
	}
	// A short message is shown whole with no marker.
	if msgs[1].Preview != "hi" {
		t.Errorf("msgs[1].Preview = %q, want %q", msgs[1].Preview, "hi")
	}
}
