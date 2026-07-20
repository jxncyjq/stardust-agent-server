package port

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// mustSymlink creates a symlink, skipping the test where the OS forbids it.
// Windows needs developer mode or elevation; silently passing there would hide
// whether the guard actually works on the platform under test.
func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks on this platform/config: %v", err)
	}
}

// Regression (P1-2): the guard compared paths lexically only — no EvalSymlinks
// anywhere in the repo. A link inside the workspace pointing outside it passed
// the check, and the subsequent os.Open followed it straight out of the sandbox.
func TestCheckRejectsSymlinkEscapingWorkspace(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{workspace, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v, want nil", dir, err)
		}
	}
	secret := filepath.Join(outside, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatalf("WriteFile(secret) error = %v, want nil", err)
	}
	link := filepath.Join(workspace, "escape")
	mustSymlink(t, outside, link)

	guard := NewWorkspacePathGuard(workspace)
	if _, err := guard.Check(context.Background(), filepath.Join(link, "id_rsa")); !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("Check(via escaping symlink) error = %v, want ErrPathOutsideWorkspace", err)
	}
}

// A link that stays inside the workspace is legitimate and must keep working —
// the fix must not turn every symlink into a denial.
func TestCheckAllowsSymlinkInsideWorkspace(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	realDir := filepath.Join(workspace, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(real) error = %v, want nil", err)
	}
	target := filepath.Join(realDir, "notes.md")
	if err := os.WriteFile(target, []byte("ok"), 0o644); err != nil {
		t.Fatalf("WriteFile(target) error = %v, want nil", err)
	}
	link := filepath.Join(workspace, "alias")
	mustSymlink(t, realDir, link)

	guard := NewWorkspacePathGuard(workspace)
	if _, err := guard.Check(context.Background(), filepath.Join(link, "notes.md")); err != nil {
		t.Fatalf("Check(via internal symlink) error = %v, want nil", err)
	}
}

// The workspace root itself may sit under a symlink (common with /tmp on macOS,
// or a linked project directory). Resolving only the target would then make
// every legitimate path look external.
func TestCheckAllowsWorkspaceRootBehindSymlink(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	realRoot := filepath.Join(base, "real-root")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(real-root) error = %v, want nil", err)
	}
	file := filepath.Join(realRoot, "notes.md")
	if err := os.WriteFile(file, []byte("ok"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v, want nil", err)
	}
	linkedRoot := filepath.Join(base, "linked-root")
	mustSymlink(t, realRoot, linkedRoot)

	guard := NewWorkspacePathGuard(linkedRoot)
	if _, err := guard.Check(context.Background(), filepath.Join(linkedRoot, "notes.md")); err != nil {
		t.Fatalf("Check(root behind symlink) error = %v, want nil", err)
	}
}

// Writes target paths that do not exist yet, so the check cannot simply give up
// when EvalSymlinks fails — it has to resolve the nearest existing ancestor.
// Otherwise "create a file under an escaping link" would bypass the guard.
func TestCheckResolvesNonExistentTargetViaAncestor(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{workspace, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v, want nil", dir, err)
		}
	}
	link := filepath.Join(workspace, "escape")
	mustSymlink(t, outside, link)

	guard := NewWorkspacePathGuard(workspace)
	// The file does not exist; the escaping ancestor still has to be caught.
	if _, err := guard.Check(context.Background(), filepath.Join(link, "new-file.txt")); !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("Check(new file under escaping symlink) error = %v, want ErrPathOutsideWorkspace", err)
	}

	// A new file at a legitimate location must still be allowed.
	if _, err := guard.Check(context.Background(), filepath.Join(workspace, "fresh.txt")); err != nil {
		t.Fatalf("Check(new file inside workspace) error = %v, want nil", err)
	}
}
