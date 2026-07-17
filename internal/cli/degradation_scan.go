package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/task"
)

// qualityForSignal maps a runtime learning signal onto a per-task quality score
// used to detect degradation over time. It reports false for signals that carry
// no task-quality meaning (e.g. permission/secret events, which are trust
// concerns handled by the trust score job) so the caller skips them.
func qualityForSignal(signal evolution.SignalKind) (float64, bool) {
	switch signal {
	case evolution.SignalSuccess:
		return 1.0, true
	case evolution.SignalFailure, evolution.SignalHardLoopFailure, evolution.SignalBudgetExhausted:
		return 0.0, true
	default:
		return 0, false
	}
}

// newDegradationScanJob builds a background job that periodically runs the
// EvolutionEvaluator over per-agent task-quality samples gathered from the
// runtime learning event stream. When an agent's quality drops past the
// configured threshold across the evaluation window, the evaluator publishes a
// degradation alert on the event bus.
//
// The scheduler ticks far more often than degradation needs checking (14-day
// window), so evaluation is time-gated to run at most once per scanPeriod; every
// tick still cheaply absorbs any new samples from the event stream. A fresh
// deployment has no baseline older than the window, so the evaluator is a no-op
// until enough history accrues — that is correct, not a silent failure.
func newDegradationScanJob(events port.EventBus, evaluator quality.EvolutionEvaluator, scanPeriod time.Duration, now func() time.Time) task.BackgroundJob {
	if now == nil {
		now = time.Now
	}
	if scanPeriod <= 0 {
		scanPeriod = time.Hour
	}
	var mu sync.Mutex
	processed := make(map[string]bool)
	samplesByAgent := make(map[string][]quality.EvolutionSample)
	var lastEval time.Time
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if events == nil {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()

		// 1. Absorb any new quality samples from the learning event stream.
		for _, event := range events.Events() {
			learning, ok := evolution.ParseLearningRuntimeEvent(event)
			if !ok {
				continue
			}
			score, relevant := qualityForSignal(learning.Signal)
			if !relevant {
				continue
			}
			key := event.TaskID + ":" + event.Message + ":" + event.CreatedAt.UTC().Format(time.RFC3339Nano)
			if processed[key] {
				continue
			}
			processed[key] = true
			at := learning.PublishedAt
			if at.IsZero() {
				at = event.CreatedAt
			}
			samplesByAgent[learning.AgentID] = append(samplesByAgent[learning.AgentID], quality.EvolutionSample{
				AgentID:     learning.AgentID,
				TaskQuality: score,
				ObservedAt:  at,
			})
		}

		// 2. Time-gate the (relatively expensive, coarse-grained) evaluation.
		t := now()
		if !lastEval.IsZero() && t.Sub(lastEval) < scanPeriod {
			return nil
		}
		lastEval = t

		for agentID, samples := range samplesByAgent {
			if _, err := evaluator.Evaluate(ctx, samples, quality.EvalResult{Status: quality.EvalNormal}); err != nil {
				return fmt.Errorf("evaluate degradation for agent %s: %w", agentID, err)
			}
		}
		return nil
	}
}
