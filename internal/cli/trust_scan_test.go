package cli

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/quality"
)

// TestTrustScoreScanJobLowersScoreOnViolation verifies the L6 trust wiring: a
// permission-violation learning event on the bus is translated into a security
// event that lowers the agent's effective trust score below the default.
func TestTrustScoreScanJobLowersScoreOnViolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	events := adapter.NewMemoryEventBus()
	manager := quality.NewTrustScoreManager()
	job := newTrustScoreScanJob(events, manager)

	at := time.Now()
	baseline, err := manager.EffectiveScore(ctx, "default-agent", at)
	if err != nil {
		t.Fatalf("baseline score: %v", err)
	}

	if err := events.Publish(ctx, evolution.NewLearningRuntimeEvent(evolution.LearningEvent{
		AgentID:     "default-agent",
		TaskID:      "task-violation-1",
		Signal:      evolution.SignalPermissionViolation,
		Reason:      "permission denied",
		PublishedAt: at,
	})); err != nil {
		t.Fatalf("publish learning event: %v", err)
	}

	if err := job(ctx); err != nil {
		t.Fatalf("trust score scan job: %v", err)
	}

	after, err := manager.EffectiveScore(ctx, "default-agent", time.Now())
	if err != nil {
		t.Fatalf("score after violation: %v", err)
	}
	if after >= baseline {
		t.Fatalf("EffectiveScore after permission violation = %v, want < baseline %v", after, baseline)
	}
}

// TestTrustScoreScanJobIgnoresNeutralSignals asserts that non-trust signals do
// not move the score: a plain failure signal carries no trust consequence.
func TestTrustScoreScanJobIgnoresNeutralSignals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	events := adapter.NewMemoryEventBus()
	manager := quality.NewTrustScoreManager()
	job := newTrustScoreScanJob(events, manager)

	at := time.Now()
	baseline, err := manager.EffectiveScore(ctx, "default-agent", at)
	if err != nil {
		t.Fatalf("baseline score: %v", err)
	}

	if err := events.Publish(ctx, evolution.NewLearningRuntimeEvent(evolution.LearningEvent{
		AgentID:     "default-agent",
		TaskID:      "task-fail-1",
		Signal:      evolution.SignalFailure,
		Reason:      "tool_error",
		PublishedAt: at,
	})); err != nil {
		t.Fatalf("publish learning event: %v", err)
	}

	if err := job(ctx); err != nil {
		t.Fatalf("trust score scan job: %v", err)
	}

	after, err := manager.EffectiveScore(ctx, "default-agent", time.Now())
	if err != nil {
		t.Fatalf("score after neutral signal: %v", err)
	}
	if after != baseline {
		t.Fatalf("EffectiveScore after neutral failure = %v, want unchanged baseline %v", after, baseline)
	}
}

// TestTrustScoreScanJobFailLoud asserts the fail-loud contract: a cancelled
// context surfaces as an error rather than silently doing nothing.
func TestTrustScoreScanJobFailLoud(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	job := newTrustScoreScanJob(adapter.NewMemoryEventBus(), quality.NewTrustScoreManager())
	if err := job(ctx); err == nil {
		t.Fatal("trust score scan job with cancelled context = nil error, want fail-loud error")
	}
}
