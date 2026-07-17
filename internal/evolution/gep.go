package evolution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/memory"
	"github.com/stardust/legion-agent/internal/port"
)

type EvolutionStage string

const (
	StageScan     EvolutionStage = "scan"
	StageSignal   EvolutionStage = "signal"
	StageIntent   EvolutionStage = "intent"
	StageMutate   EvolutionStage = "mutate"
	StageValidate EvolutionStage = "validate"
	StageSolidify EvolutionStage = "solidify"
)

type EvolutionDecision string

const (
	DecisionRecorded         EvolutionDecision = "recorded"
	DecisionCandidate        EvolutionDecision = "candidate"
	DecisionDrafted          EvolutionDecision = "drafted"
	DecisionValidated        EvolutionDecision = "validated"
	DecisionSolidified       EvolutionDecision = "solidified"
	DecisionNeedsReview      EvolutionDecision = "needs_review"
	DecisionValidationFailed EvolutionDecision = "validation_failed"
)

type EvolutionEvent struct {
	EventID      string
	CycleID      string
	Stage        EvolutionStage
	AgentID      string
	AssetID      string
	EvidenceHash string
	Decision     EvolutionDecision
	CreatedAt    time.Time
}

type EvolutionEventLog struct {
	audit port.AuditLog
}

func NewEvolutionEventLog(audit port.AuditLog) *EvolutionEventLog {
	return &EvolutionEventLog{audit: audit}
}

func (l *EvolutionEventLog) Append(ctx context.Context, event EvolutionEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l == nil || l.audit == nil {
		return nil
	}
	return l.audit.Append(ctx, domain.AuditEvent{
		ID:          event.EventID,
		RequestID:   event.CycleID,
		SubjectType: "evolution_event",
		SubjectID:   event.AssetID,
		Action:      string(event.Stage) + ":" + string(event.Decision) + ":" + event.EvidenceHash,
		CreatedAt:   event.CreatedAt,
	})
}

type GepCycleConfig struct {
	Extractor       *SignalExtractor
	Distiller       DistillationOperator
	CapabilityStore *memory.CapabilityMemoryStore
	EventLog        *EvolutionEventLog
}

type GepCycle struct {
	extractor       *SignalExtractor
	distiller       DistillationOperator
	capabilityStore *memory.CapabilityMemoryStore
	eventLog        *EvolutionEventLog
}

type DistillationOperator interface {
	Distill(ctx context.Context, intent ChangeIntent) (memory.Gene, error)
}

type DistillationOperatorFunc func(ctx context.Context, intent ChangeIntent) (memory.Gene, error)

func (f DistillationOperatorFunc) Distill(ctx context.Context, intent ChangeIntent) (memory.Gene, error) {
	return f(ctx, intent)
}

type ChangeIntent struct {
	ID       string
	AgentID  string
	TaskID   string
	Focus    SignalKind
	Goal     string
	Evidence string
	Tags     []string
	Review   bool
}

type GepResult struct {
	CycleID    string
	Signals    []LearningSignal
	Intent     ChangeIntent
	Gene       memory.Gene
	Capsule    memory.Capsule
	Events     []EvolutionEvent
	Solidified bool
	Decision   EvolutionDecision
}

func NewGepCycle(cfg GepCycleConfig) *GepCycle {
	extractor := cfg.Extractor
	if extractor == nil {
		extractor = NewSignalExtractor()
	}
	distiller := cfg.Distiller
	if distiller == nil {
		distiller = DefaultDistillationOperator{}
	}
	return &GepCycle{
		extractor:       extractor,
		distiller:       distiller,
		capabilityStore: cfg.CapabilityStore,
		eventLog:        cfg.EventLog,
	}
}

