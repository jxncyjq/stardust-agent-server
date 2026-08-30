package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Loopback hardening mints a one-time bearer token so an embedded frontend can
// reach its own serve and nothing else on the machine can.
//
// It used to be INFERRED: "the caller passed no address, so this must be the
// GUI". The GUI passes "127.0.0.1:0" explicitly -- it wants a random loopback
// port and says so -- which made the inference false in the one place the
// hardening was written for. The serve the GUI starts required no token at
// all. An embedder now ASKS for it instead of hoping to be guessed right.

// hardeningConfig writes the smallest agent.json BuildServeService accepts and
// returns its path.
func hardeningConfig(t *testing.T, extra string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	body := fmt.Sprintf(`{"storage": {"driver": "memory"}, "context_files": {"root": %s}%s}`,
		jsonString(dir), extra)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func buildHardenedServe(t *testing.T, opts ServeOptions) *ServeResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	result, err := BuildServeService(ctx, opts)
	if err != nil {
		t.Fatalf("BuildServeService: %v", err)
	}
	t.Cleanup(result.Close)
	return &result
}

func TestAnEmbedderCanAskForLoopbackHardening(t *testing.T) {
	result := buildHardenedServe(t, ServeOptions{
		ConfigPath:        hardeningConfig(t, ""),
		Addr:              "127.0.0.1:0",
		LoopbackHardening: true,
	})

	if result.Token == "" {
		t.Error("Token is empty with LoopbackHardening requested: the serve accepts anything on the machine " +
			"and the embedded frontend has nothing to authenticate with")
	}
}

// TestAServeThatWasNotAskedToHardenStaysOpen: the CLI's own
// `serve --addr 127.0.0.1:port` must not start demanding a token because this
// option exists.
func TestAServeThatWasNotAskedToHardenStaysOpen(t *testing.T) {
	result := buildHardenedServe(t, ServeOptions{
		ConfigPath: hardeningConfig(t, ""),
		Addr:       "127.0.0.1:0",
	})

	if result.Token != "" {
		t.Errorf("Token = %q without asking for hardening; that would break every existing local client",
			result.Token)
	}
}

// TestAConfiguredAdminTokenSurvivesHardening: an operator who set one keeps
// theirs. Minting over it would silently lock out the clients they configured.
func TestAConfiguredAdminTokenSurvivesHardening(t *testing.T) {
	result := buildHardenedServe(t, ServeOptions{
		ConfigPath:        hardeningConfig(t, `, "server": {"admin_token": "operator-token"}`),
		Addr:              "127.0.0.1:0",
		LoopbackHardening: true,
	})

	if result.Token != "operator-token" {
		t.Errorf("Token = %q, want the operator's own admin_token", result.Token)
	}
}

// TestAHardenedServeRefusesAnUnauthenticatedRequest is the property that
// matters. Minting a token proves nothing on its own: the defect being fixed
// was a serve that handed nobody a token AND asked nobody for one, and a test
// that only reads result.Token would pass on a build where the middleware was
// never installed.
func TestAHardenedServeRefusesAnUnauthenticatedRequest(t *testing.T) {
	result := buildHardenedServe(t, ServeOptions{
		ConfigPath:        hardeningConfig(t, ""),
		Addr:              "127.0.0.1:0",
		LoopbackHardening: true,
	})
	runServeInBackground(t, result)

	if status := getStatus(t, result.BaseURL, "/v1/sessions", ""); status != http.StatusUnauthorized {
		t.Errorf("GET /v1/sessions without a token = %d, want %d: anything on the machine can drive this serve",
			status, http.StatusUnauthorized)
	}
	if status := getStatus(t, result.BaseURL, "/v1/sessions", result.Token); status == http.StatusUnauthorized {
		t.Error("GET /v1/sessions with the minted token = 401: the embedder cannot reach its own serve")
	}
}

// runServeInBackground starts the assembled service and stops it when the test
// ends. BuildServeService only binds the listener; the caller runs the loop.
func runServeInBackground(t *testing.T, result *ServeResult) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- result.Service.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("service stopped with %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("service did not stop within 30s")
		}
	})
}

func getStatus(t *testing.T, baseURL, path, token string) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// The Origin guard fronts every route in hardening mode; a same-origin
	// caller is exactly what the embedded frontend is.
	req.Header.Set("Origin", baseURL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", baseURL+path, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
