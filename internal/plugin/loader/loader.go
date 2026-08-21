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
// What this package does NOT do: it does not read plugins.json (that is
// manifest.ParseDeployment), does not decide WHEN a convergence may land (that
// is the task-boundary gate, wired in a later task), and does not format status
// for a human (that is the CLI's). Apply is a synchronous, serialized
// operation; Status is a snapshot of what it left behind.
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
	"github.com/stardust/legion-agent/internal/port"
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
	// the capabilities actually granted, the tools it contributed, and the
	// ledger owner everything it filed lives under.
	RuntimeEventLoaded = "plugin/loaded"

	// RuntimeEventUnloaded reports one plugin unmounted, with the reason it
	// went away and how many ledger entries were revoked with it.
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

// The convergence steps a single entry passes through, as they appear in a
// RuntimeEventActivationFailed message. Naming the step is what lets an
// operator tell "the package on disk is wrong" (load-package) from "the
// deployment's authorization is wrong" (assemble-spec) from "the plugin itself
// would not come up" (activate).
const (
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
	// still be non-empty: a replacement that failed and was rolled back leaves
	// the previous instance running AND the failure that forced the rollback
	// visible.
	StateLoaded = "loaded"

	// StateFailed means the entry is in the target state but nothing is
	// mounted for it, and LastError says why.
	StateFailed = "failed"
)

