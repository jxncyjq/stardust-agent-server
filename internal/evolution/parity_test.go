package evolution

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/memory"
)

func TestDistillationGeneSixTuple(t *testing.T) {
	t.Parallel()

	gene, err := DefaultDistillationOperator{}.Distill(context.Background(), ChangeIntent{
		ID:       "intent-go-timeout",
		AgentID:  "agent-1",
		TaskID:   "task-1",
		Focus:    SignalFailure,
		Goal:     "improve handling for go test timeout",
		Evidence: "go test timed out after 30s",
		Tags:     []string{"go", "failure"},
	})
	if err != nil {
		t.Fatalf("Distill(intent-go-timeout) error = %v, want nil", err)
	}

	six := GeneSixTupleFromMemory(gene)
	for name, value := range map[string]string{
		"m":     six.M,
		"u":     six.U,
		"pi":    six.Pi,
		"alpha": six.Alpha,
		"c":     six.C,
		"v":     six.V,
	} {
		if strings.TrimSpace(value) == "" {
			t.Errorf("Distill(intent-go-timeout) six tuple %s = empty, want non-empty", name)
		}
	}
	if gene.SuccessRate <= 0 || gene.SuccessRate > 1 {
		t.Errorf("Distill(intent-go-timeout) confidence = %f, want within (0,1]", gene.SuccessRate)
	}
	wantVersion := ContentAddressedGeneVersion(six)
	if gene.Version != wantVersion {
		t.Errorf("Distill(intent-go-timeout) version = %q, want %q", gene.Version, wantVersion)
	}
}

func TestSolidifyPipelineRequiresValidation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := memory.NewCapabilityMemoryStore()
	events := NewSealedEvolutionEventLog()
	pipeline := NewSolidifyPipeline(SolidifyPipelineConfig{
		CapabilityStore: store,
		EventLog:        events,
		MaxBlastRadius:  2,
	})

	invalid := validSolidifyGene()
	invalid.Validation = ""
	_, err := pipeline.Solidify(ctx, SolidifyRequest{
		CycleID:     "cycle-1",
		AgentID:     "agent-1",
		Gene:        invalid,
		Query:       "go test timeout",
		BlastRadius: 1,
	})
	if !errors.Is(err, ErrValidationRequired) {
		t.Fatalf("Solidify(invalid validation) error = %v, want ErrValidationRequired", err)
	}

	result, err := pipeline.Solidify(ctx, SolidifyRequest{
		CycleID:     "cycle-2",
		AgentID:     "agent-1",
		Gene:        validSolidifyGene(),
		Query:       "go test timeout",
		BlastRadius: 1,
	})
	if err != nil {
		t.Fatalf("Solidify(valid gene) error = %v, want nil", err)
	}
	if !result.Solidified {
		t.Fatalf("Solidify(valid gene).Solidified = false, want true")
	}
	hits, err := store.SearchGenes(ctx, memory.CapabilityQuery{Text: "go test timeout", Tags: []string{"failure"}, TopK: 1})
	if err != nil {
		t.Fatalf("SearchGenes() error = %v, want nil", err)
	}
	if len(hits) != 1 || hits[0].Gene.ID != result.Gene.ID {
		t.Fatalf("SearchGenes() = %#v, want solidified gene %q", hits, result.Gene.ID)
	}
	sealedEvents := events.Events("cycle-2")
	if len(sealedEvents) != 1 || sealedEvents[0].Stage != StageSolidify {
		t.Fatalf("Events(cycle-2) = %#v, want one solidify event", sealedEvents)
	}
}

func TestEvolutionEventSeal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	log := NewSealedEvolutionEventLog()
	event := EvolutionEvent{
		EventID:      "event-1",
		CycleID:      "cycle-1",
		Stage:        StageSolidify,
		AgentID:      "agent-1",
		AssetID:      "gene-1",
		EvidenceHash: "hash-1",
		Decision:     DecisionSolidified,
	}

	sealed, err := log.Append(ctx, event)
	if err != nil {
		t.Fatalf("Append(event-1) error = %v, want nil", err)
	}
	if sealed.Seal == "" {
		t.Fatalf("Append(event-1).Seal = empty, want seal")
	}

	event.EvidenceHash = "tampered"
	_, err = log.Append(ctx, event)
	if !errors.Is(err, ErrEvolutionEventSealed) {
		t.Fatalf("Append(tampered event-1) error = %v, want ErrEvolutionEventSealed", err)
	}
	if err := log.Verify(ctx); err != nil {
		t.Fatalf("Verify() error = %v, want nil", err)
	}
}

func validSolidifyGene() memory.Gene {
	return memory.Gene{
		ID:          "gene-go-timeout",
		Status:      memory.GeneStatusDraft,
		Tags:        []string{"go", "failure"},
		Match:       "go test timeout",
		UseWhen:     "when go test times out",
		Plan:        "rerun with focused package and inspect timeout",
		Avoid:       "avoid broad retries",
		Constraints: "keep changes scoped",
		Validation:  "go test ./...",
		SuccessRate: 0.7,
	}
}
