package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileArchive_SaveWritesAndReturnsRelPath(t *testing.T) {
	root := t.TempDir()
	a := newFileSnapshotArchive(".legion/browser/snapshots")
	rel, err := a.Save(root, "hello world")
	if err != nil {
		t.Fatalf("Save err = %v", err)
	}
	if !strings.HasPrefix(rel, ".legion/browser/snapshots/") || !strings.HasSuffix(rel, ".txt") {
		t.Fatalf("rel = %q, unexpected shape", rel)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read back err = %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("content = %q, want hello world", data)
	}
}

func TestFileArchive_DedupSameContent(t *testing.T) {
	root := t.TempDir()
	a := newFileSnapshotArchive(".legion/browser/snapshots")
	rel1, _ := a.Save(root, "same")
	rel2, _ := a.Save(root, "same")
	if rel1 != rel2 {
		t.Fatalf("dedup failed: %q != %q", rel1, rel2)
	}
}

func TestFileArchive_CleanupRemovesExpired(t *testing.T) {
	root := t.TempDir()
	a := newFileSnapshotArchive(".legion/browser/snapshots")
	rel, _ := a.Save(root, "old")
	p := filepath.Join(root, filepath.FromSlash(rel))
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatalf("chtimes err = %v", err)
	}
	if err := a.Cleanup(root, 24*time.Hour); err != nil {
		t.Fatalf("Cleanup err = %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("expired file still present, stat err = %v", err)
	}
}
