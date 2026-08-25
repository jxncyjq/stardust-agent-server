package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/plugin/consent"
	"github.com/stardust/legion-agent/internal/plugin/fetch"
	"github.com/stardust/legion-agent/internal/plugin/host"
	"github.com/stardust/legion-agent/internal/plugin/loader"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/plugin/sign"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/tool"
)

// The states one row of `agent plugins status` can report. Together they are
// the answer to "the plugin is in my manifest, so why is nothing happening?",
// and telling them apart is the whole point of the command: an entry nobody
// has ever authorized, an entry the operator switched off on purpose, an
// entry that tried and failed, and an entry that nobody has converged yet
// are four different problems with four different fixes.
//
// Two of these come from the loader itself (loader.StateLoaded, "the plugin
// is mounted right now", and loader.StateFailed, "it is in the target state and
// nothing is mounted for it") and are printed as it reports them. The other
// three (pluginStateUnauthorized, pluginStateDisabled and pluginStatePending)
// exist only here, because only the manifest on disk can tell them apart from
// an entry the loader has simply never heard of.
const (
	// pluginStateUnauthorized means "enabled": false AND the manifest entry
	// has never carried a grant block at all (manifest.Entry.GrantStated is
	// false): plugins.json itself carries no record of an authorization
	// decision for this plugin, as distinct from pluginStateDisabled below,
	// where the file DOES record one (a grant block is present, even if
	// empty), even if it was later revoked (`agent plugins deny` leaves
	// GrantStated true for exactly this reason).
	//
	// This wording is deliberately narrower than "nobody has ever decided
	// anything": GrantStated can only observe THIS FILE, not the operator's
	// intent, so a hand-written "enabled": false entry with no grant block —
	// indistinguishable here from one written before this field existed —
	// also lands here, even though typing "enabled": false was itself a
	// decision. Reporting it unauthorized rather than disabled is the
	// deliberate, conservative call: the cost of being wrong is an operator
	// being told to run `agent plugins grant`, which is itself a no-op until
	// they name capabilities and cannot silently re-enable anything, versus
	// the alternative of classifying an unrecorded decision as "nothing to
	// see here" and having it never surface for review. See
	// manifest.Entry.GrantStated's own doc comment for why an empty
	// capabilities list alone cannot answer this question either — a
	// pure-compute plugin legitimately has zero capabilities and can still be
	// explicitly authorized (`agent plugins grant` with none named). The next
	// step for this state is `agent plugins grant`, not nothing.
	pluginStateUnauthorized = "unauthorized"

	// pluginStateDisabled means the manifest entry says "enabled": false AND
	// carries a grant block (GrantStated is true) — a decision WAS made, even
	// if it was later revoked (`agent plugins deny` leaves GrantStated true
	// for exactly this reason). This is the operator's own doing, not a
	// fault, and the next step is nothing: the state is intended.
	pluginStateDisabled = "disabled"

	// pluginStatePending means the entry is enabled in the manifest on disk and
	// the loader has not converged it — either the manifest changed since the
	// last apply, or an apply was refused before it reached this entry.
	pluginStatePending = "pending"
)

// pluginHostDeps is everything the plugin host needs from the serve assembly
// that the config alone cannot provide. Every field is required whenever a
// plugin deployment is configured; assemblePlugins reports a missing one by
// name rather than mounting plugins whose denials reach no event stream or
// whose tool calls are audited nowhere.
type pluginHostDeps struct {
	// Audit is the audit log the plugin tool registry records tool calls to,
	// including the calls a plugin makes back through call_tool.
	Audit port.AuditLog

	// Events receives the loader's plugin/loaded, plugin/unloaded and
	// plugin/activation_failed events, and the host-side denial events of every
	// granted capability.
	Events port.EventBus

	// Logger records convergence decisions and is the only channel a host
	// function has for a failure it cannot return to the guest.
	Logger *slog.Logger

	// Gate is the task-boundary gate every convergence lands through. It MUST
	// be the same gate the runtimes running this serve's tasks were built with;
	// a gate of its own would wait for a boundary nobody is standing at.
	Gate *taskgate.TaskGate
}

// assemblePlugins builds this process's plugin loader from cfg.Plugins,
// attaches it to application, and converges the declared deployment once.
//
// It returns nil and does nothing when cfg.Plugins.Manifest is empty: that is
// the contract-declared "plugins are not enabled" deployment, and the great
// majority of installations run in it. A configured manifest is the opposite
// statement — a path that cannot be read or parsed fails serve assembly,
// because an operator who named a manifest meant to run it.
//
// A plugin that cannot be ACTIVATED is different from a manifest that cannot be
// read: it is logged at Error level and startup continues, so one bad plugin
// does not ground the whole agent. It never becomes silent, though — the loader
// is attached BEFORE the convergence runs, so a failed entry keeps its failure
// visible in `agent plugins status` for as long as it stays in the manifest.
//
// The loader it builds is reachable through application.Plugins(), which is
// also how serve drains the plugins again (see drainPlugins); nothing is
// attached at all when plugins are not enabled.
//
// ctx bounds the startup convergence's wait for a task boundary, so a caller
// that cancels on shutdown releases it. In practice the wait is not spent:
// this runs before the service starts, with no task in flight, so there is no
// path here that parks a goroutine on a gate nobody will release.
func assemblePlugins(ctx context.Context, application *app.App, cfg config.Config, deps pluginHostDeps) error {
	if application == nil {
		return errors.New("assemble plugins: application is nil; there is no lifecycle ledger to file activations under")
	}
	if strings.TrimSpace(cfg.Plugins.Manifest) == "" {
		if strings.TrimSpace(cfg.Plugins.Keyring) != "" {
			// The one documented exception to "configured means you meant it":
			// with no manifest nothing is ever loaded, so this keyring is
			// never read, never parsed, and a broken one would go unnoticed
			// until the day plugins are switched on. Nothing is verified here
			// because nothing is mounted here, but the operator gets to hear
			// that the file they configured is doing nothing.
			if deps.Logger == nil {
				return errors.New("assemble plugins: Logger is nil; a configured keyring that is not in use would go unreported")
			}
			deps.Logger.Warn("plugin trust keyring is configured but plugins are not enabled",
				"component", "cli",
				"keyring", cfg.Plugins.Keyring,
				"consequence", `no "plugins.manifest" is configured, so nothing is loaded and this keyring is never read or validated`,
				"remedy", `configure "plugins.manifest" to enable plugins, or remove "plugins.keyring"`)
		}
		return nil
	}
	switch {
	case deps.Audit == nil:
		return errors.New("assemble plugins: Audit is nil; a plugin's tool calls would be recorded nowhere")
	case deps.Events == nil:
		return errors.New("assemble plugins: Events is nil; a convergence that published nothing would be invisible")
	case deps.Logger == nil:
		return errors.New("assemble plugins: Logger is nil; a convergence that logged nothing would be unexplainable")
	case deps.Gate == nil:
		return errors.New("assemble plugins: Gate is nil; a convergence with no task-boundary gate would land in the middle of a running task")
	}

	deployment, err := readPluginDeployment(cfg.Plugins.Manifest)
	if err != nil {
		return err
	}
	// The deployment's remote entries are checked against the deployment's
	// remote POLICY before a loader exists: both failures here are statements
	// about the config as a whole ("you named a remote source and nowhere to
	// put it", "you named a plaintext source and did not turn plaintext on"),
	// and neither is repairable by letting the rest of the manifest converge.
	if err := checkRemoteSources(deployment, cfg.Plugins, deps.Logger); err != nil {
		return err
	}
	pluginLoader, err := newPluginLoader(application, cfg, deps)
	if err != nil {
		return err
	}
	// Attached before the convergence, not after: an Apply that fails outright
	// must still leave a loader whose Status answers "why is nothing mounted?".
	if err := application.SetPlugins(pluginLoader); err != nil {
		return err
	}
	if err := pluginLoader.Apply(ctx, deployment, cfg.Plugins.Root); err != nil {
		// Loud, and startup continues. The alternative — refusing to serve at
		// all — lets one broken plugin take the agent down; the alternative on
		// the other side — dropping this — makes a plugin that never came up
		// indistinguishable from one nobody configured.
		deps.Logger.Error("converge plugin deployment at startup",
			"component", "cli",
			"manifest", cfg.Plugins.Manifest,
			"root", cfg.Plugins.Root,
			"consequence", "the failed entries stay visible in `agent plugins status`",
			"error", err)
	}
	return nil
}

// drainPlugins unmounts every plugin the loader attached to application has
// mounted, by converging it toward an EMPTY target state, and then detaches the
// loader (app.App.ClearPlugins) so the same process can assemble serve again.
//
// It is serve's shutdown step, and also the cleanup every FAILING serve
// assembly runs once the plugins are mounted (see BuildServeService). It is not
// housekeeping for its own sake: a plugin's tool name lives in the
// PROCESS-GLOBAL gateable catalog (toolauth), so an embedded host that stops
// and restarts serve in one process — or simply retries an assembly that failed
// on "address already in use" — would otherwise hit a duplicate-name panic the
// second time round, and every mounted wasm runtime would stay resident in
// between.
//
// It must be called after in-flight tasks have drained, so the convergence
// finds the task gate idle and does not have to wait for a boundary that a
// stopping service will never reach. Its wait is bounded only by
// loader.Config.ApplyWait (config plugins.apply_wait_ms, 60s by default), so a
// gate still held by something outside the caller's reach delays shutdown by up
// to that long before the Warn below — bounded, but not short.
//
// A failure is logged at Warn and no further: the process is usually going
// away, and there is nothing left to retry against — but a wasm runtime that
// would not close is a leak the operator has to be able to see. A failed drain
// deliberately does NOT detach: the plugins really are still mounted, and the
// next SetPlugins must still refuse.
//
// application is nil, or holds no loader, when plugins are not enabled — both
// are a no-op.
func drainPlugins(application *app.App, root string, logger *slog.Logger) {
	if application == nil {
		return
	}
	pluginLoader := application.Plugins()
	if pluginLoader == nil {
		return
	}
	if err := pluginLoader.Apply(context.Background(), manifest.Deployment{}, root); err != nil {
		logger.Warn("unmount plugins on shutdown",
			"component", "cli",
			"root", root,
			"consequence", "wasm runtimes and gateable tool names stay resident, and this process cannot assemble serve again",
			"error", err)
		return
	}
	application.ClearPlugins()
}

// newPluginLoader builds the Loader for cfg.Plugins, wiring every dependency a
// granted capability needs from what serve already has.
//
// Two deliberate omissions:
//
//   - Deps.KV is not set. This deployment has no key-value backend, and handing
//     plugins a throwaway in-memory map would be a store whose contents vanish
//     without anyone being told. A plugin granted "kv" therefore fails to
//     activate with the missing field named (host.BuildHostModule), which is
//     the honest answer until a real store exists.
//   - Deps.Config falls back to an explicit "{}" for an entry with no config
//     block. That is host.Deps' own documented contract ("a plugin with no
//     configuration is given an explicit empty object rather than nothing at
//     all"), not a papered-over absence: config_get returns the value verbatim,
//     so an empty body would reach the guest as undecodable JSON.
//
// The tool registry the plugins share is a real workspace registry rooted at
// the same directory the default task runner sandboxes to: it carries the
// process's execution policy, role permission enforcement, path guardrails and
// audit log, so a plugin's call_tool goes through exactly the checks the
// model's own tool calls do.
//
// Two things that registry does NOT do yet, and that the acceptance pass has to
// close rather than assume:
//
//   - The MODEL cannot reach a contributed tool. Both task-runner paths build a
//     fresh registry per task (defaultTaskRunner.RunTask, and the per-agent
//     resolver), and a *tool.Registry has no way to inherit from another after
//     construction, so a plugin's tools live only here. What a plugin
//     contributes is already fully visible — in the ledger, in `plugins status`
//     and in the process-global gateable catalog — it is simply not yet in any
//     task's registry.
//   - Even reached, a contributed tool would be refused: the role permission
//     enforcer is a whitelist of "role:tool" keys and no plugin tool is in it,
//     so Registry.Execute returns ErrPermissionDenied for one.
func newPluginLoader(application *app.App, cfg config.Config, deps pluginHostDeps) (*loader.Loader, error) {
	toolRoot := cfg.ContextFiles.Root
	absRoot, err := filepath.Abs(toolRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve plugin workspace root %q: %w", toolRoot, err)
	}
	registry := tool.NewFileReadWriteWorkspaceRegistry(toolRoot, deps.Audit, tool.WithProjectRoot(toolRoot))
	guard := port.NewWorkspacePathGuard(absRoot)
	// One client for every plugin, bounded by the deployment's own per-call
	// timeout: an outbound request may not outlive the call that made it.
	// config.Load guarantees the timeout is positive whenever a manifest is
	// configured, so this is never an unbounded client.
	httpClient := &http.Client{Timeout: time.Duration(cfg.Plugins.Limits.TimeoutMs) * time.Millisecond}
	identity := serveDefaultAgent()
	logger := deps.Logger
	if logger == nil {
		// Reached only through assemblePlugins, which already refuses a nil
		// Logger -- but the signature policy is reported through this logger
		// below, and a policy decision made with nowhere to report it is the
		// silent degradation this whole path exists to prevent.
		return nil, errors.New("new plugin loader: Logger is nil; the signature policy would be decided with no record of it")
	}

	// The trust set, or the deliberate nil that says this deployment does not
	// require signatures. Anything else -- a keyring that would not read, a
	// requirement with nothing to check against -- fails here rather than
	// mounting plugins nobody verified.
	keyring, unenforcedKeyring, err := resolvePluginKeyring(cfg.Plugins)
	if err != nil {
		return nil, err
	}
	if unenforcedKeyring != "" {
		// Deliberately NOT a second "this deployment verifies nothing": that
		// sentence is loader.New's to say, once, and two warnings meaning the
		// same thing are how an operator learns to skip both. This one carries
		// only what the assembly knows and the loader cannot -- that a trust
		// set was configured, and which file it is.
		logger.Warn("plugin trust keyring is configured but not enforced",
			"component", "cli",
			"keyring", unenforcedKeyring,
			"consequence", `the keys in it are loaded and then dropped, because the config says "require_signature": false`,
			"remedy", `remove "require_signature": false to enforce the trust set that is already configured`)
	}

	remote, err := resolvePluginRemote(cfg.Plugins)
	if err != nil {
		return nil, err
	}

	return loader.New(loader.Config{
		Ledger: application.PluginLedger(),
		Deps: func(name string, pluginConfig json.RawMessage) host.Deps {
			if len(pluginConfig) == 0 {
				pluginConfig = json.RawMessage(`{}`)
			}
			return host.Deps{
				PluginName: name,
				Logger:     logger.With("component", "plugin", "plugin", name),
				Config:     pluginConfig,
				HTTP:       httpClient,
				FS:         guard,
				Events:     deps.Events,
				Tools:      registry,
				Agent:      identity,
			}
		},
		Events: deps.Events,
		Logger: logger,
		DeployLimits: manifest.Limits{
			TimeoutMs:      cfg.Plugins.Limits.TimeoutMs,
			MaxMemoryPages: cfg.Plugins.Limits.MaxMemoryPages,
			MaxInstances:   cfg.Plugins.Limits.MaxInstances,
		},
		Gate:      deps.Gate,
		ApplyWait: time.Duration(cfg.Plugins.ApplyWaitMs) * time.Millisecond,
		Keyring:   keyring,
		Remote:    remote,
	})
}

