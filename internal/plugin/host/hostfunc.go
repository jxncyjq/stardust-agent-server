package host

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// The names of the host functions the `legion` module (abi.HostModuleName)
// exposes to a guest. They are wire contract: renaming one breaks every
// already-built plugin, so they live here as constants rather than as literals
// at their registration sites.
const (
	funcLog         = "log"
	funcConfigGet   = "config_get"
	funcKVGet       = "kv_get"
	funcKVPut       = "kv_put"
	funcHTTPRequest = "http_request"
	funcReadFile    = "read_file"
	funcCallTool    = "call_tool"
)

// The capability names a deployment grants, as they appear in a plugin
// manifest and in the errors this file produces.
const (
	capLog    = "log"
	capConfig = "config"
	capKV     = "kv"
	capHTTP   = "http"
	capFS     = "fs"
	capTool   = "tool"
)

// capabilityFuncs is the single table mapping each capability to the host
// functions it provides. BuildHostModule registers from it and CheckImports
// resolves imports against it, so the set a guest may import can never drift
// away from the set the host registers.
var capabilityFuncs = []struct {
	capability string
	granted    func(perm.Grant) bool
	funcs      []string
}{
	{capability: capLog, granted: func(g perm.Grant) bool { return g.Log }, funcs: []string{funcLog}},
	{capability: capConfig, granted: func(g perm.Grant) bool { return g.Config }, funcs: []string{funcConfigGet}},
	{capability: capKV, granted: func(g perm.Grant) bool { return g.KV }, funcs: []string{funcKVGet, funcKVPut}},
	{capability: capHTTP, granted: func(g perm.Grant) bool { return g.HTTP }, funcs: []string{funcHTTPRequest}},
	{capability: capFS, granted: func(g perm.Grant) bool { return g.FS }, funcs: []string{funcReadFile}},
	{capability: capTool, granted: func(g perm.Grant) bool { return g.Tool }, funcs: []string{funcCallTool}},
}

// KVStore is the plugin-private key/value store behind the kv capability.
//
// Namespacing is the host's job, not the guest's: the keys passed to a
// KVStore are already qualified with the calling plugin's name, so an
// implementation stores and reads them verbatim and two plugins can never
// collide on (or read) each other's keys.
//
// Get's found result is a genuine contract-optional: "this plugin never wrote
// that key" is a normal answer, distinct from err != nil, which means the
// store itself failed.
type KVStore interface {
	Get(ctx context.Context, key string) (value string, found bool, err error)
	Put(ctx context.Context, key, value string) error
}

// Deps carries everything the granted host capabilities need in order to do
// their work. Every field is required by at least one capability and unused by
// the others; BuildHostModule checks exactly the ones the Grant asks for and
// refuses to build a module whose granted capability would be crippled.
type Deps struct {
	// PluginName identifies the plugin this module is built for. It qualifies
	// the plugin's kv keys and becomes the "plugin:<name>" call origin on tool
	// calls the plugin makes, so it is required whenever anything is granted.
	PluginName string

	// Logger receives the guest's own log calls and — because a host function
	// cannot return an error to the host process — every host-side denial and
	// telemetry failure. It is required whenever anything is granted, not only
	// for the log capability.
	Logger *slog.Logger

	// Config is the deployment-side configuration for this plugin, returned
	// verbatim by config_get. It must be valid JSON (BuildHostModule checks);
	// a plugin with no configuration is given an explicit empty object rather
	// than nothing at all.
	Config json.RawMessage

	// KV backs kv_get / kv_put.
	KV KVStore

	// HTTP performs http_request's outbound calls. Passing a client with a
	// timeout (and, where the deployment needs one, an SSRF-guarded dialer) is
	// the caller's decision, not this package's.
	//
	// The redirect policy is NOT the caller's decision: BuildHostModule copies
	// the client and installs a CheckRedirect that re-validates every hop
	// against Grant.AllowedHosts, because a client that follows redirects
	// anywhere would walk straight out of the allowlist. The client passed here
	// is left untouched, and its own CheckRedirect (if any) still runs after
	// that check.
	HTTP *http.Client

	// FS is the workspace boundary read_file resolves paths against. This is
	// the repository's one path check — it closes symlink escapes and the
	// Windows device-name / alternate-data-stream spellings — and read_file
	// runs it before consulting Grant.AllowedPaths.
	FS port.WorkspacePathGuard

	// Events receives a plugin/call_failed runtime event for every host-side
	// denial, so a refused plugin is visible in the event stream and not only
	// in the guest's own error handling. Required by the http and fs
	// capabilities, which are the ones that can deny.
	Events port.EventBus

	// Tools executes call_tool's calls. Plugin tool calls go through
	// Registry.Execute exactly like the model's own, so permissions, policy,
	// guardrails, timeouts and audit stay on the one path.
	Tools *tool.Registry

	// Agent is the identity Registry.Execute evaluates permissions and policy
	// against for a plugin's tool calls. Its ID must be set: an empty identity
	// would silently be evaluated as "some agent with no role".
	Agent domain.Agent
}

