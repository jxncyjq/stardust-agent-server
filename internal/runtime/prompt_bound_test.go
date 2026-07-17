package runtime

import (
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestTruncateText(t *testing.T) {
	t.Parallel()

	if got := truncateText("short", 100); got != "short" {
		t.Fatalf("truncateText(short) = %q, want unchanged", got)
	}
	if got := truncateText(strings.Repeat("x", 50), 0); len(got) != 50 {
		t.Fatalf("truncateText with maxChars<=0 should not truncate, got len %d", len(got))
	}
	long := strings.Repeat("x", 5000)
	got := truncateText(long, 4000)
	if !strings.Contains(got, "[truncated 1000 chars]") {
		t.Fatalf("truncateText(long) missing truncation marker: %q", got[len(got)-40:])
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 4000)) {
		t.Fatal("truncateText should keep the first maxChars runes")
	}
}

func TestBoundPrompt(t *testing.T) {
	t.Parallel()

	if got := boundPrompt("small prompt", 1000); got != "small prompt" {
		t.Fatalf("boundPrompt(small) = %q, want unchanged", got)
	}
	head := strings.Repeat("H", 2000)
	tail := strings.Repeat("T", 2000)
	mid := strings.Repeat("M", 4000)
	got := boundPrompt(head+mid+tail, 3000)
	if len([]rune(got)) > 3000+64 {
		t.Fatalf("boundPrompt result len %d exceeds budget", len([]rune(got)))
	}
	if !strings.Contains(got, "older tool context trimmed") {
		t.Fatal("boundPrompt missing trim marker")
	}
	if !strings.HasPrefix(got, strings.Repeat("H", 100)) {
		t.Fatal("boundPrompt should keep the head")
	}
	if !strings.HasSuffix(got, strings.Repeat("T", 100)) {
		t.Fatal("boundPrompt should keep the most recent tail")
	}
}

func TestMergeToolResultsTruncatesLargeOutput(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("z", 10000)
	entries := mergeToolResults(nil,
		[]domain.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": "big.txt"}}},
		[]domain.ToolResult{{CallID: "c1", Success: true, Output: huge}},
		4000,
	)
	got := renderToolEntries(entries)
	if strings.Count(got, "z") > 4000 {
		t.Fatalf("tool output not truncated: %d z chars", strings.Count(got, "z"))
	}
	if !strings.Contains(got, "[truncated") {
		t.Fatal("truncated tool output missing marker")
	}
}

// TestMergeToolResultsDeduplicates asserts that reading the same file twice keeps
// only one (the latest) copy in the accumulated tool context.
func TestMergeToolResultsDeduplicates(t *testing.T) {
	t.Parallel()

	call := domain.ToolCall{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": "a.txt"}}
	entries := mergeToolResults(nil, []domain.ToolCall{call},
		[]domain.ToolResult{{CallID: "c1", Success: true, Output: "OLD CONTENT"}}, 4000)

	// Same file, later round, new content + new call id.
	call2 := domain.ToolCall{ID: "c2", Name: "read_file", Arguments: map[string]string{"path": "a.txt"}}
	entries = mergeToolResults(entries, []domain.ToolCall{call2},
		[]domain.ToolResult{{CallID: "c2", Success: true, Output: "NEW CONTENT"}}, 4000)

	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (same file dedups)", len(entries))
	}
	got := renderToolEntries(entries)
	if strings.Contains(got, "OLD CONTENT") {
		t.Fatal("stale duplicate of the same file still present")
	}
	if !strings.Contains(got, "NEW CONTENT") {
		t.Fatal("latest file content missing")
	}

	// A different file is a separate entry.
	call3 := domain.ToolCall{ID: "c3", Name: "read_file", Arguments: map[string]string{"path": "b.txt"}}
	entries = mergeToolResults(entries, []domain.ToolCall{call3},
		[]domain.ToolResult{{CallID: "c3", Success: true, Output: "B"}}, 4000)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (distinct files kept)", len(entries))
	}
}
