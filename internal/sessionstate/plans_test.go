package sessionstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritePlanCreatesFileUnderPlansDir(t *testing.T) {
	base := t.TempDir()
	store := NewStore(base)
	path, err := store.WritePlan("sess-1", "", "plan-1.md", "# hi\n")
	if err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	wantDir := filepath.Join(SessionDir(base, "sess-1"), "plans")
	if filepath.Dir(path) != wantDir {
		t.Errorf("plan dir = %q, want %q", filepath.Dir(path), wantDir)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if !strings.Contains(string(data), "# hi") {
		t.Errorf("plan content = %q, want it to contain the body", string(data))
	}
}

func TestWritePlanEmptyKeyFailsLoud(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.WritePlan("", "", "p.md", "x"); err == nil {
		t.Fatal("WritePlan empty key err = nil, want error")
	}
}
