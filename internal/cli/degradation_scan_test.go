package cli

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/quality"
)

func publishQualitySignal(t *testing.T, events *adapter.MemoryEventBus, agentID, taskID string, signal evolution.SignalKind, at time.Time) {
	t.Helper()
	if err := events.Publish(context.Background(), evolution.NewLearningRuntimeEvent(evolution.LearningEvent{
		AgentID:     agentID,
		TaskID:      taskID,
		Signal:      signal,
		PublishedAt: at,
	})); err != nil {
		t.Fatalf("publish learning event: %v", err)
	}
}

func countDegradationAlerts(events []domain.RuntimeEvent) int {
	n := 0
	for _, event := range events {
		if event.Type == quality.RuntimeEventDegradationAlert {
			n++
		}
	}
	return n
}

// TestDegradationScanJobPublishesAlert asserts the L6 degradation wiring: when an
// agent's task quality drops from a healthy baseline (before the window) to poor
// recent performance (within the window) past the threshold, the periodic job
// drives the EvolutionEvaluator to publish a degradation alert.
func TestDegradationScanJobPublishesAlert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return base }
	events := adapter.NewMemoryEventBus()

	// Baseline (older than the 14d window): high quality.
	publishQualitySignal(t, events, "default-agent", "old-1", evolution.SignalSuccess, base.Add(-30*24*time.Hour))
	publishQualitySignal(t, events, "default-agent", "old-2", evolution.SignalSuccess, base.Add(-28*24*time.Hour))
	// Current (within the window): poor quality.
	publishQualitySignal(t, events, "default-agent", "new-1", evolution.SignalFailure, base.Add(-2*24*time.Hour))
	publishQualitySignal(t, events, "default-agent", "new-2", evolution.SignalFailure, base.Add(-1*24*time.Hour))

	evaluator := quality.NewEvolutionEvaluator(quality.EvolutionEvaluatorConfig{
		EventBus:             events,
		QualityDropThreshold: 0.2,
		Window:               14 * 24 * time.Hour,
		Now:                  now,
	})
	job := newDegradationScanJob(events, evaluator, time.Hour, now)

	if err := job(ctx); err != nil {
		t.Fatalf("degradation scan job: %v", err)
	}
	if got := countDegradationAlerts(events.Events()); got != 1 {
		t.Fatalf("degradation alerts = %d, want 1", got)
	}
}

// TestDegradationScanJobNoAlertWhenHealthy asserts no false alarm: a consistently
// healthy agent must not trigger a degradation alert.
func TestDegradationScanJobNoAlertWhenHealthy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return base }
	events := adapter.NewMemoryEventBus()

	publishQualitySignal(t, events, "default-agent", "old-1", evolution.SignalSuccess, base.Add(-30*24*time.Hour))
	publishQualitySignal(t, events, "default-agent", "new-1", evolution.SignalSuccess, base.Add(-1*24*time.Hour))

	evaluator := quality.NewEvolutionEvaluator(quality.EvolutionEvaluatorConfig{
		EventBus:             events,
		QualityDropThreshold: 0.2,
		Window:               14 * 24 * time.Hour,
		Now:                  now,
	})
	job := newDegradationScanJob(events, evaluator, time.Hour, now)

	if err := job(ctx); err != nil {
		t.Fatalf("degradation scan job: %v", err)
	}
	if got := countDegradationAlerts(events.Events()); got != 0 {
		t.Fatalf("degradation alerts = %d, want 0 (healthy agent)", got)
	}
}

// TestDegradationScanJobTimeGated asserts the scan only re-evaluates once per
// scan period: a second immediate run (clock unchanged) must not emit another
// alert.
func TestDegradationScanJobTimeGated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return base }
	events := adapter.NewMemoryEventBus()
	publishQualitySignal(t, events, "default-agent", "old-1", evolution.SignalSuccess, base.Add(-30*24*time.Hour))
	publishQualitySignal(t, events, "default-agent", "new-1", evolution.SignalFailure, base.Add(-1*24*time.Hour))

	evaluator := quality.NewEvolutionEvaluator(quality.EvolutionEvaluatorConfig{
		EventBus:             events,
		QualityDropThreshold: 0.2,
		Window:               14 * 24 * time.Hour,
		Now:                  now,
	})
	job := newDegradationScanJob(events, evaluator, time.Hour, now)

	if err := job(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if err := job(ctx); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	// Only the first run (within one scan period) should have evaluated.
	if got := countDegradationAlerts(events.Events()); got != 1 {
		t.Fatalf("degradation alerts = %d, want 1 (time-gated to one eval per period)", got)
	}
}
