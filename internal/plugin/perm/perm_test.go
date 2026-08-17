package perm

import (
	"path/filepath"
	"testing"
)

// TestZeroGrantAuthorizesNothing pins the fail-closed default: a Grant nobody
// populated must not hand out a capability or an allowlist entry.
func TestZeroGrantAuthorizesNothing(t *testing.T) {
	var g Grant

	if g.Log || g.Config || g.KV || g.HTTP || g.FS || g.Tool {
		t.Errorf("zero Grant = %+v, want every capability false", g)
	}
	if g.HostAllowed("example.com") {
		t.Error("zero Grant HostAllowed(example.com) = true, want false")
	}
	if g.PathAllowed(filepath.Join("workspace", "file.txt")) {
		t.Error("zero Grant PathAllowed(workspace/file.txt) = true, want false")
	}
}

func TestHostAllowedMatchesExactlyAndCaseInsensitively(t *testing.T) {
	g := Grant{HTTP: true, AllowedHosts: []string{"API.Example.com", "localhost"}}

	cases := []struct {
		host string
		want bool
	}{
		{host: "API.Example.com", want: true},
		{host: "api.example.com", want: true},
		{host: "API.EXAMPLE.COM", want: true},
		{host: "localhost", want: true},
		// No suffix matching: the classic allowlist bypass.
		{host: "evil-api.example.com", want: false},
		{host: "api.example.com.evil.test", want: false},
		{host: "sub.api.example.com", want: false},
		// No wildcards, no empty-host pass.
		{host: "*.example.com", want: false},
		{host: "", want: false},
	}
	for _, tc := range cases {
		if got := g.HostAllowed(tc.host); got != tc.want {
			t.Errorf("HostAllowed(%q) = %t, want %t", tc.host, got, tc.want)
		}
	}
}

func TestPathAllowedContainsOnlyListedTrees(t *testing.T) {
	root := filepath.Join(string(filepath.Separator)+"workspace", "plugins")
	allowed := filepath.Join(root, "data")
	exactFile := filepath.Join(root, "single", "one.txt")
	g := Grant{FS: true, AllowedPaths: []string{allowed, exactFile}}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{name: "the allowed directory itself", path: allowed, want: true},
		{name: "a file inside it", path: filepath.Join(allowed, "a.txt"), want: true},
		{name: "a nested file inside it", path: filepath.Join(allowed, "deep", "b.txt"), want: true},
		{name: "the exact allowed file", path: exactFile, want: true},
		{name: "a sibling of the exact allowed file", path: filepath.Join(root, "single", "two.txt"), want: false},
		{name: "a sibling directory", path: filepath.Join(root, "other", "c.txt"), want: false},
		{name: "the parent", path: root, want: false},
		{name: "a traversal spelling that cleans back out", path: filepath.Join(allowed, "..", "escape.txt"), want: false},
		{name: "an empty path", path: "", want: false},
	}
	for _, tc := range cases {
		if got := g.PathAllowed(tc.path); got != tc.want {
			t.Errorf("%s: PathAllowed(%q) = %t, want %t", tc.name, tc.path, got, tc.want)
		}
	}
}

// TestPathAllowedIgnoresEmptyAllowlistEntries covers the fail-closed handling
// of a malformed allowlist: an empty entry must not turn into "the process
// working directory" (filepath.Clean("") == "."), which would silently widen
// the grant to wherever the agent happens to be running.
func TestPathAllowedIgnoresEmptyAllowlistEntries(t *testing.T) {
	g := Grant{FS: true, AllowedPaths: []string{""}}

	if g.PathAllowed(filepath.Join("some", "file.txt")) {
		t.Error("PathAllowed with an empty allowlist entry = true, want false")
	}
}
