package manifest

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/sign"
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

// TestAssembleSpec_ZeroPluginLimitTreatedAsUnset is the mirror of
// TestAssembleSpec_ZeroDeployLimitTreatedAsUnset: the plugin side declares
// nothing for MaxInstances/TimeoutMs, so the deployment's own value must be
// used unchanged, not collapsed to zero.
func TestAssembleSpec_ZeroPluginLimitTreatedAsUnset(t *testing.T) {
	pm := basePluginManifest()
	pm.Limits.MaxInstances = 0
	pm.Limits.TimeoutMs = 0
	entry := baseEntry()
	deployLimits := baseDeployLimits() // TimeoutMs=4000, MaxInstances=2

	spec, err := AssembleSpec(pm, entry, deployLimits)
	if err != nil {
		t.Fatalf("AssembleSpec: unexpected error: %v", err)
	}
	if spec.MaxInstances != 2 {
		t.Errorf("MaxInstances = %d, want 2 (deployment's own value, plugin side unset)", spec.MaxInstances)
	}
	// timeoutCeilingMs = minIntZeroUnset(0, 4000) = 4000 (deployment's value,
	// since the plugin side is unset). jira_create_issue's declared 5000ms is
	// capped down to that ceiling; jira_search's declared 3000ms is already
	// under it and stays unchanged.
	for _, td := range spec.Tools {
		var want time.Duration
		switch td.Name {
		case "jira_search":
			want = 3000 * time.Millisecond
		case "jira_create_issue":
			want = 4000 * time.Millisecond
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

func TestAssembleSpec_NegativeDeployTimeoutMs_Fails(t *testing.T) {
	pm := basePluginManifest()
	entry := baseEntry()
	deployLimits := baseDeployLimits()
	deployLimits.TimeoutMs = -1

	_, err := AssembleSpec(pm, entry, deployLimits)
	requireErrorContains(t, err, "timeout_ms")
}

func TestAssembleSpec_NegativeDeployMaxInstances_Fails(t *testing.T) {
	pm := basePluginManifest()
	entry := baseEntry()
	deployLimits := baseDeployLimits()
	deployLimits.MaxInstances = -1

	_, err := AssembleSpec(pm, entry, deployLimits)
	requireErrorContains(t, err, "max_instances")
}

// --- Rule 3: tools must be explicitly accepted ------------------------------

func TestAssembleSpec_AcceptedToolNotDeclared_Fails(t *testing.T) {
	pm := basePluginManifest()
	entry := baseEntry()
	entry.Tools = append(entry.Tools, ToolAccept{Name: "jira_delete_issue"})

	_, err := AssembleSpec(pm, entry, baseDeployLimits())
	requireErrorContains(t, err, "jira_delete_issue")
}

func TestAssembleSpec_DuplicateAcceptedTool_Fails(t *testing.T) {
	pm := basePluginManifest()
	entry := baseEntry()
	entry.Tools = []ToolAccept{{Name: "jira_search"}, {Name: "jira_search"}}

	_, err := AssembleSpec(pm, entry, baseDeployLimits())
	requireErrorContains(t, err, "jira_search")
	requireErrorContains(t, err, "twice")
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

func TestAssembleSpec_UnknownOverrideRiskLevel_Fails(t *testing.T) {
	pm := basePluginManifest()
	entry := baseEntry()
	entry.Tools = []ToolAccept{{Name: "jira_search", RiskLevel: "extreme"}}

	_, err := AssembleSpec(pm, entry, baseDeployLimits())
	requireErrorContains(t, err, "jira_search")
	requireErrorContains(t, err, "extreme")
}

// TestAssembleSpec_TimeoutMsOverflowsDuration_Fails exercises the
// maxTimeoutMs guard: a tool's effective timeout_ms so large that
// time.Duration(ms) * time.Millisecond would overflow int64 must be
// refused, naming the tool and the value, rather than silently producing a
// nonsensical (possibly negative) Duration.
func TestAssembleSpec_TimeoutMsOverflowsDuration_Fails(t *testing.T) {
	pm := basePluginManifest()
	pm.Tools[0].TimeoutMs = math.MaxInt64 // jira_search
	pm.Limits.TimeoutMs = 0               // don't let the ceiling cap it down first
	entry := baseEntry()
	entry.Tools = []ToolAccept{{Name: "jira_search"}}
	deployLimits := baseDeployLimits()
	deployLimits.TimeoutMs = 0 // ceiling stays unset on both sides

	_, err := AssembleSpec(pm, entry, deployLimits)
	requireErrorContains(t, err, "jira_search")
	requireErrorContains(t, err, "9223372036854775807")
}

// --- LoadPackage -------------------------------------------------------------

func TestLoadPackage_ValidFixture(t *testing.T) {
	pm, wasm, err := LoadPackage("testdata/pkg", nil)
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

	_, _, err := LoadPackage(dir, nil)
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

	_, _, err := LoadPackage(dir, nil)
	requireErrorContains(t, err, "plugin.wasm")
}

func TestLoadPackage_MissingManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), mustReadWasmFixture(t), 0o644); err != nil {
		t.Fatalf("write plugin.wasm: %v", err)
	}

	_, _, err := LoadPackage(dir, nil)
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

// --- LoadPackage: signature verification -------------------------------------
//
// Every key pair and every plugin.sig below is minted here, at test time, into
// a t.TempDir(). None of it is committed under testdata/: checking a private
// key into git — even a throwaway one used only by a test — trains exactly the
// reflex this whole feature exists to prevent.

// newKeyring mints a fresh Ed25519 key pair and returns a Keyring trusting
// exactly that one public key under id, along with the private key to sign
// with. The keyring is built by feeding a real keyring document through
// sign.ParseKeyring rather than by reaching into sign's internals, so the
// tests below exercise the same construction path a deployment does.
func newKeyring(t *testing.T, id sign.KeyID) (*sign.Keyring, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	doc := fmt.Sprintf(`{"keys":[{"id":%q,"algorithm":"ed25519","public_key":%q}]}`,
		string(id), base64.StdEncoding.EncodeToString(pub))
	kr, err := sign.ParseKeyring([]byte(doc))
	if err != nil {
		t.Fatalf("parse generated keyring: %v", err)
	}
	return kr, priv
}

// writeSignatureDoc writes sig into dir as a plugin.sig document, without
// signing anything itself. Tests that need a well-formed-but-wrong signature
// (one altered byte) use this so that the refusal they observe comes from the
// cryptographic check and not from sign.ParseSignature's shape checks.
//
// It encodes through sign.MarshalSignature rather than formatting the JSON
// here. That is the point of MarshalSignature existing: the plugin.sig format
// has exactly one reader (sign.ParseSignature) and must have exactly one
// writer, or a test and the signing command would encode the same file
// independently and only one of them would learn about a new field.
func writeSignatureDoc(t *testing.T, dir string, sig sign.Signature) {
	t.Helper()
	doc, err := sign.MarshalSignature(sig)
	if err != nil {
		t.Fatalf("marshal plugin.sig: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.sig"), doc, 0o644); err != nil {
		t.Fatalf("write plugin.sig: %v", err)
	}
}

// writeSignature signs message with priv under id and writes the resulting
// plugin.sig into dir.
func writeSignature(t *testing.T, dir string, priv ed25519.PrivateKey, id sign.KeyID, message []byte) {
	t.Helper()
	sig, err := sign.Sign(priv, id, message)
	if err != nil {
		t.Fatalf("sign message: %v", err)
	}
	writeSignatureDoc(t, dir, sig)
}

// signedPackage assembles a complete, correctly signed package — plugin.json
// and plugin.wasm copied from testdata/pkg, plus a freshly produced plugin.sig
// over plugin.json's raw bytes — in a fresh temporary directory, and returns
// that directory with a Keyring trusting the key that signed it.
func signedPackage(t *testing.T) (string, *sign.Keyring) {
	t.Helper()
	dir := t.TempDir()
	manifestData := mustReadFixture(t, "pkg/plugin.json")
	writePackage(t, dir, string(manifestData), mustReadWasmFixture(t))
	kr, priv := newKeyring(t, "ops-2026")
	writeSignature(t, dir, priv, "ops-2026", manifestData)
	return dir, kr
}

func TestLoadPackage_ValidSignatureLoads(t *testing.T) {
	dir, kr := signedPackage(t)

	pm, wasm, err := LoadPackage(dir, kr)
	if err != nil {
		t.Fatalf("LoadPackage: unexpected error: %v", err)
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
	if pm.SHA256 != "15f5822f4c269c1e6edf849d10c3c5c0b003b7b1d1bc58ec10e83ef9e8331ab5" {
		t.Errorf("SHA256 = %q, want the fixture wasm's digest", pm.SHA256)
	}
	if len(pm.Tools) != 1 || pm.Tools[0].Name != "jira_search" {
		t.Errorf("Tools = %+v, want exactly one named jira_search", pm.Tools)
	}
	if !bytes.Equal(wasm, mustReadWasmFixture(t)) {
		t.Errorf("returned wasm bytes differ from testdata/pkg/plugin.wasm")
	}
}

func TestLoadPackage_MissingSignatureWithKeyring(t *testing.T) {
	dir, kr := signedPackage(t)
	if err := os.Remove(filepath.Join(dir, "plugin.sig")); err != nil {
		t.Fatalf("remove plugin.sig: %v", err)
	}

	_, _, err := LoadPackage(dir, kr)
	if err == nil {
		t.Fatal("LoadPackage: want an error for a package with no plugin.sig, got nil")
	}
	// The missing case has its own wording, distinct from an
	// unreadable-but-present plugin.sig (see
	// TestLoadPackage_UnreadableSignatureFile) — a future caller must never
	// be able to key on "not exist" and treat it as "package absent, skip".
	requireErrorContains(t, err, "plugin.sig is missing")
}

// TestLoadPackage_UnreadableSignatureFile pins the missing/unreadable
// distinction from the other side: a plugin.sig that exists but cannot be
// read as a regular file (here, it exists as a directory) must be reported
// with the generic "read plugin.sig" wording, never the "is missing" wording
// that fs.ErrNotExist gets — the two failure modes must stay
// distinguishable, and neither may claim the package has no signature.
func TestLoadPackage_UnreadableSignatureFile(t *testing.T) {
	dir, kr := signedPackage(t)
	if err := os.Remove(filepath.Join(dir, "plugin.sig")); err != nil {
		t.Fatalf("remove plugin.sig: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "plugin.sig"), 0o755); err != nil {
		t.Fatalf("mkdir plugin.sig: %v", err)
	}

	_, _, err := LoadPackage(dir, kr)
	if err == nil {
		t.Fatal("LoadPackage: want an error for a plugin.sig that exists but cannot be read, got nil")
	}
	requireErrorContains(t, err, "read plugin.sig")
	if strings.Contains(err.Error(), "is missing") {
		t.Errorf("error %q says plugin.sig is missing, but plugin.sig exists (as a directory); "+
			"the missing and unreadable cases must say different things", err)
	}
}

func TestLoadPackage_SignatureAlteredByOneByte(t *testing.T) {
	dir, kr := signedPackage(t)
	sigData, err := os.ReadFile(filepath.Join(dir, "plugin.sig"))
	if err != nil {
		t.Fatalf("read plugin.sig: %v", err)
	}
	sig, err := sign.ParseSignature(sigData)
	if err != nil {
		t.Fatalf("parse generated plugin.sig: %v", err)
	}
	// Flip one bit of the signature value, leaving the document's shape — key
	// id, algorithm, signature length — untouched, so that the refusal can
	// only come from the cryptographic check.
	sig.Value[0] ^= 0x01
	writeSignatureDoc(t, dir, sig)

	_, _, err = LoadPackage(dir, kr)
	if err == nil {
		t.Fatal("LoadPackage: want an error for a signature altered by one byte, got nil")
	}
	requireErrorContains(t, err, "does not verify")
	requireErrorContains(t, err, "ops-2026")
}

func TestLoadPackage_SignedByUntrustedKey(t *testing.T) {
	dir, kr := signedPackage(t)
	// A second, perfectly valid key pair that the keyring does not trust. Its
	// own keyring is discarded on purpose: the point is that a real signature
	// made by a key we never trusted is refused.
	_, attackerPriv := newKeyring(t, "attacker-2027")
	writeSignature(t, dir, attackerPriv, "attacker-2027", mustReadFixture(t, "pkg/plugin.json"))

	_, _, err := LoadPackage(dir, kr)
	if err == nil {
		t.Fatal("LoadPackage: want an error for a package signed by an untrusted key, got nil")
	}
	requireErrorContains(t, err, "attacker-2027")
	requireErrorContains(t, err, "not in the keyring")
}

// TestLoadPackage_ImpersonationWithTrustedKeyID covers the attack shape
// TestLoadPackage_SignedByUntrustedKey does not: there, the key id itself is
// untrusted, so refusal comes from sign.Keyring.Verify's unknown-id branch.
// Here an attacker who holds no trusted key signs with their OWN key but
// labels the signature with a key_id the keyring DOES trust ("ops-2026"),
// hoping the id alone is enough. It is not: Verify resolves the named id to
// its one trusted public key and checks strictly against that key, so this
// must be refused by the cryptographic branch — the same one
// TestLoadPackage_SignatureAlteredByOneByte exercises — giving Task 1's
// "Verify never iterates keys hoping one works" guarantee a package-level
// witness for the impersonation case specifically, not just bit-flip.
func TestLoadPackage_ImpersonationWithTrustedKeyID(t *testing.T) {
	dir, kr := signedPackage(t)
	// A second, real key pair the keyring does not trust.
	_, attackerPriv := newKeyring(t, "attacker-2027")
	manifestData := mustReadFixture(t, "pkg/plugin.json")
	// Sign with the attacker's own key, but claim the id "ops-2026" — the
	// one key signedPackage's keyring actually trusts.
	writeSignature(t, dir, attackerPriv, "ops-2026", manifestData)

	_, _, err := LoadPackage(dir, kr)
	if err == nil {
		t.Fatal("LoadPackage: want an error for a signature made by an untrusted key but labeled " +
			"with a trusted key id, got nil")
	}
	requireErrorContains(t, err, "does not verify")
	requireErrorContains(t, err, "ops-2026")
}

// TestLoadPackage_ManifestAlteredByOneByte is the reason the whole mechanism
// exists: a plugin.json edited after signing, whose own shape is still
// perfectly valid, must be refused.
func TestLoadPackage_ManifestAlteredByOneByte(t *testing.T) {
	dir, kr := signedPackage(t)
	original := mustReadFixture(t, "pkg/plugin.json")
	altered := bytes.Replace(original, []byte(`"version": "1.2.0"`), []byte(`"version": "1.2.1"`), 1)
	if bytes.Equal(altered, original) {
		t.Fatal("fixture plugin.json no longer contains the version field this test alters")
	}
	// The altered manifest is still a valid manifest on its own — nothing but
	// the signature can tell that it was changed.
	if _, perr := ParsePlugin(altered); perr != nil {
		t.Fatalf("altered manifest should still parse; the signature must be what refuses it: %v", perr)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), altered, 0o644); err != nil {
		t.Fatalf("write altered plugin.json: %v", err)
	}

	_, _, err := LoadPackage(dir, kr)
	if err == nil {
		t.Fatal("LoadPackage: want an error for a plugin.json altered after signing, got nil")
	}
	requireErrorContains(t, err, "does not verify")
}

// TestLoadPackage_WasmAlteredWithManifestAndSignatureIntact proves the plan's
// central argument: one signature covers both files. The signature and the
// declared sha256 are left exactly as signed, so the only thing that can
// refuse the swapped binary is the digest check — and it must.
func TestLoadPackage_WasmAlteredWithManifestAndSignatureIntact(t *testing.T) {
	dir, kr := signedPackage(t)
	wasm := mustReadWasmFixture(t)
	wasm[0] ^= 0xff
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), wasm, 0o644); err != nil {
		t.Fatalf("write altered plugin.wasm: %v", err)
	}

	_, _, err := LoadPackage(dir, kr)
	if err == nil {
		t.Fatal("LoadPackage: want an error for an altered plugin.wasm, got nil")
	}
	requireErrorContains(t, err, "sha256")
	requireErrorContains(t, err, "legion-jira")
}

// TestLoadPackage_NilKeyringSkipsVerificationNotDigest pins the contract's one
// declared optional: a nil keyring means this deployment does not require
// signatures, so an unsigned package loads — but the sha256 check is NOT part
// of that concession and still refuses a mismatched binary.
func TestLoadPackage_NilKeyringSkipsVerificationNotDigest(t *testing.T) {
	unsigned := t.TempDir()
	writePackage(t, unsigned, string(mustReadFixture(t, "pkg/plugin.json")), mustReadWasmFixture(t))

	pm, _, err := LoadPackage(unsigned, nil)
	if err != nil {
		t.Fatalf("LoadPackage with a nil keyring: unexpected error: %v", err)
	}
	if pm.Name != "legion-jira" {
		t.Errorf("Name = %q, want %q", pm.Name, "legion-jira")
	}

	corrupted := t.TempDir()
	wasm := mustReadWasmFixture(t)
	wasm[0] ^= 0xff
	writePackage(t, corrupted, string(mustReadFixture(t, "pkg/plugin.json")), wasm)

	_, _, err = LoadPackage(corrupted, nil)
	if err == nil {
		t.Fatal("LoadPackage with a nil keyring: want a sha256 error for an altered plugin.wasm, got nil")
	}
	requireErrorContains(t, err, "sha256")
}

// TestLoadPackage_MalformedSignatureDocument covers the remaining refusal in
// verifyManifestSignature: a plugin.sig that is present but is not a
// signature document at all must be refused, not read as "no signature" and
// certainly not as a valid one.
func TestLoadPackage_MalformedSignatureDocument(t *testing.T) {
	dir, kr := signedPackage(t)
	if err := os.WriteFile(filepath.Join(dir, "plugin.sig"), []byte("not a signature document"), 0o644); err != nil {
		t.Fatalf("write malformed plugin.sig: %v", err)
	}

	_, _, err := LoadPackage(dir, kr)
	if err == nil {
		t.Fatal("LoadPackage: want an error for a malformed plugin.sig, got nil")
	}
	requireErrorContains(t, err, "parse plugin.sig")
}

// TestLoadPackage_EmptySignatureFile pins that a zero-byte plugin.sig is
// refused the same way any other malformed document is (sign.ParseSignature
// sees io.EOF from json.Decoder), and — more importantly — that it is NOT
// treated the same as a missing plugin.sig: the file is present, so this
// must go through verifyManifestSignature's parse failure, never through the
// fs.ErrNotExist branch os.ReadFile would take for a genuinely absent file.
func TestLoadPackage_EmptySignatureFile(t *testing.T) {
	dir, kr := signedPackage(t)
	if err := os.WriteFile(filepath.Join(dir, "plugin.sig"), []byte{}, 0o644); err != nil {
		t.Fatalf("truncate plugin.sig: %v", err)
	}

	_, _, err := LoadPackage(dir, kr)
	if err == nil {
		t.Fatal("LoadPackage: want an error for a zero-byte plugin.sig, got nil")
	}
	requireErrorContains(t, err, "parse plugin.sig")
	if strings.Contains(err.Error(), "is missing") {
		t.Errorf("error %q says plugin.sig is missing, but an empty file is not the same as no file", err)
	}
}

// --- LoadPackage: ErrUntrustedPackage classification -------------------------
//
// These pin which of verifyManifestSignature's four failure paths must be
// identifiable via errors.Is(err, ErrUntrustedPackage) and which must not.
// The three trust verdicts (missing signature, corrupt signature, signature
// that does not verify) get the sentinel; an I/O fault while reading
// plugin.sig deliberately does not — see ErrUntrustedPackage's doc comment.

// writeTestPackage writes a complete, valid, but unsigned plugin package
// (plugin.json and plugin.wasm copied from testdata/pkg) into a fresh
// temporary directory, and returns that directory. It mirrors signedPackage
// above, minus the signing step, for tests that need to control plugin.sig
// themselves.
func writeTestPackage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writePackage(t, dir, string(mustReadFixture(t, "pkg/plugin.json")), mustReadWasmFixture(t))
	return dir
}

