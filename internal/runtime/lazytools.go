package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// Meta-tool names for the lazy (on-demand) tool protocol. The model sees only
// these two tools and discovers/invokes the real registered tools through them,
// instead of receiving the full native tool schema on every inference.
const (
	metaToolListTools = "list_tools"
	metaToolCallTool  = "call_tool"
)

// metaInferenceTools returns the two meta tools offered under the lazy protocol.
// Their schemas are intentionally tiny so a simple no-tool chat pays only this
// fixed overhead instead of the full native tool schema (~1800 tokens).
func metaInferenceTools() []port.InferenceTool {
	return []port.InferenceTool{
		{
			Name:        metaToolListTools,
			Description: "List the available tools and their parameters. Call this first whenever you need a tool: it returns each tool's name, description and input schema so you can then invoke one via call_tool.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Optional case-insensitive filter; only tools whose name or description contains it are returned.",
					},
				},
			},
		},
		{
			Name:        metaToolCallTool,
			Description: "Invoke one real tool by name. Discover tool names and parameters first with list_tools.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"tool_name", "arguments_json"},
				"properties": map[string]any{
					"tool_name": map[string]any{
						"type":        "string",
						"description": "The exact name of the tool to invoke (from list_tools).",
					},
					"arguments_json": map[string]any{
						"type":        "string",
						"description": `A JSON object string holding the target tool's arguments, e.g. {"path":"README.md"}. Use {} when the tool takes no arguments.`,
					},
				},
			},
		},
	}
}

// isMetaTool reports whether name is one of the lazy-protocol meta tools.
func isMetaTool(name string) bool {
	return name == metaToolListTools || name == metaToolCallTool
}

