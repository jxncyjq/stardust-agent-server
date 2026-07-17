package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadResolvesEnvSecretsAndValidates(t *testing.T) {
	t.Setenv("TEST_ADMIN", "admintok")
	t.Setenv("TEST_TG", "bottok")
	path := writeConfig(t, `{
		"core":     {"base_url":"http://x:8080","token_env":"TEST_ADMIN"},
		"identity": {"agent_id":"im-agent","company_id":"default"},
		"binding":  {"sqlite_path":"g.db"},
		"delivery": {"retries":3,"backoff_ms":500},
		"platforms": {"telegram":{"enabled":true,"token_env":"TEST_TG","poll_timeout_s":30}}
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.Core.Token != "admintok" || cfg.Platforms["telegram"].Token != "bottok" {
		t.Fatalf("secrets not resolved from env: core=%q tg=%q", cfg.Core.Token, cfg.Platforms["telegram"].Token)
	}
	if cfg.Identity.AgentID != "im-agent" {
		t.Fatalf("identity.agent_id = %q, want im-agent", cfg.Identity.AgentID)
	}
}

func TestLoadFailsLoudOnMissingEnvSecret(t *testing.T) {
	path := writeConfig(t, `{
		"core":{"base_url":"http://x","token_env":"MISSING_ENV"},
		"identity":{"agent_id":"a","company_id":"c"},
		"binding":{"sqlite_path":"g.db"},
		"platforms":{}
	}`)
	if _, err := Load(path); err == nil {
		t.Fatalf("Load(missing env) error = nil, want non-nil")
	}
}

func TestLoadFailsLoudOnMissingCoreURL(t *testing.T) {
	t.Setenv("TEST_ADMIN", "x")
	path := writeConfig(t, `{
		"core":{"base_url":"","token_env":"TEST_ADMIN"},
		"identity":{"agent_id":"a","company_id":"c"},
		"binding":{"sqlite_path":"g.db"},
		"platforms":{}
	}`)
	if _, err := Load(path); err == nil {
		t.Fatalf("Load(no core url) error = nil, want non-nil")
	}
}
