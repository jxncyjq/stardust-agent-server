package cli

import (
	"os"
	"strings"
	"testing"
)

func TestMakefileDefinesEndToEndSmokeTargets(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("ReadFile(../../Makefile) error = %v, want nil", err)
	}
	makefile := string(data)
	for _, target := range []string{
		"smoke:",
		"demo-smoke:",
		"prompt-smoke:",
		"workflow-smoke:",
		"storage-smoke:",
	} {
		if !strings.Contains(makefile, target) {
			t.Fatalf("Makefile missing target %q", target)
		}
	}
}

func TestPowerShellSmokeScriptExists(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../scripts/smoke.ps1")
	if err != nil {
		t.Fatalf("ReadFile(../../scripts/smoke.ps1) error = %v, want nil", err)
	}
	script := string(data)
	for _, command := range []string{
		"go run ./cmd/agent -- run --demo --plain",
		"go run ./cmd/agent -- run --plain --prompt",
		"go test ./internal/workflow -run TestEngineSubworkflowRunsNestedDefinition",
		"go test ./internal/storage -run TestSQLiteRepositoryRecoversCrossProcessState",
	} {
		if !strings.Contains(script, command) {
			t.Fatalf("scripts/smoke.ps1 missing command %q", command)
		}
	}
}