// testKeyring mints a fresh Ed25519 key pair and returns a Keyring trusting
// exactly that one public key under id "ops-2026" — a deployment that
// requires signatures. The private key is discarded on purpose: callers that
// need a package actually signed by a trusted key use signedPackage instead.
func testKeyring(t *testing.T) *sign.Keyring {
	t.Helper()
	kr, _ := newKeyring(t, "ops-2026")
	return kr
}

// signTestPackageWithForeignKey signs dir's plugin.json with a freshly
// minted key pair that no keyring returned by testKeyring trusts, and writes
// the resulting plugin.sig into dir — a real, well-formed signature that
// still must be refused because the keyring does not know its key.
func signTestPackageWithForeignKey(t *testing.T, dir string) {
	t.Helper()
	_, foreignPriv := newKeyring(t, "foreign-key")
	writeSignature(t, dir, foreignPriv, "foreign-key", mustReadFixture(t, "pkg/plugin.json"))
}

func TestLoadPackageMarksAnUnsignedPackageUntrusted(t *testing.T) {
	// 包齐全但没有 plugin.sig，且部署要求签名（keyring 非 nil）
	dir := writeTestPackage(t) // 既有夹具助手：写 plugin.json + plugin.wasm
	kr := testKeyring(t)       // 既有夹具助手
	_, _, err := LoadPackage(dir, kr)
	if err == nil {
		t.Fatal("LoadPackage on an unsigned package = nil error, want an untrusted-package error")
	}
	if !errors.Is(err, ErrUntrustedPackage) {
		t.Errorf("LoadPackage error = %v, want it to wrap ErrUntrustedPackage", err)
	}
}

