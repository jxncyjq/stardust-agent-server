package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// /turns 是 GUI 唯一的历史来源，conversationTurnResponse 把 domain.ConversationTurn
// 的每个字段都暴露出去。这条测试把**全部 DTO 字段**一次对账：其中任何一个在投影
// 出口被改坏（id / task_id / agent_id / model_profile / 四个 token 字段 /
// created_at / generated_files），这里必须红。
//
// 补它的理由是复审实测出来的洞：改动之前，把 task_id、agent_id、model_profile、
// 四个 token 字段在投影出口改坏，整个 internal/server 包**全绿**——GUI 的模型标签、
// token 标签、按任务归组全都没人守。
func TestHTTPServerSessionTurnsCarryEveryDTOField(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openServerTestRepo(t)
	createdAt := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	session := domain.AgentSession{
		ID:        "session-dto-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Title:     "字段对账",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession error = %v, want nil", err)
	}
	appendTurnEvents(t, repo, session.ID,
		domain.ConversationTurn{
			ID:        "sess-dto:0:0:user/message",
			SessionID: session.ID,
			TaskID:    "task-dto-1",
			AgentID:   "agent-1",
			Role:      domain.ConversationRoleUser,
			Content:   "你好",
			CreatedAt: createdAt.Add(time.Second),
		},
		domain.ConversationTurn{
			ID:               "sess-dto:0:0:assistant/message",
			SessionID:        session.ID,
			TaskID:           "task-dto-1",
			AgentID:          "agent-1",
			ModelProfile:     "dev",
			Role:             domain.ConversationRoleAssistant,
			Content:          "已完成",
			CreatedAt:        createdAt.Add(2 * time.Second),
			PromptTokens:     11,
			CompletionTokens: 22,
			CachedTokens:     3,
			TotalTokens:      36,
			GeneratedFiles:   []string{"out/report.md"},
		},
	)
	srv := NewHTTPServer(Config{Sessions: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-dto-1/turns", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /turns status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	// 独立声明一份与 conversationTurnResponse 同形的结构：解码走的是真实的 json
	// 标签，字段少一个、名字改一个都会在这里显出来。
	var got []struct {
		ID               string          `json:"id"`
		SessionID        string          `json:"session_id"`
		TaskID           string          `json:"task_id"`
		AgentID          string          `json:"agent_id"`
		ModelProfile     string          `json:"model_profile"`
		Role             string          `json:"role"`
		Content          string          `json:"content"`
		CreatedAt        time.Time       `json:"created_at"`
		PromptTokens     int             `json:"prompt_tokens"`
		CompletionTokens int             `json:"completion_tokens"`
		CachedTokens     int             `json:"cached_tokens"`
		TotalTokens      int             `json:"total_tokens"`
		GeneratedFiles   []GeneratedFile `json:"generated_files"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode(turns) error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("GET /turns = %#v, want 2 turns", got)
	}

	user := got[0]
	if user.ID != "task-dto-1:user" {
		t.Errorf("user.id = %q, want task-dto-1:user", user.ID)
	}
	if user.SessionID != session.ID {
		t.Errorf("user.session_id = %q, want %q", user.SessionID, session.ID)
	}
	if user.TaskID != "task-dto-1" {
		t.Errorf("user.task_id = %q, want task-dto-1：GUI 按它把气泡归到同一次提问", user.TaskID)
	}
	if user.AgentID != "agent-1" {
		t.Errorf("user.agent_id = %q, want agent-1：P3 计划列出的字段缺口之一", user.AgentID)
	}
	if user.Role != string(domain.ConversationRoleUser) || user.Content != "你好" {
		t.Errorf("user = {role:%q content:%q}, want {user 你好}", user.Role, user.Content)
	}
	if !user.CreatedAt.Equal(createdAt.Add(time.Second)) {
		t.Errorf("user.created_at = %v, want %v", user.CreatedAt, createdAt.Add(time.Second))
	}
	if len(user.GeneratedFiles) != 0 {
		t.Errorf("user.generated_files = %#v, want empty", user.GeneratedFiles)
	}

	asst := got[1]
	if asst.ID != "task-dto-1:assistant" {
		t.Errorf("assistant.id = %q, want task-dto-1:assistant：search_session 的 scroll 拿它当锚点", asst.ID)
	}
	if asst.TaskID != "task-dto-1" || asst.AgentID != "agent-1" || asst.ModelProfile != "dev" {
		t.Errorf("assistant = {task_id:%q agent_id:%q model_profile:%q}, want {task-dto-1 agent-1 dev}：GUI 的模型标签靠 model_profile",
			asst.TaskID, asst.AgentID, asst.ModelProfile)
	}
	if asst.Role != string(domain.ConversationRoleAssistant) || asst.Content != "已完成" {
		t.Errorf("assistant = {role:%q content:%q}, want {assistant 已完成}", asst.Role, asst.Content)
	}
	if !asst.CreatedAt.Equal(createdAt.Add(2 * time.Second)) {
		t.Errorf("assistant.created_at = %v, want %v", asst.CreatedAt, createdAt.Add(2*time.Second))
	}
	if asst.PromptTokens != 11 || asst.CompletionTokens != 22 || asst.CachedTokens != 3 || asst.TotalTokens != 36 {
		t.Errorf("assistant token 四件套 = %d/%d/%d/%d, want 11/22/3/36：GUI 的 token 标签靠它们",
			asst.PromptTokens, asst.CompletionTokens, asst.CachedTokens, asst.TotalTokens)
	}
	if len(asst.GeneratedFiles) != 1 || asst.GeneratedFiles[0].Path != "out/report.md" {
		t.Errorf("assistant.generated_files = %#v, want one out/report.md", asst.GeneratedFiles)
	}
}

// 一个带工具循环的任务在 /turns 上只呈现一条 assistant 项，正文是最终答案。
//
// 这是 C-1 在站点 ③ 的形状：GUI 的历史面板逐条渲染 /turns 的返回。逐事件投影的话，
// 一次带 N 轮工具的任务会多出 N 个正文为空的气泡（MaxToolRounds 可到 30），且
// generated_files 重复挂在多条上、文件卡片重复渲染。存储层的
// TestAToolLoopTaskProjectsToOneAssistantTurn 验的是投影本身，这条验的是它穿过
// HTTP 出口之后 GUI 真正看到的东西。
func TestHTTPServerSessionTurnsFoldAToolLoopIntoOneAssistantBubble(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openServerTestRepo(t)
	createdAt := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	session := domain.AgentSession{
		ID:        "session-loop-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Title:     "多轮",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession error = %v, want nil", err)
	}
	// 真实的多轮形状：一条 user + 三条 assistant（前两轮只请求工具，正文为空），
	// 全部属于同一个 task。
	appendTurnEvents(t, repo, session.ID,
		domain.ConversationTurn{
			ID: "sess-loop:0:0:user/message", SessionID: session.ID, TaskID: "task-loop-1",
			AgentID: "agent-1", Role: domain.ConversationRoleUser, Content: "读两个文件再写份报告",
			CreatedAt: createdAt.Add(time.Second),
		},
		domain.ConversationTurn{
			ID: "sess-loop:0:0:assistant/message", SessionID: session.ID, TaskID: "task-loop-1",
			AgentID: "agent-1", ModelProfile: "dev", Role: domain.ConversationRoleAssistant, Content: "",
			CreatedAt: createdAt.Add(2 * time.Second), PromptTokens: 100, TotalTokens: 100,
		},
		domain.ConversationTurn{
			ID: "sess-loop:0:1:assistant/message", SessionID: session.ID, TaskID: "task-loop-1",
			AgentID: "agent-1", ModelProfile: "dev", Role: domain.ConversationRoleAssistant, Content: "",
			CreatedAt: createdAt.Add(3 * time.Second), PromptTokens: 200, TotalTokens: 200,
			GeneratedFiles: []string{"out/report.md"},
		},
		domain.ConversationTurn{
			ID: "sess-loop:0:2:assistant/message", SessionID: session.ID, TaskID: "task-loop-1",
			AgentID: "agent-1", ModelProfile: "dev", Role: domain.ConversationRoleAssistant,
			Content: "报告写好了", CreatedAt: createdAt.Add(4 * time.Second),
			PromptTokens: 300, TotalTokens: 300, GeneratedFiles: []string{"out/report.md"},
		},
	)
	srv := NewHTTPServer(Config{Sessions: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-loop-1/turns", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /turns status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got []struct {
		ID             string          `json:"id"`
		Role           string          `json:"role"`
		Content        string          `json:"content"`
		PromptTokens   int             `json:"prompt_tokens"`
		TotalTokens    int             `json:"total_tokens"`
		GeneratedFiles []GeneratedFile `json:"generated_files"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("Decode(turns) error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("GET /turns 返回 %d 项，要 2 项（一条 user + 一条 assistant）："+
			"每轮工具多一个空气泡就是 GUI 上肉眼可见的回归：%#v", len(got), got)
	}
	if got[1].ID != "task-loop-1:assistant" || got[1].Content != "报告写好了" {
		t.Errorf("assistant 项 = {id:%q content:%q}, want {task-loop-1:assistant 报告写好了}", got[1].ID, got[1].Content)
	}
	if got[1].PromptTokens != 600 || got[1].TotalTokens != 600 {
		t.Errorf("assistant token = prompt %d / total %d, want 600/600：每轮记增量，折叠时累加成任务用量",
			got[1].PromptTokens, got[1].TotalTokens)
	}
	if len(got[1].GeneratedFiles) != 1 {
		t.Errorf("assistant.generated_files = %#v, want one entry：并集去重，否则 GUI 的文件卡片会重复渲染",
			got[1].GeneratedFiles)
	}
}
