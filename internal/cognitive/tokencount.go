package cognitive

import "unicode"

// CJKTokenCounter is a heuristic TokenCounter that approximates BPE token counts
// far better than whitespace splitting on CJK and code. ASCII runs are counted
// at ~4 chars/token (BPE average); each CJK ideograph counts as one token (a
// safe upper-ish bound so compression triggers rather than silently under-counts).
type CJKTokenCounter struct{}

// NewCJKTokenCounter returns a ready-to-use CJK-aware counter.
func NewCJKTokenCounter() *CJKTokenCounter { return &CJKTokenCounter{} }

// Count returns the estimated token length of text.
func (c *CJKTokenCounter) Count(text string) int {
	tokens := 0
	asciiRun := 0
	flush := func() {
		if asciiRun > 0 {
			tokens += (asciiRun + 3) / 4 // ceil(asciiRun/4)
			asciiRun = 0
		}
	}
	for _, r := range text {
		switch {
		case isCJK(r):
			flush()
			tokens++
		case unicode.IsSpace(r):
			flush()
		default:
			asciiRun++
		}
	}
	flush()
	return tokens
}

// isCJK reports whether r is a CJK ideograph or common CJK symbol range that a
// BPE tokenizer typically splits per-character.
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
