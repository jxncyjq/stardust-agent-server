package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/host"
	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/tool"
)

// maxTimeoutMs is the largest millisecond count that can be converted to a
// time.Duration (an int64 count of nanoseconds) via
// time.Duration(ms) * time.Millisecond without overflowing. A TimeoutMs
// above this, however unlikely through normal configuration, must be
// refused rather than silently wrapping into a nonsensical (possibly
// negative) Duration.
const maxTimeoutMs = int(math.MaxInt64 / int64(time.Millisecond))

// riskLevelRank orders the risk levels a ToolDecl or ToolAccept may name, so
// AssembleSpec can tell a deployment override that TIGHTENS a plugin's own
// declared level (a smaller-or-equal rank replaced by a larger one) from one
// that LOOSENS it (which is refused). The four names mirror the ones already
// in use across internal/tool (see policy.go's "high"/"critical" deny case).
var riskLevelRank = map[string]int{
	"low":      0,
	"medium":   1,
	"high":     2,
	"critical": 3,
}

// LoadPackage reads one plugin package directory — exactly plugin.json and
// plugin.wasm, no other layout is recognized — and returns the plugin's
// validated manifest together with its wasm bytes.
//
// It verifies that the wasm bytes' sha256 digest matches the one plugin.json
// declares, refusing to return anything if they disagree: a plugin.wasm that
// does not match its own manifest is not the module the operator reviewed
// and approved, and loading it anyway would silently run different code
// under an old authorization. A mismatch names both the digest plugin.json
// declares and the digest the bytes actually hash to, so the caller can tell
// a stale manifest from a tampered or corrupted module.
func LoadPackage(dir string) (PluginManifest, []byte, error) {
	manifestPath := filepath.Join(dir, "plugin.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return PluginManifest{}, nil, fmt.Errorf("load plugin package %q: read plugin.json: %w", dir, err)
	}
	pm, err := ParsePlugin(manifestData)
	if err != nil {
		return PluginManifest{}, nil, fmt.Errorf("load plugin package %q: %w", dir, err)
	}

	wasmPath := filepath.Join(dir, "plugin.wasm")
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		return PluginManifest{}, nil, fmt.Errorf("load plugin package %q: read plugin.wasm: %w", dir, err)
	}

	sum := sha256.Sum256(wasm)
	actual := hex.EncodeToString(sum[:])
	if !strings.EqualFold(actual, pm.SHA256) {
		return PluginManifest{}, nil, fmt.Errorf(
			"load plugin package %q: plugin %q sha256 mismatch: plugin.json declares %s, plugin.wasm actually hashes to %s",
			dir, pm.Name, pm.SHA256, actual,
		)
	}

	return pm, wasm, nil
}

