$ErrorActionPreference = "Stop"

function Invoke-SmokeStep {
    param(
        [string]$Name,
        [scriptblock]$Command
    )

    Write-Host "==> $Name"
    & $Command
}

Invoke-SmokeStep "demo-smoke" {
    go run ./cmd/agent -- run --demo --plain
}

Invoke-SmokeStep "prompt-smoke" {
    go run ./cmd/agent -- run --plain --prompt "Summarize Legion Agent"
}

Invoke-SmokeStep "workflow-smoke" {
    go test ./internal/workflow -run TestEngineSubworkflowRunsNestedDefinition
}

Invoke-SmokeStep "storage-smoke" {
    go test ./internal/storage -run TestSQLiteRepositoryRecoversCrossProcessState
}
