package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/task"
)

func TestServiceStartRunsSchedulerAndStopsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	scheduler := task.NewBackgroundScheduler()
	var calls atomic.Int32
	scheduler.AddJob("count", func(context.Context) error {
		calls.Add(1)
		cancel()
		return nil
	})
	svc, err := New(ServiceConfig{
		Config: config.Config{
			Service: config.ServiceConfig{BackgroundInterval: "1ms"},
		},
		Scheduler: scheduler,
	})
	if err != nil {
		t.Fatalf("New(ServiceConfig) error = %v, want nil", err)
	}

	err = svc.Start(ctx)
	if err != nil {
		t.Fatalf("Start(ctx) error = %v, want nil", err)
	}
	if calls.Load() == 0 {
		t.Fatalf("background job calls = 0, want at least 1")
	}
}

func TestServiceStartWritesLifecycleLogs(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger, err := observability.NewLogger(&logs, observability.LoggerConfig{})
	if err != nil {
		t.Fatalf("NewLogger(default) error = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	scheduler := task.NewBackgroundScheduler()
	scheduler.AddJob("stop", func(context.Context) error {
		cancel()
		return nil
	})
	svc, err := New(ServiceConfig{
		Config: config.Config{
			Service: config.ServiceConfig{BackgroundInterval: "1ms"},
		},
		Scheduler: scheduler,
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("New(ServiceConfig) error = %v, want nil", err)
	}

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start(ctx) error = %v, want nil", err)
	}

	entries := decodeServiceLogEntries(t, logs.Bytes())
	assertServiceLogEntry(t, entries, "service started")
	assertServiceLogEntry(t, entries, "service stopped")
}

func TestServiceNewRejectsInvalidInterval(t *testing.T) {
	t.Parallel()
	_, err := New(ServiceConfig{
		Config: config.Config{
			Service: config.ServiceConfig{BackgroundInterval: "not-a-duration"},
		},
		Scheduler: task.NewBackgroundScheduler(),
	})
	if err == nil {
		t.Fatalf("New(invalid interval) error = nil, want error")
	}
}

func TestServiceStartReturnsWhenContextAlreadyCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc, err := New(ServiceConfig{
		Config: config.Config{
			Service: config.ServiceConfig{BackgroundInterval: time.Millisecond.String()},
		},
		Scheduler: task.NewBackgroundScheduler(),
	})
	if err != nil {
		t.Fatalf("New(ServiceConfig) error = %v, want nil", err)
	}

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start(canceled ctx) error = %v, want nil", err)
	}
}

func TestServiceStartRunsHTTPServerUntilCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v, want nil", err)
	}
	svc, err := New(ServiceConfig{
		Config: config.Config{
			Service: config.ServiceConfig{BackgroundInterval: time.Hour.String()},
		},
		Scheduler: task.NewBackgroundScheduler(),
		HTTPServer: &http.Server{
			Handler: handler,
		},
		Listener: listener,
	})
	if err != nil {
		t.Fatalf("New(ServiceConfig with HTTP server) error = %v, want nil", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- svc.Start(ctx)
	}()
	resp, err := waitForHTTP(t, "http://"+listener.Addr().String()+"/healthz")
	if err != nil {
		cancel()
		t.Fatalf("waitForHTTP(service health) error = %v, want nil", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Body.Close() error = %v, want nil", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("GET service health status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Service.Start() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Service.Start() did not stop after context cancellation")
	}
}

func waitForHTTP(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	var lastErr error
	for range 100 {
		resp, err := http.Get(url)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	return nil, lastErr
}

func decodeServiceLogEntries(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	var entries []map[string]any
	for decoder.More() {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatalf("Decode(service log entry) error = %v, want nil; logs=%s", err, string(data))
		}
		entries = append(entries, entry)
	}
	return entries
}

func assertServiceLogEntry(t *testing.T, entries []map[string]any, msg string) {
	t.Helper()
	for _, entry := range entries {
		if entry["msg"] != msg {
			continue
		}
		if entry["level"] != slog.LevelInfo.String() {
			t.Fatalf("service log %q level = %#v, want %s", msg, entry["level"], slog.LevelInfo.String())
		}
		if entry["component"] != "service" {
			t.Fatalf("service log %q component = %#v, want service", msg, entry["component"])
		}
		return
	}
	t.Fatalf("service logs missing msg %q; entries=%#v", msg, entries)
}
