package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/stardust/legion-agent/internal/plugin/consent"
	"github.com/stardust/legion-agent/internal/plugin/fetch"
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
	logger       *slog.Logger

	// mu serializes Grant and Deny against each other IN THIS PROCESS, held
	// for the whole read -> validate -> write -> converge sequence.
	//
	// consent.RefuseDeploymentChanged is a compare-and-swap with no swap:
	// it re-reads the manifest and compares it against the snapshot, but
	// nothing stops another goroutine writing in the gap between that check
	// and manifest.WriteDeployment. Two GUI windows authorizing two
	// different plugins at once would both pass the check, and the second
	// write would revert the first plugin's authorization while both
	// requests returned 200 -- a silent rollback on an authorization
	// boundary, which is worse than a crash. That window was tolerable for a
	// hand-speed CLI; on HTTP, concurrency is the normal case.
	//
	// Cross-process safety still rests entirely on RefuseDeploymentChanged
	// (another `agent plugins grant` shares no mutex with this one), so this
	// removes the half that is removable rather than replacing the guard.
	// The lock is held across resolvePluginPackageDir, which may download a
	// remote package: authorization is a low-frequency operator action, and
	// releasing the lock for the download is exactly what would put the gap
	// back. List deliberately does NOT take it -- it only reads, and a
	// reader blocked behind a package download would be a worse trade.
	mu sync.Mutex
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
//
// logger is where a convergence that ran and reported errors is recorded
// (see applyAndReport). It must not be nil: the loader logs a per-entry
// activation failure itself, but Apply has failure paths that never reach
// the loader's own logging, and dropping those would make a grant that
// changed nothing indistinguishable from one that worked. A nil logger is a
// wiring mistake at the call site, not a state to tolerate, so it panics
// rather than silently discarding those records.
func NewPluginConsentService(manifestPath, root string, pluginsFn func() *loader.Loader, keyringFn func() *sign.Keyring, remote loader.RemoteConfig, logger *slog.Logger) *PluginConsentService {
	if logger == nil {
		panic("cli: NewPluginConsentService: logger is nil; a convergence that reported errors would be recorded nowhere")
	}
	return &PluginConsentService{
		manifestPath: manifestPath,
		root:         root,
		pluginsFn:    pluginsFn,
		keyringFn:    keyringFn,
		remote:       remote,
		logger:       logger,
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
// Every DeclaredUnresolved row also carries a
// server.PluginView.DeclaredUnresolvedReason, because a cache MISS (which
// Resolve can fetch) and a deployment with no "plugins.cache" at all (which
// it can never fetch, and which needs a config edit and a restart instead)
// are otherwise the same JSON. See that field's own doc comment.
//
// An entry (local, or remote past a resolvable cache hit) whose package
// cannot be resolved or loaded is reported as a PER-ROW failure -- view.
// DeclaredUnresolved = true with view.DeclaredError naming what went wrong
// (a corrupted plugin.wasm, a package directory renamed or removed from
// disk, ...) -- rather than failing List as a whole. Deny's own doc comment
// says revoking a plugin must not depend on its package still being
// loadable, because a broken or now-unreachable package is exactly the kind
// of entry an operator most needs to be able to deny; List failing outright
// on that same entry would make the row carrying its deny button
// unreachable, defeating that guarantee two files away. Every OTHER row
// still renders normally, and the entry's own State/Detail (from
// mergePluginStatus, set above regardless of how this loop ends) is
// unaffected. Only a failure that breaks every row alike -- reading or
// parsing plugins.json itself -- fails List outright; see
// TestPluginConsentServiceListErrorsWhenManifestUnreadable.
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
		view.GrantedExtensions = entry.Grant.Extensions

		dir, resolved, reason, resolveErr := s.resolveDeclaredPackageDir(ctx, entry)
		if resolveErr != nil {
			// A broken or unreachable package must not take the whole list
			// down with it -- see this method's own doc comment and
			// server.PluginView.DeclaredError. Every other row still renders;
			// this row still carries its State/Detail/Granted* fields, just
			// no Declared* ones.
			view.DeclaredUnresolved = true
			view.DeclaredUnresolvedReason = server.DeclaredUnresolvedLoadFailed
			view.DeclaredError = resolveErr.Error()
			views = append(views, view)
			continue
		}
		if !resolved {
			view.DeclaredUnresolved = true
			view.DeclaredUnresolvedReason = reason
			// 缓存里没有，未必等于「取一下就有」。加载器可能刚刚就取过、并且拒了
			// ——签名不被信任、摘要对不上、归档缺文件。那种包永远进不了缓存，于是
			// 「缓存里有没有」这个问法每次都答 not_cached，而 not_cached 在契约里
			// 的意思是「包取得到、什么都没出错、取一下就好」，GUI 的插件面板正是
			// 只在这个原因上给「获取」按钮。运维于是拿到一个按下去重新下载、再次
			// 被拒、永远如此的按钮——那正是那个面板要避免的谎。
			//
			// 加载器对这一条的失败说明就是「取一次能不能解决」的答案：它有说明，
			// 就说明它已经试过了。V1 真机验证抓到的就是这个。
			if reason == server.DeclaredUnresolvedNotCached && row.State == loader.StateFailed && row.Detail != "" {
				view.DeclaredUnresolvedReason = server.DeclaredUnresolvedLoadFailed
				view.DeclaredError = row.Detail
			}
			views = append(views, view)
			continue
		}
		pm, _, loadErr := manifest.LoadPackage(dir, keyring)
		if loadErr != nil {
			view.DeclaredUnresolved = true
			view.DeclaredUnresolvedReason = server.DeclaredUnresolvedLoadFailed
			view.DeclaredError = fmt.Errorf("plugin consent: load declared manifest for %q: %w", entry.Name, loadErr).Error()
			views = append(views, view)
			continue
		}
		view.DeclaredCaps = pm.Capabilities
		view.DeclaredHosts = pm.Network.AllowedHosts
		view.DeclaredPaths = pm.Filesystem.AllowedPaths
		view.DeclaredExtensions = pm.Extensions
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
// function's own fetch-on-miss branch is unreachable from this path AS LONG
// AS nothing can invalidate a Cache.Has hit before resolvePluginPackageDir's
// own re-check runs -- true today because fetch.Cache exposes no
// Delete/Evict/Prune anywhere in this repo. A future cache-GC that adds an
// eviction API would reopen that window; this comment describes the current
// guarantee, not a permanent one. A remote entry with no configured cache
// (s.remote.Cache == nil), or whose digest Cache.Has reports absent, returns
// resolved=false and a nil error: the truth is "we do not know what this
// plugin declares", not "it declares nothing" -- see List's own doc comment
// and server.PluginView.DeclaredUnresolved.
//
// Those two not-resolved outcomes are told apart by reason, which is one of
// server.DeclaredUnresolvedNoCache (nothing to fetch INTO -- a fetch can
// never succeed until the deployment config gains a "plugins.cache") or
// server.DeclaredUnresolvedNotCached (obtainable, just not obtained yet --
// the one case PluginConsent.Resolve can remedy). reason is empty whenever
// resolved is true or err is non-nil; the caller supplies
// server.DeclaredUnresolvedLoadFailed for the error case, since a resolution
// failure and a plugin.json load failure are the same class to a consumer.
func (s *PluginConsentService) resolveDeclaredPackageDir(ctx context.Context, entry manifest.Entry) (dir string, resolved bool, reason string, err error) {
	if !entry.IsRemote() {
		dir, err = resolvePluginPackageDir(ctx, entry, s.remote, s.root)
		if err != nil {
			return "", false, "", fmt.Errorf("plugin consent: resolve package directory for %q: %w", entry.Name, err)
		}
		return dir, true, "", nil
	}
	if s.remote.Cache == nil {
		return "", false, server.DeclaredUnresolvedNoCache, nil
	}
	hit, err := s.remote.Cache.Has(entry.Digest)
	if err != nil {
		return "", false, "", fmt.Errorf("plugin consent: check plugin cache for %q: %w", entry.Name, err)
	}
	if !hit {
		return "", false, server.DeclaredUnresolvedNotCached, nil
	}
	dir, err = resolvePluginPackageDir(ctx, entry, s.remote, s.root)
	if err != nil {
		return "", false, "", fmt.Errorf("plugin consent: resolve package directory for %q: %w", entry.Name, err)
	}
	return dir, true, "", nil
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
//
// Step 4 is preceded by consent.NormalizeList on each of the three lists,
// the same refusal of an empty or repeated item `agent plugins grant` gets
// from splitFlagList. Both paths call the SAME function for it, because a
// duplicate slips through every later set check unnoticed and would be
// written into plugins.json by whichever path skipped the rule.
//
// The whole sequence runs under s.mu, so a second concurrent Grant or Deny
// in this process cannot land its write inside step 5's compare-and-swap
// window -- see s.mu's own comment. Every error is classified with one of
// internal/server's sentinels where a class applies, so the handler can map
// it to a status code without reading its text.
func (s *PluginConsentService) Grant(ctx context.Context, name string, req server.GrantRequest) (server.ConsentResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pluginLoader := s.pluginsFn()
	if pluginLoader == nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: grant %q: %w; "+
			"plugins are assembled by `agent serve`, and a loader belongs to the service that built it",
			name, server.ErrPluginUnavailable)
	}

	dep, snapshot, err := consent.ReadDeploymentWithSnapshot(s.manifestPath)
	if err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: grant %q: %w: %w", name, server.ErrPluginStorage, err)
	}
	entry, err := consent.FindEntry(dep, name)
	if err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: grant %q: %w: %w", name, server.ErrPluginNotFound, err)
	}

	dir, err := resolvePluginPackageDir(ctx, entry, s.remote, s.root)
	if err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: grant %q: %w", name, err)
	}
	pm, _, err := manifest.LoadPackage(dir, s.keyringFn())
	if err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: grant %q: %w", name, err)
	}

	requestedCaps, err := consent.NormalizeList(pluginConsentGrantActor, "capabilities", req.Capabilities)
	if err != nil {
		return server.ConsentResult{}, err
	}
	requestedHosts, err := consent.NormalizeList(pluginConsentGrantActor, "allowed_hosts", req.AllowedHosts)
	if err != nil {
		return server.ConsentResult{}, err
	}
	requestedPaths, err := consent.NormalizeList(pluginConsentGrantActor, "allowed_paths", req.AllowedPaths)
	if err != nil {
		return server.ConsentResult{}, err
	}
	requestedExtensions, err := consent.NormalizeList(pluginConsentGrantActor, "extensions", req.Extensions)
	if err != nil {
		return server.ConsentResult{}, err
	}

	capabilities, err := consent.ResolveCapabilities(pluginConsentGrantActor, requestedCaps, pm)
	if err != nil {
		return server.ConsentResult{}, err
	}
	hosts, err := consent.ResolveAllowedHosts(pluginConsentGrantActor, requestedHosts, pm)
	if err != nil {
		return server.ConsentResult{}, err
	}
	paths, err := consent.ResolveAllowedPaths(pluginConsentGrantActor, requestedPaths, pm)
	if err != nil {
		return server.ConsentResult{}, err
	}
	extensions, err := consent.ResolveExtensions(pluginConsentGrantActor, requestedExtensions, pm)
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
		return server.ConsentResult{}, classifyDeploymentGuardError("grant", name, err)
	}

	updated, err := manifest.UpdateEntry(dep, name, func(e manifest.Entry) (manifest.Entry, error) {
		e.Enabled = true
		e.GrantStated = true
		e.Grant = manifest.GrantDecl{
			Capabilities: capabilities,
			AllowedHosts: hosts,
			AllowedPaths: paths,
			Extensions:   extensions,
		}
		return e, nil
	})
	if err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: grant %q: %w", name, err)
	}
	if err := manifest.WriteDeployment(s.manifestPath, updated); err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: grant %q: %w: %w", name, server.ErrPluginStorage, err)
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
	result.View.DeclaredExtensions = pm.Extensions
	return result, nil
}

