package adapter

import (
	"fmt"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/port"
)

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
