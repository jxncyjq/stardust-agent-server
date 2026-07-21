package skill

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Audit item V12: a manifest that could not be fetched or installed only bumped
// report.Failed and moved on.
//
// A counter is not a record. The CLI printed `failed=3` and there was no way to
// learn which manifest, or why — network, malformed JSON, unwritable disk, a
// blocked scan. A corrupt or hostile skill package was untraceable. The report is
// already the return channel here, so the reason travels in it rather than
// through a logger the syncer does not have.

func TestSyncReportsWhyEachSkillFailed(t *testing.T) {
	t.Parallel()

	var baseURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			_, _ = w.Write([]byte(`{"skills":[
				{"manifest_url":"` + baseURL + `/missing.json"},
				{"manifest_url":"` + baseURL + `/broken.json"}
			]}`))
		case "/broken.json":
			_, _ = w.Write([]byte(`not json at all`))
		default:
			http.NotFound(w, r)
		}
	}))
	baseURL = server.URL
	t.Cleanup(server.Close)

	syncer := NewRegistrySyncer(RegistrySyncConfig{
		IndexURL:    server.URL + "/index.json",
		InstallRoot: filepath.Join(t.TempDir(), "skills"),
		Repository:  NewMemoryRepository(),
		Scanner:     NewSecurityScanner(),
	})
	report, err := syncer.Sync(t.Context())
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil", err)
	}
	if report.Failed != 2 {
		t.Fatalf("Sync().Failed = %d, want 2", report.Failed)
	}
	if len(report.Failures) != report.Failed {
		t.Fatalf("Sync().Failures has %d entries, want %d — the count and the record must agree",
			len(report.Failures), report.Failed)
	}
	for _, failure := range report.Failures {
		if failure.ManifestURL == "" {
			t.Errorf("failure %#v has no manifest URL; the operator cannot tell which skill it was", failure)
		}
		if failure.Reason == "" {
			t.Errorf("failure %#v has no reason; %q alone is what this fix exists to remove", failure, "failed=N")
		}
	}
	joined := ""
	for _, failure := range report.Failures {
		joined += failure.ManifestURL + " " + failure.Reason + "\n"
	}
	for _, want := range []string{"missing.json", "broken.json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("failures = %q, want them to name %q", joined, want)
		}
	}
}

// TestSyncRecordsNoFailuresOnSuccess guards the other direction — a healthy sync
// must not manufacture entries.
func TestSyncRecordsNoFailuresOnSuccess(t *testing.T) {
	t.Parallel()

	content := skillDoc("go-clean", "Go Clean", "1.0.0", "safe", "active", "go")
	sha := sha256Hex(content)
	var baseURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			_, _ = w.Write([]byte(`{"skills":[{"manifest_url":"` + baseURL + `/go-clean.json"}]}`))
		case "/go-clean.json":
			_, _ = w.Write([]byte(`{"id":"go-clean","name":"Go Clean","version":"1.0.0","content_path":"` + baseURL + `/go-clean/SKILL.md","sha256":"` + sha + `"}`))
		case "/go-clean/SKILL.md":
			_, _ = w.Write([]byte(content))
		default:
			http.NotFound(w, r)
		}
	}))
	baseURL = server.URL
	t.Cleanup(server.Close)

	syncer := NewRegistrySyncer(RegistrySyncConfig{
		IndexURL:    server.URL + "/index.json",
		InstallRoot: filepath.Join(t.TempDir(), "skills"),
		Repository:  NewMemoryRepository(),
		Scanner:     NewSecurityScanner(),
	})
	report, err := syncer.Sync(t.Context())
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil", err)
	}
	if report.Installed != 1 || report.Failed != 0 || len(report.Failures) != 0 {
		t.Fatalf("report = %#v, want 1 installed and no failures", report)
	}
}
