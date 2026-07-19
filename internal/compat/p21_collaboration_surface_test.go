package compat

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

type p21CollaborationSurfaceGolden struct {
	Surfaces []string `json:"surfaces"`
}

func TestP21CollaborationSurfaceGolden(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/p21-collaboration-surfaces.json")
	if err != nil {
		t.Fatalf("ReadFile(p21-collaboration-surfaces.json) error = %v, want nil", err)
	}
	var golden p21CollaborationSurfaceGolden
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatalf("Unmarshal(p21-collaboration-surfaces.json) error = %v, want nil", err)
	}
	required := []string{
		"runtime-routing",
		"session-continuity",
		"taskledger-file-collaboration",
		"agent-message-bus",
		"workflow-result-handoff",
		"http-message-api",
	}
	for _, surface := range required {
		if !containsString(golden.Surfaces, surface) {
			t.Fatalf("P21 collaboration surfaces missing %q: %#v", surface, golden.Surfaces)
		}
	}
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}
