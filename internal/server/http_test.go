package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/storage"
	"github.com/stardust/legion-agent/internal/task"
	"github.com/stardust/legion-agent/internal/workflow"
)

func TestHTTPServerHealthz(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{Tasks: task.NewScheduler()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("Decode(/healthz) error = %v, want nil", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("GET /healthz status body = %#v, want ok", body)
	}
}

func TestHTTPServerSubmitsAndGetsTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheduler := task.NewScheduler()
	srv := NewHTTPServer(Config{Tasks: scheduler})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewBufferString(`{
		"id": "task-http-1",
		"company_id": "company-1",
		"input": "do the thing"
	}`))

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/tasks status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	got, ok, err := scheduler.Get(ctx, "task-http-1")
	if err != nil {
		t.Fatalf("Scheduler.Get(%q) error = %v, want nil", "task-http-1", err)
	}
	if !ok || got.Status != domain.TaskPending || got.Input != "do the thing" {
		t.Fatalf("Scheduler.Get(%q) = %#v, %t, want pending submitted task", "task-http-1", got, ok)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/tasks/task-http-1", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks/task-http-1 status = %d, want %d", rec.Code, http.StatusOK)
	}
	var taskResp domain.Task
	if err := json.NewDecoder(rec.Body).Decode(&taskResp); err != nil {
		t.Fatalf("Decode(task response) error = %v, want nil", err)
	}
	if taskResp.ID != "task-http-1" || taskResp.Status != domain.TaskPending {
		t.Fatalf("GET /v1/tasks/task-http-1 = %#v, want submitted task", taskResp)
	}
}

func TestHTTPServerGetsTaskResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheduler := task.NewScheduler()
	events := adapter.NewMemoryEventBus()
	srv := NewHTTPServer(Config{Tasks: scheduler, WorkflowEvents: events})

	if err := scheduler.Add(ctx, domain.Task{
		ID:        "task-result-1",
		CompanyID: "company-1",
		Status:    domain.TaskDone,
		Input:     "say hello",
	}); err != nil {
		t.Fatalf("scheduler.Add error = %v", err)
	}
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:        "task_completed",
		TaskID:      "task-result-1",
		Message:     "hello there",
		TotalTokens: 561,
		ElapsedMs:   52000,
	}); err != nil {
		t.Fatalf("events.Publish error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/task-result-1/result", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks/task-result-1/result status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got taskResultResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode(result response) error = %v, want nil", err)
	}
	if got.TaskID != "task-result-1" || got.Status != string(domain.TaskDone) || got.Result != "hello there" {
		t.Fatalf("GET result = %#v, want done task with answer text", got)
	}
	if got.TotalTokens != 561 || got.ElapsedMs != 52000 {
		t.Fatalf("GET result tokens/elapsed = %d/%d, want 561/52000", got.TotalTokens, got.ElapsedMs)
	}
}

