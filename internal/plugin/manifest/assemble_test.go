package manifest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- fixtures --------------------------------------------------------------

// basePluginManifest returns a PluginManifest shaped like the legion-jira
// example used throughout this package's fixtures, deliberately declaring
// only capabilities that basePluginEntry's Grant also grants (log, http) so
// that using both together is a genuine happy path. Individual tests copy
// this and mutate the one field they care about, rather than repeating the
// whole shape.
func basePluginManifest() PluginManifest {
	return PluginManifest{
		Name:    "legion-jira",
		Version: "1.2.0",
		ABI:     1,
		SHA256:  validSHA256,
		Capabilities: []string{
			"log",
			"http",
		},
		Limits: Limits{
			TimeoutMs:      5000,
			MaxMemoryPages: 64,
			MaxInstances:   4,
		},
		Network: Network{
			AllowedHosts: []string{"jira.example.com", "extra.example.com"},
		},
		Filesystem: Filesystem{
			AllowedPaths: []string{"/data/jira", "/data/extra"},
		},
		Tools: []ToolDecl{
			{
				Name:        "jira_search",
				Description: "Search Jira issues",
				Group:       "issue-tracking",
				RiskLevel:   "low",
				InputSchema: map[string]any{"type": "object"},
				TimeoutMs:   3000,
				Sensitive:   false,
			},
			{
				Name:        "jira_create_issue",
				Description: "Create a Jira issue",
				Group:       "issue-tracking",
				RiskLevel:   "medium",
				InputSchema: map[string]any{"type": "object"},
				TimeoutMs:   5000,
				Sensitive:   true,
			},
		},
	}
}

// baseEntry returns a Deployment Entry that authorizes basePluginManifest,
// accepting both of its tools in the REVERSE of their declared order (so the
// happy-path test can assert Spec.Tools follows entry.Tools order, not
// pm.Tools order).
func baseEntry() Entry {
	return Entry{
		Name:    "legion-jira",
		Source:  "./plugins/legion-jira",
		Enabled: true,
		Grant: GrantDecl{
			Capabilities: []string{"log", "http"},
			AllowedHosts: []string{"jira.example.com"},
			AllowedPaths: []string{"/data/jira"},
		},
		Tools: []ToolAccept{
			{Name: "jira_create_issue"},
			{Name: "jira_search"},
		},
	}
}

func baseDeployLimits() Limits {
	return Limits{
		TimeoutMs:      4000,
		MaxMemoryPages: 32,
		MaxInstances:   2,
	}
}

func boolPtr(b bool) *bool { return &b }

// --- AssembleSpec: happy path ----------------------------------------------

