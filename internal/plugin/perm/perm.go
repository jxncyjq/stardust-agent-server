// Package perm describes what a WASM plugin has been authorized to reach on
// the host side: which host capabilities it may import at all, and — for the
// two capabilities whose danger is in their arguments rather than in their
// existence — which hosts and which paths those arguments may name.
//
// The package is deliberately NOT called capability: internal/capability is
// already taken by the agent capability catalogue, and two packages with the
// same name would have to be imported under an alias everywhere they meet,
// which is exactly the kind of confusion a security-relevant type must not
// invite.
package perm

import "strings"

// Grant is one plugin's authorization: the set of host capabilities it may
// use, plus the argument allowlists for the two that need them.
//
// The capability flags are enforced at link time — an ungranted capability's
// host function is never registered into the guest's import namespace, so a
// guest that imports it cannot instantiate (see host.BuildHostModule and
// host.CheckImports). The allowlists are enforced inside the host functions,
// because "may call http_request" is not the same authorization as "may reach
// any host on the internet".
//
// Log and Config are the two capabilities a deployment grants by default (see
// the design doc's host capability catalogue); the zero Grant nevertheless
// grants nothing, so a Grant that was never populated cannot leak a
// capability by omission. Defaulting belongs to whoever parses the plugin
// manifest, not here.
type Grant struct {
	Log    bool
	Config bool
	KV     bool
	HTTP   bool
	FS     bool
	Tool   bool

	// AllowedHosts lists the hosts http_request may reach, matched exactly and
	// case-insensitively by HostAllowed. Empty denies every host: an HTTP
	// grant with no allowlist is a plugin that may call http_request and reach
	// nothing, not a plugin with unrestricted network access.
	AllowedHosts []string

	// AllowedPaths lists the directories (or exact files) read_file may open.
	// Empty denies every path, for the same reason AllowedHosts does.
	//
	// Containment is not decided here: the host checks it with
	// port.WorkspacePathGuard rooted at each entry, so that the allowlist holds
	// under symlinks instead of being a lexical prefix test a link inside an
	// allowed directory could walk out of.
	AllowedPaths []string
}

// HostAllowed reports whether host is in AllowedHosts.
//
// The match is exact and case-insensitive (DNS names are case-insensitive):
// no wildcards, no suffix matching. Suffix matching is the classic source of
// allowlist bypasses ("evil-example.com" suffix-matching "example.com"), and
// a phase that does not need patterns must not ship them.
func (g Grant) HostAllowed(host string) bool {
	if host == "" {
		return false
	}
	for _, allowed := range g.AllowedHosts {
		if strings.EqualFold(allowed, host) {
			return true
		}
	}
	return false
}
