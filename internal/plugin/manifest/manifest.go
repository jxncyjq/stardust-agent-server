// Package manifest parses and validates the two manifests that together
// describe a plugin's target state, without knowing anything about how a
// plugin is actually mounted (that is internal/plugin/host, wired in by a
// later task).
//
// The two manifests have different authors and different jobs, and mixing
// them up is the mistake this package doc exists to prevent:
//
//   - plugin.json travels with the compiled .wasm module. It is the PLUGIN
//     AUTHOR'S declaration: who I am, which host capabilities I need, which
//     tools I contribute, and how much resource I want. ParsePlugin reads it
//     into a PluginManifest.
//
//   - plugins.json belongs to the deployment. It is the OPERATOR'S target
//     state and authorization: which plugins to install, whether each is
//     enabled, which of the capabilities it asked for are actually granted,
//     and which of the tools it offers are actually accepted. ParseDeployment
//     reads it into a Deployment.
//
// Reconciling the two — checking that a plugin's declared sha256 matches its
// actual bytes, intersecting declared capabilities against granted ones,
// and turning the pair into a host.Spec — is Task 2 (assemble.go), not this
// file. Both parse functions here only turn bytes into validated structs:
// they check that the shape of each manifest is legal on its own terms, and
// refuse anything that is not, loudly and by name. Every unrecognized JSON
// field is one such refusal (see the DisallowUnknownFields calls below): a
// mistyped key in a manifest must fail parsing, not silently do nothing.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// pluginABIVersion is the only ABI version ParsePlugin currently accepts.
// There is exactly one guest ABI today (see internal/plugin/abi), so a
// manifest declaring anything else names a plugin this host cannot load.
const pluginABIVersion = 1

// sha256HexPattern matches a sha256 digest rendered as 64 lowercase or
// uppercase hex digits — the shape ParsePlugin requires of
// PluginManifest.SHA256. It does not verify the digest matches any actual
// file; that check needs the plugin's bytes and belongs to Task 2's
// LoadPackage.
var sha256HexPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// knownCapabilities is the complete set of host capability names a manifest
// may name, on either side: PluginManifest.Capabilities (what a plugin
// asks for) and GrantDecl.Capabilities (what a deployment grants). The six
// names map onto perm.Grant's six bools one for one (see Task 2); this
// package does not import perm; the names are duplicated here on purpose
// so that this package's tests only need to know the six strings, not the
// perm package.
var knownCapabilities = map[string]bool{
	"log":    true,
	"config": true,
	"kv":     true,
	"http":   true,
	"fs":     true,
	"tool":   true,
}

// PluginManifest is the plugin author's declaration, as read from
// plugin.json. It states an identity (Name, Version), the content digest
// (SHA256) of the .wasm module it travels with, which host capabilities it
// needs (Capabilities), what resources it wants (Limits), the network hosts
// and filesystem paths it needs those capabilities to reach (Network,
// Filesystem), and the tools it contributes (Tools).
//
// ParsePlugin is the only way to obtain a validated PluginManifest; every
// field below has already passed its shape check by the time a caller sees
// one.
type PluginManifest struct {
	Name         string     `json:"name"`
	Version      string     `json:"version"`
	ABI          int        `json:"abi"`
	SHA256       string     `json:"sha256"`
	Capabilities []string   `json:"capabilities"`
	Limits       Limits     `json:"limits"`
	Network      Network    `json:"network"`
	Filesystem   Filesystem `json:"filesystem"`
	Tools        []ToolDecl `json:"tools"`
}

// Limits is the resource envelope a plugin asks for. MaxMemoryPages and
// MaxInstances are both required to be positive (ParsePlugin rejects a
// manifest that leaves either at its zero value): host.NewRuntime panics on
// a zero page count, and host.Spec.MaxInstances below 1 leaves a plugin
// with no instance to ever serve a call. TimeoutMs carries no such
// requirement here — it is the plugin's requested per-call bound, and
// Task 2 is what reconciles it against the deployment's own limit.
type Limits struct {
	TimeoutMs      int    `json:"timeout_ms"`
	MaxMemoryPages uint32 `json:"max_memory_pages"`
	MaxInstances   int    `json:"max_instances"`
}

// Network is the set of hosts a plugin asks its http capability to be able
// to reach. It only has meaning together with a granted "http" capability;
// declaring it without that capability is legal here (this is a shape
// check, not a semantic one) but pointless.
type Network struct {
	AllowedHosts []string `json:"allowed_hosts"`
}

// Filesystem is the set of paths a plugin asks its fs capability to be able
// to reach. See Network's doc comment; the same relationship holds between
// this and the "fs" capability.
type Filesystem struct {
	AllowedPaths []string `json:"allowed_paths"`
}

