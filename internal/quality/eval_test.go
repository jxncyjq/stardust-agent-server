package quality

import (
	"context"
	"testing"
)

func TestEvalEngineDetectsHardLoop(t *testing.T) {
	t.Parallel()

	engine := NewEvalEngine(3)
	result, err := engine.EvaluateTrace(context.Background(), []string{
		"retry same plan",
		"retry same plan",
		"retry same plan",
	})
	if err != nil {
		t.Fatalf("EvaluateTrace() error = %v, want nil", err)
	}
	if result.Status != EvalHardLoop {
		t.Errorf("EvaluateTrace() status = %q, want %q", result.Status, EvalHardLoop)
	}
}

func TestEvalEngineAllowsProgressingTrace(t *testing.T) {
	t.Parallel()

	engine := NewEvalEngine(3)
	result, err := engine.EvaluateTrace(context.Background(), []string{
		"read task",
		"call model",
		"write result",
	})
	if err != nil {
		t.Fatalf("EvaluateTrace() error = %v, want nil", err)
	}
	if result.Status != EvalNormal {
		t.Errorf("EvaluateTrace() status = %q, want %q", result.Status, EvalNormal)
	}
}
