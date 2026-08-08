package cli

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stardust/legion-agent/internal/observability"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestPlatformBrowserSinkPublishesOpened(t *testing.T) {
	bus := observability.NewEventBus(8)
	events, cancel := bus.Subscribe(context.Background())
	defer cancel()
	sink := newPlatformBrowserEventSink(bus, testLogger())

	sink.SessionOpened(context.Background(), "sess-1", "task-1", "https://example.com")

	select {
	case env := <-events:
		if env.Type != "browser:session_opened" || env.Data["session_id"] != "sess-1" || env.Data["url"] != "https://example.com" {
			t.Fatalf("bad event: %+v", env)
		}
		if env.SubjectID != "sess-1" {
			t.Fatalf("SubjectID = %q, want sess-1", env.SubjectID)
		}
		if env.Data["task_id"] != "task-1" {
			t.Fatalf("task_id = %v, want task-1", env.Data["task_id"])
		}
	default:
		t.Fatal("no event published")
	}
}

func TestPlatformBrowserSinkPublishesClosed(t *testing.T) {
	bus := observability.NewEventBus(8)
	events, cancel := bus.Subscribe(context.Background())
	defer cancel()
	sink := newPlatformBrowserEventSink(bus, testLogger())

	sink.SessionClosed(context.Background(), "sess-2")

	select {
	case env := <-events:
		if env.Type != "browser:session_closed" || env.Data["session_id"] != "sess-2" {
			t.Fatalf("bad event: %+v", env)
		}
		if env.SubjectID != "sess-2" {
			t.Fatalf("SubjectID = %q, want sess-2", env.SubjectID)
		}
	default:
		t.Fatal("no event published")
	}
}

// A nil platform bus must be a safe no-op (mirrors the nil-guard in publish).
func TestPlatformBrowserSinkNilBusNoPanic(t *testing.T) {
	sink := newPlatformBrowserEventSink(nil, testLogger())
	sink.SessionOpened(context.Background(), "sess-3", "task-3", "https://example.com")
	sink.SessionClosed(context.Background(), "sess-3")
}
