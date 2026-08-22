package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/toolauth"
)

// The ledger labels Activate files its resources under. They name the thing
// being revoked, so a "plugin loaded but nothing happened" report can be
// answered from lifecycle.Ledger.Snapshot alone.
//
// ledgerLabelContributions is the odd one out: it names no resource of its own
// but the whole contribution side of the activation, which lives under a
// SECOND owner (see ToolsOwner). Its disposer is what keeps
// lifecycle.Ledger.DisposeOwner(instance owner) sufficient after the split.
const (
	ledgerLabelRuntime       = "wasm-runtime"
	ledgerLabelPool          = "wasm-instance-pool"
	ledgerLabelContributions = "tool-contributions"
)

// ToolsOwner derives the ledger owner a plugin's CONTRIBUTIONS are filed
// under — "<owner>/tools" — from the owner its wasm resources are filed under.
//
// The two sides are separate owners because they have different lifetimes: a
// plugin whose dependency went away must stop offering tools the model cannot
// use, without losing the guest instance and the state inside it. Withdrawing
// the contributions is then lifecycle.Ledger.DisposeOwner(ToolsOwner(owner)),
// which cannot touch the runtime or the pool because they are not filed there
// (see Plugin.Suspend).
//
// It is a derivation rather than a field so that a caller holding only the
// instance owner can reach the contribution side without consulting a
// directory of who is mounted — the same reason lifecycle.Ledger keeps no such
// directory itself.
//
// An empty owner is a programming error, not a caller mistake to be defaulted:
// "/tools" would be a namespace shared by every plugin that forgot its owner.
func ToolsOwner(owner lifecycle.Owner) lifecycle.Owner {
	if owner == "" {
		panic("host: ToolsOwner: owner is empty; the contribution side of an activation cannot be namespaced by nothing")
	}
	return owner + "/tools"
}

// drainGrace is how much longer than the longest in-flight call teardown waits
// for a plugin's instance pool to converge (see drainDeadline).
//
// It is a package-level variable rather than a constant only so a test can
// shrink it and prove teardown is bounded without spending the real grace
// waiting (TestDisposeOwnerDoesNotHangOnAnUnboundedGuestCall). Production code
// never overrides it. Substituting it is only safe while no test in this package
// calls t.Parallel() — the same condition closeInstance carries.
var drainGrace = 5 * time.Second

// drainDeadline is the bound teardown puts on a plugin's pool drain: the
// longest Timeout any of its tools carries, plus drainGrace.
//
// The bound has to compose with the per-tool timeouts rather than be an
// independent number. Registry.Execute cancels a tool call once its
// descriptor's Timeout expires, wazero's WithCloseOnContextDone then interrupts
// the guest, and the handler's release is what lets drain converge — so a drain
// deadline SHORTER than an in-flight call's own timeout would expire first and
// report a leak that was about to resolve itself. Waiting the longest tool
// timeout is therefore the floor; drainGrace on top is the slack for the
// interrupt, the handler's unwind and the release to actually happen.
//
// validateSpec guarantees every descriptor carries a positive Timeout, so the
// result is always greater than drainGrace.
func drainDeadline(tools []tool.Descriptor) time.Duration {
	longest := time.Duration(0)
	for _, descriptor := range tools {
		if descriptor.Timeout > longest {
			longest = descriptor.Timeout
		}
	}
	return longest + drainGrace
}

// ActivationRollbackError is the value a panicking Activate re-panics with when
// its own rollback ALSO failed.
//
// It exists because a rollback failure has nowhere else to go on that path: the
// panic is unwinding, so Activate's error return is never delivered, and a
// failure to close the plugin's wazero runtime or converge its pool would
// otherwise vanish at the exact moment it matters most. Both halves travel
// together — the panic that aborted the activation and every failure rolling it
// back hit — so a caller that recovers sees the cause and the leak in one value.
//
// Recovering callers can reach the rollback failures with errors.Is/errors.As
// (see Unwrap); the panic that caused the activation to abort is Panic.
type ActivationRollbackError struct {
	// Panic is the value Activate was unwinding when the rollback ran. It is
	// typically the fail-loud panic of a duplicate tool name (see
	// contributeTools).
	Panic any

	// Rollback is every failure the rollback itself hit, joined.
	Rollback error
}

