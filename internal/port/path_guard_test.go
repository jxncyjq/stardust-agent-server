package port

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestWorkspacePathGuardAllowsPathInsideRoot(t *testing.T) {
	t.Parallel()

	root := filepath.Clean(`C:\workspace`)
	guard := NewWorkspacePathGuard(root)
	got, err := guard.Check(context.Background(), filepath.Join(root, "src", "main.go"))
	if err != nil {
		t.Fatalf("Check() error = %v, want nil", err)
	}
	if got == "" {
		t.Errorf("Check() path = empty, want cleaned path inside root")
	}
}

func TestWorkspacePathGuardRejectsPathOutsideRoot(t *testing.T) {
	t.Parallel()

	guard := NewWorkspacePathGuard(filepath.Clean(`C:\workspace`))
	_, err := guard.Check(context.Background(), filepath.Clean(`C:\other\secret.txt`))
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("Check() error = %v, want ErrPathOutsideWorkspace", err)
	}
}
