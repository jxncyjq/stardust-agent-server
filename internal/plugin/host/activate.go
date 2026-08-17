package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/tool"
)

// The ledger labels Activate files its resources under. They name the thing
// being revoked, so a "plugin loaded but nothing happened" report can be
// answered from lifecycle.Ledger.Snapshot alone.
const (
	ledgerLabelRuntime  = "wasm-runtime"
	ledgerLabelInstance = "wasm-instance"
	ledgerLabelPool     = "wasm-instance-pool"
)

// Manifest is a plugin's self-description, read from the guest itself through
// abi.OpManifest rather than taken on trust from deployment configuration.
//
// Provides lists the tool names the guest says it implements. The host's own
// claim about a plugin (the names in Spec.Tools) is cross-checked against it
// during activation, which is the point of asking the guest at all.
//
// Name and Version are both mandatory: the host claims a name of its own and
// compares it, and while it claims no version, a plugin that cannot say which
// build it is would make every later "which version answered?" question
// unanswerable. Activation refuses a manifest missing either.
type Manifest struct {
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	Provides []string `json:"provides"`
}

// Spec is everything Activate needs to bring one plugin up. It is the host
// side of the contract — what the deployment believes about this plugin —
// which activation then holds against what the guest says about itself.
type Spec struct {
	// Name is the plugin's identity: it must equal the name the guest's own
	// manifest declares, and it is the identity Deps carries into kv keys,
	// tool-call origins and denial events.
	Name string

	// Wasm is the compiled guest module. Activate takes bytes rather than a
	// path: reading the file — and hashing it, if a deployment pins content —
	// belongs to whoever assembles the Spec, not to activation.
	Wasm []byte

	// Tools describes the tools the host claims this plugin contributes, as the
	// descriptors they are registered with. The descriptors come from the host
	// side on purpose: the deployment declares what a plugin contributes and
	// with what schema, risk level and catalog group, while the guest's own
	// manifest lists plain names. A guest cannot describe itself into a lower
	// risk level or a group it was not given.
	//
	// It must be non-empty: in this phase a plugin exists to contribute tools,
	// and an empty Tools would make the cross-check below vacuous. Every NAME
	// in it must appear in the guest's own manifest; a guest that declares MORE
	// than the host claims is not a conflict, because the host only ever
	// contributes what it claims here. Name, Description and Group are all
	// required of each descriptor (see validateSpec).
	Tools []tool.Descriptor

	// Registry is where Tools are registered, and therefore what the model
	// reaches them through. It is required.
	//
	// It is normally the same registry as Deps.Tools — one registry, which the
	// plugin's tools enter and which the plugin's own call_tool calls go out
	// through. Keeping them separate fields is deliberate: Deps.Tools is what a
	// granted capability needs (and is only required when the tool capability is
	// granted), while this is what activation itself needs, and a deployment may
	// legitimately let a plugin contribute into a registry while giving its
	// call_tool a narrower view of it.
	Registry *tool.Registry

	// MaxInstances is how many of this plugin's calls may be in the guest at
	// once: the size of the instance pool the contributed tools are served
	// from. It must be at least 1 and has no default — one guest instance
	// serves one call at a time (an Instance is not safe for concurrent use),
	// so this is the plugin's concurrency, a sizing decision the deployment
	// owns.
	MaxInstances int

	// Grant is the capability set this deployment authorizes. Ungranted host
	// functions are absent from the plugin's host module, so a guest importing
	// one cannot instantiate — Activate reports that before instantiation
	// rather than letting wazero's raw link error out (see CheckImports).
	Grant perm.Grant

	// Deps carries what the granted capabilities need. Its PluginName is
	// derived from Name: leave it empty, or set it to the same value.
	Deps Deps

	// MemoryPages caps how far the guest may grow its linear memory. It is a
	// required sizing decision with no default (see NewRuntime).
	MemoryPages uint32
}

