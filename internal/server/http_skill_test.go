package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stardust/legion-agent/internal/skill"
)

const testSkillMarkdown = `---
id: gui-skill
name: GUI Skill
version: 1.0.0
---
# GUI Skill
A skill installed through the HTTP endpoint.
`

// TestHTTPServerInstallsSkill exercises the happy path: a POST to
// /v1/skills/install with a reachable source returns 200 and the installed
// skill summary.
func TestHTTPServerInstallsSkill(t *testing.T) {
	t.Parallel()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testSkillMarkdown))
	}))
	defer source.Close()

	srv := NewHTTPServer(Config{Skills: skill.NewDiskManager(t.TempDir(), nil)})

	rec := httptest.NewRecorder()
	body := `{"source":"` + source.URL + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/install", bytes.NewBufferString(body))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/skills/install status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(install response) error = %v, want nil", err)
	}
	if resp["id"] != "gui-skill" || resp["name"] != "GUI Skill" {
		t.Fatalf("install response = %#v, want id=gui-skill name=\"GUI Skill\"", resp)
	}
}

// TestHTTPServerInstallSkillRejectsMissingSource verifies the fail-loud path for
// an empty source: a 400 rather than a silent no-op.
func TestHTTPServerInstallSkillRejectsMissingSource(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{Skills: skill.NewDiskManager(t.TempDir(), nil)})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/install", bytes.NewBufferString(`{"source":"   "}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /v1/skills/install (empty source) status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestHTTPServerInstallSkillReportsBadSource verifies that an unreachable or
// malformed source produces a loud 400 carrying the underlying reason instead
// of being swallowed.
func TestHTTPServerInstallSkillReportsBadSource(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{Skills: skill.NewDiskManager(t.TempDir(), nil)})

	rec := httptest.NewRecorder()
	// A syntactically valid but unreachable URL: fetch must fail.
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/install",
		bytes.NewBufferString(`{"source":"http://127.0.0.1:0/missing"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /v1/skills/install (bad source) status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestHTTPServerUninstallSkillReportsUnknown verifies uninstalling a skill that
// is not present fails loudly with a 400.
func TestHTTPServerUninstallSkillReportsUnknown(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{Skills: skill.NewDiskManager(t.TempDir(), nil)})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/uninstall", bytes.NewBufferString(`{"name":"does-not-exist"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /v1/skills/uninstall (unknown) status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestHTTPServerSkillEndpointsUnavailableWithoutManager verifies that, when no
// skill manager is configured, the endpoints report 503 rather than panicking.
func TestHTTPServerSkillEndpointsUnavailableWithoutManager(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{})

	for _, path := range []string{"/v1/skills/install", "/v1/skills/update", "/v1/skills/uninstall"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"source":"x","name":"x"}`))
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("POST %s without manager status = %d, want %d", path, rec.Code, http.StatusServiceUnavailable)
		}
	}
}

// TestHTTPServerUpdatesSkill verifies update re-fetches a previously installed
// skill via its stored source and returns 200.
func TestHTTPServerUpdatesSkill(t *testing.T) {
	t.Parallel()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testSkillMarkdown))
	}))
	defer source.Close()

	mgr := skill.NewDiskManager(t.TempDir(), nil)
	srv := NewHTTPServer(Config{Skills: mgr})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/skills/install",
		bytes.NewBufferString(`{"source":"`+source.URL+`"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("install precondition status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/skills/update", bytes.NewBufferString(`{"name":"gui-skill"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/skills/update status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}
