// Package loader converges the running plugin set toward a deployment's
// declared target state.
//
// A deployment manifest (internal/plugin/manifest's Deployment) says which
// plugins should be installed and how. This package holds that target state
// against what is actually mounted and closes the gap: activate what is new,
// unload what is gone or disabled, replace what changed, and — the case that
// gives Apply its shape — put the previous instance back when its replacement
// fails to come up.
//
// Three properties are worth stating up front, because they are what the
// convergence is FOR:
//
//   - An entry that did not change is not touched. Restarting an unchanged
//     plugin would drop whatever the guest holds in its linear memory and pay a
//     fresh instantiation for nothing, so "no change" means no action at all,
//     not a cheap reload.
//
//   - A failure never leaves the target state half-applied and unreported. One
//     entry's failure does not abort the others, and Apply returns the joined
//     failures of everything that went wrong (fail-loud: no entry is skipped
//     silently, and no error is swallowed to keep the convergence tidy).
//
//   - A replacement that cannot be activated brings the previous instance back.
//     host.Activate rolls back everything IT filed, so a failed activation
//     leaves nothing behind — but the old instance was already disposed to free
//     its owner, and that is this package's to undo (see §5.4 of the plugin
//     system design). If the previous instance cannot be brought back either,
//     both failures travel out together and the plugin/activation_failed event
//     says the old instance was NOT restored.
//
// WHEN a convergence lands is not this package's own judgement either: Apply
// hands the whole convergence to the task-boundary gate (internal/taskgate's
// TaskGate), which runs it only with no task in flight. A task therefore keeps
// the capability catalog it started with, and a new target state reaches only
// the tasks that start after it.
//
// What this package does NOT do: it does not read plugins.json (that is
// manifest.ParseDeployment) and does not format status for a human (that is the
// CLI's). Apply is a synchronous, serialized operation; Status is a snapshot of
// what it left behind.
package loader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/host"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/plugin/sign"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/toolauth"
)

// The runtime event types a convergence publishes, matching the design doc's
// §8 diagnostics surface. They are exported because they are the contract a
// consumer selects on — the CLI's status view and any event subscriber — the
// same way host.RuntimeEventCallFailed is.
//
// The payload of each travels inside domain.RuntimeEvent.Message as
// `key=value` pairs (see the format* functions below), because RuntimeEvent
// carries no per-event structured payload and inventing one for plugins alone
// would fork the event schema.
const (
	// RuntimeEventLoaded reports one plugin activated: name, version, sha256,
	// the capabilities actually granted, the tools it contributed, the ledger
	// owner everything it filed lives under, and whether this was a fresh mount
	// or the restoration of a previous instance (reason=).
	RuntimeEventLoaded = "plugin/loaded"

	// RuntimeEventUnloaded reports one plugin unmounted, with the reason it
	// went away, how many ledger entries were revoked with it, and the
	// disposal's own failure if it had one (error=).
	RuntimeEventUnloaded = "plugin/unloaded"

	// RuntimeEventActivationFailed reports one plugin that did not come up:
	// which step failed, how many ledger entries were rolled back, and whether
	// a previous instance was restored.
	RuntimeEventActivationFailed = "plugin/activation_failed"
)

// The reasons a plugin is unmounted, as they appear in a
// RuntimeEventUnloaded message.
//
// reasonManifestRemoved and reasonDisabled are two spellings of the same
// ACTION — an entry that is disabled is unmounted exactly like one that was
// deleted — kept apart only so the event says which of the two an operator
// did.
const (
	reasonManifestRemoved = "manifest-removed"
	reasonDisabled        = "disabled"
	reasonReplaced        = "replaced"
)

// The reasons a plugin is mounted, as they appear in a RuntimeEventLoaded
// message.
//
// A restoration is called out because it reads backwards otherwise: it is
// published next to the plugin/activation_failed that FORCED it, and an
// operator tailing the stream has to be able to tell "the replacement came up"
// from "the replacement did not come up and the old instance is back".
const (
	loadReasonMounted  = "mounted"
	loadReasonRestored = "restored"
)

// The convergence steps a single entry passes through, as they appear in a
// RuntimeEventActivationFailed message. Naming the step is what lets an
// operator tell "the entry points outside the deployment root" (source) from
// "the package on disk is wrong" (load-package) from "the deployment's
// authorization is wrong" (assemble-spec) from "the plugin itself would not
// come up" (activate).
const (
	stepSource       = "source"
	stepLoadPackage  = "load-package"
	stepIdentity     = "identity"
	stepAssembleSpec = "assemble-spec"
	stepDependencies = "dependencies"
	stepFingerprint  = "fingerprint"
	stepToolNames    = "tool-names"
	stepActivate     = "activate"
)

// Whether a failed activation's previous instance came back, as it appears in
// a RuntimeEventActivationFailed message. The three values are distinct on
// purpose: "there was nothing to restore" is not the same answer as "there was,
// and it is running again", nor as "there was, and it is gone".
const (
	restoredYes  = "yes"
	restoredNo   = "no"
	restoredNone = "n/a"
)

// The states an InstanceStatus reports.
const (
	// StateLoaded means the plugin is mounted right now. Its LastError may
	// still be non-empty, in two cases: a replacement that failed and was
	// rolled back leaves the previous instance running AND the failure that
	// forced the rollback visible; and a replacement that came up after its
	// predecessor's disposal FAILED carries that disposal failure, because a
	// wasm runtime that would not close is a leak an operator has to be able to
	// see from the status.
	StateLoaded = "loaded"

	// StateSuspended means the plugin IS mounted — same wasm runtime, same
	// instance pool, same guest state — but its contributions are withdrawn
	// because a tool it requires cannot be resolved. SuspendedBy names those
	// tools. It is a state of its own rather than a flavour of StateFailed
	// because nothing failed: the plugin is waiting for a dependency, and it
	// comes back on its own the moment one arrives.
	StateSuspended = "suspended"

	// StateFailed means the entry is in the target state but nothing is
	// mounted for it, and LastError says why.
	StateFailed = "failed"
)

