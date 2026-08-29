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
	"net/url"
	"regexp"
	"strings"

	"github.com/stardust/legion-agent/internal/plugin/perm"
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

// digestPattern matches Entry.Digest's required shape: the literal prefix
// "sha256:" followed by 64 lowercase or uppercase hex digits. Only sha256
// is accepted (see Entry.Digest's doc comment for why a second algorithm
// is deliberately not supported): a second algorithm would be a second way
// for a remote source to be signed weakly.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)

// remoteSchemePattern matches an RFC 3986 URI scheme prefix: a letter, then
// letters/digits/"+"/"-"/".", then "://". ParseDeployment uses it to catch
// a Source naming some scheme other than http/https (file://, ftp://,
// ssh://, ...) before that Source can fall through to local-path handling.
// Without this check, a Source like "file:///etc/passwd" would not match
// the "https://"/"http://" prefixes IsRemote checks for, and so would look
// exactly like a relative local path to everything downstream — including
// internal/plugin/loader's packageDir, which would filepath.Join it onto
// the deployment root. That is a silent semantic slip, not a refusal, so
// this pattern exists to make the refusal explicit and name the scheme.
var remoteSchemePattern = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.-]*)://`)

// remoteScheme extracts source's URI scheme (via remoteSchemePattern) and
// lowercases it, reporting ok == false when source names no scheme at all
// (an ordinary local path has no "://"). It is the single place that
// decides what scheme a Source names: IsRemote, IsInsecureSource,
// rejectForeignScheme and validateEntrySource (via IsRemote) all call this
// instead of separately re-deriving the answer, so they cannot disagree
// with each other about a mixed-case scheme such as "HTTPS://...". RFC 3986
// treats a scheme as case-insensitive, and rejectForeignScheme already
// case-folds before comparing; IsRemote/IsInsecureSource must fold the same
// way or a source rejectForeignScheme accepts as "https" could still be
// classified "local" by IsRemote, which is the exact silent slip
// remoteSchemePattern's doc comment (and Source's) warns against.
func remoteScheme(source string) (scheme string, ok bool) {
	m := remoteSchemePattern.FindStringSubmatch(source)
	if m == nil {
		return "", false
	}
	return strings.ToLower(m[1]), true
}

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
// Filesystem), the tools it contributes (Tools), and the tools of other
// plugins it calls into (Requires).
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

	// ConfigSchema optionally describes the shape of the deployment-side
	// configuration this plugin expects (the "config" of its plugins.json
	// entry). It is a SUBSET of JSON Schema — see ParseConfigSchema for the
	// exact keyword list — and it is what turns a typo in plugins.json from a
	// runtime surprise inside the guest into a load-time refusal that names
	// the field.
	//
	// Absent means "this plugin makes no claim about its configuration", which
	// is what every plugin written before this field existed says: its config
	// is then passed through unchecked, exactly as before.
	//
	// It carries ",omitempty" because this struct is also MARSHALLED — tests
	// and packaging tools build a plugin.json from it — and a nil RawMessage
	// would otherwise be written as the literal null, which ParsePlugin would
	// then have to read as "a schema that declares nothing", i.e. an error.
	ConfigSchema json.RawMessage `json:"config_schema,omitempty"`

	// Extensions names the host-side seams this plugin wants to be consulted
	// at (perm.Extension). It is the OTHER DIRECTION from Capabilities:
	// a capability gates the guest calling the host and is enforced at link
	// time, while an extension gates the HOST CALLING THE GUEST, which no
	// import list can express — enforcement is that the host never registers
	// the plugin at that seam.
	//
	// Declaring is not receiving: the deployment grants extensions separately
	// (GrantDecl.Extensions), and AssembleSpec passes on only the
	// intersection. Absent means "this plugin participates in nothing beyond
	// contributing tools", which is what every plugin written before this
	// field existed says.
	Extensions []string `json:"extensions,omitempty"`

	// Requires lists the names of tools this plugin calls through
	// call_tool: external dependencies on tools other plugins contribute.
	// It is NOT a second capability list — that role belongs to
	// Capabilities, which is checked at load time and refuses the load
	// outright when ungranted. Requires is different in kind: it names
	// runtime call targets, and an unsatisfied entry (the tool's
	// contributor is absent or gone) is a suspendable, temporary state —
	// this plugin still loads, but the calls it makes into that name fail
	// or suspend until something contributes it. Resolving Requires
	// against the running plugin set is a later phase's job (dependency
	// graph, suspension); this package only validates the declaration's
	// own shape.
	//
	// Requires is a contract-declared optional: an absent "requires" key
	// decodes to a nil/empty slice and means no external dependencies,
	// not an unset field waiting to be filled in. ParsePlugin rejects an
	// empty-string entry, a name repeated more than once, and a name this
	// same manifest already contributes in Tools — a plugin cannot
	// require its own tool. That last case would always trivially
	// "satisfy" (a plugin resolves its own contributed name) while adding
	// a self-loop to the dependency graph that pollutes the cycle
	// detection a later phase performs on it.
	Requires []string `json:"requires"`

	// ProvidesServices names the CAPABILITIES this plugin can act as — an
	// "issue-tracker", a "calendar" — rather than the tools it contributes.
	// A consumer binds to one of these names instead of to somebody's
	// specific tool name, so swapping the implementation does not mean
	// editing every consumer (see specs/2026-08-29-plugin-service-seam-design.md).
	//
	// Claiming a name is FIRST COME, FIRST SERVED, and a name is held by
	// exactly one plugin: a second plugin claiming a name already held fails
	// to activate, naming the holder. It neither steps aside silently (which
	// would leave "who provides this?" unanswerable while both report loaded)
	// nor takes over silently (which is the risk that ruled out letting a
	// plugin displace another's capability by being installed).
	//
	// Contract-declared optional: an absent key means this plugin takes no
	// part in the service seam.
	ProvidesServices []string `json:"provides_services,omitempty"`

	// RequiresServices names the capabilities this plugin needs somebody to
	// provide. It feeds the same dependency convergence Requires does: no
	// provider means this plugin is SUSPENDED, not unloaded, and a provider
	// arriving later resumes it.
	//
	// Service names and tool names are two namespaces: a service may be named
	// like a tool without conflicting, because the model never sees service
	// names and nothing dispatches through them.
	RequiresServices []string `json:"requires_services,omitempty"`
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
// host.validateSpec checks these same three fields again, later — Name,
// Group and Timeout — against the host.Spec assembled from this declaration
// (Task 2). The check is deliberately duplicated: failing here, at manifest
// parse time, names the plugin.json field the author got wrong, which is far
// more actionable than a validateSpec error naming a Spec.Tools index the
// author never wrote.
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
	Name string

	// Source names where this entry's package comes from. It is one of
	// two things, discriminated by scheme, case-insensitively (see
	// IsRemote):
	//
	//   - a Source beginning with "https://" or "http://" (in any letter
	//     case — "HTTPS://..." counts the same as "https://...", per RFC
	//     3986's scheme case-insensitivity) is a remote source: the
	//     package is fetched from that URL (fetching itself is a later
	//     task — this package only validates the shape). It must be
	//     paired with Digest (see Digest's doc comment).
	//   - anything else is a package directory path relative to the
	//     deployment root (existing semantics, unchanged). It must NOT be
	//     paired with Digest.
	//
	// A Source naming any other URL scheme (file://, ftp://, ssh://, ...)
	// is neither of the above: ParseDeployment refuses it by name, naming
	// the scheme, rather than letting it silently fall through as a local
	// path (see remoteSchemePattern's doc comment for why that matters —
	// filepath.Join does not know a scheme prefix is not part of a
	// relative path).
	Source string

	// Digest is the sha256 content digest a remote Source's fetched bytes
	// must match, formatted as "sha256:" followed by 64 hex digits
	// (digestPattern). It is the trust anchor for a remote package: it
	// guards "should these bytes be accepted" before anything reaches
	// disk, independently of the plugin's signature, which guards a
	// separate question ("should this package load") at load time.
	//
	// Digest is required on a remote Entry (both http:// and https://
	// alike — the weaker plaintext channel needs it no less than the
	// encrypted one) and forbidden on a local Entry: a local package's
	// trust comes from its signature and the operator's own control of
	// the deployment root's disk, and a Digest field that is never
	// checked would mislead a reader into thinking it is.
	//
	// Only sha256 is accepted. This package does not read
	// plugins.allow_insecure_sources or any other policy — whether an
	// entry's insecure http:// source is actually permitted is decided at
	// assembly (a later task); this package only reports the fact via
	// IsInsecureSource.
	Digest string

	// Enabled controls whether this entry's plugin should be running.
	// ParseDeployment parses the JSON "enabled" field as an optional bool
	// and normalizes it here: a plugins.json entry that omits "enabled"
	// defaults to true (an entry written into the target state is written
	// there to be installed), and only an explicit "enabled": false turns
	// it off. This is a contract-declared optional, not a fallback: the
	// default is documented here rather than assumed silently.
	Enabled bool

	// Grant is this entry's capability authorization: which of the plugin's
	// declared capabilities are actually granted, and which hosts/paths its
	// http and fs capabilities may reach. It is always a value, never absent
	// on its own — GrantStated (below) is the separate field that tells an
	// entry someone has actually recorded a grant decision for (even an
	// empty one) apart from one nobody ever has.
	Grant GrantDecl

	// GrantStated reports whether plugins.json's "grant" key was present for
	// this entry at all, independently of what it contained.
	// ParseDeployment sets it true whenever the raw JSON carried a "grant"
	// block — even an explicit empty one, {"capabilities": []} — and false
	// when the key was absent entirely, mirroring how rawEntry.Enabled's own
	// *bool intermediate distinguishes "omitted" from "explicit zero value"
	// (see that field's doc comment) for exactly the same reason: decoding
	// straight into a bare GrantDecl could never tell "nobody wrote a grant
	// block" apart from "somebody wrote an empty one".
	//
	// GrantStated answers "did anyone ever record a grant decision about
	// this plugin", not "is any capability granted right now" — those are
	// different questions, and answering the second when asked the first is
	// exactly the bug this field exists to prevent. A pure-compute plugin
	// can legitimately declare zero capabilities and still have
	// GrantStated true (an operator explicitly decided its grant is empty —
	// see `agent plugins deny`, which sets it while clearing Capabilities),
	// while an entry `agent plugins install` just wrote has GrantStated
	// false with an equally empty Grant.Capabilities (see DraftEntry) —
	// nobody has decided anything about it yet. Checking
	// len(Grant.Capabilities) == 0 in place of GrantStated would conflate
	// the two: an operator who hand-disabled an already-authorized,
	// legitimately zero-capability plugin would be told to go authorize it
	// again.
	GrantStated bool

	Tools  []ToolAccept
	Config json.RawMessage
}