// The bounds one fetched plugin package is unpacked under. They bound the
// DECOMPRESSED archive, which is why they are not derived from
// plugins.fetch.max_bytes: that one caps the compressed bytes read off the
// network, and a compression ratio can be enormous.
//
// They are constants rather than settings because a plugin package is three
// files of known kinds — plugin.json, plugin.wasm, plugin.sig — and an archive
// that does not fit these is not a large package, it is not a package. A
// deployment that genuinely needs a bigger wasm module changes them here, in
// one place, rather than every deployment carrying three more knobs it will
// never touch.
const (
	// pluginUnpackMaxEntries bounds the tar entries read. Three files, plus
	// room for a single wrapping directory and its entry.
	pluginUnpackMaxEntries = 16

	// pluginUnpackMaxEntryBytes bounds any single file in the archive; the
	// wasm module is the only one that is ever large.
	pluginUnpackMaxEntryBytes = 64 << 20

	// pluginUnpackMaxTotalBytes bounds the whole decompressed archive,
	// including the tar headers and padding between entries.
	pluginUnpackMaxTotalBytes = 128 << 20
)

// resolvePluginRemote turns the deployment's remote-source settings into the
// loader's RemoteConfig, and is the only place a plugin cache is built.
//
// An unconfigured "plugins.cache" returns the zero RemoteConfig — the
// deployment that fetches nothing. That is NOT a quiet degradation: an entry
// that actually needs a cache has already been refused by checkRemoteSources
// at assembly, and the loader refuses one again if it ever meets it (a manifest
// reloaded under a running serve, for instance). A configured cache directory
// that cannot be created fails assembly with the path named, the same
// "configured means you meant it" rule the manifest and the keyring follow.
//
// The HTTP client is its own, deliberately not the one plugins granted "http"
// call out with: that client is bounded by the per-call plugin timeout, which
// has nothing to say about how long an artifact download may take. Its timeout
// is fetch's own bound as well, so a download is bounded even if a future edit
// drops the per-call context deadline.
func resolvePluginRemote(cfg config.PluginsConfig) (loader.RemoteConfig, error) {
	path := strings.TrimSpace(cfg.Cache)
	if path == "" {
		return loader.RemoteConfig{}, nil
	}
	cache, err := fetch.NewCache(path)
	if err != nil {
		return loader.RemoteConfig{}, fmt.Errorf("open plugin cache %q: %w", path, err)
	}
	timeout := time.Duration(cfg.Fetch.TimeoutMs) * time.Millisecond
	return loader.RemoteConfig{
		Cache:                cache,
		Client:               &http.Client{Timeout: timeout},
		FetchLimits:          fetch.Limits{MaxBytes: cfg.Fetch.MaxBytes, Timeout: timeout},
		UnpackLimits:         fetch.UnpackLimits{MaxEntries: pluginUnpackMaxEntries, MaxTotalBytes: pluginUnpackMaxTotalBytes, MaxEntryBytes: pluginUnpackMaxEntryBytes},
		AllowInsecureSources: cfg.InsecureSourcesAllowed(),
	}, nil
}

// insecurePluginSourceWarning is the message every plaintext plugin source
// gets at assembly. It is a constant so that the warning a test counts is the
// warning production writes: a deployment where one entry's warning went
// missing is exactly how "allow_insecure_sources" gets abused.
const insecurePluginSourceWarning = "plugin source is plaintext http"

// checkRemoteSources holds the deployment's remote entries against the
// deployment's remote policy, before any loader is built.
//
// Two refusals, both fatal to serve assembly, and both about the config as a
// whole rather than one entry's bad luck:
//
//   - A remote entry with no "plugins.cache" configured. There is deliberately
//     no fallback to a temporary directory: where downloaded code is written
//     is a deployment decision, and choosing one here would make it invisible.
//   - A plaintext "http://" entry while "allow_insecure_sources" is off — the
//     default, and the safe side of a security switch. The error names the
//     entry, its URL, and the setting that turns plaintext on.
//
// And one warning, written for EVERY plaintext entry when the switch IS on: a
// plaintext artifact can be watched and blocked in transit, and an operator
// can be fed an old-but-legitimately-signed version. The digest still
// guarantees the bytes are the bytes the manifest names, and the signature is
// still verified, but neither of those makes plaintext a thing to run silently.
//
// Only ENABLED entries are checked, which is the same set the loader prepares:
// a disabled entry is never fetched, and refusing to start over one nothing
// would download would be a rule about text rather than about behaviour. An
// operator who enables it later meets exactly these rules then.
func checkRemoteSources(deployment manifest.Deployment, cfg config.PluginsConfig, logger *slog.Logger) error {
	if logger == nil {
		return errors.New("check plugin remote sources: Logger is nil; a plaintext source would be used with no record of it")
	}
	cacheConfigured := strings.TrimSpace(cfg.Cache) != ""
	insecureAllowed := cfg.InsecureSourcesAllowed()
	for _, entry := range deployment.Plugins {
		if !entry.Enabled || !entry.IsRemote() {
			continue
		}
		if !cacheConfigured {
			return fmt.Errorf("plugin %q has the remote source %q, but no \"plugins.cache\" directory is configured; "+
				"a fetched package has to be written somewhere, and this process will not pick that location itself "+
				"(configure \"plugins.cache\", or install the plugin under \"plugins.root\" instead)",
				entry.Name, entry.Source)
		}
		if !entry.IsInsecureSource() {
			continue
		}
		if !insecureAllowed {
			return fmt.Errorf("plugin %q has the plaintext source %q; plugin artifacts are fetched over https, "+
				"and plaintext is a debugging aid that has to be turned on explicitly with "+
				`"allow_insecure_sources": true in the plugins config`, entry.Name, entry.Source)
		}
		logger.Warn(insecurePluginSourceWarning,
			"component", "cli",
			"plugin", entry.Name,
			"source", entry.Source,
			"consequence", "the download can be observed and blocked in transit, and an old but legitimately "+
				"signed version can be served in place of the current one",
			"retained", "the digest still guarantees the bytes are the ones the manifest names, and the package's "+
				"signature is still verified",
			"remedy", `serve the artifact over https, or remove "allow_insecure_sources": true`)
	}
	return nil
}

// resolvePluginKeyring turns the deployment's signature POLICY into the trust
// set the Loader will verify with, and is the only place a nil keyring may be
// produced. Every caller that builds a loader.Config must obtain its Keyring
// from here.
//
// The rules, and why each one fails loudly rather than degrading:
//
//   - A configured keyring path is always read and parsed, whatever the policy
//     says. An unreadable or unparseable one fails assembly with the path
//     named — the same "configured means you meant it" rule the manifest path
//     follows. This is deliberately checked even when signatures are turned
//     off: an operator who wrote down both a keyring and
//     "require_signature": false wrote two contradictory things, and quietly
//     dropping the broken file is exactly the silent degradation this whole
//     control exists to prevent.
//   - Signatures required (the default, see config.PluginsConfig.
//     SignatureRequired) with NO keyring configured fails assembly. "Verify
//     every package" with nothing to verify against is not a deployment worth
//     starting, and the error names both ways out of it.
//   - Signatures explicitly NOT required returns nil, and only then. nil is
//     what tells manifest.LoadPackage to skip verification, so it must be
//     reachable from one deliberate statement and from nothing else: not from
//     a file that would not open, not from a forgotten field, not from an
//     error someone decided to tolerate. A keyring that loaded fine is still
//     dropped here — the policy, not the presence of a file, is what decides.
//
// The second return value is the path of a keyring that loaded successfully and
// was then dropped by policy, or "" when there was none. It is returned rather
// than logged here because this function has two callers with different jobs:
// serve assembly reports it (a deployment holding a trust set it does not
// enforce is worth a line in the log), while `plugins reload` only compares
// policies and has nothing new to say about one that has not changed.
func resolvePluginKeyring(cfg config.PluginsConfig) (*sign.Keyring, string, error) {
	path := strings.TrimSpace(cfg.Keyring)
	var keyring *sign.Keyring
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("read plugin trust keyring %q: %w", path, err)
		}
		keyring, err = sign.ParseKeyring(data)
		if err != nil {
			return nil, "", fmt.Errorf("parse plugin trust keyring %q: %w", path, err)
		}
		if keyring == nil {
			// sign.ParseKeyring's contract is a non-nil keyring on a nil
			// error. A nil here would travel on as "signatures not required",
			// so it is an invariant violation rather than a case to handle.
			panic("cli: sign.ParseKeyring returned a nil keyring and a nil error")
		}
	}
	if !cfg.SignatureRequired() {
		unenforced := ""
		if keyring != nil {
			unenforced = path
		}
		return nil, unenforced, nil
	}
	if keyring == nil {
		return nil, "", fmt.Errorf("plugins.keyring is not configured while plugin signatures are required: "+
			"either configure a keyring of trusted public keys, or turn the requirement off explicitly with "+
			`"require_signature": false in the plugins config (manifest %q)`, cfg.Manifest)
	}
	return keyring, "", nil
}

// pluginSignaturePolicy is the signature policy cfg asks for, in the comparable
// form a running Loader reports through Loader.SignaturePolicy.
//
// It resolves the keyring through resolvePluginKeyring, so it applies exactly
// the rules serve assembly applies: a keyring that will not read still fails
// here, and "signatures required" with no keyring still fails here. A leniently
// computed policy would be worse than none — it could report "unchanged" for a
// config serve would refuse to start on.
func pluginSignaturePolicy(cfg config.PluginsConfig) (loader.SignaturePolicy, error) {
	// The "configured but not enforced" note belongs to whoever ASSEMBLES a
	// loader; startup already logged it, and repeating it on every reload of an
	// unchanged policy would say nothing new.
	keyring, _, err := resolvePluginKeyring(cfg)
	if err != nil {
		return loader.SignaturePolicy{}, err
	}
	return loader.SignaturePolicyOf(keyring), nil
}

// pluginRemotePolicy is the remote-source policy cfg asks for, in the
// comparable form a running Loader reports through Loader.RemotePolicy.
//
// It deliberately does NOT go through resolvePluginRemote: that one CREATES the
// cache directory (fetch.NewCache), and a comparison that is only ever asked
// "did this change?" must not leave a directory behind for a config it is about
// to refuse. The path is resolved through fetch.CacheRoot instead, which is the
// same resolution a Cache performs on itself, so an unchanged setting compares
// equal however it was spelled.
func pluginRemotePolicy(cfg config.PluginsConfig) (loader.RemotePolicy, error) {
	policy := loader.RemotePolicy{AllowInsecureSources: cfg.InsecureSourcesAllowed()}
	path := strings.TrimSpace(cfg.Cache)
	if path == "" {
		// No cache configured is a policy, not a missing one: it is the
		// deployment that fetches nothing, and it must compare unequal to one
		// that has a cache.
		return policy, nil
	}
	root, err := fetch.CacheRoot(path)
	if err != nil {
		return loader.RemotePolicy{}, fmt.Errorf("resolve plugin cache %q: %w", path, err)
	}
	policy.CacheRoot = root
	return policy, nil
}

// readPluginDeployment reads and parses the deployment manifest at path. Both
// failures name the path: "configured but unreadable" is a startup failure, and
// an operator has to be able to see which file was meant.
func readPluginDeployment(path string) (manifest.Deployment, error) {
	deployment, _, err := readPluginDeploymentWithSnapshot(path)
	return deployment, err
}

// readPluginDeploymentWithSnapshot reads and parses path exactly once,
// returning both the parsed Deployment and the raw bytes it was parsed
// from. The raw bytes are the "snapshot" refusePluginDeploymentChanged
// later compares against.
//
// This is a thin wrapper over consent.ReadDeploymentWithSnapshot -- see
// that function's doc comment for why reading once matters. Keeping the
// wrapper (rather than calling consent.ReadDeploymentWithSnapshot at each
// of this file's three call sites) is what lets install, grant and deny
// keep referring to "the deployment read", not a package-qualified name, at
// every one of them.
func readPluginDeploymentWithSnapshot(path string) (manifest.Deployment, []byte, error) {
	return consent.ReadDeploymentWithSnapshot(path)
}

