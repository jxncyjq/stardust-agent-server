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

// fakePluginConsent is a test double for PluginConsent whose List returns
// whatever the test configures -- a fixed view slice, or a fixed error.
type fakePluginConsent struct {
	views []PluginView
	err   error
}

func (f *fakePluginConsent) List(_ context.Context) ([]PluginView, error) {
	return f.views, f.err
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
