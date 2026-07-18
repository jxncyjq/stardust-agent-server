package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/memory"
	"github.com/stardust/legion-agent/internal/skill"
	"github.com/stardust/legion-agent/internal/workflow"
)

func TestSQLiteRepositoryPersistsTaskRunAndAudit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)

	task := domain.Task{
		ID:            "task-1",
		CompanyID:     "company-1",
		AgentID:       "agent-1",
		Status:        domain.TaskPending,
		Input:         "persist this task",
		MaxIterations: 3,
		CreatedAt:     time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC),
	}
	if err := repo.SaveTask(ctx, task); err != nil {
		t.Fatalf("SaveTask(%q) error = %v, want nil", task.ID, err)
	}
	gotTask, ok, err := repo.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask(%q) error = %v, want nil", task.ID, err)
	}
	if !ok {
		t.Fatalf("GetTask(%q) ok = false, want true", task.ID)
	}
	if gotTask.ID != task.ID || gotTask.Status != task.Status || gotTask.Input != task.Input {
		t.Errorf("GetTask(%q) = %#v, want matching task %#v", task.ID, gotTask, task)
	}

	run := domain.TaskRun{
		ID:        "run-1",
		TaskID:    task.ID,
		AgentID:   task.AgentID,
		StartedAt: task.CreatedAt,
		EndedAt:   task.CreatedAt.Add(time.Second),
		Result:    "done",
	}
	if err := repo.SaveTaskRun(ctx, run); err != nil {
		t.Fatalf("SaveTaskRun(%q) error = %v, want nil", run.ID, err)
	}
	runs, err := repo.ListTaskRuns(ctx, task.ID)
	if err != nil {
		t.Fatalf("ListTaskRuns(%q) error = %v, want nil", task.ID, err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID || runs[0].Result != run.Result {
		t.Errorf("ListTaskRuns(%q) = %#v, want run %#v", task.ID, runs, run)
	}

	audit := domain.AuditEvent{
		ID:          "audit-1",
		RequestID:   "request-1",
		SubjectType: "task",
		SubjectID:   task.ID,
		Action:      "task_completed",
		Hash:        "hash-1",
		CreatedAt:   task.CreatedAt,
	}
	if err := repo.AppendAuditEvent(ctx, audit); err != nil {
		t.Fatalf("AppendAuditEvent(%q) error = %v, want nil", audit.ID, err)
	}
	audits, err := repo.ListAuditEvents(ctx)
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v, want nil", err)
	}
	if len(audits) != 1 || audits[0].ID != audit.ID || audits[0].Action != audit.Action {
		t.Errorf("ListAuditEvents() = %#v, want audit %#v", audits, audit)
	}
}

func TestSQLiteRepositoryPersistsLocksAndReapsExpired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)

	ok, err := repo.TryLock(ctx, "task-1", "agent-1", time.Minute)
	if err != nil {
		t.Fatalf("TryLock(task-1) error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("TryLock(task-1) = false, want true")
	}
	ok, err = repo.TryLock(ctx, "task-1", "agent-2", time.Minute)
	if err != nil {
		t.Fatalf("TryLock(task-1) second owner error = %v, want nil", err)
	}
	if ok {
		t.Fatalf("TryLock(task-1) second owner = true, want false")
	}

	ok, err = repo.TryLock(ctx, "task-2", "agent-1", -time.Minute)
	if err != nil {
		t.Fatalf("TryLock(task-2 expired) error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("TryLock(task-2 expired) = false, want true")
	}
	reaped, err := repo.ReapExpired(ctx, time.Now())
	if err != nil {
		t.Fatalf("ReapExpired(now) error = %v, want nil", err)
	}
	if reaped != 1 {
		t.Fatalf("ReapExpired(now) = %d, want 1", reaped)
	}
}

func TestSQLiteRepositoryPersistsAgentSessionsAndConversationTurns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	createdAt := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	session := domain.AgentSession{
		ID:        "session-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Title:     "Cache discussion",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession(%q) error = %v, want nil", session.ID, err)
	}
	turns := []domain.ConversationTurn{
		{
			ID:           "turn-1",
			SessionID:    session.ID,
			TaskID:       "task-1",
			AgentID:      "agent-1",
			ModelProfile: "dev",
			Role:         domain.ConversationRoleUser,
			Content:      "缓存怎么实现",
			CreatedAt:    createdAt.Add(time.Second),
		},
		{
			ID:           "turn-2",
			SessionID:    session.ID,
			TaskID:       "task-1",
			AgentID:      "agent-1",
			ModelProfile: "dev",
			Role:         domain.ConversationRoleAssistant,
			Content:      "缓存使用内存 map",
			CreatedAt:    createdAt.Add(2 * time.Second),
		},
	}
	for _, turn := range turns {
		if err := repo.AppendConversationTurn(ctx, turn); err != nil {
			t.Fatalf("AppendConversationTurn(%q) error = %v, want nil", turn.ID, err)
		}
	}
	gotSession, ok, err := repo.LatestAgentSession(ctx, "company-1", "agent-1")
	if err != nil {
		t.Fatalf("LatestAgentSession() error = %v, want nil", err)
	}
	if !ok || gotSession.ID != session.ID || gotSession.Title != session.Title {
		t.Fatalf("LatestAgentSession() = %#v, %t, want %#v", gotSession, ok, session)
	}
	gotTurns, err := repo.ListConversationTurns(ctx, session.ID, 1)
	if err != nil {
		t.Fatalf("ListConversationTurns(%q) error = %v, want nil", session.ID, err)
	}
	if len(gotTurns) != 1 || gotTurns[0].ID != "turn-2" || gotTurns[0].Content != "缓存使用内存 map" {
		t.Fatalf("ListConversationTurns(%q, 1) = %#v, want latest assistant turn", session.ID, gotTurns)
	}
}

