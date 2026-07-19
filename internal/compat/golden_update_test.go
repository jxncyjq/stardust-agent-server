package compat

import (
	"flag"
	"os"
	"testing"
)

// updateGolden rewrites the golden fixtures from the current output instead of
// comparing against them:
//
//	go test ./internal/compat/ -run TestOpenAPI -update
//
// Use it only after reading the reported diff and confirming the change is
// intended. The golden files are the contract; regenerating them to make a red
// test go green is exactly the failure mode these tests exist to catch.
var updateGolden = flag.Bool("update", false, "rewrite golden fixtures in testdata/ from the current output")

// assertGolden compares got against the golden fixture at path, or rewrites the
// fixture when -update is set. got must already end in the trailing newline the
// fixture carries.
func assertGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *updateGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("os.WriteFile(%q) error = %v, want nil", path, err)
		}
		t.Logf("golden %q rewritten from current output", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v, want nil", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s golden mismatch\nwant:\n%s\ngot:\n%s", path, want, got)
	}
}
