package skill

import (
	"os"
	"path/filepath"
	"testing"
)

// RootAvailable is the single gate every caller uses before mounting a skills
// root, so its exact boundaries are worth pinning down: a wrong answer either
// takes down every task routed through that runtime (mounting an unusable root)
// or silently drops a perfectly good skills directory.
func TestRootAvailable(t *testing.T) {
	t.Parallel()

	base := t.TempDir()

	dir := filepath.Join(base, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v, want nil", dir, err)
	}

	file := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", file, err)
	}

	tests := []struct {
		name string
		root string
		want bool
	}{
		{name: "existing directory", root: dir, want: true},
		{name: "empty string", root: "", want: false},
		{name: "whitespace only", root: "   ", want: false},
		{name: "missing path", root: filepath.Join(base, "absent"), want: false},
		// A regular file is a misconfiguration, not a skills root. Reporting it
		// as available would push the failure into the directory walk instead.
		{name: "existing regular file", root: file, want: false},
		// Surrounding whitespace is trimmed, so a padded but valid path still
		// mounts rather than being read as "not configured".
		{name: "padded valid directory", root: "  " + dir + "  ", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RootAvailable(tt.root); got != tt.want {
				t.Errorf("RootAvailable(%q) = %t, want %t", tt.root, got, tt.want)
			}
		})
	}
}