func TestSQLiteRepositoryPersistsAgentMessagesAndMarksRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	createdAt := time.Date(2026, 5, 24, 9, 30, 0, 0, time.UTC)
	messages := []domain.AgentMessage{
		{
			ID:            "msg-1",
			CompanyID:     "company-1",
			TaskID:        "TASK-20260524-001",
			SourceEventID: "evt-taskledger-1",
			ThreadID:      "TASK-20260524-001",
			FromAgentID:   "researcher",
			ToAgentID:     "writer",
			Type:          domain.AgentMessageTypeHandoff,
			Status:        domain.AgentMessageUnread,
			Summary:       "调研完成，请整理说明",
			Artifact:      "docs/research/cache.md",
			CreatedAt:     createdAt,
		},
		{
			ID:          "msg-2",
			CompanyID:   "company-1",
			TaskID:      "TASK-20260524-001",
			ThreadID:    "TASK-20260524-001",
			FromAgentID: "reviewer",
			ToAgentID:   "writer",
			Type:        domain.AgentMessageTypeReview,
			Status:      domain.AgentMessageRead,
			Summary:     "格式没问题",
			CreatedAt:   createdAt.Add(time.Minute),
			ReadAt:      createdAt.Add(2 * time.Minute),
		},
		{
			ID:          "msg-3",
			CompanyID:   "company-1",
			TaskID:      "TASK-20260524-002",
			ThreadID:    "TASK-20260524-002",
			FromAgentID: "researcher",
			ToAgentID:   "reviewer",
			Type:        domain.AgentMessageTypeResult,
			Status:      domain.AgentMessageUnread,
			Summary:     "另一个任务",
			CreatedAt:   createdAt.Add(2 * time.Minute),
		},
	}
	for _, message := range messages {
		if err := repo.SaveAgentMessage(ctx, message); err != nil {
			t.Fatalf("SaveAgentMessage(%q) error = %v, want nil", message.ID, err)
		}
	}
	unread, err := repo.ListAgentMessages(ctx, domain.AgentMessageQuery{
		CompanyID: "company-1",
		ToAgentID: "writer",
		Status:    domain.AgentMessageUnread,
	})
	if err != nil {
		t.Fatalf("ListAgentMessages(unread writer) error = %v, want nil", err)
	}
	if len(unread) != 1 || unread[0].ID != "msg-1" || unread[0].SourceEventID != "evt-taskledger-1" {
		t.Fatalf("ListAgentMessages(unread writer) = %#v, want msg-1 with source event", unread)
	}
	if unread[0].Type != domain.AgentMessageTypeHandoff || unread[0].Artifact != "docs/research/cache.md" {
		t.Fatalf("ListAgentMessages(unread writer)[0] = %#v, want handoff artifact", unread[0])
	}
	readAt := createdAt.Add(3 * time.Minute)
	if err := repo.MarkAgentMessageRead(ctx, "msg-1", readAt); err != nil {
		t.Fatalf("MarkAgentMessageRead(msg-1) error = %v, want nil", err)
	}
	read, err := repo.ListAgentMessages(ctx, domain.AgentMessageQuery{
		CompanyID: "company-1",
		ToAgentID: "writer",
		Status:    domain.AgentMessageRead,
	})
	if err != nil {
		t.Fatalf("ListAgentMessages(read writer) error = %v, want nil", err)
	}
	if len(read) != 2 || read[0].ID != "msg-1" || read[1].ID != "msg-2" {
		t.Fatalf("ListAgentMessages(read writer) = %#v, want msg-1 then msg-2", read)
	}
	if read[0].ReadAt != readAt {
		t.Fatalf("ListAgentMessages(read writer)[0].ReadAt = %s, want %s", read[0].ReadAt, readAt)
	}
	taskMessages, err := repo.ListAgentMessages(ctx, domain.AgentMessageQuery{
		CompanyID: "company-1",
		TaskID:    "TASK-20260524-001",
	})
	if err != nil {
		t.Fatalf("ListAgentMessages(task) error = %v, want nil", err)
	}
	if len(taskMessages) != 2 || taskMessages[0].ID != "msg-1" || taskMessages[1].ID != "msg-2" {
		t.Fatalf("ListAgentMessages(task) = %#v, want task messages by created_at", taskMessages)
	}
}

