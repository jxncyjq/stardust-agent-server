package manualgate

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/tool"
)

// A plugin granted the decide extension can answer "ask", which means: a human
// must look at this call before it runs. That suspend can only happen here, at
// the round boundary, and it must land in the SAME queue the host's own
// Sensitive-tool approvals use — two parallel suspend mechanisms is the
// outcome this design exists to avoid.

func askingRegistry(t *testing.T, reason string) *tool.Registry {
	t.Helper()

	registry := gateRegistry()
	registry.AddDecider("plugin:legion-gatekeeper", tool.DeciderFunc(
		func(_ context.Context, call domain.ToolCall) tool.Verdict {
			if call.Name != "read_file" {
				return tool.Verdict{Decision: tool.DecisionAllow}
			}
			return tool.Verdict{Decision: tool.DecisionAsk, Reason: reason}
		}))
	return registry
}

// TestAPluginAskSuspendsEvenInAutoMode is the mode decision, stated as a test.
// The host's own Sensitive rule is Manual-only; a plugin's ask is not. An Auto
// deployment that installed a gatekeeper plugin did so precisely so that these
// calls get looked at, and honouring the ask only in Manual would silently
// degrade it to allow.
func TestAPluginAskSuspendsEvenInAutoMode(t *testing.T) {
	store := approval.NewToolGateStore(t.TempDir())
	sink := &spyApprovalSink{}
	gate := New(store, WithApprovalSink(sink))
	tools := askingRegistry(t, "reads are reviewed during the incident")
	task := domain.Task{ID: "task-1", SessionID: "s1", Mode: domain.ModeAuto}
	call := domain.ToolCall{ID: "call-1", Name: "read_file", Arguments: map[string]string{"path": "/tmp/x"}}

	suspend, err := gate.ShouldSuspend(context.Background(), task, []domain.ToolCall{call}, tools)
	if err != nil {
		t.Fatalf("ShouldSuspend: %v", err)
	}
	if !suspend {
		t.Fatal("ShouldSuspend = false; an Auto-mode run must still suspend when a plugin asks")
	}

	ticket, found, err := store.Get("s1", approval.TicketID("task-1", "call-1"), "")
	if err != nil || !found {
		t.Fatalf("Get = (%+v, %t, %v), want the ticket the ask opened", ticket, found, err)
	}
	if ticket.RequestedBy != "plugin:legion-gatekeeper" {
		t.Errorf("ticket.RequestedBy = %q, want the plugin that asked", ticket.RequestedBy)
	}
	if ticket.Reason != "reads are reviewed during the incident" {
		t.Errorf("ticket.Reason = %q, want the plugin's own words", ticket.Reason)
	}
	if len(sink.pending) != 1 {
		t.Errorf("approval_pending emitted %d times, want 1", len(sink.pending))
	}
}

// TestAPluginAskAndAHostSensitiveShareOneQueue: one round, two sources, two
// tickets, one store — and each says who raised it.
func TestAPluginAskAndAHostSensitiveShareOneQueue(t *testing.T) {
	store := approval.NewToolGateStore(t.TempDir())
	gate := New(store)
	tools := askingRegistry(t, "reads are reviewed")
	task := manualTask()
	calls := []domain.ToolCall{
		{ID: "call-1", Name: "read_file"},  // the plugin asks
		{ID: "call-2", Name: "write_file"}, // the host's own Sensitive rule
	}

	suspend, err := gate.ShouldSuspend(context.Background(), task, calls, tools)
	if err != nil {
		t.Fatalf("ShouldSuspend: %v", err)
	}
	if !suspend {
		t.Fatal("ShouldSuspend = false, want true")
	}

	tickets, err := store.ListForTask("s1", "t1", "")
	if err != nil {
		t.Fatalf("ListForTask: %v", err)
	}
	if len(tickets) != 2 {
		t.Fatalf("ListForTask returned %d tickets, want 2", len(tickets))
	}
	bySource := map[string]string{}
	for _, ticket := range tickets {
		bySource[ticket.RequestedBy] = ticket.ToolName
	}
	if bySource["plugin:legion-gatekeeper"] != "read_file" {
		t.Errorf("plugin ticket = %q, want read_file", bySource["plugin:legion-gatekeeper"])
	}
	if bySource[approval.RequestedByHost] != "write_file" {
		t.Errorf("host ticket = %q, want write_file", bySource[approval.RequestedByHost])
	}
}

