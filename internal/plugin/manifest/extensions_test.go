package manifest

import (
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/tool"
)

// Extensions are the second grant dimension: capabilities gate the guest
// calling the host, extensions gate the HOST CALLING THE GUEST. These tests
// pin the rules that make the second dimension safe, and the one place its
// semantics deliberately differ from capabilities.

func TestParsePluginAcceptsDeclaredExtensions(t *testing.T) {
	pm, err := ParsePlugin(pluginWith(`"extensions": ["observe"],`))
	if err != nil {
		t.Fatalf("ParsePlugin with extensions: %v, want nil", err)
	}
	if len(pm.Extensions) != 1 || pm.Extensions[0] != "observe" {
		t.Errorf("Extensions = %v, want [observe]", pm.Extensions)
	}
}

// TestParsePluginWithoutExtensionsIsUnchanged: every plugin written before
// this field existed declares none, and must keep parsing exactly as it did.
func TestParsePluginWithoutExtensionsIsUnchanged(t *testing.T) {
	pm, err := ParsePlugin(pluginWith(""))
	if err != nil {
		t.Fatalf("ParsePlugin without extensions: %v, want nil", err)
	}
	if len(pm.Extensions) != 0 {
		t.Errorf("Extensions = %v, want none", pm.Extensions)
	}
}

// TestParsePluginRefusesAnUnknownExtension: ignoring a name nobody implements
// would let an author believe their plugin is being consulted while nothing
// ever calls it — the worst of the three possible outcomes.
func TestParsePluginRefusesAnUnknownExtension(t *testing.T) {
	_, err := ParsePlugin(pluginWith(`"extensions": ["rewrite_results"],`))
	requireErrorContains(t, err, "rewrite_results")
}

func TestParsePluginRefusesARepeatedExtension(t *testing.T) {
	_, err := ParsePlugin(pluginWith(`"extensions": ["observe","observe"],`))
	requireErrorContains(t, err, "twice")
}

// TestAssembleSpecGrantsOnlyTheIntersection is the enforcement: what the
// plugin declared AND the deployment granted.
func TestAssembleSpecGrantsOnlyTheIntersection(t *testing.T) {
	pm := extensionManifest(t, []string{"observe"})
	entry := extensionEntry([]string{"observe"})

	spec, err := AssembleSpec(pm, entry, Limits{TimeoutMs: 1000, MaxMemoryPages: 8, MaxInstances: 1})
	if err != nil {
		t.Fatalf("AssembleSpec: %v", err)
	}
	if !spec.Extensions.Observe {
		t.Error("Extensions.Observe = false, want true: both sides named it")
	}
}

// TestAssembleSpecAllowsGrantingFewerExtensionsThanDeclared is the one place
// extensions deliberately differ from capabilities, which must match exactly.
// "You may observe, but you may not decide" has to be a sentence a deployment
// can say.
func TestAssembleSpecAllowsGrantingFewerExtensionsThanDeclared(t *testing.T) {
	pm := extensionManifest(t, []string{"observe"})
	entry := extensionEntry(nil) // declares observe, grants nothing

	spec, err := AssembleSpec(pm, entry, Limits{TimeoutMs: 1000, MaxMemoryPages: 8, MaxInstances: 1})
	if err != nil {
		t.Fatalf("AssembleSpec with no granted extensions: %v, want nil — a subset is legal", err)
	}
	if spec.Extensions.Any() {
		t.Errorf("Extensions = %v, want none granted", spec.Extensions.Names())
	}
}

// TestAssembleSpecRefusesGrantingAnUndeclaredExtension: giving a plugin
// something it never asked for is a config error, exactly as it is for
// capabilities.
func TestAssembleSpecRefusesGrantingAnUndeclaredExtension(t *testing.T) {
	pm := extensionManifest(t, nil) // declares nothing
	entry := extensionEntry([]string{"observe"})

	_, err := AssembleSpec(pm, entry, Limits{TimeoutMs: 1000, MaxMemoryPages: 8, MaxInstances: 1})
	if err == nil {
		t.Fatal("AssembleSpec granting an undeclared extension = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "observe") {
		t.Errorf("error = %v, want it to name the extension", err)
	}
}

func TestAssembleSpecRefusesAnUnknownGrantedExtension(t *testing.T) {
	pm := extensionManifest(t, []string{"observe"})
	entry := extensionEntry([]string{"rewrite_results"})

	_, err := AssembleSpec(pm, entry, Limits{TimeoutMs: 1000, MaxMemoryPages: 8, MaxInstances: 1})
	requireErrorContains(t, err, "rewrite_results")
}

func TestParseExtensionsRejectsUnknownAndDuplicate(t *testing.T) {
	if _, err := perm.ParseExtensions([]string{"observe"}); err != nil {
		t.Errorf("ParseExtensions([observe]) = %v, want nil", err)
	}
	if _, err := perm.ParseExtensions([]string{"nope"}); err == nil {
		t.Error("ParseExtensions([nope]) = nil error, want a refusal")
	}
	if _, err := perm.ParseExtensions([]string{"observe", "observe"}); err == nil {
		t.Error("ParseExtensions with a duplicate = nil error, want a refusal")
	}
}

// extensionManifest builds a minimal valid manifest declaring the given
// extensions, with one tool the entry below accepts.
func extensionManifest(t *testing.T, extensions []string) PluginManifest {
	t.Helper()

	return PluginManifest{
		Name: "p", Version: "1.0.0", ABI: 1, SHA256: validSHA256,
		Limits:     Limits{TimeoutMs: 1000, MaxMemoryPages: 8, MaxInstances: 1},
		Tools:      []ToolDecl{{Name: "t", Group: "g", TimeoutMs: 1000}},
		Extensions: extensions,
	}
}

func extensionEntry(granted []string) Entry {
	return Entry{
		Name:   "p",
		Source: "p",
		Grant:  GrantDecl{Extensions: granted},
		Tools:  []ToolAccept{{Name: "t"}},
	}
}

// unusedToolImport keeps the tool import honest if the assertions above stop
// touching descriptors; AssembleSpec returns host.Spec whose Tools are
// tool.Descriptor values.
var _ = tool.Descriptor{}
