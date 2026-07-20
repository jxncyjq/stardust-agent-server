package tool

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// seedTwoTenants fills a store with messages belonging to two agents in two
// companies, so an isolation failure shows up as another tenant's text leaking
// into the output rather than as a subtle count mismatch.
func seedTwoTenants(t *testing.T) *memoryAgentMessageStore {
	t.Helper()
	store := newMemoryAgentMessageStore()
	ctx := context.Background()
	seed := []domain.AgentMessage{
		{ID: "m-own", CompanyID: "company-1", FromAgentID: "writer", ToAgentID: "researcher", Type: domain.AgentMessageTypeMessage, Status: domain.AgentMessageUnread, Summary: "OWN-INBOX"},
		{ID: "m-sent", CompanyID: "company-1", FromAgentID: "researcher", ToAgentID: "writer", Type: domain.AgentMessageTypeMessage, Status: domain.AgentMessageUnread, Summary: "OWN-OUTBOX"},
		{ID: "m-peer", CompanyID: "company-1", FromAgentID: "writer", ToAgentID: "auditor", Type: domain.AgentMessageTypeMessage, Status: domain.AgentMessageUnread, Summary: "PEER-PRIVATE"},
		{ID: "m-other-co", CompanyID: "company-2", FromAgentID: "spy", ToAgentID: "researcher", Type: domain.AgentMessageTypeMessage, Status: domain.AgentMessageUnread, Summary: "OTHER-TENANT"},
	}
	for _, message := range seed {
		if err := store.SaveAgentMessage(ctx, message); err != nil {
			t.Fatalf("SaveAgentMessage(%s) error = %v, want nil", message.ID, err)
		}
	}
	return store
}

func newIsolationRegistry(t *testing.T, store *memoryAgentMessageStore) *Registry {
	t.Helper()
	registry := NewWorkspaceRegistry(t.TempDir(), nil)
	RegisterAgentMessageTools(registry, store)
	return registry
}

var researcher = domain.Agent{ID: "researcher", CompanyID: "company-1", Role: "developer"}

// Regression (critical): read_messages with no arguments used to hand the store
// an all-empty query. Every filter is "empty matches anything", so it returned
// the first 100 rows of the whole table — every agent, every tenant.
func TestReadMessagesWithoutFiltersReturnsOnlyOwnInbox(t *testing.T) {
	t.Parallel()

	registry := newIsolationRegistry(t, seedTwoTenants(t))

	result, err := registry.Execute(context.Background(), researcher, domain.ToolCall{
		ID:        "read-1",
		Name:      "read_messages",
		Arguments: map[string]string{},
	})
	if err != nil {
		t.Fatalf("Execute(read_messages) error = %v, want nil", err)
	}
	if !strings.Contains(result.Output, "OWN-INBOX") {
		t.Errorf("own inbox message missing from output:\n%s", result.Output)
	}
	for _, leaked := range []string{"PEER-PRIVATE", "OTHER-TENANT"} {
		if strings.Contains(result.Output, leaked) {
			t.Errorf("read_messages leaked %q to a caller it does not belong to:\n%s", leaked, result.Output)
		}
	}
}

// Asking for someone else's inbox is an escalation attempt, not a typo: it must
// fail loudly rather than being silently rewritten to the caller's own id.
func TestReadMessagesRejectsForeignRecipient(t *testing.T) {
	t.Parallel()

	registry := newIsolationRegistry(t, seedTwoTenants(t))

	_, err := registry.Execute(context.Background(), researcher, domain.ToolCall{
		ID:        "read-2",
		Name:      "read_messages",
		Arguments: map[string]string{"to": "auditor"},
	})
	if err == nil {
		t.Fatalf("Execute(read_messages to=auditor) error = nil, want a refusal")
	}
}

