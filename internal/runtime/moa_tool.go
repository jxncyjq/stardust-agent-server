package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

// ModelResolver resolves a MaaS profile name to an inference client. It lets the
// moa_consult tool build reference/aggregator models by name without the runtime
// package depending on the config/adapter packages (avoiding an import cycle);
// the concrete resolver is supplied at wiring time.
type ModelResolver interface {
	Resolve(profile string) (port.MaasInferenceClient, error)
}

// RegisterMoAConsultTool registers the one-shot moa_consult tool, which runs a
// Mixture-of-Agents aggregation across named model profiles. It is a no-op when
// registry or resolver is nil.
func RegisterMoAConsultTool(registry *tool.Registry, resolver ModelResolver) {
	if registry == nil || resolver == nil {
		return
	}
	registry.RegisterDescriptor(moaConsultDescriptor(), tool.HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return handleMoAConsult(ctx, resolver, call)
	}))
}

func moaConsultDescriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "moa_consult",
		Description: "One-shot Mixture-of-Agents: run several reference model profiles in parallel on one task, " +
			"then synthesize their answers with an aggregator model. Arguments: task, reference_profiles " +
			"(comma-separated profile names), aggregator_profile. Use for hard questions worth multiple models; " +
			"it costs N+1 model calls, so it is explicit and one-shot, not a per-round default.",
		RiskLevel: "high",
		Timeout:   5 * time.Minute,
		// Not in the M2a task-2 brief's classification table; fail-safe per the
		// brief's own rule (unlisted registered tool -> mark sensitive). It fans
		// out N+1 costly model calls per invocation, an effect worth gating behind
		// approval even though it has no filesystem/network/task-ledger writes.
		Sensitive: true,
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"task", "reference_profiles", "aggregator_profile"},
			"properties": map[string]any{
				"task":               map[string]any{"type": "string", "description": "The question/task to consult on."},
				"reference_profiles": map[string]any{"type": "string", "description": "Comma-separated MaaS profile names to run in parallel as reference models."},
				"aggregator_profile": map[string]any{"type": "string", "description": "MaaS profile name of the aggregator that synthesizes the final answer."},
			},
		},
	}
}

func handleMoAConsult(ctx context.Context, resolver ModelResolver, call domain.ToolCall) (domain.ToolResult, error) {
	task := strings.TrimSpace(call.Arguments["task"])
	if task == "" {
		return domain.ToolResult{CallID: call.ID, Success: false, Error: "task is required"}, nil
	}
	refProfiles := parseToolsetsCSV(call.Arguments["reference_profiles"])
	aggProfile := strings.TrimSpace(call.Arguments["aggregator_profile"])
	if len(refProfiles) == 0 || aggProfile == "" {
		return domain.ToolResult{CallID: call.ID, Success: false, Error: "reference_profiles and aggregator_profile are required"}, nil
	}

	references := make([]ModelRef, 0, len(refProfiles))
	for _, profile := range refProfiles {
		client, err := resolver.Resolve(profile)
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("moa_consult resolve reference profile %q: %w", profile, err)
		}
		references = append(references, ModelRef{Label: profile, Client: client})
	}
	aggClient, err := resolver.Resolve(aggProfile)
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("moa_consult resolve aggregator profile %q: %w", aggProfile, err)
	}

	coord, err := NewMoACoordinator(references, ModelRef{Label: aggProfile, Client: aggClient})
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("moa_consult build coordinator: %w", err)
	}
	result, err := coord.Aggregate(ctx, task)
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("moa_consult aggregate: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"text":             result.Text,
		"reference_labels": result.ReferenceLabels,
		"warnings":         result.Warnings,
		"total_tokens":     result.TotalTokens,
	})
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("moa_consult encode result: %w", err)
	}
	return domain.ToolResult{CallID: call.ID, Success: true, Output: string(payload)}, nil
}
