package agentregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/stardust/legion-agent/internal/config"
)

var ErrAgentConfigNotFound = errors.New("agent config not found")

type Registry struct {
	agents map[string]AgentConfig
}

func Load(ctx context.Context, rootCfg config.Config, configDir string) (*Registry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	agents := make(map[string]AgentConfig, len(rootCfg.Agents))
	for name, configPath := range rootCfg.Agents {
		path := configPath
		if !filepath.IsAbs(path) {
			path = filepath.Join(configDir, path)
		}
		agent, err := LoadAgentFile(path)
		if err != nil {
			return nil, err
		}
		agents[name] = agent
	}
	return New(agents), nil
}

// LoadAgentFile reads and decodes a single sub-agent config file at an already
// resolved path. It is the one place the agent-config file format is parsed, so
// callers that validate a candidate file (e.g. the GUI settings editor, via the
// serve bridge) reject exactly what Load would reject at startup.
func LoadAgentFile(path string) (AgentConfig, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return AgentConfig{}, fmt.Errorf("%w: %s", ErrAgentConfigNotFound, path)
	}
	if err != nil {
		return AgentConfig{}, fmt.Errorf("read agent config %q: %w", path, err)
	}
	var agent AgentConfig
	if err := json.Unmarshal(data, &agent); err != nil {
		return AgentConfig{}, fmt.Errorf("decode agent config %q: %w", path, err)
	}
	return agent, nil
}

func New(agents map[string]AgentConfig) *Registry {
	copied := make(map[string]AgentConfig, len(agents))
	for name, agent := range agents {
		copied[name] = agent
	}
	return &Registry{agents: copied}
}

func (r *Registry) Get(name string) (AgentConfig, bool) {
	agent, ok := r.agents[name]
	return agent, ok
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.agents))
	for name := range r.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
