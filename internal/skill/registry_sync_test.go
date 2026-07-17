package skill

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
)

func TestRegistrySyncInstallsRemoteManifest(t *testing.T) {
	t.Parallel()
	content := skillDoc("go-testing", "Go Testing", "1.0.0", "safe", "active", "go,test")
	sha := sha256Hex(content)
	var baseURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			_, _ = w.Write([]byte(`{"skills":[{"manifest_url":"` + baseURL + `/go-testing.json"}]}`))
		case "/go-testing.json":
			_, _ = w.Write([]byte(`{"id":"go-testing","name":"Go Testing","version":"1.0.0","content_path":"` + baseURL + `/go-testing/SKILL.md","sha256":"` + sha + `"}`))
		case "/go-testing/SKILL.md":
			_, _ = w.Write([]byte(content))
		default:
			http.NotFound(w, r)
		}
	}))
	baseURL = server.URL
	t.Cleanup(server.Close)

	repo := NewMemoryRepository()
	syncer := NewRegistrySyncer(RegistrySyncConfig{
		IndexURL:    server.URL + "/index.json",
		InstallRoot: filepath.Join(t.TempDir(), "skills"),
		Repository:  repo,
		Scanner:     NewSecurityScanner(),
	})
	report, err := syncer.Sync(t.Context())
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil", err)
	}
	if report.Installed != 1 {
		t.Fatalf("Sync().Installed = %d, want 1", report.Installed)
	}
	saved, ok, err := repo.GetSkill(t.Context(), "go-testing", "1.0.0")
	if err != nil {
		t.Fatalf("GetSkill(go-testing, 1.0.0) error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("GetSkill(go-testing, 1.0.0) ok = false, want true")
	}
	if saved.Hash != sha {
		t.Fatalf("GetSkill(go-testing, 1.0.0).Hash = %q, want %q", saved.Hash, sha)
	}
}

func TestRegistrySyncWritesInstallAudit(t *testing.T) {
	t.Parallel()
	content := skillDoc("go-audit", "Go Audit", "1.0.0", "safe", "active", "go,audit")
	serverURL := registryServer(t, "go-audit", content)
	audit := adapter.NewMemoryAuditLog()
	syncer := NewRegistrySyncer(RegistrySyncConfig{
		IndexURL:    serverURL + "/index.json",
		InstallRoot: filepath.Join(t.TempDir(), "skills"),
		Repository:  NewMemoryRepository(),
		Scanner:     NewSecurityScanner(),
		Audit:       audit,
	})

	report, err := syncer.Sync(t.Context())
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil", err)
	}
	if report.Installed != 1 {
		t.Fatalf("Sync().Installed = %d, want 1", report.Installed)
	}
	events := audit.Events()
	if len(events) != 1 {
		t.Fatalf("Audit.Events() len = %d, want 1", len(events))
	}
	if events[0].Action != "skill_installed" {
		t.Fatalf("Audit.Events()[0].Action = %q, want %q", events[0].Action, "skill_installed")
	}
}

func registryServer(t *testing.T, id string, content string) string {
	t.Helper()
	sha := sha256Hex(content)
	var baseURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			_, _ = w.Write([]byte(`{"skills":[{"manifest_url":"` + baseURL + `/` + id + `.json"}]}`))
		case "/" + id + ".json":
			_, _ = w.Write([]byte(`{"id":"` + id + `","name":"` + id + `","version":"1.0.0","content_path":"` + baseURL + `/` + id + `/SKILL.md","sha256":"` + sha + `"}`))
		case "/" + id + "/SKILL.md":
			_, _ = w.Write([]byte(content))
		default:
			http.NotFound(w, r)
		}
	}))
	baseURL = server.URL
	t.Cleanup(server.Close)
	return server.URL
}
