package memory

import (
	"context"
	"testing"
)

func TestCapabilityMemorySearchGenesRanksActiveMatchesWithInjectionLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewCapabilityMemoryStore()
	genes := []Gene{
		testGene("go-tests", GeneStatusActive, []string{"go", "test"}, 0.82, 8, 1),
		testGene("go-style", GeneStatusActive, []string{"go", "style"}, 0.95, 10, 0),
		testGene("go-errors", GeneStatusActive, []string{"go", "error"}, 0.77, 7, 2),
		testGene("go-docs", GeneStatusActive, []string{"go", "docs"}, 0.91, 6, 0),
		testGene("draft-go", GeneStatusDraft, []string{"go"}, 0.99, 20, 0),
		testGene("frozen-go", GeneStatusFrozen, []string{"go"}, 0.99, 20, 0),
	}
	for _, gene := range genes {
		if err := store.PutGene(ctx, gene); err != nil {
			t.Fatalf("PutGene(%q) error = %v, want nil", gene.ID, err)
		}
	}

	hits, err := store.SearchGenes(ctx, CapabilityQuery{
		Text: "write go tests and fix style",
		Tags: []string{"go", "test", "style"},
		TopK: 5,
	})
	if err != nil {
		t.Fatalf("SearchGenes() error = %v, want nil", err)
	}
	if len(hits) != 3 {
		t.Fatalf("SearchGenes() len = %d, want 3", len(hits))
	}
	wantOrder := []string{"go-style", "go-tests", "go-docs"}
	for idx, want := range wantOrder {
		if hits[idx].Gene.ID != want {
			t.Errorf("SearchGenes()[%d].Gene.ID = %q, want %q", idx, hits[idx].Gene.ID, want)
		}
		if hits[idx].Gene.Status != GeneStatusActive {
			t.Errorf("SearchGenes()[%d].Gene.Status = %q, want %q", idx, hits[idx].Gene.Status, GeneStatusActive)
		}
	}
}

func TestCapabilityMemoryMarkOutcomeUpdatesRanking(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewCapabilityMemoryStore()
	before := testGene("refactor", GeneStatusActive, []string{"go", "refactor"}, 0.5, 0, 0)
	after := testGene("simple", GeneStatusActive, []string{"go", "refactor"}, 0.6, 0, 0)
	for _, gene := range []Gene{before, after} {
		if err := store.PutGene(ctx, gene); err != nil {
			t.Fatalf("PutGene(%q) error = %v, want nil", gene.ID, err)
		}
	}
	for range 4 {
		if err := store.MarkOutcome(ctx, "refactor", CapabilityOutcomeSuccess); err != nil {
			t.Fatalf("MarkOutcome(refactor, success) error = %v, want nil", err)
		}
	}
	if err := store.MarkOutcome(ctx, "simple", CapabilityOutcomeFailure); err != nil {
		t.Fatalf("MarkOutcome(simple, failure) error = %v, want nil", err)
	}

	hits, err := store.SearchGenes(ctx, CapabilityQuery{
		Text: "go refactor",
		Tags: []string{"go", "refactor"},
		TopK: 2,
	})
	if err != nil {
		t.Fatalf("SearchGenes() error = %v, want nil", err)
	}
	if hits[0].Gene.ID != "refactor" {
		t.Fatalf("SearchGenes()[0].Gene.ID = %q, want refactor", hits[0].Gene.ID)
	}
	if hits[0].Gene.SuccessCount != 4 {
		t.Errorf("SearchGenes()[0].Gene.SuccessCount = %d, want 4", hits[0].Gene.SuccessCount)
	}
}

func TestCapabilityMemoryPromotesAndRanksCapsules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewCapabilityMemoryStore()
	capsules := []Capsule{
		{ID: "cap-low", GeneIDs: []string{"go-tests"}, Query: "go test", Tags: []string{"go", "test"}, Outcome: "pass", SuccessCount: 2, Confidence: 0.6},
		{ID: "cap-high", GeneIDs: []string{"go-style"}, Query: "go style", Tags: []string{"go", "style"}, Outcome: "pass", SuccessCount: 8, Confidence: 0.9},
		{ID: "cap-other", GeneIDs: []string{"python"}, Query: "python test", Tags: []string{"python"}, Outcome: "pass", SuccessCount: 10, Confidence: 0.95},
	}
	for _, capsule := range capsules {
		if err := store.PromoteCapsule(ctx, capsule); err != nil {
			t.Fatalf("PromoteCapsule(%q) error = %v, want nil", capsule.ID, err)
		}
	}

	hits, err := store.SearchCapsules(ctx, CapabilityQuery{
		Text: "go style cleanup",
		Tags: []string{"go", "style"},
		TopK: 2,
	})
	if err != nil {
		t.Fatalf("SearchCapsules() error = %v, want nil", err)
	}
	if len(hits) != 2 {
		t.Fatalf("SearchCapsules() len = %d, want 2", len(hits))
	}
	if hits[0].Capsule.ID != "cap-high" {
		t.Fatalf("SearchCapsules()[0].Capsule.ID = %q, want cap-high", hits[0].Capsule.ID)
	}
}

func TestCapabilityMemoryCopiesBoundaryData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewCapabilityMemoryStore()
	gene := testGene("go-tests", GeneStatusActive, []string{"go", "test"}, 0.8, 1, 0)
	if err := store.PutGene(ctx, gene); err != nil {
		t.Fatalf("PutGene(go-tests) error = %v, want nil", err)
	}
	gene.Tags[0] = "changed"

	hits, err := store.SearchGenes(ctx, CapabilityQuery{Text: "go test", TopK: 1})
	if err != nil {
		t.Fatalf("SearchGenes() error = %v, want nil", err)
	}
	hits[0].Gene.Tags[0] = "mutated"

	again, err := store.SearchGenes(ctx, CapabilityQuery{Text: "go test", TopK: 1})
	if err != nil {
		t.Fatalf("SearchGenes() second error = %v, want nil", err)
	}
	if again[0].Gene.Tags[0] != "go" {
		t.Fatalf("SearchGenes() boundary copy tag = %q, want go", again[0].Gene.Tags[0])
	}
}

func TestCapabilityMemoryFreezeGeneRemovesItFromInjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewCapabilityMemoryStore()
	gene := testGene("go-tests", GeneStatusActive, []string{"go", "test"}, 0.8, 2, 0)
	if err := store.PutGene(ctx, gene); err != nil {
		t.Fatalf("PutGene(go-tests) error = %v, want nil", err)
	}
	if err := store.FreezeGene(ctx, "go-tests"); err != nil {
		t.Fatalf("FreezeGene(go-tests) error = %v, want nil", err)
	}

	hits, err := store.SearchGenes(ctx, CapabilityQuery{Text: "go test", TopK: 1})
	if err != nil {
		t.Fatalf("SearchGenes() error = %v, want nil", err)
	}
	if len(hits) != 0 {
		t.Fatalf("SearchGenes() len = %d, want 0 after freeze", len(hits))
	}
}

func testGene(id string, status GeneStatus, tags []string, successRate float64, successCount int, failureCount int) Gene {
	return Gene{
		ID:           id,
		Version:      "1.0.0",
		Status:       status,
		Tags:         tags,
		Match:        "use this strategy for " + id,
		UseWhen:      "task mentions " + id,
		Plan:         "apply the focused strategy",
		Avoid:        "avoid broad unrelated changes",
		Constraints:  "keep edits scoped",
		Validation:   "run tests",
		SuccessRate:  successRate,
		SuccessCount: successCount,
		FailureCount: failureCount,
	}
}
