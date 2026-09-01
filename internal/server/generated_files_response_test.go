package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/task"
)

// TestHTTPServerTaskResultIncludesGeneratedFilesWithLinks guards that the
// task-result response surfaces the task_completed event's GeneratedFiles as
// linked DTOs (path/url/download_url/name), built via the same fileURL
// contract Task 5 introduced, rather than as bare relative paths.
func TestHTTPServerTaskResultIncludesGeneratedFilesWithLinks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openServerTestRepo(t)
	scheduler := task.NewScheduler()
	events := adapter.NewMemoryEventBus()
	srv := NewHTTPServer(Config{Tasks: scheduler, Sessions: repo, WorkflowEvents: events})

	if err := repo.SaveAgentSession(ctx, domain.AgentSession{
		ID:        "session-gf-1",
		CompanyID: "company-1",
		AgentID:   "default-agent",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveAgentSession error = %v, want nil", err)
	}

	if err := scheduler.Add(ctx, domain.Task{
		ID:        "task-gf-1",
		CompanyID: "company-1",
		SessionID: "session-gf-1",
		Status:    domain.TaskDone,
		Input:     "写一个报告",
	}); err != nil {
		t.Fatalf("scheduler.Add error = %v, want nil", err)
	}
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:           "task_completed",
		TaskID:         "task-gf-1",
		Message:        "已生成报告",
		GeneratedFiles: []string{"out/report.md"},
	}); err != nil {
		t.Fatalf("events.Publish error = %v, want nil", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/task-gf-1/result", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET result status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got taskResultResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode(result response) error = %v, want nil", err)
	}
	if len(got.GeneratedFiles) != 1 {
		t.Fatalf("GeneratedFiles = %#v, want exactly 1 entry", got.GeneratedFiles)
	}
	gf := got.GeneratedFiles[0]
	wantURL := srv.fileURL("session-gf-1", "out/report.md", false)
	wantDownloadURL := srv.fileURL("session-gf-1", "out/report.md", true)
	if gf.Path != "out/report.md" || gf.URL != wantURL || gf.DownloadURL != wantDownloadURL || gf.Name != "report.md" {
		t.Fatalf("GeneratedFiles[0] = %#v, want path=out/report.md url=%q download_url=%q name=report.md", gf, wantURL, wantDownloadURL)
	}
}

// TestHTTPServerTaskResultGeneratedFilesEmptyIsEmptyArray guards that a task
// with no generated files serializes generated_files as [] rather than null,
// so GUI clients that .map() over the field do not need a nil guard.
func TestHTTPServerTaskResultGeneratedFilesEmptyIsEmptyArray(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheduler := task.NewScheduler()
	events := adapter.NewMemoryEventBus()
	srv := NewHTTPServer(Config{Tasks: scheduler, WorkflowEvents: events})

	if err := scheduler.Add(ctx, domain.Task{
		ID:        "task-gf-empty",
		CompanyID: "company-1",
		Status:    domain.TaskDone,
		Input:     "say hi",
	}); err != nil {
		t.Fatalf("scheduler.Add error = %v, want nil", err)
	}
	if err := events.Publish(ctx, domain.RuntimeEvent{
		Type:    "task_completed",
		TaskID:  "task-gf-empty",
		Message: "hi",
	}); err != nil {
		t.Fatalf("events.Publish error = %v, want nil", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/task-gf-empty/result", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET result status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !containsEmptyArrayField(rec.Body.Bytes(), "generated_files") {
		t.Fatalf("response body = %s, want generated_files serialized as []", rec.Body.String())
	}
	var got taskResultResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode(result response) error = %v, want nil", err)
	}
	if got.GeneratedFiles == nil || len(got.GeneratedFiles) != 0 {
		t.Fatalf("GeneratedFiles = %#v, want non-nil empty slice", got.GeneratedFiles)
	}
}

// containsEmptyArrayField does a light textual check that the given JSON
// field is serialized as "field":[] in the raw body, distinguishing an
// explicit empty array from a field that was omitted or serialized as null.
func containsEmptyArrayField(body []byte, field string) bool {
	return strings.Contains(string(body), `"`+field+`":[]`)
}