// TestAssembleSpec_HappyPath_ProducesCorrectSpec is the anti-vacuity test the
// brief calls for: it asserts every one of Spec.Grant's six bools (not just
// "no error"), so an implementation that grants everything would fail it,
// along with Tools order, per-tool Timeout conversion (and capping),
// MemoryPages/MaxInstances taking the min, and both allowlists intersecting.
func TestAssembleSpec_HappyPath_ProducesCorrectSpec(t *testing.T) {
	spec, err := AssembleSpec(basePluginManifest(), baseEntry(), baseDeployLimits())
	if err != nil {
		t.Fatalf("AssembleSpec: unexpected error: %v", err)
	}

	if spec.Name != "legion-jira" {
		t.Errorf("Name = %q, want %q", spec.Name, "legion-jira")
	}

	// Resource ceilings: min(plugin, deployment).
	if spec.MemoryPages != 32 {
		t.Errorf("MemoryPages = %d, want 32 (min(64, 32))", spec.MemoryPages)
	}
	if spec.MaxInstances != 2 {
		t.Errorf("MaxInstances = %d, want 2 (min(4, 2))", spec.MaxInstances)
	}

	// Grant: every one of the six bools, not just "no error". Only log and
	// http were declared AND granted; the other four must be false.
	if !spec.Grant.Log {
		t.Errorf("Grant.Log = false, want true")
	}
	if spec.Grant.Config {
		t.Errorf("Grant.Config = true, want false")
	}
	if spec.Grant.KV {
		t.Errorf("Grant.KV = true, want false")
	}
	if !spec.Grant.HTTP {
		t.Errorf("Grant.HTTP = false, want true")
	}
	if spec.Grant.FS {
		t.Errorf("Grant.FS = true, want false")
	}
	if spec.Grant.Tool {
		t.Errorf("Grant.Tool = true, want false")
	}

	// Allowlists intersect declared ∩ granted.
	wantHosts := []string{"jira.example.com"}
	if !equalStrings(spec.Grant.AllowedHosts, wantHosts) {
		t.Errorf("Grant.AllowedHosts = %v, want %v", spec.Grant.AllowedHosts, wantHosts)
	}
	wantPaths := []string{"/data/jira"}
	if !equalStrings(spec.Grant.AllowedPaths, wantPaths) {
		t.Errorf("Grant.AllowedPaths = %v, want %v", spec.Grant.AllowedPaths, wantPaths)
	}

	// Tools: order follows entry.Tools (create_issue, then search — the
	// reverse of pm.Tools' own order), and Timeout is timeout_ms converted
	// to time.Duration, capped by min(pm.Limits.TimeoutMs, deployLimits.TimeoutMs) = 4000.
	if len(spec.Tools) != 2 {
		t.Fatalf("len(Tools) = %d, want 2", len(spec.Tools))
	}
	create := spec.Tools[0]
	if create.Name != "jira_create_issue" {
		t.Errorf("Tools[0].Name = %q, want %q (entry.Tools order)", create.Name, "jira_create_issue")
	}
	if create.Description != "Create a Jira issue" {
		t.Errorf("Tools[0].Description = %q, want %q", create.Description, "Create a Jira issue")
	}
	if create.Group != "issue-tracking" {
		t.Errorf("Tools[0].Group = %q, want %q", create.Group, "issue-tracking")
	}
	if create.RiskLevel != "medium" {
		t.Errorf("Tools[0].RiskLevel = %q, want %q (no override)", create.RiskLevel, "medium")
	}
	if create.Sensitive != true {
		t.Errorf("Tools[0].Sensitive = %v, want true (no override)", create.Sensitive)
	}
	// declared timeout_ms=5000, ceiling=min(5000,4000)=4000 -> capped to 4000ms.
	if create.Timeout != 4000*time.Millisecond {
		t.Errorf("Tools[0].Timeout = %v, want %v (capped by deployment ceiling)", create.Timeout, 4000*time.Millisecond)
	}

	search := spec.Tools[1]
	if search.Name != "jira_search" {
		t.Errorf("Tools[1].Name = %q, want %q", search.Name, "jira_search")
	}
	// declared timeout_ms=3000, ceiling=4000 -> stays 3000ms (min).
	if search.Timeout != 3000*time.Millisecond {
		t.Errorf("Tools[1].Timeout = %v, want %v", search.Timeout, 3000*time.Millisecond)
	}
	if search.RiskLevel != "low" {
		t.Errorf("Tools[1].RiskLevel = %q, want %q", search.RiskLevel, "low")
	}
	if search.Sensitive != false {
		t.Errorf("Tools[1].Sensitive = %v, want false", search.Sensitive)
	}

	// AssembleSpec must not populate Deps or Registry — that is the Loader's
	// (Task 3's) job.
	if spec.Registry != nil {
		t.Errorf("Registry = %v, want nil (AssembleSpec must not populate it)", spec.Registry)
	}
	if spec.Deps.PluginName != "" {
		t.Errorf("Deps.PluginName = %q, want empty (AssembleSpec must not populate Deps)", spec.Deps.PluginName)
	}
}

// --- Rule 1: capability declaration must be a subset of the grant ----------

func TestAssembleSpec_CapabilityNotGranted_Fails(t *testing.T) {
	pm := basePluginManifest()
	pm.Capabilities = []string{"log", "http", "kv"}
	entry := baseEntry() // grants only log, http

	_, err := AssembleSpec(pm, entry, baseDeployLimits())
	requireErrorContains(t, err, `plugin "legion-jira" requires capability "kv" which the deployment does not grant`)
}

func TestAssembleSpec_ExtraGrantedCapabilityIsIgnored(t *testing.T) {
	pm := basePluginManifest()
	pm.Capabilities = []string{"log"} // declares only log
	entry := baseEntry()
	entry.Grant.Capabilities = []string{"log", "http", "kv"} // grants more than declared

	spec, err := AssembleSpec(pm, entry, baseDeployLimits())
	if err != nil {
		t.Fatalf("AssembleSpec: unexpected error: %v", err)
	}
	if !spec.Grant.Log {
		t.Errorf("Grant.Log = false, want true")
	}
	if spec.Grant.HTTP {
		t.Errorf("Grant.HTTP = true, want false (declared ⊄ this capability, so an extra grant must be ignored)")
	}
	if spec.Grant.KV {
		t.Errorf("Grant.KV = true, want false (declared ⊄ this capability, so an extra grant must be ignored)")
	}
}

