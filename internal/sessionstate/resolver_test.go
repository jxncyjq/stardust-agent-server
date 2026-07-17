package sessionstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveWorkspaceRootUsesConfiguredExistingDir(t *testing.T) {
	dir := t.TempDir()
	root, warning := ResolveWorkspaceRoot(dir)
	if root != dir {
		t.Errorf("root = %q, want %q", root, dir)
	}
	if warning != "" {
		t.Errorf("warning = %q, want empty for a valid configured dir", warning)
	}
}

func TestResolveWorkspaceRootFallsBackAndWarnsOnMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	root, warning := ResolveWorkspaceRoot(missing)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	want := filepath.Join(home, ".stardust")
	if root != want {
		t.Errorf("root = %q, want fallback %q", root, want)
	}
	if !strings.Contains(warning, missing) {
		t.Errorf("warning = %q, want it to mention the bad path %q", warning, missing)
	}
}

func TestResolveWorkspaceRootEmptyConfigUsesDefaultWithoutWarning(t *testing.T) {
	root, warning := ResolveWorkspaceRoot("")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	want := filepath.Join(home, ".stardust")
	if root != want {
		t.Errorf("root = %q, want default %q", root, want)
	}
	if warning != "" {
		t.Errorf("warning = %q, want empty when config is unset (default is not a misconfiguration)", warning)
	}
}

func TestResolveWorkspaceRootExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	// ~/.stardust after expansion equals <home>/.stardust; whether it exists or
	// not the returned root must be the expanded absolute path, never literal "~".
	root, _ := ResolveWorkspaceRoot("~/.stardust")
	want := filepath.Join(home, ".stardust")
	if root != want {
		t.Errorf("root = %q, want expanded %q", root, want)
	}
}

func TestSessionDirJoinsUnderSessionSegment(t *testing.T) {
	got := SessionDir("/base", "sess-1")
	want := filepath.Join("/base", "session", "sess-1")
	if got != want {
		t.Errorf("SessionDir = %q, want %q", got, want)
	}
}