// 「结果端点把相对路径写进 assistant turn」那条测试随写入方一起删除：
// conversation_turns 退役后（spec §3 取舍 A2）端点不写任何东西，GeneratedFiles 从
// assistant/message 事件的 generated_files 载荷经 storage.projectTurns 取并集还原
// ——那条不变量由 storage 包的 TestAToolLoopTaskProjectsToOneAssistantTurn 与
// TestASuspendedAndResumedTaskStillProjectsToOneTurnPerRole 守着。下面这条测试守的
// 是另一端：/turns 出口把相对路径渲染成链接 DTO。

// TestHTTPServerSessionTurnsIncludeGeneratedFilesWithLinks guards that the
// history-turns endpoint (GET /v1/sessions/{id}/turns), consumed directly by
// the GUI to reload history, surfaces each turn's GeneratedFiles as linked
// DTOs rather than the bare relative paths stored in sqlite.
func TestHTTPServerSessionTurnsIncludeGeneratedFilesWithLinks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openServerTestRepo(t)
	createdAt := time.Now()
	session := domain.AgentSession{
		ID:        "session-gf-history-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession error = %v, want nil", err)
	}
	appendTurnEvents(t, repo, session.ID, domain.ConversationTurn{
		ID:             "turn-gf-history-1",
		SessionID:      session.ID,
		TaskID:         "task-1",
		AgentID:        "agent-1",
		Role:           domain.ConversationRoleAssistant,
		Content:        "已完成",
		CreatedAt:      createdAt.Add(time.Second),
		GeneratedFiles: []string{"out/report.md"},
	})
	srv := NewHTTPServer(Config{Sessions: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-gf-history-1/turns", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET turns status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// Decode loosely (not into domain.ConversationTurn) because the response
	// shape's generated_files field is now an array of link DTOs, not bare
	// strings -- the exact augmentation this test verifies.
	var turns []struct {
		ID             string          `json:"id"`
		SessionID      string          `json:"session_id"`
		Role           string          `json:"role"`
		Content        string          `json:"content"`
		GeneratedFiles []GeneratedFile `json:"generated_files"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&turns); err != nil {
		t.Fatalf("Decode(turns) error = %v, want nil", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %#v, want exactly 1", turns)
	}
	got := turns[0]
	// Existing fields must remain intact (least-breaking augmentation).
	// id 是投影折叠出来的 "<task_id>:<role>"，与退役中 recordAssistantTurn 写的形状
	// 逐字一致；夹具传进去的 "turn-gf-history-1" 是那条**事件**的 turn_id。
	if got.ID != "task-1:assistant" || got.SessionID != session.ID || got.Role != string(domain.ConversationRoleAssistant) || got.Content != "已完成" {
		t.Fatalf("turn base fields = %#v, want existing fields preserved", got)
	}
	if len(got.GeneratedFiles) != 1 {
		t.Fatalf("turn GeneratedFiles = %#v, want exactly 1 entry", got.GeneratedFiles)
	}
	gf := got.GeneratedFiles[0]
	wantURL := srv.fileURL(session.ID, "out/report.md", false)
	wantDownloadURL := srv.fileURL(session.ID, "out/report.md", true)
	if gf.Path != "out/report.md" || gf.URL != wantURL || gf.DownloadURL != wantDownloadURL || gf.Name != "report.md" {
		t.Fatalf("turn GeneratedFiles[0] = %#v, want path=out/report.md url=%q download_url=%q name=report.md", gf, wantURL, wantDownloadURL)
	}
}

// TestHTTPServerSessionTurnsGeneratedFilesEmptyIsEmptyArray guards that a
// turn with no generated files serializes generated_files as [] not null on
// the history endpoint, matching the task-result endpoint's contract.
func TestHTTPServerSessionTurnsGeneratedFilesEmptyIsEmptyArray(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openServerTestRepo(t)
	createdAt := time.Now()
	session := domain.AgentSession{
		ID:        "session-gf-history-empty",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession error = %v, want nil", err)
	}
	appendTurnEvents(t, repo, session.ID, domain.ConversationTurn{
		ID:        "turn-gf-history-empty",
		SessionID: session.ID,
		TaskID:    "task-1",
		AgentID:   "agent-1",
		Role:      domain.ConversationRoleUser,
		Content:   "你好",
		CreatedAt: createdAt.Add(time.Second),
	})
	srv := NewHTTPServer(Config{Sessions: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-gf-history-empty/turns", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET turns status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !containsEmptyArrayField(rec.Body.Bytes(), "generated_files") {
		t.Fatalf("response body = %s, want generated_files serialized as []", rec.Body.String())
	}
}
