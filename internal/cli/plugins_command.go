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
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/config"
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
// and telling the last three apart is the whole point of the command: an entry
// the operator switched off, an entry that tried and failed, and an entry that
// nobody has converged yet are three different problems with three different
// fixes.
//
// Two of the four come from the loader itself (loader.StateLoaded, "the plugin
// is mounted right now", and loader.StateFailed, "it is in the target state and
// nothing is mounted for it") and are printed as it reports them. The other two
// exist only here, because only the manifest on disk can tell them apart from
// an entry the loader has simply never heard of.
const (
	// pluginStateDisabled means the manifest entry says "enabled": false. This
	// is the operator's own doing, not a fault.
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
	})
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

// readPluginDeployment reads and parses the deployment manifest at path. Both
// failures name the path: "configured but unreadable" is a startup failure, and
// an operator has to be able to see which file was meant.
func readPluginDeployment(path string) (manifest.Deployment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest.Deployment{}, fmt.Errorf("read plugin deployment manifest %q: %w", path, err)
	}
	deployment, err := manifest.ParseDeployment(data)
	if err != nil {
		return manifest.Deployment{}, fmt.Errorf("parse plugin deployment manifest %q: %w", path, err)
	}
	return deployment, nil
}

// newPluginsCommand builds `agent plugins`, the operator's handle on the WASM
// plugin deployment. Its four subcommands fall into two groups that share
// nothing but the noun:
//
//   - status and reload are a view of THIS PROCESS: both read the loader serve
//     assembled (App.Plugins). There is no cross-process view — a loader
//     belongs to the serve that built it, so running them against a process
//     that never assembled one reports exactly that instead of an empty answer
//     that reads like "no plugins".
//   - keygen and sign touch no loader, no config and no running service. They
//     are the tools that PRODUCE what verification consumes, and they ship
//     alongside it deliberately: a deployment that can check signatures but
//     has no way to make one has exactly one option left, turning the
//     requirement off, which is the outcome signature verification exists to
//     prevent.
func newPluginsCommand(application *app.App, out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Inspect and reload the WASM plugin deployment, and sign plugin packages",
	}
	cmd.AddCommand(newPluginsStatusCommand(application, out))
	cmd.AddCommand(newPluginsReloadCommand(application, out))
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
