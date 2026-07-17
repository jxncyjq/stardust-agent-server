package skill

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// UsageRecorder records that a skill was used (selected into a task's context),
// feeding the Curator's idle-aging sweep. It is satisfied by *UsageStore.
type UsageRecorder interface {
	Touch(id string, at time.Time)
}

type RiskLevel string

const (
	RiskSafe     RiskLevel = "safe"
	RiskWarning  RiskLevel = "warning"
	RiskCritical RiskLevel = "critical"
)

type Status string

const (
	StatusCandidate   Status = "candidate"
	StatusQuarantined Status = "quarantined"
	StatusEnabled     Status = "enabled"
	StatusDisabled    Status = "disabled"
	StatusRejected    Status = "rejected"
	StatusActive      Status = StatusEnabled
	StatusFrozen      Status = "frozen"
	// StatusStale marks a skill idle past the stale threshold; StatusArchived
	// marks one idle past the archive threshold. Both are set by the Curator
	// sweep and are reversible (re-enabling clears them); neither deletes the
	// skill.
	StatusStale    Status = "stale"
	StatusArchived Status = "archived"
)

type Source string

const (
	SourceWorkspace Source = "workspace"
	SourceRegistry  Source = "registry"
)

type Config struct {
	Roots   []string
	Scanner SkillSecurityScanner
}

type Skill struct {
	ID        string
	Name      string
	Source    Source
	Version   string
	Path      string
	Hash      string
	RiskLevel RiskLevel
	Status    Status
	Tags      []string
	Summary   string
	Content   string
}

type Injection struct {
	TaskID string
	Skill  Skill
	Rank   int
	Reason string
}

type System struct {
	roots   []string
	scanner SkillSecurityScanner
	usage   UsageRecorder
	now     func() time.Time
}

func NewSystem(cfg Config) *System {
	return &System{
		roots:   append([]string(nil), cfg.Roots...),
		scanner: cfg.Scanner,
	}
}

// WithUsage attaches a usage recorder so SelectForTask marks each selected skill
// as active, feeding the Curator's idle sweep. now supplies the timestamp
// (defaults to time.Now when nil). recorder is optional: with none attached,
// selection records nothing. Returns the system for chaining.
func (s *System) WithUsage(recorder UsageRecorder, now func() time.Time) *System {
	s.usage = recorder
	if now == nil {
		now = time.Now
	}
	s.now = now
	return s
}

func (s *System) Load(ctx context.Context) ([]Skill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	byKey := make(map[string]Skill)
	for _, root := range s.roots {
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || entry.Name() != "SKILL.md" {
				return nil
			}
			skill, err := readSkill(path)
			if err != nil {
				return err
			}
			key := skill.ID + "@" + skill.Version
			if _, ok := byKey[key]; ok {
				return nil
			}
			byKey[key] = skill
			return nil
		}); err != nil {
			return nil, fmt.Errorf("scan skills in %q: %w", root, err)
		}
	}
	skills := make([]Skill, 0, len(byKey))
	for _, skill := range byKey {
		skills = append(skills, skill)
	}
	sort.Slice(skills, func(i, j int) bool {
		if skills[i].ID == skills[j].ID {
			return skills[i].Version < skills[j].Version
		}
		return skills[i].ID < skills[j].ID
	})
	return copySkills(skills), nil
}