// --- Rule 2: resource ceilings take the min ---------------------------------

func TestAssembleSpec_ZeroDeployLimitTreatedAsUnset(t *testing.T) {
	pm := basePluginManifest() // TimeoutMs=5000, MaxMemoryPages=64, MaxInstances=4
	entry := baseEntry()

	spec, err := AssembleSpec(pm, entry, Limits{}) // deployment declares nothing
	if err != nil {
		t.Fatalf("AssembleSpec: unexpected error: %v", err)
	}
	if spec.MemoryPages != 64 {
		t.Errorf("MemoryPages = %d, want 64 (plugin's own value, deployment side unset)", spec.MemoryPages)
	}
	if spec.MaxInstances != 4 {
		t.Errorf("MaxInstances = %d, want 4 (plugin's own value, deployment side unset)", spec.MaxInstances)
	}
	// Neither tool's declared timeout_ms should be capped.
	for _, td := range spec.Tools {
		var want time.Duration
		switch td.Name {
		case "jira_search":
			want = 3000 * time.Millisecond
		case "jira_create_issue":
			want = 5000 * time.Millisecond
		default:
			t.Fatalf("unexpected tool %q", td.Name)
		}
		if td.Timeout != want {
			t.Errorf("Tools[%q].Timeout = %v, want %v", td.Name, td.Timeout, want)
		}
	}
}

func TestAssembleSpec_MaxMemoryPagesZeroOnBothSides_Fails(t *testing.T) {
	// ParsePlugin itself refuses a manifest with MaxMemoryPages == 0, so this
	// constructs the PluginManifest by hand — bypassing ParsePlugin is what a
	// direct AssembleSpec caller could still do, and AssembleSpec must not
	// rely on ParsePlugin having run first.
	pm := basePluginManifest()
	pm.Limits.MaxMemoryPages = 0
	entry := baseEntry()
	deployLimits := baseDeployLimits()
	deployLimits.MaxMemoryPages = 0

	_, err := AssembleSpec(pm, entry, deployLimits)
	requireErrorContains(t, err, "max_memory_pages")
}

// --- Rule 3: tools must be explicitly accepted ------------------------------

func TestAssembleSpec_AcceptedToolNotDeclared_Fails(t *testing.T) {
	pm := basePluginManifest()
	entry := baseEntry()
	entry.Tools = append(entry.Tools, ToolAccept{Name: "jira_delete_issue"})

	_, err := AssembleSpec(pm, entry, baseDeployLimits())
	requireErrorContains(t, err, "jira_delete_issue")
}

func TestAssembleSpec_UnacceptedToolIsNotRegistered(t *testing.T) {
	pm := basePluginManifest()
	entry := baseEntry()
	entry.Tools = []ToolAccept{{Name: "jira_search"}} // only accepts one of the two declared tools

	spec, err := AssembleSpec(pm, entry, baseDeployLimits())
	if err != nil {
		t.Fatalf("AssembleSpec: unexpected error: %v", err)
	}
	if len(spec.Tools) != 1 {
		t.Fatalf("len(Tools) = %d, want 1", len(spec.Tools))
	}
	if spec.Tools[0].Name != "jira_search" {
		t.Errorf("Tools[0].Name = %q, want %q", spec.Tools[0].Name, "jira_search")
	}
}

func TestAssembleSpec_NoAcceptedTools_Fails(t *testing.T) {
	pm := basePluginManifest()
	entry := baseEntry()
	entry.Tools = nil

	_, err := AssembleSpec(pm, entry, baseDeployLimits())
	requireErrorContains(t, err, "legion-jira")
}

func TestAssembleSpec_RiskLevelOverrideTightens_Succeeds(t *testing.T) {
	pm := basePluginManifest() // jira_search declared "low"
	entry := baseEntry()
	entry.Tools = []ToolAccept{{Name: "jira_search", RiskLevel: "high"}}

	spec, err := AssembleSpec(pm, entry, baseDeployLimits())
	if err != nil {
		t.Fatalf("AssembleSpec: unexpected error: %v", err)
	}
	if spec.Tools[0].RiskLevel != "high" {
		t.Errorf("Tools[0].RiskLevel = %q, want %q", spec.Tools[0].RiskLevel, "high")
	}
}

