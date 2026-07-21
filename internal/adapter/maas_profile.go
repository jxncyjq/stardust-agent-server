package adapter

import (
	"fmt"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/port"
)

// NewMaasClientFromProfile builds an inference client from the named profile, or
// from the top-level base_url when no profile applies.
//
// It returns (nil, nil) in exactly one case: MaaS is not configured at all — no
// profile named, no default profile, no base_url. That absence is an explicit
// part of the contract, not a swallowed failure, and every caller substitutes
// for it deliberately: serve and app.RunTask fall back to a recording client so
// the agent still runs offline (scripts/smoke.ps1's prompt-smoke depends on
// this), and maasProfileResolver rejects nil loudly because MoA cannot run
// without a real model. A caller that cannot work with a nil client must say so
// itself, as that resolver does.
//
// A profile that was *named* but does not exist is a different thing entirely —
// a configuration error, not an absence — and is always returned as an error.
func NewMaasClientFromProfile(cfg config.MaasConfig, name string) (port.MaasInferenceClient, error) {
	if name == "" {
		name = cfg.DefaultProfile
	}
	if name != "" {
		profile, ok := cfg.Profiles[name]
		if !ok {
			return nil, fmt.Errorf("maas profile %q not found", name)
		}
		return NewHTTPMaasClient(HTTPMaasConfig{
			BaseURL:           profile.BaseURL,
			APIKey:            profile.APIKey,
			Model:             profile.Model,
			EnablePromptCache: profile.PromptCache,
		}), nil
	}
	if cfg.BaseURL == "" {
		return nil, nil
	}
	return NewHTTPMaasClient(HTTPMaasConfig{
		BaseURL: cfg.BaseURL,
		APIKey:  cfg.APIKey,
	}), nil
}
