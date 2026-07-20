package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadRequireIdentityDefaultsToFalse pins the non-breaking default: unless
// an operator opts in, the agent keeps the single-tenant permissive contract.
func TestLoadRequireIdentityDefaultsToFalse(t *testing.T) {
	// Not parallel: t.Setenv isolates this from an operator's own exported
	// LEGION_AGENT_REQUIRE_IDENTITY, which Load would otherwise honour.
	t.Setenv("LEGION_AGENT_REQUIRE_IDENTITY", "")
	cfg, err := Load(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Load(defaults) error = %v, want nil", err)
	}
	if cfg.Server.RequireIdentity {
		t.Fatalf("Load(defaults).Server.RequireIdentity = %t, want false", cfg.Server.RequireIdentity)
	}
}

func TestLoadRequireIdentityFromJSONFile(t *testing.T) {
	t.Setenv("LEGION_AGENT_REQUIRE_IDENTITY", "")
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{"server": {"require_identity": true}}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}
	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if !cfg.Server.RequireIdentity {
		t.Fatalf("Load(%q).Server.RequireIdentity = %t, want true", path, cfg.Server.RequireIdentity)
	}
}

func TestLoadRequireIdentityFromEnvironment(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "true", want: true},
		{value: "1", want: true},
		{value: "false", want: false},
		{value: "0", want: false},
		{value: "TRUE", want: true},
		{value: "t", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("LEGION_AGENT_REQUIRE_IDENTITY", tt.value)
			cfg, err := Load(context.Background(), Options{})
			if err != nil {
				t.Fatalf("Load(env) error = %v, want nil", err)
			}
			if cfg.Server.RequireIdentity != tt.want {
				t.Fatalf("LEGION_AGENT_REQUIRE_IDENTITY=%q → Server.RequireIdentity = %t, want %t",
					tt.value, cfg.Server.RequireIdentity, tt.want)
			}
		})
	}
}

// TestLoadRequireIdentityRejectsUnparseableValue pins the fail-loud contract of
// the security toggle: "yes" must not degrade into the permissive default, or
// an operator who meant to harden the server gets zero hardening and zero
// warning.
func TestLoadRequireIdentityRejectsUnparseableValue(t *testing.T) {
	for _, value := range []string{"yes", "on", "enable", "ture"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("LEGION_AGENT_REQUIRE_IDENTITY", value)
			_, err := Load(context.Background(), Options{})
			if err == nil {
				t.Fatalf("Load(LEGION_AGENT_REQUIRE_IDENTITY=%q) error = nil, want a parse failure", value)
			}
			if !strings.Contains(err.Error(), "LEGION_AGENT_REQUIRE_IDENTITY") {
				t.Fatalf("Load error = %q, want it to name the offending variable", err)
			}
		})
	}
}
