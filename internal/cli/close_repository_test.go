package cli

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// Audit item V13: five `defer func() { _ = repo.Close() }()` sites dropped the
// error from closing a SQLite repository.
//
// SQLite's Close triggers a WAL checkpoint, so unlike an HTTP response body its
// error is meaningful: backup, data export and retention --apply would finish
// with exit code 0 while data had not actually landed, and the operator would
// believe the backup or retention pass succeeded.
//
// Cleanup must not change the command's exit path, so this is a warning rather
// than a returned error — but it must exist.

type failingCloser struct{ err error }

func (c failingCloser) Close() error { return c.err }

func TestCloseRepositoryLoggingReportsFailure(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	closeRepositoryLogging(slog.New(slog.NewTextHandler(&logs, nil)),
		failingCloser{err: errors.New("disk full during checkpoint")}, "backup")

	got := logs.String()
	for _, want := range []string{"WARN", "close repository", "backup", "disk full during checkpoint"} {
		if !strings.Contains(got, want) {
			t.Fatalf("logger output = %q, want it to contain %q", got, want)
		}
	}
}

func TestCloseRepositoryLoggingQuietOnSuccess(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	closeRepositoryLogging(slog.New(slog.NewTextHandler(&logs, nil)), failingCloser{}, "backup")
	if got := logs.String(); got != "" {
		t.Errorf("logger output = %q, want nothing logged on a clean close", got)
	}
}
