package taskledger

import "time"

const (
	EventTaskCreated       = "task.created"
	EventTaskClaimed       = "task.claimed"
	EventTaskStatusChanged = "task.status_changed"
	EventMessageAppended   = "message.appended"
	EventResultAppended    = "result.appended"
	EventHandoffAppended   = "handoff.appended"
	EventReviewAppended    = "review.appended"
	EventConflictPrefix    = "conflict."
)

const schemaVersion = 1

type Event struct {
	EventID        string    `json:"event_id"`
	TaskID         string    `json:"task_id"`
	Type           string    `json:"type"`
	SchemaVersion  int       `json:"schema_version"`
	From           string    `json:"from,omitempty"`
	To             string    `json:"to,omitempty"`
	ActorAgentID   string    `json:"actor_agent_id,omitempty"`
	CorrelationID  string    `json:"correlation_id,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	Title          string    `json:"title,omitempty"`
	Status         string    `json:"status,omitempty"`
	Owner          string    `json:"owner,omitempty"`
	Summary        string    `json:"summary,omitempty"`
	Artifact       string    `json:"artifact,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type Task struct {
	ID           string
	Title        string
	Status       string
	Owner        string
	Participants map[string]bool
	Summary      string
	Artifact     string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Messages     []Event
	Conflicts    []Event
}

func (t Task) ParticipantList() []string {
	participants := make([]string, 0, len(t.Participants))
	for participant := range t.Participants {
		participants = append(participants, participant)
	}
	return participants
}
