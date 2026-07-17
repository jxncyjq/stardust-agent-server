package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

// mapResolver resolves profile names to preconfigured clients, erroring on unknown.
type mapResolver struct {
	clients map[string]port.MaasInferenceClient
}

func (r mapResolver) Resolve(profile string) (port.MaasInferenceClient, error) {
	client, ok := r.clients[profile]
	if !ok {
		return nil, errors.New("unknown profile " + profile)
	}
	return client, nil
}

func TestRegisterMoAConsultToolRegisters(t *testing.T) {
	t.Parallel()
	registry := tool.NewRegistry(nil, nil, nil)
	RegisterMoAConsultTool(registry, mapResolver{})
	if !hasDescriptor(registry, "moa_consult") {
		t.Fatalf("registry missing moa_consult")
	}
	// nil resolver → no-op.
	empty := tool.NewRegistry(nil, nil, nil)
	RegisterMoAConsultTool(empty, nil)
	if hasDescriptor(empty, "moa_consult") {
		t.Fatalf("nil resolver unexpectedly registered moa_consult")
	}
}

func TestHandleMoAConsultAggregates(t *testing.T) {
	t.Parallel()
	resolver := mapResolver{clients: map[string]port.MaasInferenceClient{
		"ref-a": &recordingSubMaas{summary: "answer a"},
		"ref-b": &recordingSubMaas{summary: "answer b"},
		"agg":   &recordingSubMaas{summary: "synthesized"},
	}}
	result, err := handleMoAConsult(context.Background(), resolver, domain.ToolCall{
		ID: "c1", Arguments: map[string]string{
			"task":               "evaluate",
			"reference_profiles": "ref-a, ref-b",
			"aggregator_profile": "agg",
		},
	})
	if err != nil {
		t.Fatalf("handleMoAConsult() error = %v, want nil", err)
	}
	if !result.Success {
		t.Fatalf("handleMoAConsult() failure: %q", result.Error)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(result.Output), &payload); err != nil {
		t.Fatalf("decode output %q: %v", result.Output, err)
	}
	if payload["text"] != "synthesized" {
		t.Fatalf("text = %v, want synthesized", payload["text"])
	}
	labels, ok := payload["reference_labels"].([]any)
	if !ok || len(labels) != 2 {
		t.Fatalf("reference_labels = %v, want 2", payload["reference_labels"])
	}
}

func TestHandleMoAConsultMissingArgsFailsSoft(t *testing.T) {
	t.Parallel()
	result, err := handleMoAConsult(context.Background(), mapResolver{}, domain.ToolCall{
		ID: "c1", Arguments: map[string]string{"task": "x"},
	})
	if err != nil {
		t.Fatalf("handleMoAConsult(missing) error = %v, want nil", err)
	}
	if result.Success {
		t.Fatalf("handleMoAConsult(missing) success = true, want failure result")
	}
}

func TestHandleMoAConsultResolveErrorFailsLoud(t *testing.T) {
	t.Parallel()
	_, err := handleMoAConsult(context.Background(), mapResolver{}, domain.ToolCall{
		ID: "c1", Arguments: map[string]string{
			"task":               "x",
			"reference_profiles": "missing",
			"aggregator_profile": "agg",
		},
	})
	if err == nil {
		t.Fatalf("handleMoAConsult(unresolvable) error = nil, want fail-loud")
	}
}