func TestSQLiteRepositoryPersistsSkillMetadataAndScanFindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	s := skill.Skill{
		ID:        "go-testing",
		Name:      "Go Testing",
		Source:    skill.SourceRegistry,
		Version:   "1.0.0",
		Path:      "/skills/go-testing/SKILL.md",
		Hash:      "hash-1",
		RiskLevel: skill.RiskSafe,
		Status:    skill.StatusActive,
		Tags:      []string{"go", "test"},
		Summary:   "write Go tests",
		Content:   "full skill content",
	}
	if err := repo.SaveSkill(ctx, s); err != nil {
		t.Fatalf("SaveSkill(%q) error = %v, want nil", s.ID, err)
	}
	got, ok, err := repo.GetSkill(ctx, s.ID, s.Version)
	if err != nil {
		t.Fatalf("GetSkill(%q, %q) error = %v, want nil", s.ID, s.Version, err)
	}
	if !ok {
		t.Fatalf("GetSkill(%q, %q) ok = false, want true", s.ID, s.Version)
	}
	if got.ID != s.ID || got.Hash != s.Hash || len(got.Tags) != 2 || got.Tags[0] != "go" {
		t.Fatalf("GetSkill(%q, %q) = %#v, want %#v", s.ID, s.Version, got, s)
	}
	finding := skill.SkillScanFinding{
		SkillID:  s.ID,
		RuleID:   skill.RuleLicenseMissing,
		Severity: skill.SeverityInfo,
		Message:  "license metadata missing",
		Location: "SKILL.md",
	}
	if err := repo.SaveSkillScanFindings(ctx, s.ID, []skill.SkillScanFinding{finding}); err != nil {
		t.Fatalf("SaveSkillScanFindings(%q) error = %v, want nil", s.ID, err)
	}
	findings, err := repo.ListSkillScanFindings(ctx, s.ID)
	if err != nil {
		t.Fatalf("ListSkillScanFindings(%q) error = %v, want nil", s.ID, err)
	}
	if len(findings) != 1 || findings[0].RuleID != finding.RuleID {
		t.Fatalf("ListSkillScanFindings(%q) = %#v, want %#v", s.ID, findings, []skill.SkillScanFinding{finding})
	}
}

func TestSQLiteRepositoryPersistsCapabilityAssets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	gene := memory.Gene{
		ID:           "gene-go-tests",
		Version:      "1.0.0",
		Status:       memory.GeneStatusActive,
		Tags:         []string{"go", "test"},
		Match:        "go test task",
		UseWhen:      "when Go tests fail",
		Plan:         "run focused tests",
		Avoid:        "avoid unrelated edits",
		Constraints:  "keep scope small",
		Validation:   "go test ./...",
		SuccessRate:  0.8,
		SuccessCount: 4,
		FailureCount: 1,
		UpdatedAt:    time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
	}
	if err := repo.SaveGene(ctx, gene); err != nil {
		t.Fatalf("SaveGene(%q) error = %v, want nil", gene.ID, err)
	}
	genes, err := repo.ListGenes(ctx)
	if err != nil {
		t.Fatalf("ListGenes() error = %v, want nil", err)
	}
	if len(genes) != 1 || genes[0].ID != gene.ID || genes[0].Tags[1] != "test" {
		t.Fatalf("ListGenes() = %#v, want gene %#v", genes, gene)
	}
	capsule := memory.Capsule{
		ID:           "capsule-go-tests",
		GeneIDs:      []string{gene.ID},
		Query:        "go test task",
		Tags:         []string{"go", "test"},
		Outcome:      "success",
		SuccessCount: 3,
		Confidence:   0.9,
		CreatedAt:    time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC),
	}
	if err := repo.SaveCapsule(ctx, capsule); err != nil {
		t.Fatalf("SaveCapsule(%q) error = %v, want nil", capsule.ID, err)
	}
	capsules, err := repo.ListCapsules(ctx)
	if err != nil {
		t.Fatalf("ListCapsules() error = %v, want nil", err)
	}
	if len(capsules) != 1 || capsules[0].ID != capsule.ID || capsules[0].GeneIDs[0] != gene.ID {
		t.Fatalf("ListCapsules() = %#v, want capsule %#v", capsules, capsule)
	}
}

func TestSQLiteRepositoryPersistsEvolutionEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	event := evolution.EvolutionEvent{
		EventID:      "event-1",
		CycleID:      "cycle-1",
		Stage:        evolution.StageSignal,
		AgentID:      "agent-1",
		AssetID:      "gene-1",
		EvidenceHash: "hash-1",
		Decision:     evolution.DecisionCandidate,
		CreatedAt:    time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC),
	}
	if err := repo.AppendEvolutionEvent(ctx, event); err != nil {
		t.Fatalf("AppendEvolutionEvent(%q) error = %v, want nil", event.EventID, err)
	}
	events, err := repo.ListEvolutionEvents(ctx, event.CycleID)
	if err != nil {
		t.Fatalf("ListEvolutionEvents(%q) error = %v, want nil", event.CycleID, err)
	}
	if len(events) != 1 || events[0].EventID != event.EventID || events[0].Stage != event.Stage {
		t.Fatalf("ListEvolutionEvents(%q) = %#v, want event %#v", event.CycleID, events, event)
	}
}

func TestSQLiteRepositoryRecoversCrossProcessState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agent.db")
	repo, err := OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) error = %v, want nil", dbPath, err)
	}
	task := domain.Task{
		ID:        "task-recover",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Status:    domain.TaskSuspended,
		Input:     "recover me",
		CreatedAt: time.Date(2026, 5, 12, 13, 0, 0, 0, time.UTC),
	}
	if err := repo.SaveTask(ctx, task); err != nil {
		t.Fatalf("SaveTask(%q) error = %v, want nil", task.ID, err)
	}
	runtimeEvent := domain.RuntimeEvent{
		Type:      "workflow_waiting_event",
		TaskID:    "workflow-recover",
		Message:   "waiting for external.ready",
		CreatedAt: task.CreatedAt.Add(time.Minute),
	}
	if err := repo.AppendRuntimeEvent(ctx, runtimeEvent); err != nil {
		t.Fatalf("AppendRuntimeEvent(%q) error = %v, want nil", runtimeEvent.Type, err)
	}
	audit := domain.AuditEvent{
		ID:          "audit-recover",
		RequestID:   "workflow-recover:workflow",
		SubjectType: "workflow",
		SubjectID:   "workflow-recover",
		Action:      "workflow_waiting_event",
		Hash:        "hash-1",
		CreatedAt:   task.CreatedAt.Add(2 * time.Minute),
	}
	if err := repo.AppendAuditEvent(ctx, audit); err != nil {
		t.Fatalf("AppendAuditEvent(%q) error = %v, want nil", audit.ID, err)
	}
	def := workflow.Definition{
		ID: "workflow-recover",
		Root: workflow.Node{
			ID:        "wait-external",
			Kind:      workflow.NodeWaitEvent,
			EventType: "external.ready",
		},
	}
	result := workflow.Result{
		WorkflowID: def.ID,
		Status:     workflow.StatusWaitingEvent,
		Nodes:      []workflow.NodeResult{{NodeID: "wait-external", Status: workflow.StatusWaitingEvent}},
	}
	if err := repo.SaveWorkflowState(ctx, def, result); err != nil {
		t.Fatalf("SaveWorkflowState(%q) error = %v, want nil", def.ID, err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	reopened, err := OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) reopen error = %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("Close() reopened error = %v, want nil", err)
		}
	})
	gotTask, ok, err := reopened.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask(%q) error = %v, want nil", task.ID, err)
	}
	if !ok || gotTask.Status != domain.TaskSuspended || gotTask.Input != task.Input {
		t.Fatalf("GetTask(%q) = %#v, %t, want recovered suspended task", task.ID, gotTask, ok)
	}
	events, err := reopened.ListRuntimeEvents(ctx)
	if err != nil {
		t.Fatalf("ListRuntimeEvents() error = %v, want nil", err)
	}
	if len(events) != 1 || events[0].Type != runtimeEvent.Type || events[0].TaskID != runtimeEvent.TaskID {
		t.Fatalf("ListRuntimeEvents() = %#v, want runtime event %#v", events, runtimeEvent)
	}
	audits, err := reopened.ListAuditEvents(ctx)
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v, want nil", err)
	}
	if len(audits) != 1 || audits[0].ID != audit.ID || audits[0].Action != audit.Action {
		t.Fatalf("ListAuditEvents() = %#v, want audit %#v", audits, audit)
	}
	waiting, err := reopened.ListWaitingWorkflowStates(ctx)
	if err != nil {
		t.Fatalf("ListWaitingWorkflowStates() error = %v, want nil", err)
	}
	if len(waiting) != 1 || waiting[0].Definition.ID != def.ID || waiting[0].Result.Status != workflow.StatusWaitingEvent {
		t.Fatalf("ListWaitingWorkflowStates() = %#v, want waiting workflow %q", waiting, def.ID)
	}
}