// AssembleSpec reconciles a plugin's own declaration (pm) against one
// deployment entry's authorization (entry) and the deployment's own
// resource ceiling (deployLimits), producing the host.Spec that
// host.Activate consumes.
//
// AssembleSpec never populates Spec.Deps, and — since it is not given a
// *tool.Registry either — never populates Spec.Registry. Injecting the
// concrete dependencies a granted capability needs (*http.Client, an event
// bus, the registry tools are entered into, ...) is the Loader's job
// (Task 3), not this manifest-reconciliation package's: this package has no
// business knowing those types exist. Callers must fill both in, along with
// Spec.Wasm (LoadPackage returns that separately), before calling
// host.Activate.
//
// Three reconciliation rules, each intentionally asymmetric:
//
//  1. Capabilities: every capability pm declares must be present in
//     entry.Grant.Capabilities, or AssembleSpec refuses to load the plugin
//     at all — a plugin that cannot get what it asked for is not safely
//     runnable in some reduced mode. A capability entry.Grant grants that
//     pm never declared is silently NOT carried into Spec.Grant: granting
//     more than was requested only enlarges the attack surface for no
//     benefit. AllowedHosts and AllowedPaths follow the same declared ∩
//     granted rule; an empty intersection is a legal (if useless) result,
//     not an error — perm.Grant already treats an empty allowlist as "the
//     capability is on but reaches nothing".
//
//  2. Resource ceilings: MaxInstances, and each tool's Timeout, take
//     min(plugin's own request, deployLimits) — with a zero on either side
//     read as "not declared", so the other side's value is used unchanged
//     rather than the min collapsing to zero. A tool's own timeout_ms is
//     first reconciled against pm.Limits.TimeoutMs vs. deployLimits.TimeoutMs
//     (the plugin's and the deployment's general per-call ceilings) using
//     the same rule, and the tool's own value is then additionally capped by
//     that ceiling. MaxMemoryPages follows the same "zero means unset" rule
//     with one exception: zero on BOTH sides is refused outright, because
//     host.NewRuntime panics on a zero page count and "unset on both sides"
//     must never silently become "zero". deployLimits.TimeoutMs and
//     deployLimits.MaxInstances must not be negative — minIntZeroUnset would
//     otherwise happily pick a negative value as "the smaller one", producing
//     a negative Timeout or MaxInstances that host.Activate would only catch
//     later, less actionably; a negative deployLimits value is refused
//     outright, naming the field. A tool's final effective TimeoutMs is also
//     bounds-checked against maxTimeoutMs before conversion to time.Duration,
//     since a sufficiently large millisecond count would overflow that
//     conversion.
//
//  3. Tools: only tools entry.Tools explicitly accepts are registered, in
//     entry.Tools' own order — a pm tool entry.Tools never mentions is
//     simply left out of Spec.Tools (most plugins offer more than one
//     deployment wants). An entry.Tools name that pm never declared is
//     refused (a typo in plugins.json is far more likely to be a mistake
//     than an unused acceptance). entry.Tools naming the same tool twice is
//     refused here too, for the same reason ParsePlugin's ToolDecl duplicate
//     check exists: failing at this specific, earlier layer names the
//     mistake more actionably than host.validateSpec's later, less specific
//     error. A ToolAccept's RiskLevel/Sensitive may only TIGHTEN the
//     plugin's own declaration (e.g. "low" to "high", false to true), never
//     loosen it; loosening is refused. An empty RiskLevel or a nil Sensitive
//     means "no override, use the plugin's own declared value".
func AssembleSpec(pm PluginManifest, entry Entry, deployLimits Limits) (host.Spec, error) {
	if deployLimits.TimeoutMs < 0 {
		return host.Spec{}, fmt.Errorf(
			"plugin %q: deployment limit timeout_ms is %d, want >= 0", pm.Name, deployLimits.TimeoutMs,
		)
	}
	if deployLimits.MaxInstances < 0 {
		return host.Spec{}, fmt.Errorf(
			"plugin %q: deployment limit max_instances is %d, want >= 0", pm.Name, deployLimits.MaxInstances,
		)
	}

	grant, err := reconcileCapabilities(pm.Name, pm.Capabilities, entry.Grant.Capabilities)
	if err != nil {
		return host.Spec{}, err
	}
	grant.AllowedHosts = intersectPreserveOrder(pm.Network.AllowedHosts, entry.Grant.AllowedHosts, true)
	grant.AllowedPaths = intersectPreserveOrder(pm.Filesystem.AllowedPaths, entry.Grant.AllowedPaths, false)

	if pm.Limits.MaxMemoryPages == 0 && deployLimits.MaxMemoryPages == 0 {
		return host.Spec{}, fmt.Errorf(
			"plugin %q: limits.max_memory_pages is 0 on both the plugin manifest and the deployment limit; "+
				"host.NewRuntime panics on a zero page count, so at least one side must declare it",
			pm.Name,
		)
	}
	memoryPages := minU32ZeroUnset(pm.Limits.MaxMemoryPages, deployLimits.MaxMemoryPages)
	maxInstances := minIntZeroUnset(pm.Limits.MaxInstances, deployLimits.MaxInstances)
	timeoutCeilingMs := minIntZeroUnset(pm.Limits.TimeoutMs, deployLimits.TimeoutMs)

	declByName := make(map[string]ToolDecl, len(pm.Tools))
	for _, td := range pm.Tools {
		declByName[td.Name] = td
	}

	seenAccepted := make(map[string]struct{}, len(entry.Tools))
	specTools := make([]tool.Descriptor, 0, len(entry.Tools))
	for _, accept := range entry.Tools {
		if _, dup := seenAccepted[accept.Name]; dup {
			return host.Spec{}, fmt.Errorf(
				"plugin %q: deployment accepts tool %q twice; one name is one tool, and the second "+
					"acceptance would only be caught later, less actionably, by host.validateSpec",
				pm.Name, accept.Name,
			)
		}
		seenAccepted[accept.Name] = struct{}{}

		td, ok := declByName[accept.Name]
		if !ok {
			return host.Spec{}, fmt.Errorf(
				"plugin %q: deployment accepts tool %q which the plugin does not declare",
				pm.Name, accept.Name,
			)
		}

		riskLevel := td.RiskLevel
		if accept.RiskLevel != "" {
			if err := requireRiskLevelNotLoosened(pm.Name, accept.Name, td.RiskLevel, accept.RiskLevel); err != nil {
				return host.Spec{}, err
			}
			riskLevel = accept.RiskLevel
		}

		sensitive := td.Sensitive
		if accept.Sensitive != nil {
			if td.Sensitive && !*accept.Sensitive {
				return host.Spec{}, fmt.Errorf(
					"plugin %q: tool %q override sensitive=false loosens the plugin's own sensitive=true "+
						"declaration; a deployment override may only tighten, never loosen",
					pm.Name, accept.Name,
				)
			}
			sensitive = *accept.Sensitive
		}

		toolTimeoutMs := minIntZeroUnset(td.TimeoutMs, timeoutCeilingMs)
		if toolTimeoutMs > maxTimeoutMs {
			return host.Spec{}, fmt.Errorf(
				"plugin %q: tool %q effective timeout_ms %d exceeds the maximum %d representable as a "+
					"time.Duration; time.Duration(timeoutMs) * time.Millisecond would overflow",
				pm.Name, td.Name, toolTimeoutMs, maxTimeoutMs,
			)
		}
		specTools = append(specTools, tool.Descriptor{
			Name:        td.Name,
			Description: td.Description,
			InputSchema: td.InputSchema,
			RiskLevel:   riskLevel,
			Timeout:     time.Duration(toolTimeoutMs) * time.Millisecond,
			Group:       td.Group,
			Sensitive:   sensitive,
		})
	}
	if len(specTools) == 0 {
		return host.Spec{}, fmt.Errorf(
			"plugin %q: deployment accepts no tools; an assembled Spec with no tools would have nothing to register",
			pm.Name,
		)
	}

	return host.Spec{
		Name:         pm.Name,
		Tools:        specTools,
		Grant:        grant,
		MaxInstances: maxInstances,
		MemoryPages:  memoryPages,
	}, nil
}

