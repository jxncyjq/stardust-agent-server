package runtime

import (
	"strings"
	"testing"
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
	if !strings.Contains(got, "硬截断") || !strings.Contains(got, "HARD-TRUNCATED") {
		t.Fatalf("truncateText(long) missing hard-truncation marker: %q", got[len(got)-80:])
	}
	if !strings.Contains(got, "4000") || !strings.Contains(got, "5000") {
		t.Fatalf("truncateText(long) missing shown/total rune counts: %q", got[len(got)-200:])
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 4000)) {
		t.Fatal("truncateText should keep the first maxChars runes")
	}
}
