package evolution

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/quality"
)

const (
	defaultSuppressionWindow    = 8
	defaultSuppressionThreshold = 3
)

type SignalKind string

const (
	SignalSuccess             SignalKind = "success"
	SignalFailure             SignalKind = "failure"
	SignalHardLoopFailure     SignalKind = "hard_loop_failure"
	SignalBudgetExhausted     SignalKind = "budget_exhausted"
	SignalPermissionViolation SignalKind = "permission_violation"
	SignalSecretExposure      SignalKind = "secret_exposure"
	SignalFeedbackPositive    SignalKind = "feedback_positive"
	SignalFeedbackNegative    SignalKind = "feedback_negative"
)

type SignalLevel string

const (
	SignalLevelStructured SignalLevel = "l1_structured"
	SignalLevelPattern    SignalLevel = "l2_pattern"
	SignalLevelInferred   SignalLevel = "l3_inferred"
)

type Feedback struct {
	Author string
	Rating int
	Text   string
}

type ExtractionInput struct {
	AgentID     string
	Task        domain.Task
	Run         domain.TaskRun
	Events      []domain.RuntimeEvent
	ToolResults []domain.ToolResult
	Eval        quality.EvalResult
	Feedback    []Feedback
	Cycle       int
}

type LearningSignal struct {
	Kind         SignalKind
	Level        SignalLevel
	Source       string
	Evidence     string
	Confidence   float64
	Suppressible bool
	CreatedAt    time.Time
}

type SignalExtractor struct {
	mu        sync.Mutex
	window    int
	threshold int
	history   map[SignalKind][]int
}

func NewSignalExtractor() *SignalExtractor {
	return &SignalExtractor{
		window:    defaultSuppressionWindow,
		threshold: defaultSuppressionThreshold,
		history:   make(map[SignalKind][]int),
	}
}

func (e *SignalExtractor) Extract(ctx context.Context, input ExtractionInput) ([]LearningSignal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input.Cycle <= 0 {
		input.Cycle = 1
	}
	candidates := e.extractCandidates(input)
	signals := make([]LearningSignal, 0, len(candidates))
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, signal := range candidates {
		if e.shouldSuppressLocked(signal.Kind, signal.Suppressible, input.Cycle) {
			continue
		}
		e.recordLocked(signal.Kind, input.Cycle)
		signals = append(signals, signal)
	}
	return append([]LearningSignal(nil), signals...), nil
}

func (e *SignalExtractor) extractCandidates(input ExtractionInput) []LearningSignal {
	now := time.Now()
	var signals []LearningSignal
	if input.Task.Status == domain.TaskDone || input.Run.Result != "" {
		signals = append(signals, LearningSignal{
			Kind:         SignalSuccess,
			Level:        SignalLevelStructured,
			Source:       "task_status",
			Evidence:     firstNonEmpty(string(input.Task.Status), input.Run.Result),
			Confidence:   0.85,
			Suppressible: true,
			CreatedAt:    now,
		})
	}
	if input.Task.Status == domain.TaskFailed || input.Task.Status == domain.TaskSuspended || hasToolFailure(input.ToolResults) {
		signals = append(signals, LearningSignal{
			Kind:         SignalFailure,
			Level:        SignalLevelStructured,
			Source:       "task_or_tool_result",
			Evidence:     failureEvidence(input),
			Confidence:   0.8,
			Suppressible: true,
			CreatedAt:    now,
		})
	}
	if input.Eval.Status == quality.EvalHardLoop || containsRuntimePattern(input.Events, "hard loop", "hardloop", "repeated trace") {
		signals = append(signals, LearningSignal{
			Kind:         SignalHardLoopFailure,
			Level:        SignalLevelStructured,
			Source:       "eval_trace",
			Evidence:     firstNonEmpty(input.Eval.Reason, eventEvidence(input.Events, "hard loop", "hardloop", "repeated trace")),
			Confidence:   0.95,
			Suppressible: false,
			CreatedAt:    now,
		})
	}
	if containsAny(allEvidence(input), "budget exhausted", "max iterations", "iteration budget") {
		signals = append(signals, LearningSignal{
			Kind:         SignalBudgetExhausted,
			Level:        SignalLevelStructured,
			Source:       "runtime_event",
			Evidence:     eventEvidence(input.Events, "budget exhausted", "max iterations", "iteration budget"),
			Confidence:   0.9,
			Suppressible: true,
			CreatedAt:    now,
		})
	}
	if containsAny(allEvidence(input), "permission denied", "unauthorized", "forbidden", "policy denied") {
		signals = append(signals, LearningSignal{
			Kind:         SignalPermissionViolation,
			Level:        SignalLevelPattern,
			Source:       "security_event",
			Evidence:     matchingEvidence(input, "permission denied", "unauthorized", "forbidden", "policy denied"),
			Confidence:   0.95,
			Suppressible: false,
			CreatedAt:    now,
		})
	}
	if containsAny(allEvidence(input), "secret exposure", "api key", "id_rsa", "token leaked", "leak secret") {
		signals = append(signals, LearningSignal{
			Kind:         SignalSecretExposure,
			Level:        SignalLevelPattern,
			Source:       "security_event",
			Evidence:     matchingEvidence(input, "secret exposure", "api key", "id_rsa", "token leaked", "leak secret"),
			Confidence:   0.95,
			Suppressible: false,
			CreatedAt:    now,
		})
	}
	for _, feedback := range input.Feedback {
		kind, ok := classifyFeedback(feedback)
		if !ok {
			continue
		}
		signals = append(signals, LearningSignal{
			Kind:         kind,
			Level:        SignalLevelPattern,
			Source:       "human_feedback",
			Evidence:     feedback.Text,
			Confidence:   feedbackConfidence(feedback),
			Suppressible: true,
			CreatedAt:    now,
		})
	}
	return dedupeSignals(signals)
}

