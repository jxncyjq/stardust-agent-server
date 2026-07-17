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
