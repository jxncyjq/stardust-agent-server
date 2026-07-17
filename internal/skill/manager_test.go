package skill

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const testSkillContent = `---
id: test-skill
name: Test Skill
version: 1.0.0
---
# Test Skill
A skill for testing.
`

func TestDiskManagerInstallFromHTTPURL(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(testSkillContent)) //nolint:errcheck
	}))
	defer srv.Close()

	root := t.TempDir()
	mgr := NewDiskManager(root, nil)

	sk, err := mgr.Install(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Install() error = %v, want nil", err)
	}
	if sk.ID != "test-skill" {
		t.Fatalf("Install().ID = %q, want test-skill", sk.ID)
	}
	if _, statErr := os.Stat(filepath.Join(root, "test-skill", "SKILL.md")); statErr != nil {
		t.Fatalf("SKILL.md not found after install: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "test-skill", ".source")); statErr != nil {
		t.Fatalf(".source not found after install: %v", statErr)
	}
}

func TestDiskManagerInstallStoresSourceForUpdate(t *testing.T) {
	t.Parallel()

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.Write([]byte(testSkillContent)) //nolint:errcheck
	}))
	defer srv.Close()

	root := t.TempDir()
	mgr := NewDiskManager(root, nil)

	if _, err := mgr.Install(context.Background(), srv.URL); err != nil {
		t.Fatalf("Install() error = %v, want nil", err)
	}
	if _, err := mgr.Update(context.Background(), "test-skill"); err != nil {
		t.Fatalf("Update() error = %v, want nil", err)
	}
	if callCount != 2 {
		t.Fatalf("HTTP server called %d times, want 2 (install + update)", callCount)
	}
}

func TestDiskManagerUpdateFailsWithoutStoredSource(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mgr := NewDiskManager(root, nil)

	_, err := mgr.Update(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("Update() error = nil, want error for missing .source file")
	}
}

func TestDiskManagerUninstallRemovesDirectory(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(testSkillContent)) //nolint:errcheck
	}))
	defer srv.Close()

	root := t.TempDir()
	mgr := NewDiskManager(root, nil)

	if _, err := mgr.Install(context.Background(), srv.URL); err != nil {
		t.Fatalf("Install() error = %v, want nil", err)
	}
	if err := mgr.Uninstall(context.Background(), "test-skill"); err != nil {
		t.Fatalf("Uninstall() error = %v, want nil", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "test-skill")); !os.IsNotExist(statErr) {
		t.Fatalf("skill directory still exists after uninstall")
	}
}

func TestDiskManagerUninstallFailsForMissingSkill(t *testing.T) {
	t.Parallel()

	mgr := NewDiskManager(t.TempDir(), nil)
	if err := mgr.Uninstall(context.Background(), "no-such-skill"); err == nil {
		t.Fatal("Uninstall() error = nil, want error for missing skill")
	}
}

func TestResolveSourceURLGitHub(t *testing.T) {
	t.Parallel()

	got := resolveSourceURL("github:owner/my-skill")
	want := "https://raw.githubusercontent.com/owner/my-skill/main/SKILL.md"
	if got != want {
		t.Fatalf("resolveSourceURL(%q) = %q, want %q", "github:owner/my-skill", got, want)
	}
}

func TestResolveSourceURLPassthroughHTTPS(t *testing.T) {
	t.Parallel()

	src := "https://example.com/skills/my-skill/SKILL.md"
	if got := resolveSourceURL(src); got != src {
		t.Fatalf("resolveSourceURL(%q) = %q, want passthrough", src, got)
	}
}
