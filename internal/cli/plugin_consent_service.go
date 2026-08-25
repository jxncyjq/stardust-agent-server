package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/stardust/legion-agent/internal/plugin/consent"
	"github.com/stardust/legion-agent/internal/plugin/loader"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/plugin/sign"
	"github.com/stardust/legion-agent/internal/server"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// PluginConsentService is internal/cli's implementation of
// server.PluginConsent: it turns the deployment manifest on disk and this
// process's running loader into the server.PluginView list the GUI's plugin
// consent dialog renders.
//
// List reuses mergePluginStatus (plugins_command.go) for the STATE every row
// carries, rather than recomputing "unauthorized/disabled/loaded/failed/
// suspended/pending" from the Deployment and the loader's statuses a second
// time -- a second set of state rules is exactly what this type exists to
// prevent (see mergePluginStatus's own doc comment). It adds only what
// `agent plugins status` does not already report: each entry's DECLARED
// capabilities/hosts/paths, read from its own plugin.json, alongside what the
// deployment manifest actually GRANTS it.
type PluginConsentService struct {
	manifestPath string
	root         string
	pluginsFn    func() *loader.Loader
	keyringFn    func() *sign.Keyring
	remote       loader.RemoteConfig
}

// NewPluginConsentService builds a PluginConsentService.
//
// manifestPath and root are the deployment manifest path and package root
// this process's serve assembly resolved (config.PluginsConfig's Manifest
// and Root). pluginsFn returns the loader currently attached to this
// process -- nil once drainPlugins has detached it (e.g. mid-shutdown), in
// which case List reports that rather than dereferencing a nil Loader.
//
// pluginsFn is a closure rather than a plain value so that a caller which
// re-assembles serve in the same process (drainPlugins, then a fresh
// assemblePlugins) is read through to the CURRENT loader, not the one that
// existed when NewPluginConsentService was first called. keyringFn is a
// closure for signature symmetry with pluginsFn, but it is NOT similarly
// live: it is wired by the caller to return a keyring resolved once, up
// front (see BuildServeService), so it reflects the trust set this
// deployment started with (nil when this deployment does not require
// signatures, see resolvePluginKeyring), not a per-call re-resolution of
// cfg.Plugins.Keyring.
//
// remote is the resolved remote-source policy (config.PluginsConfig's Cache,
// HTTP client and fetch/unpack limits, see resolvePluginRemote) this
// process's serve assembly built its loader with. List uses it only to
// resolve a REMOTE entry's package directory on a cache hit -- it never
// drives a network fetch, so its zero value (no "plugins.cache" configured)
// is a legitimate, supported input, not a caller error.
func NewPluginConsentService(manifestPath, root string, pluginsFn func() *loader.Loader, keyringFn func() *sign.Keyring, remote loader.RemoteConfig) *PluginConsentService {
	return &PluginConsentService{
		manifestPath: manifestPath,
		root:         root,
		pluginsFn:    pluginsFn,
		keyringFn:    keyringFn,
		remote:       remote,
	}
}

