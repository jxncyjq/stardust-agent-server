package adapter

import (
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/config"
)

// Audit item V14 flagged `return nil, nil` here as a fail-loud violation and
// suggested returning an error instead. It is not a violation, and that change
// would be wrong — but the contract was undocumented, which is what actually
// needed fixing. These tests pin it.
//
// A nil client is returned only when MaaS is not configured at all, and every
// consumer handles that deliberately: serve substitutes a recording client
// (cli.BuildServeService), run/tui pass it to app.RunTask which does the same,
// and maasProfileResolver rejects it loudly because MoA cannot run without a
// real model. scripts/smoke.ps1's prompt-smoke depends on exactly this path.
//
// What must stay loud is a profile that was *named* and does not exist — a
// configuration error, not an absence.

func TestNewMaasClientFromProfileReturnsNilWhenUnconfigured(t *testing.T) {
	t.Parallel()

	client, err := NewMaasClientFromProfile(config.MaasConfig{}, "")
	if err != nil {
		t.Fatalf("NewMaasClientFromProfile(empty config) error = %v, want nil", err)
	}
	if client != nil {
		t.Fatalf("client = %#v, want nil — an unconfigured MaaS is an absence the callers substitute for", client)
	}
}

func TestNewMaasClientFromProfileFailsLoudOnMissingNamedProfile(t *testing.T) {
	t.Parallel()

	_, err := NewMaasClientFromProfile(config.MaasConfig{
		Profiles: map[string]config.MaasProfile{"dev": {BaseURL: "http://localhost"}},
	}, "prod")
	if err == nil {
		t.Fatal("NewMaasClientFromProfile(unknown profile) error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "prod") {
		t.Errorf("error = %q, want it to name the missing profile", err.Error())
	}
}

func TestNewMaasClientFromProfileFailsLoudOnMissingDefaultProfile(t *testing.T) {
	t.Parallel()

	// DefaultProfile names a profile that is not in the map: still a named
	// profile, so still an error rather than the unconfigured-absence case.
	_, err := NewMaasClientFromProfile(config.MaasConfig{DefaultProfile: "ghost"}, "")
	if err == nil {
		t.Fatal("NewMaasClientFromProfile(missing default profile) error = nil, want an error")
	}
}

func TestNewMaasClientFromProfileUsesBareBaseURL(t *testing.T) {
	t.Parallel()

	client, err := NewMaasClientFromProfile(config.MaasConfig{BaseURL: "http://localhost:1234"}, "")
	if err != nil {
		t.Fatalf("NewMaasClientFromProfile(bare base_url) error = %v, want nil", err)
	}
	if client == nil {
		t.Fatal("client = nil, want a client built from the top-level base_url")
	}
}