// Config is everything a Loader needs. Every field except DeployLimits and
// Keyring is required; New reports a missing one by name rather than defaulting
// it, because each missing field would turn into a nil dereference or a
// silently unrecorded convergence at the first Apply.
//
// The two exceptions are exceptions for opposite reasons: a zero DeployLimits
// is simply "this deployment sets no ceiling of its own", while a nil Keyring
// is a POLICY STATEMENT ("this deployment does not require signatures") that
// New cannot tell apart from a forgotten field — which is why the field's own
// doc puts the burden of never letting nil arise by accident on the caller.
type Config struct {
	// Ledger is where every activation files its revocation handles, and what
	// an unload disposes. It is the single source of "what is actually
	// mounted" that survives this process's own bookkeeping.
	Ledger *lifecycle.Ledger

	// Deps builds the host dependencies for one plugin. It is a function
	// rather than a value because two of host.Deps' fields differ per plugin —
	// the plugin's name and its deployment-supplied config JSON — and because
	// it keeps the manifest package (which assembles everything else in the
	// Spec) from having to know what a *tool.Registry or an *http.Client is.
	//
	// The Deps it returns must carry a non-nil Tools registry: it is both
	// where the plugin's tools are registered (host.Spec.Registry, which the
	// Loader wires from it) and what a granted tool capability calls out
	// through. An entry whose Deps has none fails with the field named.
	Deps func(name string, cfg json.RawMessage) host.Deps

	// Events receives the plugin/loaded, plugin/unloaded and
	// plugin/activation_failed events of every convergence.
	Events port.EventBus

	// Logger records the same convergence decisions in the log: Info for a
	// mount or unmount, Error for a failure. It is required — a nil *slog.Logger
	// panics on first use, and a Loader that logged nothing would make a
	// convergence that half-happened unexplainable from the logs alone.
	Logger *slog.Logger

	// Gate is the task-boundary gate every convergence lands through: Apply
	// waits on it until no task is running, so a task that has started keeps the
	// capability catalog it started with. It is REQUIRED.
	//
	// A nil Gate is a wiring error, not "converge immediately". The mid-task
	// change it would allow is not a stray tool error: the tool registry
	// resolves handlers at call time, so a tool this Loader deregisters stops
	// working inside a task whose prompt still advertises it, and the changed
	// capability list invalidates the model's prompt prefix — a measured reload
	// zeroed one provider's cache hits and cut another's from 1792 to 768. A
	// default that silently skipped the gate would make that damage a
	// forgotten-field away. It must be the same gate the runtimes running those
	// tasks were built with; a gate of its own would wait for a boundary nobody
	// is standing at.
	Gate *taskgate.TaskGate

	// ApplyWait is how long Apply waits for the tasks already running to finish
	// before giving up. It is REQUIRED and must be positive.
	//
	// A non-positive value is a configuration error rather than "wait forever":
	// forever is not a policy a caller can recover from — an apply that never
	// returns is indistinguishable from a wedged one, and it would hold the gate
	// shut against every new task for as long as it lasted. When the wait
	// expires, NOTHING is applied and the failure says how many tasks were in
	// the way.
	ApplyWait time.Duration

	// Keyring is the deployment's trust set: the public keys a plugin
	// package's detached signature (plugin.sig) must verify against before
	// anything is mounted from it. It is handed to manifest.LoadPackage on
	// every convergence.
	//
	// A nil Keyring means THIS DEPLOYMENT DOES NOT REQUIRE SIGNATURES, and it
	// is the one field here whose absence is a legitimate configuration rather
	// than a wiring mistake — which is exactly why it is dangerous, and why
	// the caller that builds this Config must let nil arise ONLY from an
	// explicit "not required" statement. A nil that arrived because a keyring
	// file could not be read, or because a field was forgotten, is a security
	// control that switched itself off while the logs looked normal; the
	// assembly is required to fail loudly in those cases instead of passing
	// nil (see cli.resolvePluginKeyring, the only place that decides this).
	//
	// nil does NOT relax manifest.LoadPackage's sha256 check, which runs
	// either way.
	Keyring *sign.Keyring

	// DeployLimits is the deployment's resource ceiling, applied to every
	// plugin by manifest.AssembleSpec (each limit is min(plugin's request,
	// this), with zero on either side meaning "not declared"). Its zero value
	// is therefore legitimate — it means the deployment sets no ceiling of its
	// own and every plugin's own limits stand.
	DeployLimits manifest.Limits
}

// InstanceStatus is what one deployment entry actually came to, for
// diagnostics: the answer to "the plugin is in the manifest, so why is nothing
// happening?".
//
// Version is the version the plugin package declares. It is empty only when the
// entry failed before its package could be read.
type InstanceStatus struct {
	// Name is the deployment entry's name, which is also the plugin's own name
	// (Apply refuses an entry where the two disagree).
	Name string

	// Version is the version from the plugin's own manifest — the same version
	// that goes into the ledger owner.
	Version string

	// State is StateLoaded, StateSuspended or StateFailed.
	State string

	// Tools are the tool names this instance contributed, empty for a failed
	// entry. A StateSuspended entry still reports them: they are the tools it
	// contributes when it can work, and the ones that come back when its
	// dependency does — the State field is what says they are withdrawn right
	// now.
	Tools []string

	// SuspendedBy names the tools this plugin requires that nothing resolves,
	// which is WHY it is suspended. It is empty for every other state.
	SuspendedBy []string

	// LastError is the most recent failure involving this entry, empty if
	// there has not been one. It is populated for a StateLoaded entry too: see
	// StateLoaded.
	LastError string
}

// instance is one mounted plugin, as the Loader remembers it.
type instance struct {
	name    string
	version string
	owner   lifecycle.Owner

	// spec is the exact Spec this instance was activated from, RETAINED
	// (including the wasm bytes) so a failed replacement can be rolled back to
	// it. Re-reading the package from disk instead would be no rollback at all:
	// the reason the replacement failed is usually that the bytes on disk are
	// the new ones.
	spec host.Spec

	// fingerprint is what "the entry did not change" is decided on; see
	// fingerprintOf.
	fingerprint string

	sha256    string
	tools     []string
	lastError string

	// plugin is the activation's own handle, kept because suspending and
	// resuming are its methods: the Loader decides WHICH plugins may work, and
	// host.Plugin is what actually withdraws and re-files their contributions.
	// Every path that puts an instance into l.instances sets it.
	plugin *host.Plugin

	// requires is the plugin's declared dependency on other plugins' tools
	// (manifest.PluginManifest.Requires), which is what the dependency graph is
	// built from. It is part of the fingerprint, so an operator who edits it
	// gets a remount rather than an instance still running the old declaration.
	requires []string

	// suspendedBy names the required tools that are currently unresolved. It is
	// non-empty only while the plugin is suspended, and it is refreshed on every
	// convergence: which dependency is missing can change while the answer
	// "suspended" does not.
	suspendedBy []string
}

