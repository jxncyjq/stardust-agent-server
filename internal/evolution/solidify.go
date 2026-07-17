package evolution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/memory"
)

var (
	ErrValidationRequired = errors.New("validation required")
	ErrBlastRadiusTooHigh = errors.New("blast radius too high")
)

type SolidifyPipelineConfig struct {
	CapabilityStore *memory.CapabilityMemoryStore
	EventLog        *SealedEvolutionEventLog
	MaxBlastRadius  int
}

type SolidifyPipeline struct {
	capabilityStore *memory.CapabilityMemoryStore
	eventLog        *SealedEvolutionEventLog
	maxBlastRadius  int
}

type SolidifyRequest struct {
	CycleID     string
	AgentID     string
	Gene        memory.Gene
	Query       string
	BlastRadius int
}

type SolidifyResult struct {
	Gene       memory.Gene
	Capsule    memory.Capsule
	Event      SealedEvolutionEvent
	Solidified bool
}

func NewSolidifyPipeline(cfg SolidifyPipelineConfig) *SolidifyPipeline {
	return &SolidifyPipeline{
		capabilityStore: cfg.CapabilityStore,
		eventLog:        cfg.EventLog,
		maxBlastRadius:  cfg.MaxBlastRadius,
	}
}

func (p *SolidifyPipeline) Solidify(ctx context.Context, req SolidifyRequest) (SolidifyResult, error) {
	if err := ctx.Err(); err != nil {
		return SolidifyResult{}, err
	}
	gene := req.Gene
	if strings.TrimSpace(gene.Validation) == "" {
		return SolidifyResult{}, ErrValidationRequired
	}
	if p.maxBlastRadius > 0 && req.BlastRadius > p.maxBlastRadius {
		return SolidifyResult{}, ErrBlastRadiusTooHigh
	}
	tuple := GeneSixTupleFromMemory(gene)
	if strings.TrimSpace(tuple.Alpha) == "" {
		return SolidifyResult{}, fmt.Errorf("validate gene alpha: %w", ErrValidationRequired)
	}
	if gene.Version == "" {
		gene.Version = ContentAddressedGeneVersion(tuple)
	}
	if gene.Status == "" || gene.Status == memory.GeneStatusDraft {
		gene.Status = memory.GeneStatusActive
	}
	if gene.SuccessRate <= 0 {
		gene.SuccessRate = 0.5
	}
	if gene.SuccessRate > 1 {
		gene.SuccessRate = 1
	}
	if p.capabilityStore != nil {
		if err := p.capabilityStore.PutGene(ctx, gene); err != nil {
			return SolidifyResult{}, fmt.Errorf("put gene: %w", err)
		}
	}
	capsule := memory.Capsule{
		ID:           "capsule-" + contentHash(req.CycleID + gene.ID)[:12],
		GeneIDs:      []string{gene.ID},
		Query:        req.Query,
		Tags:         append([]string(nil), gene.Tags...),
		Outcome:      string(DecisionSolidified),
		SuccessCount: 1,
		Confidence:   gene.SuccessRate,
		CreatedAt:    time.Now(),
	}
	if p.capabilityStore != nil {
		if err := p.capabilityStore.PromoteCapsule(ctx, capsule); err != nil {
			return SolidifyResult{}, fmt.Errorf("promote capsule: %w", err)
		}
	}
	var sealed SealedEvolutionEvent
	if p.eventLog != nil {
		event := EvolutionEvent{
			EventID:      req.CycleID + "-solidify",
			CycleID:      req.CycleID,
			Stage:        StageSolidify,
			AgentID:      req.AgentID,
			AssetID:      gene.ID,
			EvidenceHash: contentHash(gene.ID + gene.Version + capsule.ID),
			Decision:     DecisionSolidified,
			CreatedAt:    time.Now(),
		}
		var err error
		sealed, err = p.eventLog.Append(ctx, event)
		if err != nil {
			return SolidifyResult{}, fmt.Errorf("append solidify event: %w", err)
		}
	}
	return SolidifyResult{
		Gene:       gene,
		Capsule:    capsule,
		Event:      sealed,
		Solidified: true,
	}, nil
}
