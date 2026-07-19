package tui

import (
	"context"
	"strings"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/runtime"
	"github.com/stardust/legion-agent/internal/tool"
)

// Lazy-protocol meta tool names, mirrored from runtime/lazytools.go and
// internal/manualgate/manualgate.go. Kept as local consts (rather than an
// import) so approvalGate's sensitivity lookup does not pull in either
// package for two string constants.
const (
	approvalMetaToolCallTool  = "call_tool"
	approvalMetaToolListTools = "list_tools"
)

// PendingApproval describes one Manual-mode sensitive tool call awaiting a
// human decision. approvalGate.Resolve publishes one on the gate's pendingCh
// (running on the runtime's dispatch goroutine); the bubbletea main loop
// consumes it to render the terminal approval prompt.
type PendingApproval struct {
	// Tool is the real (unwrapped) tool name: a lazy call_tool meta call is
	// resolved to the underlying tool it wraps before being reported here.
	Tool string
	// Args are the arguments of the real tool call.
	Args map[string]string
}

// ApprovalDecision carries a human's approve/deny answer back from the
// bubbletea main loop to the blocked approvalGate.Resolve call via the
// gate's decisionCh.
type ApprovalDecision struct {
	Allow bool
}

// approvalGate is the TUI's Manual-mode runtime.ToolGate — "方案 Y": in-process
// synchronous blocking approval. Unlike manualgate.ManualToolGate (which
// persists approval tickets to disk and suspends the task at the round
// boundary so it can resume out-of-process, e.g. after a restart),
// approvalGate never suspends: ShouldSuspend always reports false, and
// Resolve instead blocks the dispatching goroutine in place until a human
// answers the terminal prompt rendered by the bubbletea main loop. There is
// no persisted state — if the process dies mid-approval, the pending
// decision is lost along with the in-flight task run; this trade-off is the
// point of 方案 Y (a same-process, no-disk approval loop for the TUI) and is
// out of scope for the checkpoint-backed HTTP/server approval flow
// (internal/manualgate), which remains unchanged.
type approvalGate struct {
	pendingCh  chan<- PendingApproval
	decisionCh <-chan ApprovalDecision
}

// NewApprovalGate returns a runtime.ToolGate that funnels Manual-mode
// sensitive-tool approval requests through pendingCh and blocks for the
// matching answer on decisionCh. The two channels are the gate's ends of the
// pair wired to the bubbletea main loop (see InteractiveConfig.ApprovalCh /
// DecisionCh for the model's opposing ends); passing bare channels directly
// also lets unit tests drive the gate without a running bubbletea program.
func NewApprovalGate(pendingCh chan<- PendingApproval, decisionCh <-chan ApprovalDecision) runtime.ToolGate {
	return &approvalGate{pendingCh: pendingCh, decisionCh: decisionCh}
}

// ShouldSuspend always reports (false, nil): 方案 Y does not use round-level
// suspend/checkpoint (contrast manualgate.ManualToolGate.ShouldSuspend) —
// approval blocks in-process inside Resolve instead of pausing the task for
// later out-of-process resume.
func (g *approvalGate) ShouldSuspend(context.Context, domain.Task, []domain.ToolCall, *tool.Registry) (bool, error) {
	return false, nil
}

// Resolve reports whether call may execute. Non-Manual tasks and
// non-sensitive tools always allow without touching either channel. A
// Manual-mode sensitive call blocks: it publishes a PendingApproval on
// pendingCh, then waits on decisionCh for the human's answer. If ctx is
// cancelled before either handoff completes, Resolve returns (false,
// ctx.Err()) rather than blocking forever — a cancelled task (shutdown,
// timeout) must never leave the runtime deadlocked waiting on a terminal
// keypress that will never come (CLAUDE.md §0 fail-loud: no silent
// deadlock).
func (g *approvalGate) Resolve(ctx context.Context, task domain.Task, call domain.ToolCall, tools *tool.Registry) (bool, error) {
	if task.Mode != domain.ModeManual {
		return true, nil
	}
	name, sensitive := approvalResolveRealTool(call, tools)
	if !sensitive {
		return true, nil
	}
	select {
	case g.pendingCh <- PendingApproval{Tool: name, Args: call.Arguments}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case decision := <-g.decisionCh:
		return decision.Allow, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// approvalResolveRealTool unwraps a lazy call_tool meta call to the
// underlying real tool name and reports its sensitivity from the registry.
// It mirrors manualgate.ManualToolGate.resolveRealTool but is duplicated
// rather than imported: manualgate's disk-backed ticket bookkeeping is
// irrelevant here, and importing it would pull the approval/sessionstate
// dependency graph into the TUI package for two string constants and a
// lookup loop. A call whose target tool is unknown to the registry, or a
// bare list_tools/call_tool meta call, reports (name, false): unknown/meta
// tools cannot be sensitive here — dispatch will fail loud via the
// registry's own unknown-tool handling.
func approvalResolveRealTool(call domain.ToolCall, tools *tool.Registry) (name string, sensitive bool) {
	name = call.Name
	if call.Name == approvalMetaToolCallTool {
		name = strings.TrimSpace(call.Arguments["tool_name"])
	}
	if name == "" || name == approvalMetaToolListTools || name == approvalMetaToolCallTool {
		return name, false
	}
	if tools == nil {
		return name, false
	}
	for _, d := range tools.Descriptors() {
		if d.Name == name {
			return name, d.Sensitive
		}
	}
	return name, false
}
