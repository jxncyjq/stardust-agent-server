package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAPISpecIncludesCorePlatformRoutes(t *testing.T) {
	spec := BuildOpenAPISpec()
	requiredPaths := []string{
		"/healthz",
		"/readyz",
		"/metrics",
		"/debug/diagnostics",
		"/openapi.json",
		"/v1/sessions",
		"/v1/sessions/{id}/turns",
		"/v1/tasks",
		"/v1/tasks/{id}",
		"/v1/workflows",
		"/v1/workflows/{id}",
		"/v1/workflows/{id}/events",
		"/v1/events",
	}
	for _, path := range requiredPaths {
		if _, ok := spec.Paths[path]; !ok {
			t.Errorf("BuildOpenAPISpec().Paths missing %q", path)
		}
	}
	// Every route served by ServeHTTP must be documented with its verb, so the
	// spec stays in lockstep with the router switch.
	requiredOps := []struct {
		path   string
		method string
		op     func(OpenAPIPathItem) *OpenAPIOperation
	}{
		{"/v1/approvals", "GET", func(i OpenAPIPathItem) *OpenAPIOperation { return i.Get }},
		{"/v1/sessions", "POST", func(i OpenAPIPathItem) *OpenAPIOperation { return i.Post }},
		{"/v1/sessions/{id}", "PATCH", func(i OpenAPIPathItem) *OpenAPIOperation { return i.Patch }},
		{"/v1/sessions/{id}", "DELETE", func(i OpenAPIPathItem) *OpenAPIOperation { return i.Delete }},
		{"/v1/agents", "GET", func(i OpenAPIPathItem) *OpenAPIOperation { return i.Get }},
		{"/v1/tasks/{id}/approvals/{ticketID}", "POST", func(i OpenAPIPathItem) *OpenAPIOperation { return i.Post }},
		{"/v1/skills/install", "POST", func(i OpenAPIPathItem) *OpenAPIOperation { return i.Post }},
		{"/v1/skills/update", "POST", func(i OpenAPIPathItem) *OpenAPIOperation { return i.Post }},
		{"/v1/skills/uninstall", "POST", func(i OpenAPIPathItem) *OpenAPIOperation { return i.Post }},
	}
	for _, want := range requiredOps {
		item, ok := spec.Paths[want.path]
		if !ok {
			t.Errorf("BuildOpenAPISpec().Paths missing %q", want.path)
			continue
		}
		if want.op(item) == nil {
			t.Errorf("BuildOpenAPISpec().Paths[%q] missing %s operation", want.path, want.method)
		}
	}
	if _, ok := spec.Components.Schemas["DiagnosticsSnapshot"]; !ok {
		t.Errorf("BuildOpenAPISpec().Components.Schemas missing DiagnosticsSnapshot")
	}
	if _, ok := spec.Components.Schemas["EventEnvelope"]; !ok {
		t.Errorf("BuildOpenAPISpec().Components.Schemas missing EventEnvelope")
	}
	if _, ok := spec.Components.Schemas["AgentSession"]; !ok {
		t.Errorf("BuildOpenAPISpec().Components.Schemas missing AgentSession")
	}
	if _, ok := spec.Components.Schemas["ConversationTurn"]; !ok {
		t.Errorf("BuildOpenAPISpec().Components.Schemas missing ConversationTurn")
	}
}

func TestHTTPServerServesOpenAPIWithoutAdminToken(t *testing.T) {
	srv := NewHTTPServer(Config{AdminToken: "token"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var spec OpenAPISpec
	if err := json.NewDecoder(rec.Body).Decode(&spec); err != nil {
		t.Fatalf("Decode(/openapi.json) error = %v, want nil", err)
	}
	if spec.OpenAPI != "3.1.0" {
		t.Fatalf("GET /openapi.json openapi = %q, want %q", spec.OpenAPI, "3.1.0")
	}
}

func TestOpenAPISpecIncludesErrorResponses(t *testing.T) {
	spec := BuildOpenAPISpec()
	op := spec.Paths["/v1/tasks"].Post
	if op == nil {
		t.Fatalf("BuildOpenAPISpec().Paths[/v1/tasks].Post = nil, want operation")
	}
	for _, status := range []string{"400", "401", "403", "500"} {
		if _, ok := op.Responses[status]; !ok {
			t.Errorf("BuildOpenAPISpec().Paths[/v1/tasks].Post.Responses[%s] missing", status)
		}
	}
	if _, ok := spec.Components.Schemas["ErrorResponse"]; !ok {
		t.Errorf("BuildOpenAPISpec().Components.Schemas[ErrorResponse] missing")
	}
}
