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
	for event := range events {
		if eventType != "" && event.Type != eventType {
			continue
		}
		if err := writeSSEEvent(w, event); err != nil {
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
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

func sanitizeEventData(data map[string]any) map[string]any {
	sanitized := make(map[string]any, len(data))
	for key, value := range data {
		if isSensitiveEventField(key) {
			continue
		}
		sanitized[key] = value
	}
	return sanitized
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
