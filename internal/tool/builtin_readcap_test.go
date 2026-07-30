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
// file enters context, so a huge file cannot blow up the prompt. Every page is
// now bounded by pagination (readFilePageRunes) regardless of how large the
// underlying (byte-capped at maxReadFileBytes) file is, so a single call's
// output can never approach the file's real size.
func TestReadFileTruncatesLargeFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	big := filepath.Join(root, "big.txt")
	// 600KB, well over the 512KB read_file byte cap.
	if err := os.WriteFile(big, []byte(strings.Repeat("a", 600*1024)), 0o644); err != nil {
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
	if !strings.Contains(result.Output, "继续读用") {
		t.Fatal("read_file output of oversized file missing pagination continuation hint")
	}
	if len([]rune(result.Output)) > readFilePageRunes+200 {
		t.Fatalf("read_file output rune len %d exceeds page cap", len([]rune(result.Output)))
	}
}
