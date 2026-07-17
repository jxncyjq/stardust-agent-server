package observability

import (
	"strings"
	"testing"
	"time"
)

func TestDiagnosticsSnapshotRedactsSecretsAndPrompt(t *testing.T) {
	startedAt := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	now := startedAt.Add(5 * time.Second)
	diagnostics := NewDiagnostics(DiagnosticsConfig{
		Version:             "dev",
		Commit:              "abc123",
		BuildTime:           "2026-05-14T09:00:00Z",
		StartedAt:           startedAt,
		Now:                 func() time.Time { return now },
		StorageDriver:       "sqlite",
		StoragePath:         "C:/very/secret/agent.db",
		MaasBaseURL:         "https://maas.example.test",
		MaasAPIKey:          "maas-secret",
		AdminToken:          "admin-secret",
		RuntimeDemoResponse: "complete prompt should not appear",
		SchedulerEnabled:    true,
		SchedulerRunning:    true,
		Metrics:             NewMetricsRecorder(func() time.Time { return startedAt }),
	})

	snapshot := diagnostics.Snapshot()
	if snapshot.Version != "dev" || snapshot.Commit != "abc123" || snapshot.BuildTime != "2026-05-14T09:00:00Z" {
		t.Fatalf("Snapshot() version fields = %#v, want injected values", snapshot)
	}
	if snapshot.UptimeSeconds != 5 {
		t.Fatalf("Snapshot().UptimeSeconds = %d, want 5", snapshot.UptimeSeconds)
	}
	if snapshot.Config.MaasAPIKey != "[redacted]" || snapshot.Config.AdminToken != "[redacted]" {
		t.Fatalf("Snapshot().Config secrets = %#v, want redacted secrets", snapshot.Config)
	}
	if snapshot.Config.RuntimeDemoResponse != "[redacted]" {
		t.Fatalf("Snapshot().Config.RuntimeDemoResponse = %q, want redacted", snapshot.Config.RuntimeDemoResponse)
	}
	if snapshot.Config.StoragePathHash == "" || strings.Contains(snapshot.Config.StoragePathHash, "agent.db") {
		t.Fatalf("Snapshot().Config.StoragePathHash = %q, want non-empty path hash", snapshot.Config.StoragePathHash)
	}
}
