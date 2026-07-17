package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Manager provides install, update, and uninstall operations for skills
// that live on the local filesystem.
type Manager interface {
	// Install fetches a skill from source and saves it to the install root.
	// source can be "github:owner/repo", "https://...", or "http://...".
	Install(ctx context.Context, source string) (Skill, error)

	// Update re-fetches a previously installed skill using its stored source URL.
	Update(ctx context.Context, name string) (Skill, error)

	// Uninstall removes a skill directory from the install root.
	Uninstall(ctx context.Context, name string) error
}

// DiskManager manages skills in a local install root directory.
// Each skill lives at <installRoot>/<id>/SKILL.md.
// The original source URL is stored in <installRoot>/<id>/.source so that
// Update can re-fetch without the caller providing the URL again.
type DiskManager struct {
	installRoot string
	scanner     SkillSecurityScanner
}

func NewDiskManager(installRoot string, scanner SkillSecurityScanner) *DiskManager {
	return &DiskManager{installRoot: installRoot, scanner: scanner}
}

// Install fetches SKILL.md content from source, validates it, optionally
// scans it, and writes it to <installRoot>/<id>/SKILL.md.
func (m *DiskManager) Install(ctx context.Context, source string) (Skill, error) {
	rawURL := resolveSourceURL(source)
	content, err := fetchRegistryBytes(ctx, rawURL)
	if err != nil {
		return Skill{}, fmt.Errorf("fetch skill %q: %w", source, err)
	}

	// Parse front matter to extract the skill ID before writing to disk.
	parsed, err := parseSkillContent(content)
	if err != nil {
		return Skill{}, fmt.Errorf("parse skill from %q: %w", source, err)
	}

	if m.scanner != nil {
		report, err := m.scanner.Scan(ctx, SkillPackage{Skill: parsed, Content: string(content)})
		if err != nil {
			return Skill{}, fmt.Errorf("scan skill %q: %w", parsed.ID, err)
		}
		if report.RiskLevel == RiskCritical {
			return Skill{}, fmt.Errorf("%w: %s", ErrSkillScanBlocked, parsed.ID)
		}
		parsed.RiskLevel = report.RiskLevel
	}

	skillDir := filepath.Join(m.installRoot, parsed.ID)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return Skill{}, fmt.Errorf("create skill dir %q: %w", skillDir, err)
	}
	targetPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(targetPath, content, 0o644); err != nil {
		return Skill{}, fmt.Errorf("write skill %q: %w", targetPath, err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, ".source"), []byte(source), 0o644); err != nil {
		return Skill{}, fmt.Errorf("write skill source %q: %w", skillDir, err)
	}

	installed, err := readSkill(targetPath)
	if err != nil {
		return Skill{}, err
	}
	return installed, nil
}

// Update re-installs a skill using the source URL stored when it was first installed.
func (m *DiskManager) Update(ctx context.Context, name string) (Skill, error) {
	sourceFile := filepath.Join(m.installRoot, name, ".source")
	data, err := os.ReadFile(sourceFile)
	if err != nil {
		return Skill{}, fmt.Errorf("skill %q has no stored source; use 'install' to reinstall: %w", name, err)
	}
	return m.Install(ctx, strings.TrimSpace(string(data)))
}

// Uninstall removes the skill directory for the given skill name/id.
func (m *DiskManager) Uninstall(_ context.Context, name string) error {
	skillDir := filepath.Join(m.installRoot, name)
	info, err := os.Stat(skillDir)
	if err != nil {
		return fmt.Errorf("skill %q not found in %s", name, m.installRoot)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a skill directory", skillDir)
	}
	if err := os.RemoveAll(skillDir); err != nil {
		return fmt.Errorf("remove skill %q: %w", name, err)
	}
	return nil
}

// resolveSourceURL converts shorthand source references to a full HTTPS URL.
//
//	"github:owner/repo"  →  https://raw.githubusercontent.com/owner/repo/main/SKILL.md
//	"https://..."        →  as-is
func resolveSourceURL(source string) string {
	if strings.HasPrefix(source, "github:") {
		repo := strings.TrimPrefix(source, "github:")
		return "https://raw.githubusercontent.com/" + repo + "/main/SKILL.md"
	}
	return source
}

// parseSkillContent writes content to a temporary file and reads it back
// using the existing readSkill parser (which understands the SKILL.md front matter).
func parseSkillContent(content []byte) (Skill, error) {
	dir, err := os.MkdirTemp("", "legion-skill-parse-*")
	if err != nil {
		return Skill{}, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	tmpPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(tmpPath, content, 0o600); err != nil {
		return Skill{}, fmt.Errorf("write temp skill: %w", err)
	}
	return readSkill(tmpPath)
}
