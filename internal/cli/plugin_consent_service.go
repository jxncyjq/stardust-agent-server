package cli

import (
	"context"
	"fmt"

	"github.com/stardust/legion-agent/internal/plugin/consent"
	"github.com/stardust/legion-agent/internal/plugin/loader"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/plugin/sign"
	"github.com/stardust/legion-agent/internal/server"
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