func (e *SignalExtractor) shouldSuppressLocked(kind SignalKind, suppressible bool, cycle int) bool {
	if !suppressible {
		return false
	}
	recent := recentCycles(e.history[kind], cycle, e.window)
	return len(recent) >= e.threshold-1
}

func (e *SignalExtractor) recordLocked(kind SignalKind, cycle int) {
	e.history[kind] = append(recentCycles(e.history[kind], cycle, e.window), cycle)
}

func recentCycles(cycles []int, current int, window int) []int {
	minCycle := current - window + 1
	var recent []int
	for _, cycle := range cycles {
		if cycle >= minCycle && cycle <= current {
			recent = append(recent, cycle)
		}
	}
	return recent
}

func hasToolFailure(results []domain.ToolResult) bool {
	for _, result := range results {
		if !result.Success {
			return true
		}
	}
	return false
}

func failureEvidence(input ExtractionInput) string {
	for _, result := range input.ToolResults {
		if !result.Success {
			return firstNonEmpty(result.Error, result.Output, result.CallID)
		}
	}
	return firstNonEmpty(input.Eval.Reason, string(input.Task.Status), input.Task.ID)
}

func allEvidence(input ExtractionInput) string {
	var parts []string
	for _, event := range input.Events {
		parts = append(parts, event.Type, event.Message)
	}
	for _, result := range input.ToolResults {
		parts = append(parts, result.Output, result.Error)
	}
	parts = append(parts, input.Eval.Reason)
	for _, feedback := range input.Feedback {
		parts = append(parts, feedback.Text)
	}
	return strings.ToLower(strings.Join(parts, "\n"))
}

func containsRuntimePattern(events []domain.RuntimeEvent, patterns ...string) bool {
	return eventEvidence(events, patterns...) != ""
}

func eventEvidence(events []domain.RuntimeEvent, patterns ...string) string {
	for _, event := range events {
		text := strings.ToLower(event.Type + " " + event.Message)
		if containsAny(text, patterns...) {
			return firstNonEmpty(event.Message, event.Type)
		}
	}
	return ""
}

func matchingEvidence(input ExtractionInput, patterns ...string) string {
	if evidence := eventEvidence(input.Events, patterns...); evidence != "" {
		return evidence
	}
	for _, result := range input.ToolResults {
		text := strings.ToLower(result.Output + " " + result.Error)
		if containsAny(text, patterns...) {
			return firstNonEmpty(result.Error, result.Output, result.CallID)
		}
	}
	for _, feedback := range input.Feedback {
		if containsAny(strings.ToLower(feedback.Text), patterns...) {
			return feedback.Text
		}
	}
	return strings.Join(patterns, ",")
}

func containsAny(text string, patterns ...string) bool {
	for _, pattern := range patterns {
		if strings.Contains(text, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

func classifyFeedback(feedback Feedback) (SignalKind, bool) {
	text := strings.ToLower(feedback.Text)
	if feedback.Rating > 0 || containsAny(text, "good", "great", "success", "helpful", "works") {
		return SignalFeedbackPositive, true
	}
	if feedback.Rating < 0 || containsAny(text, "bad", "failed", "wrong", "repeated", "unsafe", "needs") {
		return SignalFeedbackNegative, true
	}
	return "", false
}

func feedbackConfidence(feedback Feedback) float64 {
	if feedback.Rating != 0 {
		return 0.85
	}
	return 0.65
}

func dedupeSignals(signals []LearningSignal) []LearningSignal {
	seen := make(map[SignalKind]bool, len(signals))
	out := make([]LearningSignal, 0, len(signals))
	for _, signal := range signals {
		if seen[signal.Kind] {
			continue
		}
		seen[signal.Kind] = true
		out = append(out, signal)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
