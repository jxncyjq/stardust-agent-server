package manifest

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// validSHA256 is a syntactically valid (64 lowercase hex digit) content
// digest used by every test that does not care about the sha256 check
// itself.
const validSHA256 = "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4"

func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// requireErrorContains asserts err is non-nil and its message names want,
// failing loudly (with the actual error, or "nil", in the message) when it
// does not. Every ParsePlugin/ParseDeployment error-path test in this file
// goes through this helper so a happy-path implementation that returns nil
// cannot pass as "the error names the field".
func requireErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

// --- ParsePlugin ---------------------------------------------------------

func TestParsePlugin_ValidFixture(t *testing.T) {
	data := mustReadFixture(t, "plugin.json")

	pm, err := ParsePlugin(data)
	if err != nil {
		t.Fatalf("ParsePlugin: unexpected error: %v", err)
	}

	if pm.Name != "legion-jira" {
		t.Errorf("Name = %q, want %q", pm.Name, "legion-jira")
	}
	if pm.Version != "1.2.0" {
		t.Errorf("Version = %q, want %q", pm.Version, "1.2.0")
	}
	if pm.ABI != 1 {
		t.Errorf("ABI = %d, want 1", pm.ABI)
	}
	if pm.SHA256 != validSHA256 {
		t.Errorf("SHA256 = %q, want %q", pm.SHA256, validSHA256)
	}
	wantCaps := []string{"log", "http", "kv"}
	if !equalStrings(pm.Capabilities, wantCaps) {
		t.Errorf("Capabilities = %v, want %v", pm.Capabilities, wantCaps)
	}
	if pm.Limits.TimeoutMs != 5000 {
		t.Errorf("Limits.TimeoutMs = %d, want 5000", pm.Limits.TimeoutMs)
	}
	if pm.Limits.MaxMemoryPages != 64 {
		t.Errorf("Limits.MaxMemoryPages = %d, want 64", pm.Limits.MaxMemoryPages)
	}
	if pm.Limits.MaxInstances != 4 {
		t.Errorf("Limits.MaxInstances = %d, want 4", pm.Limits.MaxInstances)
	}
	wantHosts := []string{"jira.example.com"}
	if !equalStrings(pm.Network.AllowedHosts, wantHosts) {
		t.Errorf("Network.AllowedHosts = %v, want %v", pm.Network.AllowedHosts, wantHosts)
	}
	wantPaths := []string{"/data/jira"}
	if !equalStrings(pm.Filesystem.AllowedPaths, wantPaths) {
		t.Errorf("Filesystem.AllowedPaths = %v, want %v", pm.Filesystem.AllowedPaths, wantPaths)
	}
	if len(pm.Tools) != 2 {
		t.Fatalf("len(Tools) = %d, want 2", len(pm.Tools))
	}

	search := pm.Tools[0]
	if search.Name != "jira_search" {
		t.Errorf("Tools[0].Name = %q, want %q", search.Name, "jira_search")
	}
	if search.Description != "Search Jira issues" {
		t.Errorf("Tools[0].Description = %q, want %q", search.Description, "Search Jira issues")
	}
	if search.Group != "issue-tracking" {
		t.Errorf("Tools[0].Group = %q, want %q", search.Group, "issue-tracking")
	}
	if search.RiskLevel != "low" {
		t.Errorf("Tools[0].RiskLevel = %q, want %q", search.RiskLevel, "low")
	}
	if search.TimeoutMs != 3000 {
		t.Errorf("Tools[0].TimeoutMs = %d, want 3000", search.TimeoutMs)
	}
	if search.Sensitive != false {
		t.Errorf("Tools[0].Sensitive = %v, want false", search.Sensitive)
	}
	if got, ok := search.InputSchema["type"]; !ok || got != "object" {
		t.Errorf("Tools[0].InputSchema[type] = %v (present=%v), want \"object\"", got, ok)
	}

	create := pm.Tools[1]
	if create.Name != "jira_create_issue" {
		t.Errorf("Tools[1].Name = %q, want %q", create.Name, "jira_create_issue")
	}
	if create.Sensitive != true {
		t.Errorf("Tools[1].Sensitive = %v, want true", create.Sensitive)
	}
	if create.TimeoutMs != 5000 {
		t.Errorf("Tools[1].TimeoutMs = %d, want 5000", create.TimeoutMs)
	}
}

