package quality

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/memory"
)

func TestDegradationGovernorFreezesGenesAndOpensApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := memory.NewCapabilityMemoryStore()
	approvals := approval.NewService()
	gene := memory.Gene{
		ID:          "gene-regressed",
		Version:     "1.0.0",
		Status:      memory.GeneStatusActive,
		Tags:        []string{"go", "test"},
		Match:       "go test task",
		UseWhen:     "when Go tests fail",
		Plan:        "run focused tests",
		Avoid:       "avoid broad changes",
		Validation:  "go test ./...",
		SuccessRate: 0.3,
	}
	if err := store.PutGene(ctx, gene); err != nil {
		t.Fatalf("PutGene(%q) error = %v, want nil", gene.ID, err)
	}
	governor := NewDegradationGovernor(DegradationGovernorConfig{
		CapabilityStore: store,
		Approvals:       approvals,
	})

	result, err := governor.HandleAlert(ctx, DegradationGovernanceRequest{
		Alert: DegradationAlert{
			AgentID:     "agent-1",
			QualityDrop: 0.32,
			WindowFrom:  time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC),
			WindowTo:    time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC),
			Reason:      "task quality dropped within evaluation window",
		},
		GeneIDs: []string{gene.ID},
	})
	if err != nil {
		t.Fatalf("HandleAlert() error = %v, want nil", err)
	}
	if len(result.FrozenGeneIDs) != 1 || result.FrozenGeneIDs[0] != gene.ID {
		t.Fatalf("HandleAlert().FrozenGeneIDs = %#v, want [%s]", result.FrozenGeneIDs, gene.ID)
	}
	hits, err := store.SearchGenes(ctx, memory.CapabilityQuery{Text: "go test", TopK: 1})
	if err != nil {
		t.Fatalf("SearchGenes() error = %v, want nil", err)
	}
	if len(hits) != 0 {
		t.Fatalf("SearchGenes() len = %d, want 0 after degradation freeze", len(hits))
	}
	tickets := approvals.Tickets()
	if len(tickets) != 1 {
		t.Fatalf("Tickets() len = %d, want 1", len(tickets))
	}
	if tickets[0].Type != approval.TicketWorkflowHumanGate {
		t.Fatalf("Tickets()[0].Type = %s, want %s", tickets[0].Type, approval.TicketWorkflowHumanGate)
	}
	if !strings.Contains(tickets[0].Reason, "gene-regressed") {
		t.Fatalf("Tickets()[0].Reason = %q, want gene id", tickets[0].Reason)
	}
	if result.Ticket.ID != tickets[0].ID {
		t.Fatalf("HandleAlert().Ticket.ID = %q, want %q", result.Ticket.ID, tickets[0].ID)
	}
}