// refusePluginDeploymentChanged re-reads manifestPath and compares it, byte
// for byte, against snapshot — the bytes readPluginDeploymentWithSnapshot
// captured earlier, before the caller went on to do something that can take
// a while (a remote package fetch for install and, on a cache miss, for
// grant too; even deny's plain read-modify-write has a window of its own,
// just a much shorter one).
//
// This is a thin wrapper over consent.RefuseDeploymentChanged -- see that
// function's doc comment for why the guard exists and why it is shared
// across install, grant and deny (F5 / BLOCKING-1: a guard that holds on
// one writer and not the other is not a guard). cmdContext (e.g. "plugins
// install", "plugins grant", "plugins deny") labels the error and names the
// command an operator should re-run.
func refusePluginDeploymentChanged(cmdContext, manifestPath string, snapshot []byte) error {
	return consent.RefuseDeploymentChanged(cmdContext, manifestPath, snapshot)
}

// newPluginsCommand builds `agent plugins`, the operator's handle on the WASM
// plugin deployment. Its seven subcommands fall into two groups that share
// nothing but the noun:
//
//   - status and reload are a view of THIS PROCESS: both read the loader serve
//     assembled (App.Plugins). There is no cross-process view — a loader
//     belongs to the serve that built it, so running them against a process
//     that never assembled one reports exactly that instead of an empty answer
//     that reads like "no plugins".
//   - keygen, sign, install, grant and deny touch no loader and start no
//     running service — that is what puts install, grant and deny in this
//     group rather than the one above, and nothing else about them resembles
//     keygen or sign. keygen and sign touch no config either: they are the
//     tools that PRODUCE what verification consumes, and they ship alongside
//     it deliberately — a deployment that can check signatures but has no way
//     to make one has exactly one option left, turning the requirement off,
//     which is the outcome signature verification exists to prevent. install,
//     grant and deny are the odd members: each DOES read the plugins config
//     (to resolve the same cache, fetch limits, remote-source policy and
//     trust set a running serve would use — see resolvePluginRemote and
//     resolvePluginKeyring) and each DOES write the deployment manifest.
//     install appends one verified entry, disabled unless --grant names the
//     plugin's complete capability set (in which case it is written already
//     authorized, in the same step); grant authorizes an existing entry to
//     run with its declared capabilities; deny revokes that authorization
//     while keeping the entry's registration (source, digest, tools) intact.
//     All three are REGISTRATIONS or authorization decisions on
//     disk, not a start: nothing any of them does reaches a running process
//     until `agent plugins reload`, which is the only reason they are not
//     grouped with status and reload instead.
func newPluginsCommand(application *app.App, out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Inspect, install, authorize and reload the WASM plugin deployment, and sign plugin packages",
	}
	cmd.AddCommand(newPluginsStatusCommand(application, out))
	cmd.AddCommand(newPluginsReloadCommand(application, out))
	cmd.AddCommand(newPluginsInstallCommand(out))
	cmd.AddCommand(newPluginsGrantCommand(out))
	cmd.AddCommand(newPluginsDenyCommand(out))
	cmd.AddCommand(newPluginsKeygenCommand(out))
	cmd.AddCommand(newPluginsSignCommand(out))
	return cmd
}

func newPluginsStatusCommand(application *app.App, out io.Writer) *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report every plugin in the deployment manifest and what it came to",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Context(), config.Options{Path: configPath})
			if err != nil {
				return err
			}
			if strings.TrimSpace(cfg.Plugins.Manifest) == "" {
				// Not an error: "no plugins" is a supported deployment, and an
				// operator asking after it deserves a plain answer naming the
				// setting that would turn plugins on.
				_, err := fmt.Fprintln(out, `plugins: disabled (no "plugins.manifest" in the config)`)
				return err
			}
			pluginLoader, err := requirePluginLoader(application)
			if err != nil {
				return err
			}
			deployment, err := readPluginDeployment(cfg.Plugins.Manifest)
			if err != nil {
				// The manifest being unreadable is exactly when an operator
				// needs this command most, so it reports the read failure and
				// then still prints what the loader actually has mounted —
				// every entry shows up under the "no longer in the manifest"
				// row. Failing serve assembly on an unreadable manifest is
				// right; blinding the diagnostic is not. The command still
				// exits non-zero, but AFTER printing.
				if _, werr := fmt.Fprintf(out, "plugins: manifest unreadable, reporting what is mounted: %v\n", err); werr != nil {
					return fmt.Errorf("write plugin status manifest failure: %w", werr)
				}
				if werr := writePluginStatus(out, cfg.Plugins.Manifest, cfg.Plugins.Root,
					mergePluginStatus(manifest.Deployment{}, pluginLoader.Status())); werr != nil {
					return werr
				}
				return err
			}
			return writePluginStatus(out, cfg.Plugins.Manifest, cfg.Plugins.Root,
				mergePluginStatus(deployment, pluginLoader.Status()))
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "agent JSON config file")
	return cmd
}

func newPluginsReloadCommand(application *app.App, out io.Writer) *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "reload",
		Short: "Re-read the deployment manifest and converge the running plugins toward it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Context(), config.Options{Path: configPath})
			if err != nil {
				return err
			}
			if strings.TrimSpace(cfg.Plugins.Manifest) == "" {
				return errors.New(`reload plugins: no "plugins.manifest" is configured, so there is no deployment to reload`)
			}
			pluginLoader, err := requirePluginLoader(application)
			if err != nil {
				return err
			}
			// The manifest and root below come from the config just read, but
			// the trust set does NOT: it was frozen when serve assembled this
			// Loader, and there is no way to swap it under a running one. So an
			// operator who tightened the policy and reloaded would get their new
			// manifest converged under their OLD trust set, with no warning
			// replayed and every log line looking normal — the exact silent
			// degradation signature verification exists to prevent. Refuse
			// instead, and say what has to happen: a security control that is
			// partly applied is worse than one that is not, because it looks
			// applied.
			//
			// limits and apply_wait are frozen the same way and are NOT checked
			// here. They are resource settings; a stale ceiling is a performance
			// surprise, not an unverified plugin.
			wanted, err := pluginSignaturePolicy(cfg.Plugins)
			if err != nil {
				return err
			}
			if running := pluginLoader.SignaturePolicy(); !running.Equal(wanted) {
				return fmt.Errorf("reload plugin deployment %q: the config now says %s, but this process is enforcing %s; "+
					"the trust set is fixed when serve starts, so restart serve to apply a signature-policy change "+
					"(reloading now would converge the new manifest under the old policy)",
					cfg.Plugins.Manifest, wanted, running)
			}
			// The remote-source policy is frozen the same way and is refused
			// for the same reason. An operator who turns
			// "allow_insecure_sources" back OFF and reloads would otherwise be
			// told the reload succeeded while this process kept fetching over
			// plaintext -- and the assembly-time warning that says plaintext is
			// in use is written by checkRemoteSources at startup only, so
			// nothing would replay it either. A moved "plugins.cache" has the
			// same shape: the reload would keep filing downloads under the old
			// directory with nothing on screen saying so.
			//
			// The fetch bounds are NOT compared, deliberately: like limits and
			// apply_wait above, a stale ceiling is a performance surprise
			// rather than a package fetched from somewhere the operator no
			// longer permits.
			wantedRemote, err := pluginRemotePolicy(cfg.Plugins)
			if err != nil {
				return err
			}
			if running := pluginLoader.RemotePolicy(); !running.Equal(wantedRemote) {
				return fmt.Errorf("reload plugin deployment %q: the config now says %s, but this process is using %s; "+
					"the remote-source policy is fixed when serve starts, so restart serve to apply a change to "+
					`"plugins.cache" or "allow_insecure_sources" (reloading now would fetch under the old policy)`,
					cfg.Plugins.Manifest, wantedRemote, running)
			}
			// Re-read from disk rather than replaying what startup parsed: a
			// reload that applied the manifest as it was at startup would be a
			// reload in name only.
			deployment, err := readPluginDeployment(cfg.Plugins.Manifest)
			if err != nil {
				return err
			}
			applyErr := pluginLoader.Apply(cmd.Context(), deployment, cfg.Plugins.Root)
			// The status goes out even when the apply failed: a partial
			// convergence is exactly the case where the operator most needs to
			// see which entries did land.
			if err := writePluginStatus(out, cfg.Plugins.Manifest, cfg.Plugins.Root,
				mergePluginStatus(deployment, pluginLoader.Status())); err != nil {
				return err
			}
			if applyErr != nil {
				return fmt.Errorf("reload plugin deployment %q: %w", cfg.Plugins.Manifest, applyErr)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "agent JSON config file")
	return cmd
}

// requirePluginLoader returns the process's loader, or an error explaining that
// there is none. It is reached only with a manifest configured, so "no loader"
// means the process never ran serve assembly — reporting an empty plugin list
// there would be a lie about a deployment that may be running elsewhere.
func requirePluginLoader(application *app.App) (*loader.Loader, error) {
	pluginLoader := application.Plugins()
	if pluginLoader == nil {
		return nil, errors.New("no plugin loader in this process: plugins are assembled by `agent serve`, " +
			"and a loader belongs to the service that built it")
	}
	return pluginLoader, nil
}

// pluginStatusRow is one entry as `agent plugins status` prints it.
type pluginStatusRow struct {
	Name    string
	Version string
	State   string
	Tools   []string

	// Detail is the row's already-labelled explanation: "error=..." for a
	// failure, "reason=..." for a state the operator's own manifest explains,
	// or both (a row that is out of the manifest AND failed carries the reason
	// followed by its own error=). Empty when a loaded entry has nothing to
	// explain.
	Detail string
}

