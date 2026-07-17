package cli

import (
	"os"
	"strings"
	"testing"
)

func TestGitHubActionsWorkflowRunsReleaseChecks(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/agent-ci.yml")
	if err != nil {
		t.Fatalf("ReadFile(agent-ci.yml) error = %v", err)
	}

	workflow := string(data)
	wantFragments := []string{
		"name: Legion Agent CI",
		"go-version-file: go.mod",
		"cache-dependency-path: go.sum",
		"go test ./...",
		"go vet ./...",
		"go build ./cmd",
		"scripts/smoke.ps1",
		"go test ./internal/compat -count=1",
		"scripts/release.ps1",
		"0.1.0-ci",
	}
	for _, want := range wantFragments {
		if !strings.Contains(workflow, want) {
			t.Errorf("agent-ci.yml missing %q", want)
		}
	}
}
