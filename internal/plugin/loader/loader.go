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
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/fetch"
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

	// RuntimeEventUnloadLeaked reports an unload whose wait for in-flight
	// calls ran out: the plugin is out of every registry, and guest work is
	// still running inside a runtime nobody owns any more. It is the design
	// doc's plugin/unload_leaked (§8).
	RuntimeEventUnloadLeaked = "plugin/unload_leaked"

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

	// reasonHealth is the unload a plugin brings on itself: consecutive
	// call faults past the deployment's threshold (see PluginHealthConfig).
	// It is spelled out as a distinct reason because it is the only unload
	// NOBODY asked for — an operator reading plugin/unloaded needs to tell it
	// from a manifest edit at a glance.
	reasonHealth = "health"

	// reasonRevoked is the unload a WITHDRAWN ENDORSEMENT forces: the entry is
	// still enabled, its package still reads, and what changed is that this
	// deployment now revokes the key that endorses it.
	//
	// It is spelled out as a distinct reason because the entry it names is one
	// the operator still has enabled in their manifest: reporting "disabled"
	// or "manifest-removed" for an entry they never touched would send them to
	// the wrong file, and the remedy is a different one too — a package a live
	// key endorses, not a manifest edit.
	reasonRevoked = "revoked"
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
// "the remote artifact could not be obtained, or was not the artifact its
// digest names" (fetch) from
// "the package on disk is wrong" (load-package) from "the deployment's
// authorization is wrong" (assemble-spec) from "the plugin itself would not
// come up" (activate).
const (
	stepSource       = "source"
	stepFetch        = "fetch"
	stepLoadPackage  = "load-package"
	stepIdentity     = "identity"
	stepAssembleSpec = "assemble-spec"
	stepConfigSchema = "config-schema"
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

// TrustSet reports the trust set one mount judges a package against.
//
// It is a function rather than a value because the fetched trust list refreshes
// in the background while this process runs: a set captured when the Loader was
// assembled would leave a revocation that arrived after startup with no way to
// reach the plugins it was published to stop, until somebody restarted the
// agent. Reading it afresh on every convergence that has anything to judge
// costs one merge and buys "an emergency revocation lands without a restart";
// that one reading then serves the whole convergence, for the reason
// trustReading gives.
//
// Returning a zero manifest.TrustInput (no keyring) is a deployment with no
// trust set. See manifest.TrustInput for why that is not the same as unchecked.
// Returning an error is a deployment that does not KNOW what it trusts, which
// is not the same as either: the mount fails.
type TrustSet func() (manifest.TrustInput, error)

// Config is everything a Loader needs. Every field except DeployLimits, Remote,
// LocalKeyring and RequireSignature is required; New reports a missing one by
// name rather than defaulting it, because each missing field would turn into a
// nil dereference or a silently unrecorded convergence at the first Apply.
//
// The four exceptions are exceptions for different reasons: a zero DeployLimits
// is simply "this deployment sets no ceiling of its own"; a zero Remote is the
// deployment whose entries are all local (validateRemote checks its other
// fields only once a cache is configured); a nil LocalKeyring is "no keyring
// file is configured", which decides nothing on its own; and a false
// RequireSignature is a POLICY STATEMENT ("this deployment does not require an
// endorsement") that New cannot tell apart from a forgotten field — which is
// why it is announced at Warn instead.
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

	// MaxConsecutiveFaults is how many consecutive health-relevant call
	// failures (see host.ClassifyCallFault) one mounted plugin may produce
	// before this Loader unloads it. It comes from
	// config.PluginHealthConfig.MaxConsecutiveFaults and must be positive:
	// zero is refused by New rather than read as "never unload".
	MaxConsecutiveFaults int

	// TrustSet is where every mount reads the trust set it judges a package
	// against: the public keys whose signature over plugin.json makes a
	// package one a registered publisher endorses. It is handed to
	// manifest.LoadPackage on every convergence, freshly, and it is REQUIRED —
	// see the TrustSet type for why it is a function and what its two
	// non-answers mean.
	//
	// A trust set that recognises no key does NOT relax
	// manifest.LoadPackage's sha256 check, which runs either way, and it does
	// not by itself let an unendorsed package mount either: that is
	// RequireSignature's and Entry.AcceptedUnsigned's decision.
	TrustSet TrustSet

	// LocalKeyring is the deployment's LOCAL keyring configuration — the
	// keyring document on this machine, as read, as opposed to whatever
	// TrustSet answers with on a given mount.
	//
	// It decides nothing about whether a package may mount; that judgement
	// reads TrustSet. Its whole job is to be reported through SignaturePolicy,
	// which is why "no keyring file is configured" is an ordinary nil here
	// rather than a policy statement.
	//
	// A nil here therefore says one thing only: no keyring document is
	// configured. It is NOT the place to express RequireSignature, which has a
	// field of its own — a nil standing in for "no endorsement is required"
	// would make SignaturePolicy the zero policy for every such deployment, and
	// a policy that is the zero value however the keyring document reads cannot
	// report a change to it.
	LocalKeyring *sign.Keyring

	// RequireSignature is whether a package no registered publisher endorses
	// needs an install-time acceptance (Entry.AcceptedUnsigned) before it may
	// mount.
	//
	// False is a deployment's explicit statement that it does not require an
	// endorsement, and it is honoured for exactly that: unendorsed packages
	// mount, with a warning every time one is admitted. It does NOT extend to
	// revoked keys — "I do not require an endorsement" and "I am willing to
	// run code that was withdrawn" are different sentences, and only the first
	// one was said. Nor does it extend to an acceptance that no longer matches: that
	// says the package CHANGED, which is not a statement about endorsements at
	// all.
	//
	// Its zero value is the permissive side, which New cannot tell apart from
	// a forgotten field — so New says so at Warn.
	RequireSignature bool

	// Remote is how an entry whose source is a URL gets its package onto disk:
	// the cache it is filed in, the client it is fetched with, the bounds both
	// steps run under, and whether plaintext sources are permitted at all.
	//
	// Its zero value is a legitimate deployment — one whose entries are all
	// local — and NOT a fallback: a Loader with no cache refuses a remote
	// entry instead of downloading it somewhere of its own choosing. See
	// RemoteConfig.
	Remote RemoteConfig

	// DeployLimits is the deployment's resource ceiling, applied to every
	// plugin by manifest.AssembleSpec (each limit is min(plugin's request,
	// this), with zero on either side meaning "not declared"). Its zero value
	// is therefore legitimate — it means the deployment sets no ceiling of its
	// own and every plugin's own limits stand.
	DeployLimits manifest.Limits
}

// RemoteConfig is everything a Loader needs to turn an entry whose source is a
// URL into a package directory: where fetched packages are filed, what fetches
// them, the bounds the download and the unpack run under, and whether a
// plaintext source is permitted at all.
//
// # The zero value means "this deployment has no remote entries"
//
// A nil Cache is not "download somewhere temporary": it is the statement that
// this deployment did not configure a place for downloaded code, and a remote
// entry met under it FAILS, naming what is missing. Where code that is about
// to run is written to disk is a deployment decision; picking a directory here
// would make that decision invisible. A deployment whose entries are all local
// leaves this whole struct zero and behaves exactly as it did before remote
// sources existed.
//
// # What it does NOT relax
//
// AllowInsecureSources relaxes the URL SCHEME and nothing else. Whatever it
// says, a remote entry still carries a mandatory digest, the fetched bytes are
// still verified against that digest before they touch the filesystem, and the
// resulting directory still goes through manifest.LoadPackage — the same
// sha256 check and the same signature verification a local package gets. The
// digest answers "should these bytes be accepted"; the signature answers
// "should this package load". Neither answers the other's question.
type RemoteConfig struct {
	// Cache files fetched packages under the digest that names them, so that a
	// package fetched once is not fetched again. Nil means no remote source is
	// configured — see the type's doc comment.
	Cache *fetch.Cache

	// Client fetches artifacts. It is REQUIRED whenever Cache is set (a nil
	// one panics inside fetch.Fetch) and is deliberately separate from the
	// client a plugin granted the "http" capability calls out with: that one
	// is bounded by the per-call plugin timeout, which has nothing to do with
	// how long an artifact download may take.
	Client *http.Client

	// FetchLimits bounds one artifact download. Every field must be positive
	// whenever Cache is set; there is no "zero means unlimited".
	FetchLimits fetch.Limits

	// UnpackLimits bounds the decompressed archive. Every field must be
	// positive whenever Cache is set.
	UnpackLimits fetch.UnpackLimits

	// AllowInsecureSources permits an entry whose source is "http://". False —
	// the safe side, and the value a deployment that says nothing gets — makes
	// such an entry fail before any request is built. It affects the scheme and
	// nothing else; see the type's doc comment.
	AllowInsecureSources bool
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

	// SuspendedBy names what this plugin requires that nothing resolves,
	// which is WHY it is suspended: tool names as they are, and services
	// under their "service:" prefix so the two are distinguishable. It is
	// empty for every other state.
	SuspendedBy []string

	// ProvidesServices and RequiresServices are the plugin's named-service
	// declarations. They are reported for every state, including suspended:
	// "which capability is this plugin waiting for" and "which capability is
	// it holding" are exactly the questions an operator has when a service
	// chain does not come up, and a row that omitted them would send them to
	// read plugin.json on disk instead.
	ProvidesServices []string
	RequiresServices []string

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

	sha256 string

	// keyID names the key whose endorsement this instance was ACTIVATED under,
	// as manifest.LoadPackage reported it for the package this mount was built
	// from. It is empty for an instance that mounted with no registered
	// endorsement — an unsigned package this deployment accepted, for which
	// manifest.Provenance carries no KeyID. That emptiness is a legitimate
	// mount rather than a value that went missing.
	//
	// It is what a revocation is judged against. Revoking a key withdraws
	// trust from THAT key, so the question pass 2 asks is whether this running
	// instance's own endorsement was withdrawn, not whether some package now
	// sitting in the deployment directory is signed by a revoked key: the two
	// can be different packages signed by different keys. An instance with no
	// keyID is consequently never unloaded by that judgement — a revocation
	// revokes a key, and this instance was endorsed by none.
	keyID sign.KeyID

	tools     []string
	lastError string

	// faults counts CONSECUTIVE health-relevant call failures of THIS mount
	// (see host.ClassifyCallFault). It lives on the instance rather than in a
	// side map so it dies with the mount: a replaced plugin starts clean,
	// which is the only reading that makes sense for "consecutive failures of
	// this instance". Guarded by Loader.mu.
	faults int

	// plugin is the activation's own handle, kept because suspending and
	// resuming are its methods: the Loader decides WHICH plugins may work, and
	// host.Plugin is what actually withdraws and re-files their contributions.
	// Every path that puts an instance into l.instances sets it.
	plugin *host.Plugin

	// providesServices / requiresServices are the plugin's named-service
	// declarations. They feed the SAME dependency convergence requires does
	// (see suspend.go), under a "service:" prefix so the two namespaces cannot
	// collide inside the graph.
	providesServices []string
	requiresServices []string
	// serviceCapabilities maps a provided service's capability names to this
	// plugin's own tools (see manifest.PluginManifest.ServiceCapabilities). It
	// is what ResolveService answers from.
	serviceCapabilities map[string]map[string]string
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

	// unconfirmedDisposal is set when this entry was unloaded because its
	// endorsement was revoked AND that unload's disposal reported a failure:
	// the deployment asked for the revoked plugin's resources to be released
	// and was not told they were.
	//
	// It is carried across convergences (see fail) rather than recomputed,
	// because it is a fact about an unload that already happened and no later
	// convergence can either confirm or disprove it: what it describes is
	// resources whose release was never acknowledged, and nothing a subsequent
	// convergence does asks that question again. A record that dropped it
	// would answer "this plugin is refused" on the second convergence and
	// every one after, which is the answer for a revocation that WAS cleanly
	// unmounted.
	//
	// It outlives this record too. When the entry mounts again — the key is
	// restored, or the operator ships a package a live key endorses — activate
	// moves the note onto the new instance's lastError before clearing this
	// record, so the leak stays on a row that now reads "loaded". A note that
	// stopped at the record would make a remount the one event that erases it,
	// and a remount says nothing about resources the previous mount never
	// released.
	unconfirmedDisposal string
}