// mergePluginStatus joins the deployment manifest on disk with what the loader
// actually converged, which is what makes the three "not working" cases
// distinguishable: the loader alone cannot report a DISABLED entry (it has
// nothing mounted and no failure for it) and the manifest alone cannot report a
// FAILED one.
//
// An entry the loader still has mounted but the manifest no longer declares is
// reported too, with its state and a reason saying the file has moved on. It is
// a real state — the file changed and nobody reloaded — and dropping it would
// hide a running plugin.
func mergePluginStatus(deployment manifest.Deployment, statuses []loader.InstanceStatus) []pluginStatusRow {
	byName := make(map[string]loader.InstanceStatus, len(statuses))
	for _, st := range statuses {
		byName[st.Name] = st
	}
	providerOf := pluginToolProviders(statuses)

	rows := make([]pluginStatusRow, 0, len(deployment.Plugins)+len(statuses))
	declared := make(map[string]bool, len(deployment.Plugins))
	for _, entry := range deployment.Plugins {
		declared[entry.Name] = true
		st, known := byName[entry.Name]
		row := pluginStatusRow{Name: entry.Name, Version: st.Version, State: st.State, Tools: st.Tools}
		switch {
		case known && st.State == loader.StateFailed:
			row.Detail = detailFor("error", st.LastError)
		case known && st.State == loader.StateSuspended:
			// Checked before the disabled-but-known case below, the same way
			// StateFailed already is: "status" re-reads the manifest from disk on
			// every call, independently of the loader's live state, so a plugin
			// that is mounted-and-suspended with a stale (not yet reloaded)
			// "enabled": false in the manifest is fully reachable. Its waiting_on=
			// explanation must not be replaced by the disabled reason below —
			// that is the entire reason SuspendedBy exists.
			row.Detail = suspendedRowDetail(st, providerOf, byName)
		case known && !entry.Enabled:
			// Mounted, but the file says it should not be. The manifest changed
			// under a running deployment and nobody has reloaded yet.
			row.Detail = detailFor("reason", `the manifest now sets "enabled": false; run "agent plugins reload" to unmount it`)
		case known:
			row.Detail = detailFor("error", st.LastError)
		case !entry.Enabled && !entry.GrantStated:
			// Not mounted, disabled, AND nobody has ever recorded a grant
			// decision for it (no "grant" block was ever present — see
			// manifest.Entry.GrantStated). This must be checked before the
			// plain "!entry.Enabled" case below, or every unauthorized entry
			// would be reported as disabled instead.
			row.State = pluginStateUnauthorized
			// NOTE-6: worded to say only what plugins.json itself supports —
			// "no grant block is present" — rather than asserting the wider
			// claim that nobody ever decided anything (see
			// pluginStateUnauthorized's own doc comment for why that wider
			// claim is not always true, and why this state is still the
			// right one to report anyway).
			row.Detail = detailFor("reason", `plugins.json records no grant block for this entry, so it has `+
				`never been authorized here; run "agent plugins grant" to authorize it`)
		case !entry.Enabled:
			row.State = pluginStateDisabled
			row.Detail = detailFor("reason", `the manifest entry sets "enabled": false`)
		default:
			row.State = pluginStatePending
			row.Detail = detailFor("reason", `enabled in the manifest but not converged; run "agent plugins reload"`)
		}
		rows = append(rows, row)
	}
	for _, st := range statuses {
		if declared[st.Name] {
			continue
		}
		row := pluginStatusRow{Name: st.Name, Version: st.Version, State: st.State, Tools: st.Tools}
		row.Detail = detailFor("reason", `no longer in the manifest; run "agent plugins reload" to unmount it`)
		// The entry's own failure keeps its own "error=" label instead of being
		// folded into the reason: a failure buried mid-sentence under the wrong
		// label is a failure nobody greps for. A suspension's waiting_on= gets
		// the same treatment, for the same reason.
		if waiting := suspendedWaitingOn(st.SuspendedBy, providerOf, byName); waiting != "" {
			row.Detail += "  " + waiting
		}
		if failure := detailFor("error", st.LastError); failure != "" {
			row.Detail += "  " + failure
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// pluginToolProviders maps every tool name to the plugin that contributes it,
// read from every entry the loader has ever mounted — a currently suspended
// entry included, since InstanceStatus.Tools always names what an instance
// contributes when it can work (see its doc comment), not just what it is
// contributing right now. It is what lets a suspended row tell "nobody
// provides this tool" apart from "the plugin that provides it is suspended
// too": the deployment manifest refuses two entries claiming the same tool
// name (manifest.AssembleSpec), so this mapping is never ambiguous.
func pluginToolProviders(statuses []loader.InstanceStatus) map[string]string {
	providerOf := make(map[string]string, len(statuses))
	for _, st := range statuses {
		for _, toolName := range st.Tools {
			providerOf[toolName] = st.Name
		}
	}
	return providerOf
}

// suspendedRowDetail is a StateSuspended row's Detail: what it is waiting on,
// followed by its own error= if the suspension itself carries one (see
// TestApplyReportsAResumeWhoseToolNameWasTaken in internal/plugin/loader for
// the case where SuspendedBy is empty and the error is the whole story).
func suspendedRowDetail(st loader.InstanceStatus, providerOf map[string]string, byName map[string]loader.InstanceStatus) string {
	detail := suspendedWaitingOn(st.SuspendedBy, providerOf, byName)
	if errDetail := detailFor("error", st.LastError); errDetail != "" {
		if detail != "" {
			detail += "  "
		}
		detail += errDetail
	}
	return detail
}

// suspendedWaitingOn renders a StateSuspended row's SuspendedBy as the row's
// own explanation of what it is blocked on, naming each tool and telling
// apart the two reasons a tool can be unresolved (brief decision #2):
//
//   - nobody in this loader's view has ever contributed the tool at all —
//     the operator's fix is to install a plugin that provides it;
//   - a plugin DOES provide it, but that plugin is not active either — the
//     operator's fix is one hop further up the chain, at the named plugin.
//     Its state is read from byName rather than hardcoded to "suspended":
//     it always IS suspended today (a loaded plugin's requirement would not
//     be unresolved, and a failed one contributes no Tools and so is never a
//     provider at all — see pluginToolProviders), but reading the true value
//     costs nothing and never asserts a state this package did not itself
//     observe.
//
// Empty when suspendedBy is empty, which is a real state: a suspended entry
// whose blocker was not a missing dependency (its tool name was taken by
// another contributor while it was down) carries nothing here and leans on
// the row's own error= instead.
func suspendedWaitingOn(suspendedBy []string, providerOf map[string]string, byName map[string]loader.InstanceStatus) string {
	if len(suspendedBy) == 0 {
		return ""
	}
	parts := make([]string, 0, len(suspendedBy))
	for _, toolName := range suspendedBy {
		provider, provided := providerOf[toolName]
		if !provided {
			parts = append(parts, fmt.Sprintf("%s(no plugin provides it)", toolName))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s(cascade: %s is %s)", toolName, provider, byName[provider].State))
	}
	return "waiting_on=" + strings.Join(parts, " ")
}

// pluginRowVersion is the version column's text: an entry that failed before
// its package could be read has no version to report, and a dash says so
// rather than leaving the column blank.
func pluginRowVersion(row pluginStatusRow) string {
	if row.Version == "" {
		return "-"
	}
	return row.Version
}

// detailFor labels one row's explanation, collapsing the newlines a joined
// error carries so that one entry stays one line and the output can be grepped.
func detailFor(label, text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	return label + "=" + strings.Join(strings.Fields(trimmed), " ")
}

// padRunes left-aligns s in a column width runes wide. It exists because the
// column widths are counted in runes while fmt's width verb counts bytes.
func padRunes(s string, width int) string {
	if pad := width - utf8.RuneCountInString(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// writePluginStatus prints the header and one aligned line per entry. The
// header names the manifest and root the rows were read against, because "the
// plugin is not there" is very often "the manifest is not the one you think".
func writePluginStatus(w io.Writer, manifestPath, root string, rows []pluginStatusRow) error {
	if _, err := fmt.Fprintf(w, "plugins: manifest=%s root=%s\n", manifestPath, root); err != nil {
		return fmt.Errorf("write plugin status header: %w", err)
	}
	if len(rows) == 0 {
		if _, err := fmt.Fprintln(w, "  no plugins declared in the deployment manifest"); err != nil {
			return fmt.Errorf("write plugin status empty state: %w", err)
		}
		return nil
	}

	// Widths are counted in RUNES, not bytes: fmt's %-*s pads to a byte count,
	// so a plugin whose name is not ASCII would otherwise push its whole row
	// out of the columns.
	nameWidth, stateWidth, versionWidth := 0, 0, 0
	for _, row := range rows {
		nameWidth = max(nameWidth, utf8.RuneCountInString(row.Name))
		stateWidth = max(stateWidth, utf8.RuneCountInString(row.State))
		versionWidth = max(versionWidth, utf8.RuneCountInString(pluginRowVersion(row)))
	}
	for _, row := range rows {
		// A suspended row's Tools names what the instance WOULD contribute once
		// unblocked (loader.InstanceStatus.Tools's own doc comment), not what it
		// is serving right now — nothing is, the tools are withdrawn for as long
		// as the plugin stays suspended. Marking that inline keeps a reader who
		// scans only the tools= column from mistaking a suspended plugin for an
		// active provider of the names it lists.
		toolsSuffix := ""
		if row.State == loader.StateSuspended && len(row.Tools) > 0 {
			toolsSuffix = "(withdrawn)"
		}
		line := fmt.Sprintf("  %s  %s  version=%s  tools=[%s]%s",
			padRunes(row.Name, nameWidth), padRunes(row.State, stateWidth),
			padRunes(pluginRowVersion(row), versionWidth), strings.Join(row.Tools, " "), toolsSuffix)
		if row.Detail != "" {
			line += "  " + row.Detail
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return fmt.Errorf("write plugin status row %q: %w", row.Name, err)
		}
	}
	return nil
}

// --- install -----------------------------------------------------------

// newPluginsInstallCommand builds `agent plugins install <url>`, which
// fetches and verifies a remote plugin package and registers it in the
// deployment manifest. Whether the written entry is authorized to run
// depends on --grant: with none given the entry is registered but not
// authorized (rule 4); with a non-empty --grant it is registered AND
// authorized in the same step (see resolveInstallGrants and D9's rationale
// in the fix brief) — install can never produce a "partially authorized"
// entry, because a grant that does not name EVERY capability the plugin
// declares is refused outright rather than written (see resolveInstallGrants
// for why a partial grant would otherwise never be able to mount).
//
// Order of operations, exactly as runPluginsInstall performs it and not
// negotiable (see its doc comment for why):
//
//  1. fetch.Fetch — the digest gates the bytes; mismatched bytes never reach
//     disk.
//  2. remote.Cache.Put — unpack and atomic placement in the plugin cache.
//  3. manifest.LoadPackage(dir, keyring) — SIGNATURE VERIFICATION, but only
//     when this deployment's plugins config sets "require_signature": true;
//     with it false, LoadPackage skips signature checking entirely and only
//     the wasm sha256 check runs (the command's own output says so on that
//     path — see the "signature NOT verified" line in runPluginsInstall).
//     Nothing past this step runs if verification that DID run fails.
//  4. manifest.DraftEntry -> manifest.AddEntry -> manifest.WriteDeployment.
//
// Nothing is ever written to plugins.json before step 3 succeeds: a
// verification failure — bad signature, a missing one under a required
// policy, or a digest mismatch caught even earlier, in step 1 — leaves the
// deployment manifest byte-for-byte untouched, and leaves nothing behind in
// the plugin cache either.
//
// install shares its cache, HTTP client, fetch/unpack limits and
// insecure-source policy with a running serve through resolvePluginRemote,
// and its trust set through resolvePluginKeyring — the same two functions
// newPluginLoader calls to build the loader `agent serve` runs. Deriving its
// own copies of either here would let a package install cleanly through this
// command and then have serve refuse to fetch or load it, with the
// contradiction invisible in both commands' output.
//
// With no --grant, the written entry keeps "enabled": false and NO "grant"
// block at all — not an empty "grant.capabilities", the whole key is
// omitted (manifest.DraftEntry leaves GrantStated false, and
// MarshalDeployment omits the entire "grant" key whenever GrantStated is
// false — see edit.go:174): install REGISTERS a package, it does not
// authorize one to run — see manifest.DraftEntry's doc comment, which
// enforces the same rule one layer down and offers no parameter to
// override it either. The next step is `agent plugins grant`.
//
// With a non-empty --grant, the named capabilities must be EXACTLY the set
// pm.Capabilities declares (not a subset — resolveInstallGrants refuses a
// partial grant by naming what is missing, because
// manifest.reconcileCapabilities would otherwise refuse the resulting entry
// at the next reload, discoverable only as a StateFailed row). The written
// entry then comes out both "enabled": true and carrying that grant
// ("grant.capabilities" set, GrantStated true) in the same step: naming a
// complete --grant IS the authorization decision, not a draft of one.
// --grant only fills Grant.Capabilities — allowed hosts and paths are still
// `agent plugins grant`'s job. If the plugin's own plugin.json declares a
// non-empty allowed_hosts (for "http") or allowed_paths (for "fs"), --grant
// refuses to name that capability at all rather than write an entry
// authorized with an allowlist that reaches nothing (NEW-1/SHOULD-FIX-4) —
// omit --grant and name the hosts/paths with `agent plugins grant` instead.
//
// install does not reload a running service, and does not need one to exist:
// it edits the deployment manifest on disk. Run `agent plugins reload` to
// converge a running serve onto the manifest this command just changed. With
// no --grant, the entry install just wrote is enabled:false, so a reload
// alone will not mount it — `agent plugins grant` is the remaining step;
// with --grant, the entry is already enabled and a reload alone applies it.
func newPluginsInstallCommand(out io.Writer) *cobra.Command {
	var configPath string
	var digestFlag string
	var grantFlag string
	cmd := &cobra.Command{
		Use:   "install <url>",
		Short: "Fetch, verify and register a remote plugin package, authorizing it only if --grant is given",
		Long: "Fetch, verify and register a remote plugin package.\n\n" +
			"install fetches the package at <url>, checks its bytes against --digest, unpacks it into the\n" +
			"configured plugin cache, and, if this deployment's plugins config sets \"require_signature\": true,\n" +
			"verifies its signature under the deployment's trust set (with it false, NOTHING is verified --\n" +
			"only the wasm sha256 check runs, and the command's own output says so). Only once verification\n" +
			"that DID run passes does it append an entry to plugins.json.\n\n" +
			"With no --grant, the entry is written \"enabled\": false with NO \"grant\" block at all:\n" +
			"install registers the package without authorizing it; run `agent plugins grant` to authorize it.\n\n" +
			"With --grant naming EXACTLY the capabilities the plugin declares in plugin.json (not a subset --\n" +
			"a partial grant would write an entry that can never load), the entry is written \"enabled\": true\n" +
			"with that grant already recorded: naming a complete --grant IS the authorization decision. --grant\n" +
			"only fills capabilities; allowed hosts and paths are still `agent plugins grant`'s job, and if the\n" +
			"plugin declares a non-empty allowed_hosts/allowed_paths, --grant refuses http/fs outright rather\n" +
			"than authorize an allowlist that reaches nothing.\n\n" +
			"install never reloads a running service: run `agent plugins reload` to converge it afterward.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginsInstall(cmd.Context(), out, args[0], digestFlag, grantFlag,
				cmd.Flags().Changed("grant"), configPath)
		},
	}
	cmd.Flags().StringVar(&digestFlag, "digest", "",
		`sha256 digest the fetched package must match, as "sha256:<hex>" (required: a remote entry is `+
			"never installed unverified)")
	cmd.Flags().StringVar(&grantFlag, "grant", "",
		"comma-separated capabilities to grant; must name EXACTLY the set the plugin declares in plugin.json "+
			"(not a subset), and authorizes the entry immediately (\"enabled\": true); allowed hosts and paths "+
			"are still `agent plugins grant`'s job, not this flag's")
	cmd.Flags().StringVar(&configPath, "config", "", "agent JSON config file")
	return cmd
}

// runPluginsInstall is newPluginsInstallCommand's RunE body. See that
// command's doc comment for the order of operations this function follows
// and why it is not negotiable.
func runPluginsInstall(ctx context.Context, out io.Writer, sourceArg, digestFlag, grantFlag string,
	grantFlagChanged bool, configPath string) error {
	source := strings.TrimSpace(sourceArg)
	if source == "" {
		return errors.New("plugins install: the source URL is empty")
	}
	digest := strings.TrimSpace(digestFlag)
	if digest == "" {
		return errors.New(`plugins install: --digest is empty; a remote plugin entry requires a sha256 digest, ` +
			`and this command will not install a package now and verify it later`)
	}

	cfg, err := config.Load(ctx, config.Options{Path: configPath})
	if err != nil {
		return fmt.Errorf("plugins install: %w", err)
	}
	if strings.TrimSpace(cfg.Plugins.Manifest) == "" {
		return errors.New(`plugins install: no "plugins.manifest" is configured, so install has no ` +
			`desired-state manifest to register this plugin's entry into; configure "plugins.manifest" first`)
	}

	// A minimal Entry built only to classify source (IsRemote, IsInsecureSource,
	// RemoteURL) exactly the way a deployed entry's Source is classified
	// everywhere else in this package — see manifest.Entry's doc comment. Its
	// Digest is left unset: classification does not need it, and setting it
	// here would risk this probe silently absorbing a validation DraftEntry is
	// supposed to be the one to perform, later, on the real entry.
	probe := manifest.Entry{Source: source}
	if !probe.IsRemote() {
		return fmt.Errorf(`plugins install: source %q is not an http:// or https:// URL; this command only `+
			"installs from a remote source (add a local package directly to the deployment manifest instead)",
			source)
	}

	// Read before anything is fetched: a manifest this package cannot read
	// back is a foundational problem this command should report cheaply,
	// before spending a network round trip and a cache write on a package
	// whose entry could never be appended to it anyway.
	//
	// D10: install deliberately does NOT bootstrap a missing plugins.json —
	// reusing readPluginDeployment keeps it agreeing with status and reload
	// on what "the deployment manifest" means, and inventing one here would
	// be install quietly creating deployment state nobody configured. But a
	// bare "file does not exist" is install's own first-run failure, not a
	// pre-existing deployment's, so it gets a remedy attached here rather
	// than inside readPluginDeployment (status and reload keep the bare
	// message; their context is "a deployment already exists").
	// existing and snapshot are read together, in one pass, by
	// readPluginDeploymentWithSnapshot: existing is used below to build the
	// updated document, and snapshot is what refusePluginDeploymentChanged
	// compares the file against right before the write, to catch an edit
	// that lands anywhere in the window this function is about to open --
	// the fetch, unpack and signature verification below can take seconds
	// to minutes under the configured limits.
	existing, snapshot, err := readPluginDeploymentWithSnapshot(cfg.Plugins.Manifest)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf(`plugins install: %w; install will not create this file itself -- the `+
				`deployment's desired state is the operator's to establish -- create it first with the `+
				`content {"plugins": []} to start from an empty deployment`, err)
		}
		return err
	}

	// Rule 9: where a fetched package is written is a deployment decision, and
	// this command will not pick one on the operator's behalf any more than
	// (*loader.Loader).remoteDir does (loader.go's remoteDir carries the same
	// refusal and the wording below stays consistent with it).
	if strings.TrimSpace(cfg.Plugins.Cache) == "" {
		return fmt.Errorf(`plugins install: source %q is remote, but this deployment configured no `+
			`"plugins.cache" directory; a fetched package has to be written somewhere, and this process will `+
			`not pick that location itself (configure "plugins.cache", or install the plugin under `+
			`"plugins.root" instead)`, source)
	}
	// The ONLY sanctioned constructor for the cache, HTTP client, fetch/unpack
	// limits and insecure-source policy — see resolvePluginRemote's own doc
	// comment. Routing through it here is what guarantees this command and a
	// running serve agree on every one of those.
	remote, err := resolvePluginRemote(cfg.Plugins)
	if err != nil {
		return err
	}

	// Rule 8, and it runs BEFORE fetch.Fetch is ever called: remoteDir
	// (loader.go) enforces the identical refusal on a manifest entry that is
	// already deployed, and install has to enforce it here too, or it becomes
	// the hole around that check — an operator could install over plaintext
	// through this command and only discover at `agent plugins reload` that
	// the running deployment refuses to fetch it.
	if probe.IsInsecureSource() && !remote.AllowInsecureSources {
		return fmt.Errorf(`plugins install: source %q is plaintext http, which this deployment does not `+
			`permit; plugin artifacts are fetched over https, and plaintext is a debugging aid that has to be `+
			`turned on explicitly with "allow_insecure_sources": true in the plugins config`, source)
	}

	// The ONLY sanctioned constructor for the trust set — see
	// resolvePluginKeyring's own doc comment. Unlike newPluginLoader (which
	// Warns once, at the assembly that drops an unenforced keyring), install
	// neither builds nor holds a Loader to log through — so the second
	// return value (droppedKeyringPath, the path of a keyring that loaded
	// but this policy does not enforce) is kept here instead, to fold into
	// the "signature NOT verified" line in this command's own success
	// output below, the only channel install has for a warning at all.
	keyring, droppedKeyringPath, err := resolvePluginKeyring(cfg.Plugins)
	if err != nil {
		return err
	}

	u, err := probe.RemoteURL()
	if err != nil {
		return fmt.Errorf("plugins install: %w", err)
	}

	// Step 1: the digest gates the bytes. A digest mismatch — or any other
	// fetch failure — returns here with nothing written anywhere: Fetch never
	// touches the filesystem, so there is nothing in the cache to clean up,
	// and nothing below this line runs to touch plugins.json.
	archive, err := fetch.Fetch(ctx, remote.Client, u, digest, remote.FetchLimits)
	if err != nil {
		return fmt.Errorf("plugins install: %w", err)
	}
	// Step 2: unpack and atomic placement under the digest that names it.
	dir, err := remote.Cache.Put(digest, archive, remote.UnpackLimits)
	if err != nil {
		return fmt.Errorf("plugins install: %w", err)
	}
	// Step 3: SIGNATURE VERIFICATION. Rule 1 — the core invariant of this
	// whole command — is that plugins.json is byte-for-byte unchanged when
	// this fails, which holds simply because nothing below this line has run
	// yet: no Deployment has been mutated, and WriteDeployment has not been
	// called.
	pm, _, err := manifest.LoadPackage(dir, keyring)
	if err != nil {
		return fmt.Errorf("plugins install: %w", err)
	}

	// Rule 5: granting a capability the plugin never declared is a config
	// error, not generosity. Checked before DraftEntry so a bad --grant never
	// reaches plugins.json either.
	grants, err := resolveInstallGrants(grantFlag, grantFlagChanged, pm)
	if err != nil {
		return err
	}

	// Step 4a: the draft. DraftEntry alone decides Name (from pm.Name, never
	// from anything this command was given) and Tools; it always produces
	// Enabled: false, GrantStated: false and empty Grant.Capabilities, with
	// no parameter to override any of them — see its own doc comment. That
	// layering stays correct: DraftEntry never authorizes anything, and the
	// override below happens here, in this command's own code, deliberately
	// not inside DraftEntry, so it is visible and reviewable rather than
	// buried in a helper every caller reaches for.
	entry, err := manifest.DraftEntry(pm, source, digest)
	if err != nil {
		return fmt.Errorf("plugins install: %w", err)
	}
	// D9: a non-empty --grant IS an authorization decision, not a draft of
	// one — resolveInstallGrants above already refused anything but a grant
	// naming EXACTLY pm.Capabilities, so this can never write an entry that
	// reconcileCapabilities would refuse for a partial grant. Enabled and
	// GrantStated must be set TOGETHER: MarshalDeployment omits the whole
	// "grant" block when GrantStated is false, so Enabled=true with
	// GrantStated left false would write an authorized entry carrying NO
	// capabilities at all, which reconcileCapabilities refuses just the same
	// — the entry would still land in `failed` on the next reload.
	if len(grants) > 0 {
		entry.Grant.Capabilities = grants
		entry.Enabled = true
		entry.GrantStated = true
	}

	// F5, shared with grant and deny via refusePluginDeploymentChanged (see
	// its own doc comment for why this exists): existing/snapshot were
	// captured together above, before the fetch — a window that spans an
	// entire artifact download, seconds to minutes under the configured
	// limits, long enough for a hand edit or another `agent plugins
	// grant`/`deny` to have changed plugins.json in the meantime.
	if err := refusePluginDeploymentChanged("plugins install", cfg.Plugins.Manifest, snapshot); err != nil {
		return err
	}

	// Step 4b/4c: AddEntry is rule 6 (a duplicate name is refused, naming it)
	// and WriteDeployment is the atomic, self-verifying write Task 2 built.
	// Both run only now, after every verification above has passed.
	updated, err := manifest.AddEntry(existing, entry)
	if err != nil {
		return fmt.Errorf("plugins install: %w", err)
	}
	if err := manifest.WriteDeployment(cfg.Plugins.Manifest, updated); err != nil {
		return fmt.Errorf("plugins install: %w", err)
	}

	if len(grants) > 0 {
		if _, err := fmt.Fprintf(out, "installed %q (version %s) from %s, with capabilities granted: %s.\n",
			pm.Name, pm.Version, source, strings.Join(grants, ", ")); err != nil {
			return fmt.Errorf("write plugins install output: %w", err)
		}
	} else if _, err := fmt.Fprintf(out, "installed %q (version %s) from %s, with no capabilities granted.\n",
		pm.Name, pm.Version, source); err != nil {
		return fmt.Errorf("write plugins install output: %w", err)
	}

	// D9: a non-empty --grant already authorized the entry above (Enabled
	// and GrantStated both true) -- claiming "NOT authorized" about an entry
	// this call just enabled would contradict what was written, so the two
	// branches say different things and name different remaining steps.
	if len(grants) > 0 {
		if _, err := fmt.Fprintf(out, "the entry is registered AND authorized to run (\"enabled\": true in "+
			"%s); run `agent plugins reload` to apply.\n", cfg.Plugins.Manifest); err != nil {
			return fmt.Errorf("write plugins install output: %w", err)
		}
	} else if _, err := fmt.Fprintf(out, "the entry is registered but NOT authorized to run (\"enabled\": false in "+
		"%s); run `agent plugins grant %s` to authorize it, then `agent plugins reload` to apply.\n",
		cfg.Plugins.Manifest, pm.Name); err != nil {
		return fmt.Errorf("write plugins install output: %w", err)
	}

	// F4: the help text and doc comments only promise verification when this
	// deployment's policy actually requires one. keyring is nil here exactly
	// when it did not run, so this is the one line that lets an operator
	// tell a verified install apart from an unverified one without reading
	// the config -- and it names the dropped keyring too, when one WAS
	// configured but this policy does not enforce it.
	if keyring == nil {
		msg := `signature NOT verified: this deployment sets "require_signature": false`
		if droppedKeyringPath != "" {
			msg = fmt.Sprintf(`signature NOT verified: this deployment configured a keyring (%s) but sets `+
				`"require_signature": false, so it was not enforced`, droppedKeyringPath)
		}
		if _, err := fmt.Fprintln(out, msg); err != nil {
			return fmt.Errorf("write plugins install output: %w", err)
		}
	}
	return nil
}

// resolveInstallGrants parses --grant's comma-separated capability list via
// splitFlagList — the SAME helper resolveGrantCapabilities uses via `agent
// plugins grant` — and checks the result against pm.Capabilities, the
// plugin's OWN declaration in plugin.json.
//
// Sharing splitFlagList here is SHOULD-FIX-1's fix: resolveInstallGrants
// used to split "--grant" with its own inline strings.Split and never
// refused a repeated name, so `--grant log,log` on a plugin declaring only
// "log" used to SUCCEED and write the literal repeated list
// ["log","log"] to plugins.json — nothing downstream complains
// (validateCapabilities checks names only, reconcileCapabilities builds a
// set), so the garbage was permanent and invisible, while the identical
// input to `agent plugins grant --capabilities log,log` was already refused
// by splitFlagList's own duplicate check (NOTE-7). The two commands
// validate one concept; they must not validate it two different ways.
//
// An ABSENT flag (grantFlagChanged false) returns a nil slice, which is what
// tells runPluginsInstall to leave DraftEntry's empty Grant.Capabilities
// exactly as it drafted it rather than overwrite it with another empty one.
// An EXPLICITLY EMPTY flag (grantFlagChanged true, but nothing but
// whitespace after trimming — "--grant" or "--grant   ") is refused rather
// than silently treated the same as absent: a malformed flag value is not
// the same as an absent one, and grantFlagChanged is what lets this
// function tell the two apart. This whole-flag-empty case has to be checked
// here, before splitFlagList ever runs, because splitFlagList itself treats
// an all-whitespace value as "no items" (returns nil, nil) rather than an
// error — only this caller has grantFlagChanged to tell an absent flag from
// a present-but-empty one apart. "--grant log,,http" is refused the same
// way splitFlagList refuses it for grant, by its own per-field empty check.
//
// The two sets — what --grant names and what pm.Capabilities declares — must
// be EQUAL, not merely "--grant is a subset of what's declared". Every named
// capability must be one pm declares (an extra name the plugin never asked
// for is a config error, not generosity, refused by naming both the
// capability and the plugin's actual declaration), AND every declared
// capability must be named (a missing one is refused too, naming what was
// left out): manifest.reconcileCapabilities (assemble.go) refuses any entry
// whose grant does not cover every capability the plugin declares, so a
// strict subset here would pass this check and then write an entry that
// install reports as installed and that can then never mount — silently
// parked in `failed`, discoverable only by reading `agent plugins status`.
// This mirrors `agent plugins grant`'s resolveGrantCapabilities exactly —
// same contract, so which command an operator used never changes whether a
// plugin's declared capabilities can install-then-fail-to-mount.
//
// Finally (SHOULD-FIX-4): --grant only ever fills Grant.Capabilities —
// AllowedHosts and AllowedPaths are left nil, exactly like a freshly
// drafted entry's own zero value. AssembleSpec intersects declared against
// granted (assemble.go:234-235) and an EMPTY intersection is legal there
// (see AssembleSpec's own doc comment), so granting "http" or "fs" here on
// a plugin that itself declares a non-empty allowed_hosts/allowed_paths
// would mount the plugin with that capability true and an allowlist
// reaching nothing — every outbound call denied by perm.Grant after the
// next reload, with nothing in install's own output, status output or the
// log saying why. This is the identical failure shape
// resolveGrantAllowedHosts/resolveGrantAllowedPaths were written to prevent
// on the grant path — caught there, left open here until now. Refuse it
// here too, naming the remaining `agent plugins grant` step.
func resolveInstallGrants(grantFlag string, grantFlagChanged bool, pm manifest.PluginManifest) ([]string, error) {
	trimmed := strings.TrimSpace(grantFlag)
	if trimmed == "" {
		if grantFlagChanged {
			return nil, fmt.Errorf(`plugins install: --grant was given but names no capability; an ` +
				`explicitly empty --grant is refused the same as a malformed one -- omit the flag entirely ` +
				`to install with no capabilities granted`)
		}
		return nil, nil
	}
	grants, err := splitFlagList("plugins install", "grant", grantFlag)
	if err != nil {
		return nil, err
	}
	for _, capability := range grants {
		if !slices.Contains(pm.Capabilities, capability) {
			return nil, fmt.Errorf("plugins install: --grant names capability %q, which plugin %q does not "+
				"declare in plugin.json (it declares: %v); granting a capability the plugin did not ask for "+
				"is a config error, not generosity", capability, pm.Name, pm.Capabilities)
		}
	}

	// F1: require the two sets to be EQUAL — see the doc comment above for
	// why a strict subset must be refused here rather than written.
	var missing []string
	for _, declared := range pm.Capabilities {
		if !slices.Contains(grants, declared) {
			missing = append(missing, declared)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("plugins install: --grant %q does not grant %v, which plugin %q declares in "+
			"plugin.json; a partial grant produces an entry the deployment can never load (every declared "+
			"capability must be granted, not a subset) -- name the complete list, or omit --grant to install "+
			"without authorizing it", grantFlag, missing, pm.Name)
	}

	// SHOULD-FIX-4 / NEW-1 — see refuseUnnamedAllowlist's doc comment: grant
	// (runPluginsGrant) calls the same function, so install and grant refuse
	// this shape identically instead of one refusing what the other silently
	// accepts. install has no --allowed-hosts/--allowed-paths flags of its
	// own, so it always calls with hosts/paths nil -- the "named none" branch
	// fires whenever the plugin declares a non-empty allowlist at all.
	if err := refuseUnnamedAllowlist("plugins install", "--grant", grants, pm, nil, nil,
		fmt.Sprintf(`omit --grant to install without authorizing it, then run "agent plugins grant %s `+
			`--capabilities ... --allowed-hosts ..." to authorize it with the hosts named too`, pm.Name),
		fmt.Sprintf(`omit --grant to install without authorizing it, then run "agent plugins grant %s `+
			`--capabilities ... --allowed-paths ..." to authorize it with the paths named too`, pm.Name)); err != nil {
		return nil, err
	}
	return grants, nil
}

// refuseUnnamedAllowlist is the ONE rule shared by install's --grant
// (resolveInstallGrants) and grant's --capabilities (runPluginsGrant),
// closing NEW-1: granting "http" (or "fs") while the plugin declares a
// non-empty "network"."allowed_hosts" (or "filesystem"."allowed_paths") in
// plugin.json, and naming none of those hosts/paths in this same call, would
// mount the plugin with that capability true and an allowlist that reaches
// nothing -- authoritative-looking, granting nothing, with nothing in the
// command's own output saying why (SHOULD-FIX-4's failure shape). Before
// this fix the two commands enforced only one direction each of the same
// concept: install refused this shape outright, while grant's own
// resolveGrantAllowedHosts/resolveGrantAllowedPaths only refused a NAMED
// host/path the plugin never declared and stayed silent when none was named
// at all -- `agent plugins grant jira --capabilities http,log` on a plugin
// declaring allowed_hosts would succeed and write an empty allowlist,
// character for character the state install refused. One shared function
// closes both directions the same way, instead of drifting the way two
// hand-written copies of one rule always eventually do.
//
// grants is the already set-equality-checked capability list about to be
// written; hosts and paths are what THIS call names (nil for install, which
// has no such flags at all). hostsRemedy and pathsRemedy are the exact
// next-step text to append -- install and grant point the operator at
// different places, since only grant itself accepts
// --allowed-hosts/--allowed-paths.
//
// This is deliberately keyed on the plugin's DECLARATION, not the effective
// allowlist AssembleSpec computes after intersecting it against what is
// granted: a plugin declaring "capabilities": ["http"] with an EMPTY
// "allowed_hosts" (or "fs" with no "allowed_paths") does NOT trip this
// guard, and must not -- that is a legitimate "reaches nothing by the
// plugin's own design" state neither command has any flag to fix (naming
// any host/path there would itself be refused as undeclared), not the
// "operator forgot to name what the plugin asked for" state this guard
// exists to catch. A plugin declaring neither http nor fs is unaffected
// either way; the guard never evaluates for it.
//
// This is a thin wrapper over consent.RefuseUnnamedAllowlist -- see that
// function's doc comment for the rule itself. cmdContext and flagLabel
// become consent.RefuseUnnamedAllowlist's actor and subject.
func refuseUnnamedAllowlist(cmdContext, flagLabel string, grants []string, pm manifest.PluginManifest,
	hosts, paths []string, hostsRemedy, pathsRemedy string) error {
	return consent.RefuseUnnamedAllowlist(cmdContext, flagLabel, grants, pm, hosts, paths, hostsRemedy, pathsRemedy)
}

// --- grant / deny ------------------------------------------------------

// newPluginsGrantCommand builds `agent plugins grant <name>`, the explicit
// act that authorizes an already-registered entry to run.
//
// grant is deliberately NOT "pick a subset of what the plugin asks for": the
// entry's grant must name EVERY capability the plugin declares in
// plugin.json, or manifest.reconcileCapabilities (assemble.go) refuses the
// resulting entry outright at the very next `agent plugins reload` — an
// entry that mounted through install/status as "unauthorized" would then
// mount as "failed" instead, discoverable only by reading plugins status
// again. So --capabilities and the plugin's own declaration must name
// exactly the same set: extra names the plugin never asked for are refused
// for the same reason install's own --grant refuses them (a capability the
// plugin did not ask for is a config error, not generosity), and missing
// names are refused just as loudly, before anything reaches plugins.json.
//
// grant never touches a running loader and never needs one to exist: like
// install, it edits the deployment manifest on disk, and its own output
// says so — run `agent plugins reload` to converge a running serve onto
// what this command just changed.
func newPluginsGrantCommand(out io.Writer) *cobra.Command {
	var configPath string
	var capabilitiesFlag string
	var allowedHostsFlag string
	var allowedPathsFlag string
	cmd := &cobra.Command{
		Use:   "grant <name>",
		Short: "Authorize a registered plugin entry to run with its declared capabilities",
		Long: "Authorize a registered plugin entry to run with its declared capabilities.\n\n" +
			"--capabilities must name EXACTLY the set of capabilities the plugin declares in its own\n" +
			"plugin.json -- not a subset and not a superset. A capability the plugin never asked for is\n" +
			"refused (a config error, not generosity), and a declared capability left out is refused just\n" +
			"as loudly: the deployment can never load an entry whose grant does not cover every capability\n" +
			"the plugin declares, so a partial grant is refused here rather than written and left to fail\n" +
			"silently at the next reload. A plugin that declares no capabilities is granted with\n" +
			"--capabilities left empty.\n\n" +
			"--allowed-hosts and --allowed-paths are checked differently: each named host/path must be one\n" +
			"the plugin itself declares in plugin.json's \"network.allowed_hosts\" / \"filesystem.allowed_paths\",\n" +
			"but naming a strict SUBSET of what is declared is accepted -- a legitimate narrowing, not a\n" +
			"partial grant. Only a name the plugin never declared is refused: AssembleSpec reconciles a grant's\n" +
			"hosts/paths against the plugin's own declaration by intersection, silently dropping anything the\n" +
			"plugin did not itself ask for, so an undeclared name would otherwise sit in plugins.json looking\n" +
			"authoritative while granting nothing.\n\n" +
			"grant never reloads a running service: run `agent plugins reload` to apply.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginsGrant(cmd.Context(), out, args[0], capabilitiesFlag, allowedHostsFlag, allowedPathsFlag, configPath)
		},
	}
	cmd.Flags().StringVar(&capabilitiesFlag, "capabilities", "",
		"comma-separated capabilities to grant; must name exactly the set the plugin itself declares in "+
			"plugin.json (empty for a plugin that declares none)")
	cmd.Flags().StringVar(&allowedHostsFlag, "allowed-hosts", "",
		"comma-separated hosts the http capability may reach; each must be one the plugin itself declares in "+
			`plugin.json's "network.allowed_hosts" (a subset of what is declared is fine; an undeclared host `+
			"is refused, since AssembleSpec would otherwise silently drop it from the grant)")
	cmd.Flags().StringVar(&allowedPathsFlag, "allowed-paths", "",
		"comma-separated paths the fs capability may reach; each must be one the plugin itself declares in "+
			`plugin.json's "filesystem.allowed_paths" (a subset of what is declared is fine; an undeclared `+
			"path is refused, since AssembleSpec would otherwise silently drop it from the grant)")
	cmd.Flags().StringVar(&configPath, "config", "", "agent JSON config file")
	return cmd
}

// runPluginsGrant is newPluginsGrantCommand's RunE body.
func runPluginsGrant(ctx context.Context, out io.Writer, nameArg, capabilitiesFlag, allowedHostsFlag, allowedPathsFlag, configPath string) error {
	name := strings.TrimSpace(nameArg)
	if name == "" {
		return errors.New("plugins grant: the plugin name is empty")
	}

	cfg, err := config.Load(ctx, config.Options{Path: configPath})
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Plugins.Manifest) == "" {
		return errors.New(`plugins grant: no "plugins.manifest" is configured, so there is no deployment entry ` +
			`to authorize`)
	}

	// existing and snapshot are read together, in one pass -- snapshot is
	// what refusePluginDeploymentChanged compares the file against right
	// before the write below, to catch an edit that lands anywhere in the
	// window this function is about to open: resolvePluginPackageDir just
	// below can perform a full artifact download on a cache miss, a window
	// as long as install's own fetch (BLOCKING-1).
	existing, snapshot, err := readPluginDeploymentWithSnapshot(cfg.Plugins.Manifest)
	if err != nil {
		return err
	}
	entry, err := consent.FindEntry(existing, name)
	if err != nil {
		return fmt.Errorf("plugins grant: %w", err)
	}

	// The same two resolvers install uses (resolvePluginRemote,
	// resolvePluginKeyring), so grant and install — and a running serve —
	// agree on what "the plugin's own declaration" means and under what
	// trust set it is read. A cache hit costs no network, exactly like
	// install's own remote path.
	remote, err := resolvePluginRemote(cfg.Plugins)
	if err != nil {
		return err
	}
	keyring, _, err := resolvePluginKeyring(cfg.Plugins)
	if err != nil {
		return err
	}
	dir, err := resolvePluginPackageDir(ctx, entry, remote, cfg.Plugins.Root)
	if err != nil {
		return fmt.Errorf("plugins grant: %w", err)
	}
	pm, _, err := manifest.LoadPackage(dir, keyring)
	if err != nil {
		return fmt.Errorf("plugins grant: %w", err)
	}

	capabilities, err := resolveGrantCapabilities(capabilitiesFlag, pm)
	if err != nil {
		return err
	}
	hosts, err := resolveGrantAllowedHosts(allowedHostsFlag, pm)
	if err != nil {
		return err
	}
	paths, err := resolveGrantAllowedPaths(allowedPathsFlag, pm)
	if err != nil {
		return err
	}

	// SHOULD-FIX-4 / NEW-1 — see refuseUnnamedAllowlist's doc comment: the
	// same function install's resolveInstallGrants calls, so this command
	// refuses the identical shape install refuses (granting http/fs while
	// the plugin declares a non-empty allowlist and none of it is named
	// here) instead of silently writing an empty allowlist install would
	// have refused to write.
	if err := refuseUnnamedAllowlist("plugins grant", "--capabilities", capabilities, pm, hosts, paths,
		"name at least one of the declared hosts with --allowed-hosts to authorize it with the hosts named too",
		"name at least one of the declared paths with --allowed-paths to authorize it with the paths named too",
	); err != nil {
		return err
	}

	// BLOCKING-1: existing/snapshot were captured together above, before
	// resolvePluginPackageDir's possible download and before LoadPackage --
	// see refusePluginDeploymentChanged's own doc comment for why this
	// check exists and why it is shared with install and deny rather than
	// reimplemented per command.
	if err := refusePluginDeploymentChanged("plugins grant", cfg.Plugins.Manifest, snapshot); err != nil {
		return err
	}

	updated, err := manifest.UpdateEntry(existing, name, func(e manifest.Entry) (manifest.Entry, error) {
		e.Enabled = true
		e.GrantStated = true
		e.Grant = manifest.GrantDecl{
			Capabilities: capabilities,
			AllowedHosts: hosts,
			AllowedPaths: paths,
		}
		return e, nil
	})
	if err != nil {
		return fmt.Errorf("plugins grant: %w", err)
	}
	if err := manifest.WriteDeployment(cfg.Plugins.Manifest, updated); err != nil {
		return fmt.Errorf("plugins grant: %w", err)
	}

	description := "no capabilities"
	if len(capabilities) > 0 {
		description = "capabilities: " + strings.Join(capabilities, ", ")
	}
	// SHOULD-FIX-5: grant replaces the WHOLE GrantDecl every time, and
	// --allowed-hosts/--allowed-paths default to empty, so a re-grant that
	// only names --capabilities silently wipes a previously granted
	// allowlist. Naming the EFFECTIVE hosts and paths here -- including
	// "none" when there are none -- is how an operator notices that before
	// the next reload denies every request, instead of after.
	hostsDescription := "none"
	if len(hosts) > 0 {
		hostsDescription = strings.Join(hosts, ", ")
	}
	pathsDescription := "none"
	if len(paths) > 0 {
		pathsDescription = strings.Join(paths, ", ")
	}
	if _, err := fmt.Fprintf(out, "granted %q (%s; allowed hosts: %s; allowed paths: %s); run `agent plugins "+
		"reload` to apply.\n", name, description, hostsDescription, pathsDescription); err != nil {
		return fmt.Errorf("write plugins grant output: %w", err)
	}
	return nil
}

// resolveGrantCapabilities parses --capabilities and checks it against
// pm.Capabilities, the plugin's OWN declaration in plugin.json.
//
// This is a thin wrapper: splitFlagList's flag parsing stays here (it is
// CLI-specific -- an HTTP endpoint gets its list already parsed out of JSON,
// with no comma-separated string to split), and the actual check --
// including why the two sets must be EQUAL rather than merely compatible --
// lives in consent.ResolveCapabilities, so a second caller cannot enforce it
// differently.
func resolveGrantCapabilities(capabilitiesFlag string, pm manifest.PluginManifest) ([]string, error) {
	capabilities, err := splitFlagList("plugins grant", "capabilities", capabilitiesFlag)
	if err != nil {
		return nil, err
	}
	return consent.ResolveCapabilities("plugins grant", capabilities, pm)
}

// resolveGrantAllowedHosts parses --allowed-hosts and checks each named host
// against pm.Network.AllowedHosts, the plugin's OWN declaration in
// plugin.json — the same set AssembleSpec (assemble.go) intersects a grant's
// AllowedHosts against (SHOULD-FIX-4).
//
// This is a thin wrapper: splitFlagList's flag parsing stays here, and the
// actual check -- including why this is deliberately NOT a set-equality
// check the way capabilities is -- lives in consent.ResolveAllowedHosts.
func resolveGrantAllowedHosts(allowedHostsFlag string, pm manifest.PluginManifest) ([]string, error) {
	hosts, err := splitFlagList("plugins grant", "allowed-hosts", allowedHostsFlag)
	if err != nil {
		return nil, err
	}
	return consent.ResolveAllowedHosts("plugins grant", hosts, pm)
}

// resolveGrantAllowedPaths is resolveGrantAllowedHosts for
// pm.Filesystem.AllowedPaths — see that function's doc comment for the
// reasoning, which applies identically here (SHOULD-FIX-4).
//
// This is a thin wrapper: splitFlagList's flag parsing stays here, and the
// actual check lives in consent.ResolveAllowedPaths.
func resolveGrantAllowedPaths(allowedPathsFlag string, pm manifest.PluginManifest) ([]string, error) {
	paths, err := splitFlagList("plugins grant", "allowed-paths", allowedPathsFlag)
	if err != nil {
		return nil, err
	}
	return consent.ResolveAllowedPaths("plugins grant", paths, pm)
}

// splitFlagList splits a comma-separated flag value into trimmed, non-empty,
// non-duplicate fields, refusing an empty item by name (a malformed value
// like "a,,b" is not the same as an absent one) and refusing a repeated item
// by name too (NOTE-7: "a,a" against a declared set that contains "a" would
// otherwise pass every set-membership check built on top of this function
// and write a nonsense repeated entry to plugins.json). It returns nil for an
// empty or all-whitespace flagValue. cmdContext and flagName only label the
// error.
func splitFlagList(cmdContext, flagName, flagValue string) ([]string, error) {
	trimmed := strings.TrimSpace(flagValue)
	if trimmed == "" {
		return nil, nil
	}
	fields := strings.Split(trimmed, ",")
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		item := strings.TrimSpace(field)
		if item == "" {
			return nil, fmt.Errorf("%s: --%s %q contains an empty entry", cmdContext, flagName, flagValue)
		}
		if _, dup := seen[item]; dup {
			return nil, fmt.Errorf("%s: --%s %q names %q more than once", cmdContext, flagName, flagValue, item)
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out, nil
}

// resolvePluginPackageDir resolves entry's package directory the same way a
// running Loader would (internal/plugin/loader's unexported packageDir and
// remoteDir), without needing a running Loader itself: grant has to load a
// plugin's OWN plugin.json to check its declared capabilities, for an entry
// that may be local or remote, and neither of loader's resolvers is
// reachable across the package boundary. A local Source is resolved against
// root with the identical root-escape refusal (see localPluginPackageDir); a
// remote Source is served from remote's plugin cache — a cache hit costs no
// network, exactly like install's own remote path, and a miss fetches
// through the same fetch.Fetch/Cache.Put install uses.
//
// NOTE-10 (duplicated trust boundary, judgement call recorded): this remote
// branch and localPluginPackageDir together are a second, hand-kept copy of
// loader.go's packageDir/remoteDir — the boundary that decides where
// executable wasm is read from. The review flagged the drift risk this
// creates: if internal/plugin/loader ever tightens either rule, this copy
// does not follow automatically, and `agent plugins grant` would keep
// validating a plugin's declared capabilities against a package resolved by
// the OLD rule. The durable fix is exporting the loader's resolver and
// calling it from here instead of re-implementing it; that touches
// internal/plugin/loader, which this fix batch's brief put out of scope
// (only internal/cli/plugins_command.go and its test may change), so the
// judgement call this batch makes is: document the duplication at both
// copies (this one names loader.go's packageDir/remoteDir; a symmetrical
// note belongs on those functions the next time loader.go is touched) and
// defer the export, rather than deepen the change beyond its authorized
// files. Anyone editing internal/plugin/loader's packageDir or remoteDir
// MUST check this function and localPluginPackageDir for the same edit.
func resolvePluginPackageDir(ctx context.Context, entry manifest.Entry, remote loader.RemoteConfig, root string) (string, error) {
	if !entry.IsRemote() {
		return localPluginPackageDir(entry.Name, root, entry.Source)
	}
	if remote.Cache == nil {
		return "", fmt.Errorf(`plugin %q: source %q is remote, but this deployment configured no "plugins.cache" `+
			"directory; a remote package has to be written somewhere, and that location is a deployment decision "+
			"rather than one this process may make on its own", entry.Name, entry.Source)
	}
	if entry.IsInsecureSource() && !remote.AllowInsecureSources {
		return "", fmt.Errorf(`plugin %q: source %q is plaintext http, which this deployment does not permit; `+
			`plaintext is a debugging aid and has to be turned on explicitly with "allow_insecure_sources": true `+
			"in the plugins config", entry.Name, entry.Source)
	}

	hit, err := remote.Cache.Has(entry.Digest)
	if err != nil {
		return "", fmt.Errorf("plugin %q: look up %s in the plugin cache: %w", entry.Name, entry.Digest, err)
	}
	if hit {
		// Same digest, same bytes: nothing is requested, and nothing needs
		// to be.
		return remote.Cache.Dir(entry.Digest), nil
	}
	u, err := entry.RemoteURL()
	if err != nil {
		return "", fmt.Errorf("plugin %q: %w", entry.Name, err)
	}
	archive, err := fetch.Fetch(ctx, remote.Client, u, entry.Digest, remote.FetchLimits)
	if err != nil {
		return "", fmt.Errorf("plugin %q: %w", entry.Name, err)
	}
	dir, err := remote.Cache.Put(entry.Digest, archive, remote.UnpackLimits)
	if err != nil {
		return "", fmt.Errorf("plugin %q: %w", entry.Name, err)
	}
	return dir, nil
}

// localPluginPackageDir mirrors internal/plugin/loader's unexported
// packageDir for a local Source, since that function cannot be called across
// the package boundary: the same two refusals — an absolute source, and a
// relative source that walks out of root via ".." (filepath.Join cleans a
// path, it does not confine it) — are enforced here too. A plugin's wasm is
// code that runs, and where it is read from is a trust decision the
// deployment root is supposed to bound; grant must enforce that bound
// exactly as strictly as a running Loader does, not more loosely just
// because no loader happens to be present.
//
// NOTE-10: this is a hand-kept copy, not a shared implementation — see
// resolvePluginPackageDir's doc comment for the drift risk that creates and
// why this fix batch documents rather than closes it. Anyone editing
// internal/plugin/loader's packageDir MUST check this function for the same
// edit.
func localPluginPackageDir(name, root, source string) (string, error) {
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

// newPluginsDenyCommand builds `agent plugins deny <name>`, which revokes a
// plugin entry's authorization to run without discarding its registration.
//
// deny KEEPS GrantStated true (a decision WAS made — see
// manifest.Entry.GrantStated) and leaves Source, Digest and Tools exactly as
// they were: deleting the entry would throw away the source and digest,
// which are painful to reconstruct by hand. It never loads the plugin
// package, and never touches a running loader — like grant, it edits
// plugins.json on disk and says so in its own output.
func newPluginsDenyCommand(out io.Writer) *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "deny <name>",
		Short: "Revoke a plugin entry's authorization to run, keeping its registration",
		Long: "Revoke a plugin entry's authorization to run, keeping its registration.\n\n" +
			"deny sets \"enabled\": false and empties the entry's granted capabilities, but keeps its\n" +
			"grant block present (a decision WAS made) and leaves source, digest and tools untouched --\n" +
			"deleting the entry would throw away the source and digest, which are painful to reconstruct\n" +
			"by hand.\n\n" +
			"deny never reloads a running service: run `agent plugins reload` to apply.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginsDeny(cmd.Context(), out, args[0], configPath)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "agent JSON config file")
	return cmd
}

// pluginsDenyConcurrentEditTestHook, when non-nil, is called by
// runPluginsDeny right after it snapshots the deployment manifest and
// before it checks the snapshot for a concurrent edit (BLOCKING-1). deny,
// unlike install and grant, performs no blocking I/O of its own between
// those two points, so a test has no naturally-occurring window to land a
// concurrent edit inside deterministically the way
// TestPluginsInstallRefusesAConcurrentEditDuringTheDownload stands one up
// inside an HTTP handler its fetch blocks on -- this hook is deny's
// equivalent seam. Left nil outside tests; production code never sets it.
var pluginsDenyConcurrentEditTestHook func()

// runPluginsDeny is newPluginsDenyCommand's RunE body. ctx bounds
// config.Load exactly the way every other subcommand in this file bounds it
// (status, reload, install, grant) — deny used to be the one command that
// ignored the command's own context and called config.Load with
// context.Background() instead (SHOULD-FIX-5), which would have made it the
// one subcommand that could not be cancelled if config.Load ever grew an I/O
// timeout or a remote config source.
func runPluginsDeny(ctx context.Context, out io.Writer, nameArg, configPath string) error {
	name := strings.TrimSpace(nameArg)
	if name == "" {
		return errors.New("plugins deny: the plugin name is empty")
	}

	cfg, err := config.Load(ctx, config.Options{Path: configPath})
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Plugins.Manifest) == "" {
		return errors.New(`plugins deny: no "plugins.manifest" is configured, so there is no deployment entry ` +
			`to deny`)
	}

	// existing and snapshot are read together, in one pass -- snapshot is
	// what refusePluginDeploymentChanged compares the file against right
	// before the write below (BLOCKING-1). deny loads no package and makes
	// no network call, so this window is far shorter than install's or
	// grant's, but the shape -- and the guard -- is identical; see
	// refusePluginDeploymentChanged's own doc comment.
	existing, snapshot, err := readPluginDeploymentWithSnapshot(cfg.Plugins.Manifest)
	if err != nil {
		return err
	}
	if pluginsDenyConcurrentEditTestHook != nil {
		pluginsDenyConcurrentEditTestHook()
	}
	if err := refusePluginDeploymentChanged("plugins deny", cfg.Plugins.Manifest, snapshot); err != nil {
		return err
	}
	updated, err := manifest.UpdateEntry(existing, name, func(e manifest.Entry) (manifest.Entry, error) {
		e.Enabled = false
		e.GrantStated = true
		e.Grant = manifest.GrantDecl{}
		// Source, Digest and Tools are left exactly as they were: deny
		// revokes authorization, it does not throw away the registration.
		return e, nil
	})
	if err != nil {
		return fmt.Errorf("plugins deny: %w", err)
	}
	if err := manifest.WriteDeployment(cfg.Plugins.Manifest, updated); err != nil {
		return fmt.Errorf("plugins deny: %w", err)
	}

	if _, err := fmt.Fprintf(out, "denied %q: capabilities revoked; run `agent plugins reload` to apply.\n",
		name); err != nil {
		return fmt.Errorf("write plugins deny output: %w", err)
	}
	return nil
}

// --- keygen / sign ---------------------------------------------------------

// privateKeyFileMode is the mode `agent plugins keygen` creates a private key
// file with: readable and writable by its owner, invisible to everyone else.
//
// On Windows Go maps a mode onto the read-only attribute alone, so the bits
// below do not become an ACL there. That is a real limitation of the platform
// and is stated in the command's own output rather than papered over: an
// operator on Windows has to protect the file themselves.
const privateKeyFileMode = 0o600

// signatureFileMode is the mode plugin.sig is written with. A signature is
// public by construction — it is checked against public keys, and it travels
// with the package — so it is deliberately NOT 0600: a signature nobody can
// read is a package nobody can verify.
const signatureFileMode = 0o644

// mustMarkFlagRequired marks name required on cmd, panicking when there is no
// such flag. That is a programming error in this file, not a runtime
// condition, and a command whose "required" marking silently failed to apply
// would go on to accept an empty value for a key id or a key path.
func mustMarkFlagRequired(cmd *cobra.Command, name string) {
	if err := cmd.MarkFlagRequired(name); err != nil {
		panic(fmt.Sprintf("cli: mark flag %q required on command %q: %v", name, cmd.Name(), err))
	}
}

// newPluginsKeygenCommand builds `agent plugins keygen`, which mints the
// Ed25519 key pair a deployment signs its plugin packages with.
//
// The two halves go to two different places, and that split is the whole
// design: the private key is written to a file only its owner can read and is
// never rendered anywhere else, while the public half is printed as a keyring
// entry, ready to paste into the "keys" array of the trust set the deployment
// configures through plugins.keyring.
func newPluginsKeygenCommand(out io.Writer) *cobra.Command {
	var keyID string
	var privateKeyPath string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Mint an Ed25519 key pair for signing plugin packages",
		Long: "Mint an Ed25519 key pair for signing plugin packages.\n\n" +
			"The private key is written to --private-key and is never printed. The public half is\n" +
			"printed as a keyring entry, to paste into the \"keys\" array of the keyring named by\n" +
			"the plugins.keyring config setting.\n\n" +
			"An existing --private-key file is never overwritten: overwriting a private key\n" +
			"invalidates every signature ever made with it, and nothing can undo it.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runPluginsKeygen(out, keyID, privateKeyPath)
		},
	}
	cmd.Flags().StringVar(&keyID, "key-id", "",
		"name this key carries into the keyring and into every signature it makes")
	cmd.Flags().StringVar(&privateKeyPath, "private-key", "",
		"file to write the new private key to; it must not already exist")
	mustMarkFlagRequired(cmd, "key-id")
	mustMarkFlagRequired(cmd, "private-key")
	return cmd
}

