package cli

import (
	"context"
	"io"
	"log/slog"

	"github.com/stardust/legion-agent/internal/observability"
)

// platformApprovalSink implements manualgate.ApprovalEventSink by translating
// approval lifecycle notifications into observability.EventEnvelope values on the
// platform bus behind /v1/events. Publish failures are logged Warn and swallowed:
// SSE is a best-effort notification layer, and the on-disk approval ticket — not
// this event — is the source of truth (design §3.3).
type platformApprovalSink struct {
	platform *observability.EventBus
	logger   *slog.Logger
}

// newPlatformApprovalSink returns a sink publishing to platform. A nil logger
// discards. platform must not be nil.
func newPlatformApprovalSink(platform *observability.EventBus, logger *slog.Logger) *platformApprovalSink {
	if platform == nil {
		panic("newPlatformApprovalSink: platform event bus must not be nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &platformApprovalSink{platform: platform, logger: logger}
}

// ApprovalPending publishes an approval_pending envelope. arguments are carried
// as-is; the SSE write boundary (sanitizeEventData) truncates and strips
// sensitive sub-keys before they leave the process.
func (s *platformApprovalSink) ApprovalPending(ctx context.Context, taskID, ticketID, tool string, args map[string]string) {
	data := map[string]any{"task_id": taskID, "ticket_id": ticketID, "tool": tool}
	if args != nil {
		data["arguments"] = args
	}
	s.publish(ctx, "approval_pending", taskID, ticketID, data)
}

// ApprovalResolved publishes an approval_resolved envelope.
func (s *platformApprovalSink) ApprovalResolved(ctx context.Context, taskID, ticketID, decision string) {
	s.publish(ctx, "approval_resolved", taskID, ticketID, map[string]any{
		"task_id": taskID, "ticket_id": ticketID, "decision": decision,
	})
}

func (s *platformApprovalSink) publish(ctx context.Context, eventType, taskID, ticketID string, data map[string]any) {
	if err := s.platform.Publish(ctx, observability.EventEnvelope{
		Type:      eventType,
		SubjectID: taskID,
		Data:      data,
	}); err != nil {
		s.logger.Warn("approval event sink: platform publish failed",
			"type", eventType, "task_id", taskID, "ticket_id", ticketID, "error", err)
	}
}
