package capability

import "strings"

// Render turns a sorted entry list into the catalog block that goes into the
// prompt's stable prefix.
//
// Entries must already be sorted (Catalog.Entries does that). Render adds no
// counts, timestamps or ids of its own: anything that varies per round would
// change the cached prefix and cost a cache miss on every inference.
//
// An empty catalog renders to the empty string rather than an empty block --
// an empty listing would tell the model it has capabilities when it has none.
func Render(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n<available_capabilities>\n")
	group := ""
	for _, entry := range entries {
		if entry.Group != group {
			group = entry.Group
			b.WriteString(group)
			b.WriteString(":\n")
		}
		b.WriteString("  - ")
		b.WriteString(entry.Name)
		b.WriteString(": ")
		b.WriteString(entry.Summary)
		b.WriteString("\n")
	}
	b.WriteString("</available_capabilities>\n")
	b.WriteString("Call load_capabilities with the names you need before using them.\n")
	return b.String()
}
