package cli

import (
	"fmt"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/port"
)

// maasProfileResolver resolves a MaaS profile name to an inference client using
// the configured profiles. It satisfies runtime.ModelResolver so the moa_consult
// tool can build reference/aggregator models by name. An unknown profile or a
// config that yields no client is a loud error, never a nil client.
type maasProfileResolver struct {
	cfg config.MaasConfig
}

func (r maasProfileResolver) Resolve(profile string) (port.MaasInferenceClient, error) {
	client, err := adapter.NewMaasClientFromProfile(r.cfg, profile)
	if err != nil {
		return nil, fmt.Errorf("resolve maas profile %q: %w", profile, err)
	}
	if client == nil {
		return nil, fmt.Errorf("resolve maas profile %q: no client configured", profile)
	}
	return client, nil
}
