package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/stardust/legion-agent/internal/observability"
)

func (s *HTTPServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.platformEvents == nil {
		writeError(w, http.StatusServiceUnavailable, "event bus is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Send the response headers immediately, before subscribing/waiting for
	// the first event. Without this, net/http withholds any bytes from the
	// client until the handler's first Write, so a subscriber connecting to
	// an idle bus would see the connection hang (indistinguishable from a
	// dead server) instead of getting the 200 + text/event-stream contract
	// the SSE endpoint promises.
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	eventType := r.URL.Query().Get("type")
	events, cancel := s.platformEvents.Subscribe(r.Context())
	defer cancel()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			if eventType != "" && event.Type != eventType {
				continue
			}
			if err := writeSSEEvent(w, event); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case <-r.Context().Done():
			// Client disconnected (or the request context was otherwise
			// cancelled). Return so the deferred cancel() above unsubscribes
			// from the bus — without this case, a bare `range events` blocks
			// forever on an idle bus, leaking this goroutine, the bus's
			// subscriber map entry, and its buffered channel.
			return
		}
	}
}

func writeSSEEvent(w io.Writer, event observability.EventEnvelope) error {
	data, err := json.Marshal(sanitizeEventData(event.Data))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event.Type); err != nil {
		return err
	}
	if event.ID != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", event.ID); err != nil {
			return err
		}
	}
	if event.SubjectID != "" {
		if _, err := fmt.Fprintf(w, ": subject_id=%s\n", event.SubjectID); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

// maxEventStringLen bounds any single string value emitted over SSE. Larger
// values (e.g. a write_file content argument) are truncated with a marker so a
// pending-approval event cannot ship an unbounded/secret-laden payload.
const maxEventStringLen = 512

// sanitizeEventData strips sensitive keys and truncates large string values from
// an event payload before it leaves the process, recursing into nested maps so a
// sensitive sub-key (e.g. arguments.api_key) or a huge sub-value cannot slip
// through under a benign top-level key like "arguments".
func sanitizeEventData(data map[string]any) map[string]any {
	sanitized := make(map[string]any, len(data))
	for key, value := range data {
		if isSensitiveEventField(key) {
			continue
		}
		sanitized[key] = sanitizeEventValue(value)
	}
	return sanitized
}

// sanitizeEventValue recurses into nested maps and truncates strings so
// sanitizeEventData's guarantees hold at every depth, not just the top level.
func sanitizeEventValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return sanitizeEventData(v)
	case map[string]string:
		return sanitizeStringMap(v)
	case string:
		return truncateEventString(v)
	// NOTE: slice carriers ([]any / []map[string]any / []string) are not
	// recursed today because no current SSE Data producer emits them
	// (eventbridge.translate = scalars; approval arguments = map[string]string).
	// If a future producer puts a slice in Data, extend this switch or its
	// sensitive sub-keys/large values will bypass sanitization.
	default:
		return v
	}
}

// sanitizeStringMap strips sensitive keys and truncates values of a string map
// (e.g. an approval ticket's Arguments). It returns a map[string]any so nested
// results compose with sanitizeEventValue. Exported-within-package for reuse by
// the /v1/approvals list handler.
func sanitizeStringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for key, value := range m {
		if isSensitiveEventField(key) {
			continue
		}
		out[key] = truncateEventString(value)
	}
	return out
}

// truncateEventString bounds s to maxEventStringLen, appending a marker noting
// how many bytes were dropped so truncation is visible rather than silent.
func truncateEventString(s string) string {
	if len(s) <= maxEventStringLen {
		return s
	}
	return s[:maxEventStringLen] + fmt.Sprintf("…[truncated %d bytes]", len(s)-maxEventStringLen)
}

func isSensitiveEventField(key string) bool {
	normalized := strings.ToLower(key)
	return normalized == "prompt" ||
		normalized == "input" ||
		normalized == "secret" ||
		normalized == "api_key" ||
		strings.Contains(normalized, "secret") ||
		strings.Contains(normalized, "token")
}
