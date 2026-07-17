package gateway

import (
	"context"
	"testing"
)

func TestBuildAdaptersRegistersEnabledPlatforms(t *testing.T) {
	reg := NewPlatformRegistry()
	if err := reg.Register(PlatformEntry{
		Platform:         "telegram",
		MaxMessageLength: 4096,
		Factory:          func(PlatformConfig) (ChannelAdapter, error) { return stubAdapter{name: "telegram"}, nil },
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	cfg := GatewayConfig{Platforms: map[string]PlatformSettings{
		"telegram": {Enabled: true, Token: "tok", PollTimeoutSeconds: 30},
		"discord":  {Enabled: false},
	}}
	adapters, router, err := BuildAdapters(cfg, reg)
	if err != nil {
		t.Fatalf("BuildAdapters() error = %v, want nil", err)
	}
	if len(adapters) != 1 || adapters[0].Platform() != "telegram" {
		t.Fatalf("adapters = %v, want only telegram (discord disabled)", adapters)
	}
	// Router can route to the built adapter.
	if err := router.Route(context.Background(), DeliveryTarget{Platform: "telegram", ChatID: "1"}, "hi"); err != nil {
		t.Fatalf("router.Route() error = %v", err)
	}
}

func TestBuildAdaptersFailsLoudOnUnregisteredEnabledPlatform(t *testing.T) {
	reg := NewPlatformRegistry()
	cfg := GatewayConfig{Platforms: map[string]PlatformSettings{"telegram": {Enabled: true, Token: "t"}}}
	if _, _, err := BuildAdapters(cfg, reg); err == nil {
		t.Fatalf("BuildAdapters(unregistered) error = nil, want non-nil")
	}
}