// failure is one entry that is in the target state with nothing mounted for it.
type failure struct {
	version string
	err     string
}

// Loader converges the running plugin set toward a target state. Use New to
// build one; the zero value is not usable.
//
// Every exported method takes the Loader's lock, so Apply is serialized against
// itself and against Status. Apply mounts and unmounts real wasm instances and
// is therefore slow; that is deliberate — two convergences running at once
// would race each other over the same tool names.
type Loader struct {
	ledger       *lifecycle.Ledger
	deps         func(name string, cfg json.RawMessage) host.Deps
	events       port.EventBus
	logger       *slog.Logger
	deployLimits manifest.Limits
	gate         *taskgate.TaskGate
	applyWait    time.Duration

	// keyring is Config.Keyring, verbatim: the deployment's trust set, or nil
	// when it has explicitly stated that it does not require signatures. See
	// Config.Keyring for why nil may only ever mean that.
	keyring *sign.Keyring

	mu        sync.Mutex
	instances map[string]*instance
	failures  map[string]failure
}

// New builds a Loader from cfg, reporting a missing dependency by field name.
//
// It returns an error rather than panicking because it is called from serve
// assembly, where a wrapped error naming the field is more useful than a stack
// trace.
func New(cfg Config) (*Loader, error) {
	switch {
	case cfg.Ledger == nil:
		return nil, errors.New("new plugin loader: Config.Ledger is nil; there is nowhere to file what an activation creates")
	case cfg.Deps == nil:
		return nil, errors.New("new plugin loader: Config.Deps is nil; a plugin cannot be activated without its host dependencies")
	case cfg.Events == nil:
		return nil, errors.New("new plugin loader: Config.Events is nil; a convergence that published nothing would be invisible")
	case cfg.Logger == nil:
		return nil, errors.New("new plugin loader: Config.Logger is nil; a convergence that logged nothing would be unexplainable")
	case cfg.Gate == nil:
		return nil, errors.New("new plugin loader: Config.Gate is nil; a convergence with no task-boundary gate would land in the middle of a running task")
	case cfg.ApplyWait <= 0:
		return nil, fmt.Errorf("new plugin loader: Config.ApplyWait is %s; it must be positive, "+
			"since an apply that waits forever for a task boundary holds the gate shut against every new task", cfg.ApplyWait)
	}
	if cfg.Keyring == nil {
		// The one state this constructor cannot tell apart from a mistake, said
		// out loud once per Loader. A deployment that verifies nothing is a
		// legitimate choice; a deployment that verifies nothing because a field
		// was forgotten looks exactly the same from in here, and the difference
		// has to be visible somewhere an operator can find it.
		cfg.Logger.Warn("plugin signature verification is disabled",
			"component", "plugin-loader",
			"consequence", "packages are accepted on their sha256 alone, which travels inside the very manifest it describes")
	}
	return &Loader{
		ledger:       cfg.Ledger,
		deps:         cfg.Deps,
		events:       cfg.Events,
		logger:       cfg.Logger,
		deployLimits: cfg.DeployLimits,
		gate:         cfg.Gate,
		applyWait:    cfg.ApplyWait,
		keyring:      cfg.Keyring,
		instances:    make(map[string]*instance),
		failures:     make(map[string]failure),
	}, nil
}

// SignaturePolicy is a Loader's signature-verification policy in comparable
// form: whether a trust set is in force at all, and exactly which keys are in
// it.
//
// It exists for one caller: a command that re-reads the deployment config
// while a Loader is already running (`agent plugins reload` does) has to be
// able to tell whether the policy it just read is the policy the running
// Loader was BUILT with. The keyring is frozen when serve assembles the
// Loader, so converging a new manifest without that comparison would apply the
// operator's new manifest under their old trust set — a signature policy that
// looks applied and is not.
type SignaturePolicy struct {
	// Enforced is whether this Loader verifies signatures at all. False is the
	// deployment's deliberate "signatures are not required" statement, which
	// is the only way a nil keyring may arise (see Config.Keyring).
	Enforced bool

	// KeyIDs are the ids of the trusted keys, sorted (sign.Keyring.IDs). It is
	// empty exactly when Enforced is false: sign.ParseKeyring refuses an empty
	// trust set, so an enforcing policy always names at least one key.
	KeyIDs []sign.KeyID
}

// SignaturePolicyOf describes the policy a Loader built with keyring enforces.
// A nil keyring — the one legitimate "this deployment does not require
// signatures" value — is the unenforced policy.
//
// It is exported so that a caller which has resolved a keyring from a config
// but has not built a Loader from it (reload does exactly that) computes the
// policy through the same function Loader.SignaturePolicy uses, rather than
// growing a second, drifting idea of what a policy is.
func SignaturePolicyOf(keyring *sign.Keyring) SignaturePolicy {
	if keyring == nil {
		return SignaturePolicy{}
	}
	return SignaturePolicy{Enforced: true, KeyIDs: keyring.IDs()}
}

// Equal reports whether p and other are the same policy: the same enforcement
// state over the same set of key ids. Key order does not matter in principle,
// but both sides come from sign.Keyring.IDs, which sorts — so this compares
// element by element rather than paying for a set.
func (p SignaturePolicy) Equal(other SignaturePolicy) bool {
	if p.Enforced != other.Enforced || len(p.KeyIDs) != len(other.KeyIDs) {
		return false
	}
	for i, id := range p.KeyIDs {
		if id != other.KeyIDs[i] {
			return false
		}
	}
	return true
}

// String renders p for an operator reading an error message: the enforcement
// state first, because that is the part that decides whether anything is
// checked at all, then the trusted key ids so a changed trust set is visible
// rather than merely asserted.
func (p SignaturePolicy) String() string {
	if !p.Enforced {
		return "signatures not required"
	}
	ids := make([]string, 0, len(p.KeyIDs))
	for _, id := range p.KeyIDs {
		ids = append(ids, string(id))
	}
	return fmt.Sprintf("signatures required, trusted keys [%s]", strings.Join(ids, " "))
}