// Plugin is an activated plugin: its host-side identity, the manifest the
// guest declared, and the pool of guest instances its contributed tools are
// served from.
//
// A Plugin owns no teardown of its own. Every resource activation created is
// filed in the lifecycle.Ledger under the owner it was activated for, so once
// Activate has returned successfully, revoking the plugin is
// ledger.DisposeOwner(owner) and nothing else — Activate's owner-exclusivity
// precondition (see its doc comment) guarantees nothing else is filed under
// that owner alongside it.
type Plugin struct {
	// Name is Spec.Name, which activation has proven equals Manifest.Name.
	Name string

	// Manifest is what the guest declared about itself.
	Manifest Manifest

	// pool serves the plugin's contributed tools: one instance per concurrent
	// call, sized by Spec.MaxInstances. It is unexported because a plugin is
	// reached through its tools — activation registers them (see
	// contributeTools), and the handlers it registered hold this pool — so
	// nothing outside this package needs to invoke a guest directly.
	pool *pool
}

// closeInstance performs the wazero close behind the instance's ledger
// disposer, and behind the pool's discard and drain closes. It is a
// package-level function value rather than a direct call to *Instance.Close
// only so tests can substitute a failing implementation and prove that a
// close failure is reported instead of dropped
// (TestActivateReportsAFailureWhileRollingBack,
// TestPoolDrainReportsACloseFailure) — forcing wazero's own Close to fail from
// outside this package is not practically reachable. Production code never
// overrides it. Substituting it is only safe while no test in this package
// calls t.Parallel(): several tests already swap it and restore it via
// t.Cleanup, which a parallel test would race under -race.
var closeInstance = func(ctx context.Context, inst *Instance) error { return inst.Close(ctx) }

