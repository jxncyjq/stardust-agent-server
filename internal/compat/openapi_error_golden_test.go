package compat

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stardust/legion-agent/internal/server"
)

func TestOpenAPIErrorContractGolden(t *testing.T) {
	got, err := json.MarshalIndent(server.BuildOpenAPISpec().Components.Schemas["ErrorResponse"], "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent(ErrorResponse) error = %v, want nil", err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile("testdata/openapi-error-response.json")
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v, want nil", "testdata/openapi-error-response.json", err)
	}
	if string(got) != string(want) {
		t.Errorf("BuildOpenAPISpec().Components.Schemas[ErrorResponse] golden mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}
