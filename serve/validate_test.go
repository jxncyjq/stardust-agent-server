package serve_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stardust/legion-agent/serve"
)

func TestValidateConfigAcceptsValid(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"storage":{"driver":"memory"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := serve.ValidateConfig(context.Background(), good); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidateAgentConfigAcceptsValid(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "researcher.json")
	body := `{"id":"researcher","role":"researcher","maas_profile":"review","workspace":{"docs_root":"docs"}}`
	if err := os.WriteFile(good, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := serve.ValidateAgentConfig(context.Background(), good); err != nil {
		t.Fatalf("valid agent config rejected: %v", err)
	}
}

func TestValidateAgentConfigRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	// workspace must be an object; a string is a type mismatch json.Unmarshal rejects.
	if err := os.WriteFile(bad, []byte(`{"id":"x","workspace":"nope"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := serve.ValidateAgentConfig(context.Background(), bad); err == nil {
		t.Fatal("malformed agent config accepted; want error")
	}
}

func TestValidateAgentConfigRejectsMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	if err := serve.ValidateAgentConfig(context.Background(), missing); err == nil {
		t.Fatal("missing agent config accepted; want error")
	}
}

func TestValidateConfigRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	// storage must be an object; a string is a type mismatch json.Unmarshal rejects.
	if err := os.WriteFile(bad, []byte(`{"storage":"nope"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := serve.ValidateConfig(context.Background(), bad); err == nil {
		t.Fatal("malformed config accepted; want error")
	}
}
