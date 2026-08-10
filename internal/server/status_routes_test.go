package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/task"
)

func TestHTTPServerListRuntimeEventsReturnsPublishedEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := adapter.NewMemoryEventBus()
	srv := NewHTTPServer(Config{Tasks: task.NewScheduler(), WorkflowEvents: events})
	for _, evt := range []domain.RuntimeEvent{
		{Type: "task_started", TaskID: "task-rt-1", Message: "started"},
		{Type: "task_completed", TaskID: "task-rt-1", Message: "done"},
	} {
		if err := events.Publish(ctx, evt); err != nil {
			t.Fatalf("events.Publish error = %v, want nil", err)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/runtime-events", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/runtime-events status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got []domain.RuntimeEvent
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode(runtime events) error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("runtime events len = %d, want 2 (%#v)", len(got), got)
	}
	var sawCompleted bool
	for _, evt := range got {
		if evt.Type == "task_completed" {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Fatalf("runtime events missing task_completed: %#v", got)
	}
}

func TestHTTPServerListRuntimeEventsWithoutBusReturnsEmptyArray(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{Tasks: task.NewScheduler()})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/runtime-events", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/runtime-events status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "[]\n" {
		t.Fatalf("GET /v1/runtime-events body = %q, want empty JSON array", got)
	}
}

func TestHTTPServerListTasksReturnsAddedTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheduler := task.NewScheduler()
	srv := NewHTTPServer(Config{Tasks: scheduler})
	for _, id := range []string{"task-list-1", "task-list-2"} {
		if err := scheduler.Add(ctx, domain.Task{ID: id, CompanyID: "company-1", Status: domain.TaskPending, Input: "in-" + id}); err != nil {
			t.Fatalf("scheduler.Add(%q) error = %v, want nil", id, err)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got []domain.Task
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode(tasks) error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("GET /v1/tasks len = %d, want 2 (%#v)", len(got), got)
	}
	ids := map[string]bool{}
	for _, taskItem := range got {
		ids[taskItem.ID] = true
	}
	if !ids["task-list-1"] || !ids["task-list-2"] {
		t.Fatalf("GET /v1/tasks ids = %#v, want both task-list-1 and task-list-2", ids)
	}
}

// TestHTTPServerTaskItemRoutesNotShadowedByList guards the switch ordering: the
// exact GET /v1/tasks branch must not swallow GET /v1/tasks/{id} or
// /v1/tasks/{id}/result, which carry a trailing path segment and have their own
// handlers.
func TestHTTPServerTaskItemRoutesNotShadowedByList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheduler := task.NewScheduler()
	events := adapter.NewMemoryEventBus()
	srv := NewHTTPServer(Config{Tasks: scheduler, WorkflowEvents: events})
	if err := scheduler.Add(ctx, domain.Task{ID: "task-item-1", CompanyID: "company-1", Status: domain.TaskDone, Input: "hi"}); err != nil {
		t.Fatalf("scheduler.Add error = %v, want nil", err)
	}
	if err := events.Publish(ctx, domain.RuntimeEvent{Type: "task_completed", TaskID: "task-item-1", Message: "answer text"}); err != nil {
		t.Fatalf("events.Publish error = %v, want nil", err)
	}

	// GET /v1/tasks/{id} must hit handleGetTask (returns a single Task object).
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks/task-item-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks/{id} status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var single domain.Task
	if err := json.NewDecoder(rec.Body).Decode(&single); err != nil {
		t.Fatalf("Decode(single task) error = %v, want nil (got list?) body=%s", err, rec.Body.String())
	}
	if single.ID != "task-item-1" {
		t.Fatalf("GET /v1/tasks/{id} id = %q, want task-item-1", single.ID)
	}

	// GET /v1/tasks/{id}/result must hit handleGetTaskResult (returns answer text).
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks/task-item-1/result", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks/{id}/result status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var result taskResultResponse
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("Decode(result) error = %v, want nil", err)
	}
	if result.TaskID != "task-item-1" || result.Result != "answer text" {
		t.Fatalf("GET /v1/tasks/{id}/result = %#v, want answer text for task-item-1", result)
	}
}

// captureSessionStore is a minimal SessionStore stub that records the turn
// passed to AppendConversationTurnIfAbsent so tests can assert on exactly what
// the HTTP layer persisted, without pulling in a real storage backend.
type captureSessionStore struct {
	appended []domain.ConversationTurn
}

func (c *captureSessionStore) ListAgentSessions(ctx context.Context, companyID string, agentID string) ([]domain.AgentSession, error) {
	return nil, nil
}

func (c *captureSessionStore) ListConversationTurns(ctx context.Context, sessionID string, limit int) ([]domain.ConversationTurn, error) {
	return nil, nil
}

func (c *captureSessionStore) GetAgentSession(ctx context.Context, sessionID string) (domain.AgentSession, bool, error) {
	return domain.AgentSession{}, false, nil
}

func (c *captureSessionStore) SaveAgentSession(ctx context.Context, session domain.AgentSession) error {
	return nil
}

func (c *captureSessionStore) DeleteAgentSession(ctx context.Context, sessionID string) error {
	return nil
}

func (c *captureSessionStore) AppendConversationTurnIfAbsent(ctx context.Context, turn domain.ConversationTurn) (bool, error) {
	c.appended = append(c.appended, turn)
	return true, nil
}

// TestHTTPServerTaskResultPersistsAssistantTurnWithTokenUsage guards that the
// assistant conversation turn recorded when GET /v1/tasks/{id}/result observes
// a completed, session-bound task carries the same token counts reported in the
// response body — not zeros. Regression coverage for the token-usage
// persistence gap: recordAssistantTurn previously built the turn without any
// of the usage fields from taskResult.
func TestHTTPServerTaskResultPersistsAssistantTurnWithTokenUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheduler := task.NewScheduler()
	events := adapter.NewMemoryEventBus()
	sessions := &captureSessionStore{}
	srv := NewHTTPServer(Config{Tasks: scheduler, WorkflowEvents: events, Sessions: sessions})
	if err := scheduler.Add(ctx, domain.Task{
		ID:        "task-tokens-1",
		CompanyID: "company-1",
		SessionID: "session-tokens-1",
		AgentID:   "agent-tokens-1",
		Status:    domain.TaskDone,
		Input:     "hi",
	}); err != nil {
		t.Fatalf("scheduler.Add error = %v, want nil", err)
	}
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:             "task_completed",
		TaskID:           "task-tokens-1",
		Message:          "answer text",
		PromptTokens:     1200,
		CompletionTokens: 340,
		CachedTokens:     800,
		TotalTokens:      1540,
	}); err != nil {
		t.Fatalf("events.Publish error = %v, want nil", err)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks/task-tokens-1/result", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks/{id}/result status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if len(sessions.appended) != 1 {
		t.Fatalf("appended turns = %d, want 1 (%#v)", len(sessions.appended), sessions.appended)
	}
	got := sessions.appended[0]
	want := domain.ConversationTurn{
		PromptTokens:     1200,
		CompletionTokens: 340,
		CachedTokens:     800,
		TotalTokens:      1540,
	}
	if got.PromptTokens != want.PromptTokens || got.CompletionTokens != want.CompletionTokens ||
		got.CachedTokens != want.CachedTokens || got.TotalTokens != want.TotalTokens {
		t.Fatalf("persisted assistant turn tokens = %#v, want %#v", got, want)
	}
}
