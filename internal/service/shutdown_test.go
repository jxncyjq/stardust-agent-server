package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Audit item V21: `_ = s.httpServer.Shutdown(shutdownCtx)` in a goroutine's top
// level — the one boundary the fail-loud rule names explicitly.
//
// When the one-second grace period expires with connections still open, those
// connections are cut and the client sees an EOF, while the server records
// nothing. Operations cannot tell an orderly shutdown from a forced one.

type stubShutdowner struct{ err error }

func (s stubShutdowner) Shutdown(context.Context) error { return s.err }

func TestShutdownHTTPServerReportsFailure(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	shutdownHTTPServer(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)),
		stubShutdowner{err: errors.New("context deadline exceeded")}, time.Millisecond)

	got := logs.String()
	for _, want := range []string{"WARN", "graceful shutdown", "context deadline exceeded"} {
		if !strings.Contains(got, want) {
			t.Fatalf("logger output = %q, want it to contain %q", got, want)
		}
	}
}

func TestShutdownHTTPServerQuietOnCleanStop(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	shutdownHTTPServer(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)),
		stubShutdowner{}, time.Millisecond)
	if got := logs.String(); got != "" {
		t.Errorf("logger output = %q, want nothing logged on a clean shutdown", got)
	}
}