// reconcileCapabilities checks that every capability in declared is present
// in granted (rule 1's "declaration ⊄ grant = refuse"), naming the first
// missing one, and — if that check passes — builds the perm.Grant whose six
// bools are exactly the declared set: a name present in granted but absent
// from declared is not carried through (rule 1's asymmetry: extra grants are
// ignored, not an error).
func reconcileCapabilities(pluginName string, declared, granted []string) (perm.Grant, error) {
	grantedSet := make(map[string]bool, len(granted))
	for _, c := range granted {
		grantedSet[c] = true
	}

	declaredSet := make(map[string]bool, len(declared))
	for _, c := range declared {
		if !grantedSet[c] {
			return perm.Grant{}, fmt.Errorf(
				"plugin %q requires capability %q which the deployment does not grant", pluginName, c,
			)
		}
		declaredSet[c] = true
	}

	return perm.Grant{
		Log:    declaredSet["log"],
		Config: declaredSet["config"],
		KV:     declaredSet["kv"],
		HTTP:   declaredSet["http"],
		FS:     declaredSet["fs"],
		Tool:   declaredSet["tool"],
	}, nil
}

// requireRiskLevelNotLoosened refuses an override whose rank is lower than
// the plugin's own declared level's rank. An override that is not one of the
// four known risk levels is refused outright: it is deployment-authored
// input crossing a trust boundary, so an unrecognized value cannot be waved
// through as "presumably fine". The plugin's own declared level, by
// contrast, is not required to be one of the four known names here (Task 1's
// ParsePlugin does not validate risk_level's value) — an unrecognized
// declared level is treated as the lowest possible rank, so any recognized
// override still counts as tightening it.
func requireRiskLevelNotLoosened(pluginName, toolName, declared, override string) error {
	overrideRank, ok := riskLevelRank[override]
	if !ok {
		return fmt.Errorf(
			"plugin %q: tool %q override risk_level %q is not a known level (want one of low, medium, high, critical)",
			pluginName, toolName, override,
		)
	}
	declaredRank, ok := riskLevelRank[declared]
	if !ok {
		declaredRank = -1
	}
	if overrideRank < declaredRank {
		return fmt.Errorf(
			"plugin %q: tool %q override risk_level %q loosens the plugin's own declared %q; "+
				"a deployment override may only tighten, never loosen",
			pluginName, toolName, override, declared,
		)
	}
	return nil
}

// minIntZeroUnset returns min(a, b), except a zero on either side is read as
// "not declared" rather than as the smallest possible value: it returns
// whichever of a, b is non-zero when only one is, 0 when both are, and the
// smaller of the two when both are positive.
func minIntZeroUnset(a, b int) int {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// minU32ZeroUnset is minIntZeroUnset for uint32, used for MaxMemoryPages.
// Callers that must additionally refuse "both sides zero" (host.NewRuntime
// panics on a zero page count) check that themselves before calling this.
func minU32ZeroUnset(a, b uint32) uint32 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// intersectPreserveOrder returns the entries of declared that also appear in
// granted, in declared's own order. caseInsensitive matches AllowedHosts'
// case-insensitive comparison (DNS names are case-insensitive, mirroring
// perm.Grant.HostAllowed); AllowedPaths is compared exactly, since paths are
// not case-insensitive in general.
func intersectPreserveOrder(declared, granted []string, caseInsensitive bool) []string {
	if len(declared) == 0 {
		return nil
	}
	grantedSet := make(map[string]struct{}, len(granted))
	for _, g := range granted {
		if caseInsensitive {
			g = strings.ToLower(g)
		}
		grantedSet[g] = struct{}{}
	}
	var result []string
	for _, d := range declared {
		key := d
		if caseInsensitive {
			key = strings.ToLower(d)
		}
		if _, ok := grantedSet[key]; ok {
			result = append(result, d)
		}
	}
	return result
}
