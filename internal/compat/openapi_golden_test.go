package compat

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stardust/legion-agent/internal/server"
)

func TestOpenAPIGolden(t *testing.T) {
	got, err := json.MarshalIndent(server.BuildOpenAPISpec(), "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent(BuildOpenAPISpec()) error = %v, want nil", err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile("testdata/openapi-agent.json")
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v, want nil", "testdata/openapi-agent.json", err)
	}
	if string(got) != string(want) {
		t.Errorf("BuildOpenAPISpec() golden mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}
