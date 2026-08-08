package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildServeServiceExposesLoopbackToken pins that the GUI-default loopback
// bind (empty listen_addr + empty ServeOptions.Addr, which defaults to
// 127.0.0.1:0 and flips guiDefaultAddr) mints a one-time bearer token and
// exposes it — together with the real listen address — on ServeResult. Without
// this, the in-process Wails GUI consumer cannot authenticate against the
// Phase 4B hardened serve and every HTTP/SSE call is 403.
//
// The empty Addr (not a literal "127.0.0.1:0") is deliberate: hardening is
// scoped to the GUI-defaulted bind, so an explicit --addr 127.0.0.1:0 would
// NOT arm it. The config file forces listen_addr="" so the default ":8080"
// does not shadow the GUI default.
func TestBuildServeServiceExposesLoopbackToken(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	body := `{
		"storage": {"driver": "memory"},
		"server": {"listen_addr": ""}
	}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}

	res, err := BuildServeService(context.Background(), ServeOptions{
		ConfigPath: configPath,
		Addr:       "",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("BuildServeService: %v", err)
	}
	defer res.Close()

	if res.Token == "" {
		t.Fatal("expected non-empty loopback Token exposed on ServeResult")
	}
	if len(res.Token) < 32 {
		t.Fatalf("token too short: %q", res.Token)
	}
	if !strings.HasPrefix(res.BaseURL, "http://127.0.0.1:") {
		t.Fatalf("BaseURL = %q, want http://127.0.0.1:<port>", res.BaseURL)
	}
}
