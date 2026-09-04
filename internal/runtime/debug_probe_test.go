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

	total, totalToolCallChars, msgs := inferenceRequestDebug(req)

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
	// 这两个 call 没有 Arguments，所以参数体积是 0：条数与体积是两个不同的数，
	// 「有 2 个 call」不等于「有参数字节」。
	if totalToolCallChars != 0 {
		t.Errorf("totalToolCallChars = %d, want 0 (these calls carry no arguments)", totalToolCallChars)
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

// tool call 的参数体积必须能被量到，而且与 Content 分开报。
//
// #148 之后 session.max_turn_chars 的预算**已经在裁** ToolCalls[].Arguments
// （applyTurnBudget），而这个探针当时只数 Content——于是「被预算管住了却量不到」。
// G3 唯一必须保持可度量的东西就是请求体积，从 runtime.debug 读到的数会系统性低估它。
//
// 分开报而不是并进 total_content_chars：后者是既有日志的口径，并进去会让新旧两段日志
// 不可比，而这个探针存在的意义正是纵向比较「prompt 什么时候涨的」。
func TestTheDebugProbeMeasuresToolCallArgumentsSeparately(t *testing.T) {
	t.Parallel()

	args := strings.Repeat("参", 300)
	req := port.InferenceRequest{
		Messages: []port.InferenceMessage{
			// Content 为空、只有参数：并进 Content 的话这条消息看起来是 0 字符。
			{Role: "assistant", ToolCalls: []domain.ToolCall{
				{ID: "c1", Name: "write_file", Arguments: map[string]string{"content": args}},
			}},
			{Role: "user", Content: "hi"},
		},
	}

	total, totalToolCallChars, msgs := inferenceRequestDebug(req)

	// 参数体积 = 参数名 + 参数值的 rune 数（"content" 7 + 300）。
	if want := 7 + 300; msgs[0].ToolCallChars != want {
		t.Errorf("msgs[0].ToolCallChars = %d，要 %d：参数体积没被量到，"+
			"而预算已经在裁它了", msgs[0].ToolCallChars, want)
	}
	if totalToolCallChars != 7+300 {
		t.Errorf("totalToolCallChars = %d，要 %d", totalToolCallChars, 7+300)
	}
	// Content 的口径不能被污染：并进去会让新旧日志不可比。
	if total != 2 {
		t.Errorf("total_content_chars = %d，要 2（只有那句 \"hi\"）："+
			"参数体积被并进了 Content 的口径，新旧日志从此不可比", total)
	}
	if msgs[0].Chars != 0 {
		t.Errorf("msgs[0].Chars = %d，要 0：这条消息没有 Content", msgs[0].Chars)
	}
	// 没有 tool call 的消息该是 0，不能凭空多出来。
	if msgs[1].ToolCallChars != 0 {
		t.Errorf("msgs[1].ToolCallChars = %d，要 0", msgs[1].ToolCallChars)
	}
}
