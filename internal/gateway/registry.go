package gateway

import "fmt"

// PlatformEntry describes a registered platform: a factory plus metadata used by
// routing (MaxMessageLength) and safety (PIISafe).
type PlatformEntry struct {
	Platform         string
	Factory          func(cfg PlatformConfig) (ChannelAdapter, error)
	MaxMessageLength int
	PIISafe          bool
}

// PlatformRegistry maps a platform name to its entry using the factory pattern,
// so adding a platform is a Register call rather than a change to core logic.
type PlatformRegistry struct {
	entries map[string]PlatformEntry
}

// NewPlatformRegistry returns an empty registry.
func NewPlatformRegistry() *PlatformRegistry {
	return &PlatformRegistry{entries: make(map[string]PlatformEntry)}
}

// Register adds a platform entry. A duplicate platform or a missing factory is a
// programming error reported loudly rather than silently overwritten.
func (r *PlatformRegistry) Register(entry PlatformEntry) error {
	if entry.Platform == "" {
		return fmt.Errorf("register platform: name is required")
	}
	if entry.Factory == nil {
		return fmt.Errorf("register platform %q: factory is required", entry.Platform)
	}
	if _, exists := r.entries[entry.Platform]; exists {
		return fmt.Errorf("register platform %q: already registered", entry.Platform)
	}
	r.entries[entry.Platform] = entry
	return nil
}

// Get returns a platform entry and whether it exists.
func (r *PlatformRegistry) Get(name string) (PlatformEntry, bool) {
	entry, ok := r.entries[name]
	return entry, ok
}

// Build constructs an adapter for a registered platform. An unknown platform is
// an error, not a nil adapter.
func (r *PlatformRegistry) Build(name string, cfg PlatformConfig) (ChannelAdapter, error) {
	entry, ok := r.entries[name]
	if !ok {
		return nil, fmt.Errorf("build platform %q: not registered", name)
	}
	adapter, err := entry.Factory(cfg)
	if err != nil {
		return nil, fmt.Errorf("build platform %q: %w", name, err)
	}
	return adapter, nil
}