// IsRemote reports whether e.Source names a remote package location rather
// than a local one: a Source whose scheme is "https" or "http" — matched
// case-insensitively via remoteScheme, so "HTTPS://..." counts the same as
// "https://..." — is remote; everything else, including a Source naming
// some other URL scheme, is not (see Source's doc comment for why a
// foreign scheme is refused by ParseDeployment rather than reported as
// remote here). IsRemote, IsInsecureSource and rejectForeignScheme all
// derive their answer from the same remoteScheme helper so they cannot
// disagree about what a given Source's scheme is.
func (e Entry) IsRemote() bool {
	scheme, ok := remoteScheme(e.Source)
	return ok && (scheme == "http" || scheme == "https")
}

// IsInsecureSource reports whether e is a plaintext remote source
// ("http", as opposed to "https", matched case-insensitively — see
// IsRemote). It carries no policy of its own — whether an insecure source
// is actually permitted is plugins.allow_insecure_sources, read only at
// assembly time by a later task — this method only states the fact so
// that layer has something to decide on.
//
// Digest still guards byte integrity end to end even over http://, so a
// man-in-the-middle cannot substitute different bytes; what plaintext
// loses is confidentiality (which plugin is being fetched, and from
// where, is observable on the wire) and availability (the connection can
// be blocked, or fed a stale-but-legitimately-signed version indefinitely).
func (e Entry) IsInsecureSource() bool {
	scheme, ok := remoteScheme(e.Source)
	return ok && scheme == "http"
}