// SignaturePolicy returns the policy this Loader is enforcing right now.
//
// No lock is taken: keyring is written once in New and never again, so there
// is nothing here for a concurrent Apply to race with. The returned KeyIDs
// slice is freshly built by sign.Keyring.IDs on every call, so a caller cannot
// reach into the Loader's trust set through it.
func (l *Loader) SignaturePolicy() SignaturePolicy {
	return SignaturePolicyOf(l.keyring)
}

// Apply converges the running plugin set toward dep, resolving each entry's
// Source against root.
//
// The convergence is:
//
//	entry is new (and not disabled)          -> activate it
//	entry is gone from dep                   -> unload it (manifest-removed)
//	entry is present with "enabled": false   -> unload it (disabled), the
//	                                            identical action
//	entry's content changed                  -> unload the old, activate the new
//	entry's content is unchanged             -> nothing at all
//
// "Content" is the plugin package's sha256 and version plus the entry's own
// grant, accepted tools and config — see fingerprintOf for why each is in
// there.
//
// It runs in three passes, and the split is the load-bearing part:
//
//  1. Every desired entry's package is read, checked, assembled and fingerprinted.
//     Nothing running is touched, so an entry whose package is broken fails on
//     its own and leaves its running instance alone.
//  2. EVERY unload this convergence performs runs — both the entries that left
//     the target state and the old instances of entries whose content changed.
//  3. Every entry that needs one is activated.
//
// Pass 2 is why a tool name moving from one plugin to another converges in a
// SINGLE Apply: every name this convergence frees is free before any activation
// claims one, whether the name is being released by an entry that is going away
// or by an entry that is merely being replaced. "Run it twice and it settles"
// is not what a convergence function may ask of an operator.
//
// Order within a pass is the deployment's own entry order (unloads go in sorted
// name order, since a mounted instance has no deployment position of its own),
// so a convergence is reproducible and its event stream reads in the order the
// operator wrote the manifest.
//
// Every failure is reported and none aborts the rest: each entry is converged
// independently and Apply returns errors.Join of everything that went wrong,
// each error naming the entry it belongs to. A returned error therefore means
// "the target state is not fully applied, and here is every reason why" — never
// "nothing happened".
//
// The one failure that stops Apply before it touches anything is a target state
// that is not a target state: an empty root (every relative Source resolves
// against it) or two entries claiming the same name (which of the two is
// supposed to be running would be decided by iteration order). Both are
// rejected before the gate is involved at all — a manifest that cannot be
// applied is no reason to make anybody's tasks wait for a boundary.
//
// Everything after those checks happens at a task boundary, with no task in
// flight, as ONE gated step (Config.Gate, Config.ApplyWait). Two consequences
// are worth stating:
//
//   - Apply BLOCKS until the tasks already running finish, up to ApplyWait. If
//     that wait expires, or ctx ends first, Apply reports an error naming how
//     many tasks were in the way and NOTHING is applied — not the entries it
//     could have converged, not a partial pass. The target state is unchanged
//     and the call can be retried at a calmer moment.
//   - While Apply waits and converges, a task that tries to START is refused
//     with taskgate.ErrApplyPending rather than joining a plugin set that is
//     mid-change.
//
// Two Apply calls do not queue behind each other either: while one holds the
// gate a second is refused with an error rather than waiting for its turn. The
// convergence belongs to whoever asked for it first, and a caller that was told
// nothing about the target state it asked for is better served by an error than
// by a wait of unknown length.
func (l *Loader) Apply(ctx context.Context, dep manifest.Deployment, root string) error {
	if root == "" {
		return errors.New("apply plugin deployment: root is empty; every entry's source resolves against it")
	}

	declared := make(map[string]bool, len(dep.Plugins))
	desired := make(map[string]bool, len(dep.Plugins))
	wanted := make([]manifest.Entry, 0, len(dep.Plugins))
	for _, entry := range dep.Plugins {
		if declared[entry.Name] {
			return fmt.Errorf("apply plugin deployment: plugin %q appears twice; "+
				"the target state must name each plugin unambiguously", entry.Name)
		}
		declared[entry.Name] = true
		if !entry.Enabled {
			continue
		}
		desired[entry.Name] = true
		wanted = append(wanted, entry)
	}

	// The WHOLE convergence goes inside one gate acquisition, not one per entry.
	// A single Apply may unload A and then install B; between those two steps
	// the plugin set is neither the old one nor the new one, and no task may
	// ever observe that. Per-entry gating would publish exactly that
	// intermediate state to any task that started between two entries.
	return l.gate.ApplyAtBoundary(ctx, l.applyWait, func() error {
		return l.converge(ctx, wanted, declared, desired, root)
	})
}

