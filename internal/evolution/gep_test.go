package evolution

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/memory"
	"github.com/stardust/legion-agent/internal/quality"
)

func TestGepCycleRunsSixStagesAndSolidifiesGeneAndCapsule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := memory.NewCapabilityMemoryStore()
	audit := adapter.NewHashChainAuditLog()
	cycle := NewGepCycle(GepCycleConfig{
		Extractor:       NewSignalExtractor(),
		CapabilityStore: store,
		EventLog:        NewEvolutionEventLog(audit),
	})

	result, err := cycle.Run(ctx, ExtractionInput{
		AgentID: "agent-1",
		Task: domain.Task{
			ID:     "task-1",
			Input:  "fix repeated go test timeout",
			Status: domain.TaskFailed,
		},
		ToolResults: []domain.ToolResult{
			{CallID: "go-test", Success: false, Error: "command timed out"},
		},
		Cycle: 1,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if !result.Solidified {
		t.Fatalf("Run().Solidified = false, want true")
	}
	if result.Gene.ID == "" {
		t.Fatalf("Run().Gene.ID = empty, want generated gene")
	}
	if result.Capsule.ID == "" {
		t.Fatalf("Run().Capsule.ID = empty, want generated capsule")
	}
	if len(result.Events) != 6 {
		t.Fatalf("Run().Events len = %d, want 6", len(result.Events))
	}
	wantStages := []EvolutionStage{StageScan, StageSignal, StageIntent, StageMutate, StageValidate, StageSolidify}
	for idx, want := range wantStages {
		if result.Events[idx].Stage != want {
			t.Errorf("Run().Events[%d].Stage = %s, want %s", idx, result.Events[idx].Stage, want)
		}
	}
	hits, err := store.SearchGenes(ctx, memory.CapabilityQuery{Text: "go timeout", Tags: []string{"failure"}, TopK: 1})
	if err != nil {
		t.Fatalf("SearchGenes() error = %v, want nil", err)
	}
	if len(hits) != 1 {
		t.Fatalf("SearchGenes() len = %d, want 1", len(hits))
	}
	capsules, err := store.SearchCapsules(ctx, memory.CapabilityQuery{Text: "go timeout", Tags: []string{"failure"}, TopK: 1})
	if err != nil {
		t.Fatalf("SearchCapsules() error = %v, want nil", err)
	}
	if len(capsules) != 1 {
		t.Fatalf("SearchCapsules() len = %d, want 1", len(capsules))
	}
	auditEvents, err := audit.Events()
	if err != nil {
		t.Fatalf("audit.Events() error = %v, want nil", err)
	}
	if len(auditEvents) != 6 {
		t.Fatalf("audit.Events() len = %d, want 6", len(auditEvents))
	}
	if err := audit.VerifyChain(ctx); err != nil {
		t.Fatalf("VerifyChain() error = %v, want nil", err)
	}
}

func TestGepCycleDoesNotSolidifyCriticalSecuritySignals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := memory.NewCapabilityMemoryStore()
	audit := adapter.NewHashChainAuditLog()
	cycle := NewGepCycle(GepCycleConfig{
		Extractor:       NewSignalExtractor(),
		CapabilityStore: store,
		EventLog:        NewEvolutionEventLog(audit),
	})

	result, err := cycle.Run(ctx, ExtractionInput{
		AgentID: "agent-1",
		Task:    domain.Task{ID: "task-critical", Input: "run dangerous command", Status: domain.TaskFailed},
		Events: []domain.RuntimeEvent{
			{Type: "tool.denied", Message: "permission denied for dangerous tool"},
		},
		Cycle: 1,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if result.Solidified {
		t.Fatalf("Run().Solidified = true, want false for critical signal")
	}
	if result.Decision != DecisionNeedsReview {
		t.Fatalf("Run().Decision = %s, want %s", result.Decision, DecisionNeedsReview)
	}
	hits, err := store.SearchGenes(ctx, memory.CapabilityQuery{Text: "dangerous command", TopK: 1})
	if err != nil {
		t.Fatalf("SearchGenes() error = %v, want nil", err)
	}
	if len(hits) != 0 {
		t.Fatalf("SearchGenes() len = %d, want 0", len(hits))
	}
	if len(result.Events) != 6 {
		t.Fatalf("Run().Events len = %d, want 6", len(result.Events))
	}
	if result.Events[5].Decision != DecisionNeedsReview {
		t.Fatalf("Run().Events[5].Decision = %s, want %s", result.Events[5].Decision, DecisionNeedsReview)
	}
}

func TestGepCycleRecordsValidationFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := memory.NewCapabilityMemoryStore()
	audit := adapter.NewHashChainAuditLog()
	cycle := NewGepCycle(GepCycleConfig{
		Extractor: NewSignalExtractor(),
		Distiller: DistillationOperatorFunc(func(_ context.Context, _ ChangeIntent) (memory.Gene, error) {
			return memory.Gene{
				ID:         "invalid-gene",
				Version:    "1.0.0",
				Status:     memory.GeneStatusDraft,
				Match:      "invalid",
				UseWhen:    "invalid",
				Plan:       "invalid",
				Avoid:      "",
				Validation: "run tests",
			}, nil
		}),
		CapabilityStore: store,
		EventLog:        NewEvolutionEventLog(audit),
	})

	result, err := cycle.Run(ctx, ExtractionInput{
		AgentID: "agent-1",
		Task:    domain.Task{ID: "task-loop", Input: "go loop", Status: domain.TaskSuspended},
		Eval:    quality.EvalResult{Status: quality.EvalHardLoop, Reason: "repeated trace item"},
		Cycle:   1,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if result.Solidified {
		t.Fatalf("Run().Solidified = true, want false")
	}
	if result.Decision != DecisionValidationFailed {
		t.Fatalf("Run().Decision = %s, want %s", result.Decision, DecisionValidationFailed)
	}
	if result.Events[4].Decision != DecisionValidationFailed {
		t.Fatalf("Run().Events[4].Decision = %s, want %s", result.Events[4].Decision, DecisionValidationFailed)
	}
	auditEvents, err := audit.Events()
	if err != nil {
		t.Fatalf("audit.Events() error = %v, want nil", err)
	}
	if len(auditEvents) != 6 {
		t.Fatalf("audit.Events() len = %d, want 6", len(auditEvents))
	}
}