// Config is everything a Loader needs. Every field except DeployLimits is
// required; New reports a missing one by name rather than defaulting it,
// because each missing field would turn into a nil dereference or a silently
// unrecorded convergence at the first Apply.
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

	// State is StateLoaded or StateFailed.
	State string

	// Tools are the tool names this instance contributed, empty for a failed
	// entry.
	Tools []string

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
	}
	return &Loader{
		ledger:       cfg.Ledger,
		deps:         cfg.Deps,
		events:       cfg.Events,
		logger:       cfg.Logger,
		deployLimits: cfg.DeployLimits,
		instances:    make(map[string]*instance),
		failures:     make(map[string]failure),
	}, nil
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
// Unloads run first, before any activation, so a tool name that moves from one
// plugin to another in the same convergence is free by the time its new owner
// claims it.
//
// Order within a step is the deployment's own entry order, so a convergence is
// reproducible and its event stream reads in the order the operator wrote the
// manifest.
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
// supposed to be running would be decided by iteration order).
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

	l.mu.Lock()
	defer l.mu.Unlock()

	var errs []error

	mounted := make([]string, 0, len(l.instances))
	for name := range l.instances {
		mounted = append(mounted, name)
	}
	sort.Strings(mounted)
	for _, name := range mounted {
		if desired[name] {
			continue
		}
		reason := reasonManifestRemoved
		if declared[name] {
			reason = reasonDisabled
		}
		inst := l.instances[name]
		delete(l.instances, name)
		if _, err := l.unload(ctx, inst, reason); err != nil {
			errs = append(errs, err)
		}
	}
	// A recorded failure outlives the Apply that produced it so that "this
	// plugin is not running, and here is why" stays answerable — but only while
	// the entry is still in the target state. An entry the operator removed or
	// disabled has no failure to report any more.
	for name := range l.failures {
		if !desired[name] {
			delete(l.failures, name)
		}
	}

	for _, entry := range wanted {
		if err := l.converge(ctx, entry, root); err != nil {
			errs = append(errs, fmt.Errorf("converge plugin %q: %w", entry.Name, err))
		}
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
	for _, inst := range l.instances {
		out = append(out, InstanceStatus{
			Name:      inst.name,
			Version:   inst.version,
			State:     StateLoaded,
			Tools:     append([]string(nil), inst.tools...),
			LastError: inst.lastError,
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

// converge brings one entry to its target state. It is called with l.mu held.
//
// The package is read, checked and assembled BEFORE anything running is torn
// down, which is what makes a broken new package safe: a plugin.wasm whose
// digest no longer matches its plugin.json leaves the current instance running
// and reports the failure, instead of unmounting a working plugin for a
// replacement that never existed.
func (l *Loader) converge(ctx context.Context, entry manifest.Entry, root string) error {
	dir := packageDir(root, entry.Source)

	pm, wasm, err := manifest.LoadPackage(dir)
	if err != nil {
		return l.fail(ctx, entry.Name, "", stepLoadPackage, err, nil, 0)
	}
	if pm.Name != entry.Name {
		// The deployment entry's name and the plugin's own name are two
		// spellings of one identity, and the Loader keys its whole convergence
		// off the entry's. Letting them differ would let two entries mount the
		// same plugin, whose second contribution of the same tool name is a
		// panic in the registry rather than an error anyone can report.
		mismatch := fmt.Errorf("deployment entry %q loads plugin %q from %s; an entry must be named after "+
			"the plugin it installs", entry.Name, pm.Name, dir)
		return l.fail(ctx, entry.Name, pm.Version, stepIdentity, mismatch, nil, 0)
	}

	spec, err := manifest.AssembleSpec(pm, entry, l.deployLimits)
	if err != nil {
		return l.fail(ctx, entry.Name, pm.Version, stepAssembleSpec, err, nil, 0)
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
		return l.fail(ctx, entry.Name, pm.Version, stepDependencies, missing, nil, 0)
	}
	spec.Deps = deps
	spec.Registry = deps.Tools

	digest, err := fingerprintOf(entry, pm, spec)
	if err != nil {
		return l.fail(ctx, entry.Name, pm.Version, stepFingerprint, err, nil, 0)
	}

	prev := l.instances[entry.Name]
	if prev != nil && prev.fingerprint == digest {
		// Unchanged: leave the running instance exactly as it is. Rebuilding it
		// would discard whatever the guest holds in memory and pay a fresh
		// instantiation for no change at all.
		return nil
	}

	var errs []error
	revoked := 0
	if prev != nil {
		// The old instance goes first, and not only for tidiness: its owner
		// carries the version, so a same-version replacement would reuse it,
		// and host.Activate refuses an owner that already holds anything.
		delete(l.instances, entry.Name)
		n, err := l.unload(ctx, prev, reasonReplaced)
		revoked = n
		if err != nil {
			// The tools and the gateable entries are revoked even by a
			// DisposeOwner that reports a failure (only the pool drain and the
			// runtime close can fail), so the new instance can still be
			// activated. Report the failure and carry on converging.
			errs = append(errs, err)
		}
	}

	// Checked here, after the previous instance is gone (its own names are free
	// again) and before host.Activate: a tool name another contributor already
	// owns is fail-loud by PANIC in both the registry and the gateable catalog,
	// and a panic would abort the whole convergence instead of reporting one
	// entry. Two plugins claiming one model-facing name is ordinary operator
	// data, so it has to come back as an error naming both the entry and the
	// names.
	if conflicts := toolNameConflicts(spec); len(conflicts) > 0 {
		clash := fmt.Errorf("plugin %q contributes tool name(s) %v that another contributor already owns; "+
			"one name is one tool", entry.Name, conflicts)
		errs = append(errs, l.fail(ctx, entry.Name, pm.Version, stepToolNames, clash, prev, revoked))
		return errors.Join(errs...)
	}

	owner := ownerFor(pm.Name, pm.Version)
	if _, err := host.Activate(ctx, l.ledger, owner, spec); err != nil {
		activation := fmt.Errorf("activate plugin %q from %s: %w", entry.Name, dir, err)
		errs = append(errs, l.fail(ctx, entry.Name, pm.Version, stepActivate, activation, prev, revoked))
		return errors.Join(errs...)
	}

	inst := &instance{
		name:        entry.Name,
		version:     pm.Version,
		owner:       owner,
		spec:        spec,
		fingerprint: digest,
		sha256:      pm.SHA256,
		tools:       toolNames(spec.Tools),
	}
	l.instances[entry.Name] = inst
	delete(l.failures, entry.Name)
	l.logger.Info("plugin loaded",
		"plugin", inst.name, "version", inst.version, "owner", string(inst.owner), "tools", inst.tools)
	l.publish(ctx, RuntimeEventLoaded, formatLoadedMessage(inst))
	return errors.Join(errs...)
}

// fail records one entry's failure, restores the previous instance if the
// failure took one down, publishes plugin/activation_failed and returns
// everything that went wrong. It is called with l.mu held.
//
// prev is the instance this failure unmounted and must therefore bring back, or
// nil when the failure happened before anything was touched. revoked is how
// many ledger entries were revoked when prev was unmounted — i.e. how many this
// failure has to put back. (The activation's OWN rollback is not counted here:
// host.Activate rolls back everything it filed and reports no count, and
// inventing one would be a number nobody measured.)
func (l *Loader) fail(
	ctx context.Context,
	name, version, step string,
	cause error,
	prev *instance,
	revoked int,
) error {
	errs := []error{cause}
	restored := restoredNone
	if prev != nil {
		if err := l.restore(ctx, prev); err != nil {
			errs = append(errs, err)
			restored = restoredNo
		} else {
			restored = restoredYes
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
	return joined
}

// restore re-activates a previous instance from the Spec it was mounted from,
// under its own owner (which the unload freed). It is called with l.mu held.
func (l *Loader) restore(ctx context.Context, prev *instance) error {
	if _, err := host.Activate(ctx, l.ledger, prev.owner, prev.spec); err != nil {
		return fmt.Errorf("restore previous instance of plugin %q (owner %s): %w", prev.name, prev.owner, err)
	}
	l.instances[prev.name] = prev
	l.logger.Info("previous plugin instance restored",
		"plugin", prev.name, "version", prev.version, "owner", string(prev.owner))
	l.publish(ctx, RuntimeEventLoaded, formatLoadedMessage(prev))
	return nil
}

// unload disposes everything one instance filed and reports how many ledger
// entries went with it. It is called with l.mu held.
//
// The event is published whether or not the disposal reported a failure: the
// plugin IS unmounted either way (lifecycle.Ledger.DisposeOwner clears the
// owner even when a disposer fails), and a failure that reached nobody would be
// the worst of both.
func (l *Loader) unload(ctx context.Context, inst *instance, reason string) (int, error) {
	revoked := len(l.ledger.Snapshot()[inst.owner])
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
	l.publish(ctx, RuntimeEventUnloaded, formatUnloadedMessage(inst, reason, revoked))

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

// packageDir resolves one entry's Source. A relative source is resolved against
// the deployment root; an absolute one is taken as it stands, since joining it
// onto the root would silently produce a path the operator never wrote.
func packageDir(root, source string) string {
	if filepath.IsAbs(source) {
		return filepath.Clean(source)
	}
	return filepath.Join(root, source)
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

// formatLoadedMessage renders a RuntimeEventLoaded payload.
func formatLoadedMessage(inst *instance) string {
	return fmt.Sprintf("plugin=%s version=%s sha256=%s owner=%s capabilities=[%s] tools=[%s]",
		inst.name, inst.version, inst.sha256, inst.owner,
		strings.Join(grantedCapabilities(inst.spec.Grant), " "), strings.Join(inst.tools, " "))
}

// formatUnloadedMessage renders a RuntimeEventUnloaded payload.
func formatUnloadedMessage(inst *instance, reason string, revoked int) string {
	return fmt.Sprintf("plugin=%s version=%s reason=%s revoked=%d",
		inst.name, inst.version, reason, revoked)
}

// formatActivationFailedMessage renders a RuntimeEventActivationFailed
// payload. The error's own newlines (errors.Join separates with them) are
// folded into "; " so one failure stays one event line.
func formatActivationFailedMessage(name, version, step string, rolledBack int, restored string, err error) string {
	return fmt.Sprintf("plugin=%s version=%s step=%s rolled_back=%d restored=%s error=%s",
		name, version, step, rolledBack, restored,
		strings.ReplaceAll(err.Error(), "\n", "; "))
}
