package tool

import (
	"context"
	"testing"
)

// countingBudget is a LoopBudget that keeps its own counts, so a test can assert
// what a caller recorded and control what it is told back.
type countingBudget struct {
	counts map[string]int
	limit  int
}

func newCountingBudget(limit int) *countingBudget {
	return &countingBudget{counts: map[string]int{}, limit: limit}
}

func (b *countingBudget) Record(name string) (int, int) {
	b.counts[name]++
	return b.counts[name], b.limit
}

func TestLoopBudgetRoundTripsOnTheContext(t *testing.T) {
	budget := newCountingBudget(7)
	ctx := WithLoopBudget(context.Background(), budget)

	got, found := LoopBudgetFrom(ctx)
	if !found {
		t.Fatal("LoopBudgetFrom did not find the budget WithLoopBudget attached")
	}
	count, limit := got.Record("fetch_url")
	if count != 1 || limit != 7 {
		t.Fatalf("Record = (%d, %d), want (1, 7)", count, limit)
	}
	if budget.counts["fetch_url"] != 1 {
		t.Fatalf("the budget on the ctx is not the one that was attached: counts = %v", budget.counts)
	}
}

// An unmarked ctx must report "no budget" rather than a usable zero value: a
// caller that can start tool calls of its own has to be able to tell the wiring
// is broken, because counting nothing is the bypass the budget exists to stop.
func TestLoopBudgetFromReportsAnAbsentBudget(t *testing.T) {
	budget, found := LoopBudgetFrom(context.Background())
	if found {
		t.Fatalf("LoopBudgetFrom found a budget on an unmarked ctx: %#v", budget)
	}
	if budget != nil {
		t.Fatalf("LoopBudgetFrom returned %#v with found=false, want nil", budget)
	}
}

func TestWithLoopBudgetRejectsANilBudget(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want a panic when attaching a nil budget; it would read as 'no budget' downstream")
		}
	}()
	WithLoopBudget(context.Background(), nil)
}