func (s *System) SelectForTask(ctx context.Context, task domain.Task, maxSkills int) ([]Injection, error) {
	if maxSkills <= 0 {
		return nil, nil
	}
	skills, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(task.Input)
	type candidate struct {
		skill Skill
		score int
	}
	var candidates []candidate
	for _, skill := range skills {
		if skill.RiskLevel == RiskCritical || !isInjectableStatus(skill.Status) {
			continue
		}
		if s.scanner != nil {
			report, err := s.scanner.Scan(ctx, SkillPackage{
				Skill:   skill,
				Content: skill.Content,
			})
			if err != nil {
				return nil, err
			}
			if report.RiskLevel == RiskCritical {
				continue
			}
		}
		score := matchScore(query, skill)
		if score == 0 {
			continue
		}
		candidates = append(candidates, candidate{skill: skill, score: score})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].skill.ID < candidates[j].skill.ID
		}
		return candidates[i].score > candidates[j].score
	})
	if len(candidates) > maxSkills {
		candidates = candidates[:maxSkills]
	}
	injections := make([]Injection, 0, len(candidates))
	for idx, candidate := range candidates {
		injections = append(injections, Injection{
			TaskID: task.ID,
			Skill:  copySkill(candidate.skill),
			Rank:   idx + 1,
			Reason: "matched task input",
		})
	}
	if s.usage != nil {
		at := s.now
		if at == nil {
			at = time.Now
		}
		stamp := at()
		for _, injection := range injections {
			s.usage.Touch(injection.Skill.ID, stamp)
		}
	}
	return injections, nil
}

func readSkill(path string) (Skill, error) {
	file, err := os.Open(path)
	if err != nil {
		return Skill{}, fmt.Errorf("open skill %q: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	metadata := make(map[string]string)
	inFrontMatter := false
	seenFrontMatter := false
	var body []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "---" {
			if !seenFrontMatter {
				inFrontMatter = true
				seenFrontMatter = true
				continue
			}
			if inFrontMatter {
				inFrontMatter = false
				continue
			}
		}
		if inFrontMatter {
			key, value, ok := strings.Cut(line, ":")
			if ok {
				metadata[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
			continue
		}
		body = append(body, line)
	}
	if err := scanner.Err(); err != nil {
		return Skill{}, fmt.Errorf("read skill %q: %w", path, err)
	}
	skill := Skill{
		ID:        metadata["id"],
		Name:      metadata["name"],
		Source:    Source(metadata["source"]),
		Version:   metadata["version"],
		Path:      path,
		RiskLevel: RiskLevel(metadata["risk_level"]),
		Status:    normalizeStatus(Status(metadata["status"])),
		Tags:      parseTags(metadata["tags"]),
		Summary:   strings.TrimSpace(strings.Join(body, "\n")),
		Content:   strings.TrimSpace(strings.Join(body, "\n")),
	}
	if skill.ID == "" {
		skill.ID = filepath.Base(filepath.Dir(path))
	}
	if skill.Name == "" {
		skill.Name = skill.ID
	}
	if skill.Version == "" {
		skill.Version = "0.0.0"
	}
	if skill.Source == "" {
		skill.Source = SourceWorkspace
	}
	if skill.RiskLevel == "" {
		skill.RiskLevel = RiskSafe
	}
	if skill.Status == "" {
		skill.Status = StatusEnabled
	}
	return skill, nil
}

func normalizeStatus(status Status) Status {
	switch status {
	case "", "active":
		return StatusEnabled
	default:
		return status
	}
}

func isInjectableStatus(status Status) bool {
	return normalizeStatus(status) == StatusEnabled
}

func IsInjectable(s Skill) bool {
	return s.RiskLevel != RiskCritical && isInjectableStatus(s.Status)
}

func parseTags(value string) []string {
	var tags []string
	for _, tag := range strings.Split(value, ",") {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		tags = append(tags, tag)
	}
	return tags
}

func matchScore(query string, skill Skill) int {
	var score int
	if strings.Contains(query, strings.ToLower(skill.ID)) {
		score += 2
	}
	if strings.Contains(query, strings.ToLower(skill.Name)) {
		score += 2
	}
	for _, tag := range skill.Tags {
		if strings.Contains(query, tag) {
			score++
		}
	}
	return score
}

func copySkills(skills []Skill) []Skill {
	out := make([]Skill, len(skills))
	for idx, skill := range skills {
		out[idx] = copySkill(skill)
	}
	return out
}

func copySkill(skill Skill) Skill {
	skill.Tags = append([]string(nil), skill.Tags...)
	return skill
}
