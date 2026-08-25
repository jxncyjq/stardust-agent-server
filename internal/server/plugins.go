package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/stardust/legion-agent/internal/security"
)

// PluginConsent is the plugin-authorization surface the HTTP layer consumes.
//
// It is an interface so internal/server never imports internal/plugin/loader:
// every other HTTPServer dependency (TaskStore, AgentCatalog, SkillManager…)
// follows the same rule, and serve assembly injects the implementation.
type PluginConsent interface {
	List(ctx context.Context) ([]PluginView, error)
}

// PluginView is one deployment entry as the consent UI needs to see it.
//
// Declared and Granted are separate on purpose: the consent dialog renders the
// checklist from what the plugin DECLARES, and marks current state from what is
// already GRANTED. Collapsing them would make "this plugin wants http" and
// "http is authorized" indistinguishable.
//
// DeclaredUnresolved distinguishes "this plugin declares nothing" from "the
// server could not determine what this plugin declares" -- both would
// otherwise serialize identically as empty/absent Declared* fields. It is
// true only for a remote-source entry whose package is not (yet) in the
// local cache: resolving it would require a network fetch, which the
// read-only List path this view comes from never performs. The consent
// dialog must render this state distinctly from "requests nothing" rather
// than rendering an empty checklist either way.
type PluginView struct {
	Name               string   `json:"name"`
	Version            string   `json:"version"`
	State              string   `json:"state"`
	Detail             string   `json:"detail,omitempty"`
	Tools              []string `json:"tools"`
	DeclaredCaps       []string `json:"declared_capabilities"`
	DeclaredHosts      []string `json:"declared_allowed_hosts"`
	DeclaredPaths      []string `json:"declared_allowed_paths"`
	DeclaredUnresolved bool     `json:"declared_unresolved"`
	GrantedCaps        []string `json:"granted_capabilities"`
	GrantedHosts       []string `json:"granted_allowed_hosts"`
	GrantedPaths       []string `json:"granted_allowed_paths"`
}

// handleListPlugins serves GET /v1/plugins: every entry in the deployment
// manifest, carrying both what it DECLARES in its own plugin.json and what
// this deployment actually GRANTS it, for the GUI's plugin consent dialog.
//
// It is gated on security.ActionReadPlugin/security.ResourcePlugin in
// addition to the blanket authorized() admin-token check, the same
// per-resource RBAC pattern handleAuditEvents and handleQualityEvals follow
// (see governance.go): plugin capability/host/path declarations are exactly
// the sensitive deployment configuration that pattern exists to protect, and
// the mutating grant/deny endpoints planned to follow on this same interface
// inherit the gate deliberately rather than by omission.
//
// A nil s.plugins means this process assembled no plugin loader at all -- no
// "plugins.manifest" is configured -- and is reported as 404 rather than an
// empty list: an empty list reads as "no plugins installed", when the truth
// is "this deployment never turned plugins on". Different situation,
// different operator response.
func (s *HTTPServer) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	principal := security.PrincipalFromRequest(r)
	if !s.policy.Allows(principal, security.ActionReadPlugin, security.ResourcePlugin) {
		s.auditRBACDenied(r, principal, security.ResourcePlugin)
		writeError(w, http.StatusForbidden, "plugin access denied")
		return
	}
	if s.plugins == nil {
		writeError(w, http.StatusNotFound, "this process assembled no plugin loader; plugins are not enabled")
		return
	}
	views, err := s.plugins.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list plugins: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": views})
}