func TestHTTPServerGetTaskResultNotFound(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{Tasks: task.NewScheduler(), WorkflowEvents: adapter.NewMemoryEventBus()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/missing/result", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing result status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHTTPServerListsSessionsAndTurns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	createdAt := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	session := domain.AgentSession{
		ID:        "session-http-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Title:     "HTTP session",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession(%q) error = %v, want nil", session.ID, err)
	}
	for _, turn := range []domain.ConversationTurn{
		{
			ID:           "turn-1",
			SessionID:    session.ID,
			TaskID:       "task-1",
			AgentID:      "agent-1",
			ModelProfile: "dev",
			Role:         domain.ConversationRoleUser,
			Content:      "你是什么模型",
			CreatedAt:    createdAt.Add(time.Second),
		},
		{
			ID:           "turn-2",
			SessionID:    session.ID,
			TaskID:       "task-1",
			AgentID:      "agent-1",
			ModelProfile: "dev",
			Role:         domain.ConversationRoleAssistant,
			Content:      "我是 Legion Agent",
			CreatedAt:    createdAt.Add(2 * time.Second),
		},
	} {
		if err := repo.AppendConversationTurn(ctx, turn); err != nil {
			t.Fatalf("AppendConversationTurn(%q) error = %v, want nil", turn.ID, err)
		}
	}
	srv := NewHTTPServer(Config{Sessions: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions?company_id=company-1&agent_id=agent-1", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/sessions status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var sessions []domain.AgentSession
	if err := json.NewDecoder(rec.Body).Decode(&sessions); err != nil {
		t.Fatalf("Decode(sessions) error = %v, want nil", err)
	}
	if len(sessions) != 1 || sessions[0].ID != session.ID {
		t.Fatalf("GET /v1/sessions = %#v, want session %q", sessions, session.ID)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/sessions/session-http-1/turns?limit=1", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/sessions/session-http-1/turns status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var turns []domain.ConversationTurn
	if err := json.NewDecoder(rec.Body).Decode(&turns); err != nil {
		t.Fatalf("Decode(turns) error = %v, want nil", err)
	}
	if len(turns) != 1 || turns[0].ID != "turn-2" || turns[0].Content != "我是 Legion Agent" {
		t.Fatalf("GET /v1/sessions/session-http-1/turns = %#v, want latest turn", turns)
	}
}

func openServerTestRepo(t *testing.T) *storage.SQLiteRepository {
	t.Helper()
	repo, err := storage.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	return repo
}

func TestHTTPServerCreatesSessionWithProject(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	srv := NewHTTPServer(Config{Sessions: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewBufferString(`{
		"project": "测试项目",
		"agent_id": "default-agent",
		"company_id": "default-company",
		"title": "会话1"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/sessions status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var session domain.AgentSession
	if err := json.NewDecoder(rec.Body).Decode(&session); err != nil {
		t.Fatalf("Decode(session) error = %v, want nil", err)
	}
	if session.ID == "" || session.Project != "测试项目" || session.Title != "会话1" {
		t.Fatalf("POST /v1/sessions = %#v, want generated id with project/title", session)
	}
	if session.AgentID != "default-agent" || session.CompanyID != "default-company" {
		t.Fatalf("POST /v1/sessions agent/company = %q/%q, want defaults applied", session.AgentID, session.CompanyID)
	}
}

func TestHTTPServerCreateSessionUnavailableStore(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewBufferString(`{"project":"p"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /v1/sessions without store status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestHTTPServerTaskWithSessionRecordsUserTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openServerTestRepo(t)
	scheduler := task.NewScheduler()
	srv := NewHTTPServer(Config{Tasks: scheduler, Sessions: repo, WorkflowEvents: adapter.NewMemoryEventBus()})

	session := domain.AgentSession{
		ID:        "session-task-1",
		CompanyID: "default-company",
		AgentID:   "default-agent",
		Project:   "测试项目",
		Title:     "会话1",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession error = %v, want nil", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewBufferString(`{
		"id": "task-session-1",
		"input": "你好",
		"agent_id": "default-agent",
		"company_id": "default-company",
		"session_id": "session-task-1"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/tasks status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	turns, err := repo.ListConversationTurns(ctx, "session-task-1", 0)
	if err != nil {
		t.Fatalf("ListConversationTurns error = %v, want nil", err)
	}
	if len(turns) != 1 || turns[0].Role != domain.ConversationRoleUser || turns[0].Content != "你好" {
		t.Fatalf("turns after submit = %#v, want one user turn 你好", turns)
	}
}

func TestHTTPServerTaskWithMissingSessionFailsLoud(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	scheduler := task.NewScheduler()
	srv := NewHTTPServer(Config{Tasks: scheduler, Sessions: repo, WorkflowEvents: adapter.NewMemoryEventBus()})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewBufferString(`{
		"id": "task-missing-session",
		"input": "你好",
		"session_id": "does-not-exist"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /v1/tasks with missing session status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	// The task must not have been enqueued when its session is missing.
	if _, ok, _ := scheduler.Get(context.Background(), "task-missing-session"); ok {
		t.Fatalf("task was enqueued despite missing session, want it rejected")
	}
}

func TestHTTPServerCompletedTaskRecordsAssistantTurnOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openServerTestRepo(t)
	scheduler := task.NewScheduler()
	events := adapter.NewMemoryEventBus()
	srv := NewHTTPServer(Config{Tasks: scheduler, Sessions: repo, WorkflowEvents: events})

	session := domain.AgentSession{
		ID:        "session-done-1",
		CompanyID: "default-company",
		AgentID:   "default-agent",
		Project:   "测试项目",
		Title:     "会话1",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession error = %v, want nil", err)
	}
	if err := scheduler.Add(ctx, domain.Task{
		ID:        "task-done-1",
		CompanyID: "default-company",
		AgentID:   "default-agent",
		SessionID: "session-done-1",
		Status:    domain.TaskDone,
		Input:     "你好",
	}); err != nil {
		t.Fatalf("scheduler.Add error = %v, want nil", err)
	}
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:        "task_completed",
		TaskID:      "task-done-1",
		Message:     "你好，我是模型的真实回答",
		TotalTokens: 42,
		ElapsedMs:   1200,
	}); err != nil {
		t.Fatalf("events.Publish error = %v, want nil", err)
	}

	// Poll the result endpoint several times; the assistant turn must be written
	// exactly once regardless of how many times it is queried.
	for i := range 3 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/tasks/task-done-1/result", nil)
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET result attempt %d status = %d, want %d body=%s", i, rec.Code, http.StatusOK, rec.Body.String())
		}
	}

	turns, err := repo.ListConversationTurns(ctx, "session-done-1", 0)
	if err != nil {
		t.Fatalf("ListConversationTurns error = %v, want nil", err)
	}
	assistantCount := 0
	for _, turn := range turns {
		if turn.Role == domain.ConversationRoleAssistant {
			assistantCount++
			if turn.Content != "你好，我是模型的真实回答" {
				t.Fatalf("assistant turn content = %q, want model answer", turn.Content)
			}
		}
	}
	if assistantCount != 1 {
		t.Fatalf("assistant turn count = %d, want exactly 1 (turns=%#v)", assistantCount, turns)
	}
}

func TestHTTPServerPatchSessionUpdatesOnlyProvidedFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openServerTestRepo(t)
	createdAt := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	session := domain.AgentSession{
		ID:        "session-patch-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Project:   "原项目",
		Title:     "原标题",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession error = %v, want nil", err)
	}
	srv := NewHTTPServer(Config{Sessions: repo})

	// Rename: send only title; project must be preserved.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/session-patch-1", bytes.NewBufferString(`{"title":"改名后"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH title status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var updated domain.AgentSession
	if err := json.NewDecoder(rec.Body).Decode(&updated); err != nil {
		t.Fatalf("Decode(patched session) error = %v, want nil", err)
	}
	if updated.Title != "改名后" || updated.Project != "原项目" || updated.Archived {
		t.Fatalf("PATCH title = %#v, want title changed, project preserved, not archived", updated)
	}

	// Archive: send only archived; title/project must be preserved.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/v1/sessions/session-patch-1", bytes.NewBufferString(`{"archived":true}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH archived status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(&updated); err != nil {
		t.Fatalf("Decode(archived session) error = %v, want nil", err)
	}
	if !updated.Archived || updated.Title != "改名后" || updated.Project != "原项目" {
		t.Fatalf("PATCH archived = %#v, want archived with title/project preserved", updated)
	}

	stored, ok, err := repo.GetAgentSession(ctx, "session-patch-1")
	if err != nil || !ok {
		t.Fatalf("GetAgentSession(patched) = _, %t, %v, want found", ok, err)
	}
	if !stored.Archived || stored.Title != "改名后" || stored.Project != "原项目" {
		t.Fatalf("stored session = %#v, want persisted title/archived with project intact", stored)
	}
}

func TestHTTPServerPatchMissingSessionReturns404(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	srv := NewHTTPServer(Config{Sessions: repo})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/does-not-exist", bytes.NewBufferString(`{"title":"x"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH missing session status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHTTPServerDeleteSessionRemovesSessionAndTurns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openServerTestRepo(t)
	createdAt := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	session := domain.AgentSession{
		ID:        "session-delete-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Project:   "项目",
		Title:     "会话",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession error = %v, want nil", err)
	}
	if err := repo.AppendConversationTurn(ctx, domain.ConversationTurn{
		ID:        "del-turn-1",
		SessionID: session.ID,
		TaskID:    "task-1",
		AgentID:   "agent-1",
		Role:      domain.ConversationRoleUser,
		Content:   "你好",
		CreatedAt: createdAt.Add(time.Second),
	}); err != nil {
		t.Fatalf("AppendConversationTurn error = %v, want nil", err)
	}
	srv := NewHTTPServer(Config{Sessions: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/sessions/session-delete-1", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE session status = %d, want %d body=%s", rec.Code, http.StatusNoContent, rec.Body.String())
	}

	// GET /v1/sessions must no longer include it.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/sessions?company_id=company-1&agent_id=agent-1", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET sessions status = %d, want %d", rec.Code, http.StatusOK)
	}
	var sessions []domain.AgentSession
	if err := json.NewDecoder(rec.Body).Decode(&sessions); err != nil {
		t.Fatalf("Decode(sessions) error = %v, want nil", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("GET sessions = %#v, want empty after delete", sessions)
	}

	// GET turns must be empty.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/sessions/session-delete-1/turns", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET turns status = %d, want %d", rec.Code, http.StatusOK)
	}
	var turns []domain.ConversationTurn
	if err := json.NewDecoder(rec.Body).Decode(&turns); err != nil {
		t.Fatalf("Decode(turns) error = %v, want nil", err)
	}
	if len(turns) != 0 {
		t.Fatalf("GET turns = %#v, want empty after cascade delete", turns)
	}
}

func TestHTTPServerDeleteMissingSessionReturns404(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	srv := NewHTTPServer(Config{Sessions: repo})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/sessions/no-such", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing session status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestDeleteSessionRemovesSessionDir(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openServerTestRepo(t)
	workingDir := t.TempDir()
	session := domain.AgentSession{
		ID:         "session-delete-dir-1",
		CompanyID:  "company-1",
		AgentID:    "agent-1",
		WorkingDir: workingDir,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession error = %v, want nil", err)
	}

	// Populate the session's on-disk directory (spec §4.0: working_dir-bound
	// sessions keep state under <working_dir>/.stardust/session/<id>) with a
	// marker file, so the assertion proves an actual directory removal rather
	// than a no-op against a directory that never existed.
	sessionDir := sessionstate.SessionDir(sessionstate.SessionBase("", session.WorkingDir), session.ID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v, want nil", sessionDir, err)
	}
	marker := filepath.Join(sessionDir, "marker.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", marker, err)
	}

	srv := NewHTTPServer(Config{Sessions: repo})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+session.ID, nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE session status = %d, want %d body=%s", rec.Code, http.StatusNoContent, rec.Body.String())
	}

	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("session dir %q still exists after delete (stat err = %v), want removed", sessionDir, err)
	}
}

func TestHTTPServerPatchDoesNotMatchTurnsRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openServerTestRepo(t)
	createdAt := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	session := domain.AgentSession{
		ID:        "session-turns-guard",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Title:     "会话",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession error = %v, want nil", err)
	}
	if err := repo.AppendConversationTurn(ctx, domain.ConversationTurn{
		ID:        "guard-turn-1",
		SessionID: session.ID,
		TaskID:    "task-1",
		AgentID:   "agent-1",
		Role:      domain.ConversationRoleUser,
		Content:   "你好",
		CreatedAt: createdAt.Add(time.Second),
	}); err != nil {
		t.Fatalf("AppendConversationTurn error = %v, want nil", err)
	}
	srv := NewHTTPServer(Config{Sessions: repo})

	// A PATCH against the /turns path must not be routed to the session patch
	// handler (which would otherwise try to treat "session-turns-guard/turns" as a
	// session id). The GET /turns route stays read-only; PATCH on it is not found.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/session-turns-guard/turns", bytes.NewBufferString(`{"title":"x"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH /turns status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}

	// The GET /turns route must still work and the session must be untouched.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/sessions/session-turns-guard/turns", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /turns status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var turns []domain.ConversationTurn
	if err := json.NewDecoder(rec.Body).Decode(&turns); err != nil {
		t.Fatalf("Decode(turns) error = %v, want nil", err)
	}
	if len(turns) != 1 || turns[0].ID != "guard-turn-1" {
		t.Fatalf("GET /turns = %#v, want the unmodified turn", turns)
	}
}

func TestHTTPServerSendsAndListsAgentMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	srv := NewHTTPServer(Config{Messages: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/writer/messages", bytes.NewBufferString(`{
		"company_id": "company-1",
		"from": "researcher",
		"task_id": "TASK-20260525-001",
		"type": "result",
		"summary": "缓存实现调研完成",
		"artifact": "docs/research/cache.md"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/agents/writer/messages status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var created domain.AgentMessage
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("Decode(created message) error = %v, want nil", err)
	}
	if created.ID == "" || created.ToAgentID != "writer" || created.FromAgentID != "researcher" || created.Status != domain.AgentMessageUnread {
		t.Fatalf("created message = %#v, want unread researcher -> writer", created)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/agents/writer/messages?company_id=company-1&status=unread", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/agents/writer/messages status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var messages []domain.AgentMessage
	if err := json.NewDecoder(rec.Body).Decode(&messages); err != nil {
		t.Fatalf("Decode(messages) error = %v, want nil", err)
	}
	if len(messages) != 1 || messages[0].ID != created.ID || messages[0].Summary != "缓存实现调研完成" {
		t.Fatalf("GET /v1/agents/writer/messages = %#v, want created message", messages)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/agents/writer/messages?company_id=company-1&status=unread&mark_read=true", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/agents/writer/messages?mark_read status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	read, err := repo.ListAgentMessages(ctx, domain.AgentMessageQuery{ToAgentID: "writer", Status: domain.AgentMessageRead})
	if err != nil {
		t.Fatalf("ListAgentMessages(read) error = %v, want nil", err)
	}
	if len(read) != 1 || read[0].ID != created.ID || read[0].ReadAt.IsZero() {
		t.Fatalf("read messages = %#v, want created message marked read", read)
	}
}

func TestHTTPServerListsWaitingWorkflows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	def := workflow.Definition{
		ID: "workflow-http-1",
		Root: workflow.Node{
			ID:        "wait-node",
			Kind:      workflow.NodeWaitEvent,
			EventType: "external.ready",
		},
	}
	result := workflow.Result{
		WorkflowID: def.ID,
		Status:     workflow.StatusWaitingEvent,
		Nodes:      []workflow.NodeResult{{NodeID: "wait-node", Status: workflow.StatusWaitingEvent}},
	}
	if err := repo.SaveWorkflowState(ctx, def, result); err != nil {
		t.Fatalf("SaveWorkflowState(%q) error = %v, want nil", def.ID, err)
	}
	srv := NewHTTPServer(Config{
		Tasks:     task.NewScheduler(),
		Workflows: repo,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/workflows/waiting", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/workflows/waiting status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var workflows []storage.WorkflowState
	if err := json.NewDecoder(rec.Body).Decode(&workflows); err != nil {
		t.Fatalf("Decode(waiting workflows) error = %v, want nil", err)
	}
	if len(workflows) != 1 || workflows[0].Definition.ID != def.ID || workflows[0].Result.Status != workflow.StatusWaitingEvent {
		t.Fatalf("GET /v1/workflows/waiting = %#v, want waiting workflow %q", workflows, def.ID)
	}
}

func TestHTTPWorkflowSubmitResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	events := adapter.NewMemoryEventBus()
	engine := workflow.NewEngine(workflow.Config{
		Scheduler: task.NewScheduler(),
		Approvals: approval.NewService(),
		Events:    events,
		Audit:     adapter.NewMemoryAuditLog(),
	})
	srv := NewHTTPServer(Config{
		Tasks:          task.NewScheduler(),
		Workflows:      repo,
		WorkflowStates: repo,
		WorkflowEngine: engine,
		WorkflowEvents: events,
	})

	definitionJSON := `{
		"id": "workflow-api",
		"root": {
			"id": "wait-ready",
			"kind": "wait_event",
			"event_type": "external.ready",
			"event_task_id": "task-42",
			"event_message_contains": "ready"
		}
	}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", bytes.NewBufferString(definitionJSON))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /v1/workflows status = %d, want %d body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var submitted storage.WorkflowState
	if err := json.NewDecoder(rec.Body).Decode(&submitted); err != nil {
		t.Fatalf("Decode(submitted workflow) error = %v, want nil", err)
	}
	if submitted.Result.Status != workflow.StatusWaitingEvent {
		t.Fatalf("POST /v1/workflows status = %s, want %s", submitted.Result.Status, workflow.StatusWaitingEvent)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/workflows/workflow-api/events", bytes.NewBufferString(`{
		"type": "external.ready",
		"task_id": "task-42",
		"message": "payload ready"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/workflows/workflow-api/events status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resumed storage.WorkflowState
	if err := json.NewDecoder(rec.Body).Decode(&resumed); err != nil {
		t.Fatalf("Decode(resumed workflow) error = %v, want nil", err)
	}
	if resumed.Result.Status != workflow.StatusCompleted {
		t.Fatalf("resume workflow status = %s, want %s", resumed.Result.Status, workflow.StatusCompleted)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/workflows/workflow-api", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/workflows/workflow-api status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestHTTPServerTaskWithImagesPersistsImagesAndAnnotatesTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openServerTestRepo(t)
	scheduler := task.NewScheduler()
	srv := NewHTTPServer(Config{Tasks: scheduler, Sessions: repo, WorkflowEvents: adapter.NewMemoryEventBus()})

	session := domain.AgentSession{
		ID:        "session-img-1",
		CompanyID: "default-company",
		AgentID:   "default-agent",
		Project:   "测试项目",
		Title:     "看图会话",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession error = %v, want nil", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewBufferString(`{
		"id": "task-img-1",
		"input": "描述这张图",
		"agent_id": "default-agent",
		"company_id": "default-company",
		"session_id": "session-img-1",
		"images": ["data:image/png;base64,iVBORw0KGgo=", "data:image/jpeg;base64,/9j/4AAQ=="]
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/tasks with images status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	got, ok, err := scheduler.Get(ctx, "task-img-1")
	if err != nil {
		t.Fatalf("scheduler.Get error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("task-img-1 not enqueued, want it stored")
	}
	if len(got.Images) != 2 || got.Images[0] != "data:image/png;base64,iVBORw0KGgo=" {
		t.Fatalf("stored task images = %#v, want the two posted data URIs", got.Images)
	}

	// The persisted user turn must annotate the attachment count without storing
	// the base64 payload (which would bloat sqlite).
	turns, err := repo.ListConversationTurns(ctx, "session-img-1", 0)
	if err != nil {
		t.Fatalf("ListConversationTurns error = %v, want nil", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns after submit = %#v, want one user turn", turns)
	}
	content := turns[0].Content
	if want := "描述这张图\n[附图 2 张]"; content != want {
		t.Fatalf("user turn content = %q, want %q", content, want)
	}
	if bytes.Contains([]byte(content), []byte("base64")) {
		t.Fatalf("user turn content embeds base64 image data: %q", content)
	}
}

func TestHTTPServerTaskWithoutImagesLeavesTurnUnannotated(t *testing.T) {
	t.Parallel()

	if got := userTurnContent("你好", 0); got != "你好" {
		t.Fatalf("userTurnContent(no images) = %q, want unchanged 你好", got)
	}
	if got := userTurnContent("", 3); got != "[附图 3 张]" {
		t.Fatalf("userTurnContent(empty input, 3 images) = %q, want bare marker", got)
	}
}

func TestCreateSessionStoresValidMode(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	srv := NewHTTPServer(Config{Sessions: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewBufferString(`{
		"company_id": "c1",
		"agent_id": "a1",
		"mode": "manual"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/sessions status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var session domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("Decode(session) error = %v, want nil", err)
	}
	if session.Mode != domain.ModeManual {
		t.Fatalf("POST /v1/sessions mode = %q, want %q", session.Mode, domain.ModeManual)
	}
}

func TestCreateSessionRejectsInvalidMode(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	srv := NewHTTPServer(Config{Sessions: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewBufferString(`{
		"company_id": "c1",
		"agent_id": "a1",
		"mode": "bogus"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /v1/sessions with invalid mode status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestCreateSessionDefaultsAutoWhenModeOmitted(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	srv := NewHTTPServer(Config{Sessions: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewBufferString(`{
		"company_id": "c1",
		"agent_id": "a1"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/sessions status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var session domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("Decode(session) error = %v, want nil", err)
	}
	if session.Mode != domain.ModeAuto {
		t.Fatalf("POST /v1/sessions mode = %q, want default %q", session.Mode, domain.ModeAuto)
	}
}

func TestPatchSessionUpdatesMode(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	srv := NewHTTPServer(Config{Sessions: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewBufferString(`{
		"company_id": "c1",
		"agent_id": "a1"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/sessions status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var created domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("Decode(created session) error = %v, want nil", err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/v1/sessions/"+created.ID, bytes.NewBufferString(`{"mode":"plan"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH mode status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var updated domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("Decode(patched session) error = %v, want nil", err)
	}
	if updated.Mode != domain.ModePlan {
		t.Fatalf("PATCH mode = %q, want %q", updated.Mode, domain.ModePlan)
	}
}

func TestCreateTaskInheritsSessionMode(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	scheduler := task.NewScheduler()
	srv := NewHTTPServer(Config{Sessions: repo, Tasks: scheduler})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewBufferString(`{
		"company_id": "c1",
		"agent_id": "a1",
		"mode": "plan"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/sessions status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var session domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("Decode(session) error = %v, want nil", err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewBufferString(`{
		"id": "task-mode-1",
		"input": "do the thing",
		"session_id": "`+session.ID+`"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/tasks status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var created domain.Task
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("Decode(task) error = %v, want nil", err)
	}
	if created.Mode != domain.ModePlan {
		t.Fatalf("POST /v1/tasks mode = %q, want inherited %q", created.Mode, domain.ModePlan)
	}
}

func TestCreateSessionStoresWorkingDir(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	srv := NewHTTPServer(Config{Sessions: repo})
	workingDir := t.TempDir()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewBufferString(`{
		"company_id": "c1",
		"agent_id": "a1",
		"working_dir": "`+filepath.ToSlash(workingDir)+`"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/sessions status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var session domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("Decode(session) error = %v, want nil", err)
	}
	if session.WorkingDir != filepath.ToSlash(workingDir) {
		t.Fatalf("POST /v1/sessions working_dir = %q, want %q", session.WorkingDir, filepath.ToSlash(workingDir))
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/sessions?company_id=c1&agent_id=a1", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/sessions status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var listed []domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("Decode(sessions) error = %v, want nil", err)
	}
	if len(listed) != 1 || listed[0].WorkingDir != filepath.ToSlash(workingDir) {
		t.Fatalf("GET /v1/sessions listed = %#v, want single session with working_dir %q", listed, filepath.ToSlash(workingDir))
	}
}

func TestPatchSessionUpdatesWorkingDir(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	srv := NewHTTPServer(Config{Sessions: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewBufferString(`{
		"company_id": "c1",
		"agent_id": "a1"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/sessions status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var created domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("Decode(created session) error = %v, want nil", err)
	}
	if created.WorkingDir != "" {
		t.Fatalf("POST /v1/sessions working_dir = %q, want empty when omitted", created.WorkingDir)
	}

	workingDir := filepath.ToSlash(t.TempDir())
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/v1/sessions/"+created.ID, bytes.NewBufferString(`{"working_dir":"`+workingDir+`"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH working_dir status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var updated domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("Decode(patched session) error = %v, want nil", err)
	}
	if updated.WorkingDir != workingDir {
		t.Fatalf("PATCH working_dir = %q, want %q", updated.WorkingDir, workingDir)
	}
}

// TestPatchSessionRejectsWorkingDirChangeOnceSet guards against silently
// stranding a session's checkpoints: sessionstate.SessionBase derives a
// session's on-disk base from its *current* working_dir, so once a session has
// a non-empty working_dir, changing it to a different value would leave any
// checkpoint filed under the old base unreachable to future restarts (which
// only enumerate the *current* set of bases). A same-value PATCH is a no-op
// and must still succeed; a PATCH on a session whose working_dir is still
// empty must be allowed to set it for the first time.
func TestPatchSessionRejectsWorkingDirChangeOnceSet(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	srv := NewHTTPServer(Config{Sessions: repo})

	dirA := filepath.ToSlash(t.TempDir())
	dirB := filepath.ToSlash(t.TempDir())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewBufferString(`{
		"company_id": "c1",
		"agent_id": "a1",
		"working_dir": "`+dirA+`"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/sessions status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var created domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("Decode(created session) error = %v, want nil", err)
	}

	// Changing an already-set working_dir to a different value must be
	// rejected fail-loud (400), not silently applied.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/v1/sessions/"+created.ID, bytes.NewBufferString(`{"working_dir":"`+dirB+`"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH working_dir (change) status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	// PATCHing the same value back is a no-op and must succeed.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/v1/sessions/"+created.ID, bytes.NewBufferString(`{"working_dir":"`+dirA+`"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH working_dir (same value) status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// A session created with no working_dir may still have one set for the
	// first time.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewBufferString(`{
		"company_id": "c1",
		"agent_id": "a1"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/sessions (no working_dir) status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var createdBare domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &createdBare); err != nil {
		t.Fatalf("Decode(created bare session) error = %v, want nil", err)
	}
	if createdBare.WorkingDir != "" {
		t.Fatalf("POST /v1/sessions (no working_dir) working_dir = %q, want empty", createdBare.WorkingDir)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/v1/sessions/"+createdBare.ID, bytes.NewBufferString(`{"working_dir":"`+dirA+`"}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH working_dir (first set) status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var updatedBare domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &updatedBare); err != nil {
		t.Fatalf("Decode(patched bare session) error = %v, want nil", err)
	}
	if updatedBare.WorkingDir != dirA {
		t.Fatalf("PATCH working_dir (first set) = %q, want %q", updatedBare.WorkingDir, dirA)
	}
}

func TestCreateTaskInheritsSessionWorkingDir(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	scheduler := task.NewScheduler()
	srv := NewHTTPServer(Config{Sessions: repo, Tasks: scheduler})
	workingDir := filepath.ToSlash(t.TempDir())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewBufferString(`{
		"company_id": "c1",
		"agent_id": "a1",
		"working_dir": "`+workingDir+`"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/sessions status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var session domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("Decode(session) error = %v, want nil", err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewBufferString(`{
		"id": "task-workingdir-1",
		"input": "do the thing",
		"session_id": "`+session.ID+`"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/tasks status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var created domain.Task
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("Decode(task) error = %v, want nil", err)
	}
	if created.WorkingDir != workingDir {
		t.Fatalf("POST /v1/tasks working_dir = %q, want inherited %q", created.WorkingDir, workingDir)
	}
}

func TestCreateTaskRejectsNonDirWorkingDir(t *testing.T) {
	t.Parallel()
	repo := openServerTestRepo(t)
	scheduler := task.NewScheduler()
	srv := NewHTTPServer(Config{Sessions: repo, Tasks: scheduler})
	missingDir := filepath.ToSlash(filepath.Join(t.TempDir(), "does-not-exist"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewBufferString(`{
		"company_id": "c1",
		"agent_id": "a1",
		"working_dir": "`+missingDir+`"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/sessions status = %d, want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var session domain.AgentSession
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("Decode(session) error = %v, want nil", err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewBufferString(`{
		"id": "task-workingdir-baddir",
		"input": "do the thing",
		"session_id": "`+session.ID+`"
	}`))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /v1/tasks with non-dir session working_dir status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

type stubDecider struct {
	gotTask, gotTicket string
	gotStatus          approval.ApprovalStatus
	err                error
}

func (s *stubDecider) Decide(_ context.Context, taskID, ticketID string, status approval.ApprovalStatus) (approval.ToolApproval, error) {
	s.gotTask, s.gotTicket, s.gotStatus = taskID, ticketID, status
	if s.err != nil {
		return approval.ToolApproval{}, s.err
	}
	return approval.ToolApproval{TicketID: ticketID, TaskID: taskID, Status: status}, nil
}

func TestDecideApprovalRoutesApprove(t *testing.T) {
	dec := &stubDecider{}
	srv := NewHTTPServer(Config{ToolApprovals: dec})
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/t1/approvals/t1__c1", strings.NewReader(`{"decision":"approve"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if dec.gotTask != "t1" || dec.gotTicket != "t1__c1" || dec.gotStatus != approval.ApprovalApproved {
		t.Fatalf("decider got task=%q ticket=%q status=%q", dec.gotTask, dec.gotTicket, dec.gotStatus)
	}
}

func TestDecideApprovalInvalidDecision400(t *testing.T) {
	srv := NewHTTPServer(Config{ToolApprovals: &stubDecider{}})
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/t1/approvals/tk1", strings.NewReader(`{"decision":"maybe"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDecideApprovalUnknownTicket404(t *testing.T) {
	srv := NewHTTPServer(Config{ToolApprovals: &stubDecider{err: approval.ErrTicketNotFound}})
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/t1/approvals/nope", strings.NewReader(`{"decision":"deny"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDecideApprovalAlreadyDecided409(t *testing.T) {
	srv := NewHTTPServer(Config{ToolApprovals: &stubDecider{err: approval.ErrTicketAlreadyDecided}})
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/t1/approvals/tk1", strings.NewReader(`{"decision":"deny"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestDecideApprovalNilStore503(t *testing.T) {
	srv := NewHTTPServer(Config{})
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/t1/approvals/tk1", strings.NewReader(`{"decision":"approve"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