// Error reports both halves: what aborted the activation, and what rolling it
// back failed to release.
func (e *ActivationRollbackError) Error() string {
	return fmt.Sprintf("activation panicked (%v), and rolling it back also failed: %v", e.Panic, e.Rollback)
}

// Unwrap returns the joined rollback failures, so a recovering caller can test
// them with errors.Is and errors.As.
func (e *ActivationRollbackError) Unwrap() error { return e.Rollback }

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
	// contributes what it claims here.
	//
	// Name, Description, Group and Timeout are all required of each descriptor,
	// with no defaults (see validateSpec). Timeout is required because it is the
	// ONLY bound on a call inside the guest: Registry.Execute applies a timeout
	// only when the descriptor carries a positive one, so a descriptor without it
	// would let a plugin call that never returns run forever — and hold up the
	// pool drain that teardown waits for (see drainDeadline).
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
// filed in the lifecycle.Ledger, so once Activate has returned successfully,
// revoking the plugin is ledger.DisposeOwner(owner) and nothing else —
// Activate's owner-exclusivity precondition (see its doc comment) guarantees
// nothing else is filed under that owner alongside it, and the entry labelled
// ledgerLabelContributions carries that disposal across to the contribution
// side.
//
// What a Plugin does own is one state transition: Suspend withdraws its
// contributions without touching the guest, and Resume files them again. A
// suspended plugin is still activated — same runtime, same pool, same guest
// state — it is only invisible to the model.
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

	// ledger and owner are what Suspend and Resume act on: the contributions
	// live under ToolsOwner(owner) in ledger, which is the only place either
	// method reaches. owner itself is never disposed by them — disposing it
	// means something else entirely, and it is the caller's decision.
	ledger *lifecycle.Ledger
	owner  lifecycle.Owner

	// spec is the activation's own Spec, kept because Resume must file exactly
	// what activation filed: the same descriptors, into the same registry, under
	// the same plugin name. Re-deriving them from the manifest would lose
	// everything the deployment declared (schema, risk level, group) and let a
	// resumed plugin come back as a different tool than it went down as.
	spec Spec

	// mu guards suspended and serializes the transitions themselves, so two
	// callers cannot both find the plugin active and both withdraw its
	// contributions — the second withdrawal would report success having revoked
	// nothing.
	mu        sync.Mutex
	suspended bool
}

// Suspended reports whether the plugin's contributions are currently
// withdrawn. A suspended plugin still holds its wasm runtime and its instance
// pool; it is only absent from the tool registry and the gateable catalog.
func (p *Plugin) Suspended() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.suspended
}

// Suspend withdraws the plugin's contributions — every tool it registered and
// every gateable entry it filed — while leaving the guest exactly as it is.
//
// It exists for the plugin that cannot work rather than the plugin that is
// going away: when something a plugin depends on is gone, leaving its tools in
// the registry means the model is offered tools whose every call must fail.
// Withdrawing them is the honest answer, and doing it WITHOUT tearing down the
// instance pool is what makes it reversible — the guest keeps whatever state it
// built up, so a Resume costs one ledger pass instead of a re-instantiation.
//
// Suspending an already-suspended plugin is an error naming the current state,
// not a silent no-op: a caller that suspends twice is reasoning from a stale
// view of the plugin set, and a second call that revoked nothing while
// reporting success is what would keep it there.
//
// The context is accepted for symmetry with Resume and with the other calls a
// caller makes at a task boundary, and is deliberately not consulted:
// withdrawing a contribution is a revocation, and a revocation that skipped
// itself because the caller's context had expired would leave the model looking
// at tools that cannot work — the exact state this method exists to prevent.
func (p *Plugin) Suspend(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.suspended {
		return fmt.Errorf("suspend plugin %q: it is already suspended; an earlier call withdrew its "+
			"contributions and there is nothing left to withdraw", p.Name)
	}
	// lifecycle.Ledger.DisposeOwner runs every disposer even when one fails, so
	// the contributions are gone whatever it returns. The state flips before the
	// error is reported for that reason: a Suspend that stayed "active" after
	// withdrawing everything would refuse the Resume that is the only way back.
	err := p.ledger.DisposeOwner(ToolsOwner(p.owner))
	p.suspended = true
	if err != nil {
		return fmt.Errorf("suspend plugin %q: %w", p.Name, err)
	}
	return nil
}

