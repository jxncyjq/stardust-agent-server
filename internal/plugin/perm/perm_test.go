package perm

import "testing"

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
	if len(g.AllowedPaths) != 0 {
		t.Errorf("zero Grant AllowedPaths = %v, want empty", g.AllowedPaths)
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

// TestHostAllowedDeniesEveryHostWithAnEmptyAllowlist covers the other
// fail-closed default: granting the http capability without naming a host is a
// plugin that may call http_request and reach nothing, not one with
// unrestricted network access.
func TestHostAllowedDeniesEveryHostWithAnEmptyAllowlist(t *testing.T) {
	g := Grant{HTTP: true}

	for _, host := range []string{"example.com", "localhost", "127.0.0.1"} {
		if g.HostAllowed(host) {
			t.Errorf("HostAllowed(%q) with an empty allowlist = true, want false", host)
		}
	}
}
