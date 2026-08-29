package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/stardust/legion-agent/internal/domain"
)

// MetaToolCallTool is the lazy protocol's forwarding meta tool: the model
// calls it with a tool_name and arguments_json, and what actually runs is the
// tool it names.
//
// The name and the unwrapping below live in this package — rather than in the
// runtime that dispatches them — because a SECOND reader appeared: the
// approval gate has to know, one round before dispatch, exactly which call
// the registry will see. Two implementations of "what does this meta call
// really run" would differ on the day one of them is edited, and the symptom
// would be an approval ticket keyed to a call that never arrives.
const MetaToolCallTool = "call_tool"

// LazyInnerCallID renders the id the inner (real) call carries when a
// call_tool meta call is forwarded. It is derived from the outer id so the
// two stay correlated in events and audit.
func LazyInnerCallID(outerID, toolName string) string { return outerID + ":" + toolName }

// UnwrapLazyCall turns a call_tool meta call into the real call it forwards
// to, reporting false for anything that is not one.
//
// The error is the model's to read, not a Go fault: a meta call with no
// tool_name or with unparseable arguments_json is a malformed request, and
// both callers turn it into a failed ToolResult naming the problem.
func UnwrapLazyCall(call domain.ToolCall) (domain.ToolCall, bool, error) {
	if call.Name != MetaToolCallTool {
		return call, false, nil
	}
	toolName := strings.TrimSpace(call.Arguments["tool_name"])
	if toolName == "" {
		return domain.ToolCall{}, true, fmt.Errorf("call_tool requires a non-empty tool_name")
	}
	args, err := ParseCallToolArguments(call.Arguments["arguments_json"])
	if err != nil {
		return domain.ToolCall{}, true, err
	}
	return domain.ToolCall{
		ID:        LazyInnerCallID(call.ID, toolName),
		Name:      toolName,
		Arguments: args,
	}, true, nil
}

// ParseCallToolArguments decodes the arguments_json string of a call_tool meta
// call into the flat string map the tool registry expects. Non-string scalar
// values are coerced to their string form because the input-schema validator
// only accepts string/number/bool. It returns a fail-loud error (surfaced back
// to the model, not a Go error that aborts the task) when the JSON is
// malformed or not a JSON object; an empty string is a legitimate "no
// arguments".
func ParseCallToolArguments(argumentsJSON string) (map[string]string, error) {
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

// stringifyArgument coerces a decoded JSON scalar into the string form the
// tool schema validator expects. Nested objects/arrays are re-encoded as JSON.
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

// ApprovalScope identifies the run a tool call belongs to, so that whoever
// answers "did a human approve this call?" can find the ticket.
//
// It travels in the context rather than in the call because the arbiter is
// wired once at assembly, while the session and task change per run.
type ApprovalScope struct {
	SessionKey string
	TaskID     string
	// WorkingDir is the session's bound directory, if any: approval tickets
	// for a working_dir-scoped session live alongside that directory rather
	// than under the workspace root.
	WorkingDir string
}

type approvalScopeKey struct{}

// WithApprovalScope marks ctx with the run a tool call belongs to.
func WithApprovalScope(ctx context.Context, scope ApprovalScope) context.Context {
	return context.WithValue(ctx, approvalScopeKey{}, scope)
}

// ApprovalScopeFrom reads the run a tool call belongs to.
//
// The false return is a legitimate, common state: a call made outside a gated
// run — a plugin's own call_tool, a CLI invocation, a test — has no task to
// look a ticket up under. Callers must read that as "nobody approved this"
// rather than as an error.
func ApprovalScopeFrom(ctx context.Context) (ApprovalScope, bool) {
	scope, ok := ctx.Value(approvalScopeKey{}).(ApprovalScope)
	return scope, ok
}
