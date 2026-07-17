package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/memory"
	"github.com/stardust/legion-agent/internal/task"
)

// TestGepFailureScanJobDistillsGene asserts the L5 learning wiring used by
// BuildServeService: a failure learning event published onto the shared event
// bus drives the GEP cycle, which solidifies a capability gene into the shared
// capability store. This is the same construction the serve path registers as
// the "gep-failure-scan" background job.
func TestGepFailureScanJobDistillsGene(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	events := adapter.NewMemoryEventBus()
	auditLog := adapter.NewMemoryAuditLog()
	capabilityStore := memory.NewCapabilityMemoryStore()
	gepCycle := evolution.NewGepCycle(evolution.GepCycleConfig{
		Extractor:       evolution.NewSignalExtractor(),
		Distiller:       evolution.DefaultDistillationOperator{},
		CapabilityStore: capabilityStore,
		EventLog:        evolution.NewEvolutionEventLog(auditLog),
	})
	job := task.NewGepFailureScanJob(events, gepCycle)

	if err := events.Publish(ctx, evolution.NewLearningRuntimeEvent(evolution.LearningEvent{
		AgentID: "default-agent",
		TaskID:  "task-fail-1",
		Signal:  evolution.SignalFailure,
		Reason:  "tool_error",
	})); err != nil {
		t.Fatalf("publish learning event: %v", err)
	}

	if err := job(ctx); err != nil {
		t.Fatalf("gep failure scan job: %v", err)
	}

	hits, err := capabilityStore.SearchGenes(ctx, memory.CapabilityQuery{
		Text: "task-fail-1 tool_error",
		Tags: []string{"failure"},
		TopK: 3,
	})
	if err != nil {
		t.Fatalf("search genes: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SearchGenes after gep failure scan = 0 hits, want a solidified gene")
	}
}

// TestGepFailureScanJobPropagatesRunnerError asserts the fail-loud contract: a
// GEP runner failure must surface as a wrapped error from the background job,
// not be silently swallowed.
func TestGepFailureScanJobPropagatesRunnerError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	events := adapter.NewMemoryEventBus()
	if err := events.Publish(ctx, evolution.NewLearningRuntimeEvent(evolution.LearningEvent{
		AgentID: "default-agent",
		TaskID:  "task-fail-2",
		Signal:  evolution.SignalFailure,
		Reason:  "tool_error",
	})); err != nil {
		t.Fatalf("publish learning event: %v", err)
	}

	sentinel := errors.New("boom")
	job := task.NewGepFailureScanJob(events, failingGepRunner{err: sentinel})
	err := job(ctx)
	if err == nil {
		t.Fatal("gep failure scan job with failing runner = nil error, want wrapped error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("gep failure scan job error = %v, want wrapped %v", err, sentinel)
	}
}

type failingGepRunner struct {
	err error
}

func (r failingGepRunner) Run(context.Context, evolution.ExtractionInput) (evolution.GepResult, error) {
	return evolution.GepResult{}, r.err
}
