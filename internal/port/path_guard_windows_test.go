//go:build windows

package port

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Windows spells one path many ways. Each spelling below reaches outside the
// workspace; the guard must refuse every one of them, because a caller that
// gets a path back does the I/O with it.
func TestGuardRefusesWindowsEscapeSpellings(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}

	guard := NewWorkspacePathGuard(root)
	cases := map[string]string{
		"parent traversal":     filepath.Join(root, "..", "outside", "secret.txt"),
		"absolute outside":     secret,
		"lowercased drive":     strings.ToLower(secret),
		"uppercased drive":     strings.ToUpper(secret),
		"forward slashes":      filepath.ToSlash(secret),
		"extended-length form": `\\?\` + secret,
		"trailing dot":         secret + ".",
		"trailing space":       secret + " ",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := guard.Check(context.Background(), path); !errors.Is(err, ErrPathOutsideWorkspace) {
				t.Fatalf("guard admitted %q (err=%v); the caller would then read it", path, err)
			}
		})
	}
}

// A case-different spelling of a path INSIDE the workspace must still be
// admitted: Windows paths are case-insensitive, and refusing them would break
// ordinary use rather than close a hole.
func TestGuardAdmitsCaseDifferentInsidePath(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(inside, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	guard := NewWorkspacePathGuard(root)
	for _, spelling := range []string{
		strings.ToUpper(inside),
		strings.ToLower(inside),
	} {
		if _, err := guard.Check(context.Background(), spelling); err != nil {
			t.Fatalf("guard refused an inside path spelled %q: %v", spelling, err)
		}
	}
}

// Windows reserves device names at every directory level: opening "NUL" or
// "COM1" reaches a device, not a file in the workspace, however the path is
// spelled.
func TestGuardRefusesWindowsDeviceNames(t *testing.T) {
	root := t.TempDir()
	guard := NewWorkspacePathGuard(root)

	for _, device := range []string{"NUL", "CON", "COM1", "LPT1", "nul", "con.txt"} {
		t.Run(device, func(t *testing.T) {
			path := filepath.Join(root, device)
			if _, err := guard.Check(context.Background(), path); !errors.Is(err, ErrPathOutsideWorkspace) {
				t.Fatalf("guard admitted device path %q (err=%v)", path, err)
			}
		})
	}
}

// An alternate data stream rides on a file name: "notes.txt:hidden" writes to
// a stream the workspace listing never shows.
func TestGuardRefusesAlternateDataStreams(t *testing.T) {
	root := t.TempDir()
	guard := NewWorkspacePathGuard(root)

	path := filepath.Join(root, "notes.txt:hidden")
	if _, err := guard.Check(context.Background(), path); !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("guard admitted an alternate data stream %q (err=%v)", path, err)
	}
}
