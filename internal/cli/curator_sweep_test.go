package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/skill"
)

func TestSkillCuratorSweepJobAgesIdleSkill(t *testing.T) {
	ctx := context.Background()
	repo := skill.NewMemoryRepository()
	now := time.Unix(1_000_000, 0)
	if err := repo.SaveSkill(ctx, skill.Skill{
		ID: "s1", Version: "1", Source: skill.SourceWorkspace, Status: skill.StatusEnabled,
	}); err != nil {
		t.Fatalf("SaveSkill() error = %v, want nil", err)
	}
	usage := skill.NewUsageStore()
	usage.Touch("s1", now.Add(-40*24*time.Hour)) // idle 40 days > 30d stale threshold

	curator, err := skill.NewCurator(skill.CuratorConfig{Repository: repo, Usage: usage})
	if err != nil {
		t.Fatalf("NewCurator() error = %v, want nil", err)
	}
	job := newSkillCuratorSweepJob(curator, func() time.Time { return now })
	if err := job(ctx); err != nil {
		t.Fatalf("curator sweep job error = %v, want nil", err)
	}
	got, ok, err := repo.GetSkill(ctx, "s1", "1")
	if err != nil || !ok {
		t.Fatalf("GetSkill() ok=%v err=%v, want found", ok, err)
	}
	if got.Status != skill.StatusStale {
		t.Fatalf("swept skill status = %s, want stale", got.Status)
	}
}

func TestSkillCuratorSweepJobNilCuratorNoOp(t *testing.T) {
	job := newSkillCuratorSweepJob(nil, time.Now)
	if err := job(context.Background()); err != nil {
		t.Fatalf("nil-curator job error = %v, want nil", err)
	}
}

type failingCuratorRepo struct{}

func (failingCuratorRepo) ListSkills(context.Context) ([]skill.Skill, error) {
	return nil, errors.New("list boom")
}
func (failingCuratorRepo) SaveSkill(context.Context, skill.Skill) error { return nil }

func TestSkillCuratorSweepJobPropagatesError(t *testing.T) {
	curator, err := skill.NewCurator(skill.CuratorConfig{Repository: failingCuratorRepo{}, Usage: skill.NewUsageStore()})
	if err != nil {
		t.Fatalf("NewCurator() error = %v, want nil", err)
	}
	job := newSkillCuratorSweepJob(curator, time.Now)
	if err := job(context.Background()); err == nil {
		t.Fatalf("curator sweep job error = nil, want propagated list error")
	}
}
