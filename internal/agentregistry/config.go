package agentregistry

import "github.com/stardust/legion-agent/internal/config"

type AgentConfig struct {
	ID           string                    `json:"id"`
	Role         string                    `json:"role"`
	MaasProfile  string                    `json:"maas_profile"`
	ContextFiles config.ContextFilesConfig `json:"context_files"`
	Workspace    config.WorkspaceConfig    `json:"workspace"`
	Skills       config.SkillsConfig       `json:"skills"`
}