// explanation is everything this record has to say, as one string: the failure
// itself and, when there is one, the disposal that was never confirmed.
//
// They are joined here rather than at the point either is recorded so that the
// worse half cannot be overwritten by a later convergence rewriting the first.
func (f failure) explanation() string {
	if f.unconfirmedDisposal == "" {
		return f.err
	}
	return f.err + "; " + f.unconfirmedDisposal
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

	// maxConsecutiveFaults is Config.MaxConsecutiveFaults, the count at which a
	// mounted plugin is unloaded for repeatedly failing to answer.
	maxConsecutiveFaults int

	// trustSet is Config.TrustSet, verbatim: where a mount reads the trust set
	// it judges a package against. It is never nil — New refuses that.
	trustSet TrustSet

	// localKeyring is Config.LocalKeyring, verbatim: the local keyring
	// configuration, in the comparable form SignaturePolicy renders. It
	// decides nothing about whether a package may mount — that judgement is
	// taken in admit, against what trustSet answers.
	localKeyring *sign.Keyring

	// requireSignature is Config.RequireSignature, verbatim.
	requireSignature bool

	// remote is Config.Remote, verbatim. Its zero value is the deployment that
	// configured no remote source at all; see RemoteConfig.
	remote RemoteConfig

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
	case cfg.MaxConsecutiveFaults <= 0:
		return nil, fmt.Errorf("new plugin loader: Config.MaxConsecutiveFaults is %d; it must be positive, "+
			"since zero has no 'never unload' reading: a deployment that tolerates more failures states a "+
			"larger number (see config.PluginHealthConfig)", cfg.MaxConsecutiveFaults)
	case cfg.TrustSet == nil:
		// A deployment states "no endorsement required" with
		// RequireSignature. It has no way to state "do not ask what I trust",
		// so a nil provider is a wiring mistake — and defaulting it to an
		// empty trust set would turn a forgotten field into a deployment in
		// which no package is endorsed by anyone.
		return nil, errors.New("new plugin loader: Config.TrustSet is nil; there is nowhere to read the trust " +
			"set a package is judged against, and an empty one would make every package unendorsed")
	}
	if err := validateRemote(cfg.Remote); err != nil {
		return nil, err
	}
	if !cfg.RequireSignature {
		// The one state this constructor cannot tell apart from a mistake, said
		// out loud once per Loader. A deployment that requires no endorsement is
		// a legitimate choice; a deployment that requires none because a field
		// was forgotten looks exactly the same from in here, and the difference
		// has to be visible somewhere an operator can find it.
		cfg.Logger.Warn("this deployment does not require a publisher endorsement for a plugin package",
			"component", "plugin-loader",
			"consequence", "a package no registered publisher endorses mounts with nothing recorded about who accepted it")
	}
	return &Loader{
		ledger:               cfg.Ledger,
		deps:                 cfg.Deps,
		events:               cfg.Events,
		logger:               cfg.Logger,
		deployLimits:         cfg.DeployLimits,
		gate:                 cfg.Gate,
		applyWait:            cfg.ApplyWait,
		maxConsecutiveFaults: cfg.MaxConsecutiveFaults,
		trustSet:             cfg.TrustSet,
		localKeyring:         cfg.LocalKeyring,
		requireSignature:     cfg.RequireSignature,
		remote:               cfg.Remote,
		instances:            make(map[string]*instance),
		failures:             make(map[string]failure),
	}, nil
}

// validateRemote checks the remote source configuration a configured cache
// makes load-bearing, naming the offending field.
//
// With no cache there is nothing to check: that is the deployment with no
// remote entries, and every other field is unused. With one, each of these
// would otherwise surface far from its cause — a nil Client as a panic inside
// fetch.Fetch, a non-positive limit as a bound that does not bound — so they
// are refused where the wiring happens instead.
func validateRemote(remote RemoteConfig) error {
	if remote.Cache == nil {
		return nil
	}
	switch {
	case remote.Client == nil:
		return errors.New("new plugin loader: Config.Remote.Client is nil while a plugin cache is configured; " +
			"a remote entry cannot be fetched without an HTTP client")
	case remote.FetchLimits.Timeout <= 0:
		return fmt.Errorf("new plugin loader: Config.Remote.FetchLimits.Timeout is %s; it must be positive, "+
			"since a download with no deadline never fails and never finishes", remote.FetchLimits.Timeout)
	case remote.FetchLimits.MaxBytes <= 0:
		return fmt.Errorf("new plugin loader: Config.Remote.FetchLimits.MaxBytes is %d; it must be positive, "+
			"since zero does not mean unlimited: it is the cap on bytes read from a remote source", remote.FetchLimits.MaxBytes)
	case remote.UnpackLimits.MaxEntries <= 0:
		return fmt.Errorf("new plugin loader: Config.Remote.UnpackLimits.MaxEntries is %d; it must be positive",
			remote.UnpackLimits.MaxEntries)
	case remote.UnpackLimits.MaxTotalBytes <= 0:
		return fmt.Errorf("new plugin loader: Config.Remote.UnpackLimits.MaxTotalBytes is %d; it must be positive",
			remote.UnpackLimits.MaxTotalBytes)
	case remote.UnpackLimits.MaxEntryBytes <= 0:
		return fmt.Errorf("new plugin loader: Config.Remote.UnpackLimits.MaxEntryBytes is %d; it must be positive",
			remote.UnpackLimits.MaxEntryBytes)
	}
	return nil
}

