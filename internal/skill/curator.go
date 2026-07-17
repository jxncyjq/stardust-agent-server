package skill

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	defaultStaleAfter   = 30 * 24 * time.Hour
	defaultArchiveAfter = 90 * 24 * time.Hour
)

// UsageRecord is the per-skill usage sidecar the Curator reads. It is kept
// separate from the Skill itself so lifecycle sweeps never rewrite skill content
// just to record that a skill was used. Pinned skills are exempt from the sweep.
type UsageRecord struct {
	LastActivityAt time.Time
	UseCount       int
	Pinned         bool
}

// UsageStore is a concurrency-safe in-memory usage sidecar keyed by skill id.
// It records activity as skills are used and lets operators pin skills so the
// Curator leaves them alone.
type UsageStore struct {
	mu      sync.Mutex
	records map[string]UsageRecord
}

// NewUsageStore returns an empty usage sidecar.
func NewUsageStore() *UsageStore {
	return &UsageStore{records: make(map[string]UsageRecord)}
}

// Touch records that a skill was used at time at, bumping its use count and
// refreshing its last-activity timestamp.
func (s *UsageStore) Touch(id string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[id]
	record.UseCount++
	record.LastActivityAt = at
	s.records[id] = record
}

// Pin marks (or unmarks) a skill as exempt from the Curator sweep.
func (s *UsageStore) Pin(id string, pinned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[id]
	record.Pinned = pinned
	s.records[id] = record
}

// Get returns a skill's usage record and whether one exists.
func (s *UsageStore) Get(id string) (UsageRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	return record, ok
}

// CuratorRepository is the storage the Curator sweeps: it lists skills and saves
// status changes. It is a narrow interface so the Curator stays decoupled from
// the concrete repository.
type CuratorRepository interface {
	ListSkills(ctx context.Context) ([]Skill, error)
	SaveSkill(ctx context.Context, skill Skill) error
}

// SkillConsolidator is the optional, LLM-backed pass that merges semantically
// overlapping skills. It is invoked by Sweep only when consolidation is enabled,
// so the deterministic zero-token aging path never depends on it.
type SkillConsolidator interface {
	Consolidate(ctx context.Context, skills []Skill) error
}

// CuratorConfig configures a lifecycle sweep. StaleAfter/ArchiveAfter default to
// 30/90 days. Consolidate is an explicitly optional LLM merge pass, off by
// default; when false the sweep is fully deterministic and spends zero tokens.
// When Consolidate is true a Consolidator MUST be supplied — enabling the pass
// without a collaborator is a configuration error reported loudly, never a silent
// no-op.
type CuratorConfig struct {
	Repository   CuratorRepository
	Usage        *UsageStore
	StaleAfter   time.Duration
	ArchiveAfter time.Duration
	Consolidate  bool
	Consolidator SkillConsolidator
}

// Curator ages idle, agent-authored skills through stale into archived on a
// deterministic, zero-token schedule. It never touches registry (bundled/hub)
// skills, never touches pinned skills, and never deletes anything.
type Curator struct {
	repo         CuratorRepository
	usage        *UsageStore
	staleAfter   time.Duration
	archiveAfter time.Duration
	consolidate  bool
	consolidator SkillConsolidator
}

// SweepReport summarizes one sweep for observability.
type SweepReport struct {
	Scanned      int
	MarkedStale  int
	Archived     int
	Skipped      int
	Consolidated bool
}

// NewCurator validates config and builds a Curator. A repository and usage store
// are required; missing thresholds fall back to defaults.
func NewCurator(cfg CuratorConfig) (*Curator, error) {
	if cfg.Repository == nil {
		return nil, fmt.Errorf("curator: repository is required")
	}
	if cfg.Usage == nil {
		return nil, fmt.Errorf("curator: usage store is required")
	}
	staleAfter := cfg.StaleAfter
	if staleAfter <= 0 {
		staleAfter = defaultStaleAfter
	}
	archiveAfter := cfg.ArchiveAfter
	if archiveAfter <= 0 {
		archiveAfter = defaultArchiveAfter
	}
	if archiveAfter < staleAfter {
		return nil, fmt.Errorf("curator: archive_after %s must be >= stale_after %s", archiveAfter, staleAfter)
	}
	if cfg.Consolidate && cfg.Consolidator == nil {
		return nil, fmt.Errorf("curator: consolidate enabled but no consolidator provided")
	}
	return &Curator{
		repo:         cfg.Repository,
		usage:        cfg.Usage,
		staleAfter:   staleAfter,
		archiveAfter: archiveAfter,
		consolidate:  cfg.Consolidate,
		consolidator: cfg.Consolidator,
	}, nil
}

// Sweep ages idle skills at time now. It is deterministic and idempotent: skills
// are processed in id order, only a genuine status change is persisted, and a
// second sweep at the same instant makes no further change. Only workspace
// (agent-authored) skills with a usage record and no pin are eligible; registry
// skills, pinned skills, and skills with no usage history are left untouched.
// Nothing is ever deleted. Any repository error fails loud.
func (c *Curator) Sweep(ctx context.Context, now time.Time) (SweepReport, error) {
	if err := ctx.Err(); err != nil {
		return SweepReport{}, err
	}
	skills, err := c.repo.ListSkills(ctx)
	if err != nil {
		return SweepReport{}, fmt.Errorf("curator sweep: list skills: %w", err)
	}
	sort.Slice(skills, func(i, j int) bool {
		if skills[i].ID != skills[j].ID {
			return skills[i].ID < skills[j].ID
		}
		return skills[i].Version < skills[j].Version
	})

	var report SweepReport
	for _, s := range skills {
		report.Scanned++
		target, ok := c.targetStatus(s, now)
		if !ok || target == s.Status {
			report.Skipped++
			continue
		}
		s.Status = target
		if err := c.repo.SaveSkill(ctx, s); err != nil {
			return SweepReport{}, fmt.Errorf("curator sweep: save skill %q: %w", s.ID, err)
		}
		switch target {
		case StatusStale:
			report.MarkedStale++
		case StatusArchived:
			report.Archived++
		}
	}
	// Optional consolidation pass. It runs only when explicitly enabled, after the
	// deterministic aging so it sees post-sweep statuses. A consolidator is
	// guaranteed non-nil here (NewCurator rejects Consolidate without one), and its
	// failure fails the sweep loud rather than being swallowed.
	if c.consolidate {
		if err := c.consolidator.Consolidate(ctx, skills); err != nil {
			return SweepReport{}, fmt.Errorf("curator sweep: consolidate: %w", err)
		}
		report.Consolidated = true
	}
	return report, nil
}

// targetStatus computes the status a skill should have given its idle time, or
// ok=false when the skill is not eligible for aging (registry-sourced, pinned,
// already archived, or lacking usage history).
func (c *Curator) targetStatus(s Skill, now time.Time) (Status, bool) {
	if s.Source != SourceWorkspace {
		return "", false
	}
	if s.Status == StatusArchived {
		return "", false
	}
	record, ok := c.usage.Get(s.ID)
	if !ok || record.Pinned || record.LastActivityAt.IsZero() {
		return "", false
	}
	idle := now.Sub(record.LastActivityAt)
	switch {
	case idle >= c.archiveAfter:
		return StatusArchived, true
	case idle >= c.staleAfter:
		return StatusStale, true
	default:
		return "", false
	}
}