// RemoteURL parses e.Source as a URL. It returns an error if e is not a
// remote entry (see IsRemote), if the URL fails to parse, if the URL
// carries userinfo (as in "https://user:pass@host/...") — credentials
// must not appear in a plugins.json manifest, which is expected to be
// committed to a git repository — or if the URL's host is empty (as in
// "https:///path"): a remote source with no host names nothing to fetch
// from, and this layer's job is to refuse a malformed source early with a
// clear name rather than let it surface later as an unrelated dial
// failure. All three checks apply to http:// and https:// alike.
func (e Entry) RemoteURL() (*url.URL, error) {
	if !e.IsRemote() {
		return nil, fmt.Errorf("plugin %q: source %q is not a remote source", e.Name, e.Source)
	}
	u, err := url.Parse(e.Source)
	if err != nil {
		return nil, fmt.Errorf("plugin %q: source %q is not a valid URL: %w", e.Name, e.Source, err)
	}
	if u.User != nil {
		return nil, fmt.Errorf("plugin %q: source %q carries userinfo; credentials must not appear in a "+
			"deployment manifest", e.Name, e.Source)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("plugin %q: source %q has no host", e.Name, e.Source)
	}
	return u, nil
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

	// Extensions are the host-side seams this deployment lets the plugin be
	// consulted at, and it is a SUBSET of what the plugin declared — unlike
	// Capabilities, which must match the declaration exactly.
	//
	// The difference is deliberate and load-bearing: a partial capability
	// grant produces an entry that can never load, while "you may observe,
	// but you may not decide" is a sentence a deployment must be able to say.
	// Do not "simplify" these two into one rule.
	Extensions []string `json:"extensions,omitempty"`
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
//   - any tool missing its Group, or with TimeoutMs <= 0;
//   - two tools sharing the same Name (host.validateSpec would reject this
//     too, later and less actionably, against the assembled host.Spec);
//   - a Requires entry that is empty, repeated, or names a tool this same
//     manifest already contributes in Tools (see PluginManifest.Requires).
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
	if len(pm.Extensions) > 0 {
		// Parsed for its rules only (unknown name, repeat); the value is
		// reconciled against the deployment's grant in AssembleSpec.
		if _, err := perm.ParseExtensions(pm.Extensions); err != nil {
			return fmt.Errorf("parse plugin manifest %q: extensions: %w", pm.Name, err)
		}
	}
	if hasConfigSchema(pm.ConfigSchema) {
		// The schema is the PLUGIN AUTHOR's declaration, so a broken one is
		// refused when the package is read rather than when a deployment
		// happens to write a config: turning an author's mistake into an
		// operator's mystery helps nobody.
		if _, err := ParseConfigSchema(pm.ConfigSchema); err != nil {
			return fmt.Errorf("parse plugin manifest %q: %w", pm.Name, err)
		}
	}
	if len(pm.Tools) == 0 {
		return fmt.Errorf("parse plugin manifest %q: tools is empty; a plugin exists to contribute tools",
			pm.Name)
	}
	seenTools := make(map[string]struct{}, len(pm.Tools))
	for i, tool := range pm.Tools {
		if strings.TrimSpace(tool.Name) == "" {
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
		if _, dup := seenTools[tool.Name]; dup {
			return fmt.Errorf("parse plugin manifest %q: tool %q claimed twice; one name is one tool, and the "+
				"second registration would only be caught later, less actionably, by host.validateSpec",
				pm.Name, tool.Name)
		}
		seenTools[tool.Name] = struct{}{}
	}
	if err := validateServiceNames(pm.Name, "provides_services", pm.ProvidesServices); err != nil {
		return err
	}
	if err := validateServiceNames(pm.Name, "requires_services", pm.RequiresServices); err != nil {
		return err
	}
	if err := validateNoSelfService(pm.Name, pm.ProvidesServices, pm.RequiresServices); err != nil {
		return err
	}
	if err := validateRequires(pm.Name, pm.Requires, seenTools); err != nil {
		return err
	}
	return nil
}

// validateRequires rejects a PluginManifest.Requires that is not fit to
// feed a later dependency graph: an empty entry (index named, since an
// empty string carries no name of its own to point at), a name repeated
// more than once, or a name contributedTools already holds — the plugin's
// own Tools, which requiring would create a self-loop (see
// PluginManifest.Requires's doc comment for why that is rejected here
// rather than left for the graph builder to discover).
func validateRequires(name string, requires []string, contributedTools map[string]struct{}) error {
	seen := make(map[string]struct{}, len(requires))
	for i, r := range requires {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("parse plugin manifest %q: requires[%d] is empty", name, i)
		}
		if _, dup := seen[r]; dup {
			return fmt.Errorf("parse plugin manifest %q: requires %q claimed twice", name, r)
		}
		seen[r] = struct{}{}
		if _, self := contributedTools[r]; self {
			return fmt.Errorf("parse plugin manifest %q: requires %q, which this plugin already contributes "+
				"itself in tools; a plugin cannot require its own tool (it would always trivially resolve while "+
				"adding a self-loop that pollutes cycle detection)", name, r)
		}
	}
	return nil
}

