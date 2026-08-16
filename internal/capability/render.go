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
//
// A group heading is emitted at every group change AND at every origin
// boundary, so a plugin group sharing a builtin group's name gets its own
// heading instead of its entries reading as builtin capabilities.
func Render(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n<available_capabilities>\n")
	group := ""
	origin := OriginBuiltin
	first := true
	for _, entry := range entries {
		// A heading is emitted whenever either key changes. Origin matters even
		// when the group name repeats: a plugin group sharing a builtin group's
		// name is a different section, and merging them would present a plugin
		// capability as a builtin one. The `first` flag is load-bearing -- with
		// origin in the comparison the zero value alone no longer guarantees a
		// heading for the opening entry.
		if first || entry.Group != group || entry.Origin != origin {
			group = entry.Group
			origin = entry.Origin
			b.WriteString(group)
			b.WriteString(":\n")
		}
		first = false
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
