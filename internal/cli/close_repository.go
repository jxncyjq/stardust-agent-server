package cli

import (
	"io"
	"log/slog"
)

// closeRepositoryLogging closes a SQLite-backed repository and reports a failure
// instead of discarding it.
//
// SQLite's Close triggers a WAL checkpoint, so — unlike an HTTP response body,
// whose Close error carries no meaning — this one does. Dropping it let backup,
// data export and retention --apply finish with exit code 0 while data had not
// actually landed, leaving the operator believing the backup or retention pass
// had succeeded.
//
// It warns rather than returning an error: these calls run in deferred cleanup,
// where changing the command's exit path would mean reporting a cleanup problem
// as the command's own failure (or masking a real error already on its way out).
// The operation name is what tells the operator which command was affected.
func closeRepositoryLogging(logger *slog.Logger, closer io.Closer, operation string) {
	if err := closer.Close(); err != nil {
		logger.Warn("close repository",
			"component", "cli",
			"operation", operation,
			"error", err)
	}
}
