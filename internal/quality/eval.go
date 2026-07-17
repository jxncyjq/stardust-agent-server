package quality

import "context"

type EvalStatus string

const (
	EvalNormal            EvalStatus = "normal"
	EvalOutputIssue       EvalStatus = "output_issue"
	EvalSoftLoop          EvalStatus = "soft_loop"
	EvalHardLoop          EvalStatus = "hard_loop"
	EvalComponentDegraded EvalStatus = "component_degraded"
	EvalDriftDetected     EvalStatus = "drift_detected"
)

type EvalResult struct {
	Status   EvalStatus
	Reason   string
	Findings []EvalFinding
}

type EvalFinding struct {
	Layer  EvalLayer
	Status EvalStatus
	Reason string
}

type EvalLayer string

const (
	EvalLayerOutput    EvalLayer = "output_quality"
	EvalLayerTrace     EvalLayer = "trace_health"
	EvalLayerComponent EvalLayer = "component_health"
	EvalLayerDrift     EvalLayer = "drift_detection"
)

type BehaviorReport struct {
	Output     OutputReport
	Trace      []string
	Components []ComponentMetric
	Drift      DriftReport
}

type OutputReport struct {
	Text         string
	RequiredDone bool
}

type ComponentMetric struct {
	Name        string
	SuccessRate float64
}

type DriftReport struct {
	Score float64
}

type EvalEngine struct {
	repeatThreshold int
}

func NewEvalEngine(repeatThreshold int) EvalEngine {
	return EvalEngine{repeatThreshold: repeatThreshold}
}

func (e EvalEngine) EvaluateTrace(ctx context.Context, trace []string) (EvalResult, error) {
	if err := ctx.Err(); err != nil {
		return EvalResult{}, err
	}
	if e.repeatThreshold <= 1 {
		e.repeatThreshold = 3
	}
	var last string
	var repeated int
	for _, item := range trace {
		if item == last {
			repeated++
		} else {
			last = item
			repeated = 1
		}
		if repeated >= e.repeatThreshold {
			return EvalResult{Status: EvalHardLoop, Reason: "repeated trace item"}, nil
		}
	}
	return EvalResult{Status: EvalNormal}, nil
}

func (e EvalEngine) EvaluateBehavior(ctx context.Context, report BehaviorReport) (EvalResult, error) {
	if err := ctx.Err(); err != nil {
		return EvalResult{}, err
	}
	var findings []EvalFinding
	if report.Output.RequiredDone && report.Output.Text == "" {
		findings = append(findings, EvalFinding{
			Layer:  EvalLayerOutput,
			Status: EvalOutputIssue,
			Reason: "required output is empty",
		})
	}
	traceResult, err := e.evaluateTraceFindings(ctx, report.Trace)
	if err != nil {
		return EvalResult{}, err
	}
	if traceResult.Status == EvalHardLoop {
		return traceResult, nil
	}
	findings = append(findings, traceResult.Findings...)
	for _, metric := range report.Components {
		if metric.SuccessRate < 0.5 {
			findings = append(findings, EvalFinding{
				Layer:  EvalLayerComponent,
				Status: EvalComponentDegraded,
				Reason: metric.Name + " success rate below threshold",
			})
		}
	}
	if report.Drift.Score > 0.8 {
		findings = append(findings, EvalFinding{
			Layer:  EvalLayerDrift,
			Status: EvalDriftDetected,
			Reason: "drift score above threshold",
		})
	}
	if len(findings) == 0 {
		return EvalResult{Status: EvalNormal}, nil
	}
	return EvalResult{
		Status:   findings[0].Status,
		Reason:   findings[0].Reason,
		Findings: findings,
	}, nil
}

func (e EvalEngine) evaluateTraceFindings(ctx context.Context, trace []string) (EvalResult, error) {
	result, err := e.EvaluateTrace(ctx, trace)
	if err != nil {
		return EvalResult{}, err
	}
	if result.Status == EvalHardLoop {
		return EvalResult{
			Status: EvalHardLoop,
			Reason: result.Reason,
			Findings: []EvalFinding{
				{
					Layer:  EvalLayerTrace,
					Status: EvalHardLoop,
					Reason: result.Reason,
				},
			},
		}, nil
	}
	if hasRepeatedTrace(trace, 2) {
		return EvalResult{
			Status: EvalSoftLoop,
			Findings: []EvalFinding{
				{
					Layer:  EvalLayerTrace,
					Status: EvalSoftLoop,
					Reason: "trace repeated before hard loop threshold",
				},
			},
		}, nil
	}
	return EvalResult{Status: EvalNormal}, nil
}

func hasRepeatedTrace(trace []string, threshold int) bool {
	if threshold <= 1 {
		threshold = 2
	}
	var last string
	var repeated int
	for _, item := range trace {
		if item == last {
			repeated++
		} else {
			last = item
			repeated = 1
		}
		if repeated >= threshold {
			return true
		}
	}
	return false
}
