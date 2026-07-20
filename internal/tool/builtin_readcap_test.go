package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// TestReadFileTruncatesLargeFile asserts read_file caps how much of an oversized
// file enters context, so a huge file cannot blow up the prompt.
func TestReadFileTruncatesLargeFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	big := filepath.Join(root, "big.txt")
	// 512KB, well over the 256KB read_file cap.
	if err := os.WriteFile(big, []byte(strings.Repeat("a", 512*1024)), 0o644); err != nil {
		t.Fatalf("write big file: %v", err)
	}

	registry := NewFileReadOnlyWorkspaceRegistry(root, nil)
	result, err := registry.Execute(context.Background(), domain.Agent{Role: "developer"}, domain.ToolCall{
		ID:        "read-1",
		Name:      "read_file",
		Arguments: map[string]string{"path": "big.txt"},
	})
	if err != nil {
		t.Fatalf("Execute(read_file) error = %v, want nil", err)
	}
	if !result.Success {
		t.Fatalf("Execute(read_file).Success = false (%s)", result.Error)
	}
	if !strings.Contains(result.Output, "truncated") {
		t.Fatal("read_file output of oversized file missing truncation marker")
	}
	if len(result.Output) > 256*1024+200 {
		t.Fatalf("read_file output len %d exceeds cap", len(result.Output))
	}
}