// Reading one's own outbox stays allowed — the caller is a party to those
// messages. Without this the fix would break a legitimate use of the tool.
func TestReadMessagesAllowsOwnOutbox(t *testing.T) {
	t.Parallel()

	registry := newIsolationRegistry(t, seedTwoTenants(t))

	result, err := registry.Execute(context.Background(), researcher, domain.ToolCall{
		ID:        "read-3",
		Name:      "read_messages",
		Arguments: map[string]string{"from": "researcher"},
	})
	if err != nil {
		t.Fatalf("Execute(read_messages from=self) error = %v, want nil", err)
	}
	if !strings.Contains(result.Output, "OWN-OUTBOX") {
		t.Errorf("own outbox message missing from output:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "PEER-PRIVATE") {
		t.Errorf("outbox read leaked a peer's message:\n%s", result.Output)
	}
}

// company_id is a plain tool argument the model fills in, so it cannot be
// trusted as a tenant boundary; the caller's own company must win.
func TestReadMessagesIgnoresCallerSuppliedCompany(t *testing.T) {
	t.Parallel()

	registry := newIsolationRegistry(t, seedTwoTenants(t))

	result, err := registry.Execute(context.Background(), researcher, domain.ToolCall{
		ID:        "read-4",
		Name:      "read_messages",
		Arguments: map[string]string{"company_id": "company-2"},
	})
	if err != nil {
		t.Fatalf("Execute(read_messages company_id=other) error = %v, want nil", err)
	}
	if strings.Contains(result.Output, "OTHER-TENANT") {
		t.Errorf("a caller-supplied company_id crossed the tenant boundary:\n%s", result.Output)
	}
}

// The sender is an identity claim, not free-form data: a message that says it
// came from someone else makes every downstream trust decision unsound.
// Refused rather than silently rewritten, so the attempt stays visible —
// matching how read_messages treats a foreign recipient.
func TestSendMessageRejectsForgedSender(t *testing.T) {
	t.Parallel()

	store := seedTwoTenants(t)
	registry := newIsolationRegistry(t, store)

	_, err := registry.Execute(context.Background(), researcher, domain.ToolCall{
		ID:   "send-1",
		Name: "send_message",
		Arguments: map[string]string{
			"from":    "auditor",
			"to":      "writer",
			"summary": "forged",
		},
	})
	if err == nil {
		t.Fatalf("Execute(send_message from=auditor) error = nil, want a refusal")
	}

	messages, listErr := store.ListAgentMessages(context.Background(), domain.AgentMessageQuery{ToAgentID: "writer"})
	if listErr != nil {
		t.Fatalf("ListAgentMessages error = %v, want nil", listErr)
	}
	for _, message := range messages {
		if message.Summary == "forged" {
			t.Fatalf("a refused send still persisted a message: %#v", message)
		}
	}
}

// Omitting the sender is the normal path: it is filled from the caller, and the
// tenant comes from the caller too — never from the arguments.
func TestSendMessageStampsCallerAsSender(t *testing.T) {
	t.Parallel()

	store := seedTwoTenants(t)
	registry := newIsolationRegistry(t, store)

	if _, err := registry.Execute(context.Background(), researcher, domain.ToolCall{
		ID:   "send-2",
		Name: "send_message",
		Arguments: map[string]string{
			"to":         "writer",
			"summary":    "authentic",
			"company_id": "company-2", // must be ignored in favour of the caller's
		},
	}); err != nil {
		t.Fatalf("Execute(send_message) error = %v, want nil", err)
	}

	messages, err := store.ListAgentMessages(context.Background(), domain.AgentMessageQuery{ToAgentID: "writer"})
	if err != nil {
		t.Fatalf("ListAgentMessages error = %v, want nil", err)
	}
	var found bool
	for _, message := range messages {
		if message.Summary != "authentic" {
			continue
		}
		found = true
		if message.FromAgentID != "researcher" {
			t.Errorf("stored sender = %q, want the real caller %q", message.FromAgentID, "researcher")
		}
		if message.CompanyID != "company-1" {
			t.Errorf("stored company = %q, want the caller's %q (arguments must not decide the tenant)", message.CompanyID, "company-1")
		}
	}
	if !found {
		t.Fatalf("the sent message was not persisted")
	}
}

// Without a caller the tools cannot enforce anything. Refusing is the only safe
// answer — falling back to "no filter" is exactly the bug being fixed.
func TestMessageToolsRefuseWithoutCallerIdentity(t *testing.T) {
	t.Parallel()

	store := seedTwoTenants(t)
	registry := newIsolationRegistry(t, store)
	handler, ok := registry.handlers["read_messages"]
	if !ok {
		t.Fatalf("read_messages handler not registered")
	}

	// Bypass Registry.Execute so the context carries no caller.
	if _, err := handler.Execute(context.Background(), domain.ToolCall{ID: "read-5", Name: "read_messages"}); err == nil {
		t.Fatalf("read_messages without a caller in context: error = nil, want a refusal")
	}
}
