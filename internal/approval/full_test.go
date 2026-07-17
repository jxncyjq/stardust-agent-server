package approval

import (
	"context"
	"errors"
	"testing"
)

func TestServiceOpensFullApprovalGateTypes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service := NewService()

	for _, ticketType := range []TicketType{
		TicketHardLoop,
		TicketKnowledgeReview,
		TicketBudgetExceeded,
		TicketModelUpgrade,
		TicketDangerousTool,
		TicketWorkflowHumanGate,
		TicketSkillInstall,
	} {
		ticket, err := service.OpenTicket(ctx, OpenTicketRequest{
			Type:      ticketType,
			SubjectID: "subject-" + string(ticketType),
			Reason:    "manual review required",
		})
		if err != nil {
			t.Fatalf("OpenTicket(%s) error = %v, want nil", ticketType, err)
		}
		if ticket.Type != ticketType {
			t.Errorf("OpenTicket(%s) type = %s, want %s", ticketType, ticket.Type, ticketType)
		}
		if ticket.Status != TicketOpen {
			t.Errorf("OpenTicket(%s) status = %s, want %s", ticketType, ticket.Status, TicketOpen)
		}
	}
}

func TestServiceDecideByRequiresHumanApprover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service := NewService()
	ticket, err := service.OpenTicket(ctx, OpenTicketRequest{
		Type:      TicketDangerousTool,
		SubjectID: "tool-call-1",
		Reason:    "dangerous command",
	})
	if err != nil {
		t.Fatalf("OpenTicket() error = %v, want nil", err)
	}

	_, err = service.DecideBy(ctx, ticket.ID, DecisionApprove, "", "approve")
	if !errors.Is(err, ErrHumanApproverRequired) {
		t.Fatalf("DecideBy(%q) error = %v, want %v", ticket.ID, err, ErrHumanApproverRequired)
	}

	approved, err := service.DecideBy(ctx, ticket.ID, DecisionApprove, "human-1", "approve")
	if err != nil {
		t.Fatalf("DecideBy(%q) error = %v, want nil", ticket.ID, err)
	}
	if approved.DeciderID != "human-1" {
		t.Errorf("DecideBy(%q) decider = %q, want %q", ticket.ID, approved.DeciderID, "human-1")
	}
	if approved.Status != TicketApproved {
		t.Errorf("DecideBy(%q) status = %s, want %s", ticket.ID, approved.Status, TicketApproved)
	}
}

func TestServiceRejectsRepeatedDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service := NewService()
	ticket, err := service.OpenTicket(ctx, OpenTicketRequest{
		Type:      TicketBudgetExceeded,
		SubjectID: "task-1",
		Reason:    "budget limit exceeded",
	})
	if err != nil {
		t.Fatalf("OpenTicket() error = %v, want nil", err)
	}
	if _, err := service.DecideBy(ctx, ticket.ID, DecisionDeny, "human-1", "deny"); err != nil {
		t.Fatalf("DecideBy(%q) error = %v, want nil", ticket.ID, err)
	}

	_, err = service.DecideBy(ctx, ticket.ID, DecisionApprove, "human-2", "approve")
	if !errors.Is(err, ErrTicketAlreadyDecided) {
		t.Fatalf("DecideBy(%q) repeated error = %v, want %v", ticket.ID, err, ErrTicketAlreadyDecided)
	}
}

func TestServiceRejectsUnknownDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service := NewService()
	ticket, err := service.OpenTicket(ctx, OpenTicketRequest{
		Type:      TicketSkillInstall,
		SubjectID: "skill-1",
		Reason:    "install new skill",
	})
	if err != nil {
		t.Fatalf("OpenTicket() error = %v, want nil", err)
	}

	_, err = service.DecideBy(ctx, ticket.ID, Decision("maybe"), "human-1", "not sure")
	if !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("DecideBy(%q) error = %v, want %v", ticket.ID, err, ErrInvalidDecision)
	}
}