// List reads the deployment manifest and this process's loader status, and
// returns one server.PluginView per row mergePluginStatus reports.
//
// For a row backed by a manifest entry (consent.FindEntry finds it),
// GrantedCaps/Hosts/Paths come straight from that entry's own Grant -- never
// recomputed -- and DeclaredCaps/Hosts/Paths come from loading the plugin's
// own plugin.json (manifest.LoadPackage) via resolveDeclaredPackageDir, the
// same package-directory resolution `agent plugins grant` uses
// (resolvePluginPackageDir). A row with no backing entry -- mergePluginStatus's
// "no longer in the manifest" case, a plugin the loader still has mounted
// but the file no longer declares -- carries neither: there is nothing left
// on disk to declare or grant against, and the row's own Detail already says
// so.
//
// A REMOTE entry only ever resolves its Declared fields on a CACHE HIT: a GET
// must not carry a network fetch as a side effect, so a genuine cache miss
// (or no "plugins.cache" configured at all) is never fetched here. That is
// reported as view.DeclaredUnresolved = true with Declared* left empty -- a
// distinct state from "declares nothing", which a plugin that IS resolved but
// genuinely declares zero capabilities/hosts/paths would report instead. Its
// State, Detail and Granted fields are unaffected either way and always
// reported in full.
//
// An entry (local, or remote past a resolvable cache hit) whose package
// cannot be resolved or loaded fails List outright (wrapped with the entry's
// name): a resolvable package is supposed to be readable, so a failure here
// is a real problem the caller needs to see, not a gap to paper over with an
// empty declaration.
//
// ctx is accepted for symmetry with server.PluginConsent and future
// cancellation; every operation List performs today is local disk I/O (a
// cache lookup, never a fetch).
func (s *PluginConsentService) List(ctx context.Context) ([]server.PluginView, error) {
	pluginLoader := s.pluginsFn()
	if pluginLoader == nil {
		return nil, fmt.Errorf("plugin consent: no plugin loader attached to this process; " +
			"plugins are assembled by `agent serve`, and a loader belongs to the service that built it")
	}
	deployment, _, err := consent.ReadDeploymentWithSnapshot(s.manifestPath)
	if err != nil {
		return nil, err
	}
	keyring := s.keyringFn()

	rows := mergePluginStatus(deployment, pluginLoader.Status())
	views := make([]server.PluginView, 0, len(rows))
	for _, row := range rows {
		view := server.PluginView{
			Name:    row.Name,
			Version: row.Version,
			State:   row.State,
			Detail:  row.Detail,
			Tools:   row.Tools,
		}
		entry, entryErr := consent.FindEntry(deployment, row.Name)
		if entryErr != nil {
			// No manifest entry backs this row -- see mergePluginStatus's own
			// doc comment for why that is a real, deliberate state ("no
			// longer in the manifest") rather than a lookup failure. There is
			// nothing to declare or grant against, so both stay empty.
			views = append(views, view)
			continue
		}
		view.GrantedCaps = entry.Grant.Capabilities
		view.GrantedHosts = entry.Grant.AllowedHosts
		view.GrantedPaths = entry.Grant.AllowedPaths

		dir, resolved, resolveErr := s.resolveDeclaredPackageDir(ctx, entry)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if !resolved {
			view.DeclaredUnresolved = true
			views = append(views, view)
			continue
		}
		pm, _, loadErr := manifest.LoadPackage(dir, keyring)
		if loadErr != nil {
			return nil, fmt.Errorf("plugin consent: load declared manifest for %q: %w", entry.Name, loadErr)
		}
		view.DeclaredCaps = pm.Capabilities
		view.DeclaredHosts = pm.Network.AllowedHosts
		view.DeclaredPaths = pm.Filesystem.AllowedPaths
		views = append(views, view)
	}
	return views, nil
}

// resolveDeclaredPackageDir resolves entry's on-disk package directory for
// reading its DECLARED capabilities, without ever making a network call:
// List backs a GET request, and a GET must not carry a network fetch as a
// side effect.
//
// A local entry is always resolved -- via resolvePluginPackageDir (the same
// resolution `agent plugins grant` uses, which for a local source is exactly
// localPluginPackageDir) -- so resolved=false is never returned for a local
// entry; a missing or broken local package is a real problem the caller
// needs to see, reported as a non-nil error instead.
//
// A remote entry is resolved ONLY on a cache hit. Cache.Has is checked here,
// BEFORE resolvePluginPackageDir is ever called for a remote entry, so that
// function's own fetch-on-miss branch is provably unreachable from this
// path. A remote entry with no configured cache (s.remote.Cache == nil), or
// whose digest Cache.Has reports absent, returns resolved=false and a nil
// error: the truth is "we do not know what this plugin declares", not "it
// declares nothing" -- see List's own doc comment and
// server.PluginView.DeclaredUnresolved.
func (s *PluginConsentService) resolveDeclaredPackageDir(ctx context.Context, entry manifest.Entry) (dir string, resolved bool, err error) {
	if !entry.IsRemote() {
		dir, err = resolvePluginPackageDir(ctx, entry, s.remote, s.root)
		if err != nil {
			return "", false, fmt.Errorf("plugin consent: resolve package directory for %q: %w", entry.Name, err)
		}
		return dir, true, nil
	}
	if s.remote.Cache == nil {
		return "", false, nil
	}
	hit, err := s.remote.Cache.Has(entry.Digest)
	if err != nil {
		return "", false, fmt.Errorf("plugin consent: check plugin cache for %q: %w", entry.Name, err)
	}
	if !hit {
		return "", false, nil
	}
	dir, err = resolvePluginPackageDir(ctx, entry, s.remote, s.root)
	if err != nil {
		return "", false, fmt.Errorf("plugin consent: resolve package directory for %q: %w", entry.Name, err)
	}
	return dir, true, nil
}

