package evolution

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

const RuntimeEventLearning = "learning_event"

const (
	FailureReasonInferenceError  = "inference_error"
	FailureReasonHardLoop        = "hard_loop"
	FailureReasonBudgetExhausted = "budget_exhausted"
	FailureReasonInterrupted     = "interrupted"
	FailureReasonToolError       = "tool_error"
)

type LearningEvent struct {
	AgentID       string
	TaskID        string
	Signal        SignalKind
	Reason        string
	EpisodicRef   string
	IsLightweight bool
	PublishedAt   time.Time
}

func NewLearningRuntimeEvent(event LearningEvent) domain.RuntimeEvent {
	if event.PublishedAt.IsZero() {
		event.PublishedAt = time.Now()
	}
	return domain.RuntimeEvent{
		Type:   RuntimeEventLearning,
		TaskID: event.TaskID,
		Message: fmt.Sprintf(
			"agent_id=%s signal=%s reason=%s episodic_ref=%s lightweight=%t",
			event.AgentID,
			event.Signal,
			event.Reason,
			event.EpisodicRef,
			event.IsLightweight,
		),
		CreatedAt: event.PublishedAt,
	}
}

func ParseLearningRuntimeEvent(event domain.RuntimeEvent) (LearningEvent, bool) {
	if event.Type != RuntimeEventLearning {
		return LearningEvent{}, false
	}
	fields := make(map[string]string)
	for part := range strings.FieldsSeq(event.Message) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		fields[key] = value
	}
	lightweight, _ := strconv.ParseBool(fields["lightweight"])
	return LearningEvent{
		AgentID:       fields["agent_id"],
		TaskID:        event.TaskID,
		Signal:        SignalKind(fields["signal"]),
		Reason:        fields["reason"],
		EpisodicRef:   fields["episodic_ref"],
		IsLightweight: lightweight,
		PublishedAt:   event.CreatedAt,
	}, fields["signal"] != ""
}
