package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

// Meta-tool names for the lazy (on-demand) tool protocol. The model sees only
// these meta tools and discovers/invokes the real registered tools and skills
// through them, instead of receiving the full native tool schema on every
// inference.
const (
	metaToolCallTool         = "call_tool"
	metaToolLoadCapabilities = "load_capabilities"
)

// maxLoadBatch bounds one load_capabilities call. A single skill body runs to
// kilobytes; five at once is already a large slice of the loaded block, and
// the model can simply call again.
const maxLoadBatch = 5

// metaInferenceTools returns the meta tools offered under the lazy protocol.
// Their schemas are intentionally tiny so a simple no-tool chat pays only this
// fixed overhead instead of the full native tool schema (~1800 tokens).
func metaInferenceTools() []port.InferenceTool {
	return []port.InferenceTool{
		{
			Name:        metaToolCallTool,
			Description: "Invoke one real tool by name. Discover tool names and parameters via load_capabilities and <available_capabilities>.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"tool_name", "arguments_json"},
				"properties": map[string]any{
					"tool_name": map[string]any{
						"type":        "string",
						"description": "The exact name of the tool to invoke.",
					},
					"arguments_json": map[string]any{
						"type":        "string",
						"description": `A JSON object string holding the target tool's arguments, e.g. {"path":"README.md"}. Use {} when the tool takes no arguments.`,
					},
				},
			},
		},
		{
			Name:        metaToolLoadCapabilities,
			Description: "Load the full definition of one or more capabilities listed in <available_capabilities>: a tool's parameter schema, or a skill's full instructions. Load before using. Pass a comma-separated list of names.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"names"},
				"properties": map[string]any{
					"names": map[string]any{
						"type":        "string",
						"description": "Comma-separated capability names, at most 5 per call.",
					},
				},
			},
		},
	}
}

// isMetaTool reports whether name is one of the lazy-protocol meta tools.
func isMetaTool(name string) bool {
	return name == metaToolCallTool || name == metaToolLoadCapabilities
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
// call_tool unpacks and forwards to the named real tool through the registry
// (preserving permission/audit/timeout/sanitizer), and load_capabilities pins
// the requested capabilities' definitions into the run's loaded block. Every
// other call — and all calls under the eager protocol — goes straight to the
// registry. Meta-tool fail-loud conditions (empty tool_name, malformed
// arguments_json, unknown target tool/capability) return an unsuccessful
// ToolResult so the model can see and correct them, rather than a Go error that
// would abort the task.
//
// It takes the run's mutable *loopState so load_capabilities can write st.loaded
// and so both the effective registry (st.tools) and the catalog (st.catalog) are
// drawn from one scoped source: a Plan-mode run dispatches and loads against its
// read-only subset, never a broader set than it offered.
func (r *Runtime) dispatchToolCall(ctx context.Context, agent domain.Agent, task domain.Task, call domain.ToolCall, st *loopState) (domain.ToolResult, error) {
	tools := st.tools
	if r.toolGate != nil {
		allow, err := r.toolGate.Resolve(ctx, task, call, tools)
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("gate resolve for task %s call %s: %w", task.ID, call.ID, err)
		}
		if !allow {
			return domain.ToolResult{CallID: call.ID, Success: false, Error: "tool call denied by human approver"}, nil
		}
	}
	if !r.lazyTools || !isMetaTool(call.Name) {
		return tools.Execute(ctx, agent, call)
	}
	switch call.Name {
	case metaToolCallTool:
		return r.dispatchCallTool(ctx, agent, task, call, tools)
	case metaToolLoadCapabilities:
		return r.dispatchLoadCapabilities(ctx, st, call, st.catalog)
	default:
		return domain.ToolResult{}, fmt.Errorf("unhandled meta tool %q", call.Name)
	}
}

// dispatchLoadCapabilities pins the requested capabilities' full definitions
// into the run's loaded block.
//
// The tool result itself is only an acknowledgement. Putting the definitions
// in the result instead would subject them to the 4000-char per-result
// truncation and to the mid-prompt dropping that boundPrompt does -- a schema
// cut in half is invalid JSON, and one silently dropped leaves the model
// calling from memory.
//
// Every failure comes back as an unsuccessful ToolResult rather than a Go
// error: the model can read it and correct itself, whereas an error aborts the
// task.
func (r *Runtime) dispatchLoadCapabilities(ctx context.Context, st *loopState, call domain.ToolCall, catalog *capability.Catalog) (domain.ToolResult, error) {
	// A nil catalog here is not "no capabilities": it is a wiring invariant
	// violation -- load_capabilities was offered and dispatched but the run built
	// no catalog to load from. Fail loud rather than nil-deref or pretend success.
	if catalog == nil {
		return domain.ToolResult{}, fmt.Errorf("load_capabilities for call %s dispatched without a catalog", call.ID)
	}
	names := splitNames(call.Arguments["names"])
	if len(names) == 0 {
		return domain.ToolResult{CallID: call.ID, Success: false, Error: "load_capabilities requires at least one name"}, nil
	}
	if len(names) > maxLoadBatch {
		return domain.ToolResult{CallID: call.ID, Success: false,
			Error: fmt.Sprintf("load at most %d capabilities per call, got %d", maxLoadBatch, len(names))}, nil
	}
	maxLoadedChars := r.maxPromptChars / 3
	loadedNames := make([]string, 0, len(names))
	evictedAll := make([]string, 0)
	for _, name := range names {
		detail, err := catalog.Detail(ctx, name)
		if errors.Is(err, capability.ErrUnknownCapability) {
			return domain.ToolResult{CallID: call.ID, Success: false,
				Error: fmt.Sprintf("unknown capability %q: it is not in <available_capabilities> for this task", name)}, nil
		}
		if err != nil {
			return domain.ToolResult{}, err
		}
		next, evicted, err := appendLoaded(st.loaded, name, detail, maxLoadedChars)
		if err != nil {
			return domain.ToolResult{CallID: call.ID, Success: false, Error: err.Error()}, nil
		}
		st.loaded = next
		evictedAll = append(evictedAll, evicted...)
		loadedNames = append(loadedNames, name)
		if r.skillUsage != nil {
			r.skillUsage.Touch(name, time.Now())
		}
	}
	output := "loaded: " + strings.Join(loadedNames, ", ")
	if notice := renderEvictionNotice(evictedAll); notice != "" {
		output += "\n" + notice
	}
	return domain.ToolResult{CallID: call.ID, Success: true, Output: output}, nil
}

// splitNames parses the comma-separated names argument, dropping empties.
func splitNames(raw string) []string {
	parts := strings.Split(raw, ",")
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	return names
}

// dispatchCallTool unpacks a call_tool meta call and forwards it to the named
// real tool, publishing the inner tool's request/result/executed events so the
// event stream reflects which real tool actually ran.
func (r *Runtime) dispatchCallTool(ctx context.Context, agent domain.Agent, task domain.Task, call domain.ToolCall, tools *tool.Registry) (domain.ToolResult, error) {
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
	result, err := tools.Execute(ctx, agent, realCall)
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