// pluginConsentActor labels every consent.* call Grant makes, so its error
// messages point an operator at this endpoint rather than at
// `agent plugins grant`.
const pluginConsentGrantActor = "POST /v1/plugins/{name}/grant"

// pluginConsentDenyActor is pluginConsentGrantActor's counterpart for Deny.
const pluginConsentDenyActor = "POST /v1/plugins/{name}/deny"

// Grant implements server.PluginConsent: it authorizes the deployment entry
// named name to run with req's capabilities/allowed hosts/allowed paths,
// following the same sequence `agent plugins grant` (runPluginsGrant) does,
// step for step:
//
//  1. consent.ReadDeploymentWithSnapshot reads the target state and the
//     exact bytes it came from.
//  2. consent.FindEntry locates the entry -- an unknown name is refused
//     here, before anything is loaded or fetched.
//  3. resolvePluginPackageDir resolves the entry's package directory,
//     fetching a remote one on a cache miss (unlike List's
//     resolveDeclaredPackageDir, which a GET must never do -- see that
//     function's own doc comment), and manifest.LoadPackage reads the
//     plugin's OWN declaration. Because this either resolves a real
//     directory or fails outright, there is no "we could not tell what it
//     declares, so let the check pass" state for Grant to fall into: a
//     resolution failure is reported as an ordinary write-failed error,
//     with nothing written.
//  4. consent.ResolveCapabilities / ResolveAllowedHosts / ResolveAllowedPaths
//     / RefuseUnnamedAllowlist validate req against that declaration --
//     entirely through consent.*, so this endpoint can never enforce a rule
//     `agent plugins grant` does not also enforce.
//  5. consent.RefuseDeploymentChanged re-checks the snapshot from step 1,
//     catching an edit that landed in the window steps 3-4 opened.
//  6. manifest.UpdateEntry + manifest.WriteDeployment write the grant to
//     disk. Every error returned above this point leaves the manifest
//     untouched; every path below it has already written.
//  7. applyAndReport converges the loader and turns the outcome into the
//     ConsentResult the four-outcome contract on server.ConsentResult
//     describes.
func (s *PluginConsentService) Grant(ctx context.Context, name string, req server.GrantRequest) (server.ConsentResult, error) {
	pluginLoader := s.pluginsFn()
	if pluginLoader == nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: no plugin loader attached to this process; " +
			"plugins are assembled by `agent serve`, and a loader belongs to the service that built it")
	}

	dep, snapshot, err := consent.ReadDeploymentWithSnapshot(s.manifestPath)
	if err != nil {
		return server.ConsentResult{}, err
	}
	entry, err := consent.FindEntry(dep, name)
	if err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: grant %q: %w", name, err)
	}

	dir, err := resolvePluginPackageDir(ctx, entry, s.remote, s.root)
	if err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: grant %q: %w", name, err)
	}
	pm, _, err := manifest.LoadPackage(dir, s.keyringFn())
	if err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: grant %q: %w", name, err)
	}

	capabilities, err := consent.ResolveCapabilities(pluginConsentGrantActor, req.Capabilities, pm)
	if err != nil {
		return server.ConsentResult{}, err
	}
	hosts, err := consent.ResolveAllowedHosts(pluginConsentGrantActor, req.AllowedHosts, pm)
	if err != nil {
		return server.ConsentResult{}, err
	}
	paths, err := consent.ResolveAllowedPaths(pluginConsentGrantActor, req.AllowedPaths, pm)
	if err != nil {
		return server.ConsentResult{}, err
	}
	if err := consent.RefuseUnnamedAllowlist(pluginConsentGrantActor, "capabilities", capabilities, pm, hosts, paths,
		`name at least one of the declared hosts in "allowed_hosts" to authorize it with the hosts named too`,
		`name at least one of the declared paths in "allowed_paths" to authorize it with the paths named too`,
	); err != nil {
		return server.ConsentResult{}, err
	}

	if err := consent.RefuseDeploymentChanged(pluginConsentGrantActor, s.manifestPath, snapshot); err != nil {
		return server.ConsentResult{}, err
	}

	updated, err := manifest.UpdateEntry(dep, name, func(e manifest.Entry) (manifest.Entry, error) {
		e.Enabled = true
		e.GrantStated = true
		e.Grant = manifest.GrantDecl{Capabilities: capabilities, AllowedHosts: hosts, AllowedPaths: paths}
		return e, nil
	})
	if err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: grant %q: %w", name, err)
	}
	if err := manifest.WriteDeployment(s.manifestPath, updated); err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: grant %q: %w", name, err)
	}

	result, err := s.applyAndReport(ctx, pluginLoader, updated, name)
	if err != nil {
		return server.ConsentResult{}, err
	}
	// Grant, unlike Deny, always has pm in hand by this point -- LoadPackage
	// above already succeeded, so the entry's declaration is genuinely
	// resolved (never a cache-miss "we don't know" state; see step 3 in
	// this method's own doc comment) and DeclaredUnresolved stays false.
	result.View.DeclaredCaps = pm.Capabilities
	result.View.DeclaredHosts = pm.Network.AllowedHosts
	result.View.DeclaredPaths = pm.Filesystem.AllowedPaths
	return result, nil
}

