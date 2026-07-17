package gateway

import (
	"context"
	"testing"
)

type stubAdapter struct{ name string }

func (a stubAdapter) Platform() string                                   { return a.name }
func (a stubAdapter) Start(context.Context, chan<- InboundMessage) error { return nil }
func (a stubAdapter) Send(context.Context, DeliveryTarget, string) error { return nil }
func (a stubAdapter) Close() error                                       { return nil }

func TestPlatformRegistryBuildAndUnknown(t *testing.T) {
	reg := NewPlatformRegistry()
	if err := reg.Register(PlatformEntry{
		Platform:         "telegram",
		MaxMessageLength: 4096,
		Factory:          func(PlatformConfig) (ChannelAdapter, error) { return stubAdapter{name: "telegram"}, nil },
	}); err != nil {
		t.Fatalf("Register() error = %v, want nil", err)
	}
	// Duplicate registration fails loud.
	if err := reg.Register(PlatformEntry{Platform: "telegram", Factory: func(PlatformConfig) (ChannelAdapter, error) { return nil, nil }}); err == nil {
		t.Fatalf("Register(dup) error = nil, want non-nil")
	}
	adapter, err := reg.Build("telegram", PlatformConfig{})
	if err != nil || adapter.Platform() != "telegram" {
		t.Fatalf("Build(telegram) = %v, %v, want telegram adapter", adapter, err)
	}
	if _, err := reg.Build("discord", PlatformConfig{}); err == nil {
		t.Fatalf("Build(unknown) error = nil, want non-nil")
	}
}
