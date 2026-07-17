package domain

import (
	"testing"
	"time"
)

func TestAgentMessageFromTaskEventFields(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	message := AgentMessageFromTaskEventFields(AgentMessageTaskEventFields{
		CompanyID:      "company-1",
		EventID:        "evt-1",
		TaskID:         "TASK-20260524-001",
		EventType:      "handoff.appended",
		FromAgentID:    "researcher",
		ToAgentID:      "writer",
		ActorAgentID:   "researcher",
		Summary:        "请整理调研结果",
		Artifact:       "docs/research/cache.md",
		CreatedAt:      createdAt,
		IdempotencyKey: "handoff-1",
	})

	if message.ID != "handoff-1" || message.SourceEventID != "evt-1" || message.ThreadID != "TASK-20260524-001" {
		t.Fatalf("AgentMessageFromTaskEventFields() identity = %#v, want id/source/thread", message)
	}
	if message.Type != AgentMessageTypeHandoff || message.Status != AgentMessageUnread {
		t.Fatalf("AgentMessageFromTaskEventFields() type/status = %q/%q, want handoff/unread", message.Type, message.Status)
	}
	if message.FromAgentID != "researcher" || message.ToAgentID != "writer" || message.Summary != "请整理调研结果" {
		t.Fatalf("AgentMessageFromTaskEventFields() routing = %#v, want researcher -> writer", message)
	}
}
