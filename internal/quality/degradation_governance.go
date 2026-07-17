package quality

import (
	"context"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/approval"
)

type capabilityFreezer interface {
	FreezeGene(ctx context.Context, geneID string) error
}

type approvalOpener interface {
	OpenTicket(ctx context.Context, req approval.OpenTicketRequest) (approval.Ticket, error)
}

type DegradationGovernorConfig struct {
	CapabilityStore capabilityFreezer
	Approvals       approvalOpener
}

type DegradationGovernor struct {
	capabilityStore capabilityFreezer
	approvals       approvalOpener
}

type DegradationGovernanceRequest struct {
	Alert   DegradationAlert
	GeneIDs []string
}

type DegradationGovernanceResult struct {
	FrozenGeneIDs []string
	Ticket        approval.Ticket
}

func NewDegradationGovernor(cfg DegradationGovernorConfig) DegradationGovernor {
	return DegradationGovernor{
		capabilityStore: cfg.CapabilityStore,
		approvals:       cfg.Approvals,
	}
}

func (g DegradationGovernor) HandleAlert(ctx context.Context, req DegradationGovernanceRequest) (DegradationGovernanceResult, error) {
	if err := ctx.Err(); err != nil {
		return DegradationGovernanceResult{}, err
	}
	var result DegradationGovernanceResult
	for _, geneID := range req.GeneIDs {
		geneID = strings.TrimSpace(geneID)
		if geneID == "" {
			continue
		}
		if g.capabilityStore != nil {
			if err := g.capabilityStore.FreezeGene(ctx, geneID); err != nil {
				return DegradationGovernanceResult{}, fmt.Errorf("freeze degraded gene %q: %w", geneID, err)
			}
		}
		result.FrozenGeneIDs = append(result.FrozenGeneIDs, geneID)
	}
	if g.approvals == nil || len(result.FrozenGeneIDs) == 0 {
		return result, nil
	}
	ticket, err := g.approvals.OpenTicket(ctx, approval.OpenTicketRequest{
		Type:      approval.TicketWorkflowHumanGate,
		SubjectID: req.Alert.AgentID,
		Reason:    degradationApprovalReason(req.Alert, result.FrozenGeneIDs),
	})
	if err != nil {
		return DegradationGovernanceResult{}, fmt.Errorf("open degradation approval: %w", err)
	}
	result.Ticket = ticket
	return result, nil
}

func degradationApprovalReason(alert DegradationAlert, frozenGeneIDs []string) string {
	return fmt.Sprintf(
		"degradation alert for agent %s: quality_drop=%.2f frozen_genes=%s reason=%s",
		alert.AgentID,
		alert.QualityDrop,
		strings.Join(frozenGeneIDs, ","),
		alert.Reason,
	)
}