func TestAssembleSpec_RiskLevelOverrideLoosens_Fails(t *testing.T) {
	pm := basePluginManifest() // jira_create_issue declared "medium"
	entry := baseEntry()
	entry.Tools = []ToolAccept{{Name: "jira_create_issue", RiskLevel: "low"}}

	_, err := AssembleSpec(pm, entry, baseDeployLimits())
	requireErrorContains(t, err, "jira_create_issue")
	requireErrorContains(t, err, "loosens")
}

func TestAssembleSpec_SensitiveOverrideTightens_Succeeds(t *testing.T) {
	pm := basePluginManifest() // jira_search declared Sensitive=false
	entry := baseEntry()
	entry.Tools = []ToolAccept{{Name: "jira_search", Sensitive: boolPtr(true)}}

	spec, err := AssembleSpec(pm, entry, baseDeployLimits())
	if err != nil {
		t.Fatalf("AssembleSpec: unexpected error: %v", err)
	}
	if spec.Tools[0].Sensitive != true {
		t.Errorf("Tools[0].Sensitive = %v, want true", spec.Tools[0].Sensitive)
	}
}

func TestAssembleSpec_SensitiveOverrideLoosens_Fails(t *testing.T) {
	pm := basePluginManifest() // jira_create_issue declared Sensitive=true
	entry := baseEntry()
	entry.Tools = []ToolAccept{{Name: "jira_create_issue", Sensitive: boolPtr(false)}}

	_, err := AssembleSpec(pm, entry, baseDeployLimits())
	requireErrorContains(t, err, "jira_create_issue")
	requireErrorContains(t, err, "loosens")
}

// --- LoadPackage -------------------------------------------------------------

func TestLoadPackage_ValidFixture(t *testing.T) {
	pm, wasm, err := LoadPackage("testdata/pkg")
	if err != nil {
		t.Fatalf("LoadPackage: unexpected error: %v", err)
	}
	if pm.Name != "legion-jira" {
		t.Errorf("Name = %q, want %q", pm.Name, "legion-jira")
	}
	wantWasm, readErr := os.ReadFile("testdata/pkg/plugin.wasm")
	if readErr != nil {
		t.Fatalf("read fixture plugin.wasm: %v", readErr)
	}
	if len(wasm) != len(wantWasm) {
		t.Fatalf("len(wasm) = %d, want %d", len(wasm), len(wantWasm))
	}
	for i := range wasm {
		if wasm[i] != wantWasm[i] {
			t.Fatalf("wasm bytes differ from testdata/pkg/plugin.wasm at offset %d", i)
		}
	}
}

func TestLoadPackage_SHA256Mismatch(t *testing.T) {
	dir := t.TempDir()
	writePackage(t, dir, `{
		"name": "p", "version": "1.0.0", "abi": 1,
		"sha256": "`+validSHA256+`",
		"limits": {"max_memory_pages": 1, "max_instances": 1},
		"tools": [{"name": "t", "group": "g", "timeout_ms": 1000}]
	}`, mustReadWasmFixture(t))

	_, _, err := LoadPackage(dir)
	requireErrorContains(t, err, "sha256")
	requireErrorContains(t, err, validSHA256) // expected digest named
	// actual digest of the real fixture wasm must also be named
	requireErrorContains(t, err, "15f5822f4c269c1e6edf849d10c3c5c0b003b7b1d1bc58ec10e83ef9e8331ab5")
}

func TestLoadPackage_MissingWasm(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), mustReadFixture(t, "pkg/plugin.json"), 0o644); err != nil {
		t.Fatalf("write plugin.json: %v", err)
	}

	_, _, err := LoadPackage(dir)
	requireErrorContains(t, err, "plugin.wasm")
}

func TestLoadPackage_MissingManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), mustReadWasmFixture(t), 0o644); err != nil {
		t.Fatalf("write plugin.wasm: %v", err)
	}

	_, _, err := LoadPackage(dir)
	requireErrorContains(t, err, "plugin.json")
}

func writePackage(t *testing.T, dir, manifestJSON string, wasm []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifestJSON), 0o644); err != nil {
		t.Fatalf("write plugin.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), wasm, 0o644); err != nil {
		t.Fatalf("write plugin.wasm: %v", err)
	}
}

func mustReadWasmFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/pkg/plugin.wasm")
	if err != nil {
		t.Fatalf("read testdata/pkg/plugin.wasm: %v", err)
	}
	return data
}
