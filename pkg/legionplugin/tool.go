// Package legionplugin is the Go guest SDK for Legion Agent WASM plugins
// (ABI v1).
//
// A whole plugin looks like this:
//
//	package main
//
//	import "github.com/stardust/legion-agent/pkg/legionplugin"
//
//	func init() {
//		legionplugin.Serve("legion-hello-go", "0.1.0", legionplugin.Tool{
//			Name: "hello_echo",
//			Handler: func(call legionplugin.ToolCall) legionplugin.ToolResult {
//				name := call.Argument("name")
//				if name == "" {
//					return legionplugin.Fail("missing required argument: name")
//				}
//				legionplugin.LogInfo("hello_echo called with name=" + name)
//				return legionplugin.OK("hello, " + name + "!")
//			},
//		})
//	}
//
//	func main() {}
//
// Build it with:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
//
// # Why registration goes in init and main stays empty
//
// A plugin is a WASI *reactor*, not a command: the host instantiates it with
// WithStartFunctions("_initialize") and then calls exported functions. Nothing
// ever calls main. Package initialization — which is what _initialize runs —
// is therefore the only place registration can happen, and main exists solely
// because package main requires it.
//
// # Capabilities
//
// LogInfo is available unconditionally. The other six host functions are
// behind build tags (legion_config, legion_kv, legion_http, legion_fs,
// legion_tool) for the reason spelled out in host_wasip1.go: importing a host
// function the deployment did not grant makes the module fail to INSTANTIATE,
// so "import only what you use" is a hard rule rather than a preference.
package legionplugin

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ToolCall is one call the host hands this plugin.
//
// CallID is here so a plugin can correlate its own logs with the host's; it is
// NOT echoed back. The host owns the correlation id and overwrites whatever a
// guest returns, which is why ToolResult has no such field.
type ToolCall struct {
	CallID string `json:"call_id"`
	Tool   string `json:"tool"`
	// Arguments is string-to-string because that is the host's own type for it
	// (internal/plugin/host.guestToolCall): numbers and booleans arrive as
	// their string spellings and the plugin parses them itself.
	Arguments map[string]string `json:"arguments"`
}

// Argument returns a required argument, or "" when it is missing OR empty.
//
// Treating an empty value as absent is deliberate: an argument that is only
// whitespace is almost always a caller mistake, and a result computed from one
// is a result nobody can explain.
func (c ToolCall) Argument(name string) string {
	return c.Arguments[name]
}

// ToolResult is what a plugin answers, mirroring the host's domain.ToolResult.
//
// Report failure with Fail, never with a panic: a panic traps the whole wasm
// module — costing the instance's state, every other call in flight on it, and
// a fault against the plugin's health — while a failed result is just an
// answer the model can read and react to.
type ToolResult struct {
	success bool
	output  string
	failure string
}

// OK builds a successful result. output is what the model sees.
func OK(output string) ToolResult {
	return ToolResult{success: true, output: output}
}

// Fail builds a failed result. message should say what was wrong; it reaches
// the model verbatim.
func Fail(message string) ToolResult {
	return ToolResult{success: false, failure: message}
}

// wireResult is the document the host strictly decodes. Every field is written
// out, none omitted: the host's domain.ToolResult has no omitempty, so leaving
// fields out would only give the two sides different readings of one document.
type wireResult struct {
	CallID  string `json:"call_id"`
	Success bool   `json:"success"`
	Output  string `json:"output"`
	Error   string `json:"error"`
}

// encode renders the result as the host's ToolResult JSON.
//
// call_id is always empty: the host overwrites it, so filling it in here would
// only mislead whoever reads the guest's own logs.
func (r ToolResult) encode() ([]byte, error) {
	body, err := json.Marshal(wireResult{
		Success: r.success,
		Output:  r.output,
		Error:   r.failure,
	})
	if err != nil {
		return nil, fmt.Errorf("encode tool result: %w", err)
	}
	return body, nil
}

// Handler implements one tool.
type Handler func(ToolCall) ToolResult

// Tool is one tool this plugin contributes: the name the deployment's
// plugin.json must also list, and the function behind it.
type Tool struct {
	Name    string
	Handler Handler
}

// parseToolCall decodes an abi.OpCallTool request body.
//
// A missing tool name is an error rather than an empty name: an empty name
// would be dispatched as "unknown tool", which blames the caller for the SDK's
// own silence. Unknown fields are NOT refused — the host may add an optional
// field later, and a plugin that failed on it would be a fault the deployment
// counts against its health.
func parseToolCall(body []byte) (ToolCall, error) {
	var call ToolCall
	if err := json.Unmarshal(body, &call); err != nil {
		return ToolCall{}, fmt.Errorf("parse tool call: %w", err)
	}
	if call.Tool == "" {
		return ToolCall{}, errors.New("parse tool call: no tool name")
	}
	return call, nil
}

// manifestDoc is op 0's self-description. Provides is derived from the
// registered tools, so what the guest says it implements and what it actually
// dispatches cannot drift apart — a mismatch is what makes activation's
// cross-check refuse to mount the package.
type manifestDoc struct {
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	Provides []string `json:"provides"`
}