// ToolDecl is one tool a plugin declares it contributes. Name, Group and a
// positive TimeoutMs are all required (ParsePlugin rejects a tool missing
// any of them): Group is what places the tool in the capability catalog —
// host.validateSpec refuses an ungrouped host.Spec.Tools entry for the same
// reason — and TimeoutMs is the only bound ever placed on a call into the
// guest, so a non-positive value would let a call run forever.
//
// host.validateSpec checks these same two fields again, later, against the
// host.Spec assembled from this declaration (Task 2). The check is
// deliberately duplicated: failing here, at manifest parse time, names the
// plugin.json field the author got wrong, which is far more actionable than
// a validateSpec error naming a Spec.Tools index the author never wrote.
type ToolDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Group       string         `json:"group"`
	RiskLevel   string         `json:"risk_level"`
	InputSchema map[string]any `json:"input_schema"`
	TimeoutMs   int            `json:"timeout_ms"`
	Sensitive   bool           `json:"sensitive"`
}

// Deployment is the operator's target state, as read from plugins.json: the
// complete list of plugins that should be installed. A plugin absent from
// Plugins is not installed, full stop — there is no separate "uninstalled"
// list.
//
// ParseDeployment is the only way to obtain a validated Deployment.
type Deployment struct {
	Plugins []Entry
}

// Entry is one plugin's target state and authorization within a
// Deployment: which plugin (Name identifies it, Source says where to load
// it from), whether it should be running at all (Enabled), what it is
// authorized to reach (Grant), which of its declared tools are accepted
// (Tools), and any operator-supplied configuration for it (Config, passed
// through unparsed — this package does not know a plugin's config schema).
type Entry struct {
	Name   string
	Source string

	// Enabled controls whether this entry's plugin should be running.
	// ParseDeployment parses the JSON "enabled" field as an optional bool
	// and normalizes it here: a plugins.json entry that omits "enabled"
	// defaults to true (an entry written into the target state is written
	// there to be installed), and only an explicit "enabled": false turns
	// it off. This is a contract-declared optional, not a fallback: the
	// default is documented here rather than assumed silently.
	Enabled bool

	Grant  GrantDecl
	Tools  []ToolAccept
	Config json.RawMessage
}

// GrantDecl is the capability authorization a deployment gives one plugin:
// which capability names it grants, and — mirroring PluginManifest's
// Network/Filesystem — which hosts and paths the http and fs capabilities
// may reach. Task 2 intersects this against the plugin's own declaration;
// this package only checks that every named capability is one of the six
// known names (see knownCapabilities).
type GrantDecl struct {
	Capabilities []string `json:"capabilities"`
	AllowedHosts []string `json:"allowed_hosts"`
	AllowedPaths []string `json:"allowed_paths"`
}

// ToolAccept is one tool a deployment accepts from a plugin. Name selects
// which of the plugin's declared ToolDecl entries this refers to
// (Task 2 rejects a name the plugin never declared). RiskLevel and
// Sensitive are pointers/optional overrides — Task 2 lets a deployment
// tighten a plugin's own declared risk level or sensitivity, never loosen
// it — and are left as the plugin's own values when absent, which is why
// Sensitive is a *bool: nil means "use the plugin's declaration", and only
// a present false or true is an override.
type ToolAccept struct {
	Name      string `json:"name"`
	RiskLevel string `json:"risk_level"`
	Sensitive *bool  `json:"sensitive"`
}

// ParsePlugin decodes and validates a plugin.json manifest. It refuses:
//
//   - a document with any field name it does not recognize, at any nesting
//     level (json.Decoder.DisallowUnknownFields);
//   - a missing Name or Version;
//   - an ABI other than 1;
//   - a SHA256 that is not exactly 64 hex digits (it is checked for shape
//     only here — matching it against an actual file's digest is Task 2);
//   - a Capabilities entry that is not one of the six known capability
//     names (log, config, kv, http, fs, tool);
//   - Limits.MaxMemoryPages == 0 or Limits.MaxInstances < 1;
//   - an empty Tools list (a plugin that contributes no tools has no
//     reason to be loaded — the same requirement host.Spec.Tools carries);
//   - any tool missing its Group, or with TimeoutMs <= 0.
//
// Every error names the offending field (and, for a per-tool violation, the
// tool's own name) rather than only reporting that parsing failed.
func ParsePlugin(data []byte) (PluginManifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var pm PluginManifest
	if err := dec.Decode(&pm); err != nil {
		return PluginManifest{}, fmt.Errorf("parse plugin manifest: %w", err)
	}
	if err := validatePlugin(pm); err != nil {
		return PluginManifest{}, err
	}
	return pm, nil
}