// runPluginsKeygen generates a key pair, writes the private half to path and
// prints the public half as a keyring entry.
//
// The order of operations is deliberate. The key pair is minted and both
// documents are encoded IN MEMORY first, so every way this can fail on bad
// input fails before anything is created on disk. The file is then created
// with O_EXCL, which is both the refusal to overwrite an existing key and the
// only form of that refusal that cannot lose a race with another process
// between the check and the write. The write is followed by Sync before
// Close: without it, a crash between "wrote the private key" being printed
// and the data actually reaching the platter can lose the only copy of a key
// whose entry the operator has already pasted into a keyring, and the O_EXCL
// refusal above would then make recreating it under the same name harder,
// not easier. A failure to print the keyring entry after the key is safely
// on disk is treated the same as a failure to write it: the file is removed
// and the whole call reports an error, rather than leaving a valid key on
// disk whose public entry the operator never saw and which O_EXCL would then
// block them from regenerating at the same path.
//
// --key-id and --private-key are both trimmed of leading and trailing
// whitespace before use; the trimmed value is what goes into the key file
// AND the printed entry, so the two can never disagree, but a value entered
// with stray whitespace is silently normalized rather than rejected.
//
// No error, and no line of output, ever carries the private key or any part
// of it. The only place it goes is the file. Beyond the clear() calls below,
// nothing in this function attempts to zero the key material it holds: this
// is a seconds-long CLI invocation whose process exits (or fails) shortly
// after this function returns, Go's garbage collector is free to relocate or
// copy the underlying memory before or after clear() runs, and ed25519's
// GenerateKey/Sign and encoding/json's Marshal are handed the key and keep
// their own internal copies outside this function's control. clear() is
// still worth doing as defense in depth against a slow leak in a long-lived
// process, but its presence here is not a guarantee that no copy of the key
// ever lingers in this process's memory.
func runPluginsKeygen(out io.Writer, keyID string, privateKeyPath string) error {
	id := sign.KeyID(strings.TrimSpace(keyID))
	if id == "" {
		return errors.New("plugins keygen: --key-id is empty; a signature names the keyring entry it was " +
			"made with, so a key with no name can never be resolved by a verifier")
	}
	path := strings.TrimSpace(privateKeyPath)
	if path == "" {
		return errors.New("plugins keygen: --private-key is empty; there is nowhere to write the key, and " +
			"a private key is not something this command will print instead")
	}

	pub, priv, err := sign.GenerateKey()
	if err != nil {
		return fmt.Errorf("plugins keygen: %w", err)
	}
	defer clear(priv)
	keyDoc, err := sign.MarshalPrivateKey(id, priv)
	if err != nil {
		return fmt.Errorf("plugins keygen: %w", err)
	}
	defer clear(keyDoc)
	entry, err := sign.MarshalKeyEntry(id, pub)
	if err != nil {
		return fmt.Errorf("plugins keygen: %w", err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateKeyFileMode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("plugins keygen: %s already exists and will not be overwritten: overwriting a "+
				"private key invalidates every signature ever made with it and cannot be undone; write the new "+
				"key elsewhere, or move the existing one aside yourself first", path)
		}
		return fmt.Errorf("create private key file %q: %w", path, err)
	}
	if _, err := file.Write(keyDoc); err != nil {
		failure := fmt.Errorf("write private key file %q: %w", path, err)
		if cerr := file.Close(); cerr != nil {
			failure = errors.Join(failure, fmt.Errorf("close private key file %q: %w", path, cerr))
		}
		return errors.Join(failure, removeIncompletePrivateKey(path))
	}
	if err := file.Sync(); err != nil {
		failure := fmt.Errorf("sync private key file %q: %w", path, err)
		if cerr := file.Close(); cerr != nil {
			failure = errors.Join(failure, fmt.Errorf("close private key file %q: %w", path, cerr))
		}
		return errors.Join(failure, removeIncompletePrivateKey(path))
	}
	if err := file.Close(); err != nil {
		return errors.Join(fmt.Errorf("close private key file %q: %w", path, err),
			removeIncompletePrivateKey(path))
	}

	if _, err := fmt.Fprintf(out, "wrote the private key for %q to %s (mode %#o%s).\n",
		id, path, privateKeyFileMode, privateKeyModeCaveat()); err != nil {
		return errors.Join(fmt.Errorf("write plugins keygen output: %w", err), removeIncompletePrivateKey(path))
	}
	if _, err := fmt.Fprintf(out, "it is not printed here, and nothing in this tool ever prints it.\n\n"+
		"paste this entry into the \"keys\" array of the keyring named by plugins.keyring:\n%s", entry); err != nil {
		return errors.Join(fmt.Errorf("write plugins keygen output: %w", err), removeIncompletePrivateKey(path))
	}
	return nil
}

