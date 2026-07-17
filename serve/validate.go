package serve

import (
	"context"

	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/config"
)

// ValidateConfig reports whether the config file at path can be loaded by the
// authoritative loader. It returns the same error config.Load would, so callers
// in other modules (e.g. legionAgentGUI, which cannot import internal/config)
// can validate a candidate agent.json before replacing the live file.
func ValidateConfig(ctx context.Context, path string) error {
	_, err := config.Load(ctx, config.Options{Path: path})
	return err
}

// ValidateAgentConfig reports whether the sub-agent config file at path can be
// decoded by the authoritative agent-registry loader — the same parse the
// service performs at startup. It exists so callers outside this module (the
// GUI settings editor) can validate a candidate sub-agent file before replacing
// the live one, without importing internal/agentregistry.
func ValidateAgentConfig(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := agentregistry.LoadAgentFile(path)
	return err
}