// Activate brings one plugin up as a sequence of steps, each of which files
// its own one-shot revocation handle before the next step can fail:
//
//  1. create the plugin's wazero runtime (file it), so the memory cap is in
//     force before any guest code is compiled;
//  2. compile the module;
//  3. check the module's host-function imports against spec.Grant, so an
//     unauthorized import is reported as the capability a deployment would
//     have to grant, not as wazero's raw link failure;
//  4. build the host module with exactly the granted capabilities;
//  5. instantiate the guest (file it);
//  6. read the guest's self-description via abi.OpManifest;
//  7. cross-check that self-description against spec;
//  8. create the instance pool the plugin's tools are served from and file its
//     drain;
//  9. contribute the plugin's tools (see contributeTools), which files one
//     registry entry and one gateable-catalog entry per tool.
//
// The cross-check is deliberately before the pool and the contribution, and
// AFTER the instance is filed: a plugin
// whose host manifest disagrees with its guest is exactly the failure that
// must exercise the rollback, so the rollback path cannot rot into dead code.
//
// Owner exclusivity is a precondition, not a courtesy: owner must belong to
// this activation alone. Activate refuses to start if
// ledger.Snapshot()[owner] is already non-empty, naming the owner and the
// entries already filed under it, rather than silently tearing them down. A
// caller reloading a plugin under a stable owner (a hot-reload keeping
// "plugin:foo" across activations, for instance) MUST dispose the previous
// activation before calling Activate again with the same owner.
//
// On any failure Activate rolls back only the entries THIS CALL filed, in
// reverse order, using the one-shot revoke handle lifecycle.Ledger.Add
// returned for each of them — never lifecycle.Ledger.DisposeOwner(owner),
// which would also destroy anything else filed under owner (the exact
// failure mode the owner-exclusivity precondition above and this rollback
// scoping both exist to prevent: a hot-reload's failed cross-check must
// never tear down the live plugin it was trying to replace). A failure while
// rolling back is itself reported: every revoke error is joined
// (errors.Join) onto the activation error rather than replacing or hiding
// it, and none is dropped. Activate never returns a partially activated
// Plugin. That holds for a PANIC as well: the contribution step is fail-loud on
// a tool name another contributor already owns, and the rollback runs on the way
// out of it, so a panicking activation leaves nothing filed either.
//
// Once Activate has returned, the plugin IS its tools: they are in
// spec.Registry, they are gateable, and every call to one of them is served by
// the instance pool through the handler contributeTools registered. Revoking all
// of that — tools, gateable entries, pool, instance, runtime, in that order — is
// ledger.DisposeOwner(owner) and nothing else.
func Activate(ctx context.Context, ledger *lifecycle.Ledger, owner lifecycle.Owner, spec Spec) (_ *Plugin, err error) {
	if ledger == nil {
		return nil, fmt.Errorf("activate plugin %q: ledger is nil; activation has nowhere to file its rollback", spec.Name)
	}
	if owner == "" {
		return nil, fmt.Errorf("activate plugin %q: owner is empty; every filed resource needs an owner to revoke it", spec.Name)
	}
	if held := ledger.Snapshot()[owner]; len(held) != 0 {
		return nil, fmt.Errorf("activate plugin %q: owner %s already holds %v; owner must be exclusive to a single "+
			"activation — dispose the previous activation before activating a new one under the same owner",
			spec.Name, owner, held)
	}
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	// Spec.Name is the single source of the plugin's identity. spec is a value,
	// so filling Deps.PluginName in here changes nothing for the caller; a
	// caller that set a DIFFERENT name is an assembly error, because kv keys
	// and tool-call origins would then be attributed to another plugin.
	spec.Deps.PluginName = spec.Name

	// Disposers can run long after this call — and must run even when ctx is
	// already dead, which is exactly the case when a cancelled activation is
	// being rolled back. A disposer that closed with a cancelled ctx would fail
	// on arrival and leak what it was supposed to release.
	disposeCtx := context.WithoutCancel(ctx)

	// revokers holds the one-shot handle lifecycle.Ledger.Add returns for
	// every entry THIS CALL files, in filing order. Only these are rolled back
	// on failure — see the owner-exclusivity paragraph above.
	var revokers []func() error
	// keep records one handle. It is passed to contributeTools, whose failure
	// mode is a panic part-way through a list of tools: collecting each handle as
	// it is filed is what lets the rollback below revoke the tools that were
	// registered before the failing one.
	keep := func(revoke func() error) { revokers = append(revokers, revoke) }

	committed := false
	defer func() {
		if committed {
			return
		}
		for i := len(revokers) - 1; i >= 0; i-- {
			if derr := revokers[i](); derr != nil {
				err = errors.Join(err, fmt.Errorf("roll back activation of plugin %q: %w", spec.Name, derr))
			}
		}
	}()

	rt := NewRuntime(ctx, spec.MemoryPages)
	// Filed before anything is compiled into it: from here on the runtime holds
	// resources that leak unless it is closed, and every following step can
	// fail. Closing the runtime also closes the host module and any instance
	// created from it, which is why it is filed FIRST and therefore disposed
	// LAST.
	revokers = append(revokers, ledger.Add(owner, ledgerLabelRuntime, func() error {
		if cerr := rt.Close(disposeCtx); cerr != nil {
			return fmt.Errorf("close wasm runtime of plugin %q: %w", spec.Name, cerr)
		}
		return nil
	}))

	// The CompiledModule needs no ledger entry of its own: wazero ties it to the
	// runtime that compiled it, so the runtime's disposer releases it too.
	compiled, err := Compile(ctx, rt, spec.Wasm)
	if err != nil {
		return nil, fmt.Errorf("activate plugin %q: %w", spec.Name, err)
	}
	if err := CheckImports(compiled, spec.Grant); err != nil {
		return nil, fmt.Errorf("activate plugin %q: %w", spec.Name, err)
	}
	if _, err := BuildHostModule(ctx, rt, spec.Grant, spec.Deps); err != nil {
		return nil, fmt.Errorf("activate plugin %q: %w", spec.Name, err)
	}

	inst, err := NewInstance(ctx, rt, compiled)
	if err != nil {
		return nil, fmt.Errorf("activate plugin %q: %w", spec.Name, err)
	}
	revokers = append(revokers, ledger.Add(owner, ledgerLabelInstance, func() error {
		if cerr := closeInstance(disposeCtx, inst); cerr != nil {
			return fmt.Errorf("close instance of plugin %q: %w", spec.Name, cerr)
		}
		return nil
	}))

	// Read and cross-check AFTER the instance is filed: a mismatch must be a
	// failure with something to roll back (see the doc comment above, and
	// TestActivateFilesTheInstanceBeforeReadingTheManifest, which pins it).
	manifest, err := readManifest(ctx, inst)
	if err != nil {
		return nil, fmt.Errorf("activate plugin %q: %w", spec.Name, err)
	}
	if err := crossCheck(spec, manifest); err != nil {
		return nil, fmt.Errorf("activate plugin %q: %w", spec.Name, err)
	}

	// The pool's instances are built lazily, by the calls that need them, so its
	// factory must NOT hold ctx: activation's context is typically the request
	// that loaded the plugin, and it is long dead by the time a model calls one
	// of the plugin's tools. wazero would then either refuse to instantiate or
	// close the fresh module immediately (see NewRuntime's
	// WithCloseOnContextDone), so every tool call would fail for a reason that
	// has nothing to do with the call.
	instantiateCtx := context.WithoutCancel(ctx)
	instances := newPool(spec.MaxInstances, func() (*Instance, error) {
		return NewInstance(instantiateCtx, rt, compiled)
	})
	// Filed AFTER the runtime and the instance precisely so that reverse-order
	// disposal runs it BEFORE them: drain converges the calls that are inside the
	// guest right now, and closing the runtime underneath them would truncate a
	// tool call in flight. The wait is bounded by each in-flight call's own
	// context (a tool descriptor's Timeout, or whatever the caller passed), not
	// by a deadline invented here, so disposeCtx carries no cancellation of its
	// own.
	revokers = append(revokers, ledger.Add(owner, ledgerLabelPool, func() error {
		if derr := instances.drain(disposeCtx); derr != nil {
			return fmt.Errorf("drain instance pool of plugin %q: %w", spec.Name, derr)
		}
		return nil
	}))

	// Contribution is the last step, so a tool name another contributor already
	// owns rolls back everything above it — including the tools registered
	// before the failing one.
	contributeTools(ledger, owner, spec, instances, keep)

	committed = true
	return &Plugin{Name: spec.Name, Manifest: manifest, pool: instances}, nil
}