// privateKeyModeCaveat is the honest half of what keygen prints about the
// file mode: on Windows the mode is not what it says it is, and an operator
// reading "mode 0600" there would be reading a promise the platform did not
// make. Go maps a file mode onto the read-only attribute only, so the file is
// readable by every account on the machine.
func privateKeyModeCaveat() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	return "; on Windows this mode is not enforced — restrict the file yourself"
}

// removeIncompletePrivateKey deletes a private key file that was created but
// never completely written.
//
// It exists because the refusal to overwrite is keyed on the file EXISTING: a
// zero-length leftover from a failed run would block every retry while holding
// no key at all, and the operator's only way out would be to delete a file
// they have every reason to believe is a private key. A cleanup that itself
// fails is returned, never swallowed — a half-written key file that could not
// be removed is exactly the kind of thing that must not be discovered later.
func removeIncompletePrivateKey(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the incomplete private key file %q: %w", path, err)
	}
	return nil
}

// newPluginsSignCommand builds `agent plugins sign`, which signs a plugin
// package's plugin.json with a private key and writes the detached signature
// to plugin.sig beside it — the file manifest.LoadPackage verifies when the
// deployment has a keyring.
//
// The key id is NOT a flag here: it travels inside the private key file, bound
// to the key material when keygen minted the pair. A key id passed separately
// would be a name that can be mistyped into a signature no keyring can
// resolve, and the mistake would only surface at mount time.
func newPluginsSignCommand(out io.Writer) *cobra.Command {
	var privateKeyPath string
	cmd := &cobra.Command{
		Use:   "sign <package directory>",
		Short: "Sign a plugin package's plugin.json, writing plugin.sig beside it",
		Long: "Sign a plugin package's plugin.json, writing plugin.sig beside it.\n\n" +
			"The signature covers plugin.json's raw bytes exactly as they are on disk, which is\n" +
			"what a deployment's keyring verifies at load time. Re-signing a package is a normal\n" +
			"operation and replaces an existing plugin.sig, which this command says out loud.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runPluginsSign(out, args[0], privateKeyPath)
		},
	}
	cmd.Flags().StringVar(&privateKeyPath, "private-key", "",
		"private key file to sign with, as written by `agent plugins keygen`")
	mustMarkFlagRequired(cmd, "private-key")
	return cmd
}

