// Package manualgate implements the Manual-mode approval gate: at each tool-round
// boundary it suspends a task whose model wants to run a sensitive (side-effecting)
// tool until a human approves, and at dispatch time it enforces the recorded
// decision. It satisfies runtime.ToolGate.
package manualgate

import (
	"context"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/tool"
)

// Lazy-protocol meta tool names, mirrored from runtime/lazytools.go. Kept as
// local consts (rather than an import) because manualgate must not import
// runtime: runtime defines the ToolGate interface manualgate satisfies
// structurally, and importing it back here would create an import cycle once
// runtime's resolver references the gate.
const (
	metaToolCallTool  = "call_tool"
	metaToolListTools = "list_tools"
)

// ManualToolGate is the Manual-mode implementation of runtime.ToolGate. It
// suspends a task's tool round when it contains an undecided sensitive tool
// call, and enforces the recorded human decision at dispatch time.
type ManualToolGate struct {
	store *approval.ToolGateStore
}

// New returns a ManualToolGate persisting its approval tickets to store.
func New(store *approval.ToolGateStore) *ManualToolGate {
	return &ManualToolGate{store: store}
}

// resolveRealTool unwraps a lazy call_tool meta call to the underlying real tool
// name and reports its sensitivity from the registry. Non-meta calls resolve to
// their own name. A call whose target tool is unknown to the registry reports
// (name, false, false): unknown tools cannot be sensitive here — dispatch will
// fail loud via the registry's own unknown-tool handling.
func (g *ManualToolGate) resolveRealTool(call domain.ToolCall, tools *tool.Registry) (name string, sensitive bool, ok bool) {
	name = call.Name
	if call.Name == metaToolCallTool {
		name = strings.TrimSpace(call.Arguments["tool_name"])
	}
	if name == "" || name == metaToolListTools || name == metaToolCallTool {
		return name, false, false
	}
	for _, d := range tools.Descriptors() {
		if d.Name == name {
			return name, d.Sensitive, true
		}
	}
	return name, false, false
}

// ShouldSuspend reports whether the runtime must suspend before executing
// this round's calls (Manual mode + an undecided sensitive call). It opens a
// persisted approval ticket for each such call as a side effect.
func (g *ManualToolGate) ShouldSuspend(ctx context.Context, task domain.Task, calls []domain.ToolCall, tools *tool.Registry) (bool, error) {
	if task.Mode != domain.ModeManual {
		return false, nil
	}
	needApproval := false
	for _, call := range calls {
		name, sensitive, ok := g.resolveRealTool(call, tools)
		if !ok || !sensitive {
			continue
		}
		sessionKey := sessionKeyForTask(task)
		ticketID := approval.TicketID(task.ID, call.ID)
		existing, found, err := g.store.Get(sessionKey, ticketID)
		if err != nil {
			return false, fmt.Errorf("check approval for task %s call %s: %w", task.ID, call.ID, err)
		}
		if found && existing.Status != approval.ApprovalPending {
			continue // already decided — do not re-suspend on this call
		}
		if _, err := g.store.Open(approval.ToolApproval{
			SessionKey: sessionKey, TaskID: task.ID, ToolCallID: call.ID,
			ToolName: name, Arguments: call.Arguments,
		}); err != nil {
			return false, fmt.Errorf("open approval for task %s call %s: %w", task.ID, call.ID, err)
		}
		needApproval = true
	}
	return needApproval, nil
}

// Resolve reports, at dispatch time for one call, whether it may execute.
// Non-manual or non-sensitive calls always allow. Manual+sensitive+approved
// allows; Manual+sensitive+denied disallows (the caller returns a reject
// ToolResult to the model). An undecided sensitive call reaching dispatch is a
// control-flow invariant violation — the round-level gate should already have
// suspended — so it fails loud rather than silently executing.
func (g *ManualToolGate) Resolve(ctx context.Context, task domain.Task, call domain.ToolCall, tools *tool.Registry) (bool, error) {
	if task.Mode != domain.ModeManual {
		return true, nil
	}
	_, sensitive, ok := g.resolveRealTool(call, tools)
	if !ok || !sensitive {
		return true, nil
	}
	rec, found, err := g.store.Get(sessionKeyForTask(task), approval.TicketID(task.ID, call.ID))
	if err != nil {
		return false, fmt.Errorf("resolve approval for task %s call %s: %w", task.ID, call.ID, err)
	}
	switch {
	case found && rec.Status == approval.ApprovalApproved:
		return true, nil
	case found && rec.Status == approval.ApprovalDenied:
		return false, nil
	default:
		// Undecided sensitive call reaching dispatch is a control-flow invariant
		// violation (round gate should have suspended). Fail loud, never execute.
		return false, fmt.Errorf("dispatch reached undecided sensitive call %s for task %s (found=%v)", call.ID, task.ID, found)
	}
}

// sessionKeyForTask mirrors runtime.sessionKeyForTask (SessionID else ID); kept
// local to avoid importing runtime (which defines the ToolGate interface).
func sessionKeyForTask(task domain.Task) string {
	if task.SessionID != "" {
		return task.SessionID
	}
	return task.ID
}
