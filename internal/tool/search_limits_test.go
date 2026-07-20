package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

func searchAgent() domain.Agent {
	return domain.Agent{ID: "researcher", CompanyID: "company-1", Role: "developer"}
}

// Regression (P2-2): every candidate file was read with os.ReadFile, whole and
// unbounded. One large file in the workspace was enough to blow up memory —
// read_file has had a 256KiB cap all along, search_content had none.
func TestSearchContentSkipsOversizedFileAndSaysSo(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	huge := filepath.Join(root, "huge.log")
	// Comfortably past the per-file cap, with the needle at the very end so a
	// naive implementation would have to read all of it to match.
	filler := strings.Repeat("x", 512*1024)
	if err := os.WriteFile(huge, []byte(filler+"\nNEEDLE\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(huge) error = %v, want nil", err)
	}
	small := filepath.Join(root, "small.txt")
	if err := os.WriteFile(small, []byte("NEEDLE here\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(small) error = %v, want nil", err)
	}

	registry := NewFileReadOnlyWorkspaceRegistry(root, nil)
	result, err := registry.Execute(context.Background(), searchAgent(), domain.ToolCall{
		ID:        "search-1",
		Name:      "search_content",
		Arguments: map[string]string{"pattern": "NEEDLE"},
	})
	if err != nil {
		t.Fatalf("Execute(search_content) error = %v, want nil", err)
	}
	if !strings.Contains(result.Output, "small.txt") {
		t.Errorf("the searchable file is missing from the results:\n%s", result.Output)
	}

	// The oversized file must appear as a *skip notice*, not as a match.
	// Asserting merely that "huge.log" appears somewhere would also pass when
	// the cap is absent — the file contains the needle, so it would show up as
	// an ordinary result. The assertion has to name the reason.
	var skipped bool
	var matchedDespiteCap bool
	for _, line := range strings.Split(result.Output, "\n") {
		if !strings.Contains(line, "huge.log") {
			continue
		}
		if strings.Contains(line, "skipped") && strings.Contains(line, "exceeds") {
			skipped = true
			continue
		}
		matchedDespiteCap = true
	}
	if matchedDespiteCap {
		t.Errorf("an oversized file was read and matched instead of being capped:\n%s", result.Output)
	}
	if !skipped {
		t.Errorf("an oversized file was skipped without being reported:\n%s", result.Output)
	}
}

// Regression: matches had no cap, so a broad pattern could return hundreds of
// megabytes straight into the model context.
func TestSearchContentCapsResultCountAndSaysSo(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for i := range 400 {
		name := filepath.Join(root, fmt.Sprintf("file-%03d.txt", i))
		if err := os.WriteFile(name, []byte("NEEDLE\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v, want nil", name, err)
		}
	}

	registry := NewFileReadOnlyWorkspaceRegistry(root, nil)
	result, err := registry.Execute(context.Background(), searchAgent(), domain.ToolCall{
		ID:        "search-2",
		Name:      "search_content",
		Arguments: map[string]string{"pattern": "NEEDLE"},
	})
	if err != nil {
		t.Fatalf("Execute(search_content) error = %v, want nil", err)
	}
	lines := strings.Split(strings.TrimSpace(result.Output), "\n")
	if len(lines) > searchContentMaxMatches+5 {
		t.Errorf("result lines = %d, want it capped near %d", len(lines), searchContentMaxMatches)
	}
	// A truncated list that does not say it is truncated reads as a complete
	// answer, which is worse than the size it was meant to avoid.
	if !strings.Contains(result.Output, "truncated") {
		t.Errorf("results were capped without saying so:\n%s", result.Output)
	}
}

// Same reasoning for list_files: an unbounded recursive listing is both a memory
// risk and a silently-incomplete answer.
func TestListFilesCapsResultCountAndSaysSo(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Comfortably past listFilesMaxEntries so the cap actually engages.
	for i := range listFilesMaxEntries + 50 {
		name := filepath.Join(root, fmt.Sprintf("file-%04d.txt", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v, want nil", name, err)
		}
	}

	registry := NewFileReadOnlyWorkspaceRegistry(root, nil)
	result, err := registry.Execute(context.Background(), searchAgent(), domain.ToolCall{
		ID:        "list-1",
		Name:      "list_files",
		Arguments: map[string]string{},
	})
	if err != nil {
		t.Fatalf("Execute(list_files) error = %v, want nil", err)
	}
	lines := strings.Split(strings.TrimSpace(result.Output), "\n")
	if len(lines) > listFilesMaxEntries+5 {
		t.Errorf("result lines = %d, want it capped near %d", len(lines), listFilesMaxEntries)
	}
	if !strings.Contains(result.Output, "truncated") {
		t.Errorf("listing was capped without saying so:\n%s", result.Output)
	}
}

// A normal search must stay unchanged — the caps are a safety net, not a new
// behaviour for ordinary workspaces.
func TestSearchContentUnaffectedBelowLimits(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha NEEDLE\n"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v, want nil", err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v, want nil", err)
	}

	registry := NewFileReadOnlyWorkspaceRegistry(root, nil)
	result, err := registry.Execute(context.Background(), searchAgent(), domain.ToolCall{
		ID:        "search-3",
		Name:      "search_content",
		Arguments: map[string]string{"pattern": "NEEDLE"},
	})
	if err != nil {
		t.Fatalf("Execute(search_content) error = %v, want nil", err)
	}
	if !strings.Contains(result.Output, "a.txt") {
		t.Errorf("expected match missing:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "truncated") {
		t.Errorf("a small search should not report truncation:\n%s", result.Output)
	}
}
