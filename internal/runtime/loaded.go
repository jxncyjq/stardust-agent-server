package runtime

import (
	"fmt"
	"strings"
)

// loadedEntry is one capability whose full definition the model asked for.
type loadedEntry struct {
	name   string
	detail string
}

// appendLoaded adds one capability's detail to the loaded block, evicting the
// least recently loaded entries when the block would exceed maxChars.
//
// It returns the new block, the names it evicted, and an error only when the
// detail cannot fit on its own. Truncating instead would hand the model an
// invalid JSON schema or a half a skill -- both worse than a refusal it can
// see and react to.
func appendLoaded(entries []loadedEntry, name, detail string, maxChars int) ([]loadedEntry, []string, error) {
	// Same size accounting as the eviction loop below (loadedSize: name+detail).
	// A weaker check here (e.g. detail alone) can pass while name+detail still
	// exceeds maxChars; once eviction reduces kept to this sole entry, the loop
	// stops on len(kept)>1 without ever re-checking the budget, so a mismatched
	// check here would let a block that is known to exceed maxChars come back
	// with a nil error -- fail-loud forbids returning a look-normal value like
	// that to mask the invariant violation.
	if size := len([]rune(name)) + len([]rune(detail)); maxChars > 0 && size > maxChars {
		return entries, nil, fmt.Errorf("capability %q is too large to load: %d chars, limit %d", name, size, maxChars)
	}
	kept := make([]loadedEntry, 0, len(entries)+1)
	for _, e := range entries {
		if e.name != name {
			kept = append(kept, e)
		}
	}
	kept = append(kept, loadedEntry{name: name, detail: detail})

	evicted := make([]string, 0)
	for maxChars > 0 && loadedSize(kept) > maxChars && len(kept) > 1 {
		evicted = append(evicted, kept[0].name)
		kept = kept[1:]
	}
	return kept, evicted, nil
}

func loadedSize(entries []loadedEntry) int {
	total := 0
	for _, e := range entries {
		total += len([]rune(e.detail)) + len([]rune(e.name))
	}
	return total
}

// renderLoaded renders the loaded block. It is pinned: composePrompt never
// trims it, so a definition the model was given stays visible until it is
// explicitly evicted.
func renderLoaded(entries []loadedEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nLoaded capabilities:\n")
	for _, e := range entries {
		b.WriteString("- ")
		b.WriteString(e.name)
		b.WriteString(":\n")
		b.WriteString(e.detail)
		b.WriteString("\n")
	}
	return b.String()
}

// renderEvictionNotice tells the model what was dropped to make room. Silence
// here would leave it calling a capability whose definition disappeared.
func renderEvictionNotice(evicted []string) string {
	if len(evicted) == 0 {
		return ""
	}
	return fmt.Sprintf("[unloaded to free space: %s — call load_capabilities again if you still need them]\n", strings.Join(evicted, ", "))
}

// composePrompt assembles the round's prompt in three parts with separate
// budgets: the task framing and the loaded block are never trimmed, only the
// accumulated tool output is.
//
// The previous single-budget version handed base+tools to boundPrompt, which
// drops the middle -- so the task framing's tail and every early tool result
// were the first things to go.
func composePrompt(basePrompt string, loaded []loadedEntry, toolCtx []toolEntry, maxPromptChars int) string {
	loadedBlock := renderLoaded(loaded)
	if maxPromptChars <= 0 {
		return basePrompt + loadedBlock + renderToolEntries(toolCtx)
	}
	budget := maxPromptChars - len([]rune(basePrompt)) - len([]rune(loadedBlock))
	if floor := maxPromptChars / 4; budget < floor {
		budget = floor
	}
	return basePrompt + loadedBlock + boundPrompt(renderToolEntries(toolCtx), budget)
}
