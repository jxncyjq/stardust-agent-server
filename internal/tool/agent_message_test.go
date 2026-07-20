package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestRegistryAgentMessageToolsSendReadAndMarkRead(t *testing.T) {
	t.Parallel()

	store := newMemoryAgentMessageStore()
	registry := NewWorkspaceRegistry(t.TempDir(), nil)
	RegisterAgentMessageTools(registry, store)
	agent := domain.Agent{ID: "researcher", CompanyID: "company-1", Role: "developer"}

	send, err := registry.Execute(context.Background(), agent, domain.ToolCall{
		ID:   "send-1",
		Name: "send_message",
		Arguments: map[string]string{
			"company_id": "company-1",
			"task_id":    "TASK-20260524-010",
			"from":       "researcher",
			"to":         "writer",
			"type":       "handoff",
			"summary":    "调研完成，请整理说明",
			"artifact":   "docs/research/cache.md",
		},
	})
	if err != nil {
		t.Fatalf("Registry.Execute(send_message) error = %v, want nil", err)
	}
	if !send.Success || !strings.Contains(send.Output, "sent message") {
		t.Fatalf("Registry.Execute(send_message) = %#v, want success", send)
	}

	read, err := registry.Execute(context.Background(), domain.Agent{ID: "writer", CompanyID: "company-1", Role: "developer"}, domain.ToolCall{
		ID:   "read-1",
		Name: "read_messages",
		Arguments: map[string]string{
			"company_id": "company-1",
			"to":         "writer",
			"status":     "unread",
			"mark_read":  "true",
		},
	})
	if err != nil {
		t.Fatalf("Registry.Execute(read_messages) error = %v, want nil", err)
	}
	for _, want := range []string{"msg-", "researcher -> writer", "handoff", "调研完成，请整理说明", "docs/research/cache.md"} {
		if !strings.Contains(read.Output, want) {
			t.Fatalf("Registry.Execute(read_messages).Output missing %q:\n%s", want, read.Output)
		}
	}

	unread, err := store.ListAgentMessages(context.Background(), domain.AgentMessageQuery{
		CompanyID: "company-1",
		ToAgentID: "writer",
		Status:    domain.AgentMessageUnread,
	})
	if err != nil {
		t.Fatalf("ListAgentMessages(unread) error = %v, want nil", err)
	}
	if len(unread) != 0 {
		t.Fatalf("ListAgentMessages(unread) len = %d, want 0 after mark_read", len(unread))
	}
}

func TestWorkspaceRegistryAgentMessageToolSchemasAreOpenAICompatibleObjects(t *testing.T) {
	t.Parallel()

	registry := NewWorkspaceRegistry(t.TempDir(), nil)
	RegisterAgentMessageTools(registry, newMemoryAgentMessageStore())
	want := map[string]bool{"send_message": false, "read_messages": false}
	for _, descriptor := range registry.Descriptors() {
		if _, ok := want[descriptor.Name]; !ok {
			continue
		}
		want[descriptor.Name] = true
		if got, _ := descriptor.InputSchema["type"].(string); got != "object" {
			t.Fatalf("Descriptor(%s).InputSchema[type] = %q, want object", descriptor.Name, got)
		}
		if required, ok := descriptor.InputSchema["required"]; ok && required == nil {
			t.Fatalf("Descriptor(%s).InputSchema[required] = nil, want omitted or array", descriptor.Name)
		}
		encoded, err := json.Marshal(descriptor.InputSchema)
		if err != nil {
			t.Fatalf("Marshal(Descriptor(%s).InputSchema) error = %v, want nil", descriptor.Name, err)
		}
		if strings.Contains(string(encoded), `"required":null`) {
			t.Fatalf("Descriptor(%s).InputSchema JSON = %s, want no required:null", descriptor.Name, encoded)
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("Registry.Descriptors() missing %s", name)
		}
	}
}

type memoryAgentMessageStore struct {
	messages []domain.AgentMessage
}

func newMemoryAgentMessageStore() *memoryAgentMessageStore {
	return &memoryAgentMessageStore{}
}

func (s *memoryAgentMessageStore) SaveAgentMessage(_ context.Context, message domain.AgentMessage) error {
	s.messages = append(s.messages, message)
	return nil
}

func (s *memoryAgentMessageStore) ListAgentMessages(_ context.Context, query domain.AgentMessageQuery) ([]domain.AgentMessage, error) {
	var out []domain.AgentMessage
	for _, message := range s.messages {
		// Mirror SQLiteRepository.ListAgentMessages exactly: every filter is
		// "empty means match anything". A fake that filters on fewer fields than
		// the real store lets a test pass against behaviour production does not
		// have — which is how an isolation gap would slip through unnoticed.
		if query.CompanyID != "" && message.CompanyID != query.CompanyID {
			continue
		}
		if query.TaskID != "" && message.TaskID != query.TaskID {
			continue
		}
		if query.ThreadID != "" && message.ThreadID != query.ThreadID {
			continue
		}
		if query.FromAgentID != "" && message.FromAgentID != query.FromAgentID {
			continue
		}
		if query.ToAgentID != "" && message.ToAgentID != query.ToAgentID {
			continue
		}
		if query.Status != "" && message.Status != query.Status {
			continue
		}
		if query.SourceEventID != "" && message.SourceEventID != query.SourceEventID {
			continue
		}
		out = append(out, message)
	}
	return out, nil
}

func (s *memoryAgentMessageStore) MarkAgentMessageRead(_ context.Context, messageID string, readAt time.Time) error {
	for i := range s.messages {
		if s.messages[i].ID == messageID {
			s.messages[i].Status = domain.AgentMessageRead
			s.messages[i].ReadAt = readAt
		}
	}
	return nil
}