// validateSpec rejects a Spec that cannot describe a real activation. These
// are caller mistakes, so each one is named rather than defaulted: a zero
// MemoryPages in particular would panic in NewRuntime, and a Spec is
// deployment data, not a programming invariant.
func validateSpec(spec Spec) error {
	if spec.Name == "" {
		return errors.New("activate plugin: Spec.Name is empty; the plugin's identity is what the manifest is cross-checked against")
	}
	if len(spec.Wasm) == 0 {
		return fmt.Errorf("activate plugin %q: Spec.Wasm is empty; there is no module to compile", spec.Name)
	}
	if spec.MemoryPages == 0 {
		return fmt.Errorf("activate plugin %q: Spec.MemoryPages is 0; the guest memory cap is a required sizing decision", spec.Name)
	}
	if spec.Registry == nil {
		return fmt.Errorf("activate plugin %q: Spec.Registry is nil; the plugin's tools have no registry to enter, "+
			"so activating it would load a plugin nothing can call", spec.Name)
	}
	if spec.MaxInstances < 1 {
		return fmt.Errorf("activate plugin %q: Spec.MaxInstances is %d; how many calls this plugin may serve at once "+
			"is a required sizing decision with no default (one instance serves one call at a time)",
			spec.Name, spec.MaxInstances)
	}
	if len(spec.Tools) == 0 {
		return fmt.Errorf("activate plugin %q: Spec.Tools is empty; a plugin exists to contribute tools, "+
			"and a cross-check against nothing is not a check", spec.Name)
	}
	claimed := make(map[string]struct{}, len(spec.Tools))
	for i, descriptor := range spec.Tools {
		switch {
		case descriptor.Name == "":
			return fmt.Errorf("activate plugin %q: Spec.Tools[%d].Name is empty; remove the entry or give it a tool name",
				spec.Name, i)
		case descriptor.Description == "":
			return fmt.Errorf("activate plugin %q: Spec.Tools[%d] (%q) has no Description; it is the line the "+
				"per-agent config UI shows for a gateable tool, so an empty one leaves an unexplained switch",
				spec.Name, i, descriptor.Name)
		case descriptor.Group == "":
			return fmt.Errorf("activate plugin %q: Spec.Tools[%d] (%q) has no Group; an unplaced tool cannot be "+
				"listed in the capability catalog, so the model would never see it", spec.Name, i, descriptor.Name)
		}
		if _, dup := claimed[descriptor.Name]; dup {
			return fmt.Errorf("activate plugin %q: Spec.Tools claims %q twice; one name is one tool, and the second "+
				"registration would be refused half-way through the contribution", spec.Name, descriptor.Name)
		}
		claimed[descriptor.Name] = struct{}{}
	}
	if spec.Deps.PluginName != "" && spec.Deps.PluginName != spec.Name {
		return fmt.Errorf("activate plugin %q: Deps.PluginName is %q; the plugin's identity must be spelled once "+
			"(leave Deps.PluginName empty and Spec.Name fills it)", spec.Name, spec.Deps.PluginName)
	}
	return nil
}