func (c *GepCycle) Run(ctx context.Context, input ExtractionInput) (GepResult, error) {
	if err := ctx.Err(); err != nil {
		return GepResult{}, err
	}
	cycleID := gepCycleID(input)
	result := GepResult{
		CycleID:  cycleID,
		Decision: DecisionRecorded,
	}
	if err := c.appendStage(ctx, &result, StageScan, input.AgentID, input.Task.ID, DecisionRecorded, input.Task.Input); err != nil {
		return GepResult{}, err
	}
	signals, err := c.extractor.Extract(ctx, input)
	if err != nil {
		return GepResult{}, err
	}
	result.Signals = append([]LearningSignal(nil), signals...)
	if err := c.appendStage(ctx, &result, StageSignal, input.AgentID, input.Task.ID, DecisionCandidate, signalsEvidence(signals)); err != nil {
		return GepResult{}, err
	}
	intent := buildIntent(input, signals)
	result.Intent = intent
	if err := c.appendStage(ctx, &result, StageIntent, input.AgentID, intent.ID, DecisionCandidate, intent.Evidence); err != nil {
		return GepResult{}, err
	}
	gene, err := c.distiller.Distill(ctx, intent)
	if err != nil {
		return GepResult{}, fmt.Errorf("distill gene: %w", err)
	}
	result.Gene = gene
	if err := c.appendStage(ctx, &result, StageMutate, input.AgentID, gene.ID, DecisionDrafted, gene.Match+" "+gene.Avoid); err != nil {
		return GepResult{}, err
	}
	validationDecision := validateGene(gene, intent)
	if validationDecision != DecisionValidated {
		result.Decision = validationDecision
		if err := c.appendStage(ctx, &result, StageValidate, input.AgentID, gene.ID, validationDecision, gene.Validation); err != nil {
			return GepResult{}, err
		}
		return c.finishWithoutSolidify(ctx, input, &result, validationDecision)
	}
	if err := c.appendStage(ctx, &result, StageValidate, input.AgentID, gene.ID, DecisionValidated, gene.Validation); err != nil {
		return GepResult{}, err
	}
	if intent.Review {
		result.Decision = DecisionNeedsReview
		return c.finishWithoutSolidify(ctx, input, &result, DecisionNeedsReview)
	}
	capsule := capsuleForGene(input, gene, intent)
	if c.capabilityStore != nil {
		if err := c.capabilityStore.PutGene(ctx, gene); err != nil {
			return GepResult{}, fmt.Errorf("put gene: %w", err)
		}
		if err := c.capabilityStore.PromoteCapsule(ctx, capsule); err != nil {
			return GepResult{}, fmt.Errorf("promote capsule: %w", err)
		}
	}
	result.Capsule = capsule
	result.Solidified = true
	result.Decision = DecisionSolidified
	if err := c.appendStage(ctx, &result, StageSolidify, input.AgentID, gene.ID, DecisionSolidified, capsule.ID); err != nil {
		return GepResult{}, err
	}
	return result, nil
}

func (c *GepCycle) finishWithoutSolidify(ctx context.Context, input ExtractionInput, result *GepResult, decision EvolutionDecision) (GepResult, error) {
	if err := c.appendStage(ctx, result, StageSolidify, input.AgentID, result.Gene.ID, decision, string(decision)); err != nil {
		return GepResult{}, err
	}
	return *result, nil
}

func (c *GepCycle) appendStage(ctx context.Context, result *GepResult, stage EvolutionStage, agentID string, assetID string, decision EvolutionDecision, evidence string) error {
	event := EvolutionEvent{
		EventID:      result.CycleID + "-" + string(stage),
		CycleID:      result.CycleID,
		Stage:        stage,
		AgentID:      agentID,
		AssetID:      assetID,
		EvidenceHash: contentHash(evidence),
		Decision:     decision,
		CreatedAt:    time.Now(),
	}
	if c.eventLog != nil {
		if err := c.eventLog.Append(ctx, event); err != nil {
			return err
		}
	}
	result.Events = append(result.Events, event)
	return nil
}

type DefaultDistillationOperator struct{}

