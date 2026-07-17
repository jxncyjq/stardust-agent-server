# stardust-agent

Legion Agent is the standalone Go implementation of the Agent Engine.

## Quick Start

```text
go test ./...
go run ./cmd/agent -- run --demo --plain
go run ./cmd/agent -- version --plain
```

## Service Mode

```text
go run ./cmd/agent -- serve --config ./configs/local.json --addr :8080
```

Service endpoints include `/healthz`, `/readyz`, `/metrics`, `/debug/diagnostics`, `/v1/tasks`, and `/v1/workflows/waiting`.

## Operations

Operational docs live under `docs/agents/legion-agent` in the parent workspace:

- `configuration.md`
- `http-api.md`
- `storage-ops.md`
- `release.md`
- `operations.md`

## Validation

```text
go test ./...
go vet ./...
go build -o NUL ./cmd/agent
.\scripts\smoke.ps1
```