// SignaturePolicy is a Loader's SIGNATURE POLICY in comparable form: the local
// keyring configuration — whether one is configured at all and exactly which
// keys are in it, trusted and revoked — together with whether an endorsement is
// required at all.
//
// It exists so that a deployment config re-read while a Loader is already
// running can be compared against the policy that Loader was BUILT with. Both
// halves are frozen when the Loader is constructed and neither can be replaced
// under a running one, so converging a new manifest without that comparison
// would apply it under the OLD policy — a signature policy that looks applied
// and is not.
//
// Both halves are in it because either one alone leaves a change invisible. Drop
// the requirement and a deployment that turns endorsements off over an unchanged
// keyring compares equal — a RELAXATION converging silently. Drop the keyring
// and a revocation added to the keyring document compares equal, which is the
// exact silence a revocation exists to break.
//
// # What it does NOT describe
//
// It says nothing about what Config.TrustSet answers with, and it does not need
// to: TrustSet is read afresh on every mount, so there is no "converged under
// the old trust set" to catch there — a trust set that changed a second ago is
// already in force for the next mount, with or without a reload.
//
// Putting what TrustSet answers in here would make things worse rather than
// more complete. A TrustSet is free to give a different answer on every call —
// that is the whole reason it is a function — so a comparison over it would
// fail whenever a reload happened to land just after it changed: a guard that
// refuses at random is worse than no guard at all, because people learn to
// pass it by reflex and stop reading what it says.
type SignaturePolicy struct {
	// KeyringConfigured is whether a local keyring is configured at all. False
	// means Config.LocalKeyring was nil.
	//
	// It is NOT "endorsements are enforced". Whether a package no registered
	// publisher endorses may mount is RequireSignature's answer, and the two are
	// independent — a deployment may configure a keyring and still not require
	// an endorsement.
	KeyringConfigured bool

	// KeyIDs are the ids of the trusted keys, sorted (sign.Keyring.IDs). It is
	// empty exactly when KeyringConfigured is false: sign.ParseKeyring refuses
	// an empty trust set, so a configured keyring always names at least one
	// key.
	KeyIDs []sign.KeyID
	// RevokedIDs are the ids the keyring has revoked, sorted
	// (sign.Keyring.RevokedIDs).
	//
	// It is part of the policy because a revocation added to a keyring changes
	// nothing else an observer can see: the trusted ids may be identical (a
	// revoked key stays listed so refusals can explain themselves), so without
	// this field `agent plugins reload` would compare the new config against
	// the running process, find them equal, and converge the deployment under
	// the OLD trust set — the revoked key still verifying, with "reload
	// succeeded" on screen. That is the exact silence a revocation exists to
	// break.
	RevokedIDs []sign.KeyID

	// RequireSignature is Config.RequireSignature: whether a package no
	// registered publisher endorses needs an install-time acceptance before it
	// may mount.
	//
	// It is part of the policy because the keyring half cannot stand in for it.
	// A deployment that turns the requirement off while keeping its keyring
	// changes no key id at all, so without this field that edit compares equal
	// and converges with nothing said — and it is a RELAXATION, the direction
	// that must never pass unnoticed.
	RequireSignature bool
}

// SignaturePolicyOf describes the signature policy of a Loader built with
// keyring and requireSignature. A nil keyring — no keyring file configured — is
// the policy that has no local keyring.
//
// keyring must be the local keyring configuration AS READ, never one the
// requirement has already been applied to. A keyring that is nil whenever no
// endorsement is required yields the zero policy for every such deployment —
// equal to itself however the keyring document changes — so a revocation added
// under that requirement would compare unchanged, which is the one thing this
// type exists to notice.
//
// It is exported so that a policy resolved from a config with no Loader built
// from it is computed through the same function Loader.SignaturePolicy uses,
// rather than growing a second, drifting idea of what a policy is.
func SignaturePolicyOf(keyring *sign.Keyring, requireSignature bool) SignaturePolicy {
	if keyring == nil {
		return SignaturePolicy{RequireSignature: requireSignature}
	}
	return SignaturePolicy{
		KeyringConfigured: true,
		KeyIDs:            keyring.IDs(),
		RevokedIDs:        keyring.RevokedIDs(),
		RequireSignature:  requireSignature,
	}
}

// Equal reports whether p and other are the same policy: the same endorsement
// requirement, a local keyring configured on both sides or on neither, over the
// same trusted and revoked key ids. Key order does not matter in principle, but
// both sides come from sign.Keyring.IDs, which sorts — so this compares element
// by element rather than paying for a set.
func (p SignaturePolicy) Equal(other SignaturePolicy) bool {
	if p.KeyringConfigured != other.KeyringConfigured || p.RequireSignature != other.RequireSignature {
		return false
	}
	return sameKeyIDs(p.KeyIDs, other.KeyIDs) && sameKeyIDs(p.RevokedIDs, other.RevokedIDs)
}

// sameKeyIDs compares two sorted id lists element by element. Both sides come
// from sign.Keyring, which sorts, so this costs nothing a set would save.
func sameKeyIDs(a, b []sign.KeyID) bool {
	if len(a) != len(b) {
		return false
	}
	for i, id := range a {
		if id != b[i] {
			return false
		}
	}
	return true
}

// String renders p for an operator reading an error message: whether a local
// keyring is configured at all first, because a policy that has none has no
// ids to print, then the key ids in it so a changed keyring is visible rather
// than merely asserted, and last whether an endorsement is required.
//
// The requirement is rendered because p carries it, and it is rendered in BOTH
// directions rather than only when it is on. Two policies that differ only
// there compare unequal, so a rendering that left it out would print the same
// sentence twice at an operator who has just been told the policy changed. The
// rendering states what RequireSignature says and nothing more: a configured
// keyring is not by itself a requirement, and neither is the absence of one a
// statement that endorsements are off.
func (p SignaturePolicy) String() string {
	requirement := "no endorsement is required"
	if p.RequireSignature {
		requirement = "an endorsement is required"
	}
	if !p.KeyringConfigured {
		return "no local keyring, and " + requirement
	}
	ids := make([]string, 0, len(p.KeyIDs))
	for _, id := range p.KeyIDs {
		ids = append(ids, string(id))
	}
	if len(p.RevokedIDs) == 0 {
		return fmt.Sprintf("a local keyring with trusted keys [%s], and %s", strings.Join(ids, " "), requirement)
	}
	// Revocations are rendered separately rather than by subtracting them from
	// the trusted list: an operator comparing two policies in an error message
	// needs to see WHICH keys were revoked, and a list that silently shrank
	// would tell them only that something differs.
	revoked := make([]string, 0, len(p.RevokedIDs))
	for _, id := range p.RevokedIDs {
		revoked = append(revoked, string(id))
	}
	return fmt.Sprintf("a local keyring with trusted keys [%s], revoked keys [%s], and %s",
		strings.Join(ids, " "), strings.Join(revoked, " "), requirement)
}

// SignaturePolicy returns this Loader's signature policy, in the comparable
// form SignaturePolicy documents.
//
// No lock is taken: localKeyring and requireSignature are each written once in
// New and never again, so there is nothing here for a concurrent Apply to race
// with. The returned KeyIDs slice is freshly built by sign.Keyring.IDs on every
// call, so a caller cannot reach into the Loader's keyring through it.
func (l *Loader) SignaturePolicy() SignaturePolicy {
	return SignaturePolicyOf(l.localKeyring, l.requireSignature)
}

// RemotePolicy is a Loader's remote-source policy in comparable form: where
// fetched packages are filed, and whether a plaintext source may be fetched at
// all.
//
// It exists for the same caller SignaturePolicy does, and for the same reason.
// Config.Remote is frozen when serve assembles the Loader and cannot be swapped
// under a running one, so a command that re-reads the config while a Loader
// runs (`agent plugins reload`) has to be able to tell whether the policy it
// just read is the one the Loader was BUILT with. Without that comparison an
// operator who turns "allow_insecure_sources" back off and reloads is told the
// reload succeeded while the process keeps fetching over plaintext, and an
// operator who moves "plugins.cache" keeps writing to the old directory with
// nothing on screen saying so — a control that looks applied and is not.
//
// Only the two settings that decide WHAT MAY BE FETCHED AND WHERE IT LANDS are
// in it. The fetch and unpack limits are deliberately left out: like the
// Loader's resource ceilings, a stale bound is a performance surprise rather
// than an unverified package or a download written somewhere the operator did
// not choose.
type RemotePolicy struct {
	// CacheRoot is the absolute directory fetched packages are filed under
	// (fetch.Cache.Root). It is empty exactly when no cache is configured —
	// the deployment that cannot fetch anything at all, where every remote
	// entry fails naming what is missing.
	CacheRoot string

	// AllowInsecureSources is whether an "http://" source may be fetched.
	// False is the safe side and the value a deployment that says nothing
	// gets; it relaxes the URL scheme and nothing else (see RemoteConfig).
	AllowInsecureSources bool
}

// RemotePolicyOf describes the policy a Loader built with remote enforces. The
// zero RemoteConfig — the deployment with no remote entries — is the policy
// with no cache root and plaintext refused.
//
// It is exported so that a caller which has resolved a remote configuration
// from a config but has not built a Loader from it computes the policy through
// the same function Loader.RemotePolicy uses, rather than growing a second,
// drifting idea of what a policy is.
func RemotePolicyOf(remote RemoteConfig) RemotePolicy {
	policy := RemotePolicy{AllowInsecureSources: remote.AllowInsecureSources}
	if remote.Cache != nil {
		policy.CacheRoot = remote.Cache.Root()
	}
	return policy
}

// Equal reports whether p and other are the same policy: the same cache root
// and the same answer on plaintext sources. Both roots are absolute (a Cache
// resolves its root at construction, and a caller reading one from a config
// resolves it through fetch.CacheRoot), so this compares them as written.
func (p RemotePolicy) Equal(other RemotePolicy) bool {
	return p == other
}

// String renders p for an operator reading an error message: the plaintext
// answer first, because it is the security-relevant half, then the cache root
// so a moved cache is visible rather than merely asserted.
//
// The path is rendered unquoted on purpose. %q would escape every separator of
// a Windows path, and an operator comparing this against what they wrote in
// "plugins.cache" should be reading the path they wrote.
func (p RemotePolicy) String() string {
	plaintext := "plaintext sources refused"
	if p.AllowInsecureSources {
		plaintext = "plaintext sources allowed"
	}
	if p.CacheRoot == "" {
		return plaintext + ", no plugin cache configured"
	}
	return fmt.Sprintf("%s, plugin cache %s", plaintext, p.CacheRoot)
}

