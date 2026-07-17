package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// GatewayConfig is the gateway's runtime configuration, loaded from a JSON file
// with secrets resolved from environment variables (never stored in the file).
type GatewayConfig struct {
	Core      CoreConfig                  `json:"core"`
	Identity  IdentityConfig              `json:"identity"`
	Binding   BindingConfig               `json:"binding"`
	Delivery  DeliveryConfig              `json:"delivery"`
	Platforms map[string]PlatformSettings `json:"platforms"`
}

// CoreConfig points at the Legion core API. Token is resolved from TokenEnv.
type CoreConfig struct {
	BaseURL  string `json:"base_url"`
	TokenEnv string `json:"token_env"`
	Token    string `json:"-"`
}

// IdentityConfig is the agent/company the gateway submits IM tasks as.
type IdentityConfig struct {
	AgentID   string `json:"agent_id"`
	CompanyID string `json:"company_id"`
}

// BindingConfig configures the binding store.
type BindingConfig struct {
	SQLitePath string `json:"sqlite_path"`
}

// DeliveryConfig bounds outbound retry behavior.
type DeliveryConfig struct {
	Retries   int `json:"retries"`
	BackoffMS int `json:"backoff_ms"`
}

// PlatformSettings is one platform's config. Token is resolved from TokenEnv.
type PlatformSettings struct {
	Enabled            bool   `json:"enabled"`
	TokenEnv           string `json:"token_env"`
	PollTimeoutSeconds int    `json:"poll_timeout_s"`
	Token              string `json:"-"`
}

// Load reads the gateway config file, resolves env secrets, and validates
// required fields. Missing file, malformed JSON, an unset secret env var, or a
// missing core URL/identity is a fatal configuration error reported loudly.
func Load(path string) (GatewayConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return GatewayConfig{}, fmt.Errorf("read gateway config %q: %w", path, err)
	}
	var cfg GatewayConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return GatewayConfig{}, fmt.Errorf("parse gateway config %q: %w", path, err)
	}
	if strings.TrimSpace(cfg.Core.BaseURL) == "" {
		return GatewayConfig{}, fmt.Errorf("gateway config: core.base_url is required")
	}
	if strings.TrimSpace(cfg.Identity.AgentID) == "" || strings.TrimSpace(cfg.Identity.CompanyID) == "" {
		return GatewayConfig{}, fmt.Errorf("gateway config: identity.agent_id and company_id are required")
	}
	token, err := readEnvSecret(cfg.Core.TokenEnv)
	if err != nil {
		return GatewayConfig{}, fmt.Errorf("gateway config core token: %w", err)
	}
	cfg.Core.Token = token
	for name, p := range cfg.Platforms {
		if !p.Enabled {
			continue
		}
		token, err := readEnvSecret(p.TokenEnv)
		if err != nil {
			return GatewayConfig{}, fmt.Errorf("gateway config platform %q token: %w", name, err)
		}
		p.Token = token
		cfg.Platforms[name] = p
	}
	return cfg, nil
}

// readEnvSecret returns the value of env var name, failing loud when the name is
// empty or the variable is unset — secrets must never silently default to empty.
func readEnvSecret(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("token_env is required")
	}
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		return "", fmt.Errorf("env %q is not set", name)
	}
	return value, nil
}