// Resume files the plugin's contributions again, on the pool it never lost, so
// its tools come back served by the same guest instance and the state inside
// it.
//
// Resuming a plugin that is not suspended is an error naming the current
// state. It cannot be a no-op: contributing a second time is a duplicate
// registration, which is fail-loud in the registry and in the gateable catalog
// alike.
//
// A tool name is only the plugin's while it holds it. While it was suspended
// its names were free, so another contributor may legitimately have taken one —
// operator-authored data that collides, which is an error, not a violated
// invariant. Resume therefore checks BEFORE it contributes and reports the
// conflicting names, because both tool.Registry.RegisterDescriptor and
// toolauth.Contribute answer a duplicate with a panic, and a resume that
// panicked would take the caller down over a state its operator can fix.
//
// The check and the contribution are not one atomic step against the world:
// nothing stops a third party from taking one of these names in between, and
// that remains fail-loud. Closing that window would mean holding a lock the
// registry does not offer; what the check buys is that the reachable,
// operator-caused version — the name was taken and stayed taken — is an error,
// leaving only a genuine race to panic.
//
// The context is accepted and not consulted for the same reason Suspend's is:
// re-filing ledger entries reaches no guest and cannot block.
func (p *Plugin) Resume(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.suspended {
		return fmt.Errorf("resume plugin %q: it is active, not suspended; its contributions are already "+
			"filed and filing them again would be a duplicate registration", p.Name)
	}
	// A disposed plugin is not a suspended one, and the ledger is where the
	// difference shows: disposing the instance owner took the
	// ledgerLabelContributions entry with it, so contributions filed now would
	// have nothing left to revoke them — they would outlive the plugin in the
	// registry and, process-globally, in the gateable catalog. That is the leak
	// the ledger exists to prevent, so it is refused rather than filed.
	if !slices.Contains(p.ledger.Snapshot()[p.owner], ledgerLabelContributions) {
		return fmt.Errorf("resume plugin %q: owner %s no longer holds its %q entry, so the plugin has been "+
			"disposed; contributions filed now could never be revoked", p.Name, p.owner, ledgerLabelContributions)
	}
	if taken := p.takenToolNames(); len(taken) > 0 {
		return fmt.Errorf("resume plugin %q: another contributor now holds its tool names %v; "+
			"a suspended plugin's names are free, so they have to be released before it can come back",
			p.Name, taken)
	}

	// keep is a no-op collector rather than a rollback: with the names checked,
	// what is left is the race above, and everything contributeTools files goes
	// under the contribution owner — which the instance owner's
	// ledgerLabelContributions entry still reaches, so even a panicking resume
	// leaves nothing DisposeOwner cannot take down.
	contributeTools(p.ledger, ToolsOwner(p.owner), p.spec, p.pool, func(func() error) {})
	p.suspended = false
	return nil
}

