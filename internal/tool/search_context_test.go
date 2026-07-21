package tool

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// readFileContext is the ctx-aware replacement for os.ReadFile on the
// search_content hot path. os.ReadFile takes no context, so a cancelled or
// timed-out search still had to wait out the current file before noticing.
// The per-file cap bounds that wait, but "bounded" is not "responsive".

func TestReadFileContextStopsOnCancelledContext(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(path, []byte("NEEDLE"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	data, err := readFileContext(ctx, path, searchContentMaxFileBytes)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readFileContext(cancelled) error = %v, want context.Canceled", err)
	}
	if len(data) != 0 {
		t.Errorf("readFileContext(cancelled) returned %d bytes, want none", len(data))
	}
}

func TestReadFileContextReadsFileLargerThanOneChunk(t *testing.T) {
	t.Parallel()

	// Several chunks' worth, with a distinct tail so a loop that drops or
	// overwrites everything past the first chunk cannot pass.
	body := []byte(strings.Repeat("ab", searchContentReadChunk) + "TAIL")
	path := filepath.Join(t.TempDir(), "multi.txt")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("WriteFile error = %v, want nil", err)
	}

	data, err := readFileContext(context.Background(), path, searchContentMaxFileBytes)
	if err != nil {
		t.Fatalf("readFileContext error = %v, want nil", err)
	}
	if !bytes.Equal(data, body) {
		t.Fatalf("readFileContext returned %d bytes (tail %q), want %d bytes (tail %q)",
			len(data), tailOf(data), len(body), tailOf(body))
	}
}

// TestSearchContentStillMatchesAcrossChunkBoundary guards the integration: a
// needle straddling the boundary between two reads must still match. A chunked
// read that assembled the buffer wrongly would silently stop finding it.
func TestSearchContentStillMatchesAcrossChunkBoundary(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Pad so that "NEEDLE" lands astride the first chunk boundary.
	pad := strings.Repeat("x", searchContentReadChunk-3)
	if err := os.WriteFile(filepath.Join(root, "straddle.txt"), []byte(pad+"NEEDLE\n"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v, want nil", err)
	}

	registry := NewFileReadOnlyWorkspaceRegistry(root, nil)
	result, err := registry.Execute(context.Background(), searchAgent(), domain.ToolCall{
		ID:        "search-chunk",
		Name:      "search_content",
		Arguments: map[string]string{"pattern": "NEEDLE"},
	})
	if err != nil {
		t.Fatalf("Execute(search_content) error = %v, want nil", err)
	}
	if !strings.Contains(result.Output, "straddle.txt") {
		t.Errorf("a needle spanning a chunk boundary was not matched:\n%s", result.Output)
	}
}

func tailOf(b []byte) string {
	if len(b) <= 8 {
		return string(b)
	}
	return string(b[len(b)-8:])
}
