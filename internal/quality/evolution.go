package quality

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

const RuntimeEventDegradationAlert = "evolution.degradation_alert"

type EvolutionEvaluatorConfig struct {
	EventBus             port.EventBus
	QualityDropThreshold float64
	Window               time.Duration
	Now                  func() time.Time
}

type EvolutionEvaluator struct {
	eventBus             port.EventBus
	qualityDropThreshold float64
	window               time.Duration
	now                  func() time.Time
}

type EvolutionSample struct {
	AgentID            string
	TaskQuality        float64
	LearningVelocity   float64
	ReuseEffectiveness float64
	CostPerTask        float64
	Stability          float64
	ObservedAt         time.Time
}

type EvolutionDimensions struct {
	TaskQuality        float64
	LearningVelocity   float64
	ReuseEffectiveness float64
	CostEfficiency     float64
	Stability          float64
}

type EvolutionReport struct {
	AgentID    string
	Dimensions EvolutionDimensions
	WindowFrom time.Time
	WindowTo   time.Time
	Alert      *DegradationAlert
}

type DegradationAlert struct {
	AgentID     string
	QualityDrop float64
	WindowFrom  time.Time
	WindowTo    time.Time
	Reason      string
	CreatedAt   time.Time
}

func NewEvolutionEvaluator(cfg EvolutionEvaluatorConfig) EvolutionEvaluator {
	if cfg.QualityDropThreshold <= 0 {
		cfg.QualityDropThreshold = 0.2
	}
	if cfg.Window <= 0 {
		cfg.Window = 14 * 24 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return EvolutionEvaluator{
		eventBus:             cfg.EventBus,
		qualityDropThreshold: cfg.QualityDropThreshold,
		window:               cfg.Window,
		now:                  cfg.Now,
	}
}

func (e EvolutionEvaluator) Evaluate(ctx context.Context, samples []EvolutionSample, eval EvalResult) (EvolutionReport, error) {
	if err := ctx.Err(); err != nil {
		return EvolutionReport{}, err
	}
	if len(samples) == 0 {
		return EvolutionReport{}, nil
	}
	ordered := append([]EvolutionSample(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].ObservedAt.Before(ordered[j].ObservedAt)
	})
	now := e.now()
	currentStart := now.Add(-e.window)
	report := EvolutionReport{
		AgentID:    firstAgentID(ordered),
		Dimensions: dimensions(ordered),
		WindowFrom: currentStart,
		WindowTo:   now,
	}
	baseline, current := splitSamples(ordered, currentStart)
	if len(baseline) == 0 || len(current) == 0 || hasExternalFault(eval) {
		return report, nil
	}
	baselineQuality := averageQuality(baseline)
	currentQuality := averageQuality(current)
	qualityDrop := baselineQuality - currentQuality
	if qualityDrop < e.qualityDropThreshold {
		return report, nil
	}
	alert := DegradationAlert{
		AgentID:     report.AgentID,
		QualityDrop: qualityDrop,
		WindowFrom:  currentStart,
		WindowTo:    now,
		Reason:      "task quality dropped within evaluation window",
		CreatedAt:   now,
	}
	report.Alert = &alert
	if e.eventBus != nil {
		if err := e.eventBus.Publish(ctx, domain.RuntimeEvent{
			Type:      RuntimeEventDegradationAlert,
			TaskID:    "",
			Message:   fmt.Sprintf("agent %s quality dropped %.2f in 14d window", alert.AgentID, alert.QualityDrop),
			CreatedAt: now,
		}); err != nil {
			return EvolutionReport{}, err
		}
	}
	return report, nil
}

func dimensions(samples []EvolutionSample) EvolutionDimensions {
	if len(samples) == 0 {
		return EvolutionDimensions{}
	}
	var quality, velocity, reuse, cost, stability float64
	for _, sample := range samples {
		quality += sample.TaskQuality
		velocity += sample.LearningVelocity
		reuse += sample.ReuseEffectiveness
		cost += sample.CostPerTask
		stability += sample.Stability
	}
	n := float64(len(samples))
	return EvolutionDimensions{
		TaskQuality:        clamp01(quality / n),
		LearningVelocity:   clamp01((velocity / n) / 5),
		ReuseEffectiveness: clamp01(reuse / n),
		CostEfficiency:     clamp01(1 / (1 + (cost/n)/100)),
		Stability:          clamp01(stability / n),
	}
}

func splitSamples(samples []EvolutionSample, currentStart time.Time) ([]EvolutionSample, []EvolutionSample) {
	var baseline []EvolutionSample
	var current []EvolutionSample
	for _, sample := range samples {
		if sample.ObservedAt.Before(currentStart) {
			baseline = append(baseline, sample)
			continue
		}
		current = append(current, sample)
	}
	return baseline, current
}

func averageQuality(samples []EvolutionSample) float64 {
	if len(samples) == 0 {
		return 0
	}
	var total float64
	for _, sample := range samples {
		total += sample.TaskQuality
	}
	return total / float64(len(samples))
}

func hasExternalFault(eval EvalResult) bool {
	for _, finding := range eval.Findings {
		if finding.Layer == EvalLayerComponent && finding.Status == EvalComponentDegraded {
			return true
		}
	}
	return eval.Status == EvalComponentDegraded
}

func firstAgentID(samples []EvolutionSample) string {
	for _, sample := range samples {
		if sample.AgentID != "" {
			return sample.AgentID
		}
	}
	return ""
}

func clamp01(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}
