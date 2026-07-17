package quality

import (
	"strings"
	"testing"
)

func TestOutputSanitizerCleansLabel(t *testing.T) {
	t.Parallel()
	sanitizer := NewOutputSanitizer()

	got := sanitizer.Label("hello\x00\x1b[31m world")
	if strings.ContainsAny(got, "\x00\x1b") {
		t.Fatalf("Label() = %q, want no control characters", got)
	}
	long := strings.Repeat("a", 300)
	if got := sanitizer.Label(long); len(got) != 256 {
		t.Fatalf("Label(long) len = %d, want 256", len(got))
	}
}

func TestOutputSanitizerEscapesHTML(t *testing.T) {
	t.Parallel()
	sanitizer := NewOutputSanitizer()

	got := sanitizer.HTML(`<script>alert("x")</script>`)
	want := `&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;`
	if got != want {
		t.Fatalf("HTML(script) = %q, want %q", got, want)
	}
}

func TestOutputSanitizerEscapesYAMLString(t *testing.T) {
	t.Parallel()
	sanitizer := NewOutputSanitizer()

	got := sanitizer.YAMLString("line1\nline2\t\"quote\"\\path\x00")
	want := `"line1\nline2\t\"quote\"\\path\u0000"`
	if got != want {
		t.Fatalf("YAMLString() = %q, want %q", got, want)
	}
}

func TestOutputSanitizerCleansMarkdownInline(t *testing.T) {
	t.Parallel()
	sanitizer := NewOutputSanitizer()

	got := sanitizer.MarkdownInline("safe \x1b[31mred\x1b[0m text\nnext")
	if strings.Contains(got, "\x1b") {
		t.Fatalf("MarkdownInline() = %q, want no ANSI escape", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("MarkdownInline() = %q, want no newline", got)
	}
}

func TestOutputSanitizerReplacesPseudoToolCallsInModelOutput(t *testing.T) {
	t.Parallel()
	sanitizer := NewOutputSanitizer()

	got := sanitizer.MarkdownBlock("我先搜索一下。\nsearch_content({\"pattern\":\"cache\"})\n然后回答。")
	if strings.Contains(got, "search_content") {
		t.Fatalf("MarkdownBlock(pseudo tool) = %q, want pseudo tool call removed", got)
	}
	for _, want := range []string{"我先搜索一下。", "当前 Agent 尚未接入对应的真实工具执行能力", "然后回答。"} {
		if !strings.Contains(got, want) {
			t.Fatalf("MarkdownBlock(pseudo tool) missing %q:\n%s", want, got)
		}
	}
}

func TestOutputSanitizerTruncatesWithNonPositiveLimit(t *testing.T) {
	t.Parallel()
	sanitizer := NewOutputSanitizer()

	if got := sanitizer.Truncate("abc", 0); got != "" {
		t.Fatalf("Truncate(maxLen=0) = %q, want empty", got)
	}
	if got := sanitizer.Truncate("abcdef", 3); got != "abc" {
		t.Fatalf("Truncate(maxLen=3) = %q, want %q", got, "abc")
	}
}
