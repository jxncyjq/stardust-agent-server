package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// TestReadWriteRegistryWritesInsideSandbox pins the whole point of the
// read-write workspace registry: unlike NewFileReadOnlyWorkspaceRegistry, its
// agent can create a file inside the workspace root.
func TestReadWriteRegistryWritesInsideSandbox(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := NewFileReadWriteWorkspaceRegistry(root, nil)
	result, err := registry.Execute(context.Background(), searchAgent(), domain.ToolCall{
		ID:        "write-1",
		Name:      "write_file",
		Arguments: map[string]string{"path": "notes/out.txt", "content": "hello"},
	})
	if err != nil {
		t.Fatalf("Execute(write_file) error = %v, want nil", err)
	}
	if !result.Success {
		t.Fatalf("write_file result.Success = false, error = %q", result.Error)
	}
	if _, statErr := os.Stat(filepath.Join(root, "notes", "out.txt")); statErr != nil {
		t.Fatalf("Stat(written file) error = %v, want the file to exist", statErr)
	}
}

// TestReadWriteRegistryRejectsWriteOutsideSandbox pins that write_file reuses
// the same WorkspacePathGuard as the read tools: a path escaping the root is
// refused and nothing lands outside.
func TestReadWriteRegistryRejectsWriteOutsideSandbox(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := NewFileReadWriteWorkspaceRegistry(root, nil)
	_, err := registry.Execute(context.Background(), searchAgent(), domain.ToolCall{
		ID:        "write-escape",
		Name:      "write_file",
		Arguments: map[string]string{"path": "../escape.txt", "content": "nope"},
	})
	if err == nil {
		t.Fatal("Execute(write_file ../escape.txt) error = nil, want a sandbox rejection")
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(root), "escape.txt")); statErr == nil {
		t.Fatal("an escaping write_file created a file outside the workspace root")
	}
}

// TestReadWriteRegistryDoesNotInjectAgentsNote pins the reason this is a
// separate constructor rather than reusing NewWorkspaceRegistry: the CLI's
// "append the nearest directory's agents.md to the write result" behaviour is an
// interactive-session UX feature, and must not leak into serve / per-agent
// tasks. A directory agents.md present at the write location must NOT be
// injected into the tool result here.
func TestReadWriteRegistryDoesNotInjectAgentsNote(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	subDir := filepath.Join(root, "sub")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", subDir, err)
	}
	// A non-resident agents.md at the write location: NewWorkspaceRegistry with
	// injection would append its contents to the result; this registry must not.
	if err := os.WriteFile(filepath.Join(subDir, "agents.md"), []byte("本目录约定：请勿注入我"), 0o644); err != nil {
		t.Fatalf("WriteFile(agents.md) error = %v", err)
	}

	registry := NewFileReadWriteWorkspaceRegistry(root, nil)
	result, err := registry.Execute(context.Background(), searchAgent(), domain.ToolCall{
		ID:        "write-noinject",
		Name:      "write_file",
		Arguments: map[string]string{"path": "sub/out.txt", "content": "hi"},
	})
	if err != nil {
		t.Fatalf("Execute(write_file) error = %v, want nil", err)
	}
	if strings.Contains(result.Output, "本目录约定") || strings.Contains(result.Output, "📁") {
		t.Errorf("write_file result leaked an agents.md injection: %q", result.Output)
	}
}

// TestReadWriteRegistryIsReadOnlyPlusWriteFile pins the intended relationship
// between the two file registries: they differ by exactly write_file. The two
// constructors maintain their tool policy/permission separately, so without
// this an added file tool could land in one and not the other and neither the
// compiler nor any other test would notice.
func TestReadWriteRegistryIsReadOnlyPlusWriteFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	names := func(r *Registry) map[string]bool {
		m := make(map[string]bool)
		for _, d := range r.Descriptors() {
			m[d.Name] = true
		}
		return m
	}
	readOnly := names(NewFileReadOnlyWorkspaceRegistry(root, nil))
	readWrite := names(NewFileReadWriteWorkspaceRegistry(root, nil))

	if readOnly["write_file"] {
		t.Error("read-only registry unexpectedly registers write_file")
	}
	if !readWrite["write_file"] {
		t.Fatal("read-write registry missing write_file")
	}
	// Remove the one intended difference; what remains must match exactly.
	delete(readWrite, "write_file")
	if len(readWrite) != len(readOnly) {
		t.Errorf("read-write minus write_file = %v, want it equal to read-only %v (the two constructors drifted)", readWrite, readOnly)
	}
	for name := range readOnly {
		if !readWrite[name] {
			t.Errorf("read-write registry missing read-only tool %q", name)
		}
	}
}
