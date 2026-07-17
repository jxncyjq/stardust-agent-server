package observability

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

type LoggerConfig struct {
	Level  string
	Format string
}

func NewLogger(w io.Writer, cfg LoggerConfig) (*slog.Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}
	format := strings.ToLower(strings.TrimSpace(cfg.Format))
	if format == "" || format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("unknown log format %q", cfg.Format)
}

func WithComponent(logger *slog.Logger, component string) *slog.Logger {
	return logger.With("component", component)
}

func WithRequestID(logger *slog.Logger, requestID string) *slog.Logger {
	return logger.With("request_id", requestID)
}

func WithTaskID(logger *slog.Logger, taskID string) *slog.Logger {
	return logger.With("task_id", taskID)
}

func parseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown log level %q", level)
	}
}
