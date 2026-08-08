package cli

import (
	"context"
	"io"
	"log/slog"

	"github.com/stardust/legion-agent/internal/observability"
)

// platformBrowserEventSink implements tool.BrowserEventSink by translating
// browser session lifecycle notifications into observability.EventEnvelope
// values on the platform bus behind /v1/events. The tool layer intentionally
// does not depend on observability; the bridge lives here in the cli layer
// (correct dependency direction, same as platformApprovalSink).
//
// Publish failures are logged Warn and swallowed: SSE is a best-effort
// notification layer for the GUI browser view to discover active sessions, not
// a source of truth. A nil platform bus makes every publish a no-op, so the
// scenarios without an event bus (tests, non-sqlite paths) stay unbroken.
type platformBrowserEventSink struct {
	platform *observability.EventBus
	logger   *slog.Logger
}

// newPlatformBrowserEventSink returns a sink publishing to platform. A nil
// logger discards. A nil platform bus is tolerated: publish is a no-op.
func newPlatformBrowserEventSink(platform *observability.EventBus, logger *slog.Logger) *platformBrowserEventSink {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &platformBrowserEventSink{platform: platform, logger: logger}
}

// SessionOpened publishes a browser:session_opened envelope. The event type and
// Data keys (session_id/task_id/url) are the contract the GUI browser view
// forwards and parses; keep them in lockstep with the GUI-side handler.
func (s *platformBrowserEventSink) SessionOpened(ctx context.Context, sessionID, taskID, url string) {
	s.publish(ctx, "browser:session_opened", sessionID, map[string]any{
		"session_id": sessionID,
		"task_id":    taskID,
		"url":        url,
	})
}

// SessionClosed publishes a browser:session_closed envelope.
func (s *platformBrowserEventSink) SessionClosed(ctx context.Context, sessionID string) {
	s.publish(ctx, "browser:session_closed", sessionID, map[string]any{"session_id": sessionID})
}

func (s *platformBrowserEventSink) publish(ctx context.Context, eventType, sessionID string, data map[string]any) {
	if s.platform == nil {
		return
	}
	if err := s.platform.Publish(ctx, observability.EventEnvelope{
		Type:      eventType,
		SubjectID: sessionID,
		Data:      data,
	}); err != nil {
		s.logger.Warn("browser event sink: platform publish failed",
			"type", eventType, "session_id", sessionID, "error", err)
	}
}
