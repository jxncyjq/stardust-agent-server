package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/observability"
)

func TestSSEEventsFiltersAndSanitizesPayload(t *testing.T) {
	bus := observability.NewEventBus(8)
	srv := NewHTTPServer(Config{
		AdminToken:     "token",
		PlatformEvents: bus,
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/events?type=task.completed", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	go func() {
		time.Sleep(10 * time.Millisecond)
		if err := bus.Publish(context.Background(), observability.EventEnvelope{
			Type:      "task.completed",
			SubjectID: "task-1",
			Data: map[string]any{
				"task_id": "task-1",
				"prompt":  "secret prompt",
			},
		}); err != nil {
			t.Errorf("EventBus.Publish(task.completed) error = %v, want nil", err)
		}
		if err := bus.Publish(context.Background(), observability.EventEnvelope{
			Type: "workflow.completed",
			Data: map[string]any{"workflow_id": "workflow-1"},
		}); err != nil {
			t.Errorf("EventBus.Publish(workflow.completed) error = %v, want nil", err)
		}
		if err := bus.Close(); err != nil {
			t.Errorf("EventBus.Close() error = %v, want nil", err)
		}
	}()

	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "event: task.completed") {
		t.Fatalf("GET /v1/events body = %q, want task.completed event", body)
	}
	if strings.Contains(body, "workflow.completed") {
		t.Fatalf("GET /v1/events body = %q, want filtered workflow event omitted", body)
	}
	if strings.Contains(body, "secret prompt") || strings.Contains(body, "prompt") {
		t.Fatalf("GET /v1/events body leaked prompt content: %q", body)
	}
}

func TestSSEEventsSanitizesNestedArgumentsAndTruncates(t *testing.T) {
	bus := observability.NewEventBus(8)
	srv := NewHTTPServer(Config{AdminToken: "token", PlatformEvents: bus})
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	big := strings.Repeat("A", 2000)
	go func() {
		// Give ServeHTTP time to Subscribe before we Publish/Close: EventBus
		// replays its event history to new subscribers, but only if they
		// subscribe before Close (internal/observability/eventbus.go), so
		// publishing too early here would race the handler and drop the
		// event entirely rather than testing sanitization.
		time.Sleep(10 * time.Millisecond)
		if err := bus.Publish(context.Background(), observability.EventEnvelope{
			Type:      "approval_pending",
			SubjectID: "task-1",
			Data: map[string]any{
				"task_id":   "task-1",
				"ticket_id": "ticket-1",
				"tool":      "write_file",
				"arguments": map[string]string{
					"path":    "/tmp/x",
					"api_key": "SUPER-SECRET-KEY",
					"content": big,
				},
			},
		}); err != nil {
			t.Errorf("Publish(approval_pending) error = %v, want nil", err)
		}
		if err := bus.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	}()

	srv.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "/tmp/x") {
		t.Fatalf("body = %q, want non-sensitive arg path present", body)
	}
	if strings.Contains(body, "SUPER-SECRET-KEY") || strings.Contains(body, "api_key") {
		t.Fatalf("body leaked sensitive nested arg: %q", body)
	}
	if strings.Contains(body, big) {
		t.Fatalf("body carried untruncated 2000-byte content")
	}
	if !strings.Contains(body, "truncated") {
		t.Fatalf("body = %q, want truncation marker for large content", body)
	}
}

// TestSSEEventsReturnsOnClientDisconnect covers the goroutine/subscriber leak
// fixed for Important I-1: handleEvents used a bare `for event := range
// events` with no select on r.Context().Done(), so a client disconnecting
// from an idle bus (or one filtered by ?type= that never matches) left the
// handler goroutine blocked forever on the channel receive, leaking the
// goroutine, the EventBus subscriber map entry, and its buffered channel.
// This test subscribes to an idle bus (no Publish, no Close) and cancels the
// request context, asserting the handler returns promptly instead of
// blocking forever.
func TestSSEEventsReturnsOnClientDisconnect(t *testing.T) {
	bus := observability.NewEventBus(8)
	srv := NewHTTPServer(Config{AdminToken: "token", PlatformEvents: bus})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	// Give ServeHTTP time to reach Subscribe before we cancel, so the
	// cancellation actually races the blocked channel receive rather than
	// preempting Subscribe itself.
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// handler returned after client disconnect — no leak.
	case <-time.After(2 * time.Second):
		t.Fatal("handleEvents did not return after client context cancellation (goroutine/subscriber leak)")
	}
}