// readManifest asks the guest to describe itself and decodes what it
// returns. The actual decoding is decodeManifest, split out so its error
// branches can be table-tested without an Instance or a compiled guest
// module.
func readManifest(ctx context.Context, inst *Instance) (Manifest, error) {
	body, err := inst.Invoke(ctx, abi.OpManifest, nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("read self-description (op %d): %w", abi.OpManifest, err)
	}
	manifest, err := decodeManifest(body)
	if err != nil {
		return Manifest{}, fmt.Errorf("read self-description (op %d): %w", abi.OpManifest, err)
	}
	return manifest, nil
}

// decodeManifest parses a guest's raw self-description body. The empty body
// is not a legal answer: abi.OpManifest's contract is a JSON document, and a
// guest that returns nothing has not answered — this is reachable in
// production, not theoretical, because Instance.Invoke returns (nil, nil)
// whenever the guest's packed result length is 0.
func decodeManifest(body []byte) (Manifest, error) {
	if len(body) == 0 {
		return Manifest{}, errors.New("guest returned no body")
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode self-description %s: %w", quoteForError(body), err)
	}
	return manifest, nil
}

// quoteForError renders guest-controlled bytes safely for an error message.
// body is bounded only by Spec.MemoryPages and may contain newlines or other
// control characters, so it is %q-escaped rather than spliced in raw, and
// capped to its first 256 bytes (plus a total-length suffix) so a guest that
// fills its whole memory cap with garbage cannot inflate the error — and,
// eventually, the log line Task 6 writes it to — to match.
func quoteForError(body []byte) string {
	const max = 256
	if len(body) <= max {
		return fmt.Sprintf("%q", body)
	}
	return fmt.Sprintf("%q (truncated from %d bytes)", body[:max], len(body))
}

// crossCheck holds the host's claims about a plugin against what the guest
// declared. It names the step, the host's claim and the guest's declaration,
// because that triple is what an operator needs in order to tell a stale
// deployment manifest from a stale plugin binary.
func crossCheck(spec Spec, manifest Manifest) error {
	if manifest.Name == "" {
		return fmt.Errorf("cross-check manifest: guest declares no name (self-description: %s); "+
			"host claims name %q", describe(manifest), spec.Name)
	}
	if manifest.Version == "" {
		return fmt.Errorf("cross-check manifest: guest %q declares no version (self-description: %s)",
			manifest.Name, describe(manifest))
	}
	if manifest.Name != spec.Name {
		return fmt.Errorf("cross-check manifest: host claims plugin name %q, guest declares %q",
			spec.Name, manifest.Name)
	}

	declared := make(map[string]struct{}, len(manifest.Provides))
	for _, provided := range manifest.Provides {
		declared[provided] = struct{}{}
	}
	// The host's claim is the NAMES of the descriptors it is about to register:
	// the rest of a descriptor (schema, risk level, group) is the deployment's to
	// state and nothing the guest could confirm.
	claimed := make([]string, 0, len(spec.Tools))
	var missing []string
	for _, descriptor := range spec.Tools {
		claimed = append(claimed, descriptor.Name)
		if _, ok := declared[descriptor.Name]; !ok {
			missing = append(missing, descriptor.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("cross-check manifest: host claims plugin %q provides %v, "+
			"guest declares %v; not declared by the guest: %v",
			spec.Name, claimed, manifest.Provides, missing)
	}
	return nil
}

// describe renders a manifest for an error message, so a guest that answered
// with something unusable is quoted rather than summarized away.
func describe(manifest Manifest) string {
	return fmt.Sprintf("name=%q version=%q provides=[%s]",
		manifest.Name, manifest.Version, strings.Join(manifest.Provides, " "))
}
