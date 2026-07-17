package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestLoggerAddsRequiredFields(t *testing.T) {
	var out bytes.Buffer
	logger, err := NewLogger(&out, LoggerConfig{})
	if err != nil {
		t.Fatalf("NewLogger(default) error = %v, want nil", err)
	}

	WithTaskID(WithRequestID(WithComponent(logger, "server"), "req-1"), "task-1").Info("task submitted")

	var entry map[string]any
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatalf("json.Unmarshal(log entry) error = %v, want nil; log=%s", err, out.String())
	}
	for _, key := range []string{"time", "level", "msg", "component", "request_id", "task_id"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("log entry missing key %q: %#v", key, entry)
		}
	}
	if entry["level"] != slog.LevelInfo.String() {
		t.Errorf("log entry level = %v, want %s", entry["level"], slog.LevelInfo.String())
	}
	if entry["msg"] != "task submitted" {
		t.Errorf("log entry msg = %v, want task submitted", entry["msg"])
	}
	if entry["component"] != "server" || entry["request_id"] != "req-1" || entry["task_id"] != "task-1" {
		t.Errorf("log entry fields = %#v, want component/request_id/task_id", entry)
	}
}

func TestNewLoggerRejectsUnknownLevel(t *testing.T) {
	_, err := NewLogger(&bytes.Buffer{}, LoggerConfig{Level: "loud"})
	if err == nil {
		t.Fatalf("NewLogger(unknown level) error = nil, want error")
	}
}
