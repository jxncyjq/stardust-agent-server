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
}

// NewPluginConsentService builds a PluginConsentService.
//
// manifestPath and root are the deployment manifest path and package root
// this process's serve assembly resolved (config.PluginsConfig's Manifest
// and Root). pluginsFn returns the loader currently attached to this
// process -- nil once drainPlugins has detached it (e.g. mid-shutdown), in
// which case List reports that rather than dereferencing a nil Loader.
// keyringFn returns the trust set the loader was itself built with (nil when
// this deployment does not require signatures, see resolvePluginKeyring).
//
// Both are closures rather than plain values so that a caller which
// re-assembles serve in the same process (drainPlugins, then a fresh
// assemblePlugins) is read through to the CURRENT loader and keyring, not
// the ones that existed when NewPluginConsentService was first called.
func NewPluginConsentService(manifestPath, root string, pluginsFn func() *loader.Loader, keyringFn func() *sign.Keyring) *PluginConsentService {
	return &PluginConsentService{
		manifestPath: manifestPath,
		root:         root,
		pluginsFn:    pluginsFn,
		keyringFn:    keyringFn,
	}
}

// List reads the deployment manifest and this process's loader status, and
// returns one server.PluginView per row mergePluginStatus reports.
//
// For a row backed by a manifest entry (consent.FindEntry finds it),
// GrantedCaps/Hosts/Paths come straight from that entry's own Grant -- never
// recomputed -- and, for a LOCAL entry, DeclaredCaps/Hosts/Paths come from
// loading the plugin's own plugin.json (manifest.LoadPackage) via the same
// package-directory resolution `agent plugins grant` uses for a local source
// (localPluginPackageDir). A row with no backing entry -- mergePluginStatus's
// "no longer in the manifest" case, a plugin the loader still has mounted
// but the file no longer declares -- carries neither: there is nothing left
// on disk to declare or grant against, and the row's own Detail already says
// so.
//
// A REMOTE entry's Declared fields are a documented scope limit, not a
// swallowed error: resolving one can require a network fetch on a cache
// miss (loader.RemoteConfig's cache and HTTP client), and this constructor
// is deliberately not handed one -- a GET must not carry a network fetch as
// a side effect. DeclaredCaps/Hosts/Paths stay empty for a remote entry; its
// State, Detail and Granted fields are unaffected and still reported in
// full.
//
// A LOCAL entry whose package cannot be resolved or loaded fails List
// outright (wrapped with the entry's name): unlike a remote entry, a local
// package is supposed to always be readable from the deployment root, so a
// failure here is a real problem the caller needs to see, not a gap to
// paper over with an empty declaration.
//
// ctx is accepted for symmetry with server.PluginConsent and future
// cancellation; every operation List performs today is local disk I/O.
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

		if entry.IsRemote() {
			views = append(views, view)
			continue
		}
		dir, dirErr := localPluginPackageDir(entry.Name, s.root, entry.Source)
		if dirErr != nil {
			return nil, fmt.Errorf("plugin consent: resolve package directory for %q: %w", entry.Name, dirErr)
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
