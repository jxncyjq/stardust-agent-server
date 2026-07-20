package quality

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
)

func TestEvolutionEvaluatorReportsFiveDimensions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	evaluator := NewEvolutionEvaluator(EvolutionEvaluatorConfig{})
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)

	report, err := evaluator.Evaluate(ctx, []EvolutionSample{
		sampleAt(now.AddDate(0, 0, -3), 0.7, 2, 0.55, 120, 0.9),
		sampleAt(now.AddDate(0, 0, -2), 0.8, 4, 0.65, 100, 0.92),
		sampleAt(now.AddDate(0, 0, -1), 0.9, 5, 0.75, 80, 0.95),
	}, EvalResult{Status: EvalNormal})
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}
	if report.AgentID == "" {
		t.Fatalf("Evaluate().AgentID = empty, want populated")
	}
	if report.Dimensions.TaskQuality <= 0 {
		t.Errorf("Evaluate().Dimensions.TaskQuality = %f, want positive", report.Dimensions.TaskQuality)
	}
	if report.Dimensions.LearningVelocity <= 0 {
		t.Errorf("Evaluate().Dimensions.LearningVelocity = %f, want positive", report.Dimensions.LearningVelocity)
	}
	if report.Dimensions.ReuseEffectiveness <= 0 {
		t.Errorf("Evaluate().Dimensions.ReuseEffectiveness = %f, want positive", report.Dimensions.ReuseEffectiveness)
	}
	if report.Dimensions.CostEfficiency <= 0 {
		t.Errorf("Evaluate().Dimensions.CostEfficiency = %f, want positive", report.Dimensions.CostEfficiency)
	}
	if report.Dimensions.Stability <= 0 {
		t.Errorf("Evaluate().Dimensions.Stability = %f, want positive", report.Dimensions.Stability)
	}
	if report.Alert != nil {
		t.Fatalf("Evaluate().Alert = %#v, want nil", report.Alert)
	}
}

func TestEvolutionEvaluatorPublishesDegradationAlertForFourteenDayQualityDrop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bus := adapter.NewMemoryEventBus()
	evaluator := NewEvolutionEvaluator(EvolutionEvaluatorConfig{
		EventBus:             bus,
		QualityDropThreshold: 0.2,
		Now: func() time.Time {
			return time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
		},
	})
	samples := []EvolutionSample{
		sampleAt(time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC), 0.95, 5, 0.9, 80, 0.95),
		sampleAt(time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC), 0.92, 5, 0.88, 90, 0.95),
		sampleAt(time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC), 0.58, 2, 0.45, 140, 0.65),
		sampleAt(time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC), 0.54, 1, 0.4, 160, 0.6),
	}

	report, err := evaluator.Evaluate(ctx, samples, EvalResult{Status: EvalNormal})
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}
	if report.Alert == nil {
		t.Fatalf("Evaluate().Alert = nil, want DegradationAlert")
	}
	if report.Alert.AgentID != "agent-1" {
		t.Errorf("Evaluate().Alert.AgentID = %q, want agent-1", report.Alert.AgentID)
	}
	if report.Alert.QualityDrop < 0.2 {
		t.Errorf("Evaluate().Alert.QualityDrop = %f, want >= 0.2", report.Alert.QualityDrop)
	}
	events, err := bus.Events()
	if err != nil {
		t.Fatalf("EventBus.Events() error = %v, want nil", err)
	}
	if len(events) != 1 {
		t.Fatalf("EventBus.Events() len = %d, want 1", len(events))
	}
	if events[0].Type != RuntimeEventDegradationAlert {
		t.Fatalf("EventBus.Events()[0].Type = %q, want %q", events[0].Type, RuntimeEventDegradationAlert)
	}
	if !strings.Contains(events[0].Message, "agent-1") {
		t.Fatalf("EventBus.Events()[0].Message = %q, want agent id", events[0].Message)
	}
}

func TestEvolutionEvaluatorDoesNotAlertWhenEvalFindsExternalFault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bus := adapter.NewMemoryEventBus()
	evaluator := NewEvolutionEvaluator(EvolutionEvaluatorConfig{
		EventBus:             bus,
		QualityDropThreshold: 0.2,
		Now: func() time.Time {
			return time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
		},
	})
	samples := []EvolutionSample{
		sampleAt(time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC), 0.9, 5, 0.9, 80, 0.95),
		sampleAt(time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC), 0.5, 1, 0.4, 160, 0.6),
	}

	report, err := evaluator.Evaluate(ctx, samples, EvalResult{
		Status: EvalComponentDegraded,
		Reason: "maas gateway success rate below threshold",
	})
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}
	if report.Alert != nil {
		t.Fatalf("Evaluate().Alert = %#v, want nil when external fault exists", report.Alert)
	}
	events, err := bus.Events()
	if err != nil {
		t.Fatalf("EventBus.Events() error = %v, want nil", err)
	}
	if len(events) != 0 {
		t.Fatalf("EventBus.Events() len = %d, want 0", len(events))
	}
}

func TestEvolutionEvaluatorNeedsWindowSamplesBeforeAlert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	evaluator := NewEvolutionEvaluator(EvolutionEvaluatorConfig{
		QualityDropThreshold: 0.2,
		Now: func() time.Time {
			return time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
		},
	})
	samples := []EvolutionSample{
		sampleAt(time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC), 0.4, 1, 0.3, 200, 0.6),
	}

	report, err := evaluator.Evaluate(ctx, samples, EvalResult{Status: EvalNormal})
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}
	if report.Alert != nil {
		t.Fatalf("Evaluate().Alert = %#v, want nil with insufficient window history", report.Alert)
	}
}

func sampleAt(at time.Time, quality float64, velocity float64, reuse float64, cost float64, stability float64) EvolutionSample {
	return EvolutionSample{
		AgentID:            "agent-1",
		TaskQuality:        quality,
		LearningVelocity:   velocity,
		ReuseEffectiveness: reuse,
		CostPerTask:        cost,
		Stability:          stability,
		ObservedAt:         at,
	}
}
