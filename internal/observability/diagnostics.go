package observability

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type DiagnosticsConfig struct {
	Version             string
	Commit              string
	BuildTime           string
	StartedAt           time.Time
	Now                 func() time.Time
	StorageDriver       string
	StoragePath         string
	MaasBaseURL         string
	MaasAPIKey          string
	AdminToken          string
	RuntimeDemoResponse string
	SchedulerEnabled    bool
	SchedulerRunning    bool
	Metrics             *MetricsRecorder
	Quality             QualitySnapshot
}

type Diagnostics struct {
	cfg DiagnosticsConfig
	now func() time.Time
}

type DiagnosticsSnapshot struct {
	Version       string                    `json:"version"`
	Commit        string                    `json:"commit"`
	BuildTime     string                    `json:"build_time"`
	UptimeSeconds int64                     `json:"uptime_seconds"`
	Config        DiagnosticsConfigSnapshot `json:"config"`
	Scheduler     SchedulerSnapshot         `json:"scheduler"`
	Metrics       MetricsSnapshot           `json:"metrics"`
	Quality       QualitySnapshot           `json:"quality"`
}

type DiagnosticsConfigSnapshot struct {
	StorageDriver       string `json:"storage_driver"`
	StoragePathHash     string `json:"storage_path_hash"`
	MaasBaseURL         string `json:"maas_base_url"`
	MaasAPIKey          string `json:"maas_api_key"`
	AdminToken          string `json:"admin_token"`
	RuntimeDemoResponse string `json:"runtime_demo_response"`
}

type SchedulerSnapshot struct {
	Enabled bool `json:"enabled"`
	Running bool `json:"running"`
}

type QualitySnapshot struct {
	EvalRuns             int    `json:"eval_runs"`
	LatestEvalStatus     string `json:"latest_eval_status,omitempty"`
	TrustSnapshots       int    `json:"trust_snapshots"`
	LatestTrustDecision  string `json:"latest_trust_decision,omitempty"`
	DegradationDecisions int    `json:"degradation_decisions"`
}

func NewDiagnostics(cfg DiagnosticsConfig) *Diagnostics {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	if cfg.StartedAt.IsZero() {
		cfg.StartedAt = now()
	}
	return &Diagnostics{cfg: cfg, now: now}
}

func (d *Diagnostics) Snapshot() DiagnosticsSnapshot {
	now := d.now()
	uptime := now.Sub(d.cfg.StartedAt)
	if uptime < 0 {
		uptime = 0
	}
	return DiagnosticsSnapshot{
		Version:       defaultString(d.cfg.Version, "dev"),
		Commit:        defaultString(d.cfg.Commit, "unknown"),
		BuildTime:     defaultString(d.cfg.BuildTime, "unknown"),
		UptimeSeconds: int64(uptime.Seconds()),
		Config: DiagnosticsConfigSnapshot{
			StorageDriver:       d.cfg.StorageDriver,
			StoragePathHash:     hashValue(d.cfg.StoragePath),
			MaasBaseURL:         d.cfg.MaasBaseURL,
			MaasAPIKey:          redact(d.cfg.MaasAPIKey),
			AdminToken:          redact(d.cfg.AdminToken),
			RuntimeDemoResponse: redact(d.cfg.RuntimeDemoResponse),
		},
		Scheduler: SchedulerSnapshot{
			Enabled: d.cfg.SchedulerEnabled,
			Running: d.cfg.SchedulerRunning,
		},
		Metrics: d.metricsSnapshot(),
		Quality: d.cfg.Quality,
	}
}

func (d *Diagnostics) metricsSnapshot() MetricsSnapshot {
	if d.cfg.Metrics == nil {
		return NewMetricsRecorder(func() time.Time { return d.cfg.StartedAt }).Snapshot()
	}
	return d.cfg.Metrics.Snapshot()
}

func redact(value string) string {
	if value == "" {
		return ""
	}
	return "[redacted]"
}

func hashValue(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

func defaultString(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
