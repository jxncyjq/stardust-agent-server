package agentregistry

import "github.com/stardust/legion-agent/internal/config"

type AgentConfig struct {
	ID           string                    `json:"id"`
	Role         string                    `json:"role"`
	MaasProfile  string                    `json:"maas_profile"`
	ContextFiles config.ContextFilesConfig `json:"context_files"`
	Workspace    config.WorkspaceConfig    `json:"workspace"`
	Skills       config.SkillsConfig       `json:"skills"`
	// DisabledTools names the tools this agent may not use (deny-list). Absent /
	// null / empty means no tool is disabled — every tool is available. Each name
	// must be a known gateable tool (validated at agent assembly); meta-tools are
	// never listed here and cannot be disabled.
	DisabledTools []string `json:"disabled_tools,omitempty"`
}
