package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// fakePluginConsent is a test double for PluginConsent whose List, Grant and
// Deny each return whatever the test configures -- a fixed result, or a
// fixed error. grantName/grantReq and denyName capture what the handler
// actually passed through, so a test can assert the handler performed no
// judgement of its own (see handleGrantPlugin's own doc comment).
type fakePluginConsent struct {
	views []PluginView
	err   error

	grantResult ConsentResult
	grantErr    error
	grantName   string
	grantReq    GrantRequest

	denyResult ConsentResult
	denyErr    error
	denyName   string
}

func (f *fakePluginConsent) List(_ context.Context) ([]PluginView, error) {
	return f.views, f.err
}

func (f *fakePluginConsent) Grant(_ context.Context, name string, req GrantRequest) (ConsentResult, error) {
	f.grantName = name
	f.grantReq = req
	return f.grantResult, f.grantErr
}

func (f *fakePluginConsent) Deny(_ context.Context, name string) (ConsentResult, error) {
	f.denyName = name
	return f.denyResult, f.denyErr
}

// TestPluginsListReturnsDeclaredAndGrantedSeparately is the handler-level
// happy path: GET /v1/plugins returns 200, and each entry's declared
// capabilities (what plugin.json asks for) and granted capabilities (what the
// deployment actually authorizes) travel as two distinct fields rather than
// being collapsed into one.
func TestPluginsListReturnsDeclaredAndGrantedSeparately(t *testing.T) {
	t.Parallel()
	fake := &fakePluginConsent{views: []PluginView{
		{
			Name:          "jira",
			Version:       "1.0.0",
			State:         "pending",
			Tools:         []string{"jira_search"},
			DeclaredCaps:  []string{"http", "log"},
			DeclaredHosts: []string{"jira.example.com"},
			GrantedCaps:   []string{"log"},
		},
	}}
	srv := NewHTTPServer(Config{Plugins: fake})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/plugins status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Plugins []PluginView `json:"plugins"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Plugins) != 1 {
		t.Fatalf("len(plugins) = %d, want 1 body=%s", len(resp.Plugins), rec.Body.String())
	}
	got := resp.Plugins[0]
	wantDeclared := []string{"http", "log"}
	wantGranted := []string{"log"}
	if !reflect.DeepEqual(got.DeclaredCaps, wantDeclared) {
		t.Fatalf("DeclaredCaps = %v, want %v", got.DeclaredCaps, wantDeclared)
	}
	if !reflect.DeepEqual(got.GrantedCaps, wantGranted) {
		t.Fatalf("GrantedCaps = %v, want %v", got.GrantedCaps, wantGranted)
	}
	// The mutation this pins: DeclaredCaps and GrantedCaps must be reported
	// from two separate fields, not the same one -- collapsing them would
	// make "this plugin WANTS http" and "http IS authorized" indistinguishable
	// (see PluginView's own doc comment). A plugin.json that declares two
	// capabilities but is only granted one is exactly the case that would
	// stop being visible if the two fields were ever merged into one.
	if reflect.DeepEqual(got.DeclaredCaps, got.GrantedCaps) {
		t.Fatalf("DeclaredCaps and GrantedCaps must be reported separately, both came back %v", got.DeclaredCaps)
	}
}

// TestPluginsListRequiresAuthorization verifies GET /v1/plugins is gated by
// the same admin-token check as every other endpoint: a caller with no (or
// the wrong) Authorization header is refused before List is ever called.
func TestPluginsListRequiresAuthorization(t *testing.T) {
	t.Parallel()
	fake := &fakePluginConsent{views: []PluginView{{Name: "jira"}}}
	srv := NewHTTPServer(Config{Plugins: fake, AdminToken: "token"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/plugins without token status = %d, want %d body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	req.Header.Set("Authorization", "Bearer token")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/plugins with token status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestPluginsListReportsServiceError verifies that a List failure produces a
// 5xx carrying the underlying reason in the body, rather than a 200 with an
// empty/zero-value plugins array that would read as "no plugins installed".
func TestPluginsListReportsServiceError(t *testing.T) {
	t.Parallel()
	fake := &fakePluginConsent{err: errors.New("read plugin deployment manifest: permission denied")}
	srv := NewHTTPServer(Config{Plugins: fake})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /v1/plugins (List error) status = %d, want %d body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, rec.Body.String())
	}
	if resp.Error == "" {
		t.Fatalf("error response body carries no reason: %s", rec.Body.String())
	}
	if !strings.Contains(resp.Error, "permission denied") {
		t.Fatalf("error response = %q, want it to name the underlying failure", resp.Error)
	}
}

// TestPluginsListRequiresPluginReadPermission verifies GET /v1/plugins is
// gated by security.ActionReadPlugin/security.ResourcePlugin -- the same
// per-resource RBAC pattern handleAuditEvents and handleQualityEvals follow
// (governance.go) -- in addition to the blanket authorized() admin-token
// check TestPluginsListRequiresAuthorization already covers. A caller
// asserting the viewer role is refused before List is ever called; a caller
// asserting admin succeeds.
func TestPluginsListRequiresPluginReadPermission(t *testing.T) {
	t.Parallel()
	fake := &fakePluginConsent{views: []PluginView{{Name: "jira"}}}
	srv := NewHTTPServer(Config{Plugins: fake, AdminToken: "token"})

	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	req.Header.Set("X-Role", "viewer")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/plugins viewer status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	req.Header.Set("X-Role", "admin")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/plugins admin status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "jira") {
		t.Fatalf("GET /v1/plugins admin body = %s, want the plugin list", rec.Body.String())
	}
}

// TestPluginsListWithoutConsentServiceReports404 verifies the documented
// distinction: a process that never assembled a plugin loader answers 404
// naming that plugins are not enabled, rather than 200 with an empty list --
// which would read as "no plugins installed" instead of "plugins are off".
func TestPluginsListWithoutConsentServiceReports404(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/plugins without a plugin consent service status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, rec.Body.String())
	}
	if resp.Error == "" {
		t.Fatalf("404 response body carries no explanation: %s", rec.Body.String())
	}
}

// --- POST /v1/plugins/{name}/grant and /deny --------------------------------

// grantConsentResponse decodes handleGrantPlugin/handleDenyPlugin's JSON
// body: PluginView's fields promoted to the top level, alongside the two
// convergence-outcome fields.
type grantConsentResponse struct {
	Name               string   `json:"name"`
	Version            string   `json:"version"`
	State              string   `json:"state"`
	Detail             string   `json:"detail"`
	Tools              []string `json:"tools"`
	PendingConvergence bool     `json:"pending_convergence"`
	ConvergenceDetail  string   `json:"convergence_detail"`
}

// adminGrantRequest builds an authorized POST request against
// /v1/plugins/{name}/grant (or /deny, when path already names it) carrying
// body as its JSON payload -- nil for deny, which has no body.
func adminGrantRequest(t *testing.T, path string, body any) *http.Request {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		reader = strings.NewReader(string(data))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(http.MethodPost, path, reader)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	req.Header.Set("X-Role", "admin")
	return req
}

// TestPluginsGrantWriteFailureReportsErrorStatus is outcome 1 of the
// four-outcome contract: the service's Grant refused the request (a write
// failure -- validation, an unknown entry, a concurrent edit, …) before
// ever writing to disk, and the handler must report that as a 4xx/5xx
// naming the reason rather than a 200. The disk-bytes-unchanged half of
// this outcome is asserted at the service level (plugin_consent_service_test.go),
// where a real manifest file exists to check.
func TestPluginsGrantWriteFailureReportsErrorStatus(t *testing.T) {
	t.Parallel()
	fake := &fakePluginConsent{grantErr: errors.New(`plugin consent: grant "jira": names capability "http", which plugin "jira" does not declare`)}
	srv := NewHTTPServer(Config{Plugins: fake, AdminToken: "token"})

	req := adminGrantRequest(t, "/v1/plugins/jira/grant", GrantRequest{Capabilities: []string{"http"}})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code < 400 || rec.Code >= 600 {
		t.Fatalf("POST /v1/plugins/jira/grant (write failure) status = %d, want 4xx/5xx body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "does not declare") {
		t.Fatalf("POST /v1/plugins/jira/grant error body = %s, want it to name the underlying failure", rec.Body.String())
	}
	if fake.grantName != "jira" {
		t.Fatalf("Grant was called with name = %q, want %q", fake.grantName, "jira")
	}
	if !reflect.DeepEqual(fake.grantReq.Capabilities, []string{"http"}) {
		t.Fatalf("Grant was called with Capabilities = %v, want [http]: the handler must pass the request through untouched", fake.grantReq.Capabilities)
	}
}

// TestPluginsGrantConvergedReportsStateFromLoader is outcome 2: convergence
// ran and this entry came up cleanly. pending_convergence must be false and
// state must be exactly what the service's ConsentResult.View reported --
// the handler performs no judgement of its own.
func TestPluginsGrantConvergedReportsStateFromLoader(t *testing.T) {
	t.Parallel()
	fake := &fakePluginConsent{grantResult: ConsentResult{
		View: PluginView{Name: "jira", Version: "1.0.0", State: "loaded", Tools: []string{"jira_search"}},
	}}
	srv := NewHTTPServer(Config{Plugins: fake, AdminToken: "token"})

	req := adminGrantRequest(t, "/v1/plugins/jira/grant", GrantRequest{Capabilities: []string{"log"}})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/plugins/jira/grant (converged) status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp grantConsentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if resp.PendingConvergence {
		t.Fatalf("PendingConvergence = true, want false: convergence already ran")
	}
	if resp.State != "loaded" {
		t.Fatalf("state = %q, want %q (the loader's own state, from ConsentResult.View)", resp.State, "loaded")
	}
}

// TestPluginsGrantPendingConvergenceReportsPendingTrue is outcome 3:
// convergence did NOT run (a concurrent apply, or a boundary wait that
// timed out or was cancelled). The write already landed -- see
// ConsentResult's own doc comment -- so this is still 200, but
// pending_convergence must be true and convergence_detail must name why:
// reporting this as plain success would leave an operator believing a
// grant took effect when it has not yet been applied at all.
func TestPluginsGrantPendingConvergenceReportsPendingTrue(t *testing.T) {
	t.Parallel()
	fake := &fakePluginConsent{grantResult: ConsentResult{
		PendingConvergence: true,
		ConvergenceDetail:  "apply plugin change at task boundary: waited 5s for the running tasks to finish, 1 task(s) still running, nothing was applied: context deadline exceeded",
	}}
	srv := NewHTTPServer(Config{Plugins: fake, AdminToken: "token"})

	req := adminGrantRequest(t, "/v1/plugins/jira/grant", GrantRequest{Capabilities: []string{"log"}})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/plugins/jira/grant (pending) status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp grantConsentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if !resp.PendingConvergence {
		t.Fatalf("PendingConvergence = false, want true: convergence did not run")
	}
	if resp.ConvergenceDetail == "" {
		t.Fatalf("ConvergenceDetail is empty, want it to name why convergence did not run")
	}
}

// TestPluginsGrantEntryActivationFailureReportsFailedNotPending is outcome
// 4: convergence RAN, but this entry itself failed to activate (a broken
// package, a tool-name conflict, …). This must be reported as
// pending_convergence=false with state="failed" and a non-empty detail --
// the OPPOSITE PendingConvergence value from
// TestPluginsGrantPendingConvergenceReportsPendingTrue. Reporting this
// outcome as pending would leave an operator waiting for a convergence
// that already ran and will never come again on its own.
func TestPluginsGrantEntryActivationFailureReportsFailedNotPending(t *testing.T) {
	t.Parallel()
	fake := &fakePluginConsent{grantResult: ConsentResult{
		View: PluginView{Name: "jira", State: "failed", Detail: `error: activate plugin "jira" from /pkg/jira: contributes tool name "jira_search" that another contributor already owns`},
	}}
	srv := NewHTTPServer(Config{Plugins: fake, AdminToken: "token"})

	req := adminGrantRequest(t, "/v1/plugins/jira/grant", GrantRequest{Capabilities: []string{"log"}})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/plugins/jira/grant (activation failed) status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp grantConsentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if resp.PendingConvergence {
		t.Fatalf("PendingConvergence = true, want false: convergence already ran, this entry just failed to activate")
	}
	if resp.State != "failed" {
		t.Fatalf("state = %q, want %q", resp.State, "failed")
	}
	if resp.Detail == "" {
		t.Fatalf("detail is empty, want it to name why this entry failed to activate")
	}
}

// TestPluginsGrantRequiresPluginWritePermission verifies POST
// /v1/plugins/{name}/grant is gated by
// security.ActionWritePlugin/security.ResourcePlugin -- distinct from
// (and stricter than) ActionReadPlugin: an operator, who reads plugins
// fine, is refused here, and so is a viewer; only admin succeeds.
func TestPluginsGrantRequiresPluginWritePermission(t *testing.T) {
	t.Parallel()
	fake := &fakePluginConsent{grantResult: ConsentResult{View: PluginView{Name: "jira", State: "loaded"}}}
	srv := NewHTTPServer(Config{Plugins: fake, AdminToken: "token"})

	for _, role := range []string{"operator", "viewer"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/plugins/jira/grant", strings.NewReader(`{"capabilities":["log"]}`))
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("X-Company-ID", "company-1")
		req.Header.Set("X-Role", role)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("POST /v1/plugins/jira/grant role=%s status = %d, want %d body=%s", role, rec.Code, http.StatusForbidden, rec.Body.String())
		}
	}

	req := adminGrantRequest(t, "/v1/plugins/jira/grant", GrantRequest{Capabilities: []string{"log"}})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/plugins/jira/grant role=admin status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestPluginsGrantWithoutConsentServiceReports404 mirrors
// TestPluginsListWithoutConsentServiceReports404 for the write side: a
// process that assembled no plugin loader reports 404, not a 200 that
// implies the grant took effect.
func TestPluginsGrantWithoutConsentServiceReports404(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{})

	req := adminGrantRequest(t, "/v1/plugins/jira/grant", GrantRequest{Capabilities: []string{"log"}})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /v1/plugins/jira/grant without a plugin consent service status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// TestPluginsDenyRevokesAndReportsStateFromLoader is Deny's happy path: 200,
// pending_convergence false, and the state the service's ConsentResult.View
// reports (a denied entry that converged cleanly shows up as "disabled" via
// mergePluginStatus -- see plugin_consent_service.go). It also pins that
// Deny sends no request body: the handler must not try to decode one.
func TestPluginsDenyRevokesAndReportsStateFromLoader(t *testing.T) {
	t.Parallel()
	fake := &fakePluginConsent{denyResult: ConsentResult{
		View: PluginView{Name: "jira", State: "disabled", Detail: `reason: the manifest entry sets "enabled": false`},
	}}
	srv := NewHTTPServer(Config{Plugins: fake, AdminToken: "token"})

	req := adminGrantRequest(t, "/v1/plugins/jira/deny", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/plugins/jira/deny status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp grantConsentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if resp.PendingConvergence {
		t.Fatalf("PendingConvergence = true, want false")
	}
	if resp.State != "disabled" {
		t.Fatalf("state = %q, want %q", resp.State, "disabled")
	}
	if fake.denyName != "jira" {
		t.Fatalf("Deny was called with name = %q, want %q", fake.denyName, "jira")
	}
}

// TestPluginsDenyRequiresPluginWritePermission is
// TestPluginsGrantRequiresPluginWritePermission for Deny -- the same gate,
// exercised on the other mutating endpoint.
func TestPluginsDenyRequiresPluginWritePermission(t *testing.T) {
	t.Parallel()
	fake := &fakePluginConsent{denyResult: ConsentResult{View: PluginView{Name: "jira", State: "disabled"}}}
	srv := NewHTTPServer(Config{Plugins: fake, AdminToken: "token"})

	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/jira/deny", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Company-ID", "company-1")
	req.Header.Set("X-Role", "operator")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /v1/plugins/jira/deny role=operator status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}