// takenToolNames returns the plugin's tool names something else holds right
// now, sorted, so the error Resume returns names all of them at once instead of
// one per attempt.
//
// A name counts as taken if the registry exposes it OR the gateable catalog has
// it, because contributing does both and either half panics on its own. The
// registry side is read through Descriptors, which is a superset of what
// RegisterDescriptor would refuse: a derived registry view also reports the
// names it inherits, and registering one of those would shadow rather than
// panic. Refusing to resume into a name something else already answers is the
// safer half of that inaccuracy — the model would otherwise see one name with
// two implementations behind it.
func (p *Plugin) takenToolNames() []string {
	exposed := make(map[string]struct{})
	for _, descriptor := range p.spec.Registry.Descriptors() {
		exposed[descriptor.Name] = struct{}{}
	}

	var taken []string
	for _, descriptor := range p.spec.Tools {
		if _, inRegistry := exposed[descriptor.Name]; inRegistry || toolauth.IsGateable(descriptor.Name) {
			taken = append(taken, descriptor.Name)
		}
	}
	sort.Strings(taken)
	return taken
}

// closeInstance performs the wazero close behind every place an Instance is
// released: the pool's discard of a dead instance, and the closes its drain
// does — which, since activation keeps no instance of its own, covers every
// guest instance a plugin ever has. It is a package-level function value rather
// than a direct call to *Instance.Close only so tests can substitute a failing
// implementation and prove that a close failure is reported instead of dropped
// (TestActivateReportsAFailureWhileRollingBack,
// TestActivateCarriesARollbackFailureOutOfAPanic,
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
//  5. create the instance pool every call into this plugin is served from, and
//     file its drain;
//  6. read the guest's self-description via abi.OpManifest — on an instance from
//     that pool, so no guest instance ever exists outside it;
//  7. cross-check that self-description against spec;
//  8. file the entry that reaches the contribution side (see below), and
//     contribute the plugin's tools (see contributeTools), which files one
//     registry entry and one gateable-catalog entry per tool.
//
// The resulting entries are split across TWO owners. owner holds the wasm
// resources — runtime, instance pool — plus one entry, labelled
// ledgerLabelContributions, whose disposer disposes ToolsOwner(owner); that
// second owner holds the per-tool entries. The split is what lets the
// contributions be withdrawn on their own while the guest keeps running
// (Plugin.Suspend), and the entry that bridges them is what keeps
// ledger.DisposeOwner(owner) sufficient for callers that know only that one
// owner. It is filed after both wasm entries precisely so reverse-order
// disposal runs it first: tools withdrawn, then the pool drained, then the
// runtime closed.
//
// The self-description is read and cross-checked deliberately AFTER the pool is
// filed and before the contribution: a plugin whose host manifest disagrees with
// its guest is exactly the failure that must exercise the rollback, so the
// rollback path cannot rot into dead code
// (TestActivateFilesThePoolBeforeReadingTheManifest pins that ordering).
//
// Reading the manifest through the pool rather than from a separate instance is
// what keeps a plugin's instance count at spec.MaxInstances instead of
// 1 + MaxInstances: an activation-owned instance would sit idle for the plugin's
// whole life while every call was served from the pool, doubling a
// MaxInstances=1 plugin's guest memory for nothing.
//
// Owner exclusivity is a precondition, not a courtesy: owner must belong to
// this activation alone. Activate refuses to start if
// ledger.Snapshot()[owner] is already non-empty, naming the owner and the
// entries already filed under it, rather than silently tearing them down. A
// caller reloading a plugin under a stable owner (a hot-reload keeping
// "plugin:foo" across activations, for instance) MUST dispose the previous
// activation before calling Activate again with the same owner. The same holds
// for ToolsOwner(owner), which this activation disposes wholesale and which is
// therefore checked too.
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
// Plugin.
//
// None is dropped on a PANIC either, and that path takes a different shape
// because there is no error return to join onto. The contribution step is
// fail-loud on a tool name another contributor already owns, and the rollback
// runs on the way out of that panic; if the rollback itself fails, Activate
// recovers, wraps the original panic value together with the joined rollback
// failures in an *ActivationRollbackError, and panics with THAT. So a panicking
// activation leaves nothing filed, and a rollback failure during the panic —
// a wazero runtime that refused to close, a pool that would not converge — is
// carried out with the panic instead of disappearing into a return value nobody
// will ever read. A panic whose rollback succeeded propagates unchanged.
//
// Once Activate has returned, the plugin IS its tools: they are in
// spec.Registry, they are gateable, and every call to one of them is served by
// the instance pool through the handler contributeTools registered. Revoking all
// of that — tools, gateable entries, pool (drained), runtime, in that order — is
// ledger.DisposeOwner(owner) and nothing else, the second owner included.
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
	// The contribution side is part of the same exclusivity: this activation's
	// teardown disposes that owner wholesale (see ledgerLabelContributions), so
	// entries already sitting there would be revoked by a plugin that never
	// filed them, and until then the snapshot would attribute them to it.
	toolsOwner := ToolsOwner(owner)
	if held := ledger.Snapshot()[toolsOwner]; len(held) != 0 {
		return nil, fmt.Errorf("activate plugin %q: contribution owner %s already holds %v; it is derived from "+
			"owner %s and must be exclusive to this activation, which disposes it wholesale",
			spec.Name, toolsOwner, held, owner)
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
	// keep records one handle, and every step below files through it. It is also
	// what contributeTools is handed, because that step's failure mode is a panic
	// part-way through a list of tools: collecting each handle as it is filed is
	// what lets the rollback revoke the tools registered before the failing one.
	keep := func(revoke func() error) { revokers = append(revokers, revoke) }

	committed := false
	defer func() {
		if committed {
			return
		}
		var rollbackErrs []error
		for i := len(revokers) - 1; i >= 0; i-- {
			if derr := revokers[i](); derr != nil {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("roll back activation of plugin %q: %w", spec.Name, derr))
			}
		}
		joined := errors.Join(rollbackErrs...)
		if joined == nil {
			// Nothing to report. recover() is deliberately NOT called on this
			// path: a panic whose rollback succeeded must propagate exactly as it
			// was thrown.
			return
		}
		if p := recover(); p != nil {
			// The named return is never delivered while a panic unwinds, so
			// joining onto it here would drop the rollback failure at the one
			// moment it matters most (the plugin's runtime may still be open).
			// Carry both out instead — see ActivationRollbackError.
			panic(&ActivationRollbackError{Panic: p, Rollback: joined})
		}
		err = errors.Join(err, joined)
	}()

	rt := NewRuntime(ctx, spec.MemoryPages)
	// Filed before anything is compiled into it: from here on the runtime holds
	// resources that leak unless it is closed, and every following step can
	// fail. Closing the runtime also closes the host module and any instance
	// created from it, which is why it is filed FIRST and therefore disposed
	// LAST.
	keep(ledger.Add(owner, ledgerLabelRuntime, func() error {
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

	// The pool's instances are built lazily, by the calls that need them, so its
	// factory must NOT hold ctx: activation's context is typically the request
	// that loaded the plugin, and it is long dead by the time a model calls one
	// of the plugin's tools. wazero would then either refuse to instantiate or
	// close the fresh module immediately (see NewRuntime's
	// WithCloseOnContextDone), so every tool call would fail for a reason that
	// has nothing to do with the call.
	// This is the same cancellation scrubbing disposeCtx does, for a different
	// reason, so it gets its own name rather than borrowing that one.
	instantiateCtx := context.WithoutCancel(ctx)
	instances := newPool(spec.MaxInstances, func() (*Instance, error) {
		return NewInstance(instantiateCtx, rt, compiled)
	})
	// Filed AFTER the runtime precisely so that reverse-order disposal runs it
	// BEFORE it: drain converges the calls that are inside the guest right now,
	// and closing the runtime underneath them would truncate a tool call in
	// flight. The wait is bounded — see drainDeadline for how the bound is chosen
	// so that it composes with each in-flight call's own timeout — and expiry is
	// a reported failure: the error reaches whoever called DisposeOwner, because
	// a drain that did not converge means guest work outlived the plugin and
	// that must be recorded, never shrugged off.
	drainBound := drainDeadline(spec.Tools)
	keep(ledger.Add(owner, ledgerLabelPool, func() error {
		drainCtx, cancel := context.WithTimeout(disposeCtx, drainBound)
		defer cancel()
		if derr := instances.drain(drainCtx); derr != nil {
			return fmt.Errorf("drain instance pool of plugin %q (waited %s): %w", spec.Name, drainBound, derr)
		}
		return nil
	}))

	// Read and cross-check AFTER the pool's drain is filed: a mismatch must be a
	// failure with something to roll back (see the doc comment above, and
	// TestActivateFilesThePoolBeforeReadingTheManifest, which pins it).
	//
	// The read goes through the pool, so the instance it builds is the same one
	// the plugin's tool calls will use rather than an extra one activation keeps
	// alive forever. instances.call takes the pool's acquire/release discipline
	// with it (see pool.call).
	manifest, err := readManifest(ctx, instances)
	if err != nil {
		return nil, fmt.Errorf("activate plugin %q: %w", spec.Name, err)
	}
	if err := crossCheck(spec, manifest); err != nil {
		return nil, fmt.Errorf("activate plugin %q: %w", spec.Name, err)
	}

	// The link to the contribution side, filed under the instance owner AFTER
	// both wasm entries so that reverse disposal withdraws the tools first, then
	// drains the pool, then closes the runtime. It is what keeps
	// DisposeOwner(owner) sufficient once the contributions live under a second
	// owner: the Loader, the CLI drain and ServeResult.Close all know only this
	// one.
	//
	// It is filed BEFORE anything is contributed, not after, so there is never
	// an instant at which entries exist under the contribution owner with
	// nothing pointing at them — a panicking contribution (a duplicate tool
	// name) would otherwise be reachable only by a caller that already knew to
	// look there.
	keep(ledger.Add(owner, ledgerLabelContributions, func() error {
		if derr := ledger.DisposeOwner(toolsOwner); derr != nil {
			return fmt.Errorf("withdraw contributions of plugin %q: %w", spec.Name, derr)
		}
		return nil
	}))

	// Contribution is the last step, so a tool name another contributor already
	// owns rolls back everything above it — including the tools registered
	// before the failing one. It files under the contribution owner, which is
	// what lets Plugin.Suspend withdraw exactly this much and nothing else.
	contributeTools(ledger, toolsOwner, spec, instances, keep)

	committed = true
	return &Plugin{
		Name:     spec.Name,
		Manifest: manifest,
		pool:     instances,
		ledger:   ledger,
		owner:    owner,
		spec:     spec,
	}, nil
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
		case descriptor.Timeout <= 0:
			// No default here for the same reason MemoryPages and MaxInstances
			// have none, plus a sharper one: Registry.Execute applies a timeout
			// only when the descriptor carries a positive one, so this is the ONLY
			// bound on a call inside the guest. Without it a plugin call that never
			// returns runs forever and holds up the pool drain teardown waits for
			// (see drainDeadline).
			return fmt.Errorf("activate plugin %q: Spec.Tools[%d] (%q) has Timeout %s; a plugin tool needs a "+
				"positive timeout with no default, because it is the only bound on a call inside the guest "+
				"and an unbounded one would also block this plugin's teardown",
				spec.Name, i, descriptor.Name, descriptor.Timeout)
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
//
// It takes a guestCaller rather than an *Instance because activation reads the
// manifest through the plugin's own instance pool: the pool is the only place a
// guest instance lives (see Activate), and pool.call is what keeps its
// acquire/release discipline.
func readManifest(ctx context.Context, guest guestCaller) (Manifest, error) {
	body, err := guest.call(ctx, abi.OpManifest, nil)
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
