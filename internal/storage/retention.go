package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

type RetentionPolicy struct {
	Now                  time.Time
	AuditMaxAge          time.Duration
	RuntimeEventMaxAge   time.Duration
	QualityHistoryMaxAge time.Duration
	DryRun               bool
}

type RetentionPlan struct {
	AuditEventsDeleted    int
	RuntimeEventsDeleted  int
	QualityHistoryDeleted int
	DryRun                bool
}

func (r *SQLiteRepository) PlanRetention(ctx context.Context, policy RetentionPolicy) (RetentionPlan, error) {
	return r.retention(ctx, policy, true)
}

func (r *SQLiteRepository) ApplyRetention(ctx context.Context, policy RetentionPolicy) (RetentionPlan, error) {
	return r.retention(ctx, policy, false)
}

func (r *SQLiteRepository) retention(ctx context.Context, policy RetentionPolicy, forceDryRun bool) (RetentionPlan, error) {
	if err := ctx.Err(); err != nil {
		return RetentionPlan{}, err
	}
	if policy.Now.IsZero() {
		policy.Now = time.Now()
	}
	plan := RetentionPlan{DryRun: policy.DryRun || forceDryRun}
	var err error
	if policy.AuditMaxAge > 0 {
		cutoff := policy.Now.Add(-policy.AuditMaxAge)
		plan.AuditEventsDeleted, err = r.countOlderThan(ctx, "audit_events", cutoff)
		if err != nil {
			return RetentionPlan{}, fmt.Errorf("plan audit retention: %w", err)
		}
	}
	if policy.RuntimeEventMaxAge > 0 {
		cutoff := policy.Now.Add(-policy.RuntimeEventMaxAge)
		plan.RuntimeEventsDeleted, err = r.countOlderThan(ctx, "runtime_events", cutoff)
		if err != nil {
			return RetentionPlan{}, fmt.Errorf("plan runtime event retention: %w", err)
		}
	}
	if policy.QualityHistoryMaxAge > 0 {
		cutoff := policy.Now.Add(-policy.QualityHistoryMaxAge)
		plan.QualityHistoryDeleted, err = r.countOlderThan(ctx, "quality_history", cutoff)
		if err != nil {
			return RetentionPlan{}, fmt.Errorf("plan quality history retention: %w", err)
		}
	}
	if plan.DryRun {
		return plan, nil
	}
	if policy.AuditMaxAge > 0 {
		cutoff := policy.Now.Add(-policy.AuditMaxAge)
		if err := r.deleteOlderThan(ctx, "audit_events", cutoff); err != nil {
			return RetentionPlan{}, fmt.Errorf("apply audit retention: %w", err)
		}
	}
	if policy.RuntimeEventMaxAge > 0 {
		cutoff := policy.Now.Add(-policy.RuntimeEventMaxAge)
		if err := r.deleteOlderThan(ctx, "runtime_events", cutoff); err != nil {
			return RetentionPlan{}, fmt.Errorf("apply runtime event retention: %w", err)
		}
	}
	if policy.QualityHistoryMaxAge > 0 {
		cutoff := policy.Now.Add(-policy.QualityHistoryMaxAge)
		if err := r.deleteOlderThan(ctx, "quality_history", cutoff); err != nil {
			return RetentionPlan{}, fmt.Errorf("apply quality history retention: %w", err)
		}
	}
	if err := r.AppendAuditEvent(ctx, domain.AuditEvent{
		ID:          "retention:" + policy.Now.UTC().Format(time.RFC3339Nano),
		RequestID:   "storage.retention",
		SubjectType: "storage",
		SubjectID:   "sqlite",
		Action:      "storage.retention.apply",
		Hash:        retentionPlanHash(plan),
		CreatedAt:   policy.Now,
	}); err != nil {
		return RetentionPlan{}, fmt.Errorf("append retention audit: %w", err)
	}
	return plan, nil
}

func (r *SQLiteRepository) countOlderThan(ctx context.Context, table string, cutoff time.Time) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE created_at < ?`, table), formatTime(cutoff)).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (r *SQLiteRepository) deleteOlderThan(ctx context.Context, table string, cutoff time.Time) error {
	_, err := r.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE created_at < ?`, table), formatTime(cutoff))
	return err
}

func retentionPlanHash(plan RetentionPlan) string {
	return fmt.Sprintf("audit=%d runtime=%d quality=%d", plan.AuditEventsDeleted, plan.RuntimeEventsDeleted, plan.QualityHistoryDeleted)
}