// runPluginsSign signs packageDir/plugin.json with the key in privateKeyPath
// and writes packageDir/plugin.sig.
//
// It signs the manifest's RAW BYTES, exactly as read, and does not parse them:
// what a verifier checks is a byte string, so re-encoding anything here would
// risk signing something other than the file that ships. By the same token
// this command does not judge whether plugin.json is a valid manifest —
// LoadPackage does that, after checking the signature, and duplicating the
// schema here would mean a signer that has to be updated in lockstep with a
// format it has no stake in.
//
// The signature is verified before it is written (see verifyOwnSignature).
// Nothing is written if that check fails, and plugin.sig is written through
// writeFileAtomically so a failed write can never leave a truncated,
// unparseable document in place of a good signature that used to be there.
//
// The package directory and --private-key path are both trimmed of leading
// and trailing whitespace before use; a value entered with stray whitespace
// is silently normalized rather than rejected.
//
// Beyond the clear() calls below, nothing in this function attempts to zero
// the key material it reads: this is a seconds-long CLI invocation, Go's
// garbage collector is free to relocate the underlying memory around
// clear(), and ed25519.Sign is handed the key and may keep its own copy.
// clear() is still worth doing as defense in depth, not as a guarantee.
func runPluginsSign(out io.Writer, packageDir string, privateKeyPath string) error {
	dir := strings.TrimSpace(packageDir)
	if dir == "" {
		return errors.New("plugins sign: the package directory is empty; name the directory holding the " +
			"plugin.json to sign")
	}
	keyPath := strings.TrimSpace(privateKeyPath)
	if keyPath == "" {
		return errors.New("plugins sign: --private-key is empty; there is no key to sign with")
	}

	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read private key file %q: %w", keyPath, err)
	}
	defer clear(keyData)
	id, priv, err := sign.ParsePrivateKey(keyData)
	if err != nil {
		return fmt.Errorf("parse private key file %s: %w", keyPath, err)
	}
	defer clear(priv)

	manifestPath := filepath.Join(dir, "plugin.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read the plugin.json to sign at %s: %w", manifestPath, err)
	}

	signature, err := sign.Sign(priv, id, manifestData)
	if err != nil {
		return fmt.Errorf("sign %s: %w", manifestPath, err)
	}
	doc, err := sign.MarshalSignature(signature)
	if err != nil {
		return fmt.Errorf("encode the signature for %s: %w", manifestPath, err)
	}
	if err := verifyOwnSignature(doc, id, priv, manifestData); err != nil {
		return fmt.Errorf("sign %s: %w", manifestPath, err)
	}

	sigPath := filepath.Join(dir, "plugin.sig")
	replaced := false
	switch _, err := os.Stat(sigPath); {
	case err == nil:
		replaced = true
	case errors.Is(err, fs.ErrNotExist):
		replaced = false
	default:
		return fmt.Errorf("inspect the existing signature at %s: %w", sigPath, err)
	}
	if err := writeFileAtomically(sigPath, doc, signatureFileMode); err != nil {
		return fmt.Errorf("write the signature to %s: %w", sigPath, err)
	}

	note := ""
	if replaced {
		// Announced rather than silent: re-signing with a DIFFERENT key is the
		// quietest way to change which key a deployment's packages answer to,
		// and an operator who did it by accident has to be able to see it in
		// the output they already read.
		note = " (replaced the signature that was already there)"
	}
	if _, err := fmt.Fprintf(out, "signed %s with key %q, and verified the result against that key.\n",
		manifestPath, id); err != nil {
		return fmt.Errorf("write plugins sign output: %w", err)
	}
	if _, err := fmt.Fprintf(out, "wrote %s%s.\n", sigPath, note); err != nil {
		return fmt.Errorf("write plugins sign output: %w", err)
	}
	return nil
}

