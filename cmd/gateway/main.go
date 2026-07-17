// Command legion-gateway bridges IM platforms to a running Legion core over its
// HTTP API and SSE event stream. It is fully outbound (long polling), so it needs
// no public URL. Configure via a JSON file (path in LEGION_GATEWAY_CONFIG or the
// first argument); secrets come from environment variables named in that file.
package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/stardust/legion-agent/internal/gateway"
	"github.com/stardust/legion-agent/internal/gateway/platforms/telegram"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	configPath := os.Getenv("LEGION_GATEWAY_CONFIG")
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}
	if configPath == "" {
		log.Fatal("legion-gateway: config path required (LEGION_GATEWAY_CONFIG or arg 1)")
	}
	cfg, err := gateway.Load(configPath)
	if err != nil {
		log.Fatalf("legion-gateway: load config: %v", err)
	}

	reg := gateway.NewPlatformRegistry()
	if err := reg.Register(gateway.PlatformEntry{
		Platform:         "telegram",
		Factory:          telegram.New,
		MaxMessageLength: 4096,
		PIISafe:          true,
	}); err != nil {
		log.Fatalf("legion-gateway: register telegram: %v", err)
	}

	adapters, router, err := gateway.BuildAdapters(cfg, reg)
	if err != nil {
		log.Fatalf("legion-gateway: build adapters: %v", err)
	}
	if len(adapters) == 0 {
		log.Fatal("legion-gateway: no platforms enabled")
	}

	ctx := context.Background()
	binder, err := gateway.OpenSQLiteBinder(ctx, cfg.Binding.SQLitePath)
	if err != nil {
		log.Fatalf("legion-gateway: open binder: %v", err)
	}
	defer func() {
		if err := binder.Close(); err != nil {
			logger.Error("close binder", "err", err)
		}
	}()

	core := gateway.NewHTTPCoreClient(cfg.Core.BaseURL, cfg.Core.Token)
	runner := gateway.NewGatewayRunner(cfg, core, binder, router, gateway.NewDeliveryTracker(), adapters, logger)

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("legion-gateway started", "platforms", len(adapters), "core", cfg.Core.BaseURL)
	if err := runner.Run(ctx); err != nil {
		log.Fatalf("legion-gateway: run: %v", err)
	}
	logger.Info("legion-gateway stopped")
}