// TestAnAlreadyDecidedPluginAskDoesNotSuspendAgain: after the human answered
// and the task resumed, the same round comes back through this gate. Opening
// the question again would make the run unresumable.
func TestAnAlreadyDecidedPluginAskDoesNotSuspendAgain(t *testing.T) {
	store := approval.NewToolGateStore(t.TempDir())
	gate := New(store)
	tools := askingRegistry(t, "reads are reviewed")
	task := domain.Task{ID: "task-1", SessionID: "s1", Mode: domain.ModeAuto}
	call := domain.ToolCall{ID: "call-1", Name: "read_file"}

	if _, err := gate.ShouldSuspend(context.Background(), task, []domain.ToolCall{call}, tools); err != nil {
		t.Fatalf("ShouldSuspend: %v", err)
	}
	if _, err := store.Decide("s1", approval.TicketID("task-1", "call-1"), approval.ApprovalApproved, ""); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	suspend, err := gate.ShouldSuspend(context.Background(), task, []domain.ToolCall{call}, tools)
	if err != nil {
		t.Fatalf("ShouldSuspend after the decision: %v", err)
	}
	if suspend {
		t.Error("ShouldSuspend = true after the ask was decided; the run would never resume")
	}
}

// TestAPluginAskOnALazyCallOpensTheTicketForTheInnerCall: under the lazy
// protocol the model calls call_tool, and what actually reaches the registry
// is the INNER call, with its own id and name. The ticket has to be keyed to
// that one, or the arbiter's dispatch-time lookup finds nothing and refuses a
// call a human already approved.
func TestAPluginAskOnALazyCallOpensTheTicketForTheInnerCall(t *testing.T) {
	store := approval.NewToolGateStore(t.TempDir())
	gate := New(store)
	tools := askingRegistry(t, "reads are reviewed")
	task := domain.Task{ID: "task-1", SessionID: "s1", Mode: domain.ModeAuto}
	meta := domain.ToolCall{ID: "call-1", Name: "call_tool", Arguments: map[string]string{
		"tool_name":      "read_file",
		"arguments_json": `{"path":"/tmp/x"}`,
	}}

	if _, err := gate.ShouldSuspend(context.Background(), task, []domain.ToolCall{meta}, tools); err != nil {
		t.Fatalf("ShouldSuspend: %v", err)
	}

	innerID := tool.LazyInnerCallID("call-1", "read_file")
	ticket, found, err := store.Get("s1", approval.TicketID("task-1", innerID), "")
	if err != nil || !found {
		t.Fatalf("Get(inner) = (%+v, %t, %v), want a ticket keyed to the inner call", ticket, found, err)
	}
	if ticket.ToolName != "read_file" {
		t.Errorf("ticket.ToolName = %q, want the real tool", ticket.ToolName)
	}
	if ticket.Arguments["path"] != "/tmp/x" {
		t.Errorf("ticket.Arguments = %v, want the inner call's own arguments", ticket.Arguments)
	}
}

// The arbiter is the dispatch-side half: it never waits, it only reports what
// a human already decided.

func approvalScope() tool.ApprovalScope {
	return tool.ApprovalScope{SessionKey: "s1", TaskID: "task-1"}
}