// Deny implements server.PluginConsent: it revokes the deployment entry
// named name's authorization to run -- flips Enabled false, clears Grant,
// but keeps GrantStated true (a decision WAS made, see
// manifest.Entry.GrantStated) and leaves Source, Digest and Tools untouched
// -- and converges, following the same shape `agent plugins deny`
// (runPluginsDeny) does.
//
// Deny deliberately skips Grant's steps 3-4 (LoadPackage and the
// capability/allowlist validation): it never loads the plugin's own
// package, and never touches a running loader beyond the convergence at the
// end. Revoking a plugin must not depend on that plugin's package still
// being loadable -- a broken, tampered or now-unreachable package is
// exactly the kind of entry an operator most needs to be able to deny, and
// making that depend on a clean LoadPackage would defeat the point.
//
// Because Deny never resolves the plugin's declaration, it has no pm to
// report Declared* fields from. Reporting them as empty-but-resolved would
// misrepresent "Deny didn't look" as "this plugin declares nothing" (see
// server.PluginView.DeclaredUnresolved's own doc comment for why those two
// states must stay distinguishable), so the returned View is marked
// DeclaredUnresolved unconditionally instead.
func (s *PluginConsentService) Deny(ctx context.Context, name string) (server.ConsentResult, error) {
	pluginLoader := s.pluginsFn()
	if pluginLoader == nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: no plugin loader attached to this process; " +
			"plugins are assembled by `agent serve`, and a loader belongs to the service that built it")
	}

	dep, snapshot, err := consent.ReadDeploymentWithSnapshot(s.manifestPath)
	if err != nil {
		return server.ConsentResult{}, err
	}
	if _, err := consent.FindEntry(dep, name); err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: deny %q: %w", name, err)
	}
	if err := consent.RefuseDeploymentChanged(pluginConsentDenyActor, s.manifestPath, snapshot); err != nil {
		return server.ConsentResult{}, err
	}

	updated, err := manifest.UpdateEntry(dep, name, func(e manifest.Entry) (manifest.Entry, error) {
		e.Enabled = false
		e.GrantStated = true
		e.Grant = manifest.GrantDecl{}
		// Source, Digest and Tools are left exactly as they were: deny
		// revokes authorization, it does not throw away the registration.
		return e, nil
	})
	if err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: deny %q: %w", name, err)
	}
	if err := manifest.WriteDeployment(s.manifestPath, updated); err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: deny %q: %w", name, err)
	}

	result, err := s.applyAndReport(ctx, pluginLoader, updated, name)
	if err != nil {
		return server.ConsentResult{}, err
	}
	result.View.DeclaredUnresolved = true
	return result, nil
}

