package quality

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/observability"
)

func TestEvalHistoryTrend(t *testing.T) {
	ctx := context.Background()
	store := NewQualityHistoryStore()
	base := time.Date(2026, 5, 15, 9, 0, 0, 0, time.UTC)

	records := []EvalRunRecord{
		{
			ID:        "eval-1",
			AgentID:   "agent-1",
			TaskID:    "task-1",
			Component: "planner",
			Status:    EvalNormal,
			Score:     0.92,
			CreatedAt: base,
		},
		{
			ID:        "eval-2",
			AgentID:   "agent-1",
			TaskID:    "task-2",
			Component: "planner",
			Status:    EvalComponentDegraded,
			Reason:    "success rate dropped",
			Score:     0.58,
			CreatedAt: base.Add(time.Minute),
		},
		{
			ID:        "eval-3",
			AgentID:   "agent-2",
			TaskID:    "task-3",
			Component: "executor",
			Status:    EvalNormal,
			Score:     0.99,
			CreatedAt: base.Add(2 * time.Minute),
		},
	}
	for _, record := range records {
		if err := store.AppendEvalRun(ctx, record); err != nil {
			t.Fatalf("append eval run: %v", err)
		}
	}

	trend, err := store.EvalTrend(ctx, TrendQuery{AgentID: "agent-1", Component: "planner"})
	if err != nil {
		t.Fatalf("query eval trend: %v", err)
	}
	if trend.Total != 2 {
		t.Fatalf("trend total = %d, want 2", trend.Total)
	}
	if got := trend.ByStatus[EvalComponentDegraded]; got != 1 {
		t.Fatalf("component degraded count = %d, want 1", got)
	}
	if trend.Latest.ID != "eval-2" {
		t.Fatalf("latest eval = %q, want eval-2", trend.Latest.ID)
	}
	if trend.AverageScore != 0.75 {
		t.Fatalf("average score = %.2f, want 0.75", trend.AverageScore)
	}
}

func TestTrustScoreHistory(t *testing.T) {
	ctx := context.Background()
	manager := NewTrustScoreManager()
	base := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)

	if err := manager.LogSecurityEvent(ctx, SecurityEvent{
		AgentID: "agent-1",
		Type:    SecurityEventSafeCompletion,
		At:      base,
	}); err != nil {
		t.Fatalf("log safe event: %v", err)
	}
	first, err := manager.Snapshot(ctx, "agent-1", base)
	if err != nil {
		t.Fatalf("snapshot first: %v", err)
	}

	for _, eventType := range []SecurityEventType{SecurityEventHardLoop, SecurityEventPermissionDenied} {
		if err := manager.LogSecurityEvent(ctx, SecurityEvent{
			AgentID: "agent-1",
			Type:    eventType,
			At:      base.Add(time.Minute),
		}); err != nil {
			t.Fatalf("log security event %q: %v", eventType, err)
		}
	}
	second, err := manager.Snapshot(ctx, "agent-1", base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("snapshot second: %v", err)
	}

	history, err := manager.History(ctx, "agent-1")
	if err != nil {
		t.Fatalf("trust history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history length = %d, want 2", len(history))
	}
	if first.Score <= second.Score {
		t.Fatalf("trust score should decline after security events: first %.2f second %.2f", first.Score, second.Score)
	}
	if history[1].Decision != TrustDecisionCautious {
		t.Fatalf("latest decision = %q, want cautious", history[1].Decision)
	}
}

func TestDiagnosticsIncludesQualitySummary(t *testing.T) {
	ctx := context.Background()
	store := NewQualityHistoryStore()
	at := time.Date(2026, 5, 15, 11, 0, 0, 0, time.UTC)
	const secretPrompt = "secret prompt: rotate root key"

	if err := store.AppendEvalRun(ctx, EvalRunRecord{
		ID:        "eval-1",
		AgentID:   "agent-1",
		TaskID:    "task-1",
		Component: "planner",
		Status:    EvalOutputIssue,
		Reason:    "output issue summary",
		Score:     0.4,
		CreatedAt: at,
	}); err != nil {
		t.Fatalf("append eval run: %v", err)
	}
	if err := store.AppendTrustSnapshot(ctx, TrustScoreSnapshot{
		AgentID:   "agent-1",
		Score:     0.45,
		Decision:  TrustDecisionCautious,
		Reason:    "trust summary",
		CreatedAt: at,
	}); err != nil {
		t.Fatalf("append trust snapshot: %v", err)
	}
	if err := store.AppendDegradationDecision(ctx, DegradationDecision{
		AgentID:     "agent-1",
		Component:   "planner",
		Decision:    "quarantine",
		Reason:      "quality degraded; prompt redacted",
		QualityDrop: 0.35,
		CreatedAt:   at,
	}); err != nil {
		t.Fatalf("append degradation decision: %v", err)
	}

	summary, err := store.Summary(ctx, TrendQuery{AgentID: "agent-1"})
	if err != nil {
		t.Fatalf("quality summary: %v", err)
	}
	diagnostics := observability.NewDiagnostics(observability.DiagnosticsConfig{
		Now: func() time.Time { return at },
		Quality: observability.QualitySnapshot{
			EvalRuns:             summary.EvalRuns,
			LatestEvalStatus:     string(summary.LatestEvalStatus),
			TrustSnapshots:       summary.TrustSnapshots,
			LatestTrustDecision:  string(summary.LatestTrustDecision),
			DegradationDecisions: summary.DegradationDecisions,
		},
		RuntimeDemoResponse: secretPrompt,
	})
	snapshot := diagnostics.Snapshot()
	if snapshot.Quality.EvalRuns != 1 {
		t.Fatalf("diagnostics eval runs = %d, want 1", snapshot.Quality.EvalRuns)
	}
	if snapshot.Quality.LatestTrustDecision != string(TrustDecisionCautious) {
		t.Fatalf("diagnostics latest trust decision = %q, want cautious", snapshot.Quality.LatestTrustDecision)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal diagnostics: %v", err)
	}
	if strings.Contains(string(raw), secretPrompt) {
		t.Fatalf("diagnostics leaked prompt content: %s", raw)
	}
}
