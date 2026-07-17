.PHONY: test vet build demo-smoke prompt-smoke workflow-smoke storage-smoke smoke

test:
	go test ./...

vet:
	go vet ./...

build:
	go build ./cmd/agent

demo-smoke:
	go run ./cmd/agent -- run --demo --plain

prompt-smoke:
	go run ./cmd/agent -- run --plain --prompt "Summarize Legion Agent"

workflow-smoke:
	go test ./internal/workflow -run TestEngineSubworkflowRunsNestedDefinition

storage-smoke:
	go test ./internal/storage -run TestSQLiteRepositoryRecoversCrossProcessState

smoke:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/smoke.ps1
