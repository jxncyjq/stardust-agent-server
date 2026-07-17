package approval

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type TicketType string

const (
	TicketHardLoop          TicketType = "HardLoop"
	TicketKnowledgeReview   TicketType = "KnowledgeReview"
	TicketBudgetExceeded    TicketType = "BudgetExceeded"
	TicketModelUpgrade      TicketType = "ModelUpgrade"
	TicketDangerousTool     TicketType = "DangerousTool"
	TicketWorkflowHumanGate TicketType = "WorkflowHumanGate"
	TicketSkillInstall      TicketType = "SkillInstall"
)

type TicketStatus string

const (
	TicketOpen     TicketStatus = "open"
	TicketApproved TicketStatus = "approved"
	TicketDenied   TicketStatus = "denied"
)

type Decision string

const (
	DecisionApprove Decision = "approve"
	DecisionDeny    Decision = "deny"
)

var (
	ErrTicketNotFound        = errors.New("ticket not found")
	ErrHumanApproverRequired = errors.New("human approver required")
	ErrTicketAlreadyDecided  = errors.New("ticket already decided")
	ErrInvalidDecision       = errors.New("invalid decision")
)

type OpenTicketRequest struct {
	Type      TicketType
	SubjectID string
	Reason    string
}

type Ticket struct {
	ID        string
	Type      TicketType
	SubjectID string
	Status    TicketStatus
	Decision  Decision
	DeciderID string
	Reason    string
	Comment   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Service struct {
	mu      sync.Mutex
	nextID  int
	tickets map[string]Ticket
}

func NewService() *Service {
	return &Service{tickets: make(map[string]Ticket)}
}

func (s *Service) OpenTicket(ctx context.Context, req OpenTicketRequest) (Ticket, error) {
	if err := ctx.Err(); err != nil {
		return Ticket{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	now := time.Now()
	ticket := Ticket{
		ID:        fmt.Sprintf("ticket-%d", s.nextID),
		Type:      req.Type,
		SubjectID: req.SubjectID,
		Status:    TicketOpen,
		Reason:    req.Reason,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.tickets[ticket.ID] = ticket
	return ticket, nil
}

func (s *Service) Decide(ctx context.Context, ticketID string, decision Decision, comment string) (Ticket, error) {
	return s.decide(ctx, ticketID, decision, "", comment, false)
}

func (s *Service) DecideBy(ctx context.Context, ticketID string, decision Decision, deciderID string, comment string) (Ticket, error) {
	return s.decide(ctx, ticketID, decision, deciderID, comment, true)
}

func (s *Service) decide(ctx context.Context, ticketID string, decision Decision, deciderID string, comment string, requireHuman bool) (Ticket, error) {
	if err := ctx.Err(); err != nil {
		return Ticket{}, err
	}
	if requireHuman && deciderID == "" {
		return Ticket{}, ErrHumanApproverRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ticket, ok := s.tickets[ticketID]
	if !ok {
		return Ticket{}, ErrTicketNotFound
	}
	if ticket.Status != TicketOpen {
		return Ticket{}, ErrTicketAlreadyDecided
	}
	ticket.Decision = decision
	ticket.DeciderID = deciderID
	ticket.Comment = comment
	ticket.UpdatedAt = time.Now()
	switch decision {
	case DecisionApprove:
		ticket.Status = TicketApproved
	case DecisionDeny:
		ticket.Status = TicketDenied
	default:
		return Ticket{}, ErrInvalidDecision
	}
	s.tickets[ticketID] = ticket
	return ticket, nil
}

func (s *Service) Tickets() []Ticket {
	s.mu.Lock()
	defer s.mu.Unlock()
	tickets := make([]Ticket, 0, len(s.tickets))
	for _, ticket := range s.tickets {
		tickets = append(tickets, ticket)
	}
	return tickets
}
