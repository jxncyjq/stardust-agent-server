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

// TestParsePlugin_ToolNameWhitespaceOnly asserts a whitespace-only tool name
// is rejected the same as an outright empty string: strings.TrimSpace(tool.Name)
// == "" catches it, and the error still names the offending index.
func TestParsePlugin_ToolNameWhitespaceOnly(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "   ", "group": "g", "timeout_ms": 1000}]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "tools[0]")
	requireErrorContains(t, err, "no name")
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
	requireErrorContains(t, err, "requires[1]")
}

// TestParsePlugin_RequiresWhitespaceOnly asserts a whitespace-only entry is
// rejected the same as an outright empty string: strings.TrimSpace(r) == ""
// catches it, and the error still names the offending index.
func TestParsePlugin_RequiresWhitespaceOnly(t *testing.T) {
	data := []byte(`{
		"name": "p", "version": "1.0.0", "abi": 1, "sha256": "` + validSHA256 + `",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "my_tool", "group": "g", "timeout_ms": 1000}],
		"requires": ["other_tool", "   "]
	}`)
	_, err := ParsePlugin(data)
	requireErrorContains(t, err, "requires[1]")
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

// --- Remote sources (Entry.Digest, IsRemote, IsInsecureSource, RemoteURL) --

// validDigest is a syntactically valid "sha256:" + 64 lowercase hex digit
// digest, used by every remote-source test that does not care about the
// digest shape check itself.
const validDigest = "sha256:" + validSHA256

// TestParseDeployment_RemoteLocalMixed_Positive is the Step 1 positive case:
// one https remote entry, one http remote entry, and one local entry in the
// same deployment, each parsed correctly with IsRemote/IsInsecureSource
// asserted field by field.
func TestParseDeployment_RemoteLocalMixed_Positive(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "remote-https", "source": "https://pkgs.example.com/legion-jira.tar.gz", "digest": "` + validDigest + `"},
		{"name": "remote-http", "source": "http://localhost:8080/legion-notify.tar.gz", "digest": "` + validDigest + `"},
		{"name": "local", "source": "./plugins/legion-echo"}
	]}`)
	dep, err := ParseDeployment(data)
	if err != nil {
		t.Fatalf("ParseDeployment: unexpected error: %v", err)
	}
	if len(dep.Plugins) != 3 {
		t.Fatalf("len(Plugins) = %d, want 3", len(dep.Plugins))
	}

	https := dep.Plugins[0]
	if https.Digest != validDigest {
		t.Errorf("https.Digest = %q, want %q", https.Digest, validDigest)
	}
	if !https.IsRemote() {
		t.Errorf("https.IsRemote() = false, want true")
	}
	if https.IsInsecureSource() {
		t.Errorf("https.IsInsecureSource() = true, want false")
	}
	u, err := https.RemoteURL()
	if err != nil {
		t.Fatalf("https.RemoteURL(): unexpected error: %v", err)
	}
	if u.Scheme != "https" || u.Host != "pkgs.example.com" {
		t.Errorf("https.RemoteURL() = %+v, want scheme https, host pkgs.example.com", u)
	}

	http_ := dep.Plugins[1]
	if http_.Digest != validDigest {
		t.Errorf("http.Digest = %q, want %q", http_.Digest, validDigest)
	}
	if !http_.IsRemote() {
		t.Errorf("http.IsRemote() = false, want true")
	}
	if !http_.IsInsecureSource() {
		t.Errorf("http.IsInsecureSource() = false, want true")
	}
	u2, err := http_.RemoteURL()
	if err != nil {
		t.Fatalf("http.RemoteURL(): unexpected error: %v", err)
	}
	if u2.Scheme != "http" || u2.Host != "localhost:8080" {
		t.Errorf("http.RemoteURL() = %+v, want scheme http, host localhost:8080", u2)
	}

	local := dep.Plugins[2]
	if local.Digest != "" {
		t.Errorf("local.Digest = %q, want empty", local.Digest)
	}
	if local.IsRemote() {
		t.Errorf("local.IsRemote() = true, want false")
	}
	if local.IsInsecureSource() {
		t.Errorf("local.IsInsecureSource() = true, want false")
	}
	if _, err := local.RemoteURL(); err == nil {
		t.Errorf("local.RemoteURL(): want error (not a remote entry), got nil")
	}
}

// Rule 1: a remote entry must have a digest, for both https and http alike.

func TestParseDeployment_RemoteHTTPSMissingDigest(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "p", "source": "https://pkgs.example.com/p.tar.gz"}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "p")
	requireErrorContains(t, err, "digest")
}

func TestParseDeployment_RemoteHTTPMissingDigest(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "p", "source": "http://pkgs.example.com/p.tar.gz"}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "p")
	requireErrorContains(t, err, "digest")
}

// Rule 2: a local entry must NOT have a digest.

func TestParseDeployment_LocalEntryWithDigestRejected(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "p", "source": "./plugins/p", "digest": "` + validDigest + `"}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "p")
	requireErrorContains(t, err, "digest")
}

// Rule 3: digest must be "sha256:" + 64 hex digits, and only sha256.

func TestParseDeployment_RemoteDigestMissingAlgoPrefix(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "p", "source": "https://pkgs.example.com/p.tar.gz", "digest": "` + validSHA256 + `"}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, validSHA256)
}

func TestParseDeployment_RemoteDigestWrongAlgo(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "p", "source": "https://pkgs.example.com/p.tar.gz", "digest": "sha1:` + validSHA256 + `"}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "sha1:"+validSHA256)
}

func TestParseDeployment_RemoteDigestWrongLength(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "p", "source": "https://pkgs.example.com/p.tar.gz", "digest": "sha256:a1b2c3"}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "sha256:a1b2c3")
}

func TestParseDeployment_RemoteDigestNotHex(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "p", "source": "https://pkgs.example.com/p.tar.gz", "digest": "sha256:` + strings.Repeat("z", 64) + `"}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "sha256:"+strings.Repeat("z", 64))
}

// Rule 4: IsRemote is true for both https and http; IsInsecureSource only
// for http. Covered field-by-field in TestParseDeployment_RemoteLocalMixed_Positive
// above; this test isolates the boundary case of a local Source that merely
// contains the substrings "http"/"https" without being a URL, to confirm
// prefix matching (not substring matching) is what decides it.
func TestParseDeployment_LocalSourceContainingHTTPSubstring(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "p", "source": "./plugins/my-http-plugin"}
	]}`)
	dep, err := ParseDeployment(data)
	if err != nil {
		t.Fatalf("ParseDeployment: unexpected error: %v", err)
	}
	if dep.Plugins[0].IsRemote() {
		t.Errorf("IsRemote() = true for %q, want false (only a URL prefix counts)", dep.Plugins[0].Source)
	}
}

