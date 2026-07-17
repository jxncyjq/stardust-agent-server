package approval

import (
	"context"
	"testing"
)

func TestServiceApproveHardLoopTicket(t *testing.T) {
	t.Parallel()

	service := NewService()
	ticket, err := service.OpenTicket(context.Background(), OpenTicketRequest{
		Type:      TicketHardLoop,
		SubjectID: "task-1",
		Reason:    "loop detected",
	})
	if err != nil {
		t.Fatalf("OpenTicket() error = %v, want nil", err)
	}
	if ticket.Status != TicketOpen {
		t.Errorf("OpenTicket() status = %q, want %q", ticket.Status, TicketOpen)
	}

	approved, err := service.Decide(context.Background(), ticket.ID, DecisionApprove, "resume")
	if err != nil {
		t.Fatalf("Decide(%q) error = %v, want nil", ticket.ID, err)
	}
	if approved.Status != TicketApproved {
		t.Errorf("Decide(%q) status = %q, want %q", ticket.ID, approved.Status, TicketApproved)
	}
}