func TestSQLiteRepositoryPing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	if err := repo.Ping(ctx); err != nil {
		t.Fatalf("Ping(open repo) error = %v, want nil", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if err := repo.Ping(ctx); err == nil {
		t.Fatalf("Ping(closed repo) error = nil, want error")
	}
}

func TestSQLiteRepositoryPersistsSessionProjectAndTaskSessionID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	createdAt := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	session := domain.AgentSession{
		ID:        "session-proj-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Project:   "测试项目",
		Title:     "会话1",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession(%q) error = %v, want nil", session.ID, err)
	}

	gotByID, ok, err := repo.GetAgentSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetAgentSession(%q) error = %v, want nil", session.ID, err)
	}
	if !ok || gotByID.Project != "测试项目" || gotByID.Title != "会话1" {
		t.Fatalf("GetAgentSession(%q) = %#v, %t, want project/title preserved", session.ID, gotByID, ok)
	}

	listed, err := repo.ListAgentSessions(ctx, "company-1", "agent-1")
	if err != nil {
		t.Fatalf("ListAgentSessions() error = %v, want nil", err)
	}
	if len(listed) != 1 || listed[0].Project != "测试项目" {
		t.Fatalf("ListAgentSessions() = %#v, want one session with project", listed)
	}

	_, ok, err = repo.GetAgentSession(ctx, "missing-session")
	if err != nil {
		t.Fatalf("GetAgentSession(missing) error = %v, want nil", err)
	}
	if ok {
		t.Fatalf("GetAgentSession(missing) ok = true, want false")
	}

	task := domain.Task{
		ID:        "task-with-session",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		SessionID: session.ID,
		Status:    domain.TaskPending,
		Input:     "你好",
		CreatedAt: createdAt,
	}
	if err := repo.SaveTask(ctx, task); err != nil {
		t.Fatalf("SaveTask(%q) error = %v, want nil", task.ID, err)
	}
	gotTask, ok, err := repo.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask(%q) error = %v, want nil", task.ID, err)
	}
	if !ok || gotTask.SessionID != session.ID {
		t.Fatalf("GetTask(%q) session_id = %q, want %q", task.ID, gotTask.SessionID, session.ID)
	}
}

func TestSQLiteColumnMigrationsAreIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")

	first, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatalf("OpenSQLite() first error = %v, want nil", err)
	}
	// Re-running the column migrations on an already-migrated database must not
	// fail: the duplicate-column case is the documented already-applied signal.
	if err := first.applyColumnMigrations(ctx); err != nil {
		t.Fatalf("applyColumnMigrations() second run error = %v, want nil", err)
	}
	if err := first.migrate(ctx); err != nil {
		t.Fatalf("migrate() second run error = %v, want nil", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() first error = %v, want nil", err)
	}

	// Reopening the same file exercises the migration path against a database
	// that already has the new columns, the real upgrade scenario.
	second, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatalf("OpenSQLite() reopen error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("Close() second error = %v, want nil", err)
		}
	})
	session := domain.AgentSession{
		ID:        "session-after-migrate",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Project:   "迁移后项目",
		Title:     "迁移会话",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := second.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession after reopen error = %v, want nil", err)
	}
	got, ok, err := second.GetAgentSession(ctx, session.ID)
	if err != nil || !ok || got.Project != "迁移后项目" {
		t.Fatalf("GetAgentSession after reopen = %#v, %t, %v, want project persisted", got, ok, err)
	}
}

