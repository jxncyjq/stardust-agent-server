package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/port"
)

// concurrencyMaas records the peak number of overlapping Generate calls, so a
// test can prove the reference models ran in parallel rather than serially.
type concurrencyMaas struct {
	mu        sync.Mutex
	active    int
	maxActive int
	reply     string
}

func (m *concurrencyMaas) Generate(ctx context.Context, _ port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.mu.Lock()
	m.active++
	if m.active > m.maxActive {
		m.maxActive = m.active
	}
	m.mu.Unlock()
	time.Sleep(10 * time.Millisecond) // widen the overlap window
	m.mu.Lock()
	m.active--
	m.mu.Unlock()
	return port.InferenceResponse{Text: m.reply}, nil
}

// capturingMaas records the last prompt it was asked to generate from.
type capturingMaas struct {
	mu     sync.Mutex
	reply  string
	prompt string
}

func (m *capturingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.mu.Lock()
	m.prompt = req.Prompt
	m.mu.Unlock()
	return port.InferenceResponse{Text: m.reply}, nil
}

func TestMoAAggregateRunsReferencesInParallelAndSynthesizes(t *testing.T) {
	t.Parallel()

	refClient := &concurrencyMaas{reply: "参考观点"}
	aggregator := &capturingMaas{reply: "融合后的最终答复"}
	coord, err := NewMoACoordinator([]ModelRef{
		{Label: "alpha", Client: refClient},
		{Label: "beta", Client: refClient},
		{Label: "gamma", Client: refClient},
	}, ModelRef{Label: "agg", Client: aggregator})
	if err != nil {
		t.Fatalf("NewMoACoordinator() error = %v, want nil", err)
	}

	result, err := coord.Aggregate(context.Background(), "评估架构方案")
	if err != nil {
		t.Fatalf("Aggregate() error = %v, want nil", err)
	}
	if result.Text != "融合后的最终答复" {
		t.Fatalf("Aggregate().Text = %q, want aggregator output", result.Text)
	}
	if refClient.maxActive < 2 {
		t.Fatalf("reference models max concurrency = %d, want >= 2 (parallel)", refClient.maxActive)
	}
	// The aggregator prompt must carry all three labeled reference blocks.
	for _, label := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(aggregator.prompt, "[参考回答 "+label+"]") {
			t.Fatalf("aggregator prompt missing labeled block for %q:\n%s", label, aggregator.prompt)
		}
	}
	if len(result.ReferenceLabels) != 3 {
		t.Fatalf("ReferenceLabels = %v, want 3 contributors", result.ReferenceLabels)
	}
}

// flakyMaas errors on demand, to drive the reference-drop and all-failed paths.
type flakyMaas struct {
	fail  bool
	reply string
}

func (m flakyMaas) Generate(ctx context.Context, _ port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	if m.fail {
		return port.InferenceResponse{}, errors.New("model unavailable")
	}
	return port.InferenceResponse{Text: m.reply}, nil
}

func TestMoAAggregateDropsFailedReferenceButSucceeds(t *testing.T) {
	t.Parallel()

	aggregator := &capturingMaas{reply: "final"}
	coord, err := NewMoACoordinator([]ModelRef{
		{Label: "good", Client: flakyMaas{reply: "kept"}},
		{Label: "bad", Client: flakyMaas{fail: true}},
	}, ModelRef{Label: "agg", Client: aggregator})
	if err != nil {
		t.Fatalf("NewMoACoordinator() error = %v, want nil", err)
	}
	result, err := coord.Aggregate(context.Background(), "task")
	if err != nil {
		t.Fatalf("Aggregate() error = %v, want nil", err)
	}
	if len(result.ReferenceLabels) != 1 || result.ReferenceLabels[0] != "good" {
		t.Fatalf("ReferenceLabels = %v, want [good]", result.ReferenceLabels)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want one dropped-reference warning", result.Warnings)
	}
}

func TestMoAAggregateFailsLoudWhenAllReferencesFail(t *testing.T) {
	t.Parallel()

	aggregator := &capturingMaas{reply: "should not be produced"}
	coord, err := NewMoACoordinator([]ModelRef{
		{Label: "bad1", Client: flakyMaas{fail: true}},
		{Label: "bad2", Client: flakyMaas{fail: true}},
	}, ModelRef{Label: "agg", Client: aggregator})
	if err != nil {
		t.Fatalf("NewMoACoordinator() error = %v, want nil", err)
	}
	if _, err := coord.Aggregate(context.Background(), "task"); err == nil {
		t.Fatalf("Aggregate(all-fail) error = nil, want fail-loud")
	}
	if aggregator.prompt != "" {
		t.Fatalf("aggregator was invoked with %q, want no call when all references failed", aggregator.prompt)
	}
}

func TestNewMoACoordinatorValidates(t *testing.T) {
	t.Parallel()

	if _, err := NewMoACoordinator(nil, ModelRef{Client: capturingMaas2()}); err == nil {
		t.Fatalf("NewMoACoordinator(no refs) error = nil, want non-nil")
	}
	if _, err := NewMoACoordinator([]ModelRef{{Label: "a", Client: capturingMaas2()}}, ModelRef{}); err == nil {
		t.Fatalf("NewMoACoordinator(nil aggregator) error = nil, want non-nil")
	}
}

func capturingMaas2() port.MaasInferenceClient { return &capturingMaas{} }
