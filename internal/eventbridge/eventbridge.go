// Package eventbridge tees the runtime's poll-only RuntimeEvent stream into the
// push/subscribe platform event bus that backs the /v1/events SSE endpoint,
// without changing any existing RuntimeEvent publisher.
package eventbridge

import (
	"context"
	"io"
	"log/slog"
	"sync"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/observability"
)

// Bridge is a port.EventBus that tees every RuntimeEvent two ways: it appends to
// an internal slice (preserving the poll-only Events() contract the GEP / trust /
// degradation background jobs still rely on) and translates it into an
// observability.EventEnvelope pushed to the platform bus behind the /v1/events
// SSE stream. It is the single wiring point that connects the runtime's existing
// RuntimeEvent publishers to SSE.
type Bridge struct {
	mu       sync.Mutex
	events   []domain.RuntimeEvent
	platform *observability.EventBus
	logger   *slog.Logger
}

// New returns a Bridge teeing to platform. platform must not be nil (SSE wiring
// is the whole point of this type). logger records tee failures; a nil logger
// discards. A platform publish error is logged Warn and never propagated: the
// appended slice is the authoritative half of the old MemoryEventBus contract,
// and SSE is a best-effort notification layer (design §3.4.2).
func New(platform *observability.EventBus, logger *slog.Logger) *Bridge {
	if platform == nil {
		panic("eventbridge.New: platform event bus must not be nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Bridge{platform: platform, logger: logger}
}

// Publish appends the event to the internal snapshot (authoritative, for poll
// consumers) and tees a translated envelope to the platform bus (best-effort,
// for SSE). It returns an error only when the context is already done; a
// platform publish failure is logged Warn, not propagated.
func (b *Bridge) Publish(ctx context.Context, event domain.RuntimeEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	b.events = append(b.events, event)
	b.mu.Unlock()

	if err := b.platform.Publish(ctx, translate(event)); err != nil {
		b.logger.Warn("event bridge: platform publish failed",
			"type", event.Type, "task_id", event.TaskID, "error", err)
	}
	return nil
}

// Events returns a snapshot copy of every published RuntimeEvent, satisfying the
// poll-only half of port.EventBus that existing background jobs consume.
func (b *Bridge) Events() []domain.RuntimeEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]domain.RuntimeEvent(nil), b.events...)
}

// translate maps a RuntimeEvent to a platform EventEnvelope. Type is preserved
// verbatim (underscore form, matching every internal/runtime publisher) so the
// SSE type namespace equals the RuntimeEvent namespace. Token / elapsed fields
// are included only when non-zero to keep the payload lean. No "prompt"/"input"
// keys are emitted, so sanitizeEventData never has to strip lifecycle payloads.
func translate(ev domain.RuntimeEvent) observability.EventEnvelope {
	data := map[string]any{"task_id": ev.TaskID}
	if ev.Message != "" {
		data["message"] = ev.Message
	}
	if ev.PromptTokens != 0 {
		data["prompt_tokens"] = ev.PromptTokens
	}
	if ev.CompletionTokens != 0 {
		data["completion_tokens"] = ev.CompletionTokens
	}
	if ev.CachedTokens != 0 {
		data["cached_tokens"] = ev.CachedTokens
	}
	if ev.TotalTokens != 0 {
		data["total_tokens"] = ev.TotalTokens
	}
	if ev.ElapsedMs != 0 {
		data["elapsed_ms"] = ev.ElapsedMs
	}
	return observability.EventEnvelope{
		Type:      ev.Type,
		SubjectID: ev.TaskID,
		Data:      data,
		CreatedAt: ev.CreatedAt,
	}
}
