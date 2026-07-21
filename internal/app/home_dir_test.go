package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Audit item V15: `homeDir, _ := os.UserHomeDir()` dropped the error.
//
// With HOME/USERPROFILE unset — service accounts, containers, Windows services —
// homeDir became "" and the global ~/.stardust/agents.md silently stopped
// loading, while isResidentAgents(root, absPath, "") also stopped recognising it
// as already-in-context and could re-inject it. The behaviour changes and the
// user just sees "my global conventions aren't taking effect".
//
// A missing home directory must not stop the agent from starting, so this is a
// Warn, not a returned error — but it must be visible.

func TestResolveHomeDirWarnsWhenUnresolvable(t *testing.T) {
	// Not parallel: t.Setenv forbids it.
	// Both variables, so this holds on either platform — Go reads USERPROFILE on
	// Windows and HOME elsewhere.
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	var logs bytes.Buffer
	got := resolveHomeDir(slog.New(slog.NewTextHandler(&logs, nil)))

	if got != "" {
		t.Errorf("resolveHomeDir = %q, want empty when the home directory cannot be resolved", got)
	}
	out := logs.String()
	for _, want := range []string{"WARN", "resolve home directory"} {
		if !strings.Contains(out, want) {
			t.Fatalf("logger output = %q, want it to contain %q", out, want)
		}
	}
}

func TestResolveHomeDirReturnsHomeWhenAvailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	var logs bytes.Buffer
	if got := resolveHomeDir(slog.New(slog.NewTextHandler(&logs, nil))); got == "" {
		t.Error("resolveHomeDir = \"\", want the resolved home directory")
	}
	if out := logs.String(); out != "" {
		t.Errorf("logger output = %q, want nothing logged on the happy path", out)
	}
}
