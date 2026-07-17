package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/tool"
)

// delegateTaskArgs is the JSON shape of a single entry in the delegate_task
// "tasks" batch argument.
type delegateTaskArgs struct {
	Goal     string   `json:"goal"`
	Context  string   `json:"context,omitempty"`
	Role     string   `json:"role,omitempty"`
	AgentID  string   `json:"agent_id,omitempty"`
	Toolsets []string `json:"toolsets,omitempty"`
}

// RegisterDelegateTaskTool registers the delegate_task tool on registry, bridging
// model tool calls to this runtime's delegation methods. It is registered only
// for runtimes that may delegate (orchestrators below the depth limit): a leaf
// child never gets the tool, so it cannot recurse — matching the leaf/orchestrator
// contract. Registration is a no-op when registry is nil or delegation is not
// permitted.
func (r *Runtime) RegisterDelegateTaskTool(registry *tool.Registry) {
	if registry == nil || !r.canDelegate() {
		return
	}
	registry.RegisterDescriptor(delegateTaskDescriptor(), tool.HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return r.handleDelegateTask(ctx, call)
	}))
}

func delegateTaskDescriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "delegate_task",
		Description: "Delegate work to a sub-agent with its own independent context; only the sub-agent's " +
			"final summary returns, keeping this agent's context small. Single mode: pass goal (and optional " +
			"context, role). Batch mode: pass tasks as a JSON array of {goal, context, role} run in parallel. " +
			"Set background=true to return immediately and receive the result later as a subtask_completed event. " +
			"role is \"leaf\" (default, cannot re-delegate) or \"orchestrator\" (may nest until the depth limit).",
		RiskLevel: "high",
		Timeout:   5 * time.Minute,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"goal":       map[string]any{"type": "string", "description": "The sub-task objective (single mode)."},
				"context":    map[string]any{"type": "string", "description": "Optional supporting context for the sub-task."},
				"role":       map[string]any{"type": "string", "description": "\"leaf\" (default) or \"orchestrator\"."},
				"background": map[string]any{"type": "string", "description": "When \"true\", run asynchronously and return a handle."},
				"toolsets":   map[string]any{"type": "string", "description": "Optional comma-separated tool names narrowing the sub-agent to a subset of the parent tools (single mode). Empty inherits all."},
				"tasks":      map[string]any{"type": "string", "description": "Batch mode: JSON array of {goal, context, role, toolsets} objects run in parallel."},
			},
		},
	}
}

func (r *Runtime) handleDelegateTask(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
	parentTaskID := strings.TrimSpace(call.Arguments["parent_task_id"])
	if parentTaskID == "" {
		parentTaskID = strings.TrimSpace(call.ID)
	}

	if raw := strings.TrimSpace(call.Arguments["tasks"]); raw != "" {
		var batch []delegateTaskArgs
		if err := json.Unmarshal([]byte(raw), &batch); err != nil {
			return domain.ToolResult{CallID: call.ID, Success: false, Error: fmt.Sprintf("invalid tasks JSON: %v", err)}, nil
		}
		specs := make([]SubTaskSpec, 0, len(batch))
		for _, entry := range batch {
			specs = append(specs, SubTaskSpec{
				ParentTaskID: parentTaskID,
				AgentID:      entry.AgentID,
				Goal:         entry.Goal,
				Context:      entry.Context,
				Role:         entry.Role,
				Toolsets:     entry.Toolsets,
			})
		}
		results, err := r.RunSubTasks(ctx, specs)
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("delegate_task batch: %w", err)
		}
		return delegateJSON(call.ID, map[string]any{"mode": "batch", "results": delegateResultsView(results)})
	}

	spec := SubTaskSpec{
		ParentTaskID: parentTaskID,
		AgentID:      strings.TrimSpace(call.Arguments["agent_id"]),
		Goal:         call.Arguments["goal"],
		Context:      call.Arguments["context"],
		Role:         strings.TrimSpace(call.Arguments["role"]),
		Toolsets:     parseToolsetsCSV(call.Arguments["toolsets"]),
	}
	if parseDelegateBool(call.Arguments["background"]) {
		handle, err := r.RunSubTaskAsync(ctx, spec)
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("delegate_task background: %w", err)
		}
		return delegateJSON(call.ID, map[string]any{"mode": "background", "handle": handle.TaskID})
	}
	res, err := r.RunSubTask(ctx, spec)
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("delegate_task: %w", err)
	}
	return delegateJSON(call.ID, map[string]any{"mode": "single", "task_id": res.TaskID, "summary": res.Summary})
}

func delegateResultsView(results []SubTaskResult) []map[string]any {
	view := make([]map[string]any, 0, len(results))
	for _, res := range results {
		view = append(view, map[string]any{
			"task_id": res.TaskID,
			"summary": res.Summary,
			"error":   res.Err,
		})
	}
	return view
}

// parseToolsetsCSV splits a comma-separated tool-name list into a trimmed,
// non-empty slice. Empty input yields nil (inherit the full parent tool set).
func parseToolsetsCSV(value string) []string {
	var names []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	return names
}

func parseDelegateBool(value string) bool {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "1", "true", "yes", "y":
		return true
	default:
		return false
	}
}

func delegateJSON(callID string, payload map[string]any) (domain.ToolResult, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("encode delegate_task result: %w", err)
	}
	return domain.ToolResult{CallID: callID, Success: true, Output: string(encoded)}, nil
}