func validatePlugin(pm PluginManifest) error {
	if pm.Name == "" {
		return errors.New("parse plugin manifest: name is empty")
	}
	if pm.Version == "" {
		return fmt.Errorf("parse plugin manifest %q: version is empty", pm.Name)
	}
	if pm.ABI != pluginABIVersion {
		return fmt.Errorf("parse plugin manifest %q: abi is %d, want %d", pm.Name, pm.ABI, pluginABIVersion)
	}
	if !sha256HexPattern.MatchString(pm.SHA256) {
		return fmt.Errorf("parse plugin manifest %q: sha256 %q is not 64 hex digits", pm.Name, pm.SHA256)
	}
	if err := validateCapabilities(fmt.Sprintf("parse plugin manifest %q", pm.Name), pm.Capabilities); err != nil {
		return err
	}
	if pm.Limits.MaxMemoryPages == 0 {
		return fmt.Errorf("parse plugin manifest %q: limits.max_memory_pages is 0; it must be positive "+
			"(host.NewRuntime panics on a zero page count)", pm.Name)
	}
	if pm.Limits.MaxInstances < 1 {
		return fmt.Errorf("parse plugin manifest %q: limits.max_instances is %d, want >= 1",
			pm.Name, pm.Limits.MaxInstances)
	}
	if len(pm.Tools) == 0 {
		return fmt.Errorf("parse plugin manifest %q: tools is empty; a plugin exists to contribute tools",
			pm.Name)
	}
	for i, tool := range pm.Tools {
		if tool.Name == "" {
			return fmt.Errorf("parse plugin manifest %q: tools[%d] has no name", pm.Name, i)
		}
		if tool.Group == "" {
			return fmt.Errorf("parse plugin manifest %q: tool %q has no group; an unplaced tool cannot be "+
				"listed in the capability catalog", pm.Name, tool.Name)
		}
		if tool.TimeoutMs <= 0 {
			return fmt.Errorf("parse plugin manifest %q: tool %q has timeout_ms %d, want > 0; it is the only "+
				"bound ever placed on a call into the guest", pm.Name, tool.Name, tool.TimeoutMs)
		}
	}
	return nil
}

// rawEntry mirrors Entry's JSON shape exactly, except Enabled is a pointer:
// decoding into this intermediate type is what lets ParseDeployment tell
// "enabled omitted" (nil, defaults to true) apart from "enabled": false
// (explicit, stays false) before it ever constructs the public Entry (see
// Entry.Enabled's doc comment for why that distinction is not a fallback).
type rawEntry struct {
	Name    string          `json:"name"`
	Source  string          `json:"source"`
	Enabled *bool           `json:"enabled"`
	Grant   GrantDecl       `json:"grant"`
	Tools   []ToolAccept    `json:"tools"`
	Config  json.RawMessage `json:"config"`
}

// rawDeployment mirrors Deployment's JSON shape for decoding; see rawEntry.
type rawDeployment struct {
	Plugins []rawEntry `json:"plugins"`
}

// ParseDeployment decodes and validates a plugins.json manifest. It refuses:
//
//   - a document with any field name it does not recognize, at any nesting
//     level (json.Decoder.DisallowUnknownFields);
//   - an entry missing its Name or Source;
//   - two entries sharing the same Name (the target state must name each
//     plugin unambiguously — Task 3's Loader keys its convergence off Name);
//   - a Grant.Capabilities entry that is not one of the six known
//     capability names, for the same reason ParsePlugin checks
//     PluginManifest.Capabilities: both feed the same six perm.Grant bools.
//
// Reconciling an entry's Grant/Tools against the plugin's own
// PluginManifest — checking that a granted capability was actually
// requested, or that an accepted tool was actually declared — is Task 2;
// this function only checks plugins.json's own shape.
func ParseDeployment(data []byte) (Deployment, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw rawDeployment
	if err := dec.Decode(&raw); err != nil {
		return Deployment{}, fmt.Errorf("parse deployment manifest: %w", err)
	}

	seen := make(map[string]struct{}, len(raw.Plugins))
	entries := make([]Entry, 0, len(raw.Plugins))
	for i, re := range raw.Plugins {
		if re.Name == "" {
			return Deployment{}, fmt.Errorf("parse deployment manifest: plugins[%d] has no name", i)
		}
		if re.Source == "" {
			return Deployment{}, fmt.Errorf("parse deployment manifest: plugin %q has no source", re.Name)
		}
		if _, dup := seen[re.Name]; dup {
			return Deployment{}, fmt.Errorf("parse deployment manifest: plugin %q appears twice; "+
				"the target state must name each plugin unambiguously", re.Name)
		}
		seen[re.Name] = struct{}{}

		if err := validateCapabilities(
			fmt.Sprintf("parse deployment manifest: plugin %q grant", re.Name), re.Grant.Capabilities,
		); err != nil {
			return Deployment{}, err
		}

		enabled := true
		if re.Enabled != nil {
			enabled = *re.Enabled
		}
		entries = append(entries, Entry{
			Name:    re.Name,
			Source:  re.Source,
			Enabled: enabled,
			Grant:   re.Grant,
			Tools:   re.Tools,
			Config:  re.Config,
		})
	}
	return Deployment{Plugins: entries}, nil
}

// validateCapabilities rejects any name in caps that is not one of the six
// known capability names, naming the offending value. context prefixes the
// error so the same helper serves both ParsePlugin's PluginManifest.Capabilities
// and ParseDeployment's GrantDecl.Capabilities without losing which one
// failed.
func validateCapabilities(context string, caps []string) error {
	for _, c := range caps {
		if !knownCapabilities[c] {
			return fmt.Errorf("%s: unknown capability %q; known capabilities are log, config, kv, http, fs, tool",
				context, c)
		}
	}
	return nil
}
