package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/task"
)

type ServiceConfig struct {
	Config     config.Config
	Scheduler  *task.BackgroundScheduler
	HTTPServer *http.Server
	Listener   net.Listener
	Logger     *slog.Logger
}

type Service struct {
	scheduler  *task.BackgroundScheduler
	httpServer *http.Server
	listener   net.Listener
	interval   time.Duration
	logger     *slog.Logger
}

func New(cfg ServiceConfig) (*Service, error) {
	interval := time.Second
	if cfg.Config.Service.BackgroundInterval != "" {
		parsed, err := time.ParseDuration(cfg.Config.Service.BackgroundInterval)
		if err != nil {
			return nil, fmt.Errorf("parse background interval %q: %w", cfg.Config.Service.BackgroundInterval, err)
		}
		interval = parsed
	}
	scheduler := cfg.Scheduler
	if scheduler == nil {
		scheduler = task.NewBackgroundScheduler()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{
		scheduler:  scheduler,
		httpServer: cfg.HTTPServer,
		listener:   cfg.Listener,
		interval:   interval,
		logger:     observability.WithComponent(logger, "service"),
	}, nil
}

func (s *Service) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return nil
	}
	s.logger.Info("service started", "background_interval", s.interval.String())
	done := s.scheduler.Start(ctx, s.interval)
	httpDone := s.startHTTP(ctx)
	<-done
	if httpDone != nil {
		if err := <-httpDone; err != nil {
			return err
		}
	}
	s.logger.Info("service stopped")
	return nil
}

func (s *Service) startHTTP(ctx context.Context) <-chan error {
	if s.httpServer == nil || s.listener == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() {
		err := s.httpServer.Serve(s.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(shutdownCtx)
	}()
	return done
}
