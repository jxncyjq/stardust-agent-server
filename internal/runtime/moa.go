package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/stardust/legion-agent/internal/port"
)

// ModelRef names one model participating in a Mixture-of-Agents run. Label is a
// human-readable tag placed on the model's output in the aggregation prompt;
// Client is the inference client to call.
type ModelRef struct {
	Label  string
	Client port.MaasInferenceClient
}

// MoAResult is the outcome of a Mixture-of-Agents aggregation: the aggregator's
// synthesized answer, the labels of the reference models that actually
// contributed, any per-reference warnings (a reference that failed or returned
// empty is dropped, not fatal), and the summed token usage.
type MoAResult struct {
	Text             string
	ReferenceLabels  []string
	Warnings         []string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// MoACoordinator runs several reference models in parallel on one task, then asks
// an aggregator model to synthesize a single answer from their labeled outputs.
// It is a one-shot collaboration (invoked explicitly), not something the main
// tool loop runs every round, so it avoids multiplying per-round token cost.
type MoACoordinator struct {
	references []ModelRef
	aggregator ModelRef
}

// NewMoACoordinator validates and builds a coordinator. At least one reference
// model and a non-nil aggregator client are required; anything less is a
// programming error reported loudly rather than a silently degraded run.
func NewMoACoordinator(references []ModelRef, aggregator ModelRef) (*MoACoordinator, error) {
	if len(references) == 0 {
		return nil, fmt.Errorf("moa: at least one reference model is required")
	}
	if aggregator.Client == nil {
		return nil, fmt.Errorf("moa: aggregator client is required")
	}
	return &MoACoordinator{
		references: append([]ModelRef(nil), references...),
		aggregator: aggregator,
	}, nil
}

type moaReferenceOutput struct {
	label string
	text  string
	resp  port.InferenceResponse
	err   error
}

// Aggregate runs every reference model on task concurrently, then synthesizes
// their answers through the aggregator. A reference that errors or returns empty
// is recorded as a warning and dropped. If every reference drops, Aggregate fails
// loud rather than asking the aggregator to synthesize nothing — the settlement
// step never fabricates an answer from empty inputs.
func (c *MoACoordinator) Aggregate(ctx context.Context, task string) (MoAResult, error) {
	if err := ctx.Err(); err != nil {
		return MoAResult{}, err
	}
	if strings.TrimSpace(task) == "" {
		return MoAResult{}, fmt.Errorf("moa aggregate: task is required")
	}

	outputs := make([]moaReferenceOutput, len(c.references))
	var wg sync.WaitGroup
	for i, ref := range c.references {
		wg.Add(1)
		go func(i int, ref ModelRef) {
			defer wg.Done()
			if ref.Client == nil {
				outputs[i] = moaReferenceOutput{label: ref.Label, err: fmt.Errorf("nil client")}
				return
			}
			resp, err := ref.Client.Generate(ctx, port.InferenceRequest{
				RequestID: "moa-ref-" + ref.Label,
				Prompt:    task,
			})
			outputs[i] = moaReferenceOutput{label: ref.Label, text: strings.TrimSpace(resp.Text), resp: resp, err: err}
		}(i, ref)
	}
	wg.Wait()

	var blocks []string
	var labels []string
	var warnings []string
	result := MoAResult{}
	for _, out := range outputs {
		if out.err != nil {
			warnings = append(warnings, fmt.Sprintf("reference %q failed: %v", out.label, out.err))
			continue
		}
		if out.text == "" {
			warnings = append(warnings, fmt.Sprintf("reference %q returned an empty answer", out.label))
			continue
		}
		labels = append(labels, out.label)
		blocks = append(blocks, fmt.Sprintf("[参考回答 %s]\n%s", out.label, out.text))
		result.PromptTokens += out.resp.PromptTokens
		result.CompletionTokens += out.resp.CompletionTokens
		result.TotalTokens += out.resp.TotalTokens
	}
	if len(blocks) == 0 {
		return MoAResult{}, fmt.Errorf("moa aggregate: all %d reference models failed or returned empty; refusing to aggregate nothing", len(c.references))
	}

	aggResp, err := c.aggregator.Client.Generate(ctx, port.InferenceRequest{
		RequestID: "moa-aggregator",
		Prompt:    buildAggregatorPrompt(task, blocks),
	})
	if err != nil {
		return MoAResult{}, fmt.Errorf("moa aggregate: aggregator model failed: %w", err)
	}
	result.Text = strings.TrimSpace(aggResp.Text)
	result.ReferenceLabels = labels
	result.Warnings = warnings
	result.PromptTokens += aggResp.PromptTokens
	result.CompletionTokens += aggResp.CompletionTokens
	result.TotalTokens += aggResp.TotalTokens
	return result, nil
}

// buildAggregatorPrompt assembles the synthesis prompt: the original task plus
// every reference model's labeled answer, with an instruction to critique and
// merge them into one superior response.
func buildAggregatorPrompt(task string, blocks []string) string {
	var b strings.Builder
	b.WriteString("你是聚合器。以下是多个模型对同一任务的独立回答。请甄别其中的正确与错误之处，融合各家优点，产出一个比任何单一回答都更完整、更准确的最终答复。\n\n")
	b.WriteString("[任务]\n")
	b.WriteString(task)
	b.WriteString("\n\n")
	b.WriteString(strings.Join(blocks, "\n\n"))
	b.WriteString("\n\n[最终答复]\n")
	return b.String()
}