// writeFileAtomically writes data to a temp file created beside path and
// renames it into place, so that a process that dies mid-write can never
// leave path holding a truncated, unparseable document in place of a good
// one that used to be there. It is used for plugin.sig, which sign allows to
// be replaced (an operator re-signing a package is normal): the risk it
// guards against is availability, not secrecy — a signature is public by
// construction — but a half-written plugin.sig destroys a working signature
// for no better reason than an interrupted write, and the fix costs one
// extra file plus a rename.
//
// The temp file is created in the SAME directory as path (not the OS temp
// directory), so the final os.Rename is a same-filesystem move: on every
// platform this package runs on, that makes the rename atomic with respect
// to a concurrent reader, which would not be guaranteed across filesystems.
func writeFileAtomically(path string, data []byte, mode fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create a temp file beside %q: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err == nil {
			return
		}
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove temp file %q: %w", tmpPath, rmErr))
		}
	}()

	if _, werr := tmp.Write(data); werr != nil {
		failure := fmt.Errorf("write temp file %q: %w", tmpPath, werr)
		if cerr := tmp.Close(); cerr != nil {
			failure = errors.Join(failure, fmt.Errorf("close temp file %q: %w", tmpPath, cerr))
		}
		return failure
	}
	if serr := tmp.Sync(); serr != nil {
		failure := fmt.Errorf("sync temp file %q: %w", tmpPath, serr)
		if cerr := tmp.Close(); cerr != nil {
			failure = errors.Join(failure, fmt.Errorf("close temp file %q: %w", tmpPath, cerr))
		}
		return failure
	}
	if cerr := tmp.Close(); cerr != nil {
		return fmt.Errorf("close temp file %q: %w", tmpPath, cerr)
	}
	if cherr := os.Chmod(tmpPath, mode); cherr != nil {
		return fmt.Errorf("set mode on temp file %q: %w", tmpPath, cherr)
	}
	if rerr := os.Rename(tmpPath, path); rerr != nil {
		return fmt.Errorf("rename %q into place at %q: %w", tmpPath, path, rerr)
	}
	return nil
}

// verifyOwnSignature checks the plugin.sig document this command is about to
// write the same way the loader will check it: the bytes are parsed back with
// sign.ParseSignature, the trust set an operator would hold is rebuilt from
// the signing key's own public half (through sign.MarshalKeyring and
// sign.ParseKeyring, so the check runs through the production reader rather
// than a privately assembled keyring that could drift from it), and
// sign.Keyring.Verify runs over the same manifest bytes that were signed.
//
// This is not a formality, and it is not a test of the crypto library. An
// Ed25519 private key is a seed followed by the public key that seed derives,
// and nothing in the format binds the two together: a file that pairs one
// pair's seed with another pair's public half is well-formed in every
// checkable way, signs without complaint, and produces signatures that verify
// against NOTHING — not the stated key, not any other. Without this check the
// command would report success, write that signature out, and the failure
// would surface at deployment as "signature verification is broken", which is
// the one lesson a signing tool must never teach.
func verifyOwnSignature(doc []byte, id sign.KeyID, priv ed25519.PrivateKey, message []byte) error {
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		panic(fmt.Sprintf("cli: an ed25519 private key's Public() returned %T", priv.Public()))
	}
	keyringDoc, err := sign.MarshalKeyring(id, pub)
	if err != nil {
		return fmt.Errorf("build the trust set to check our own signature against: %w", err)
	}
	keyring, err := sign.ParseKeyring(keyringDoc)
	if err != nil {
		return fmt.Errorf("read back the trust set to check our own signature against: %w", err)
	}
	parsed, err := sign.ParseSignature(doc)
	if err != nil {
		return fmt.Errorf("read back the signature just produced: %w", err)
	}
	if err := keyring.Verify(parsed, message); err != nil {
		return fmt.Errorf("the signature just produced does not verify against key %q's own public half, so a "+
			"deployment would refuse this package; nothing was written: %w", id, err)
	}
	return nil
}