// BuildHostModule instantiates the `legion` host module (abi.HostModuleName)
// on rt with exactly the host functions g authorizes.
//
// An ungranted capability is ABSENT from the module — not registered and
// refusing at run time. A guest that imports it therefore fails to
// instantiate, which is a link-time fault the host can explain before it
// happens (see CheckImports) rather than a runtime surprise. The capability
// flags are only half of the enforcement, though: being granted http says
// nothing about which hosts are reachable, so the granted functions check
// their arguments against Grant.AllowedHosts / Grant.AllowedPaths on every
// call. When http is granted, deps.HTTP is copied and given a redirect policy
// that applies the same allowlist to every hop (see redirectGuardedClient), so
// the argument check cannot be walked around with a Location header.
//
// Every capability g grants must have its dependencies present in deps.
// A granted capability with a missing dependency is an assembly-time
// invariant violation, and BuildHostModule fails naming both the capability
// and the field, rather than registering a function that cannot do its job.
//
// The returned api.Module is owned by rt: closing the runtime closes it.
func BuildHostModule(ctx context.Context, rt wazero.Runtime, g perm.Grant, deps Deps) (api.Module, error) {
	if err := validateDeps(g, deps); err != nil {
		return nil, err
	}

	if g.HTTP {
		// The allowlist is this module's gate, so the module — not the caller —
		// makes sure a redirect cannot step around it. deps is a value, so the
		// caller's Deps and its client are unchanged.
		deps.HTTP = redirectGuardedClient(g, deps.HTTP)
	}

	calls := hostCalls{grant: g, deps: deps}
	builder := rt.NewHostModuleBuilder(abi.HostModuleName)
	if g.Log {
		builder.NewFunctionBuilder().WithFunc(calls.log).Export(funcLog)
	}
	if g.Config {
		builder.NewFunctionBuilder().WithFunc(calls.configGet).Export(funcConfigGet)
	}
	if g.KV {
		builder.NewFunctionBuilder().WithFunc(calls.kvGet).Export(funcKVGet)
		builder.NewFunctionBuilder().WithFunc(calls.kvPut).Export(funcKVPut)
	}
	if g.HTTP {
		builder.NewFunctionBuilder().WithFunc(calls.httpRequest).Export(funcHTTPRequest)
	}
	if g.FS {
		builder.NewFunctionBuilder().WithFunc(calls.readFile).Export(funcReadFile)
	}
	if g.Tool {
		builder.NewFunctionBuilder().WithFunc(calls.callTool).Export(funcCallTool)
	}

	mod, err := builder.Instantiate(ctx)
	if err != nil {
		return nil, fmt.Errorf("instantiate host module %q: %w", abi.HostModuleName, err)
	}

	// The registrations above and capabilityFuncs are two spellings of the same
	// decision, and CheckImports trusts the table. A drift between them would
	// either hand a guest a capability nobody granted or reject an import the
	// module does provide, so assert they agree. A mismatch is a programming
	// error in this file, not a runtime condition a caller can handle.
	granted := grantedFuncs(g)
	if len(mod.ExportedFunctionDefinitions()) != len(granted) {
		panic(fmt.Sprintf("host: BuildHostModule registered %d functions but grant %+v authorizes %d: "+
			"the registrations and capabilityFuncs have drifted apart",
			len(mod.ExportedFunctionDefinitions()), g, len(granted)))
	}
	for name := range mod.ExportedFunctionDefinitions() {
		if _, ok := granted[name]; !ok {
			panic(fmt.Sprintf("host: BuildHostModule registered %q, which grant %+v does not authorize: "+
				"the registrations and capabilityFuncs have drifted apart", name, g))
		}
	}

	return mod, nil
}

// CheckImports compares compiled's imports from the `legion` host module
// against what g authorizes, returning a human-readable error naming every
// function the guest wants and the grant does not provide.
//
// wazero would fail the instantiation anyway — that is the point of not
// registering an ungranted capability — but its message is a raw link error.
// Calling CheckImports first turns "module[legion] function[http_request] not
// exported" into a sentence that names the capability a deployment has to
// grant. Imports from other modules (wasi_snapshot_preview1, which the runtime
// provides) are not this function's business.
func CheckImports(compiled wazero.CompiledModule, g perm.Grant) error {
	var names []string
	for _, def := range compiled.ImportedFunctions() {
		module, name, _ := def.Import()
		if module == abi.HostModuleName {
			names = append(names, name)
		}
	}
	return checkImportNames(names, g)
}