// toolCatalogEntry is one real tool exposed by list_tools, mirroring the native
// InferenceTool shape so the model gets the same name/description/schema it would
// have seen up-front under the eager protocol.
type toolCatalogEntry struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// listToolsCatalog renders the real-tool directory (excluding the meta tools
// themselves) as JSON, optionally filtered by a case-insensitive query that
// matches tool name or description.
func (r *Runtime) listToolsCatalog(query string) (string, error) {
	descriptors := r.tools.Descriptors()
	query = strings.ToLower(strings.TrimSpace(query))
	entries := make([]toolCatalogEntry, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if isMetaTool(descriptor.Name) {
			continue
		}
		if query != "" &&
			!strings.Contains(strings.ToLower(descriptor.Name), query) &&
			!strings.Contains(strings.ToLower(descriptor.Description), query) {
			continue
		}
		entries = append(entries, toolCatalogEntry{
			Name:        descriptor.Name,
			Description: descriptor.Description,
			InputSchema: descriptor.InputSchema,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	encoded, err := json.Marshal(map[string]any{"tools": entries})
	if err != nil {
		return "", fmt.Errorf("marshal tool catalog: %w", err)
	}
	return string(encoded), nil
}

// parseCallToolArguments decodes the arguments_json string of a call_tool meta
// call into the flat string map the tool registry expects. Non-string scalar
// values are coerced to their string form because the input-schema validator
// only accepts string/number/bool. It returns a fail-loud error (surfaced back
// to the model, not a Go error that aborts the task) when the JSON is missing,
// malformed, or not a JSON object.
func parseCallToolArguments(argumentsJSON string) (map[string]string, error) {
	trimmed := strings.TrimSpace(argumentsJSON)
	if trimmed == "" {
		return map[string]string{}, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, fmt.Errorf("arguments_json is not a valid JSON object: %v", err)
	}
	args := make(map[string]string, len(raw))
	for key, value := range raw {
		args[key] = stringifyArgument(value)
	}
	return args, nil
}

// stringifyArgument coerces a decoded JSON scalar into the string form the tool
// schema validator expects. Nested objects/arrays are re-encoded as JSON.
func stringifyArgument(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case nil:
		return ""
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(encoded)
	}
}

// dispatchToolCall routes one model tool call. Under the lazy protocol the meta
// tools are handled in-runtime (they are not registered in the tool registry):
// list_tools returns the real-tool catalog and call_tool unpacks and forwards to
// the named real tool through the registry (preserving permission/audit/timeout/
// sanitizer). Every other call — and all calls under the eager protocol — goes
// straight to the registry. Meta-tool fail-loud conditions (empty tool_name,
// malformed arguments_json, unknown target tool) return an unsuccessful
// ToolResult so the model can see and correct them, rather than a Go error that
// would abort the task.
func (r *Runtime) dispatchToolCall(ctx context.Context, agent domain.Agent, task domain.Task, call domain.ToolCall) (domain.ToolResult, error) {
	if !r.lazyTools || !isMetaTool(call.Name) {
		return r.tools.Execute(ctx, agent, call)
	}
	switch call.Name {
	case metaToolListTools:
		catalog, err := r.listToolsCatalog(call.Arguments["query"])
		if err != nil {
			return domain.ToolResult{}, err
		}
		return domain.ToolResult{CallID: call.ID, Success: true, Output: catalog}, nil
	case metaToolCallTool:
		return r.dispatchCallTool(ctx, agent, task, call)
	default:
		return domain.ToolResult{}, fmt.Errorf("unhandled meta tool %q", call.Name)
	}
}

// dispatchCallTool unpacks a call_tool meta call and forwards it to the named
// real tool, publishing the inner tool's request/result/executed events so the
// event stream reflects which real tool actually ran.
func (r *Runtime) dispatchCallTool(ctx context.Context, agent domain.Agent, task domain.Task, call domain.ToolCall) (domain.ToolResult, error) {
	toolName := strings.TrimSpace(call.Arguments["tool_name"])
	if toolName == "" {
		return domain.ToolResult{CallID: call.ID, Success: false, Error: "call_tool requires a non-empty tool_name"}, nil
	}
	if isMetaTool(toolName) {
		return domain.ToolResult{CallID: call.ID, Success: false, Error: fmt.Sprintf("tool_name %q is a meta tool and cannot be called via call_tool", toolName)}, nil
	}
	args, err := parseCallToolArguments(call.Arguments["arguments_json"])
	if err != nil {
		return domain.ToolResult{CallID: call.ID, Success: false, Error: err.Error()}, nil
	}
	realCall := domain.ToolCall{
		ID:        call.ID + ":" + toolName,
		Name:      toolName,
		Arguments: args,
	}
	if err := r.events.Publish(ctx, domain.RuntimeEvent{
		Type:      "tool_call_requested",
		TaskID:    task.ID,
		Message:   toolName,
		CreatedAt: time.Now(),
	}); err != nil {
		return domain.ToolResult{}, fmt.Errorf("publish lazy tool request event: %w", err)
	}
	result, err := r.tools.Execute(ctx, agent, realCall)
	if err != nil {
		if pubErr := r.events.Publish(ctx, domain.RuntimeEvent{
			Type:      "tool_failed",
			TaskID:    task.ID,
			Message:   toolName,
			CreatedAt: time.Now(),
		}); pubErr != nil {
			return domain.ToolResult{}, fmt.Errorf("publish lazy tool failed event: %w", pubErr)
		}
		// Surface the real tool's failure (e.g. unknown tool, schema violation)
		// back to the model as an unsuccessful result instead of aborting.
		return domain.ToolResult{CallID: call.ID, Success: false, Error: err.Error()}, nil
	}
	if err := r.events.Publish(ctx, domain.RuntimeEvent{
		Type:      "tool_result",
		TaskID:    task.ID,
		Message:   result.Output,
		CreatedAt: time.Now(),
	}); err != nil {
		return domain.ToolResult{}, fmt.Errorf("publish lazy tool result event: %w", err)
	}
	if err := r.events.Publish(ctx, domain.RuntimeEvent{
		Type:      "tool_executed",
		TaskID:    task.ID,
		Message:   toolName,
		CreatedAt: time.Now(),
	}); err != nil {
		return domain.ToolResult{}, fmt.Errorf("publish lazy tool executed event: %w", err)
	}
	// Re-tag the result with the outer call_tool ID so the tool loop's dedup and
	// rendering keys line up with the call the model actually issued.
	result.CallID = call.ID
	return result, nil
}
