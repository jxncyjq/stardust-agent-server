package manifest

import (
	"reflect"
	"testing"
)

// draftManifest returns a minimal, already-valid PluginManifest with two
// declared tools in a fixed order, for use by every DraftEntry test that
// does not care about a specific field. Callers that need to mutate a
// field (e.g. an empty Tools list) copy this and override.
func draftManifest() PluginManifest {
	return PluginManifest{
		Name:    "legion-jira",
		Version: "1.2.0",
		ABI:     1,
		SHA256:  validSHA256,
		Tools: []ToolDecl{
			{Name: "jira_search", Group: "jira", TimeoutMs: 5000, RiskLevel: "low", Sensitive: false},
			{Name: "jira_create_issue", Group: "jira", TimeoutMs: 5000, RiskLevel: "high", Sensitive: true},
		},
	}
}

const draftDigest = "sha256:a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4"

// --- Rule 1: Enabled is always false --------------------------------------

func TestDraftEntry_EnabledAlwaysFalse(t *testing.T) {
	entry, err := DraftEntry(draftManifest(), "https://example.com/plugin.wasm", draftDigest)
	if err != nil {
		t.Fatalf("DraftEntry: unexpected error: %v", err)
	}
	if entry.Enabled != false {
		t.Errorf("Enabled = %v, want false; install must never authorize a plugin to run", entry.Enabled)
	}
}

// --- Rule 2: Grant.Capabilities is always empty ---------------------------

func TestDraftEntry_GrantCapabilitiesAlwaysEmpty(t *testing.T) {
	pm := draftManifest()
	pm.Capabilities = []string{"log", "http"}

	entry, err := DraftEntry(pm, "https://example.com/plugin.wasm", draftDigest)
	if err != nil {
		t.Fatalf("DraftEntry: unexpected error: %v", err)
	}
	if len(entry.Grant.Capabilities) != 0 {
		t.Errorf("Grant.Capabilities = %v, want empty; a draft must not authorize any capability",
			entry.Grant.Capabilities)
	}
}

// --- Rule 3: Tools maps pm.Tools -> ToolAccept{Name} in order, no overrides ---

func TestDraftEntry_ToolsMapInOrderNoOverrides(t *testing.T) {
	pm := draftManifest()

	entry, err := DraftEntry(pm, "https://example.com/plugin.wasm", draftDigest)
	if err != nil {
		t.Fatalf("DraftEntry: unexpected error: %v", err)
	}

	want := []ToolAccept{
		{Name: "jira_search"},
		{Name: "jira_create_issue"},
	}
	if !reflect.DeepEqual(entry.Tools, want) {
		t.Errorf("Tools = %+v, want %+v (same order as pm.Tools, no RiskLevel/Sensitive overrides)",
			entry.Tools, want)
	}
}

// --- Rule 4: Name comes from pm.Name, not the caller ----------------------

func TestDraftEntry_NameFromManifestNotCaller(t *testing.T) {
	pm := draftManifest()

	entry, err := DraftEntry(pm, "https://example.com/plugin.wasm", draftDigest)
	if err != nil {
		t.Fatalf("DraftEntry: unexpected error: %v", err)
	}
	if entry.Name != pm.Name {
		t.Errorf("Name = %q, want %q (pm.Name, not any caller-supplied value)", entry.Name, pm.Name)
	}
}

// --- Rule 5: empty pm.Tools -> error naming the plugin ---------------------

func TestDraftEntry_EmptyToolsErrors(t *testing.T) {
	pm := draftManifest()
	pm.Tools = nil

	_, err := DraftEntry(pm, "https://example.com/plugin.wasm", draftDigest)
	requireErrorContains(t, err, pm.Name)
}

// --- Positive: full field-by-field assertion of the draft's content -------

func TestDraftEntry_PopulatesAllFields(t *testing.T) {
	pm := draftManifest()
	const source = "https://example.com/plugin.wasm"

	entry, err := DraftEntry(pm, source, draftDigest)
	if err != nil {
		t.Fatalf("DraftEntry: unexpected error: %v", err)
	}

	want := Entry{
		Name:    "legion-jira",
		Source:  source,
		Digest:  draftDigest,
		Enabled: false,
		Grant:   GrantDecl{Capabilities: []string{}},
		Tools: []ToolAccept{
			{Name: "jira_search"},
			{Name: "jira_create_issue"},
		},
	}
	if !reflect.DeepEqual(entry, want) {
		t.Errorf("DraftEntry(%+v, %q, %q) = %+v, want %+v", pm, source, draftDigest, entry, want)
	}
}