func TestSQLiteAppendConversationTurnIfAbsentIsExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	createdAt := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	session := domain.AgentSession{
		ID:        "session-once",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Project:   "项目",
		Title:     "once",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession error = %v, want nil", err)
	}
	turn := domain.ConversationTurn{
		ID:        "task-x:assistant",
		SessionID: session.ID,
		TaskID:    "task-x",
		AgentID:   "agent-1",
		Role:      domain.ConversationRoleAssistant,
		Content:   "真实回答",
		CreatedAt: createdAt.Add(time.Second),
	}
	inserted, err := repo.AppendConversationTurnIfAbsent(ctx, turn)
	if err != nil {
		t.Fatalf("AppendConversationTurnIfAbsent() first error = %v, want nil", err)
	}
	if !inserted {
		t.Fatalf("AppendConversationTurnIfAbsent() first inserted = false, want true")
	}
	inserted, err = repo.AppendConversationTurnIfAbsent(ctx, turn)
	if err != nil {
		t.Fatalf("AppendConversationTurnIfAbsent() second error = %v, want nil", err)
	}
	if inserted {
		t.Fatalf("AppendConversationTurnIfAbsent() second inserted = true, want false")
	}
	turns, err := repo.ListConversationTurns(ctx, session.ID, 0)
	if err != nil {
		t.Fatalf("ListConversationTurns() error = %v, want nil", err)
	}
	if len(turns) != 1 {
		t.Fatalf("ListConversationTurns() len = %d, want exactly 1", len(turns))
	}
}

func TestSQLiteRepositoryPersistsSessionArchivedFlag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	createdAt := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	session := domain.AgentSession{
		ID:        "session-archive-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Project:   "归档项目",
		Title:     "归档会话",
		Archived:  true,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, session); err != nil {
		t.Fatalf("SaveAgentSession(%q) error = %v, want nil", session.ID, err)
	}
	got, ok, err := repo.GetAgentSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetAgentSession(%q) error = %v, want nil", session.ID, err)
	}
	if !ok || !got.Archived || got.Project != "归档项目" || got.Title != "归档会话" {
		t.Fatalf("GetAgentSession(%q) = %#v, %t, want archived session with project/title", session.ID, got, ok)
	}

	// Toggling archived back off must round-trip to false, proving the 0/1
	// conversion works in both directions.
	got.Archived = false
	if err := repo.SaveAgentSession(ctx, got); err != nil {
		t.Fatalf("SaveAgentSession(unarchive) error = %v, want nil", err)
	}
	reloaded, ok, err := repo.GetAgentSession(ctx, session.ID)
	if err != nil || !ok {
		t.Fatalf("GetAgentSession(unarchived) = _, %t, %v, want found", ok, err)
	}
	if reloaded.Archived {
		t.Fatalf("GetAgentSession(unarchived).Archived = true, want false")
	}

	// The list endpoint must include archived sessions; archived sessions are
	// filtered client-side, never hidden by the store.
	listed, err := repo.ListAgentSessions(ctx, "company-1", "agent-1")
	if err != nil {
		t.Fatalf("ListAgentSessions() error = %v, want nil", err)
	}
	if len(listed) != 1 {
		t.Fatalf("ListAgentSessions() = %#v, want one session including archived state", listed)
	}
}

