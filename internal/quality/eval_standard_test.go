package quality

import (
	"context"
	"testing"
)

func TestEvalEngineStandardEvaluatesFourLayers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine := NewEvalEngine(3)

	result, err := engine.EvaluateBehavior(ctx, BehaviorReport{
		Output: OutputReport{
			Text:         "",
			RequiredDone: true,
		},
		Trace: []string{
			"retry plan",
			"retry plan",
		},
		Components: []ComponentMetric{
			{Name: "tool.echo", SuccessRate: 0.40},
		},
		Drift: DriftReport{
			Score: 0.85,
		},
	})
	if err != nil {
		t.Fatalf("EvaluateBehavior() error = %v, want nil", err)
	}

	for _, want := range []EvalStatus{
		EvalOutputIssue,
		EvalSoftLoop,
		EvalComponentDegraded,
		EvalDriftDetected,
	} {
		if !hasFinding(result, want) {
			t.Errorf("EvaluateBehavior() findings missing %q: %#v", want, result.Findings)
		}
	}
}

func TestEvalEngineStandardReturnsNormalWhenHealthy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine := NewEvalEngine(3)

	result, err := engine.EvaluateBehavior(ctx, BehaviorReport{
		Output: OutputReport{
			Text:         "task completed",
			RequiredDone: true,
		},
		Trace: []string{
			"read task",
			"call model",
			"write result",
		},
		Components: []ComponentMetric{
			{Name: "tool.echo", SuccessRate: 0.99},
		},
		Drift: DriftReport{
			Score: 0.10,
		},
	})
	if err != nil {
		t.Fatalf("EvaluateBehavior() error = %v, want nil", err)
	}
	if result.Status != EvalNormal {
		t.Fatalf("EvaluateBehavior() status = %q, want %q", result.Status, EvalNormal)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("EvaluateBehavior() findings = %#v, want empty", result.Findings)
	}
}

func TestEvalEngineStandardEscalatesHardLoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine := NewEvalEngine(3)

	result, err := engine.EvaluateBehavior(ctx, BehaviorReport{
		Output: OutputReport{
			Text: "still looping",
		},
		Trace: []string{
			"retry",
			"retry",
			"retry",
		},
	})
	if err != nil {
		t.Fatalf("EvaluateBehavior() error = %v, want nil", err)
	}
	if result.Status != EvalHardLoop {
		t.Fatalf("EvaluateBehavior() status = %q, want %q", result.Status, EvalHardLoop)
	}
}

func hasFinding(result EvalResult, status EvalStatus) bool {
	for _, finding := range result.Findings {
		if finding.Status == status {
			return true
		}
	}
	return false
}