// validateServiceNames rejects a service list that is not fit to feed the
// dependency graph: an empty entry (named by index, since an empty string has
// no name to point at) or a name claimed twice.
func validateServiceNames(pluginName, field string, services []string) error {
	seen := make(map[string]struct{}, len(services))
	for i, service := range services {
		if strings.TrimSpace(service) == "" {
			return fmt.Errorf("parse plugin manifest %q: %s[%d] is empty", pluginName, field, i)
		}
		if _, dup := seen[service]; dup {
			return fmt.Errorf("parse plugin manifest %q: %s %q claimed twice", pluginName, field, service)
		}
		seen[service] = struct{}{}
	}
	return nil
}

// validateNoSelfService rejects a plugin that requires a service it provides
// itself — the same rule Requires has for tools, for the same reason: it
// always trivially resolves while adding a self-loop that pollutes the cycle
// detection the dependency graph performs.
func validateNoSelfService(pluginName string, provides, requires []string) error {
	provided := make(map[string]struct{}, len(provides))
	for _, service := range provides {
		provided[service] = struct{}{}
	}
	for _, service := range requires {
		if _, self := provided[service]; self {
			return fmt.Errorf("parse plugin manifest %q: requires_services %q, which this plugin provides "+
				"itself; a plugin cannot require its own service (it would always trivially resolve while "+
				"adding a self-loop that pollutes cycle detection)", pluginName, service)
		}
	}
	return nil
}

