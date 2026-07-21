package service

import (
	"context"
	"log/slog"
	"time"
)

// httpShutdowner is the shutdown surface of *http.Server, named so the failure
// path can be tested without arranging a real connection that refuses to drain.
type httpShutdowner interface {
	Shutdown(ctx context.Context) error
}

// shutdownHTTPServer stops the server within the grace period and reports a
// failure instead of discarding it.
//
// This runs at the top of a goroutine, with no caller left to return to — the
// boundary the fail-loud rule names explicitly. When the grace period expires
// with connections still open they are cut and the client sees an EOF; without
// this line the server recorded nothing at all, so operations could not tell an
// orderly shutdown from a forced one.
func shutdownHTTPServer(ctx context.Context, logger *slog.Logger, server httpShutdowner, grace time.Duration) {
	shutdownCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.WarnContext(shutdownCtx, "graceful shutdown",
			"grace", grace,
			"consequence", "in-flight connections were cut",
			"error", err)
	}
}
