package compat

import (
	"encoding/json"
	"testing"

	"github.com/stardust/legion-agent/internal/server"
)

func TestOpenAPIErrorContractGolden(t *testing.T) {
	got, err := json.MarshalIndent(server.BuildOpenAPISpec().Components.Schemas["ErrorResponse"], "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent(ErrorResponse) error = %v, want nil", err)
	}
	got = append(got, '\n')
	assertGolden(t, "testdata/openapi-error-response.json", got)
}
