package approval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A ticket has to say WHO wants this call approved. Once plugins can ask for
// approval, "a human is being asked to approve write_file" is not enough to
// act on: the operator needs to know whether the host's own sensitivity rule
// or some installed plugin raised it, and why.

func TestOpenKeepsTheRequestersIdentityAndReason(t *testing.T) {
	store := NewToolGateStore(t.TempDir())

	opened, err := store.Open(ToolApproval{
		SessionKey: "s1", TaskID: "t1", ToolCallID: "c1", ToolName: "write_file",
		RequestedBy: "plugin:legion-gatekeeper", Reason: "writes are frozen during the incident",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened.RequestedBy != "plugin:legion-gatekeeper" || opened.Reason != "writes are frozen during the incident" {
		t.Errorf("opened = %+v, want the requester and reason it was opened with", opened)
	}

	got, found, err := store.Get("s1", opened.TicketID, "")
	if err != nil || !found {
		t.Fatalf("Get = (%+v, %t, %v), want the ticket back", got, found, err)
	}
	if got.RequestedBy != "plugin:legion-gatekeeper" || got.Reason == "" {
		t.Errorf("round-tripped ticket = %+v, want the provenance to survive the disk", got)
	}
}

// TestATicketWithNoRequesterReadsAsTheHost pins backward compatibility: every
// ticket written before this field existed is one the host's own Sensitive
// rule opened. Reading those as "unknown" would put a question mark on the
// one source that was never in question.
func TestATicketWithNoRequesterReadsAsTheHost(t *testing.T) {
	root := t.TempDir()
	store := NewToolGateStore(root)
	opened, err := store.Open(ToolApproval{
		SessionKey: "s1", TaskID: "t1", ToolCallID: "c1", ToolName: "write_file",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened.RequestedBy != RequestedByHost {
		t.Errorf("RequestedBy = %q, want %q: a ticket nobody attributed is the host's own",
			opened.RequestedBy, RequestedByHost)
	}

	// And an OLD ticket on disk, written before the field existed, reads the
	// same way.
	// No working_dir on this ticket, so its base is the workspace root itself
	// (sessionstate.SessionBase).
	path := filepath.Join(root, "session", "s1", "approvals", opened.TicketID+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("decode ticket: %v", err)
	}
	delete(legacy, "requested_by")
	delete(legacy, "reason")
	rewritten, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("encode legacy ticket: %v", err)
	}
	if err := os.WriteFile(path, rewritten, 0o644); err != nil {
		t.Fatalf("write legacy ticket: %v", err)
	}

	got, found, err := store.Get("s1", opened.TicketID, "")
	if err != nil || !found {
		t.Fatalf("Get = (%+v, %t, %v), want the legacy ticket back", got, found, err)
	}
	if got.RequestedBy != RequestedByHost {
		t.Errorf("legacy ticket RequestedBy = %q, want %q", got.RequestedBy, RequestedByHost)
	}
}

// TestHostAndPluginTicketsShareOneQueue: two sources, one store, one shape.
// Two parallel suspend mechanisms is the outcome this design exists to avoid.
func TestHostAndPluginTicketsShareOneQueue(t *testing.T) {
	store := NewToolGateStore(t.TempDir())

	if _, err := store.Open(ToolApproval{
		SessionKey: "s1", TaskID: "t1", ToolCallID: "c1", ToolName: "write_file",
	}); err != nil {
		t.Fatalf("Open host ticket: %v", err)
	}
	if _, err := store.Open(ToolApproval{
		SessionKey: "s1", TaskID: "t1", ToolCallID: "c2", ToolName: "read_file",
		RequestedBy: "plugin:legion-gatekeeper", Reason: "reads during an incident are reviewed",
	}); err != nil {
		t.Fatalf("Open plugin ticket: %v", err)
	}

	pending, err := store.ListForTask("s1", "t1", "")
	if err != nil {
		t.Fatalf("ListForTask: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("ListForTask returned %d tickets, want 2 in the one queue", len(pending))
	}
	sources := map[string]bool{}
	for _, ticket := range pending {
		sources[ticket.RequestedBy] = true
	}
	if !sources[RequestedByHost] || !sources["plugin:legion-gatekeeper"] {
		t.Errorf("ticket sources = %v, want both the host's and the plugin's", sources)
	}
}
