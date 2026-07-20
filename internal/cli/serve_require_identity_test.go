package cli

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serveRequireIdentityService boots a real serve assembly from an on-disk
// agent.json with server.require_identity enabled, and returns the base URL of
// its listener. It exists to cover the one wiring line
// (server.Config.RequireIdentity <- cfg.Server.RequireIdentity in
// BuildServeService) that no config/server/security package test can see:
// deleting it makes the configured switch silently inert, which the fail-loud
// rule forbids.
func serveRequireIdentityService(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	body := `{
		"storage": {"driver": "memory"},
		"server": {
			"admin_token": "serve-token",
			"require_identity": true
		}
	}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result, err := BuildServeService(ctx, ServeOptions{
		ConfigPath: configPath,
		Addr:       "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		cancel()
		t.Fatalf("BuildServeService() error = %v, want nil", err)
	}

	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = result.Service.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("service did not stop within 5s after context cancel")
		}
		result.Close()
	})

	return "http://" + result.Listener.Addr().String()
}

func serveRequireIdentityGet(t *testing.T, baseURL string, headers map[string]string) (int, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/audit-events", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v, want nil", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/audit-events error = %v, want nil", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll(response body) error = %v, want nil", err)
	}
	return resp.StatusCode, string(payload)
}

// TestBuildServeServiceAppliesRequireIdentityToHTTP is the assembly guard: with
// server.require_identity=true in the config file, a request carrying a valid
// admin token but no X-Role must be rejected by the RBAC policy. If the config
// value stops reaching server.Config the endpoint answers 200 and this test
// fails — which is precisely the silent-config-failure this covers.
func TestBuildServeServiceAppliesRequireIdentityToHTTP(t *testing.T) {
	t.Parallel()
	baseURL := serveRequireIdentityService(t)

	status, body := serveRequireIdentityGet(t, baseURL, map[string]string{
		"Authorization": "Bearer serve-token",
		"X-Company-ID":  "company-1",
	})
	if status != http.StatusForbidden {
		t.Fatalf("GET /v1/audit-events without X-Role status = %d, want %d body=%s", status, http.StatusForbidden, body)
	}
	if !strings.Contains(body, "audit access denied") {
		t.Fatalf("GET /v1/audit-events without X-Role body = %s, want %q", body, "audit access denied")
	}
}

// TestBuildServeServiceRequireIdentityDeniesOnPolicyNotToken is the control
// case that keeps the guard above honest: the very same request plus an admin
// X-Role succeeds, proving the 403 came from the identity policy and not from
// token authentication (which would answer 401 anyway).
func TestBuildServeServiceRequireIdentityDeniesOnPolicyNotToken(t *testing.T) {
	t.Parallel()
	baseURL := serveRequireIdentityService(t)

	status, body := serveRequireIdentityGet(t, baseURL, map[string]string{
		"Authorization": "Bearer serve-token",
		"X-Company-ID":  "company-1",
		"X-Role":        "admin",
	})
	if status != http.StatusOK {
		t.Fatalf("GET /v1/audit-events with admin role status = %d, want %d body=%s", status, http.StatusOK, body)
	}

	badToken, _ := serveRequireIdentityGet(t, baseURL, map[string]string{
		"Authorization": "Bearer wrong-token",
		"X-Company-ID":  "company-1",
		"X-Role":        "admin",
	})
	if badToken != http.StatusUnauthorized {
		t.Fatalf("GET /v1/audit-events with bad token status = %d, want %d", badToken, http.StatusUnauthorized)
	}
}
