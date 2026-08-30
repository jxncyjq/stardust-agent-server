package manualgate

import (
	"context"
	"fmt"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/tool"
)

// This file is the plugin half of the gate: a plugin granted the decide
// extension may answer "ask", which means a human must look at the call
// before it runs.
//
// It reuses everything: the same ticket store, the same ticket shape, the same
// approval_pending notification, the same resume path. The only new fact a
// ticket carries is WHO asked (approval.ToolApproval.RequestedBy). Two
// parallel suspend mechanisms — one for the host's Sensitive rule, one for
// plugins — is precisely the outcome this design avoids.

// dispatchCall returns the call the REGISTRY will eventually see for this
// pending call: a call_tool meta call resolves to its inner real call, and
// everything else is itself.
//
// Getting this exactly right is what makes an approval findable later. The
// arbiter looks a ticket up by the id of the call being dispatched, so a
// ticket opened against the outer meta id would be invisible at dispatch time
// and a human's approval would be silently ignored.
//
// A malformed meta call (no tool_name, unparseable arguments_json) resolves to
// itself: this gate is not the layer that reports that to the model — dispatch
// is, with the message the model needs — and it must not open an approval for
// a call that will never run.
func dispatchCall(call domain.ToolCall) domain.ToolCall {
	inner, isMeta, err := tool.UnwrapLazyCall(call)
	if !isMeta || err != nil {
		return call
	}
	return inner
}

// suspendForPluginAsks opens a ticket for every pending call a plugin wants a
// human to approve, and reports whether any of them is still undecided.
//
// It does NOT look at task.Mode. The host's own Sensitive rule is Manual-only,
// but a plugin's ask is not: an Auto deployment that installed a gatekeeper
// plugin did so precisely so these calls get looked at, and honouring the ask
// only in Manual would silently degrade it to allow. The cost — an Auto task
// can now stop and wait for a person — is real, and belongs in the manual next
// to the grant that causes it.
func (g *ManualToolGate) suspendForPluginAsks(
	ctx context.Context,
	task domain.Task,
	calls []domain.ToolCall,
	tools *tool.Registry,
) (bool, error) {
	if tools == nil {
		return false, nil
	}
	needApproval := false
	for _, call := range calls {
		target := dispatchCall(call)
		label, verdict := tools.ConsultDeciders(ctx, target)
		if verdict.Decision != tool.DecisionAsk {
			continue
		}
		sessionKey := sessionKeyForTask(task)
		ticketID := approval.TicketID(task.ID, target.ID)
		existing, found, err := g.store.Get(sessionKey, ticketID, task.WorkingDir)
		if err != nil {
			return false, fmt.Errorf("check plugin approval for task %s call %s: %w", task.ID, target.ID, err)
		}
		if found && existing.Status != approval.ApprovalPending {
			// Already answered. Re-opening the question here is what would
			// make a resumed run suspend forever on the same call.
			continue
		}
		if _, err := g.store.Open(approval.ToolApproval{
			SessionKey: sessionKey, TaskID: task.ID, ToolCallID: target.ID,
			ToolName: target.Name, Arguments: target.Arguments, WorkingDir: task.WorkingDir,
			RequestedBy: label, Reason: verdict.Reason,
		}); err != nil {
			return false, fmt.Errorf("open plugin approval for task %s call %s: %w", task.ID, target.ID, err)
		}
		if !found && g.sink != nil {
			g.sink.ApprovalPending(ctx, task.ID, ticketID, target.Name, target.Arguments, label, verdict.Reason)
		}
		needApproval = true
	}
	return needApproval, nil
}

// Approved implements tool.AskArbiter: it reports whether a human has ALREADY
// decided this call, and never waits for one.
//
// Everything that is not a recorded approval is a no, and each of those is a
// legitimate state rather than a fault:
//
//   - no approval scope in the context — a call made outside a gated run (a
//     plugin's own call_tool, a CLI invocation) that nobody was watching;
//   - no ticket — nothing ever asked for this call;
//   - a pending or denied ticket — asked, and not granted.
//
// A store that cannot be read IS an error, and the registry turns it into a
// refusal (see tool.Registry.resolveVerdict): "I could not read the ticket"
// must never be answered with "go ahead".
func (g *ManualToolGate) Approved(ctx context.Context, call domain.ToolCall) (bool, error) {
	scope, ok := tool.ApprovalScopeFrom(ctx)
	if !ok {
		return false, nil
	}
	rec, found, err := g.store.Get(scope.SessionKey, approval.TicketID(scope.TaskID, call.ID), scope.WorkingDir)
	if err != nil {
		return false, fmt.Errorf("read approval for task %s call %s: %w", scope.TaskID, call.ID, err)
	}
	return found && rec.Status == approval.ApprovalApproved, nil
}
