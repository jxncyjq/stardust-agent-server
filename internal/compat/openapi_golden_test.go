package compat

import (
	"encoding/json"
	"testing"

	"github.com/stardust/legion-agent/internal/server"
)

func TestOpenAPIGolden(t *testing.T) {
	got, err := json.MarshalIndent(server.BuildOpenAPISpec(), "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent(BuildOpenAPISpec()) error = %v, want nil", err)
	}
	got = append(got, '\n')
	assertGolden(t, "testdata/openapi-agent.json", got)
}
