package skill

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

var ErrSkillNotEnableable = errors.New("skill not enableable")

type LifecycleManager struct {
	repository Repository
	audit      port.AuditLog
}

func NewLifecycleManager(repository Repository, audit port.AuditLog) *LifecycleManager {
	return &LifecycleManager{repository: repository, audit: audit}
}

func (m *LifecycleManager) Enable(ctx context.Context, id string, version string) (Skill, error) {
	s, err := m.load(ctx, id, version)
	if err != nil {
		return Skill{}, err
	}
	if s.RiskLevel == RiskCritical || s.Status == StatusQuarantined || s.Status == StatusRejected {
		return Skill{}, ErrSkillNotEnableable
	}
	if scanRepo, ok := m.repository.(ScanFindingRepository); ok {
		findings, err := scanRepo.ListSkillScanFindings(ctx, id)
		if err != nil {
			return Skill{}, fmt.Errorf("list skill scan findings %q: %w", id, err)
		}
		for _, finding := range findings {
			if finding.Severity == SeverityCritical {
				return Skill{}, ErrSkillNotEnableable
			}
		}
	}
	s.Status = StatusEnabled
	if err := m.repository.SaveSkill(ctx, s); err != nil {
		return Skill{}, fmt.Errorf("save enabled skill %q: %w", id, err)
	}
	if err := m.appendAudit(ctx, s, "skill_enabled"); err != nil {
		return Skill{}, err
	}
	return copySkill(s), nil
}

func (m *LifecycleManager) Disable(ctx context.Context, id string, version string) (Skill, error) {
	s, err := m.load(ctx, id, version)
	if err != nil {
		return Skill{}, err
	}
	s.Status = StatusDisabled
	if err := m.repository.SaveSkill(ctx, s); err != nil {
		return Skill{}, fmt.Errorf("save disabled skill %q: %w", id, err)
	}
	if err := m.appendAudit(ctx, s, "skill_disabled"); err != nil {
		return Skill{}, err
	}
	return copySkill(s), nil
}

// Archive transitions a skill to StatusArchived. It is the terminal, reversible
// state the Curator drives idle skills into; it never removes the skill, so the
// content stays recoverable by re-enabling. A missing skill is a loud error.
func (m *LifecycleManager) Archive(ctx context.Context, id string, version string) (Skill, error) {
	s, err := m.load(ctx, id, version)
	if err != nil {
		return Skill{}, err
	}
	s.Status = StatusArchived
	if err := m.repository.SaveSkill(ctx, s); err != nil {
		return Skill{}, fmt.Errorf("save archived skill %q: %w", id, err)
	}
	if err := m.appendAudit(ctx, s, "skill_archived"); err != nil {
		return Skill{}, err
	}
	return copySkill(s), nil
}

func (m *LifecycleManager) load(ctx context.Context, id string, version string) (Skill, error) {
	if m.repository == nil {
		return Skill{}, fmt.Errorf("skill repository is required")
	}
	s, ok, err := m.repository.GetSkill(ctx, id, version)
	if err != nil {
		return Skill{}, fmt.Errorf("get skill %q@%q: %w", id, version, err)
	}
	if !ok {
		return Skill{}, fmt.Errorf("skill %q@%q not found", id, version)
	}
	return s, nil
}

func (m *LifecycleManager) appendAudit(ctx context.Context, skill Skill, action string) error {
	if m.audit == nil {
		return nil
	}
	if err := m.audit.Append(ctx, domain.AuditEvent{
		ID:          skill.ID + ":" + skill.Version + ":" + action,
		RequestID:   skill.ID + ":" + skill.Version,
		SubjectType: "skill",
		SubjectID:   skill.ID,
		Action:      action,
		Hash:        skill.Hash,
		CreatedAt:   time.Now(),
	}); err != nil {
		return fmt.Errorf("append skill audit %q: %w", action, err)
	}
	return nil
}
