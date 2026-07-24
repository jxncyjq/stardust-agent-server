package app

import "github.com/stardust/legion-agent/internal/toolauth"

// GateableToolDTO is one tool the per-agent config UI can allow or disable.
type GateableToolDTO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListGateableTools returns every tool a per-agent config may disable, each with
// a one-line description, sorted by name. It is the source the tool-authorization
// checklist in the config UI renders. Meta-tools are excluded — they are always
// resident and cannot be disabled.
func (a *App) ListGateableTools() []GateableToolDTO {
	tools := toolauth.GateableTools()
	out := make([]GateableToolDTO, 0, len(tools))
	for _, t := range tools {
		out = append(out, GateableToolDTO{Name: t.Name, Description: t.Description})
	}
	return out
}
