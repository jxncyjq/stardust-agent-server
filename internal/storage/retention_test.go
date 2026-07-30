package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/quality"
)

func TestRetentionPlanDryRunDoesNotDeleteRecentQualityHistory(t *testing.T) {
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	if err := repo.AppendQualityEvalRun(ctx, quality.EvalRunRecord{
		ID:        "eval-new",
		AgentID:   "agent-1",
		TaskID:    "task-1",
		Component: "planner",
		Status:    quality.EvalNormal,
		Score:     1,
		CreatedAt: now.Add(-24 * time.Hour),
	}); err != nil {
		t.Fatalf("AppendQualityEvalRun(%q) error = %v, want nil", "eval-new", err)
	}

	plan, err := repo.PlanRetention(ctx, RetentionPolicy{
		Now:                  now,
		QualityHistoryMaxAge: 7 * 24 * time.Hour,
		DryRun:               true,
	})
	if err != nil {
		t.Fatalf("PlanRetention() error = %v, want nil", err)
	}
	if plan.QualityHistoryDeleted != 0 {
		t.Fatalf("PlanRetention().QualityHistoryDeleted = %d, want 0", plan.QualityHistoryDeleted)
	}
	records, err := repo.ListQualityEvalRuns(ctx, quality.TrendQuery{})
	if err != nil {
		t.Fatalf("ListQualityEvalRuns() error = %v, want nil", err)
	}
	if len(records) != 1 {
		t.Fatalf("ListQualityEvalRuns() len = %d, want 1", len(records))
	}
}

func TestRetentionApplyDeletesExpiredQualityHistoryAndWritesAudit(t *testing.T) {
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	for _, record := range []quality.EvalRunRecord{
		{
			ID:        "eval-old",
			AgentID:   "agent-1",
			TaskID:    "task-old",
			Component: "planner",
			Status:    quality.EvalComponentDegraded,
			Score:     0.2,
			CreatedAt: now.Add(-30 * 24 * time.Hour),
		},
		{
			ID:        "eval-new",
			AgentID:   "agent-1",
			TaskID:    "task-new",
			Component: "planner",
			Status:    quality.EvalNormal,
			Score:     1,
			CreatedAt: now.Add(-24 * time.Hour),
		},
	} {
		if err := repo.AppendQualityEvalRun(ctx, record); err != nil {
			t.Fatalf("AppendQualityEvalRun(%q) error = %v, want nil", record.ID, err)
		}
	}

	plan, err := repo.ApplyRetention(ctx, RetentionPolicy{
		Now:                  now,
		QualityHistoryMaxAge: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("ApplyRetention() error = %v, want nil", err)
	}
	if plan.QualityHistoryDeleted != 1 {
		t.Fatalf("ApplyRetention().QualityHistoryDeleted = %d, want 1", plan.QualityHistoryDeleted)
	}
	records, err := repo.ListQualityEvalRuns(ctx, quality.TrendQuery{})
	if err != nil {
		t.Fatalf("ListQualityEvalRuns() error = %v, want nil", err)
	}
	if len(records) != 1 || records[0].ID != "eval-new" {
		t.Fatalf("ListQualityEvalRuns() = %#v, want only eval-new", records)
	}
	audits, err := repo.ListAuditEvents(ctx)
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v, want nil", err)
	}
	if len(audits) != 1 {
		t.Fatalf("ListAuditEvents() len = %d, want 1", len(audits))
	}
	if audits[0].Action != "storage.retention.apply" {
		t.Fatalf("ListAuditEvents()[0].Action = %q, want %q", audits[0].Action, "storage.retention.apply")
	}
}

func TestRetentionPurgesEpisodicMemory(t *testing.T) {
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	old := domain.MemoryEntry{ID: "old", AgentID: "a", TaskID: "t", Content: "旧记忆", CreatedAt: now.Add(-40 * 24 * time.Hour)}
	fresh := domain.MemoryEntry{ID: "new", AgentID: "a", TaskID: "t", Content: "新记忆", CreatedAt: now.Add(-time.Hour)}
	if err := repo.AddEpisodicMemory(ctx, old); err != nil {
		t.Fatalf("AddEpisodicMemory(old) error = %v, want nil", err)
	}
	if err := repo.AddEpisodicMemory(ctx, fresh); err != nil {
		t.Fatalf("AddEpisodicMemory(fresh) error = %v, want nil", err)
	}

	plan, err := repo.ApplyRetention(ctx, RetentionPolicy{
		Now:            now,
		EpisodicMaxAge: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("ApplyRetention() error = %v, want nil", err)
	}
	if plan.EpisodicDeleted != 1 {
		t.Fatalf("ApplyRetention().EpisodicDeleted = %d, want 1", plan.EpisodicDeleted)
	}

	hits, err := repo.SearchEpisodicMemory(ctx, "记忆", 10)
	if err != nil {
		t.Fatalf("SearchEpisodicMemory() error = %v, want nil", err)
	}
	if len(hits) != 1 || hits[0].ID != "new" {
		t.Fatalf("SearchEpisodicMemory() = %+v, want only fresh entry", hits)
	}

	audits, err := repo.ListAuditEvents(ctx)
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v, want nil", err)
	}
	if len(audits) != 1 || audits[0].Action != "storage.retention.apply" {
		t.Fatalf("ListAuditEvents() = %+v, want single storage.retention.apply", audits)
	}
}
