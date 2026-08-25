package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/stardust/legion-agent/internal/security"
)

// ErrPluginNotFound is what PluginConsent.Grant/.Deny report when the
// deployment manifest holds no entry under the requested name.
//
// The four sentinels declared here exist so the HTTP handlers can turn a
// failure into a status code by CLASS instead of by matching on error text,
// which is the thing that really drifts. They are declared in this package,
// not in the implementation's, because the status contract is this package's
// to own and the implementation lives on the other side of the interface —
// see pluginConsentStatus for the mapping, which is mechanical: classifying
// an error is not re-deciding authorization, so it does not breach the rule
// that these handlers judge no capability or allowlist of their own.
var ErrPluginNotFound = errors.New("no such plugin in the deployment manifest")

// ErrPluginDeploymentChanged is what PluginConsent.Grant/.Deny report when
// the deployment manifest was edited by somebody else while the request was
// in flight, so writing now would silently revert that edit. It is a
// conflict the caller can resolve by re-reading and retrying, which is a
// different answer from "your request was malformed".
var ErrPluginDeploymentChanged = errors.New("the plugin deployment manifest changed while this request was running")

// ErrPluginStorage is what PluginConsent.Grant/.Deny report when the
// deployment manifest could not be read, parsed or written — an I/O or
// on-disk-state fault on the server, not a defect in the request. Reporting
// it as a 4xx would tell an operator to fix a request that was never the
// problem.
var ErrPluginStorage = errors.New("the plugin deployment manifest could not be read or written")

// ErrPluginUnavailable is what PluginConsent.Grant/.Deny report when this
// process has no plugin loader attached to converge against right now (it
// was detached mid-shutdown, say). The request is well formed and may
// succeed later, which is exactly what a 503 says and a 400 does not.
var ErrPluginUnavailable = errors.New("no plugin loader is attached to this process")

// PluginConsent is the plugin-authorization surface the HTTP layer consumes.
//
// It is an interface so internal/server never imports internal/plugin/loader:
// every other HTTPServer dependency (TaskStore, AgentCatalog, SkillManager…)
// follows the same rule, and serve assembly injects the implementation.
//
// Grant and Deny both write to the deployment manifest BEFORE they attempt to
// converge it, so by the time either returns, everything except a non-nil
// error means the disk already changed — see ConsentResult's own doc comment
// for the three states that follow from that, and this interface's HTTP
// handlers (handleGrantPlugin, handleDenyPlugin) for why neither of them
// performs any capability or allowlist judgement of its own: that
// validation lives once, in internal/plugin/consent, and Grant/Deny's
// implementation is the only caller this interface authorizes to run it.
type PluginConsent interface {
	List(ctx context.Context) ([]PluginView, error)

	// Grant authorizes the deployment entry named name to run with req's
	// capabilities/allowed hosts/allowed paths, validated against what the
	// plugin itself declares. A non-nil error means the deployment manifest
	// was NOT changed (every check that can reject the request runs before
	// the write); a nil error means the manifest already carries this grant,
	// with the ConsentResult's PendingConvergence/View reporting whether and
	// how it went on to converge.
	//
	// An error that belongs to one of the classes this package declares
	// (ErrPluginNotFound, ErrPluginDeploymentChanged, ErrPluginStorage,
	// ErrPluginUnavailable) must be returned wrapping that sentinel, so the
	// handler can map it to a status code without reading its text. An error
	// carrying none of them is a rejected request and reports 400.
	Grant(ctx context.Context, name string, req GrantRequest) (ConsentResult, error)

	// Deny revokes the deployment entry named name's authorization to run.
	// Like Grant, a non-nil error means the manifest was not changed; a nil
	// error means it already records the revocation, and an error in one of
	// this package's classes carries the matching sentinel.
	Deny(ctx context.Context, name string) (ConsentResult, error)
}

// GrantRequest is a POST /v1/plugins/{name}/grant request body: the
// capabilities, allowed hosts and allowed paths the caller wants to
// authorize. Every field is validated against the plugin's own plugin.json
// declaration by internal/plugin/consent before anything is written — see
// PluginConsent.Grant.
type GrantRequest struct {
	Capabilities []string `json:"capabilities"`
	AllowedHosts []string `json:"allowed_hosts"`
	AllowedPaths []string `json:"allowed_paths"`
}

