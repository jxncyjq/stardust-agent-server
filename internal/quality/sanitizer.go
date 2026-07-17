package quality

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/stardust/legion-agent/internal/port"
)

const defaultLabelMaxLen = 256
const pseudoToolCallReplacement = "当前 Agent 尚未接入对应的真实工具执行能力，不能直接确认该信息；请启用代码库搜索工具或提供相关文件上下文。"

var pseudoToolCallPattern = regexp.MustCompile(`(?m)^\s*(search_content|read_file|write_file|run_shell|execute_command|list_files|grep|glob|apply_patch)\s*\([^\r\n]*\)\s*$`)

type OutputSanitizer struct{}

func NewOutputSanitizer() OutputSanitizer {
	return OutputSanitizer{}
}

func (OutputSanitizer) Label(text string) string {
	return truncate(cleanControls(stripANSI(text), false), defaultLabelMaxLen)
}

func (s OutputSanitizer) HTML(text string) string {
	return html.EscapeString(cleanControls(stripANSI(text), false))
}

func (OutputSanitizer) YAMLString(text string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range stripANSI(text) {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case 0:
			b.WriteString(`\u0000`)
		default:
			if isDisallowedControl(r, true) {
				b.WriteString(`\u`)
				b.WriteString(strings.ToUpper(strconv.FormatInt(int64(r), 16)))
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (OutputSanitizer) MarkdownInline(text string) string {
	return cleanControls(stripANSI(text), false)
}

func (OutputSanitizer) MarkdownBlock(text string) string {
	return cleanControls(stripPseudoToolCalls(stripANSI(text)), true)
}

func SanitizeModelOutput(text string) string {
	return OutputSanitizer{}.MarkdownBlock(text)
}

func (OutputSanitizer) Truncate(text string, maxLen int) string {
	return truncate(text, maxLen)
}

func stripANSI(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == '\x1b' && i+1 < len(text) && text[i+1] == '[' {
			i += 2
			for i < len(text) {
				r2, size2 := utf8.DecodeRuneInString(text[i:])
				i += size2
				if r2 >= '@' && r2 <= '~' {
					break
				}
			}
			continue
		}
		b.WriteRune(r)
		i += size
	}
	return b.String()
}

func cleanControls(text string, keepWhitespaceControls bool) string {
	var b strings.Builder
	for _, r := range text {
		if isDisallowedControl(r, keepWhitespaceControls) {
			continue
		}
		if r == '\n' || r == '\r' || r == '\t' {
			if keepWhitespaceControls {
				b.WriteRune(r)
				continue
			}
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func stripPseudoToolCalls(text string) string {
	return strings.TrimSpace(pseudoToolCallPattern.ReplaceAllString(text, pseudoToolCallReplacement))
}

func isDisallowedControl(r rune, keepWhitespaceControls bool) bool {
	if keepWhitespaceControls && (r == '\n' || r == '\r' || r == '\t') {
		return false
	}
	return unicode.IsControl(r)
}

func truncate(text string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen]
}

var _ port.OutputSanitizer = OutputSanitizer{}