func (DefaultDistillationOperator) Distill(ctx context.Context, intent ChangeIntent) (memory.Gene, error) {
	if err := ctx.Err(); err != nil {
		return memory.Gene{}, err
	}
	id := "gene-" + contentHash(intent.ID + intent.Goal)[:12]
	gene := memory.Gene{
		ID:          id,
		Status:      memory.GeneStatusActive,
		Tags:        append([]string(nil), intent.Tags...),
		Match:       intent.Goal,
		UseWhen:     "task evidence matches " + string(intent.Focus),
		Plan:        "use the focused signal as the single improvement target before changing behavior",
		Avoid:       "avoid broad multi-goal changes; do not bypass safety or approval gates",
		Constraints: "keep the next attempt scoped to the signal evidence",
		Validation:  "rerun the failing task or its closest automated verification",
		SuccessRate: 0.6,
	}
	gene.Version = ContentAddressedGeneVersion(GeneSixTupleFromMemory(gene))
	return gene, nil
}

func buildIntent(input ExtractionInput, signals []LearningSignal) ChangeIntent {
	focus := SignalFailure
	evidence := input.Task.Input
	review := false
	if len(signals) > 0 {
		focus = signals[0].Kind
		evidence = signals[0].Evidence
	}
	for _, signal := range signals {
		if isCriticalSignal(signal.Kind) {
			focus = signal.Kind
			evidence = signal.Evidence
			review = true
			break
		}
	}
	tags := []string{strings.ReplaceAll(string(focus), "_", "-")}
	if focus == SignalFailure || focus == SignalHardLoopFailure {
		tags = append(tags, "failure")
	}
	return ChangeIntent{
		ID:       "intent-" + contentHash(input.Task.ID + string(focus) + evidence)[:12],
		AgentID:  input.AgentID,
		TaskID:   input.Task.ID,
		Focus:    focus,
		Goal:     intentGoal(input, focus),
		Evidence: evidence,
		Tags:     tags,
		Review:   review,
	}
}

func intentGoal(input ExtractionInput, focus SignalKind) string {
	taskText := strings.TrimSpace(input.Task.Input)
	if taskText == "" {
		taskText = input.Task.ID
	}
	return "improve handling for " + string(focus) + " in " + taskText
}

func validateGene(gene memory.Gene, intent ChangeIntent) EvolutionDecision {
	if intent.Review {
		return DecisionNeedsReview
	}
	if strings.TrimSpace(gene.ID) == "" ||
		strings.TrimSpace(gene.Match) == "" ||
		strings.TrimSpace(gene.UseWhen) == "" ||
		strings.TrimSpace(gene.Plan) == "" ||
		strings.TrimSpace(gene.Avoid) == "" ||
		strings.TrimSpace(gene.Validation) == "" {
		return DecisionValidationFailed
	}
	return DecisionValidated
}

func capsuleForGene(input ExtractionInput, gene memory.Gene, intent ChangeIntent) memory.Capsule {
	return memory.Capsule{
		ID:           "capsule-" + contentHash(input.Task.ID + gene.ID)[:12],
		GeneIDs:      []string{gene.ID},
		Query:        input.Task.Input,
		Tags:         append([]string(nil), intent.Tags...),
		Outcome:      string(intent.Focus),
		SuccessCount: 1,
		Confidence:   gene.SuccessRate,
		CreatedAt:    time.Now(),
	}
}

func isCriticalSignal(kind SignalKind) bool {
	switch kind {
	case SignalPermissionViolation, SignalSecretExposure:
		return true
	default:
		return false
	}
}

func signalsEvidence(signals []LearningSignal) string {
	var parts []string
	for _, signal := range signals {
		parts = append(parts, string(signal.Kind), signal.Evidence)
	}
	return strings.Join(parts, "\n")
}

func gepCycleID(input ExtractionInput) string {
	return "gep-" + contentHash(fmt.Sprintf("%s:%s:%d", input.AgentID, input.Task.ID, input.Cycle))[:12]
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
