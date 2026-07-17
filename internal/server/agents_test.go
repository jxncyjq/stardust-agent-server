package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeCatalog is a stand-in AgentCatalog for the list-agents handler test.
type fakeCatalog struct{ names []string }

func (f fakeCatalog) Names() []string { return f.names }

func TestHandleListAgentsReturnsNames(t *testing.T) {
	srv := NewHTTPServer(Config{Agents: fakeCatalog{names: []string{"researcher", "writer"}}})

	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Agents) != 2 || body.Agents[0] != "researcher" || body.Agents[1] != "writer" {
		t.Fatalf("agents = %v, want [researcher writer]", body.Agents)
	}
}

// An empty catalog must serialize as [] (not null), so the GUI always receives a
// JSON array it can iterate.
func TestHandleListAgentsEmptyIsArray(t *testing.T) {
	srv := NewHTTPServer(Config{Agents: fakeCatalog{names: nil}})

	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); !containsJSONAgentsArray(got) {
		t.Fatalf("body = %s, want an agents array", got)
	}
}

func TestHandleListAgentsUnavailable(t *testing.T) {
	srv := NewHTTPServer(Config{}) // no catalog wired

	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// containsJSONAgentsArray confirms the response decodes with a non-nil agents
// slice (an empty catalog must be [] not null).
func containsJSONAgentsArray(body string) bool {
	var parsed struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return false
	}
	return parsed.Agents != nil && len(parsed.Agents) == 0
}
