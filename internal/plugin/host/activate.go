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
)

// The ledger labels Activate files its resources under. They name the thing
// being revoked, so a "plugin loaded but nothing happened" report can be
// answered from lifecycle.Ledger.Snapshot alone.
const (
	ledgerLabelRuntime  = "wasm-runtime"
	ledgerLabelInstance = "wasm-instance"
)

// Manifest is a plugin's self-description, read from the guest itself through
// abi.OpManifest rather than taken on trust from deployment configuration.
//
// Provides lists the tool names the guest says it implements. The host's own
// claim about a plugin (Spec.Provides) is cross-checked against it during
// activation, which is the point of asking the guest at all.
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

	// Provides lists the tool names the host claims this plugin contributes.
	// Every one of them must appear in the guest's own manifest; a guest that
	// declares MORE than the host claims is not a conflict, because the host
	// only ever contributes what it claims here.
	Provides []string

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
// guest declared, and the one live Instance activation created.
//
// A Plugin owns no teardown of its own. Every resource activation created is
// filed in the lifecycle.Ledger under the owner it was activated for, so
// revoking a plugin is ledger.DisposeOwner(owner) and nothing else.
type Plugin struct {
	// Name is Spec.Name, which activation has proven equals Manifest.Name.
	Name string

	// Manifest is what the guest declared about itself.
	Manifest Manifest

	// Instance is the plugin's single live instance. Instance methods are not
	// safe for concurrent use; serializing calls is the caller's job until
	// an instance pool takes it over.
	Instance *Instance
}

// Activate brings one plugin up as a sequence of steps, each of which files
// its revocation under owner before the next step can fail:
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
//  7. cross-check that self-description against spec.
//
// The cross-check is deliberately last, AFTER the instance is filed: a plugin
// whose host manifest disagrees with its guest is exactly the failure that
// must exercise the rollback, so the rollback path cannot rot into dead code.
//
// On any failure Activate rolls back everything already filed under owner, in
// reverse order (lifecycle.Ledger.DisposeOwner), and returns an error — never
// a partially activated Plugin. A failure while rolling back is itself
// reported: it is joined onto the activation error rather than replacing or
// hiding it.
//
// Activate does NOT register the plugin's tools. Tool contribution is a
// separate step with its own revocation, and it is not part of the sequence
// above.
func Activate(ctx context.Context, ledger *lifecycle.Ledger, owner lifecycle.Owner, spec Spec) (_ *Plugin, err error) {
	if ledger == nil {
		return nil, fmt.Errorf("activate plugin %q: ledger is nil; activation has nowhere to file its rollback", spec.Name)
	}
	if owner == "" {
		return nil, fmt.Errorf("activate plugin %q: owner is empty; every filed resource needs an owner to revoke it", spec.Name)
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

	committed := false
	defer func() {
		if committed {
			return
		}
		if derr := ledger.DisposeOwner(owner); derr != nil {
			err = errors.Join(err, fmt.Errorf("roll back activation of plugin %q: %w", spec.Name, derr))
		}
	}()

	rt := NewRuntime(ctx, spec.MemoryPages)
	// Filed before anything is compiled into it: from here on the runtime holds
	// resources that leak unless it is closed, and every following step can
	// fail. Closing the runtime also closes the host module and any instance
	// created from it, which is why it is filed FIRST and therefore disposed
	// LAST.
	ledger.Add(owner, ledgerLabelRuntime, func() error {
		if cerr := rt.Close(disposeCtx); cerr != nil {
			return fmt.Errorf("close wasm runtime of plugin %q: %w", spec.Name, cerr)
		}
		return nil
	})

	// The CompiledModule needs no ledger entry of its own: wazero ties it to the
	// runtime that compiled it, so the runtime's disposer releases it too.
	compiled, err := Compile(ctx, rt, spec.Wasm)
	if err != nil {
		return nil, fmt.Errorf("activate plugin %q: %w", spec.Name, err)
	}
	if err := CheckImports(compiled, spec.Grant); err != nil {
		return nil, fmt.Errorf("activate plugin %q: check imports: %w", spec.Name, err)
	}
	if _, err := BuildHostModule(ctx, rt, spec.Grant, spec.Deps); err != nil {
		return nil, fmt.Errorf("activate plugin %q: %w", spec.Name, err)
	}

	inst, err := NewInstance(ctx, rt, compiled)
	if err != nil {
		return nil, fmt.Errorf("activate plugin %q: %w", spec.Name, err)
	}
	ledger.Add(owner, ledgerLabelInstance, func() error {
		if cerr := inst.Close(disposeCtx); cerr != nil {
			return fmt.Errorf("close instance of plugin %q: %w", spec.Name, cerr)
		}
		return nil
	})

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

	committed = true
	return &Plugin{Name: spec.Name, Manifest: manifest, Instance: inst}, nil
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
	for i, provided := range spec.Provides {
		if provided == "" {
			return fmt.Errorf("activate plugin %q: Spec.Provides[%d] is empty; remove the entry or give it a tool name", spec.Name, i)
		}
	}
	if spec.Deps.PluginName != "" && spec.Deps.PluginName != spec.Name {
		return fmt.Errorf("activate plugin %q: Deps.PluginName is %q; the plugin's identity must be spelled once "+
			"(leave Deps.PluginName empty and Spec.Name fills it)", spec.Name, spec.Deps.PluginName)
	}
	return nil
}

// readManifest asks the guest to describe itself. The empty body is not a
// legal answer here: abi.OpManifest's contract is a JSON document, and a guest
// that returns nothing has not answered.
func readManifest(ctx context.Context, inst *Instance) (Manifest, error) {
	body, err := inst.Invoke(ctx, abi.OpManifest, nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("read self-description (op %d): %w", abi.OpManifest, err)
	}
	if len(body) == 0 {
		return Manifest{}, fmt.Errorf("read self-description (op %d): guest returned no body", abi.OpManifest)
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode self-description %s: %w", body, err)
	}
	return manifest, nil
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
	var missing []string
	for _, claimed := range spec.Provides {
		if _, ok := declared[claimed]; !ok {
			missing = append(missing, claimed)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("cross-check manifest: host claims plugin %q provides %v, "+
			"guest declares %v; not declared by the guest: %v",
			spec.Name, spec.Provides, manifest.Provides, missing)
	}
	return nil
}

// describe renders a manifest for an error message, so a guest that answered
// with something unusable is quoted rather than summarized away.
func describe(manifest Manifest) string {
	return fmt.Sprintf("name=%q version=%q provides=[%s]",
		manifest.Name, manifest.Version, strings.Join(manifest.Provides, " "))
}