// rawEntry mirrors Entry's JSON shape exactly, except Enabled and Grant are
// pointers: decoding into this intermediate type is what lets ParseDeployment
// tell "enabled omitted" (nil, defaults to true) apart from "enabled": false
// (explicit, stays false), and — the identical trick, for the identical
// reason — "grant omitted" (nil, Entry.GrantStated stays false: nobody has
// ever recorded an authorization decision) apart from an explicit but empty
// "grant": {} (non-nil, GrantStated becomes true even though its
// Capabilities is empty). Both distinctions are made before rawEntry ever
// constructs the public Entry (see Entry.Enabled's and Entry.GrantStated's
// own doc comments for why neither is a fallback).
type rawEntry struct {
	Name    string          `json:"name"`
	Source  string          `json:"source"`
	Digest  string          `json:"digest"`
	Enabled *bool           `json:"enabled"`
	Grant   *GrantDecl      `json:"grant"`
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
//   - a Source naming a URL scheme other than http/https (file://, ftp://,
//     ssh://, ...), naming the offending scheme (see remoteSchemePattern);
//   - a remote entry (Source beginning "https://" or "http://") with no
//     Digest, or one whose Digest is not "sha256:" followed by 64 hex
//     digits, or whose Source fails to parse as a URL, or whose URL
//     carries userinfo (see Entry.Digest and Entry.RemoteURL);
//   - a local entry (any other Source) that carries a Digest — a field
//     that would never be checked, which would mislead a reader into
//     thinking it is (see Entry.Digest).
//
// Reconciling an entry's Grant/Tools against the plugin's own
// PluginManifest — checking that a granted capability was actually
// requested, or that an accepted tool was actually declared — and actually
// fetching a remote entry's bytes, are later tasks; this function only
// checks plugins.json's own shape.
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

		// grantStated is Entry.GrantStated: whether the "grant" key was
		// present at all, not whether it named anything. See rawEntry's and
		// Entry.GrantStated's doc comments.
		grantStated := re.Grant != nil
		grant := GrantDecl{}
		if grantStated {
			grant = *re.Grant
		}
		if err := validateCapabilities(
			fmt.Sprintf("parse deployment manifest: plugin %q grant", re.Name), grant.Capabilities,
		); err != nil {
			return Deployment{}, err
		}

		if err := rejectForeignScheme(re.Name, re.Source); err != nil {
			return Deployment{}, err
		}

		enabled := true
		if re.Enabled != nil {
			enabled = *re.Enabled
		}
		entry := Entry{
			Name:        re.Name,
			Source:      re.Source,
			Digest:      re.Digest,
			Enabled:     enabled,
			Grant:       grant,
			GrantStated: grantStated,
			Tools:       re.Tools,
			Config:      re.Config,
		}

		if err := validateEntrySource(entry); err != nil {
			return Deployment{}, err
		}

		entries = append(entries, entry)
	}
	return Deployment{Plugins: entries}, nil
}