// RemotePolicy returns the remote-source policy this Loader is enforcing right
// now.
//
// No lock is taken: remote is written once in New and never again, so there is
// nothing here for a concurrent Apply to race with. The returned value holds
// no reference to the Loader's Cache — only the path it is rooted at — so a
// caller cannot reach into the cache through it.
func (l *Loader) RemotePolicy() RemotePolicy {
	return RemotePolicyOf(l.remote)
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
//	entry's package is signed by a key this
//	deployment has revoked                   -> refuse it AND unload whatever
//	                                            is running under its name
//	                                            (revoked)
//
// "Content" is the plugin package's sha256 and version plus the entry's own
// grant, accepted tools and config — see fingerprintOf for why each is in
// there.
//
// It runs in three passes, and the split is the load-bearing part:
//
//  1. Every desired entry's package is read, checked, assembled and fingerprinted.
//     Nothing running is touched, so an entry whose package is broken fails on
//     its own and leaves its running instance alone. A REVOCATION is the one
//     verdict that does not stop there — see pass 2.
//  2. EVERY unload this convergence performs runs — the entries that left the
//     target state, the old instances of entries whose content changed, and the
//     entries this deployment has revoked the endorsement of.
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
// The convergence itself is no longer necessarily brief either: an entry whose
// source is a URL is FETCHED inside it (see prepare), so a round that has to
// download N uncached remote packages holds the gate and the Loader's lock for
// as long as those downloads take — bounded by Config.Remote.FetchLimits.
// Timeout times the number of uncached remote entries, on top of the wait for
// a task boundary. For that whole stretch new tasks are refused with
// taskgate.ErrApplyPending and Status blocks. A deployment whose remote entries
// are already cached pays none of it: a cache hit reads the disk and returns.
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

	// The trust set is read ONCE for this whole convergence, before pass 1
	// touches anything, and it is what every judgement below is taken against:
	// which packages may mount (pass 1), and which instances may be in the
	// mounted state under the endorsement they were mounted under — pass 2 for
	// the ones that are, restore for one that a failed replacement would put
	// back. See trustReading for why one reading rather than one per judgement.
	//
	// It is read when this convergence has something to judge: an entry to
	// prepare, or an instance already mounted. A convergence with neither has
	// no question to ask, and an answer nothing is judged against is an answer
	// nobody reads.
	//
	// A provider that cannot answer does not become "no trust set". Each entry
	// reports it as its own activation failure, exactly as it did when this
	// read lived inside prepare, and the ONE thing that cannot be reported per
	// entry is said here: this convergence cannot tell whether a mounted
	// instance's endorsement has been withdrawn, so it leaves every mounted
	// instance as it is. Left unsaid, that silence would read like "nothing
	// was revoked", which is the one answer a deployment that does not know
	// what it trusts must not give.
	//
	// That sentence is about the mounted instances, so it is said only when
	// there are some. With nothing mounted this convergence failed to judge
	// nothing, and the entries still each report the read failure as their own
	// activation failure — the read failure is never downgraded to "so nothing
	// was revoked", it simply has no mounted instance to be silent about.
	trust := trustReading{}
	if len(wanted) > 0 || len(l.instances) > 0 {
		trust.input, trust.err = l.trustSet()
	}
	if trust.err != nil && len(l.instances) > 0 {
		l.logger.Error("this convergence cannot judge whether a mounted plugin's endorsement was revoked",
			"detail", "the trust set could not be read, so every mounted instance is left as it is",
			"mounted", len(l.instances), "error", trust.err)
	}

	// Pass 1: work out what each desired entry needs, touching nothing.
	plans := make([]*convergePlan, 0, len(wanted))
	planFor := make(map[string]*convergePlan, len(wanted))
	for _, entry := range wanted {
		plan, err := l.prepare(ctx, entry, root, trust)
		if err != nil {
			errs = append(errs, fmt.Errorf("converge plugin %q: %w", entry.Name, err))
			// The record read here is still the previous convergence's — pass
			// 2 has not run yet — so an unconfirmedDisposal note found here is
			// always one an earlier convergence filed, never one this call is
			// about to file itself. See noteUnconfirmedDisposalIfCarried for
			// why that earns it a re-announcement on this convergence too.
			l.noteUnconfirmedDisposalIfCarried(entry.Name, l.failures[entry.Name].unconfirmedDisposal)
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
	// target state, the previous instances of the entries that changed, AND the
	// entries whose endorsement this deployment has revoked — before pass 3
	// activates anything. A replaced instance's own unload belongs here and not
	// next to its activation, so that a tool name it releases is available to
	// whichever entry claims it next, in this same Apply.
	mounted := make([]string, 0, len(l.instances))
	for name := range l.instances {
		mounted = append(mounted, name)
	}
	sort.Strings(mounted)
	for _, name := range mounted {
		plan := planFor[name]
		inst := l.instances[name]
		// Asked of the instance's OWN key and of this convergence's one trust
		// reading, and of nothing else — see the reasonRevoked case.
		withdrawn, endorsementRevoked := trust.revokedEndorsementOf(inst)
		var reason string
		switch {
		case desired[name] && endorsementRevoked:
			// The key that endorsed THIS RUNNING INSTANCE — the one
			// manifest.LoadPackage named for the package this mount was built
			// from, see instance.keyID — is one this convergence's trust set
			// has revoked. While that is so, and for as long as this
			// convergence can read a trust set at all, nothing that key
			// endorses serves under this entry's name: a package it signs is
			// refused by admit, an instance it endorsed is unloaded here
			// however long ago it was mounted, and an instance so unloaded is
			// not handed back by a rollback — restore puts nothing back
			// without asking this same question of this same reading. Leaving
			// that instance up would put the arrival of an emergency
			// revocation at "whenever this process next restarts", which is
			// the case it can least afford to miss.
			//
			// The question is the instance's own endorsement against the trust
			// set and NOTHING else — not whether the package now on disk
			// mounted, was refused, or was refused for some unrelated reason.
			// Both independences are load-bearing. Judging the package on disk
			// would hand anyone who can write into the deployment directory a
			// way to take a healthy, trusted plugin down by dropping in a
			// package signed by a revoked key. And reaching this only when the
			// package on disk was refused AS REVOKED would let a package that
			// fails an EARLIER check — a plugin.wasm whose sha256 no longer
			// matches the plugin.json describing it, say — keep a revoked
			// instance serving until the next restart, which is the shape an
			// attacker gets for free by corrupting one byte.
			//
			// It is decided FIRST, ahead of "this entry is being replaced",
			// because a replacement in flight changes what happens next but not
			// what happened: the instance goes down because this deployment
			// withdrew the endorsement it was mounted under, and an unload
			// recorded as merely "replaced" would leave that unsaid in the one
			// place — the event stream — where an operator watches revocations
			// arrive. The replacement is unaffected: its plan is still pass 3's
			// to activate, exactly as after any other unload, and only if that
			// activation fails does the difference bite, because then there is
			// nothing to put back.
			//
			// It is checked before the plain desired case below precisely
			// because the entry is still desired. Every OTHER refusal leaves
			// the running instance alone, for the reason prepare's doc comment
			// gives: a package that will not load is a REPLACEMENT that
			// failed, and unmounting a working plugin for a replacement that
			// never existed helps nobody.
			//
			// Only for a DESIRED entry, because a name that left the target
			// state is unloaded by the branches below anyway — and unloaded
			// WITHOUT a failure record, which is what "an entry that is not
			// supposed to be running is not a diagnosis" means in Status.
			// Taking it down as revoked instead would file a record the top of
			// this convergence has just deleted.
			reason = reasonRevoked
		case plan != nil && plan.prev != nil:
			reason = reasonReplaced
		case desired[name]:
			continue
		case declared[name]:
			reason = reasonDisabled
		default:
			reason = reasonManifestRemoved
		}
		delete(l.instances, name)
		revoked, err := l.unload(ctx, inst, reason)
		if plan != nil {
			// The replacement carries its predecessor's disposal failure: it is
			// how many entries a rollback would have to put back, and — if the
			// disposal failed — a leak that has to stay visible on whatever ends
			// up mounted under this name.
			//
			// No record is filed here even when this unload was a revocation,
			// because pass 3 has not answered yet and a record filed now would
			// describe an entry that is about to be mounted again. The account
			// is whatever pass 3 leaves behind: a successful activation is the
			// entry running under an endorsement this deployment still holds,
			// and a failed one goes through fail, which is where the refusal to
			// hand the revoked instance back is joined to the activation's own
			// failure.
			plan.revoked = revoked
			plan.unloadErr = err
			continue
		}
		if reason == reasonRevoked {
			// Reported as well as filed. Pass 1 may or may not have refused
			// the package on disk — an entry whose package is unchanged and
			// merely re-signed passes pass 1 outright — and an unload nobody
			// asked for that Apply returns nil about is an emergency
			// revocation that reached a running plugin silently. When pass 1
			// DID refuse, the two sentences are two facts: that package was
			// refused, and this instance was taken down. They coincide only
			// when the refused package is the one this instance was built
			// from.
			cause := revokedInstanceRefusal(name, inst, withdrawn)
			errs = append(errs, fmt.Errorf("converge plugin %q: %w", name, cause))
			l.recordRevokedUnload(name, inst.version, cause, err)
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

// recordRevokedUnload files the Status record of an entry pass 2 has just
// unloaded because this deployment revoked the key that endorses its package.
// It is called with l.mu held.
//
// It exists because that entry would otherwise vanish from Status entirely: the
// refusal was recorded by fail while the instance was still mounted, so it went
// onto the instance record — and pass 2 has just discarded that record. An
// entry that is enabled, refused and invisible is the one outcome a revocation
// must not produce.
//
// The version is the one that WAS running rather than the one the package on
// disk now declares, because that is what the unload took down and what an
// operator has to match against their own inventory of where that code ran.
//
// unloadErr is the disposal's own answer and is kept apart from the refusal:
// "this deployment revoked the endorsement of this plugin" and "and its
// resources were never confirmed released" are two facts, the second strictly
// worse than the first, and folding them into one sentence is how the worse one
// stops being read. The note it leaves behind outlives this convergence — see
// failure.unconfirmedDisposal.
func (l *Loader) recordRevokedUnload(name, version string, cause, unloadErr error) {
	record := failure{version: version, err: cause.Error()}
	if unloadErr != nil {
		record.unconfirmedDisposal = unconfirmedDisposalNote(unloadErr)
		l.logger.Error("a revoked plugin's unload reported a failure",
			"plugin", name, "version", version, "reason", reasonRevoked, "error", unloadErr)
	}
	l.failures[name] = record
}

// unconfirmedDisposalNote words the note left behind when the unload a
// revocation forced reported a failure. It is one wording rather than one per
// place the note is filed, so that "the revoked code may still be running"
// reads the same whether the revoked instance was simply taken down or was
// taken down for a replacement that then failed to activate. See
// failure.unconfirmedDisposal for why the note outlives its convergence.
func unconfirmedDisposalNote(unloadErr error) string {
	return fmt.Sprintf("the unload this revocation forced reported a failure, so the revoked plugin's "+
		"resources were never confirmed released and its code may still be running in this process: %v",
		unloadErr)
}

// noteUnconfirmedDisposalIfCarried says that a revoked plugin's unload was
// never confirmed, for note found already sitting on the entry rather than one
// this call is about to file itself — a no-op when note is empty, i.e. there
// is nothing carried to say.
//
// Said again on EVERY convergence that leaves this entry unmounted, not only
// on the one that first recorded it: an unload whose disposal was reported
// once and never again reads, from the second convergence on, exactly like an
// unload that succeeded. It hangs on "this entry failed again", not on which
// failure: the package that succeeds a revoked one may fail an entirely
// unrelated check — at prepare, as pass 1 does, or at activation with its
// rollback then refused as revoked, as fail does — and the resources the
// revoked mount never released are no less unaccounted for either way. Both
// callers are the ones responsible for telling carried apart from
// just-filed: this function only ever repeats what it is handed.
func (l *Loader) noteUnconfirmedDisposalIfCarried(name, note string) {
	if note == "" {
		return
	}
	l.logger.Error("a revoked plugin's unload was never confirmed",
		"plugin", name, "detail", note)
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
			Name:             inst.name,
			Version:          inst.version,
			State:            state,
			Tools:            append([]string(nil), inst.tools...),
			SuspendedBy:      suspendedBy,
			ProvidesServices: append([]string(nil), inst.providesServices...),
			RequiresServices: append([]string(nil), inst.requiresServices...),
			LastError:        inst.lastError,
		})
	}
	for name, f := range l.failures {
		out = append(out, InstanceStatus{
			Name:      name,
			Version:   f.version,
			State:     StateFailed,
			LastError: f.explanation(),
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

	// keyID is the key manifest.LoadPackage says endorses this plan's package,
	// empty for a package no registered key endorses. It becomes the mounted
	// instance's own keyID, which is what a later revocation is judged
	// against; see instance.keyID.
	keyID sign.KeyID

	// revoked and unloadErr are pass 2's answer about prev: how many ledger
	// entries went with it, and whether its disposal reported a failure. Both
	// are zero-valued for a plan with no prev.
	unloadErr error
	revoked   int

	// trust is the reading this entry's package was judged against, carried on
	// the plan so that a rollback to prev is decided against the SAME answer
	// that decided prev's fate in pass 2 rather than against a second reading
	// taken later. A convergence that asked twice could take an instance down
	// under one trust set and put it back under another.
	trust trustReading
}

// trustReading is the ONE answer this convergence got from its trust-set
// provider: the input every package it reads is judged against, or the
// provider's own failure to answer.
//
// One reading per convergence rather than one per judgement, because the
// provider is free to answer differently on every call (see the TrustSet
// type). A convergence that asked twice could refuse a package under one trust
// set and decide the fate of the instance mounted under that name against
// another, and would be converging toward a state nobody ever declared.
//
// Both fields come from one read of the provider: input is what it answered,
// err is why it could not answer. A failure is carried rather than folded into
// "no trust set", because a deployment that does not KNOW what it trusts is not
// a deployment that trusts nobody — see TrustSet. A reading that failed is one
// every entry reports as its own activation failure, and one no mounted
// instance is judged against at all.
type trustReading struct {
	input manifest.TrustInput
	err   error
}

// revokedEndorsementOf reports the revocation this convergence's trust set
// holds against the key inst was mounted under, if it holds one.
//
// It is the single form of one question — may an instance endorsed by this key
// be in the mounted state, judged against this convergence's one reading? —
// and that question arises at more than one moment in a convergence: of an
// instance that is mounted now, and of one a failed replacement would put back.
// One form rather than one per moment is deliberate. The same rule written out
// twice is a rule that can hold in one copy and quietly not in the other, and
// "a revoked instance goes on serving" has already been reached three times
// through a moment the rule had not been written into.
//
// It answers about a KEY, never about a package: the key that endorsed the
// instance mounted under some name (see instance.keyID) is the one this
// convergence has to ask about, and the answer comes from the reading it
// already took rather than from a second one.
//
// Three inputs answer "no", each for its own reason. A reading that failed
// judges nothing — the convergence has already reported that it cannot judge,
// and answering "not revoked" here is precisely what that report exists to
// prevent. A deployment whose reading carries no keyring holds no revocations
// to find (manifest.TrustInput.Keyring is a pointer, and nil is how "no trust
// set" is spelled). And an empty instance.keyID is a mount with no registered
// endorsement — manifest.ProvenanceUnsigned carries no key id — so there is no
// key here for any revocation to name: revoking a key withdraws trust from
// that key, and this mount was endorsed by none.
func (t trustReading) revokedEndorsementOf(inst *instance) (sign.Revocation, bool) {
	if t.err != nil || t.input.Keyring == nil || inst.keyID == "" {
		return sign.Revocation{}, false
	}
	return t.input.Keyring.Revoked(inst.keyID)
}

// revokedInstanceRefusal explains an unload forced by this deployment revoking
// the key a mounted instance was endorsed by (see reasonRevoked).
//
// It names the INSTANCE — the version that was running and the key it was
// mounted under — rather than any package now sitting on disk, because the
// instance is what was taken down and its version is what an operator matches
// against their own inventory of where that code ran. The package on disk may
// be a different package signed by a different key, and a refusal of THAT one
// is reported separately, in its own sentence.
//
// It wraps manifest.ErrRevokedPublisher because it is the same refusal admit
// makes about a package, reached at the other end of the mount's life: a caller
// asking errors.Is whether a revocation was what happened gets the same answer
// whether the package was stopped at the door or the instance was taken down
// afterwards.
//
// The revocation's own recorded context is rendered by
// manifest.DescribeRevocation, which reads exactly the two optional fields
// carried here and renders "" when a record holds neither.
func revokedInstanceRefusal(name string, inst *instance, withdrawn sign.Revocation) error {
	described := manifest.DescribeRevocation(manifest.Provenance{
		RevokedAt: withdrawn.At,
		Reason:    withdrawn.Reason,
	})
	return fmt.Errorf("plugin %q version %s was mounted under key %q, which this deployment revoked%s, "+
		"so it was unloaded: %w", name, inst.version, inst.keyID, described, manifest.ErrRevokedPublisher)
}

// prepare works out what one entry needs, without touching anything that is
// running. It returns a nil plan and a nil error when the entry did not change.
// It is called with l.mu held.
//
// trust is one convergence's reading of its trust-set provider, handed in
// rather than read here so that every entry and every mounted instance is
// judged against one answer (see trustReading). A reading that FAILED fails
// this entry, after its source is resolved and before its package is read: a
// deployment that does not know what it trusts must not mount anything, and
// reading the failure as "no trust set" would turn a broken cache into a
// deployment where nobody endorses anything.
//
// The package is read, checked and assembled BEFORE anything running is torn
// down, which is what makes a broken new package safe: a plugin.wasm whose
// digest no longer matches its plugin.json leaves the current instance running
// and reports the failure, instead of unmounting a working plugin for a
// replacement that never existed.
//
// EVERY refusal this function reports keeps that sentence, a package signed by
// a revoked key included: what a refusal says is that this package is unfit to
// mount, and a package that never mounted cannot be the reason a running
// instance goes down. Whether the instance mounted under this name survives is
// a separate question about the key IT was mounted under, asked of this same
// trust reading and answered without reference to anything on disk (see
// reasonRevoked). The one thing a revoked refusal does here is say WHICH of the
// two packages it is about, for the row that has to carry both — see
// sayWhichPackageWasRevoked.
func (l *Loader) prepare(ctx context.Context, entry manifest.Entry, root string, trust trustReading) (*convergePlan, error) {
	// The two kinds of source differ HERE and nowhere else: a local entry
	// resolves against the deployment root, a remote one is fetched into the
	// cache. Everything from LoadPackage down is one path — that is the whole
	// point of doing the fetch here rather than deeper in.
	var dir string
	var err error
	if entry.IsRemote() {
		dir, err = l.remoteDir(ctx, entry)
		if err != nil {
			return nil, l.fail(ctx, entry.Name, "", stepFetch, err, nil)
		}
	} else {
		dir, err = packageDir(entry.Name, root, entry.Source)
		if err != nil {
			return nil, l.fail(ctx, entry.Name, "", stepSource, err, nil)
		}
	}

	// The provider's own failure is reported HERE, after the entry's source is
	// resolved and before its package is read, so that each entry reports the
	// first thing that actually stopped it: an entry whose source does not
	// resolve reports that, and one whose source does resolve reports that this
	// deployment does not know what it trusts.
	if trust.err != nil {
		return nil, l.fail(ctx, entry.Name, "", stepLoadPackage,
			fmt.Errorf("read the trust set to judge plugin %q against: %w", entry.Name, trust.err), nil)
	}

	// LoadPackage answers who stands behind the package and leaves what that
	// permits to admit, below. What it still returns as an ERROR is everything
	// that is not a verdict: a plugin.sig that is malformed or does not verify
	// against a key this trust set names, a plugin.wasm whose sha256 disagrees
	// with the plugin.json describing it, a manifest that will not parse, and
	// the plain read failures underneath all three. Each is an ordinary
	// activation failure — it goes through l.fail like every other one, so the
	// entry lands in StateFailed with a LastError naming the check, and the
	// other entries keep converging.
	pm, wasm, prov, err := manifest.LoadPackage(dir, trust.input)
	if err != nil {
		// An untrusted package does not belong in the cache: the bytes just
		// failed signature verification, and leaving them there means every
		// later convergence reads the same rejected package back from a
		// directory this deployment trusts enough to read.
		//
		// Only for a TRUST failure, and only for a REMOTE entry: a package
		// that merely will not load is re-downloaded identically next time, so
		// evicting it buys nothing, and a local entry's directory is the
		// operator's own tree rather than this cache's.
		if errors.Is(err, manifest.ErrUntrustedPackage) && entry.IsRemote() {
			fetch.EvictUntrusted(l.remote.Cache, entry.Digest, l.logger)
		}
		return nil, l.fail(ctx, entry.Name, "", stepLoadPackage, err, nil)
	}
	if err := l.admit(entry, dir, trust.input, prov); err != nil {
		// The version is the one the refused package declares, which is a
		// version this convergence read rather than a blank a reader has to
		// guess at. It is also what keeps the version on the record of an entry
		// a revocation unloaded: recordRevokedUnload files the version that was
		// running, and every convergence after it rewrites that record from
		// here.
		return nil, l.fail(ctx, entry.Name, pm.Version, stepLoadPackage,
			l.sayWhichPackageWasRevoked(entry.Name, pm.Version, trust, err), nil)
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

	// The shape of the configuration is the PLUGIN's declaration (plugin.json's
	// config_schema) and the values are the DEPLOYMENT's, so this is the first
	// point where both are in hand.
	//
	// It runs before the host dependencies are built and long before the guest
	// is entered, on purpose: an entry whose config is wrong should not get as
	// far as owning resources, and the guest is the worst possible place to
	// discover it — a plugin's error reporting is one flat string, while here
	// the failure names the field.
	//
	// A plugin that declares no schema passes through untouched (see
	// manifest.ValidateEntryConfig), which is every plugin written so far.
	if err := manifest.ValidateEntryConfig(pm, entry.Config); err != nil {
		return nil, l.fail(ctx, entry.Name, pm.Version, stepConfigSchema, err, nil)
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
	// Health accounting is wired here rather than inside host.Deps' factory
	// because it belongs to the MOUNT, not to the deployment's dependency
	// wiring: the counter it feeds lives on this Loader's instance record and
	// must die when that record does. Config.Deps is caller-supplied and knows
	// nothing about either.
	name := entry.Name
	deps.OnFault = func(faultCtx context.Context, category, toolName, reason string) {
		if l.recordFault(name, category) {
			l.unloadUnhealthy(faultCtx, name, category, toolName, reason)
		}
	}
	deps.OnSuccess = func(_ context.Context, _ string) { l.recordSuccess(name) }
	// Named-service resolution belongs to the mount for the same reason:
	// providers come and go while the process runs, so the answer is looked up
	// per call against THIS loader's current instances, never captured when a
	// consumer was activated.
	deps.Services = l

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
	return &convergePlan{
		entry: entry, dir: dir, pm: pm, spec: spec, digest: digest, prev: prev, keyID: prov.KeyID,
		trust: trust,
	}, nil
}

// sayWhichPackageWasRevoked adds, to a refusal of a package this deployment has
// revoked, the one thing the status row cannot say on its own: WHICH of two
// packages the refusal is about. It returns cause unchanged for every other
// refusal and whenever there is no second package to confuse it with. It is
// called with l.mu held.
//
// A row carries one state and one explanation. When an instance is mounted
// under this name and its OWN endorsement was not withdrawn, converge's pass 2
// leaves it running — it asks this same trustReading about that instance's own
// key — so the row reads "loaded" while its explanation says "revoked", and a
// reader has no way to tell whether the revoked thing is the plugin they can
// watch serving requests or the package somebody just dropped into the
// deployment directory to replace it. The refusal therefore says so itself: the
// refused package is the one this entry DECLARES, at the version that package
// declares, and the mounted version is named next to it.
//
// It leaves cause alone when the mounted instance's own endorsement was
// withdrawn too, because that instance does not survive this convergence:
// recordRevokedUnload replaces the whole record with pass 2's own account of
// the unload, and nothing written here would be read.
//
// Only for a revoked refusal. Every other refusal a mounted instance survives
// already reads as what it is — the replacement did not load — while this one
// reads as a statement about the plugin that is running.
func (l *Loader) sayWhichPackageWasRevoked(name, version string, trust trustReading, cause error) error {
	inst := l.instances[name]
	if inst == nil || !errors.Is(cause, manifest.ErrRevokedPublisher) {
		return cause
	}
	if _, endorsementRevoked := trust.revokedEndorsementOf(inst); endorsementRevoked {
		return cause
	}
	return fmt.Errorf("the package this entry declares, version %s, was refused: %w; it is a REPLACEMENT, "+
		"and version %s stays mounted under this name because its own endorsement was not withdrawn",
		version, cause, inst.version)
}

// admit applies this deployment's policy to a package's provenance: it is where
// every graded-install decision a convergence makes is taken.
//
// The three outcomes are not symmetric. A package a registered publisher
// endorses loads. An unendorsed one loads only if an operator accepted these
// exact bytes, or if the deployment has said it does not require an
// endorsement. A revoked one never loads, whatever else is configured — see
// Config.RequireSignature for why that switch does not reach it.
//
// The one case worth spelling out is an acceptance that does not match while
// RequireSignature is false. It is REFUSED, like every other mismatched
// acceptance: a record covering different bytes says this package changed since
// somebody looked at it, and that is a fact about the package rather than a
// statement about endorsements — which is all the switch ever conceded. An
// operator who never wanted endorsements still wants to be told their plugin
// was swapped.
//
// dir is the package directory, and trust is the set the verdict was reached
// against — the second only so that a refusal can say which keys an endorsement
// would have had to come from.
func (l *Loader) admit(entry manifest.Entry, dir string, trust manifest.TrustInput, prov manifest.Provenance) error {
	switch prov.State {
	case manifest.ProvenanceRevoked:
		return fmt.Errorf("plugin %q is signed by key %q, which this deployment revoked%s: %w",
			entry.Name, prov.KeyID, manifest.DescribeRevocation(prov), manifest.ErrRevokedPublisher)

	case manifest.ProvenanceRegistered:
		l.logger.Info("plugin package is endorsed by a registered publisher",
			"plugin", entry.Name, "key_id", prov.KeyID, "publisher", prov.Publisher)
		return nil

	case manifest.ProvenanceUnsigned:
		digest, err := manifest.ManifestDigest(dir)
		if err != nil {
			return fmt.Errorf("judge unendorsed plugin %q: %w", entry.Name, err)
		}
		switch {
		// EqualFold, not ==: manifest.ManifestDigest writes lowercase hex, but
		// Entry.AcceptedUnsigned is validated against a pattern that accepts
		// either case, so a hand-written acceptance may legitimately arrive
		// upper-cased. Comparing byte for byte would report that one as a
		// CHANGED package — a tampering alarm about a package nothing happened
		// to. It is the same comparison LoadPackage makes against the wasm
		// digest plugin.json declares, for the same reason.
		case strings.EqualFold(entry.AcceptedUnsigned, digest):
			l.logger.Info("plugin package is unendorsed and is admitted on an install-time acceptance",
				"plugin", entry.Name, "accepted_digest", digest)
			return nil
		case entry.AcceptedUnsigned == "" && !l.requireSignature:
			l.logger.Warn("plugin package is admitted with no endorsement because this deployment does not require one",
				"plugin", entry.Name, "digest", digest)
			return nil
		case entry.AcceptedUnsigned == "":
			return fmt.Errorf("plugin %q is endorsed by no registered publisher — %s — and nobody has "+
				"accepted these bytes either; its plugin.json hashes to %s: %w",
				entry.Name, describeMissingEndorsement(dir, trust, prov), digest,
				manifest.ErrUnsignedNotAccepted)
		default:
			return fmt.Errorf("plugin %q changed since it was accepted: the acceptance on record covers %s, "+
				"and the package in %s hashes to %s. These are not the bytes anyone approved: %w",
				entry.Name, entry.AcceptedUnsigned, dir, digest, manifest.ErrUnsignedNotAccepted)
		}

	default:
		// A state this function does not know how to judge must not be admitted
		// by falling through: an unhandled enum value is a programming error,
		// and treating it as "fine" is the exact shape of silent trust
		// escalation. It is a refusal rather than a panic because a convergence
		// covers many entries, and one entry nobody can judge is not a reason
		// to take the process down.
		return fmt.Errorf("plugin %q has provenance state %s, which this deployment does not know how to "+
			"judge: %w", entry.Name, prov.State, manifest.ErrUnsignedNotAccepted)
	}
}

// describeMissingEndorsement renders WHY a package counts as unendorsed, as a
// clause meant to sit inside a sentence that has already said no registered
// publisher stands behind it.
//
// The two renderings are two different facts, not two phrasings of one. "There
// is nothing here this deployment recognises as an endorsement" and "this
// package claims dev-abc signed it, and dev-abc means nothing here" send an
// operator to different places: the second says somebody did sign it, and the
// question is why that key is not registered — a registration that lapsed, an
// id typed wrong, and an attacker all produce it, and they ask for three
// different responses. The offered id is the only thing that separates them,
// so it is printed, and printed as a claim rather than as a name this machine
// knows: manifest.Provenance.UnrecognizedKeyID is a string the package chose,
// and a message that let it read like a credential would be worse than one
// that omitted it.
//
// Both renderings name the keys this deployment does trust, because "who it
// says signed it" and "who this deployment would have believed" are
// complementary: one alone never shows that the two sets do not meet.
func describeMissingEndorsement(dir string, trust manifest.TrustInput, prov manifest.Provenance) string {
	sigPath := filepath.Join(dir, "plugin.sig")
	if prov.UnrecognizedKeyID == "" {
		// Deliberately not "plugin.sig is absent": this branch is also reached
		// by a deployment with no trust set at all, where a plugin.sig may well
		// exist and simply never got placed against anything. Reporting it as
		// missing would send an operator looking for a file that is right
		// there.
		return fmt.Sprintf("no endorsement this deployment recognises is attached at %s (trusted keys: %s)",
			sigPath, trustedKeyIDs(trust))
	}
	return fmt.Sprintf("%s claims a signature by key %q, which this deployment's trust set does not hold "+
		"(trusted keys: %s)", sigPath, prov.UnrecognizedKeyID, trustedKeyIDs(trust))
}

// trustedKeyIDs renders the key ids an endorsement would have had to come from,
// for a refusal that has to tell an operator what this machine would accept.
//
// A nil keyring is spelled out rather than rendered as an empty list: "trusted
// keys: []" reads like a configuration that failed to load, and this one is a
// deployment that recognises nobody, which is a different thing to go and look
// at.
func trustedKeyIDs(trust manifest.TrustInput) string {
	if trust.Keyring == nil {
		return "none, this deployment recognises no signing key at all"
	}
	return fmt.Sprintf("%v", trust.Keyring.IDs())
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

	// A service name is held by exactly one plugin, first come first served
	// (see specs/2026-08-29-plugin-service-seam-design.md, decisions B and C).
	// The second claimant fails here rather than stepping aside or taking
	// over: either of those would leave "who provides this capability?"
	// unanswerable while both plugins report themselves healthy.
	//
	// "First" is the order the deployment declared, because that is what Apply
	// walks — not the order mounting happens to finish in, which would let the
	// same deployment hand a service to different plugins on two starts.
	if holder, service := l.serviceHeldBy(entry.Name, plan.pm.ProvidesServices); holder != "" {
		clash := fmt.Errorf("plugin %q claims service %q, which plugin %q already provides; a service has "+
			"exactly one provider, and the first to claim it keeps it", entry.Name, service, holder)
		return l.fail(ctx, entry.Name, plan.pm.Version, stepToolNames, clash, plan)
	}

	owner := ownerFor(plan.pm.Name, plan.pm.Version)
	plugin, err := host.Activate(ctx, l.ledger, owner, plan.spec)
	if err != nil {
		activation := fmt.Errorf("activate plugin %q from %s: %w", entry.Name, plan.dir, err)
		return l.fail(ctx, entry.Name, plan.pm.Version, stepActivate, activation, plan)
	}

	inst := &instance{
		name:                entry.Name,
		version:             plan.pm.Version,
		owner:               owner,
		spec:                plan.spec,
		fingerprint:         plan.digest,
		sha256:              plan.pm.SHA256,
		keyID:               plan.keyID,
		tools:               toolNames(plan.spec.Tools),
		plugin:              plugin,
		requires:            append([]string(nil), plan.pm.Requires...),
		providesServices:    append([]string(nil), plan.pm.ProvidesServices...),
		requiresServices:    append([]string(nil), plan.pm.RequiresServices...),
		serviceCapabilities: plan.pm.ServiceCapabilities,
	}
	if plan.unloadErr != nil {
		// The replacement came up, but its predecessor did not go down cleanly:
		// only the pool drain and the runtime close can fail a DisposeOwner, so
		// what is left behind is a leaked wasm runtime. It travels out in Apply's
		// error, and it stays on the status row too — an operator looking at
		// "state=loaded" has to be able to see it.
		inst.lastError = plan.unloadErr.Error()
	}
	// The record this activation clears may be carrying an unload whose
	// disposal was never confirmed — this entry was taken down by a revocation,
	// its resources were not acknowledged as released, and now the entry is
	// mounting again because the deployment trusts it once more. Trusting it
	// again says nothing about the resources the previous mount never released,
	// so the note moves onto the instance instead of being deleted with the
	// record: an operator looking at "state=loaded" has to be able to see it,
	// exactly as they do for a predecessor's failed disposal above. It is also
	// said aloud on this convergence, because a remount is the one event that
	// would otherwise let it go quiet.
	if note := l.failures[entry.Name].unconfirmedDisposal; note != "" {
		if inst.lastError == "" {
			inst.lastError = note
		} else {
			inst.lastError = inst.lastError + "; " + note
		}
		l.logger.Error("a revoked plugin's unload was never confirmed",
			"plugin", entry.Name, "detail", note)
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
	endorsementWithdrawn := false
	if prev != nil {
		if err := l.restore(ctx, prev, plan.trust); err != nil {
			errs = append(errs, err)
			restored = restoredNo
			// Whether the rollback was refused BECAUSE this deployment
			// withdrew prev's endorsement, asked of the refusal itself rather
			// than re-derived: restore wraps manifest.ErrRevokedPublisher for
			// that one refusal and for no other.
			endorsementWithdrawn = errors.Is(err, manifest.ErrRevokedPublisher)
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
		// Carried across, not dropped. For an entry whose revocation-forced
		// unload was never confirmed this is that record being rewritten by a
		// later convergence, and the refusal recorded here says nothing about
		// that disposal either way. For every other entry there is nothing to
		// carry. See failure.unconfirmedDisposal.
		unconfirmed := l.failures[name].unconfirmedDisposal
		if endorsementWithdrawn && plan.unloadErr != nil {
			// This convergence is the one that filed it: pass 2 took prev down
			// as revoked while a replacement was in flight, so it left the
			// record to whatever came next rather than writing one — and its
			// disposal reported a failure. The replacement then failed too and
			// prev is not coming back, which makes this the last chance to say
			// that a revoked plugin's resources were never confirmed released.
			unconfirmed = unconfirmedDisposalNote(plan.unloadErr)
			l.logger.Error("a revoked plugin's unload reported a failure",
				"plugin", name, "version", prev.version, "reason", reasonRevoked, "error", plan.unloadErr)
		} else {
			// Whatever unconfirmed holds here was filed by an earlier
			// convergence: the branch above is fail's only place that files a
			// fresh one, and it did not run. See noteUnconfirmedDisposalIfCarried
			// for why an earlier note earns a re-announcement here too, exactly
			// as pass 1 gives one to the entry it never got as far as
			// activating.
			l.noteUnconfirmedDisposalIfCarried(name, unconfirmed)
		}
		// The version this row names is the one the explanation above is
		// about. For every other failure that is the version this call was
		// asked to activate (the caller's version). But a rollback refused as
		// revoked names a version that never mounted a single instance —
		// prev's replacement — while the explanation joined into err is about
		// prev itself (see revokedInstanceRefusal), so the row would otherwise
		// carry two different versions for one plugin. recordRevokedUnload
		// already names prev's own version for the plain revocation that never
		// had a replacement in flight; this keeps the rollback-refused case
		// consistent with it.
		recordedVersion := version
		if endorsementWithdrawn {
			recordedVersion = prev.version
		}
		l.failures[name] = failure{
			version:             recordedVersion,
			err:                 joined.Error(),
			unconfirmedDisposal: unconfirmed,
		}
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
// trust is the reading prev's fate was decided against in pass 2, carried on
// the plan (see convergePlan.trust). An instance whose own endorsement this
// deployment has withdrawn is NOT put back, however the replacement failed:
// coming back is coming back to the mounted state, which is the state that
// judgement is about, and it is the same judgement pass 2 took — see
// trustReading.revokedEndorsementOf. Without it a replacement endorsed by a key
// that is still trusted but that fails to activate would hand the revoked
// instance its name, its tools and its ledger entries back, every convergence,
// until somebody restarted the process: the emergency revocation would have
// arrived and been rolled back by the same Apply.
//
// The refusal is an error rather than a silent skip because the entry ends this
// convergence with nothing mounted and two things to explain — the replacement
// did not activate, AND what was running was not put back — and fail joins both
// into the record the row carries.
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
func (l *Loader) restore(ctx context.Context, prev *instance, trust trustReading) error {
	if withdrawn, endorsementRevoked := trust.revokedEndorsementOf(prev); endorsementRevoked {
		refusal := revokedInstanceRefusal(prev.name, prev, withdrawn)
		l.logger.Error("a revoked plugin instance was not put back after its replacement failed",
			"plugin", prev.name, "version", prev.version, "key", string(prev.keyID),
			"detail", "the replacement did not activate, and what it displaced was mounted under a key "+
				"this deployment has revoked, so nothing is mounted under this name",
			"error", refusal)
		return fmt.Errorf("restore previous instance of plugin %q (owner %s): it stays unloaded because "+
			"this deployment has withdrawn the endorsement it was mounted under: %w",
			prev.name, prev.owner, refusal)
	}
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
// reportDrainLeak publishes plugin/unload_leaked when disposal gave up waiting
// for calls still inside the guest.
//
// It reports the COUNT, not just the fact: "something is still running" is not
// actionable, while "3 calls are still inside legion-foo after waiting 5s"
// tells an operator whether they are looking at one stuck call or at a plugin
// that never returns. The count is read after the wait, so it is what was left
// behind rather than what was there when the wait began.
//
// Any other disposal failure is left alone — it already travels out of unload
// as an error and into the entry's lastError. This event exists for the one
// case where the failure is not the unload itself but what SURVIVED it.
func (l *Loader) reportDrainLeak(ctx context.Context, inst *instance, disposeErr error) {
	if disposeErr == nil || !errors.Is(disposeErr, host.ErrDrainIncomplete) {
		return
	}
	inflight := inst.plugin.InflightCalls()
	waited := inst.plugin.DrainBound()
	l.logger.Error("plugin unload left calls inside the guest",
		"plugin", inst.name, "version", inst.version, "inflight", inflight, "waited", waited)
	l.publish(ctx, RuntimeEventUnloadLeaked,
		fmt.Sprintf("plugin=%s version=%s inflight=%d waited=%s", inst.name, inst.version, inflight, waited))
}

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
	l.reportDrainLeak(ctx, inst, disposeErr)

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
// unloadUnhealthy unloads a plugin whose consecutive-fault count crossed the
// deployment's threshold. Task 4 of the runtime-health plan fills in the
// unload itself; this step only makes the decision visible so a threshold
// crossing is never silent.
func (l *Loader) unloadUnhealthy(ctx context.Context, name, category, toolName, reason string) {
	l.mu.Lock()
	inst, ok := l.instances[name]
	if !ok {
		// Another convergence unmounted it between the threshold crossing and
		// this call. Nothing to unload, and nothing to explain: whatever
		// replaced it (or the removal itself) is the current truth.
		l.mu.Unlock()
		return
	}
	delete(l.instances, name)
	l.mu.Unlock()

	detail := fmt.Sprintf("health: %d consecutive faults (last: category=%s tool=%s: %s)",
		l.maxConsecutiveFaults, category, toolName, reason)

	revoked, err := l.unload(ctx, inst, reasonHealth)
	if err != nil {
		// The unload still happened as far as the deployment is concerned —
		// the entry is out of l.instances either way — so the failure is
		// recorded in the explanation rather than swallowed or retried.
		l.logger.Error("unhealthy plugin unloaded with failures",
			"plugin", name, "category", category, "error", err)
		detail += fmt.Sprintf("; unload reported: %v", err)
	}

	l.mu.Lock()
	l.failures[name] = failure{version: inst.version, err: detail}
	l.mu.Unlock()

	l.logger.Warn("plugin unloaded for repeated faults",
		"plugin", name, "threshold", l.maxConsecutiveFaults, "category", category,
		"tool", toolName, "reason", reason, "revoked", revoked)
}

// recordFault counts one health-relevant failure of a mounted plugin and
// reports whether the deployment must now unload it.
//
// A denial is not counted and never will be: it means the plugin asked for
// something outside its grant, which is the grant working, not the plugin
// breaking. Everything host.ClassifyCallFault does count arrives here already
// filtered — a caller's cancellation never reaches this function.
//
// A fault for a plugin that is no longer mounted is dropped: the call that
// failed was answered by an instance this Loader has already let go, and
// counting it against whatever replaced it would punish the new mount for the
// old one's failures.
func (l *Loader) recordFault(name, category string) bool {
	if category == host.CategoryDenied {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	inst := l.instances[name]
	if inst == nil {
		return false
	}
	inst.faults++
	return inst.faults >= l.maxConsecutiveFaults
}

// recordSuccess clears a plugin's fault count: health is about CONSECUTIVE
// failures, so one answered call means it is answering again. Without this a
// plugin that fails once a day would eventually be unloaded for it.
func (l *Loader) recordSuccess(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if inst := l.instances[name]; inst != nil {
		inst.faults = 0
	}
}

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

// remoteDir returns the directory holding entry's package, fetching it first
// if the cache does not already have it. It is packageDir's counterpart for an
// entry whose source is a URL, and everything after it — LoadPackage, the
// identity check, AssembleSpec, activation — is the same code a local entry
// travels.
//
// # A cache hit does not go online
//
// The digest names one exact sequence of bytes, so a hit is the same package a
// fetch would produce. There is nothing to revalidate and nothing to expire:
// the request is not made at all. That is what lets a restart, or a machine
// with no network, mount the plugins the first start mounted.
//
// # Two gates, in this order
//
// A missing cache and a refused scheme are both decided BEFORE any request is
// built, so neither can be discovered halfway through a download:
//
//   - No cache configured is a failure, never a temporary directory. Where
//     downloaded code lands is a deployment decision (see RemoteConfig).
//   - An "http://" source is refused unless the deployment turned plaintext on
//     explicitly, and the refusal names the entry and its URL.
//
// What it does NOT decide is whether the bytes are acceptable — fetch.Fetch
// verifies them against entry.Digest before they reach the filesystem — or
// whether the package may load, which is manifest.LoadPackage's answer, given
// on the directory this returns exactly as it is for a local one.
//
// NOTE-10 (drift risk, recorded on this side too): internal/cli's
// resolvePluginPackageDir (plugins_command.go) is a hand-kept copy of this
// function's remote branch — `agent plugins grant` has to resolve the same
// directory without a running Loader to ask, and cannot call this method
// across the package boundary. Its own doc comment names the judgement call:
// document the duplication rather than export a shared resolver, since the
// fix batch that found the drift was scoped to internal/cli alone. Anyone
// changing this function's security-relevant checks (the cache-configured
// refusal, the insecure-source refusal, the digest lookup) MUST check
// resolvePluginPackageDir for the same edit.
func (l *Loader) remoteDir(ctx context.Context, entry manifest.Entry) (string, error) {
	if l.remote.Cache == nil {
		return "", fmt.Errorf("plugin %q: source %q is remote, but this deployment configured no plugin cache "+
			"directory; a remote package has to be written somewhere, and that location is a deployment decision "+
			"rather than one this process may make on its own", entry.Name, entry.Source)
	}
	if entry.IsInsecureSource() && !l.remote.AllowInsecureSources {
		return "", fmt.Errorf("plugin %q: source %q is plaintext http, which this deployment does not permit; "+
			"plaintext is a debugging aid and has to be turned on explicitly with "+
			`"allow_insecure_sources": true in the plugins config (and this Loader's policy is fixed when serve `+
			"starts, so changing it takes a restart rather than a reload)", entry.Name, entry.Source)
	}

	hit, err := l.remote.Cache.Has(entry.Digest)
	if err != nil {
		return "", fmt.Errorf("plugin %q: look up %s in the plugin cache: %w", entry.Name, entry.Digest, err)
	}
	if hit {
		// Same digest, same bytes. Nothing is requested, and nothing needs to
		// be: content addressing leaves no staleness to revalidate.
		return l.remote.Cache.Dir(entry.Digest), nil
	}

	u, err := entry.RemoteURL()
	if err != nil {
		return "", fmt.Errorf("plugin %q: %w", entry.Name, err)
	}
	archive, err := fetch.Fetch(ctx, l.remote.Client, u, entry.Digest, l.remote.FetchLimits)
	if err != nil {
		return "", fmt.Errorf("plugin %q: %w", entry.Name, err)
	}
	dir, err := l.remote.Cache.Put(entry.Digest, archive, l.remote.UnpackLimits)
	if err != nil {
		return "", fmt.Errorf("plugin %q: %w", entry.Name, err)
	}
	return dir, nil
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
//
// NOTE-10 (drift risk, recorded on this side too): internal/cli's
// localPluginPackageDir (plugins_command.go) is a hand-kept copy of this
// function — `agent plugins grant` needs the identical root-escape refusal
// to check a plugin's declared capabilities without a running Loader, and
// cannot call this function across the package boundary. Its own doc
// comment names the judgement call: document the duplication rather than
// export a shared resolver, since the fix batch that found the drift was
// scoped to internal/cli alone. Anyone tightening or loosening this
// function's absolute-path or root-escape refusal MUST check
// localPluginPackageDir for the same edit.
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

// ResolveService implements host.ServiceResolver: it answers which tool is the
// named capability of whoever provides the service RIGHT NOW.
//
// Two refusals, both naming what was asked for, because a consumer that
// receives "tool not found" for a service reference goes looking for the wrong
// thing:
//
//   - nobody provides the service (or its provider is suspended, so it is not
//     contributing anything at the moment);
//   - the provider holds the service but exposes no such capability.
//
// A suspended provider is deliberately not resolvable: its tools are withdrawn
// from the registry, so resolving to one would hand the caller a name that is
// about to fail as unknown — and the consumer of a suspended provider is
// normally suspended itself (see suspend.go), which is the state that says so.
func (l *Loader) ResolveService(service, capability string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for name, inst := range l.instances {
		if !containsName(inst.providesServices, service) {
			continue
		}
		if inst.plugin.Suspended() {
			return "", fmt.Errorf("service %q is provided by plugin %q, which is suspended", service, name)
		}
		toolName, ok := inst.serviceCapabilities[service][capability]
		if !ok {
			return "", fmt.Errorf("plugin %q provides service %q but exposes no capability %q",
				name, service, capability)
		}
		return toolName, nil
	}
	return "", fmt.Errorf("no mounted plugin provides service %q", service)
}
