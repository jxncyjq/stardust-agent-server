package cli

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
)

// fakeMessageStore is an in-memory AgentMessageStore that upserts by id, matching
// the persistence semantics the reinjection job relies on for idempotency.
type fakeMessageStore struct {
	byID map[string]domain.AgentMessage
}

func newFakeMessageStore() *fakeMessageStore {
	return &fakeMessageStore{byID: make(map[string]domain.AgentMessage)}
}

func (s *fakeMessageStore) SaveAgentMessage(_ context.Context, m domain.AgentMessage) error {
	s.byID[m.ID] = m
	return nil
}

func (s *fakeMessageStore) ListAgentMessages(_ context.Context, q domain.AgentMessageQuery) ([]domain.AgentMessage, error) {
	var out []domain.AgentMessage
	for _, m := range s.byID {
		if q.TaskID == "" || m.TaskID == q.TaskID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *fakeMessageStore) MarkAgentMessageRead(context.Context, string, time.Time) error { return nil }

func TestSubtaskReinjectionJobRoutesResultToParent(t *testing.T) {
	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	store := newFakeMessageStore()

	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:      "subtask_completed",
		TaskID:    "task-7:run:sub-1",
		Message:   "子任务摘要：完成",
		CreatedAt: time.Unix(10, 0),
	}); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	// An unrelated event must be ignored.
	if err := events.Publish(ctx, domain.RuntimeEvent{Type: "task_completed", TaskID: "other"}); err != nil {
		t.Fatalf("Publish(other) error = %v, want nil", err)
	}

	job := newSubtaskReinjectionJob(events, store)
	if err := job(ctx); err != nil {
		t.Fatalf("reinjection job error = %v, want nil", err)
	}

	msgs, err := store.ListAgentMessages(ctx, domain.AgentMessageQuery{TaskID: "task-7:run"})
	if err != nil {
		t.Fatalf("ListAgentMessages() error = %v, want nil", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("parent messages = %d, want 1", len(msgs))
	}
	got := msgs[0]
	if got.Type != domain.AgentMessageTypeResult || got.Summary != "子任务摘要：完成" || got.SourceEventID != "task-7:run:sub-1" {
		t.Fatalf("reinjected message = %+v, want result carrying subtask summary", got)
	}

	// Idempotent: a second run does not duplicate the message.
	if err := job(ctx); err != nil {
		t.Fatalf("second reinjection job error = %v, want nil", err)
	}
	if len(store.byID) != 1 {
		t.Fatalf("stored messages = %d, want 1 (idempotent)", len(store.byID))
	}
}