func TestLoadPackageMarksACorruptSignatureUntrusted(t *testing.T) {
	dir := writeTestPackage(t)
	kr := testKeyring(t)
	if err := os.WriteFile(filepath.Join(dir, "plugin.sig"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt plugin.sig: %v", err)
	}
	_, _, err := LoadPackage(dir, kr)
	if !errors.Is(err, ErrUntrustedPackage) {
		t.Errorf("LoadPackage error = %v, want it to wrap ErrUntrustedPackage", err)
	}
}

func TestLoadPackageMarksAWrongSignatureUntrusted(t *testing.T) {
	dir := writeTestPackage(t)
	kr := testKeyring(t)
	// 用一把 keyring 不信任的密钥签名
	signTestPackageWithForeignKey(t, dir) // 见 Step 3 说明；若既有助手可用则复用
	_, _, err := LoadPackage(dir, kr)
	if !errors.Is(err, ErrUntrustedPackage) {
		t.Errorf("LoadPackage error = %v, want it to wrap ErrUntrustedPackage", err)
	}
}

// 第四条：I/O 故障不是信任问题，必须 NOT 命中哨兵。
// 用一个目录冒充 plugin.sig：读它会得到 EISDIR 一类的 I/O 错误，而非 fs.ErrNotExist。
func TestLoadPackageDoesNotMarkAnIOFailureUntrusted(t *testing.T) {
	dir := writeTestPackage(t)
	kr := testKeyring(t)
	if err := os.Mkdir(filepath.Join(dir, "plugin.sig"), 0o755); err != nil {
		t.Fatalf("mkdir plugin.sig: %v", err)
	}
	_, _, err := LoadPackage(dir, kr)
	if err == nil {
		t.Fatal("LoadPackage with an unreadable plugin.sig = nil error, want an I/O error")
	}
	if errors.Is(err, ErrUntrustedPackage) {
		t.Errorf("LoadPackage error = %v, want an I/O failure NOT classified as untrusted: "+
			"a disk or permission problem is a retryable environment fault, not a trust verdict", err)
	}
}