// ConsentResult is what PluginConsent.Grant and .Deny return once their
// write to the deployment manifest has already succeeded (a failed write is
// reported as an error instead, never as a ConsentResult — see
// PluginConsent's own doc comment). It distinguishes the three states that
// remain once the disk has changed:
//
//   - PendingConvergence is true when convergence did NOT run (a
//     concurrent apply already in flight, or the wait for a task boundary
//     timed out or was cancelled): the write landed, but nothing has been
//     applied yet. View carries the facts that are already ON DISK — Name
//     and the Granted* fields just written — and no loader state at all
//     (State, Version, Detail and Tools stay empty, because no convergence
//     produced any). Name in particular is not withheld: a response the GUI
//     cannot match back to the row it came from is not a kinder answer.
//     ConvergenceDetail names why convergence did not run.
//   - PendingConvergence is false and View.State is not "failed" when
//     convergence ran and this entry came up (or went down, for a deny)
//     cleanly. ConvergenceDetail is non-empty here only when the
//     convergence reported errors that this entry nonetheless survived —
//     an unrelated entry failing, say. "Convergence ran, and these errors
//     happened" is a legitimate state, and dropping it would hide it.
//   - PendingConvergence is false and View.State is "failed" when
//     convergence ran but THIS entry specifically failed to activate (a
//     broken package, a tool-name conflict, …) — View.Detail names why.
//
// Reporting the third state as the first would leave an operator waiting for
// a convergence that already happened and will never come again on its own;
// reporting it as the second would hide a genuine activation failure behind
// "success". Both are the exact defect this type exists to make
// unrepresentable: the caller cannot construct a ConsentResult without
// picking one of the three honestly.
type ConsentResult struct {
	View               PluginView
	PendingConvergence bool
	ConvergenceDetail  string
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

// pluginConsentResponse is the JSON body handleGrantPlugin and
// handleDenyPlugin both write: the resulting PluginView with its fields
// promoted to the top level (PluginView is embedded anonymously and carries
// no json tag of its own), alongside the two fields that report how
// convergence went. See ConsentResult's own doc comment for what the three
// combinations of PendingConvergence and View.State mean.
type pluginConsentResponse struct {
	PluginView
	PendingConvergence bool   `json:"pending_convergence"`
	ConvergenceDetail  string `json:"convergence_detail,omitempty"`
}

// handleGrantPlugin serves POST /v1/plugins/{name}/grant: it decodes the
// request body into a GrantRequest and passes it straight to
// s.plugins.Grant, without performing any capability or allowlist judgement
// of its own -- see PluginConsent's own doc comment for why a second set of
// rules here is exactly the divergence this whole surface exists to
// prevent. Every validation failure, the concurrent-edit guard, and the
// four-outcome convergence reporting all come back through the single
// ConsentResult (or error) Grant returns; an error's CLASS, and nothing
// else, picks the status code — see pluginConsentStatus.
func (s *HTTPServer) handleGrantPlugin(w http.ResponseWriter, r *http.Request) {
	principal := security.PrincipalFromRequest(r)
	if !s.policy.Allows(principal, security.ActionWritePlugin, security.ResourcePlugin) {
		s.auditRBACDenied(r, principal, security.ResourcePlugin)
		writeError(w, http.StatusForbidden, "plugin access denied")
		return
	}
	if s.plugins == nil {
		writeError(w, http.StatusNotFound, "this process assembled no plugin loader; plugins are not enabled")
		return
	}
	name, ok := parsePluginConsentName(r.URL.Path, "/grant")
	if !ok {
		writeError(w, http.StatusNotFound, "bad plugin grant path")
		return
	}
	var req GrantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid grant request: %v", err))
		return
	}
	result, err := s.plugins.Grant(r.Context(), name, req)
	if err != nil {
		writeError(w, pluginConsentStatus(err), fmt.Sprintf("grant plugin %q: %v", name, err))
		return
	}
	writeJSON(w, http.StatusOK, pluginConsentResponse{
		PluginView:         result.View,
		PendingConvergence: result.PendingConvergence,
		ConvergenceDetail:  result.ConvergenceDetail,
	})
}

// handleDenyPlugin serves POST /v1/plugins/{name}/deny: the same shape as
// handleGrantPlugin, minus a request body -- deny names no new
// capabilities, so there is nothing to decode. See handleGrantPlugin's own
// doc comment for why neither handler judges anything itself.
func (s *HTTPServer) handleDenyPlugin(w http.ResponseWriter, r *http.Request) {
	principal := security.PrincipalFromRequest(r)
	if !s.policy.Allows(principal, security.ActionWritePlugin, security.ResourcePlugin) {
		s.auditRBACDenied(r, principal, security.ResourcePlugin)
		writeError(w, http.StatusForbidden, "plugin access denied")
		return
	}
	if s.plugins == nil {
		writeError(w, http.StatusNotFound, "this process assembled no plugin loader; plugins are not enabled")
		return
	}
	name, ok := parsePluginConsentName(r.URL.Path, "/deny")
	if !ok {
		writeError(w, http.StatusNotFound, "bad plugin deny path")
		return
	}
	result, err := s.plugins.Deny(r.Context(), name)
	if err != nil {
		writeError(w, pluginConsentStatus(err), fmt.Sprintf("deny plugin %q: %v", name, err))
		return
	}
	writeJSON(w, http.StatusOK, pluginConsentResponse{
		PluginView:         result.View,
		PendingConvergence: result.PendingConvergence,
		ConvergenceDetail:  result.ConvergenceDetail,
	})
}

// pluginConsentStatus maps one of PluginConsent's error classes to the HTTP
// status that names it, defaulting to 400 for an error carrying none of
// them.
//
// The mapping is mechanical on purpose: without it every failure a grant can
// have — an unknown plugin, a concurrent edit, a disk that will not write,
// and a genuinely malformed request — arrives at the GUI as the same 400,
// leaving it to match on error text to tell "your request is wrong" from
// "the server's disk is broken". Error text is the thing that drifts; an
// errors.Is against an exported sentinel is not.
//
// Classifying an error is not re-deciding authorization, so this does not
// breach handleGrantPlugin's rule that the handler judges no capability or
// allowlist of its own: which rule rejected the request, and why, still
// comes entirely from the service.
func pluginConsentStatus(err error) int {
	switch {
	case errors.Is(err, ErrPluginNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrPluginDeploymentChanged):
		return http.StatusConflict
	case errors.Is(err, ErrPluginStorage):
		return http.StatusInternalServerError
	case errors.Is(err, ErrPluginUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}

// parsePluginConsentName extracts the {name} path segment from
// "/v1/plugins/{name}"+suffix (suffix is "/grant" or "/deny"), the same
// prefix/suffix trim shape parseBrowserActionID uses for
// "/v1/browser/sessions/{id}/...". ok is false when the path does not carry
// that shape, or when the extracted name is empty or itself contains a "/"
// (which would mean the path had more segments than this route expects).
func parsePluginConsentName(path, suffix string) (name string, ok bool) {
	const prefix = "/v1/plugins/"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	name = strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}