func TestParsePlugin_MissingName(t *testing.T) {
	data := []byte(`{
		"version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "t", "group": "g", "timeout_ms": 1000}]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "name")
}

func TestParsePlugin_MissingVersion(t *testing.T) {
	data := []byte(`{
		"name": "p", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "t", "group": "g", "timeout_ms": 1000}]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "version")
}

func TestParsePlugin_ABINotOne(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 2, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "t", "group": "g", "timeout_ms": 1000}]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "abi")
}

func TestParsePlugin_SHA256NotHex64(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "not-a-digest",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "t", "group": "g", "timeout_ms": 1000}]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "sha256")
}

func TestParsePlugin_SHA256WrongLength(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "a1b2c3",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "t", "group": "g", "timeout_ms": 1000}]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "sha256")
}

func TestParsePlugin_EmptyTools(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": []
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "tools")
}

func TestParsePlugin_ToolMissingGroup(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "my_tool", "timeout_ms": 1000}]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "my_tool")
	requireErrorContains(t, err, "group")
}

func TestParsePlugin_ToolTimeoutZero(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "my_tool", "group": "g", "timeout_ms": 0}]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "my_tool")
	requireErrorContains(t, err, "timeout_ms")
}

func TestParsePlugin_ToolTimeoutNegative(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "my_tool", "group": "g", "timeout_ms": -5}]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "my_tool")
	requireErrorContains(t, err, "timeout_ms")
}

func TestParsePlugin_DuplicateToolName(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [
			{"name": "jira_search", "group": "g", "timeout_ms": 1000},
			{"name": "jira_search", "group": "g", "timeout_ms": 1000}
		]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "jira_search")
	requireErrorContains(t, err, "twice")
}

func TestParsePlugin_UnknownCapability(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"capabilities": ["log", "networking"],
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "t", "group": "g", "timeout_ms": 1000}]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "networking")
}

func TestParsePlugin_MaxMemoryPagesZero(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 0, "max_instances": 1},
		"tools": [{"name": "t", "group": "g", "timeout_ms": 1000}]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "max_memory_pages")
}

func TestParsePlugin_MaxInstancesZero(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 0},
		"tools": [{"name": "t", "group": "g", "timeout_ms": 1000}]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "max_instances")
}

func TestParsePlugin_MaxInstancesNegative(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": -1},
		"tools": [{"name": "t", "group": "g", "timeout_ms": 1000}]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "max_instances")
}

// TestParsePlugin_UnknownFieldRejected is the test the Step 3 mutation
// verification targets: removing DisallowUnknownFields() from ParsePlugin
// must make this test fail.
func TestParsePlugin_UnknownFieldRejected(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "t", "group": "g", "timeout_ms": 1000}],
		"nmae": "typo of name"
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "nmae")
}

func TestParsePlugin_RequiresAbsentDefaultsEmpty(t *testing.T) {
	data := mustReadFixture(t, "plugin.json")
	pm, err := ParsePlugin(data)
	if err != nil {
		t.Fatalf("ParsePlugin: unexpected error: %v", err)
	}
	if len(pm.Requires) != 0 {
		t.Errorf("Requires = %v, want empty (fixture has no requires key)", pm.Requires)
	}
}

