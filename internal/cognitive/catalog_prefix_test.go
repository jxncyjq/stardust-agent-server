package cognitive

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/domain"
)

// catalogCloseTag ends the rendered capability listing. Plugin entries are
// inserted before it, so it marks the end of the builtin portion.
const catalogCloseTag = "</available_capabilities>"

type prefixProvider struct{ entries []capability.Entry }

func (p prefixProvider) Entries(context.Context) ([]capability.Entry, error) {
	return p.entries, nil
}

func (p prefixProvider) Detail(context.Context, string) (string, error) {
	return "", capability.ErrUnknownCapability
}

func buildPrefix(t *testing.T, entries []capability.Entry) string {
	t.Helper()
	core := NewCore(NewThresholdCompressor(1 << 20)) // never compress: keep the offset
	built, err := core.BuildContext(context.Background(), Request{
		Agent:   domain.Agent{ID: "agent-1", Role: "developer"},
		Task:    domain.Task{ID: "task-1", Input: "do the thing"},
		Catalog: capability.NewCatalog(prefixProvider{entries: entries}),
	})
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	runes := []rune(built.Prompt)
	if built.StablePrefixLen > len(runes) {
		t.Fatalf("StablePrefixLen %d exceeds prompt length %d", built.StablePrefixLen, len(runes))
	}
	return string(runes[:built.StablePrefixLen])
}

func prefixBuiltins() []capability.Entry {
	return []capability.Entry{
		{Name: "read_file", Group: "files", Summary: "Read a file", Kind: capability.KindTool},
		{Name: "web_search", Group: "web", Summary: "Search the web", Kind: capability.KindTool},
	}
}

func withJiraPlugin() []capability.Entry {
	return append(append([]capability.Entry{}, prefixBuiltins()...),
		capability.Entry{Name: "jira_search", Group: "aaa-jira", Summary: "Search Jira",
			Kind: capability.KindTool, Origin: capability.OriginPlugin})
}

// Loading a plugin must not rewrite the BUILTIN portion of the cache-stable
// prefix. Under DeepSeek-style longest-common-prefix caching, everything up to
// the change point still hits.
//
// The builtin portion is everything before the catalog's closing tag: plugin
// entries are inserted inside the listing, so the tag and the loader hint that
// follow it are necessarily pushed later. Asserting on the whole prefix would
// therefore fail even when the partitioning works perfectly -- the boundary
// that matters is the last builtin entry.
// See docs/agents/bug/2026-08-16-prompt-cache-backend-mismatch.md
func TestPluginLoadOnlyAppendsToBuiltinPortionOfStablePrefix(t *testing.T) {
	before := buildPrefix(t, prefixBuiltins())
	after := buildPrefix(t, withJiraPlugin())

	end := strings.Index(before, catalogCloseTag)
	if end < 0 {
		t.Fatalf("no %q in the builtin prefix; the catalog block shape changed:\n%s", catalogCloseTag, before)
	}
	builtinPortion := before[:end]

	if strings.HasPrefix(after, builtinPortion) {
		return // every builtin byte survived -- the goal
	}
	t.Fatalf("plugin load rewrote the builtin portion: only %d of %d runes survived\nbefore:\n%s\nafter:\n%s",
		commonPrefixLen(builtinPortion, after), len([]rune(builtinPortion)), before, after)
}

// Unloading restores the original bytes, so a load/unload cycle costs at most
// two partial misses rather than permanently shifting the prefix.
func TestPluginUnloadRestoresStablePrefix(t *testing.T) {
	before := buildPrefix(t, prefixBuiltins())
	_ = buildPrefix(t, withJiraPlugin())

	if after := buildPrefix(t, prefixBuiltins()); after != before {
		t.Fatalf("unload did not restore the identical prefix")
	}
}

func commonPrefixLen(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	n := min(len(ra), len(rb))
	for i := 0; i < n; i++ {
		if ra[i] != rb[i] {
			return i
		}
	}
	return n
}
