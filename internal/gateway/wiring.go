package gateway

import "fmt"

// BuildAdapters instantiates an adapter for every enabled platform in cfg using
// the registry, and returns them plus a DeliveryRouter pre-registered with each.
// An enabled platform with no registered entry is a loud configuration error.
func BuildAdapters(cfg GatewayConfig, reg *PlatformRegistry) ([]ChannelAdapter, *DeliveryRouter, error) {
	var adapters []ChannelAdapter
	router := NewDeliveryRouter()
	for name, settings := range cfg.Platforms {
		if !settings.Enabled {
			continue
		}
		entry, ok := reg.Get(name)
		if !ok {
			return nil, nil, fmt.Errorf("build adapters: platform %q enabled but not registered", name)
		}
		adapter, err := reg.Build(name, PlatformConfig{
			Token:              settings.Token,
			PollTimeoutSeconds: settings.PollTimeoutSeconds,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("build adapters: %w", err)
		}
		adapters = append(adapters, adapter)
		router.RegisterAdapter(adapter, entry.MaxMessageLength)
	}
	return adapters, router, nil
}
