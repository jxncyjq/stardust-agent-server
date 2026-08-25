package server

import (
	"context"
	"fmt"
	"net/http"
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
type PluginView struct {
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	State         string   `json:"state"`
	Detail        string   `json:"detail,omitempty"`
	Tools         []string `json:"tools"`
	DeclaredCaps  []string `json:"declared_capabilities"`
	DeclaredHosts []string `json:"declared_allowed_hosts"`
	DeclaredPaths []string `json:"declared_allowed_paths"`
	GrantedCaps   []string `json:"granted_capabilities"`
	GrantedHosts  []string `json:"granted_allowed_hosts"`
	GrantedPaths  []string `json:"granted_allowed_paths"`
}

// handleListPlugins serves GET /v1/plugins: every entry in the deployment
// manifest, carrying both what it DECLARES in its own plugin.json and what
// this deployment actually GRANTS it, for the GUI's plugin consent dialog.
//
// A nil s.plugins means this process assembled no plugin loader at all -- no
// "plugins.manifest" is configured -- and is reported as 404 rather than an
// empty list: an empty list reads as "no plugins installed", when the truth
// is "this deployment never turned plugins on". Different situation,
// different operator response.
func (s *HTTPServer) handleListPlugins(w http.ResponseWriter, r *http.Request) {
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