func TestArbiterReportsTheRecordedDecision(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, store *approval.ToolGateStore)
		want    bool
	}{
		{
			name: "approved",
			prepare: func(t *testing.T, store *approval.ToolGateStore) {
				openTicket(t, store)
				if _, err := store.Decide("s1", approval.TicketID("task-1", "call-1"), approval.ApprovalApproved, ""); err != nil {
					t.Fatalf("Decide: %v", err)
				}
			},
			want: true,
		},
		{
			name: "denied",
			prepare: func(t *testing.T, store *approval.ToolGateStore) {
				openTicket(t, store)
				if _, err := store.Decide("s1", approval.TicketID("task-1", "call-1"), approval.ApprovalDenied, ""); err != nil {
					t.Fatalf("Decide: %v", err)
				}
			},
		},
		{
			name:    "still pending",
			prepare: func(t *testing.T, store *approval.ToolGateStore) { openTicket(t, store) },
		},
		{
			name:    "no ticket at all",
			prepare: func(*testing.T, *approval.ToolGateStore) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := approval.NewToolGateStore(t.TempDir())
			tc.prepare(t, store)
			gate := New(store)

			ctx := tool.WithApprovalScope(context.Background(), approvalScope())
			got, err := gate.Approved(ctx, domain.ToolCall{ID: "call-1", Name: "read_file"})
			if err != nil {
				t.Fatalf("Approved: %v", err)
			}
			if got != tc.want {
				t.Errorf("Approved = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestArbiterRefusesACallWithNoApprovalScope: a call made outside a gated run
// — a plugin's own call_tool, a CLI invocation — has no task to look a ticket
// up under. That is not an error, it is a call nobody is watching, and the
// answer to "did a human approve this?" is no.
func TestArbiterRefusesACallWithNoApprovalScope(t *testing.T) {
	gate := New(approval.NewToolGateStore(t.TempDir()))

	approved, err := gate.Approved(context.Background(), domain.ToolCall{ID: "call-1", Name: "read_file"})
	if err != nil {
		t.Fatalf("Approved: %v", err)
	}
	if approved {
		t.Error("Approved = true for a call with no approval scope; nobody could have approved it")
	}
}

func openTicket(t *testing.T, store *approval.ToolGateStore) {
	t.Helper()

	if _, err := store.Open(approval.ToolApproval{
		SessionKey: "s1", TaskID: "task-1", ToolCallID: "call-1", ToolName: "read_file",
		RequestedBy: "plugin:legion-gatekeeper", Reason: "reads are reviewed",
	}); err != nil {
		t.Fatalf("Open: %v", err)
	}
}

// TestThePendingNotificationCarriesWhoAskedAndWhy: the approval card is
// rendered FROM this event. A real-machine walkthrough found a plugin's ask
// displayed as "the deployment's own sensitive-tool rule" because the event
// carried neither field and the UI fell back to the host.
func TestThePendingNotificationCarriesWhoAskedAndWhy(t *testing.T) {
	store := approval.NewToolGateStore(t.TempDir())
	sink := &spyApprovalSink{}
	gate := New(store, WithApprovalSink(sink))
	tools := askingRegistry(t, "reads are reviewed during the incident")
	task := domain.Task{ID: "task-1", SessionID: "s1", Mode: domain.ModeAuto}

	if _, err := gate.ShouldSuspend(context.Background(), task,
		[]domain.ToolCall{{ID: "call-1", Name: "read_file"}}, tools); err != nil {
		t.Fatalf("ShouldSuspend: %v", err)
	}

	if len(sink.provenance) != 1 {
		t.Fatalf("provenance = %v, want one entry", sink.provenance)
	}
	if sink.provenance[0] != "plugin:legion-gatekeeper|reads are reviewed during the incident" {
		t.Errorf("provenance = %q, want the plugin and its reason", sink.provenance[0])
	}
}

// TestAHostSensitiveNotificationSaysItIsTheHosts: the other source has to be
// stated too, or the card would have to guess.
func TestAHostSensitiveNotificationSaysItIsTheHosts(t *testing.T) {
	store := approval.NewToolGateStore(t.TempDir())
	sink := &spyApprovalSink{}
	gate := New(store, WithApprovalSink(sink))

	if _, err := gate.ShouldSuspend(context.Background(), manualTask(),
		[]domain.ToolCall{{ID: "call-2", Name: "write_file"}}, gateRegistry()); err != nil {
		t.Fatalf("ShouldSuspend: %v", err)
	}

	if len(sink.provenance) != 1 || sink.provenance[0] != approval.RequestedByHost+"|" {
		t.Errorf("provenance = %v, want [%s|]", sink.provenance, approval.RequestedByHost)
	}
}