// applyAndReport converges pluginLoader toward dep (which the caller has
// already written to disk) and turns the outcome into a ConsentResult per
// the four-outcome contract server.ConsentResult documents. It is called
// only AFTER Grant/Deny's own write has already succeeded, so every error
// it can still report is the "entry vanished from its own deployment"
// invariant violation at the bottom -- a genuine write failure is reported
// by the caller, earlier, before applyAndReport is ever invoked.
//
// ld.Apply's error is deliberately NOT surfaced as-is: the disk write
// already landed, so — per taskgate.ApplyAtBoundary's own doc comment —
// this is never "nothing happened". It is either "not applied yet"
// (Apply's error wraps context.DeadlineExceeded/context.Canceled from a
// boundary wait that timed out or was cancelled, or taskgate.ErrApplyPending
// / taskgate.ErrApplyInProgress from a concurrent apply already holding the
// gate) or "applied, and THIS entry's own state in the loader's Status() is
// the honest answer" — which covers both a clean convergence and this
// entry specifically failing to activate, and also covers an unrelated
// entry failing while this one converged fine: Apply's error can name
// either, and only Status() says which one this entry got.
func (s *PluginConsentService) applyAndReport(ctx context.Context, pluginLoader *loader.Loader, dep manifest.Deployment, name string) (server.ConsentResult, error) {
	applyErr := pluginLoader.Apply(ctx, dep, s.root)
	if applyErr != nil && (errors.Is(applyErr, context.DeadlineExceeded) ||
		errors.Is(applyErr, context.Canceled) ||
		errors.Is(applyErr, taskgate.ErrApplyPending) ||
		errors.Is(applyErr, taskgate.ErrApplyInProgress)) {
		return server.ConsentResult{PendingConvergence: true, ConvergenceDetail: applyErr.Error()}, nil
	}

	entry, err := consent.FindEntry(dep, name)
	if err != nil {
		return server.ConsentResult{}, fmt.Errorf(
			"plugin consent: entry %q vanished from its own deployment between write and status read: %w", name, err)
	}
	view := server.PluginView{
		Name:         name,
		GrantedCaps:  entry.Grant.Capabilities,
		GrantedHosts: entry.Grant.AllowedHosts,
		GrantedPaths: entry.Grant.AllowedPaths,
	}
	for _, row := range mergePluginStatus(dep, pluginLoader.Status()) {
		if row.Name != name {
			continue
		}
		view.Version, view.State, view.Detail, view.Tools = row.Version, row.State, row.Detail, row.Tools
		return server.ConsentResult{View: view}, nil
	}
	// mergePluginStatus emits exactly one row per dep.Plugins entry (see its
	// own doc comment), and consent.FindEntry just above proved name is one
	// of them -- reaching here means that invariant broke.
	return server.ConsentResult{}, fmt.Errorf(
		"plugin consent: entry %q produced no status row after converging; this should be unreachable", name)
}
