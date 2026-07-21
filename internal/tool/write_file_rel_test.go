package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// Audit item V16: write_file built its result with `rel, _ := filepath.Rel(...)`,
// dropping the error.
//
// When Rel fails, rel is "" and the tool reports `wrote 1234 bytes to ` — the
// model then believes the file landed at an empty path, and the read_file that
// follows cannot find it, which turns into a pointless retry loop. resolved has
// already passed the sandbox check by this point, so Rel failing means the
// sandbox's idea of the path and the real one disagree: an invariant violation,
// not something to paper over with a fallback.
//
// Note this is NOT the same site as relativeToRootOrBase, which falls back to the
// base name on purpose for search_content's notices.

func TestRelativeToRootFailsLoud(t *testing.T) {
	t.Parallel()

	// A relative base against an absolute target is the portable way to make
	// filepath.Rel fail — it errors on every OS, unlike the cross-drive case that
	// motivated this on Windows.
	if _, err := relativeToRoot("relative/base", string(filepath.Separator)+filepath.Join("abs", "target")); err == nil {
		t.Fatal("relativeToRoot(relative base, absolute target) error = nil, want an error")
	}
}

func TestRelativeToRootReturnsPathWhenResolvable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	got, err := relativeToRoot(root, filepath.Join(root, "sub", "file.txt"))
	if err != nil {
		t.Fatalf("relativeToRoot error = %v, want nil", err)
	}
	if want := filepath.Join("sub", "file.txt"); got != want {
		t.Errorf("relativeToRoot = %q, want %q", got, want)
	}
}

// TestWriteFileReportsARealPath guards the property the caller actually depends
// on: whatever write_file says it wrote to must not be empty.
func TestWriteFileReportsARealPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := NewWorkspaceRegistry(root, nil)
	result, err := registry.Execute(context.Background(), searchAgent(), domain.ToolCall{
		ID:        "write-1",
		Name:      "write_file",
		Arguments: map[string]string{"path": "notes/out.txt", "content": "hello"},
	})
	if err != nil {
		t.Fatalf("Execute(write_file) error = %v, want nil", err)
	}
	if strings.Contains(result.Output, "bytes to \n") || strings.HasSuffix(strings.TrimRight(result.Output, "\n"), "bytes to ") {
		t.Fatalf("write_file reported an empty destination: %q", result.Output)
	}
	if _, statErr := os.Stat(filepath.Join(root, "notes", "out.txt")); statErr != nil {
		t.Fatalf("Stat(written file) error = %v, want nil", statErr)
	}
}