// converge is Apply's three passes, run at a task boundary with no task in
// flight. It is separate from Apply so that the target state is validated
// before anybody's tasks are paused for it, and so that the gate wraps every
// pass together rather than each one on its own.
//
// declared names every entry in the target state (enabled or not), desired only
// the enabled ones, and wanted is the enabled entries in the manifest's own
// order. All three are derived from the same deployment by Apply.
func (l *Loader) converge(ctx context.Context, wanted []manifest.Entry, declared, desired map[string]bool, root string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	var errs []error

	// A recorded failure outlives the Apply that produced it so that "this
	// plugin is not running, and here is why" stays answerable — but only while
	// the entry is still in the target state. An entry the operator removed or
	// disabled has no failure to report any more.
	for name := range l.failures {
		if !desired[name] {
			delete(l.failures, name)
		}
	}

	// Pass 1: work out what each desired entry needs, touching nothing.
	plans := make([]*convergePlan, 0, len(wanted))
	planFor := make(map[string]*convergePlan, len(wanted))
	for _, entry := range wanted {
		plan, err := l.prepare(ctx, entry, root)
		if err != nil {
			errs = append(errs, fmt.Errorf("converge plugin %q: %w", entry.Name, err))
			continue
		}
		if plan == nil {
			// Unchanged: leave the running instance exactly as it is. Rebuilding
			// it would discard whatever the guest holds in memory and pay a fresh
			// instantiation for no change at all.
			//
			// Its recorded explanation is NOT left as it is, though. prepare
			// returns nil only for an entry whose package was read, verified,
			// assembled and fingerprinted cleanly just now, which disproves
			// whatever an earlier convergence recorded against it — a tampered
			// plugin.wasm, a signature that did not verify, a manifest that
			// would not assemble. Left in place that note would never come off:
			// an unchanged entry is never activated again, and activate is the
			// only other place that clears it, so an operator who fixed the
			// package and reloaded successfully would go on reading the failure
			// they had already fixed on every `agent plugins status`.
			//
			// A SUSPENDED instance is skipped. Its note explains a withdrawal
			// that is still in force, and convergeDependencies deliberately does
			// not rewrite it for a plugin that was already suspended before this
			// convergence began.
			if inst := l.mounted(entry.Name); !inst.plugin.Suspended() {
				inst.lastError = ""
			}
			continue
		}
		plans = append(plans, plan)
		planFor[entry.Name] = plan
	}

	// Pass 2: free everything this convergence frees — the entries that left the
	// target state AND the previous instances of the entries that changed —
	// before pass 3 activates anything. A replaced instance's own unload belongs
	// here and not next to its activation, so that a tool name it releases is
	// available to whichever entry claims it next, in this same Apply.
	mounted := make([]string, 0, len(l.instances))
	for name := range l.instances {
		mounted = append(mounted, name)
	}
	sort.Strings(mounted)
	for _, name := range mounted {
		plan := planFor[name]
		var reason string
		switch {
		case plan != nil && plan.prev != nil:
			reason = reasonReplaced
		case desired[name]:
			continue
		case declared[name]:
			reason = reasonDisabled
		default:
			reason = reasonManifestRemoved
		}
		inst := l.instances[name]
		delete(l.instances, name)
		revoked, err := l.unload(ctx, inst, reason)
		if plan != nil {
			// The replacement carries its predecessor's disposal failure: it is
			// how many entries a rollback would have to put back, and — if the
			// disposal failed — a leak that has to stay visible on whatever ends
			// up mounted under this name.
			plan.revoked = revoked
			plan.unloadErr = err
			continue
		}
		if err != nil {
			errs = append(errs, err)
		}
	}

	// Pass 3: activate, now that every name this Apply frees is free.
	for _, plan := range plans {
		if err := l.activate(ctx, plan); err != nil {
			errs = append(errs, fmt.Errorf("converge plugin %q: %w", plan.entry.Name, err))
		}
	}

	// Pass 4: with the mounted set settled, decide which of those plugins can
	// actually work and suspend or resume each one accordingly (see
	// convergeDependencies). It runs last because it reads the mounted set the
	// three passes above produced, and it runs inside the same gate for the
	// reason the rest does: a plugin mounted by pass 3 and suspended here was
	// never advertised to a task in between.
	if err := l.convergeDependencies(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Status reports what every entry the Loader has seen actually came to, sorted
// by name. Entries that were unloaded are not reported: they are not supposed
// to be running, so they are not a diagnosis.
func (l *Loader) Status() []InstanceStatus {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]InstanceStatus, 0, len(l.instances)+len(l.failures))
	for name := range l.instances {
		// Read through mounted so an instance with no host.Plugin behind it
		// trips the same invariant here as it does in the convergence, instead
		// of being reported as a healthy "loaded": a status that answers
		// "everything is fine" for a half-built instance is worse than no
		// status at all, and the two call sites must not disagree about what a
		// mounted plugin is.
		inst := l.mounted(name)
		// The plugin itself is the authority on whether its contributions are
		// currently withdrawn; the Loader's own note only says why.
		state := StateLoaded
		var suspendedBy []string
		if inst.plugin.Suspended() {
			state = StateSuspended
			suspendedBy = append([]string(nil), inst.suspendedBy...)
		}
		out = append(out, InstanceStatus{
			Name:        inst.name,
			Version:     inst.version,
			State:       state,
			Tools:       append([]string(nil), inst.tools...),
			SuspendedBy: suspendedBy,
			LastError:   inst.lastError,
		})
	}
	for name, f := range l.failures {
		out = append(out, InstanceStatus{
			Name:      name,
			Version:   f.version,
			State:     StateFailed,
			LastError: f.err,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// convergePlan is one desired entry that needs an activation, with everything
// pass 1 worked out about it. It exists so that "what has to happen" is decided
// before pass 2 tears anything down: a plan is the only thing that carries an
// entry's state between Apply's three passes.
type convergePlan struct {
	entry  manifest.Entry
	dir    string
	pm     manifest.PluginManifest
	spec   host.Spec
	digest string

	// prev is the instance this plan replaces, or nil for an entry that is not
	// mounted yet. A non-nil prev is what tells pass 2 to unload this name and
	// what a failed activation is rolled back to.
	prev *instance

	// revoked and unloadErr are pass 2's answer about prev: how many ledger
	// entries went with it, and whether its disposal reported a failure. Both
	// are zero-valued for a plan with no prev.
	unloadErr error
	revoked   int
}

// prepare works out what one entry needs, without touching anything that is
// running. It returns nil, nil when the entry did not change. It is called with
// l.mu held.
//
// The package is read, checked and assembled BEFORE anything running is torn
// down, which is what makes a broken new package safe: a plugin.wasm whose
// digest no longer matches its plugin.json leaves the current instance running
// and reports the failure, instead of unmounting a working plugin for a
// replacement that never existed.
func (l *Loader) prepare(ctx context.Context, entry manifest.Entry, root string) (*convergePlan, error) {
	dir, err := packageDir(entry.Name, root, entry.Source)
	if err != nil {
		return nil, l.fail(ctx, entry.Name, "", stepSource, err, nil)
	}

	// l.keyring is the deployment's trust set, or nil when the deployment has
	// EXPLICITLY stated that it does not require signatures (Config.Keyring).
	// Either way LoadPackage's sha256 check runs; a non-nil keyring adds the
	// signature check, whose failure is an ordinary activation failure — it
	// goes through l.fail like every other one, so the entry lands in
	// StateFailed with a LastError naming the signature, and the other entries
	// keep converging.
	pm, wasm, err := manifest.LoadPackage(dir, l.keyring)
	if err != nil {
		return nil, l.fail(ctx, entry.Name, "", stepLoadPackage, err, nil)
	}
	if pm.Name != entry.Name {
		// The deployment entry's name and the plugin's own name are two
		// spellings of one identity, and the Loader keys its whole convergence
		// off the entry's. Letting them differ would let two entries mount the
		// same plugin, whose second contribution of the same tool name is a
		// panic in the registry rather than an error anyone can report.
		mismatch := fmt.Errorf("deployment entry %q loads plugin %q from %s; an entry must be named after "+
			"the plugin it installs", entry.Name, pm.Name, dir)
		return nil, l.fail(ctx, entry.Name, pm.Version, stepIdentity, mismatch, nil)
	}

	spec, err := manifest.AssembleSpec(pm, entry, l.deployLimits)
	if err != nil {
		return nil, l.fail(ctx, entry.Name, pm.Version, stepAssembleSpec, err, nil)
	}
	spec.Wasm = wasm

	// AssembleSpec deliberately leaves Deps and Registry empty — the manifest
	// package has no business knowing those types exist — so wiring them is
	// this package's job. Registry comes from Deps.Tools: one registry, which
	// the plugin's tools enter and which its own call_tool calls go out
	// through.
	deps := l.deps(entry.Name, entry.Config)
	if deps.Tools == nil {
		missing := fmt.Errorf("plugin %q: Config.Deps returned host.Deps with a nil Tools registry; "+
			"it is where the plugin's tools are registered, so activating it would mount a plugin "+
			"nothing can call", entry.Name)
		return nil, l.fail(ctx, entry.Name, pm.Version, stepDependencies, missing, nil)
	}
	spec.Deps = deps
	spec.Registry = deps.Tools

	digest, err := fingerprintOf(entry, pm, spec)
	if err != nil {
		return nil, l.fail(ctx, entry.Name, pm.Version, stepFingerprint, err, nil)
	}

	prev := l.instances[entry.Name]
	if prev != nil && prev.fingerprint == digest {
		return nil, nil
	}
	return &convergePlan{entry: entry, dir: dir, pm: pm, spec: spec, digest: digest, prev: prev}, nil
}

// activate mounts one prepared plan. Its predecessor, if it had one, is already
// gone: Apply's pass 2 unloaded it, which is both what frees its owner (a
// same-version replacement reuses it, and host.Activate refuses an owner that
// already holds anything) and what frees its tool names for whoever claims them
// next. It is called with l.mu held.
func (l *Loader) activate(ctx context.Context, plan *convergePlan) error {
	entry := plan.entry

	// Checked here, after pass 2 (so an entry never conflicts with its own
	// predecessor, and a name another entry is releasing in this same Apply is
	// already free) and before host.Activate: a tool name another contributor
	// already owns is fail-loud by PANIC in both the registry and the gateable
	// catalog, and a panic would abort the whole convergence instead of
	// reporting one entry. Two plugins claiming one model-facing name is
	// ordinary operator data, so it has to come back as an error naming both
	// the entry and the names.
	if conflicts := toolNameConflicts(plan.spec); len(conflicts) > 0 {
		clash := fmt.Errorf("plugin %q contributes tool name(s) %v that another contributor already owns; "+
			"one name is one tool", entry.Name, conflicts)
		return l.fail(ctx, entry.Name, plan.pm.Version, stepToolNames, clash, plan)
	}

	owner := ownerFor(plan.pm.Name, plan.pm.Version)
	plugin, err := host.Activate(ctx, l.ledger, owner, plan.spec)
	if err != nil {
		activation := fmt.Errorf("activate plugin %q from %s: %w", entry.Name, plan.dir, err)
		return l.fail(ctx, entry.Name, plan.pm.Version, stepActivate, activation, plan)
	}

	inst := &instance{
		name:        entry.Name,
		version:     plan.pm.Version,
		owner:       owner,
		spec:        plan.spec,
		fingerprint: plan.digest,
		sha256:      plan.pm.SHA256,
		tools:       toolNames(plan.spec.Tools),
		plugin:      plugin,
		requires:    append([]string(nil), plan.pm.Requires...),
	}
	if plan.unloadErr != nil {
		// The replacement came up, but its predecessor did not go down cleanly:
		// only the pool drain and the runtime close can fail a DisposeOwner, so
		// what is left behind is a leaked wasm runtime. It travels out in Apply's
		// error, and it stays on the status row too — an operator looking at
		// "state=loaded" has to be able to see it.
		inst.lastError = plan.unloadErr.Error()
	}
	l.instances[entry.Name] = inst
	delete(l.failures, entry.Name)
	l.logger.Info("plugin loaded",
		"plugin", inst.name, "version", inst.version, "owner", string(inst.owner), "tools", inst.tools)
	l.publish(ctx, RuntimeEventLoaded, formatLoadedMessage(inst, loadReasonMounted))
	return plan.unloadErr
}

// fail records one entry's failure, restores the previous instance if the
// failure took one down, publishes plugin/activation_failed and returns
// everything that went wrong. It is called with l.mu held.
//
// plan is the entry's convergence plan, or nil when the failure happened in
// pass 1 — before anything was touched, so there is nothing to restore and no
// earlier failure to carry. From a non-nil plan the failure inherits the
// instance it must bring back (plan.prev), how many ledger entries were revoked
// when that instance was unmounted — i.e. how many this failure has to put back
// — and the unload's own failure if it had one, which is joined in ahead of the
// cause because it happened first. (The activation's OWN rollback is not
// counted: host.Activate rolls back everything it filed and reports no count,
// and inventing one would be a number nobody measured.)
func (l *Loader) fail(
	ctx context.Context,
	name, version, step string,
	cause error,
	plan *convergePlan,
) error {
	var errs []error
	var prev *instance
	revoked := 0
	if plan != nil {
		if plan.unloadErr != nil {
			errs = append(errs, plan.unloadErr)
		}
		prev = plan.prev
		revoked = plan.revoked
	}
	errs = append(errs, cause)

	restored := restoredNone
	var restoredInstance *instance
	if prev != nil {
		if err := l.restore(ctx, prev); err != nil {
			errs = append(errs, err)
			restored = restoredNo
		} else {
			restored = restoredYes
			restoredInstance = prev
		}
	}
	joined := errors.Join(errs...)

	// A running instance keeps its failure on itself; only an entry with
	// nothing mounted becomes a failure record. Both cases are one entry in
	// Status, never two.
	if running, ok := l.instances[name]; ok {
		running.lastError = joined.Error()
	} else {
		l.failures[name] = failure{version: version, err: joined.Error()}
	}

	l.logger.Error("plugin activation failed",
		"plugin", name, "version", version, "step", step,
		"rolled_back", revoked, "restored", restored, "error", joined)
	l.publish(ctx, RuntimeEventActivationFailed,
		formatActivationFailedMessage(name, version, step, revoked, restored, joined))

	// The restore's own plugin/loaded goes out AFTER the failure that forced it,
	// so the stream reads unloaded(replaced) -> activation_failed -> loaded, and
	// carries reason=restored. Published the other way round an operator sees a
	// load immediately followed by a failure and blames the load that succeeded.
	if restoredInstance != nil {
		l.publish(ctx, RuntimeEventLoaded, formatLoadedMessage(restoredInstance, loadReasonRestored))
	}
	return joined
}

// restore re-activates a previous instance from the Spec it was mounted from,
// under its own owner (which the unload freed). It is called with l.mu held.
//
// It does not publish: its plugin/loaded is fail's to send, after the
// plugin/activation_failed that explains why a restore was needed at all.
//
// A restore is not guaranteed to be possible. One Apply frees every name it
// frees before it claims any (converge's pass 2 before pass 3), so between the
// unload that took prev down and the failure that asks for it back, another
// entry in the same convergence may legitimately have taken a tool name prev
// held. Re-activating over that name is fail-loud by PANIC in the registry and
// in the gateable catalog, and a panic on the ROLLBACK path would kill the
// process mid-teardown — in a state that is neither the old deployment nor the
// new one. So restore runs activate's own pre-flight and reports a taken name
// as an error: fail joins it and publishes restored=no, which is the honest
// account of what happened.
func (l *Loader) restore(ctx context.Context, prev *instance) error {
	if conflicts := toolNameConflicts(prev.spec); len(conflicts) > 0 {
		return fmt.Errorf("restore previous instance of plugin %q (owner %s): tool name(s) %v are now "+
			"owned by another contributor", prev.name, prev.owner, conflicts)
	}
	plugin, err := host.Activate(ctx, l.ledger, prev.owner, prev.spec)
	if err != nil {
		return fmt.Errorf("restore previous instance of plugin %q (owner %s): %w", prev.name, prev.owner, err)
	}
	// The restoration is a NEW activation, so it comes with a new host.Plugin:
	// the handle the instance carried before was marked disposed by the unload
	// that took it down, and suspending or resuming through it would be refused
	// for a plugin that is running again. A restore also re-files the
	// contributions, so what comes back is never suspended.
	prev.plugin = plugin
	prev.suspendedBy = nil
	l.instances[prev.name] = prev
	// Defensive: a name cannot be in failures and instances at once today
	// (fail only writes a failure record when nothing is mounted, and this
	// insertion happens first), but the invariant is implicit, and a duplicate
	// Status row for one entry would be a diagnosis that contradicts itself.
	delete(l.failures, prev.name)
	l.logger.Info("previous plugin instance restored",
		"plugin", prev.name, "version", prev.version, "owner", string(prev.owner))
	return nil
}

// unload disposes everything one instance filed and reports how many ledger
// entries went with it — across BOTH owners the activation files under (see
// host.ToolsOwner), because "revoked" is read as how much this unload took
// down. It is called with l.mu held.
//
// The event is published whether or not the disposal reported a failure: the
// plugin IS unmounted either way (lifecycle.Ledger.DisposeOwner clears the
// owner even when a disposer fails), and a failure that reached nobody would be
// the worst of both. The event carries the failure in its error= field, so an
// operator reading the event stream sees the same thing the returned error
// says.
func (l *Loader) unload(ctx context.Context, inst *instance, reason string) (int, error) {
	// Both owners, counted from ONE snapshot: an activation files its wasm
	// resources and the link to its contributions under inst.owner, and one
	// entry per tool per half (registry, gateable catalog) under
	// host.ToolsOwner(inst.owner). Counting only the instance owner would report
	// the same 3 for every plugin no matter how many tools went away with it —
	// a number that no longer means what the field says it means, in an event
	// operators read to see what an unload actually took down.
	snapshot := l.ledger.Snapshot()
	revoked := len(snapshot[inst.owner]) + len(snapshot[host.ToolsOwner(inst.owner)])
	disposeErr := l.ledger.DisposeOwner(inst.owner)

	if disposeErr != nil {
		l.logger.Error("plugin unloaded with failures",
			"plugin", inst.name, "version", inst.version, "owner", string(inst.owner),
			"reason", reason, "revoked", revoked, "error", disposeErr)
	} else {
		l.logger.Info("plugin unloaded",
			"plugin", inst.name, "version", inst.version, "owner", string(inst.owner),
			"reason", reason, "revoked", revoked)
	}
	l.publish(ctx, RuntimeEventUnloaded, formatUnloadedMessage(inst, reason, revoked, disposeErr))

	if disposeErr != nil {
		return revoked, fmt.Errorf("unload plugin %q (owner %s, reason %s): %w",
			inst.name, inst.owner, reason, disposeErr)
	}
	return revoked, nil
}

// publish sends one runtime event, logging at Error level if the bus refused
// it. A convergence is not failed by an event that could not be published — the
// plugin really is mounted or unmounted — but the loss is recorded rather than
// swallowed, the same stance the host's own denial events take.
func (l *Loader) publish(ctx context.Context, eventType, message string) {
	event := domain.RuntimeEvent{Type: eventType, Message: message, CreatedAt: time.Now()}
	if err := l.events.Publish(ctx, event); err != nil {
		l.logger.Error("plugin loader event was not published",
			"type", eventType, "event_message", message, "error", err)
	}
}

// ownerFor renders the ledger owner one activation files everything under.
//
// The version is part of the owner so that changing versions is naturally two
// DIFFERENT owners, which is what host.Activate's owner-exclusivity contract
// wants: the new instance never has to wait for the old owner to be free. A
// same-version replacement reuses the owner and therefore depends on the old
// instance being disposed first — which converge does.
func ownerFor(name, version string) lifecycle.Owner {
	return lifecycle.Owner("plugin:" + name + "@" + version)
}

// packageDir resolves one entry's Source against the deployment root, refusing
// anything that would read plugin code from outside it.
//
// root is what bounds where plugin code comes from, and a bound that only holds
// when the operator happens to write well-behaved paths is not a bound: an
// absolute source ignores root entirely, and a relative one containing ".."
// walks out of it (filepath.Join cleans a path, it does not confine it). Both
// are refused by name, naming the entry and the source, rather than silently
// loading a module from wherever the string pointed — a plugin's wasm is code
// that runs, so where it is read from is a trust decision.
func packageDir(name, root, source string) (string, error) {
	if filepath.IsAbs(source) {
		return "", fmt.Errorf("plugin %q: source %q is absolute; a plugin source must be relative to the "+
			"deployment root %s, which is what bounds where plugin code is read from", name, source, root)
	}
	dir := filepath.Join(root, source)
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", fmt.Errorf("plugin %q: source %q cannot be resolved against the deployment root %s: %w",
			name, source, root, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("plugin %q: source %q escapes the deployment root %s (it resolves to %s); "+
			"plugin code is only read from inside the root", name, source, root, dir)
	}
	return dir, nil
}

// fingerprintInput is everything "did this entry change?" is decided on. It is
// a struct rather than a concatenated string so that adding a field to the
// decision is a compile-time-visible change and cannot collide with a
// neighbouring value.
type fingerprintInput struct {
	// Source, SHA256 and Version come from the package: a different module, a
	// different build of the same module, or a different version is a
	// different plugin.
	Source  string
	SHA256  string
	Version string

	// Grant, Tools, MaxInstances and MemoryPages are the assembled Spec, which
	// is where the deployment's grant, accepted tools and resource ceilings end
	// up. Taking them from the Spec rather than from the raw entry means an
	// override that assembles to the same thing is correctly seen as no change.
	Grant        perm.Grant
	Tools        []tool.Descriptor
	MaxInstances int
	MemoryPages  uint32

	// Requires is the plugin's declared dependency on other plugins' tools. It
	// changes nothing about the module, so without it here an operator who
	// edited "requires" in plugin.json would get no remount at all and the
	// dependency graph would keep resolving the declaration the running
	// instance was mounted with.
	Requires []string

	// Config is the entry's configuration JSON, verbatim. It is not part of
	// the Spec (it reaches the plugin through Deps) and a plugin reads it once,
	// at activation, so a changed config must remount. Comparison is by bytes:
	// a reformatted-but-equivalent config counts as a change, which errs
	// towards one unnecessary remount rather than towards a plugin left running
	// with configuration the operator has already replaced.
	Config string
}

// fingerprintOf reduces one converged entry to the digest "unchanged" is
// decided on. A value it cannot encode is an error, never a fingerprint that
// silently ignores a field: two entries that hashed the same because encoding
// failed would look unchanged forever.
func fingerprintOf(entry manifest.Entry, pm manifest.PluginManifest, spec host.Spec) (string, error) {
	data, err := json.Marshal(fingerprintInput{
		Source:       entry.Source,
		SHA256:       pm.SHA256,
		Version:      pm.Version,
		Grant:        spec.Grant,
		Tools:        spec.Tools,
		MaxInstances: spec.MaxInstances,
		MemoryPages:  spec.MemoryPages,
		Requires:     pm.Requires,
		Config:       string(entry.Config),
	})
	if err != nil {
		return "", fmt.Errorf("fingerprint plugin %q: %w", entry.Name, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// toolNameConflicts lists the names in spec.Tools that are already taken — by
// another registered tool or by an entry in the gateable catalog (which
// includes the built-in tools) — in spec.Tools' own order.
//
// It is a pre-flight check for host.Activate's contribution step, not a
// substitute for it: the registry and the catalog remain the authority, and
// this only turns the one case an operator can cause into an error before the
// authority turns it into a panic.
func toolNameConflicts(spec host.Spec) []string {
	taken := make(map[string]bool)
	for _, descriptor := range spec.Registry.Descriptors() {
		taken[descriptor.Name] = true
	}
	var conflicts []string
	for _, descriptor := range spec.Tools {
		if taken[descriptor.Name] || toolauth.IsGateable(descriptor.Name) {
			conflicts = append(conflicts, descriptor.Name)
		}
	}
	return conflicts
}

// toolNames lists the descriptors' names, in registration order.
func toolNames(descriptors []tool.Descriptor) []string {
	names := make([]string, 0, len(descriptors))
	for _, d := range descriptors {
		names = append(names, d.Name)
	}
	return names
}

// grantedCapabilities names the capabilities a Grant actually authorizes, in a
// fixed order, for the plugin/loaded event's payload.
func grantedCapabilities(g perm.Grant) []string {
	var names []string
	for _, c := range []struct {
		name    string
		granted bool
	}{
		{"log", g.Log}, {"config", g.Config}, {"kv", g.KV},
		{"http", g.HTTP}, {"fs", g.FS}, {"tool", g.Tool},
	} {
		if c.granted {
			names = append(names, c.name)
		}
	}
	return names
}

// formatLoadedMessage renders a RuntimeEventLoaded payload. reason is
// loadReasonMounted or loadReasonRestored.
func formatLoadedMessage(inst *instance, reason string) string {
	return fmt.Sprintf("plugin=%s version=%s sha256=%s owner=%s reason=%s capabilities=[%s] tools=[%s]",
		inst.name, inst.version, inst.sha256, inst.owner, reason,
		strings.Join(grantedCapabilities(inst.spec.Grant), " "), strings.Join(inst.tools, " "))
}

// formatUnloadedMessage renders a RuntimeEventUnloaded payload. disposeErr is
// the disposal's own failure, or nil; the field is present either way (empty
// when the unload was clean) so the payload has one shape a consumer can parse,
// the same way plugin/activation_failed's error= always is.
func formatUnloadedMessage(inst *instance, reason string, revoked int, disposeErr error) string {
	text := ""
	if disposeErr != nil {
		text = strings.ReplaceAll(disposeErr.Error(), "\n", "; ")
	}
	return fmt.Sprintf("plugin=%s version=%s reason=%s revoked=%d error=%s",
		inst.name, inst.version, reason, revoked, text)
}

// formatActivationFailedMessage renders a RuntimeEventActivationFailed
// payload. The error's own newlines (errors.Join separates with them) are
// folded into "; " so one failure stays one event line.
func formatActivationFailedMessage(name, version, step string, rolledBack int, restored string, err error) string {
	return fmt.Sprintf("plugin=%s version=%s step=%s rolled_back=%d restored=%s error=%s",
		name, version, step, rolledBack, restored,
		strings.ReplaceAll(err.Error(), "\n", "; "))
}