// checkImportNames is CheckImports without wazero, so the resolution rules can
// be exercised directly — including the import no capability provides at all.
func checkImportNames(names []string, g perm.Grant) error {
	granted := grantedFuncs(g)
	provider := make(map[string]string, len(capabilityFuncs))
	for _, entry := range capabilityFuncs {
		for _, name := range entry.funcs {
			provider[name] = entry.capability
		}
	}

	var problems []string
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, ok := granted[name]; ok {
			continue
		}
		if capability, known := provider[name]; known {
			problems = append(problems, fmt.Sprintf(
				"%q requires the %q capability, which this plugin was not granted", name, capability))
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%q is not a host function of the %q module in this version", name, abi.HostModuleName))
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("plugin imports host functions it is not authorized to use: %s",
		strings.Join(problems, "; "))
}

// grantedFuncs returns the host function names g authorizes, mapped to the
// capability that authorizes each.
func grantedFuncs(g perm.Grant) map[string]string {
	out := make(map[string]string, len(capabilityFuncs))
	for _, entry := range capabilityFuncs {
		if !entry.granted(g) {
			continue
		}
		for _, name := range entry.funcs {
			out[name] = entry.capability
		}
	}
	return out
}

// validateDeps checks that every capability g grants has what it needs. It
// deliberately says nothing about the dependencies of ungranted capabilities:
// nothing is registered for those, so a nil there is not a fault.
func validateDeps(g perm.Grant, deps Deps) error {
	granted := grantedFuncs(g)
	if len(granted) == 0 {
		// Nothing is registered, so nothing is needed. An empty grant is a
		// legitimate state (a plugin that only contributes tools of its own).
		return nil
	}

	// PluginName and Logger are not tied to one capability: the first
	// identifies the plugin in kv keys, tool-call origins, denial events and
	// log lines, and the second is the only channel a host function has for a
	// failure it cannot return to the guest.
	if deps.PluginName == "" {
		return fmt.Errorf("build host module: grant %+v authorizes host functions but Deps.PluginName is empty", g)
	}
	if deps.Logger == nil {
		return fmt.Errorf("build host module: grant %+v authorizes host functions but Deps.Logger is nil", g)
	}

	if g.Config {
		if len(deps.Config) == 0 {
			return fmt.Errorf(`build host module: capability %q is granted but Deps.Config is empty `+
				`(a plugin with no configuration must be given an explicit "{}")`, capConfig)
		}
		if !json.Valid(deps.Config) {
			return fmt.Errorf("build host module: capability %q is granted but Deps.Config is not valid JSON; "+
				"config_get returns it verbatim, so a guest would receive an undecodable body", capConfig)
		}
	}
	if g.KV && deps.KV == nil {
		return fmt.Errorf("build host module: capability %q is granted but Deps.KV is nil", capKV)
	}
	if g.HTTP && deps.HTTP == nil {
		return fmt.Errorf("build host module: capability %q is granted but Deps.HTTP is nil", capHTTP)
	}
	if g.FS {
		if deps.FS == (port.WorkspacePathGuard{}) {
			// The zero guard is not a guard rooted at the filesystem root: it has
			// no root at all, and every Check against it would be decided by
			// filepath.Clean("") == ".", i.e. by the process working directory.
			return fmt.Errorf("build host module: capability %q is granted but Deps.FS is the zero "+
				"WorkspacePathGuard; pass port.NewWorkspacePathGuard(root)", capFS)
		}
		for i, allowed := range g.AllowedPaths {
			// An empty entry is fail-closed at call time (checkAllowedPath skips
			// it rather than letting filepath.Clean("") == "." widen the grant to
			// the working directory), but a deployment that shipped one would
			// never find out: read_file would just refuse paths the operator
			// believes are allowed. Refuse to build instead.
			if allowed == "" {
				return fmt.Errorf("build host module: capability %q is granted but "+
					"Grant.AllowedPaths[%d] is empty; remove the entry or give it a path", capFS, i)
			}
		}
	}
	if (g.HTTP || g.FS) && deps.Events == nil {
		return fmt.Errorf("build host module: capabilities %q/%q can deny a call and must report it, "+
			"but Deps.Events is nil", capHTTP, capFS)
	}
	if g.Tool {
		if deps.Tools == nil {
			return fmt.Errorf("build host module: capability %q is granted but Deps.Tools is nil", capTool)
		}
		if deps.Agent.ID == "" {
			return fmt.Errorf("build host module: capability %q is granted but Deps.Agent has no ID; "+
				"Registry.Execute would evaluate permissions against an empty identity", capTool)
		}
	}
	return nil
}
