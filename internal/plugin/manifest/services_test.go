package manifest

import (
	"encoding/json"
	"strings"
	"testing"
)

// Named services let a plugin depend on a CAPABILITY ("an issue tracker")
// instead of on somebody's specific tool name. These tests pin the
// declaration's own shape; who ends up providing a service, and what happens
// when nobody does, is the dependency graph's business.

func servicePluginJSON(t *testing.T, provides, requires []string) []byte {
	t.Helper()

	pm := PluginManifest{
		Name:    "legion-jira",
		Version: "1.0.0",
		ABI:     1,
		SHA256:  strings.Repeat("a", 64),
		Limits:  Limits{TimeoutMs: 1000, MaxMemoryPages: 8, MaxInstances: 1},
		Tools: []ToolDecl{{
			Name: "jira_search", Description: "d", Group: "plugins", RiskLevel: "low", TimeoutMs: 500,
		}},
		ProvidesServices: provides,
		RequiresServices: requires,
	}
	data, err := json.Marshal(pm)
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return data
}

func TestAManifestCarriesItsServiceDeclarations(t *testing.T) {
	pm, err := ParsePlugin(servicePluginJSON(t, []string{"issue-tracker"}, []string{"calendar"}))
	if err != nil {
		t.Fatalf("ParsePlugin: %v", err)
	}
	if len(pm.ProvidesServices) != 1 || pm.ProvidesServices[0] != "issue-tracker" {
		t.Errorf("provides_services = %v, want [issue-tracker]", pm.ProvidesServices)
	}
	if len(pm.RequiresServices) != 1 || pm.RequiresServices[0] != "calendar" {
		t.Errorf("requires_services = %v, want [calendar]", pm.RequiresServices)
	}
}

// TestAManifestWithNoServicesIsUnchanged: services are contract-declared
// optional. Absent keys mean "this plugin takes no part in the service seam",
// not an unset field.
func TestAManifestWithNoServicesIsUnchanged(t *testing.T) {
	pm, err := ParsePlugin(servicePluginJSON(t, nil, nil))
	if err != nil {
		t.Fatalf("ParsePlugin: %v", err)
	}
	if len(pm.ProvidesServices) != 0 || len(pm.RequiresServices) != 0 {
		t.Errorf("services = %v / %v, want none", pm.ProvidesServices, pm.RequiresServices)
	}
}

func TestServiceDeclarationsRejectEmptyAndRepeatedNames(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provides []string
		requires []string
		want     string
	}{
		{name: "empty provides", provides: []string{""}, want: "provides_services[0] is empty"},
		{name: "empty requires", requires: []string{"  "}, want: "requires_services[0] is empty"},
		{name: "repeated provides", provides: []string{"issue-tracker", "issue-tracker"}, want: "claimed twice"},
		{name: "repeated requires", requires: []string{"calendar", "calendar"}, want: "claimed twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePlugin(servicePluginJSON(t, tc.provides, tc.requires))
			if err == nil {
				t.Fatal("ParsePlugin = nil error, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestAPluginCannotRequireAServiceItProvides mirrors the existing rule for
// tools: a self-dependency always trivially resolves while adding a self-loop
// that pollutes cycle detection.
func TestAPluginCannotRequireAServiceItProvides(t *testing.T) {
	_, err := ParsePlugin(servicePluginJSON(t, []string{"issue-tracker"}, []string{"issue-tracker"}))
	if err == nil {
		t.Fatal("ParsePlugin = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "issue-tracker") {
		t.Errorf("error = %v, want it to name the service", err)
	}
}

// TestServiceNamesAreTheirOwnNamespace: a service may be named after a tool
// this plugin contributes, and that is not a conflict — the model never sees
// service names, and nothing resolves a tool call through them (D1a's scope).
func TestServiceNamesAreTheirOwnNamespace(t *testing.T) {
	if _, err := ParsePlugin(servicePluginJSON(t, []string{"jira_search"}, nil)); err != nil {
		t.Errorf("ParsePlugin with a service named like a tool = %v, want nil: two namespaces", err)
	}
}
