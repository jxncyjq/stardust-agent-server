package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Both tests below change the process working directory via t.Chdir, so neither
// may be marked t.Parallel().
//
// TestDefaultLoggerFailsLoudWhenLogFileUnavailable pins the fail-loud contract of
// defaultLogger: when the log destination cannot be created, the process must be
// told, not silently downgraded to io.Discard (which would mute every later
// Warn/Error in the whole binary).
func TestDefaultLoggerFailsLoudWhenLogFileUnavailable(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// Occupy the log directory path with a regular file so MkdirAll fails.
	if err := os.WriteFile(filepath.Join(dir, filepath.Dir(defaultLogFilePath)), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", filepath.Dir(defaultLogFilePath), err)
	}

	logger, err := defaultLogger()

	if err == nil {
		t.Fatalf("defaultLogger() error = nil, want non-nil")
	}
	if logger != nil {
		t.Fatalf("defaultLogger() logger = %v, want nil on error", logger)
	}
	if !strings.Contains(err.Error(), defaultLogFilePath) {
		t.Fatalf("defaultLogger() error = %q, want it to name %q", err, defaultLogFilePath)
	}
}

func TestDefaultLoggerSucceedsInWritableWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())

	logger, err := defaultLogger()

	if err != nil {
		t.Fatalf("defaultLogger() error = %v, want nil", err)
	}
	if logger == nil {
		t.Fatalf("defaultLogger() logger = nil, want non-nil")
	}
}
