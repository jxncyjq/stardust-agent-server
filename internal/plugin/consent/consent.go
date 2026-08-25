// Package consent holds the plugin authorization validation shared by every
// caller that grants or revokes a plugin's capabilities: today that is the
// `agent plugins grant`/`install`/`deny` CLI commands (internal/cli), and it
// is meant to be the same package a future HTTP endpoint calls, rather than
// a second hand-written copy of the same rules.
//
// The two code paths' previous state -- CLI-only, with each subcommand
// validating on its own -- is exactly the shape that let install and grant
// enforce two different directions of the same rule (one refused a
// capability with no matching allowlist entry outright; the other stayed
// silent and wrote an entry that authorized nothing). A second caller
// re-implementing these checks instead of calling through here would be
// able to reintroduce that same drift a third time.
//
// Every function here is actor-agnostic: actor names whoever is asking, for
// the error message ("plugins grant", "POST /v1/plugins/{name}/grant"), and
// subject (where present) names the input that carried the list
// ("--capabilities", "capabilities"). Splitting a raw flag string into a
// []string is the caller's job -- this package only ever sees the parsed
// list.
package consent

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/stardust/legion-agent/internal/plugin/manifest"
)

// ResolveCapabilities checks a proposed capability grant against what the
// plugin itself declares, and returns the set to record.
//
// The two sets must be EQUAL, not merely compatible. manifest.reconcileCapabilities
// (assemble.go) refuses any entry whose grant does not cover every declared
// capability, so a strict subset produces an entry the deployment can never
// load; extras are ignored there anyway. A plugin's declaration is not a menu.
//
// actor names whoever is asking, for the error message ("plugins grant",
// "POST /v1/plugins/{name}/grant"). subject names the input that carried the
// list ("--capabilities", "capabilities").
func ResolveCapabilities(actor string, granted []string, pm manifest.PluginManifest) ([]string, error) {
	for _, capability := range granted {
		if !slices.Contains(pm.Capabilities, capability) {
			return nil, fmt.Errorf("%s: names capability %q, which plugin %q does not declare in "+
				"plugin.json (it declares: %v); granting a capability the plugin did not ask for is "+
				"a config error, not generosity", actor, capability, pm.Name, pm.Capabilities)
		}
	}
	var missing []string
	for _, declared := range pm.Capabilities {
		if !slices.Contains(granted, declared) {
			missing = append(missing, declared)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: %v does not grant %v, which plugin %q declares in plugin.json; "+
			"a partial grant produces an entry the deployment can never load (every declared "+
			"capability must be granted, not a subset) -- name the complete list, or deny it "+
			"instead of half-authorizing it", actor, granted, missing, pm.Name)
	}
	return granted, nil
}

// ResolveAllowedHosts checks a proposed --allowed-hosts (or equivalent) list
// against pm.Network.AllowedHosts, the plugin's OWN declaration in
// plugin.json -- the same set manifest.AssembleSpec (assemble.go) intersects
// a grant's AllowedHosts against.
//
// Unlike ResolveCapabilities, this is deliberately NOT a set-equality check:
// AssembleSpec's own rule for hosts is a plain declared ∩ granted
// intersection, and an empty result there is explicitly legal (see
// AssembleSpec's own doc comment) -- there is no "every declared host must be
// granted" requirement anywhere in the manifest layer to enforce, and
// inventing one here would be exactly the kind of new validation concept
// this package must not invent on its own. So naming a strict subset of what
// the plugin declares is accepted as a legitimate narrowing.
//
// What IS checked is the direction that silently misfires otherwise: a host
// granted names that the plugin never declared is not an error anywhere else
// in this package, but AssembleSpec would drop it from the intersection at
// the next reload with nothing anywhere saying so, leaving the deployment
// recording a host that grants nothing and looks authoritative (a typo'd
// hostname that "succeeds" and then denies every outbound call). Refusing it
// by name here, before it is ever written, matches the "config error, not
// generosity" principle ResolveCapabilities already enforces.
//
// Comparison is case-insensitive, matching intersectPreserveOrder's own
// comparison for hosts (DNS names are case-insensitive).
//
// actor names whoever is asking, for the error message ("plugins grant",
// "POST /v1/plugins/{name}/grant").
func ResolveAllowedHosts(actor string, granted []string, pm manifest.PluginManifest) ([]string, error) {
	declared := make(map[string]struct{}, len(pm.Network.AllowedHosts))
	for _, host := range pm.Network.AllowedHosts {
		declared[strings.ToLower(host)] = struct{}{}
	}
	for _, host := range granted {
		if _, ok := declared[strings.ToLower(host)]; !ok {
			return nil, fmt.Errorf(`%s: names host %q, which plugin %q does not declare in plugin.json's `+
				`"network"."allowed_hosts" (it declares: %v); AssembleSpec would silently drop an undeclared `+
				`host from the grant at the next reload, so this is refused here instead`,
				actor, host, pm.Name, pm.Network.AllowedHosts)
		}
	}
	return granted, nil
}

