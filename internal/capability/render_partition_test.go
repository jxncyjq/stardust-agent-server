package capability_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
)

func partitionedEntries() []capability.Entry {
	// Already in (origin, group, name) order, as Catalog.Entries returns them.
	return []capability.Entry{
		{Name: "read_file", Group: "files", Summary: "Read a file", Kind: capability.KindTool},
		{Name: "web_search", Group: "web", Summary: "Search the web", Kind: capability.KindTool},
		{Name: "jira_search", Group: "web", Summary: "Search Jira", Kind: capability.KindTool,
			Origin: capability.OriginPlugin},
	}
}

// A plugin group with the same name as a builtin group must get its own
// heading. Without it the plugin entry reads as a builtin capability.
func TestRenderRepeatsHeadingAcrossOriginBoundary(t *testing.T) {
	t.Parallel()
	got := capability.Render(partitionedEntries())
	if n := strings.Count(got, "web:\n"); n != 2 {
		t.Fatalf("want the shared group heading twice (once per origin), got %d:\n%s", n, got)
	}
	// Order still holds: the plugin entry is last.
	if idx := strings.Index(got, "jira_search"); idx < strings.Index(got, "web_search") {
		t.Fatalf("plugin entry must render after builtin entries:\n%s", got)
	}
}

// THE core invariant: adding or removing plugin entries must not change one
// byte of the builtin portion of the render. This is what keeps the cached
// prefix hitting on every round.
func TestBuiltinPortionIsByteIdenticalAcrossPluginChanges(t *testing.T) {
	t.Parallel()
	builtinOnly := []capability.Entry{
		{Name: "read_file", Group: "files", Summary: "Read a file", Kind: capability.KindTool},
		{Name: "web_search", Group: "web", Summary: "Search the web", Kind: capability.KindTool},
	}
	withPlugins := append(append([]capability.Entry{}, builtinOnly...),
		// Deliberately early-sorting group names; the origin key must keep them last.
		capability.Entry{Name: "jira_search", Group: "aaa-jira", Summary: "Search Jira",
			Kind: capability.KindTool, Origin: capability.OriginPlugin},
		capability.Entry{Name: "gitlab_mr", Group: "aaa-gitlab", Summary: "List MRs",
			Kind: capability.KindTool, Origin: capability.OriginPlugin},
	)

	base := capability.Render(builtinOnly)
	// The builtin prefix ends where the closing tag begins in the plugin-free
	// render; everything before that must survive verbatim.
	head := base[:strings.Index(base, "</available_capabilities>")]

	full := capability.Render(sortForTest(withPlugins))
	if !strings.HasPrefix(full, head) {
		t.Fatalf("builtin portion changed when plugins were added.\nwant prefix:\n%q\ngot:\n%q", head, full)
	}

	// And removing them restores the original bytes exactly.
	if again := capability.Render(builtinOnly); again != base {
		t.Fatalf("unloading plugins must restore the identical render")
	}
}

// sortForTest mirrors Catalog.Entries' ordering so this test exercises Render
// against the input shape it actually receives.
func sortForTest(entries []capability.Entry) []capability.Entry {
	out := append([]capability.Entry{}, entries...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Origin != out[j].Origin {
			return out[i].Origin < out[j].Origin
		}
		if out[i].Group == out[j].Group {
			return out[i].Name < out[j].Name
		}
		return out[i].Group < out[j].Group
	})
	return out
}