// Resolve fetches and verifies the package behind one deployment entry so the
// caller can SEE what the plugin declares, and stops there: it never touches
// plugins.json, never writes a grant, never converges. It exists because a
// remote entry whose package is not cached reports DeclaredUnresolved from
// List — GET must not carry a network fetch as a side effect — which leaves an
// operator unable to review what they would be authorizing. This is the
// deliberate, operator-initiated fetch that closes that gap.
//
// It runs the SAME chain Grant does (read the deployment, find the entry,
// resolvePluginPackageDir, manifest.LoadPackage) and reuses every precondition
// that chain enforces: a plaintext http source is still refused unless the
// deployment opted in, and a missing plugin cache is still refused. Resolving
// through a second, laxer path would be a way around those checks.
//
// The package is left in the cache on success. That is intended — it is what
// spares a following Grant a second download, and it matches what the CLI's
// own grant already does — and it happens even if the operator then decides
// NOT to authorize. Callers are expected to say so; the settings panel does.
//
// An untrusted package (see manifest.ErrUntrustedPackage) is reported with
// that sentinel on the error chain, alongside server.ErrPluginUntrusted --
// the sentinel the HTTP layer's pluginConsentStatus keys its 422 response on
// -- so a caller can tell it apart from a package it merely could not obtain
// and refrain from offering a retry that could never succeed.
func (s *PluginConsentService) Resolve(ctx context.Context, name string) (server.PluginView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dep, _, err := consent.ReadDeploymentWithSnapshot(s.manifestPath)
	if err != nil {
		return server.PluginView{}, fmt.Errorf("plugin consent: resolve %q: %w: %w", name, server.ErrPluginStorage, err)
	}
	entry, err := consent.FindEntry(dep, name)
	if err != nil {
		return server.PluginView{}, fmt.Errorf("plugin consent: resolve %q: %w: %w", name, server.ErrPluginNotFound, err)
	}

	dir, err := resolvePluginPackageDir(ctx, entry, s.remote, s.root)
	if err != nil {
		return server.PluginView{}, fmt.Errorf("plugin consent: resolve %q: %w", name, err)
	}
	pm, _, err := manifest.LoadPackage(dir, s.keyringFn())
	if err != nil {
		if errors.Is(err, manifest.ErrUntrustedPackage) {
			// The bytes just failed signature verification, so they do not
			// belong in a directory this deployment reads from. Only a REMOTE
			// entry's package lives in the cache — a local entry's directory
			// is the operator's own tree — and only a trust failure earns
			// eviction: a package that merely will not load is re-downloaded
			// identically next time, so removing it buys nothing.
			if entry.IsRemote() {
				fetch.EvictUntrusted(s.remote.Cache, entry.Digest, s.logger)
			}
			return server.PluginView{}, fmt.Errorf("plugin consent: resolve %q: %w: %w", name, server.ErrPluginUntrusted, err)
		}
		return server.PluginView{}, fmt.Errorf("plugin consent: resolve %q: %w", name, err)
	}

	// Only Declared*/Granted*/Name are filled: Resolve never touches the
	// loader (no Apply, no Status() merge, unlike Grant/Deny), so it has no
	// honest State/Detail/Tools to report -- those stay at their zero value
	// rather than being guessed at.
	return server.PluginView{
		Name:               name,
		GrantedCaps:        entry.Grant.Capabilities,
		GrantedHosts:       entry.Grant.AllowedHosts,
		GrantedPaths:       entry.Grant.AllowedPaths,
		GrantedExtensions:  entry.Grant.Extensions,
		DeclaredCaps:       pm.Capabilities,
		DeclaredHosts:      pm.Network.AllowedHosts,
		DeclaredPaths:      pm.Filesystem.AllowedPaths,
		DeclaredExtensions: pm.Extensions,
	}, nil
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
// DeclaredUnresolved unconditionally instead, with
// server.DeclaredUnresolvedNotInspected as its reason: nothing failed here,
// nobody looked.
//
// Like Grant, the whole read -> write -> converge sequence runs under s.mu
// (see its own comment) and every error is classified with one of
// internal/server's sentinels where a class applies.
func (s *PluginConsentService) Deny(ctx context.Context, name string) (server.ConsentResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pluginLoader := s.pluginsFn()
	if pluginLoader == nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: deny %q: %w; "+
			"plugins are assembled by `agent serve`, and a loader belongs to the service that built it",
			name, server.ErrPluginUnavailable)
	}

	dep, snapshot, err := consent.ReadDeploymentWithSnapshot(s.manifestPath)
	if err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: deny %q: %w: %w", name, server.ErrPluginStorage, err)
	}
	if _, err := consent.FindEntry(dep, name); err != nil {
		return server.ConsentResult{}, fmt.Errorf("plugin consent: deny %q: %w: %w", name, server.ErrPluginNotFound, err)
	}
	if err := consent.RefuseDeploymentChanged(pluginConsentDenyActor, s.manifestPath, snapshot); err != nil {
		return server.ConsentResult{}, classifyDeploymentGuardError("deny", name, err)
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
		return server.ConsentResult{}, fmt.Errorf("plugin consent: deny %q: %w: %w", name, server.ErrPluginStorage, err)
	}

	result, err := s.applyAndReport(ctx, pluginLoader, updated, name)
	if err != nil {
		return server.ConsentResult{}, err
	}
	result.View.DeclaredUnresolved = true
	result.View.DeclaredUnresolvedReason = server.DeclaredUnresolvedNotInspected
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
// (taskgate.ErrBoundaryNotReached from a boundary wait that timed out or was
// cancelled, or taskgate.ErrApplyInProgress from a concurrent apply already
// holding the gate) or "applied, and THIS entry's own state in the loader's
// Status() is the honest answer" — which covers both a clean convergence and
// this entry specifically failing to activate, and also covers an unrelated
// entry failing while this one converged fine: Apply's error can name
// either, and only Status() says which one this entry got.
//
// Those TWO taskgate sentinels are the whole test, and the bare ctx
// sentinels are deliberately not consulted. gpc-task-3 review, Critical-1:
// Apply passes the same ctx down through converge -> prepare -> remoteDir ->
// fetch.Fetch, which derives its own deadline from it and wraps
// context.DeadlineExceeded when a download times out. Keying on
// context.DeadlineExceeded/context.Canceled therefore read a convergence
// that RAN AND FAILED as one still to come — an operator waiting forever for
// something that already happened — and, in the other direction, reported
// "your authorization has not taken effect yet" about a plugin that was
// already running with its new capabilities. Only taskgate knows first hand
// whether fn ran; ErrBoundaryNotReached is it saying so.
//
// A non-nil applyErr on the CONVERGED branch is kept, not dropped: it goes
// into ConvergenceDetail (see server.ConsentResult — "convergence ran, and
// these errors happened" is one of its documented states) and is logged.
// Most of what it names the loader has already logged per entry, but
// loader.Apply has two failure paths that return BEFORE the gate and never
// reach that logging; dropping the error would turn those into "reported
// success, applied nothing", which is the same class of lie as Critical-1,
// only silent.
func (s *PluginConsentService) applyAndReport(ctx context.Context, pluginLoader *loader.Loader, dep manifest.Deployment, name string) (server.ConsentResult, error) {
	applyErr := pluginLoader.Apply(ctx, dep, s.root)

	entry, err := consent.FindEntry(dep, name)
	if err != nil {
		return server.ConsentResult{}, fmt.Errorf(
			"plugin consent: entry %q vanished from its own deployment between write and status read: %w", name, err)
	}
	// Name and the Granted* fields are known on BOTH branches -- they are
	// what was just written to disk, independently of whether anything
	// converged -- so the pending branch reports them too. A pending
	// response with no name is one the GUI cannot match back to the row it
	// came from.
	view := server.PluginView{
		Name:              name,
		GrantedCaps:       entry.Grant.Capabilities,
		GrantedHosts:      entry.Grant.AllowedHosts,
		GrantedPaths:      entry.Grant.AllowedPaths,
		GrantedExtensions: entry.Grant.Extensions,
	}

	if applyErr != nil && (errors.Is(applyErr, taskgate.ErrBoundaryNotReached) ||
		errors.Is(applyErr, taskgate.ErrApplyInProgress)) {
		s.logger.Warn("plugin consent convergence did not run",
			"component", "cli",
			"plugin", name,
			"manifest", s.manifestPath,
			"consequence", "the deployment manifest already carries this decision, but no plugin was mounted or unmounted for it",
			"remedy", "retry once the task gate is idle, or run `agent plugins reload`",
			"error", applyErr)
		return server.ConsentResult{View: view, PendingConvergence: true, ConvergenceDetail: applyErr.Error()}, nil
	}

	convergenceDetail := ""
	if applyErr != nil {
		convergenceDetail = applyErr.Error()
		s.logger.Warn("plugin consent convergence reported errors",
			"component", "cli",
			"plugin", name,
			"manifest", s.manifestPath,
			"consequence", "the decision is on disk and convergence ran; this entry's own reported state says whether IT came up",
			"error", applyErr)
	}
	for _, row := range mergePluginStatus(dep, pluginLoader.Status()) {
		if row.Name != name {
			continue
		}
		view.Version, view.State, view.Detail, view.Tools = row.Version, row.State, row.Detail, row.Tools
		return server.ConsentResult{View: view, ConvergenceDetail: convergenceDetail}, nil
	}
	// mergePluginStatus emits exactly one row per dep.Plugins entry (see its
	// own doc comment), and consent.FindEntry just above proved name is one
	// of them -- reaching here means that invariant broke.
	return server.ConsentResult{}, fmt.Errorf(
		"plugin consent: entry %q produced no status row after converging; this should be unreachable", name)
}

// classifyDeploymentGuardError turns consent.RefuseDeploymentChanged's error
// into one carrying the server sentinel that names its class: a detected
// concurrent edit is a conflict the caller can retry (409), while a failure
// to re-read the manifest at all is a server-side I/O fault (500). Both
// arrive from the same call, so without this they would share one status.
//
// action is "grant" or "deny", for the message only.
func classifyDeploymentGuardError(action, name string, err error) error {
	if errors.Is(err, consent.ErrDeploymentChanged) {
		return fmt.Errorf("plugin consent: %s %q: %w: %w", action, name, server.ErrPluginDeploymentChanged, err)
	}
	return fmt.Errorf("plugin consent: %s %q: %w: %w", action, name, server.ErrPluginStorage, err)
}