// ResolveAllowedPaths is ResolveAllowedHosts for pm.Filesystem.AllowedPaths
// -- see that function's doc comment for the reasoning, which applies
// identically here. Paths are compared exactly, not case-insensitively,
// matching intersectPreserveOrder's own comparison (paths are not
// case-insensitive in general).
//
// actor names whoever is asking, for the error message ("plugins grant",
// "POST /v1/plugins/{name}/grant").
func ResolveAllowedPaths(actor string, granted []string, pm manifest.PluginManifest) ([]string, error) {
	declared := make(map[string]struct{}, len(pm.Filesystem.AllowedPaths))
	for _, path := range pm.Filesystem.AllowedPaths {
		declared[path] = struct{}{}
	}
	for _, path := range granted {
		if _, ok := declared[path]; !ok {
			return nil, fmt.Errorf(`%s: names path %q, which plugin %q does not declare in plugin.json's `+
				`"filesystem"."allowed_paths" (it declares: %v); AssembleSpec would silently drop an undeclared `+
				`path from the grant at the next reload, so this is refused here instead`,
				actor, path, pm.Name, pm.Filesystem.AllowedPaths)
		}
	}
	return granted, nil
}

// RefuseUnnamedAllowlist is the ONE rule shared by every caller that grants
// capabilities: install's --grant, grant's --capabilities, and an HTTP
// endpoint's equivalent body field alike. Without it, granting "http" (or
// "fs") while the plugin declares a non-empty "network"."allowed_hosts" (or
// "filesystem"."allowed_paths") in plugin.json, and naming none of those
// hosts/paths in this same call, would mount the plugin with that capability
// true and an allowlist that reaches nothing -- authoritative-looking,
// granting nothing, with nothing in the caller's own output saying why.
// Before this rule existed, install and grant enforced only one direction
// each of the same concept: install refused this shape outright, while
// grant's own resolvers only refused a NAMED host/path the plugin never
// declared and stayed silent when none was named at all. One shared
// function closes both directions the same way, instead of drifting the way
// two hand-written copies of one rule always eventually do.
//
// capabilities is the already set-equality-checked capability list about to
// be written; hosts and paths are what THIS call names (nil for install,
// which has no such flags at all). hostsRemedy and pathsRemedy are the exact
// next-step text to append -- different callers point the operator at
// different places, since not every caller accepts an allowed-hosts /
// allowed-paths input at all.
//
// This is deliberately keyed on the plugin's DECLARATION, not the effective
// allowlist AssembleSpec computes after intersecting it against what is
// granted: a plugin declaring "capabilities": ["http"] with an EMPTY
// "allowed_hosts" (or "fs" with no "allowed_paths") does NOT trip this
// guard, and must not -- that is a legitimate "reaches nothing by the
// plugin's own design" state no caller has any way to fix (naming any
// host/path there would itself be refused as undeclared), not the "operator
// forgot to name what the plugin asked for" state this guard exists to
// catch. A plugin declaring neither http nor fs is unaffected either way;
// the guard never evaluates for it.
//
// actor names whoever is asking, for the error message ("plugins grant",
// "POST /v1/plugins/{name}/grant"). subject names the input that carried the
// capability list ("--capabilities", "--grant", "capabilities").
func RefuseUnnamedAllowlist(actor, subject string, capabilities []string, pm manifest.PluginManifest,
	hosts, paths []string, hostsRemedy, pathsRemedy string) error {
	if slices.Contains(capabilities, "http") && len(pm.Network.AllowedHosts) > 0 && len(hosts) == 0 {
		return fmt.Errorf(`%s: %s names "http", but plugin %q declares "network"."allowed_hosts" in `+
			`plugin.json (%v); %s only fills capabilities, not allowed hosts, so this would authorize http `+
			`with an allowlist that reaches nothing -- %s`,
			actor, subject, pm.Name, pm.Network.AllowedHosts, subject, hostsRemedy)
	}
	if slices.Contains(capabilities, "fs") && len(pm.Filesystem.AllowedPaths) > 0 && len(paths) == 0 {
		return fmt.Errorf(`%s: %s names "fs", but plugin %q declares "filesystem"."allowed_paths" in `+
			`plugin.json (%v); %s only fills capabilities, not allowed paths, so this would authorize fs `+
			`with an allowlist that reaches nothing -- %s`,
			actor, subject, pm.Name, pm.Filesystem.AllowedPaths, subject, pathsRemedy)
	}
	return nil
}