// rejectForeignScheme refuses a source naming a URL scheme other than
// http/https (file://, ftp://, ssh://, ...), naming the offending scheme.
// It must run before an Entry is classified by IsRemote: without this
// check, a scheme-bearing source that is not http/https would simply be
// IsRemote() == false and fall through as an ordinary local path, which is
// the silent semantic slip Source's doc comment warns against. It shares
// the remoteScheme helper with IsRemote/IsInsecureSource so this gate and
// that classification can never disagree about what scheme a source names.
func rejectForeignScheme(name, source string) error {
	scheme, ok := remoteScheme(source)
	if !ok {
		return nil
	}
	if scheme == "http" || scheme == "https" {
		return nil
	}
	return fmt.Errorf("parse deployment manifest: plugin %q source %q names scheme %q; only http and https "+
		"are supported remote schemes (it is refused here rather than treated as a local path, which would "+
		"let it be joined onto the deployment root as if it were a relative directory)", name, source, scheme)
}

// validateEntrySource enforces Entry.Digest's pairing rules against an
// already-classified Entry (see IsRemote): a remote entry must carry a
// Digest matching digestPattern and a URL that RemoteURL accepts (parses,
// no userinfo); a local entry must carry no Digest at all.
func validateEntrySource(e Entry) error {
	if !e.IsRemote() {
		if e.Digest != "" {
			return fmt.Errorf("parse deployment manifest: plugin %q has digest %q but source %q is a local "+
				"path; a local package's trust comes from its signature and the operator's own control of "+
				"the deployment root, not a digest that would never be checked", e.Name, e.Digest, e.Source)
		}
		return nil
	}

	if e.Digest == "" {
		return fmt.Errorf("parse deployment manifest: plugin %q source %q is remote and has no digest; a "+
			"remote source must carry a sha256 digest that guards byte integrity end to end — this applies "+
			"equally to http:// and https://, since the weaker plaintext channel needs it no less", e.Name, e.Source)
	}
	if !digestPattern.MatchString(e.Digest) {
		return fmt.Errorf("parse deployment manifest: plugin %q digest %q is not \"sha256:\" followed by 64 "+
			"hex digits", e.Name, e.Digest)
	}
	if _, err := e.RemoteURL(); err != nil {
		return fmt.Errorf("parse deployment manifest: %w", err)
	}
	return nil
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