// Rule 5: URL parse failure, or userinfo in the URL, is an error — for both
// schemes.

func TestParseDeployment_RemoteHTTPSUnparsableURL(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "p", "source": "https://[::1", "digest": "` + validDigest + `"}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "p")
}

func TestParseDeployment_RemoteHTTPSUserinfoRejected(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "p", "source": "https://user:pass@pkgs.example.com/p.tar.gz", "digest": "` + validDigest + `"}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "p")
	requireErrorContains(t, err, "userinfo")
}

func TestParseDeployment_RemoteHTTPUserinfoRejected(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "p", "source": "http://user:pass@pkgs.example.com/p.tar.gz", "digest": "` + validDigest + `"}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "p")
	requireErrorContains(t, err, "userinfo")
}

// Rule 6: a scheme other than http/https is refused by name, not silently
// treated as a local path.

func TestParseDeployment_ForeignScheme(t *testing.T) {
	tests := []struct {
		name   string
		source string
		scheme string
	}{
		{"file", "file:///etc/passwd", "file"},
		{"ftp", "ftp://pkgs.example.com/p.tar.gz", "ftp"},
		{"ssh", "ssh://git@example.com/p.git", "ssh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(`{"plugins": [
				{"name": "p", "source": "` + tc.source + `", "digest": "` + validDigest + `"}
			]}`)
			_, err := ParseDeployment(data)
			requireErrorContains(t, err, tc.scheme)
			requireErrorContains(t, err, "p")
		})
	}
}

// Rule 7: existing local-path rules (absolute, "..") must not be loosened
// by the new remote-source classification — an absolute or "../"-escaping
// Source must still be classified as local (IsRemote() false, no digest
// required/allowed), so it is still caught downstream by
// internal/plugin/loader's packageDir, which this task does not touch.

func TestParseDeployment_LocalPathRulesUnaffected(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{"absolute", "/etc/passwd"},
		{"parent-escape", "../../etc/passwd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(`{"plugins": [{"name": "p", "source": "` + tc.source + `"}]}`)
			dep, err := ParseDeployment(data)
			if err != nil {
				t.Fatalf("ParseDeployment: unexpected error: %v (manifest layer does not itself reject "+
					"absolute/escaping local paths; that is loader.packageDir's job)", err)
			}
			if dep.Plugins[0].IsRemote() {
				t.Errorf("IsRemote() = true for local path %q, want false", tc.source)
			}
		})
	}
}

// TestParseDeployment_RemoteRequiresDigest_MutationTarget and
// TestParseDeployment_ForeignSchemeRejected_MutationTarget are the two
// tests Step 3's mutation verification targets directly (in addition to
// the rule-1 and rule-6 tests above, which already cover the same ground):
// making digest optional for remote entries must fail the former, and
// treating a foreign scheme as a local path must fail the latter.
func TestParseDeployment_RemoteRequiresDigest_MutationTarget(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "mutation-target", "source": "https://pkgs.example.com/p.tar.gz"}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "mutation-target")
	requireErrorContains(t, err, "digest")
}

func TestParseDeployment_ForeignSchemeRejected_MutationTarget(t *testing.T) {
	data := []byte(`{"plugins": [
		{"name": "mutation-target", "source": "file:///etc/passwd", "digest": "` + validDigest + `"}
	]}`)
	_, err := ParseDeployment(data)
	requireErrorContains(t, err, "file")
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
