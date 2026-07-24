package serve

import "github.com/stardust/legion-agent/internal/toolauth"

// GateableTool is one tool a per-agent config may disable, with a one-line
// description for the config UI. It mirrors toolauth.GateableTool so external
// modules (legionAgentGUI) get the data without importing internal/toolauth.
type GateableTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// GateableTools returns every tool a per-agent disabled_tools list may name,
// each with a one-line description, sorted by name. It is the public seam the
// GUI's ListGateableTools binding reads to render the tool-authorization
// checklist; meta-tools are excluded because they are always resident and
// cannot be disabled.
func GateableTools() []GateableTool {
	src := toolauth.GateableTools()
	out := make([]GateableTool, 0, len(src))
	for _, t := range src {
		out = append(out, GateableTool{Name: t.Name, Description: t.Description})
	}
	return out
}
