package capability_test

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/capability"
)

// partitionProvider serves a fixed entry list.
type partitionProvider struct{ entries []capability.Entry }

func (p partitionProvider) Entries(context.Context) ([]capability.Entry, error) {
	return p.entries, nil
}

func (p partitionProvider) Detail(context.Context, string) (string, error) {
	return "", capability.ErrUnknownCapability
}

func builtinEntries() []capability.Entry {
	return []capability.Entry{
		{Name: "read_file", Group: "files", Summary: "Read a file", Kind: capability.KindTool},
		{Name: "write_file", Group: "files", Summary: "Write a file", Kind: capability.KindTool},
		{Name: "web_search", Group: "web", Summary: "Search the web", Kind: capability.KindTool},
	}
}

// A plugin group whose name sorts BEFORE every builtin group, to prove the
// partition key outranks the group name.
func pluginEntries() []capability.Entry {
	return []capability.Entry{
		{Name: "jira_search", Group: "aaa-jira", Summary: "Search Jira", Kind: capability.KindTool,
			Origin: capability.OriginPlugin},
	}
}

func names(entries []capability.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

// Origin outranks Group: a plugin group named "aaa-jira" still sorts after
// every builtin group, so plugin entries never land in the middle of the
// cached prefix.
func TestPluginEntriesSortAfterBuiltinRegardlessOfGroupName(t *testing.T) {
	t.Parallel()
	catalog := capability.NewCatalog(
		partitionProvider{entries: pluginEntries()},
		partitionProvider{entries: builtinEntries()},
	)
	got, err := catalog.Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	want := []string{"read_file", "write_file", "web_search", "jira_search"}
	gotNames := names(got)
	if len(gotNames) != len(want) {
		t.Fatalf("want %v, got %v", want, gotNames)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Fatalf("want %v, got %v", want, gotNames)
		}
	}
}

// The zero value is builtin, so existing providers keep their behavior with no
// code change.
func TestZeroOriginIsBuiltin(t *testing.T) {
	t.Parallel()
	var e capability.Entry
	if e.Origin != capability.OriginBuiltin {
		t.Fatalf("zero Origin must be OriginBuiltin, got %v", e.Origin)
	}
}

// Within one partition the existing (group, name) ordering is unchanged.
func TestOrderingWithinPartitionUnchanged(t *testing.T) {
	t.Parallel()
	catalog := capability.NewCatalog(partitionProvider{entries: []capability.Entry{
		{Name: "write_file", Group: "files", Summary: "Write a file", Kind: capability.KindTool},
		{Name: "web_search", Group: "web", Summary: "Search the web", Kind: capability.KindTool},
		{Name: "read_file", Group: "files", Summary: "Read a file", Kind: capability.KindTool},
	}})
	got, err := catalog.Entries(context.Background())
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	want := []string{"read_file", "write_file", "web_search"}
	for i, name := range names(got) {
		if name != want[i] {
			t.Fatalf("want %v, got %v", want, names(got))
		}
	}
}

func TestOriginString(t *testing.T) {
	t.Parallel()
	if capability.OriginBuiltin.String() != "builtin" {
		t.Fatalf("got %q", capability.OriginBuiltin.String())
	}
	if capability.OriginPlugin.String() != "plugin" {
		t.Fatalf("got %q", capability.OriginPlugin.String())
	}
}