func TestSQLiteRepositoryDeleteAgentSessionCascadesTurns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	createdAt := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	target := domain.AgentSession{
		ID:        "session-del-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Project:   "删除项目",
		Title:     "待删会话",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	other := domain.AgentSession{
		ID:        "session-keep-1",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Project:   "保留项目",
		Title:     "保留会话",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	for _, s := range []domain.AgentSession{target, other} {
		if err := repo.SaveAgentSession(ctx, s); err != nil {
			t.Fatalf("SaveAgentSession(%q) error = %v, want nil", s.ID, err)
		}
	}
	turns := []domain.ConversationTurn{
		{ID: "del-turn-1", SessionID: target.ID, TaskID: "task-1", AgentID: "agent-1", Role: domain.ConversationRoleUser, Content: "问题", CreatedAt: createdAt.Add(time.Second)},
		{ID: "del-turn-2", SessionID: target.ID, TaskID: "task-1", AgentID: "agent-1", Role: domain.ConversationRoleAssistant, Content: "回答", CreatedAt: createdAt.Add(2 * time.Second)},
		{ID: "keep-turn-1", SessionID: other.ID, TaskID: "task-2", AgentID: "agent-1", Role: domain.ConversationRoleUser, Content: "保留问题", CreatedAt: createdAt.Add(3 * time.Second)},
	}
	for _, turn := range turns {
		if err := repo.AppendConversationTurn(ctx, turn); err != nil {
			t.Fatalf("AppendConversationTurn(%q) error = %v, want nil", turn.ID, err)
		}
	}

	if err := repo.DeleteAgentSession(ctx, target.ID); err != nil {
		t.Fatalf("DeleteAgentSession(%q) error = %v, want nil", target.ID, err)
	}

	_, ok, err := repo.GetAgentSession(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetAgentSession(deleted) error = %v, want nil", err)
	}
	if ok {
		t.Fatalf("GetAgentSession(deleted) ok = true, want false")
	}
	deletedTurns, err := repo.ListConversationTurns(ctx, target.ID, 0)
	if err != nil {
		t.Fatalf("ListConversationTurns(deleted) error = %v, want nil", err)
	}
	if len(deletedTurns) != 0 {
		t.Fatalf("ListConversationTurns(deleted) = %#v, want empty after cascade delete", deletedTurns)
	}

	// The unrelated session and its turns must be untouched.
	keptTurns, err := repo.ListConversationTurns(ctx, other.ID, 0)
	if err != nil {
		t.Fatalf("ListConversationTurns(kept) error = %v, want nil", err)
	}
	if len(keptTurns) != 1 || keptTurns[0].ID != "keep-turn-1" {
		t.Fatalf("ListConversationTurns(kept) = %#v, want the unrelated turn preserved", keptTurns)
	}
}

func TestSQLiteRepositoryDeleteMissingAgentSessionFailsLoud(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	err := repo.DeleteAgentSession(ctx, "no-such-session")
	if err == nil {
		t.Fatalf("DeleteAgentSession(missing) error = nil, want error")
	}
	if !errors.Is(err, ErrAgentSessionNotFound) {
		t.Fatalf("DeleteAgentSession(missing) error = %v, want ErrAgentSessionNotFound", err)
	}
}

func TestSQLiteSessionSearchDiscoveryScrollBrowse(t *testing.T) {
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)

	if err := repo.SaveAgentSession(ctx, domain.AgentSession{
		ID: "sess-1", CompanyID: "acme", AgentID: "researcher", Title: "token work",
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("SaveAgentSession() error = %v, want nil", err)
	}

	turns := []domain.ConversationTurn{
		{ID: "t1", SessionID: "sess-1", TaskID: "task-1", AgentID: "researcher", Role: domain.ConversationRoleUser, Content: "如何做 token 压缩阈值", CreatedAt: time.Unix(2, 0)},
		{ID: "t2", SessionID: "sess-1", TaskID: "task-1", AgentID: "researcher", Role: domain.ConversationRoleAssistant, Content: "prompt cache 复用稳定前缀", CreatedAt: time.Unix(3, 0)},
		{ID: "t3", SessionID: "sess-1", TaskID: "task-1", AgentID: "researcher", Role: domain.ConversationRoleUser, Content: "delegate subtask summary", CreatedAt: time.Unix(4, 0)},
	}
	for _, turn := range turns {
		if err := repo.AppendConversationTurn(ctx, turn); err != nil {
			t.Fatalf("AppendConversationTurn(%q) error = %v, want nil", turn.ID, err)
		}
	}

	// discovery: FTS5 match returns the relevant turn.
	hits, err := repo.SearchMessages(ctx, "prompt", 10)
	if err != nil {
		t.Fatalf("SearchMessages() error = %v, want nil", err)
	}
	if len(hits) != 1 || hits[0].ID != "t2" {
		t.Fatalf("SearchMessages(prompt) = %+v, want single hit t2", hits)
	}

	// discovery error path: empty query fails loud.
	if _, err := repo.SearchMessages(ctx, "  ", 10); err == nil {
		t.Fatalf("SearchMessages(empty) error = nil, want non-nil")
	}

	// scroll: window around an anchor returns neighbors in order.
	scrolled, err := repo.ScrollMessages(ctx, "sess-1", "t2", 1)
	if err != nil {
		t.Fatalf("ScrollMessages() error = %v, want nil", err)
	}
	if len(scrolled) != 3 || scrolled[0].ID != "t1" || scrolled[2].ID != "t3" {
		t.Fatalf("ScrollMessages(t2, 1) = %+v, want t1..t3", scrolled)
	}

	// scroll error path: unknown anchor fails loud.
	if _, err := repo.ScrollMessages(ctx, "sess-1", "missing", 1); err == nil {
		t.Fatalf("ScrollMessages(missing) error = nil, want non-nil")
	}

	// browse: recent sessions surface.
	sessions, err := repo.BrowseSessions(ctx, 10)
	if err != nil {
		t.Fatalf("BrowseSessions() error = %v, want nil", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "sess-1" {
		t.Fatalf("BrowseSessions() = %+v, want sess-1", sessions)
	}
}

func TestSQLiteBackfillConversationTurnsFTS(t *testing.T) {
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)

	turn := domain.ConversationTurn{
		ID: "t1", SessionID: "s1", TaskID: "task-1", AgentID: "a1",
		Role: domain.ConversationRoleUser, Content: "prompt cache 稳定前缀", CreatedAt: time.Unix(2, 0),
	}
	if err := repo.AppendConversationTurn(ctx, turn); err != nil {
		t.Fatalf("AppendConversationTurn() error = %v, want nil", err)
	}
	// Simulate a pre-v4 row: drop it from the FTS index but keep the source row.
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM conversation_turns_fts WHERE turn_id = ?`, "t1"); err != nil {
		t.Fatalf("delete fts row error = %v, want nil", err)
	}
	if hits, err := repo.SearchMessages(ctx, "prompt", 10); err != nil || len(hits) != 0 {
		t.Fatalf("SearchMessages before backfill = %v (err %v), want no hits", hits, err)
	}

	added, err := repo.BackfillConversationTurnsFTS(ctx)
	if err != nil {
		t.Fatalf("BackfillConversationTurnsFTS() error = %v, want nil", err)
	}
	if added != 1 {
		t.Fatalf("BackfillConversationTurnsFTS() added = %d, want 1", added)
	}
	hits, err := repo.SearchMessages(ctx, "prompt", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != "t1" {
		t.Fatalf("SearchMessages after backfill = %v (err %v), want t1", hits, err)
	}
	// Idempotent: a second backfill adds nothing.
	again, err := repo.BackfillConversationTurnsFTS(ctx)
	if err != nil || again != 0 {
		t.Fatalf("second BackfillConversationTurnsFTS() = %d (err %v), want 0", again, err)
	}
}

func TestAgentSessionModeRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	createdAt := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	sess := domain.AgentSession{
		ID:        "sess-mode-1",
		CompanyID: "c1",
		AgentID:   "a1",
		Mode:      domain.ModeManual,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, sess); err != nil {
		t.Fatalf("SaveAgentSession: %v", err)
	}
	got, ok, err := repo.GetAgentSession(ctx, "sess-mode-1")
	if err != nil || !ok {
		t.Fatalf("GetAgentSession ok=%v err=%v", ok, err)
	}
	if got.Mode != domain.ModeManual {
		t.Errorf("Mode = %q, want %q", got.Mode, domain.ModeManual)
	}
}

func TestAgentSessionModeDefaultsAutoWhenEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	createdAt := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	sess := domain.AgentSession{
		ID:        "sess-legacy",
		CompanyID: "c1",
		AgentID:   "a1",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := repo.SaveAgentSession(ctx, sess); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, _, err := repo.GetAgentSession(ctx, "sess-legacy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Mode != domain.ModeAuto {
		t.Errorf("empty Mode read back = %q, want %q", got.Mode, domain.ModeAuto)
	}
}

func TestSQLiteListSkillsRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)

	for _, s := range []skill.Skill{
		{ID: "b", Name: "beta", Source: skill.SourceWorkspace, Version: "1", RiskLevel: skill.RiskSafe, Status: skill.StatusEnabled, Tags: []string{"x"}},
		{ID: "a", Name: "alpha", Source: skill.SourceRegistry, Version: "1", RiskLevel: skill.RiskSafe, Status: skill.StatusDisabled, Tags: []string{"y"}},
	} {
		if err := repo.SaveSkill(ctx, s); err != nil {
			t.Fatalf("SaveSkill(%q) error = %v, want nil", s.ID, err)
		}
	}

	skills, err := repo.ListSkills(ctx)
	if err != nil {
		t.Fatalf("ListSkills() error = %v, want nil", err)
	}
	if len(skills) != 2 || skills[0].ID != "a" || skills[1].ID != "b" {
		t.Fatalf("ListSkills() = %+v, want a,b in id order", skills)
	}
	if skills[0].Source != skill.SourceRegistry || skills[0].Status != skill.StatusDisabled {
		t.Fatalf("ListSkills()[0] = %+v, want registry/disabled round-trip", skills[0])
	}
}

func openTestSQLiteRepository(t *testing.T) *SQLiteRepository {
	t.Helper()
	repo, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "agent.db"))
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
