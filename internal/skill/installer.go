package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

var (
	ErrInvalidManifest   = errors.New("invalid skill manifest")
	ErrSkillHashMismatch = errors.New("skill hash mismatch")
	ErrSkillScanBlocked  = errors.New("skill scan blocked")
)

type InstallerConfig struct {
	InstallRoot string
	Scanner     SkillSecurityScanner
	Repository  Repository
	Audit       port.AuditLog
}

type Manifest struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Source      string   `json:"source"`
	SHA256      string   `json:"sha256"`
	ContentPath string   `json:"content_path"`
	Tags        []string `json:"tags"`
}

type Installer struct {
	installRoot string
	scanner     SkillSecurityScanner
	repository  Repository
	audit       port.AuditLog
}

type Repository interface {
	SaveSkill(ctx context.Context, skill Skill) error
	GetSkill(ctx context.Context, id string, version string) (Skill, bool, error)
}

type ScanFindingRepository interface {
	SaveSkillScanFindings(ctx context.Context, skillID string, findings []SkillScanFinding) error
	ListSkillScanFindings(ctx context.Context, skillID string) ([]SkillScanFinding, error)
}

type MemoryRepository struct {
	mu       sync.RWMutex
	skills   map[string]Skill
	findings map[string][]SkillScanFinding
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		skills:   make(map[string]Skill),
		findings: make(map[string][]SkillScanFinding),
	}
}

func (r *MemoryRepository) SaveSkill(ctx context.Context, skill Skill) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skills[skillKey(skill.ID, skill.Version)] = copySkill(skill)
	return nil
}

func (r *MemoryRepository) GetSkill(ctx context.Context, id string, version string) (Skill, bool, error) {
	if err := ctx.Err(); err != nil {
		return Skill{}, false, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	skill, ok := r.skills[skillKey(id, version)]
	if !ok {
		return Skill{}, false, nil
	}
	return copySkill(skill), true, nil
}

// ListSkills returns every stored skill, satisfying CuratorRepository so an
// in-memory repository can drive the Curator sweep in tests.
func (r *MemoryRepository) ListSkills(ctx context.Context) ([]Skill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	skills := make([]Skill, 0, len(r.skills))
	for _, s := range r.skills {
		skills = append(skills, copySkill(s))
	}
	return skills, nil
}

func (r *MemoryRepository) SaveSkillScanFindings(ctx context.Context, skillID string, findings []SkillScanFinding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	copied := make([]SkillScanFinding, len(findings))
	copy(copied, findings)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.findings[skillID] = copied
	return nil
}

func (r *MemoryRepository) ListSkillScanFindings(ctx context.Context, skillID string) ([]SkillScanFinding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	findings := r.findings[skillID]
	copied := make([]SkillScanFinding, len(findings))
	copy(copied, findings)
	return copied, nil
}

func NewInstaller(cfg InstallerConfig) *Installer {
	return &Installer{
		installRoot: cfg.InstallRoot,
		scanner:     cfg.Scanner,
		repository:  cfg.Repository,
		audit:       cfg.Audit,
	}
}

func (i *Installer) InstallFromManifest(ctx context.Context, manifestPath string) (Skill, error) {
	if err := ctx.Err(); err != nil {
		return Skill{}, err
	}
	manifest, err := readManifest(manifestPath)
	if err != nil {
		return Skill{}, err
	}
	if err := validateManifest(manifest); err != nil {
		return Skill{}, err
	}
	contentPath := manifest.ContentPath
	if !filepath.IsAbs(contentPath) {
		contentPath = filepath.Join(filepath.Dir(manifestPath), filepath.FromSlash(contentPath))
	}
	content, err := os.ReadFile(contentPath)
	if err != nil {
		return Skill{}, fmt.Errorf("read skill package %q: %w", contentPath, err)
	}
	actualHash := hashBytes(content)
	if !strings.EqualFold(actualHash, manifest.SHA256) {
		return Skill{}, fmt.Errorf("%w: manifest=%s actual=%s", ErrSkillHashMismatch, manifest.SHA256, actualHash)
	}

	skill := Skill{
		ID:        manifest.ID,
		Name:      manifest.Name,
		Source:    SourceRegistry,
		Version:   manifest.Version,
		Hash:      actualHash,
		RiskLevel: RiskSafe,
		Status:    StatusCandidate,
		Tags:      append([]string(nil), manifest.Tags...),
		Content:   strings.TrimSpace(string(content)),
		Summary:   strings.TrimSpace(string(content)),
	}
	if i.scanner != nil {
		report, err := i.scanner.Scan(ctx, SkillPackage{
			Skill:   skill,
			Content: string(content),
		})
		if err != nil {
			return Skill{}, fmt.Errorf("scan skill %q: %w", manifest.ID, err)
		}
		if scanRepo, ok := i.repository.(ScanFindingRepository); ok {
			if err := scanRepo.SaveSkillScanFindings(ctx, manifest.ID, report.Findings); err != nil {
				return Skill{}, fmt.Errorf("save skill scan findings %q: %w", manifest.ID, err)
			}
		}
		if report.RiskLevel == RiskCritical {
			skill.Status = StatusQuarantined
			skill.RiskLevel = report.RiskLevel
			if i.repository != nil {
				if err := i.repository.SaveSkill(ctx, skill); err != nil {
					return Skill{}, fmt.Errorf("save quarantined skill metadata %q: %w", skill.ID, err)
				}
			}
			if err := i.appendAudit(ctx, skill, "skill_quarantined"); err != nil {
				return Skill{}, err
			}
			return Skill{}, fmt.Errorf("%w: %s", ErrSkillScanBlocked, manifest.ID)
		}
		skill.RiskLevel = report.RiskLevel
	}

	targetPath := filepath.Join(i.installRoot, manifest.ID, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return Skill{}, fmt.Errorf("create skill install dir %q: %w", filepath.Dir(targetPath), err)
	}
	if err := os.WriteFile(targetPath, content, 0o644); err != nil {
		return Skill{}, fmt.Errorf("write skill %q: %w", targetPath, err)
	}

	installed, err := readSkill(targetPath)
	if err != nil {
		return Skill{}, err
	}
	installed.Source = SourceRegistry
	installed.Hash = actualHash
	installed.RiskLevel = skill.RiskLevel
	installed.Status = normalizeStatus(installed.Status)
	if len(installed.Tags) == 0 {
		installed.Tags = append([]string(nil), manifest.Tags...)
	}
	if i.repository != nil {
		if err := i.repository.SaveSkill(ctx, installed); err != nil {
			return Skill{}, fmt.Errorf("save skill metadata %q: %w", installed.ID, err)
		}
	}
	if err := i.appendAudit(ctx, installed, "skill_installed"); err != nil {
		return Skill{}, err
	}
	return copySkill(installed), nil
}

func (i *Installer) appendAudit(ctx context.Context, skill Skill, action string) error {
	if i.audit == nil {
		return nil
	}
	if err := i.audit.Append(ctx, domain.AuditEvent{
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

func readManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest %q: %w", path, err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	return manifest, nil
}

func validateManifest(manifest Manifest) error {
	switch {
	case manifest.ID == "":
		return fmt.Errorf("%w: id is required", ErrInvalidManifest)
	case manifest.Version == "":
		return fmt.Errorf("%w: version is required", ErrInvalidManifest)
	case manifest.SHA256 == "":
		return fmt.Errorf("%w: sha256 is required", ErrInvalidManifest)
	case manifest.ContentPath == "":
		return fmt.Errorf("%w: content_path is required", ErrInvalidManifest)
	}
	return nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func skillKey(id string, version string) string {
	return id + "@" + version
}
