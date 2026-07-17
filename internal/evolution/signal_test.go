package evolution

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/quality"
)

func TestSignalExtractorExtractsSuccessFailureHardLoopAndFeedback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	extractor := NewSignalExtractor()

	success, err := extractor.Extract(ctx, ExtractionInput{
		Task:  domain.Task{ID: "task-success", Status: domain.TaskDone},
		Run:   domain.TaskRun{ID: "run-success", TaskID: "task-success", Result: "completed"},
		Cycle: 1,
	})
	if err != nil {
		t.Fatalf("Extract(success) error = %v, want nil", err)
	}
	if !hasSignal(success, SignalSuccess) {
		t.Fatalf("Extract(success) missing %s: %#v", SignalSuccess, success)
	}

	failure, err := extractor.Extract(ctx, ExtractionInput{
		Task: domain.Task{ID: "task-fail", Status: domain.TaskFailed},
		ToolResults: []domain.ToolResult{
			{CallID: "tool-1", Success: false, Error: "command timed out"},
		},
		Cycle: 2,
	})
	if err != nil {
		t.Fatalf("Extract(failure) error = %v, want nil", err)
	}
	if !hasSignal(failure, SignalFailure) {
		t.Fatalf("Extract(failure) missing %s: %#v", SignalFailure, failure)
	}

	hardLoop, err := extractor.Extract(ctx, ExtractionInput{
		Task: domain.Task{ID: "task-loop", Status: domain.TaskSuspended},
		Eval: quality.EvalResult{Status: quality.EvalHardLoop, Reason: "repeated trace item"},
		Feedback: []Feedback{
			{Author: "reviewer", Rating: -1, Text: "Repeated the same tool call and needs a better stopping rule."},
		},
		Cycle: 3,
	})
	if err != nil {
		t.Fatalf("Extract(hardLoop) error = %v, want nil", err)
	}
	if !hasSignal(hardLoop, SignalHardLoopFailure) {
		t.Fatalf("Extract(hardLoop) missing %s: %#v", SignalHardLoopFailure, hardLoop)
	}
	if !hasSignal(hardLoop, SignalFeedbackNegative) {
		t.Fatalf("Extract(hardLoop) missing %s: %#v", SignalFeedbackNegative, hardLoop)
	}
	assertSignalsHaveEvidence(t, hardLoop)
}

func TestSignalExtractorSuppressesRepeatedLowValueSignalsWithinWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	extractor := NewSignalExtractor()
	input := ExtractionInput{
		Task: domain.Task{ID: "task-fail", Status: domain.TaskFailed},
		ToolResults: []domain.ToolResult{
			{CallID: "tool-1", Success: false, Error: "temporary timeout"},
		},
	}

	first, err := extractor.Extract(ctx, withCycle(input, 1))
	if err != nil {
		t.Fatalf("Extract(cycle 1) error = %v, want nil", err)
	}
	second, err := extractor.Extract(ctx, withCycle(input, 2))
	if err != nil {
		t.Fatalf("Extract(cycle 2) error = %v, want nil", err)
	}
	third, err := extractor.Extract(ctx, withCycle(input, 3))
	if err != nil {
		t.Fatalf("Extract(cycle 3) error = %v, want nil", err)
	}
	if !hasSignal(first, SignalFailure) || !hasSignal(second, SignalFailure) {
		t.Fatalf("Extract() first two cycles should include failure: first=%#v second=%#v", first, second)
	}
	if hasSignal(third, SignalFailure) {
		t.Fatalf("Extract(cycle 3) included suppressed failure: %#v", third)
	}

	afterWindow, err := extractor.Extract(ctx, withCycle(input, 12))
	if err != nil {
		t.Fatalf("Extract(cycle 12) error = %v, want nil", err)
	}
	if !hasSignal(afterWindow, SignalFailure) {
		t.Fatalf("Extract(cycle 12) missing failure after suppression window moved: %#v", afterWindow)
	}
}

func TestSignalExtractorNeverSuppressesCriticalSignals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	extractor := NewSignalExtractor()
	input := ExtractionInput{
		Task: domain.Task{ID: "task-critical", Status: domain.TaskFailed},
		Events: []domain.RuntimeEvent{
			{Type: "tool.denied", Message: "permission denied for dangerous tool"},
			{Type: "security", Message: "potential secret exposure: api key leaked"},
		},
		Eval: quality.EvalResult{Status: quality.EvalHardLoop, Reason: "repeated trace item"},
	}

	for cycle := 1; cycle <= 4; cycle++ {
		signals, err := extractor.Extract(ctx, withCycle(input, cycle))
		if err != nil {
			t.Fatalf("Extract(cycle %d) error = %v, want nil", cycle, err)
		}
		for _, kind := range []SignalKind{SignalHardLoopFailure, SignalPermissionViolation, SignalSecretExposure} {
			if !hasSignal(signals, kind) {
				t.Fatalf("Extract(cycle %d) missing critical signal %s: %#v", cycle, kind, signals)
			}
		}
	}
}

func hasSignal(signals []LearningSignal, kind SignalKind) bool {
	for _, signal := range signals {
		if signal.Kind == kind {
			return true
		}
	}
	return false
}

func assertSignalsHaveEvidence(t *testing.T, signals []LearningSignal) {
	t.Helper()
	for _, signal := range signals {
		if signal.Source == "" {
			t.Errorf("LearningSignal{%s}.Source = empty, want source", signal.Kind)
		}
		if signal.Evidence == "" {
			t.Errorf("LearningSignal{%s}.Evidence = empty, want evidence", signal.Kind)
		}
		if signal.Confidence <= 0 {
			t.Errorf("LearningSignal{%s}.Confidence = %f, want positive", signal.Kind, signal.Confidence)
		}
	}
}

func withCycle(input ExtractionInput, cycle int) ExtractionInput {
	input.Cycle = cycle
	return input
}