// ReadDeploymentWithSnapshot reads and parses path exactly once, returning
// both the parsed Deployment and the raw bytes it was parsed from. The raw
// bytes are the "snapshot" RefuseDeploymentChanged later compares against.
//
// A caller that instead read the file a second time to take its snapshot
// (as install used to, before this fix) parses read #1 but compares a
// separate read #2 against a later read #3 -- a race between reads #1 and #2
// that an edit could land in and pass the comparison unnoticed. Reading once
// and keeping both the parse and the bytes it came from removes that window
// entirely.
func ReadDeploymentWithSnapshot(path string) (manifest.Deployment, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest.Deployment{}, nil, fmt.Errorf("read plugin deployment manifest %q: %w", path, err)
	}
	deployment, err := manifest.ParseDeployment(data)
	if err != nil {
		return manifest.Deployment{}, nil, fmt.Errorf("parse plugin deployment manifest %q: %w", path, err)
	}
	return deployment, data, nil
}

// RefuseDeploymentChanged re-reads manifestPath and compares it, byte for
// byte, against snapshot -- the bytes ReadDeploymentWithSnapshot captured
// earlier, before the caller went on to do something that can take a while
// (a remote package fetch for install and, on a cache miss, for grant too;
// even deny's plain read-modify-write has a window of its own, just a much
// shorter one). WriteDeployment is atomic but performs no compare-and-swap,
// so without this check a concurrent edit landing in that window would be
// silently reverted by a stale rewrite: both callers would report success,
// and the next `agent plugins reload` would quietly mount or unmount
// something nobody decided to. Refuse instead of overwriting it.
//
// This guard is shared so every caller -- install, grant, deny, and an HTTP
// endpoint alike -- carries the IDENTICAL guard instead of several
// separately worded ones: a guard that holds on one writer and not the
// other is not a guard, and two spellings of one guard is how they drift
// apart. actor (e.g. "plugins install", "plugins grant", "plugins deny",
// "POST /v1/plugins/{name}/grant") labels the error and names the command
// or endpoint an operator should re-run.
func RefuseDeploymentChanged(actor, manifestPath string, snapshot []byte) error {
	current, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("%s: re-read plugin deployment manifest %q before write: %w", actor, manifestPath, err)
	}
	if !bytes.Equal(snapshot, current) {
		// %s, not %q: on Windows a %q-quoted path doubles every backslash,
		// which would stop it matching a caller's raw path string built
		// straight from filepath.Join -- unlike the wrapped-error case just
		// above, there is no underlying *PathError here to carry a second,
		// unquoted copy of the path along for free.
		return fmt.Errorf(`%s: plugin deployment manifest %s changed while this command was running; refusing `+
			`to overwrite that edit with a stale copy -- re-run "agent %s" to apply on top of the current `+
			`state`, actor, manifestPath, actor)
	}
	return nil
}

// FindEntry returns the entry named name in dep, or an error naming both
// the requested name and every name that does exist -- the same shape
// manifest.UpdateEntry's own "no entry" error uses -- so a caller can check
// the plugin's own declared capabilities BEFORE attempting the UpdateEntry
// mutation that would otherwise report the identical "no such entry"
// failure only after a package load was already spent on it.
func FindEntry(dep manifest.Deployment, name string) (manifest.Entry, error) {
	names := make([]string, 0, len(dep.Plugins))
	for _, e := range dep.Plugins {
		names = append(names, e.Name)
		if e.Name == name {
			return e, nil
		}
	}
	existing := "(none)"
	if len(names) > 0 {
		existing = strings.Join(names, ", ")
	}
	return manifest.Entry{}, fmt.Errorf("no entry named %q exists in the deployment manifest; existing entries "+
		"are: %s", name, existing)
}