func TestParsePlugin_RequiresValid(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "my_tool", "group": "g", "timeout_ms": 1000}],
		"requires": ["other_plugin_tool", "yet_another_tool"]
	}`)
	pm, err := ParsePlugin(data)
	if err != nil {
		t.Fatalf("ParsePlugin: unexpected error: %v", err)
	}
	want := []string{"other_plugin_tool", "yet_another_tool"}
	if !equalStrings(pm.Requires, want) {
		t.Errorf("Requires = %v, want %v", pm.Requires, want)
	}
}

func TestParsePlugin_RequiresEmptyString(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "my_tool", "group": "g", "timeout_ms": 1000}],
		"requires": ["other_tool", ""]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "requires")
}

func TestParsePlugin_RequiresDuplicate(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "my_tool", "group": "g", "timeout_ms": 1000}],
		"requires": ["other_tool", "other_tool"]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "other_tool")
	requireErrorContains(t, err, "twice")
}

func TestParsePlugin_RequiresSelfDependency(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "my_tool", "group": "g", "timeout_ms": 1000}],
		"requires": ["my_tool"]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "my_tool")
	requireErrorContains(t, err, "itself")
}

// --- ParseDeployment ------------------------------------------------------

func TestParseDeployment_ValidFixture(t *testing.T) {
	data := mustReadFixture(t, "plugins.json")

	dep, err := ParseDeployment(data)
	if err != nil {
		t.Fatalf("ParseDeployment: unexpected error: %v", err)
	}
	if len(dep.Plugins) != 2 {
		t.Fatalf("len(Plugins) = %d, want 2", len(dep.Plugins))
	}

	jira := dep.Plugins[0]
	if jira.Name != "legion-jira" {
		t.Errorf("Plugins[0].Name = %q, want %q", jira.Name, "legion-jira")
	}
	if jira.Source != "./plugins/legion-jira" {
		t.Errorf("Plugins[0].Source = %q, want %q", jira.Source, "./plugins/legion-jira")
	}
	if jira.Enabled != true {
		t.Errorf("Plugins[0].Enabled = %v, want true (explicit)", jira.Enabled)
	}
	wantCaps := []string{"log", "http"}
	if !equalStrings(jira.Grant.Capabilities, wantCaps) {
		t.Errorf("Plugins[0].Grant.Capabilities = %v, want %v", jira.Grant.Capabilities, wantCaps)
	}
	wantHosts := []string{"jira.example.com"}
	if !equalStrings(jira.Grant.AllowedHosts, wantHosts) {
		t.Errorf("Plugins[0].Grant.AllowedHosts = %v, want %v", jira.Grant.AllowedHosts, wantHosts)
	}
	if len(jira.Tools) != 1 || jira.Tools[0].Name != "jira_search" {
		t.Errorf("Plugins[0].Tools = %+v, want one entry named jira_search", jira.Tools)
	}
	if jira.Tools[0].RiskLevel != "low" {
		t.Errorf("Plugins[0].Tools[0].RiskLevel = %q, want %q", jira.Tools[0].RiskLevel, "low")
	}
	if jira.Tools[0].Sensitive == nil || *jira.Tools[0].Sensitive != false {
		t.Errorf("Plugins[0].Tools[0].Sensitive = %v, want pointer to false", jira.Tools[0].Sensitive)
	}
	var cfg map[string]string
	if err := json.Unmarshal(jira.Config, &cfg); err != nil {
		t.Fatalf("unmarshal Plugins[0].Config: %v", err)
	}
	if cfg["project"] != "LEGION" {
		t.Errorf("Plugins[0].Config[project] = %q, want %q", cfg["project"], "LEGION")
	}

	notify := dep.Plugins[1]
	if notify.Name != "legion-notify" {
		t.Errorf("Plugins[1].Name = %q, want %q", notify.Name, "legion-notify")
	}
	if notify.Enabled != true {
		t.Errorf("Plugins[1].Enabled = %v, want true (defaulted, field absent from fixture)", notify.Enabled)
	}
}

func TestParseDeployment_EnabledExplicitFalse(t *testing.T) {
	data := []byte(`{"plugins": [{"name": "p", "source": "./p", "enabled": false}]}`)
	dep, err := ParseDeployment(data)
	if err != nil {
		t.Fatalf("ParseDeployment: unexpected error: %v", err)
	}
	if len(dep.Plugins) != 1 {
		t.Fatalf("len(Plugins) = %d, want 1", len(dep.Plugins))
	}
	if dep.Plugins[0].Enabled != false {
		t.Errorf("Enabled = %v, want false (explicit)", dep.Plugins[0].Enabled)
	}
}

func TestParseDeployment_MissingName(t *testing.T) {
	data := []byte(`{"plugins": [{"source": "./p"}]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "name")
}

func TestParseDeployment_MissingSource(t *testing.T) {
	data := []byte(`{"plugins": [{"name": "my-plugin"}]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "my-plugin")
	requireErrorContains(t, err, "source")
}

func TestParseDeployment_DuplicateName(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "dup", "source": "./a"},
		{"name": "dup", "source": "./b"}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "dup")
}

func TestParseDeployment_UnknownCapabilityInGrant(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "p", "source": "./p", "grant": {"capabilities": ["log", "bogus"]}}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "bogus")
}

// TestParseDeployment_UnknownFieldRejected is the second target of the Step
// 3 mutation verification: removing DisallowUnknownFields() from
// ParseDeployment must make this test fail.
func TestParseDeployment_UnknownFieldRejected(t *testing.T) {
	data := []byte(`{"plugins": [{"name": "p", "source": "./p", "enbaled": true}]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "enbaled")
}

func TestParseDeployment_UnknownFieldInEntryToolsRejected(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "p", "source": "./p", "tools": [{"name": "t", "riks_level": "low"}]}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "riks_level")
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
