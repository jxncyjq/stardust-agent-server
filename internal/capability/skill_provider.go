package capability

import (
	"context"
	"fmt"
	"sync"

	"github.com/stardust/legion-agent/internal/skill"
)

// MaxSkillsPerAgent bounds how many skills one agent may expose.
//
// The catalog is rendered into the prompt's cached prefix on every inference,
// so its size is a standing cost. The limit is a declared contract, not a
// suggestion: exceeding it fails loudly, because silently dropping the tail
// would leave those skills listed nowhere and therefore unreachable.
const MaxSkillsPerAgent = 64

// SkillProvider exposes an agent's skills as catalog entries.
//
// skill.Skill has no field of its own for a short, catalog-sized summary:
// Skill.Summary and Skill.Content are both populated from the exact same
// trimmed SKILL.md body (see internal/skill/system.go's readSkill). So the
// one-line entry offered here is derived from the body via summarize(), the
// same first-line-and-truncate rule ToolProvider applies to tool
// descriptions, rather than read off a distinct field.
type SkillProvider struct {
	system *skill.System

	mu     sync.Mutex
	cached []Entry
	bodies map[string]string
}

// NewSkillProvider returns a Provider backed by system.
func NewSkillProvider(system *skill.System) *SkillProvider {
	return &SkillProvider{system: system}
}

// Entries lists the agent's injectable skills, one line each.
func (p *SkillProvider) Entries(ctx context.Context) ([]Entry, error) {
	if err := p.refresh(ctx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Entry, len(p.cached))
	copy(out, p.cached)
	return out, nil
}

// Detail returns a skill's full body.
func (p *SkillProvider) Detail(ctx context.Context, name string) (string, error) {
	if err := p.refresh(ctx); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	body, ok := p.bodies[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownCapability, name)
	}
	return body, nil
}

// refresh reloads the skill set. skill.System.Load already walks the roots on
// every call, so this keeps only the derived catalog rather than re-deriving
// entries per inference round -- and it means a skill added between calls is
// always picked up on the next one, never masked by a stale cache.
func (p *SkillProvider) refresh(ctx context.Context) error {
	if p.system == nil {
		return fmt.Errorf("skill provider: system is nil")
	}
	skills, err := p.system.Load(ctx)
	if err != nil {
		return fmt.Errorf("load skills: %w", err)
	}
	entries := make([]Entry, 0, len(skills))
	bodies := make(map[string]string, len(skills))
	for _, s := range skills {
		if !skill.IsInjectable(s) {
			continue
		}
		summary := summarize(s.Content)
		if summary == "" {
			return fmt.Errorf("skill %q at %q declares no summary: a catalog line cannot be derived from an empty body", s.ID, s.Path)
		}
		entries = append(entries, Entry{
			Name:    s.ID,
			Group:   "skills",
			Summary: summary,
			Kind:    KindSkill,
		})
		bodies[s.ID] = s.Content
	}
	if len(entries) > MaxSkillsPerAgent {
		return fmt.Errorf("agent exposes %d skills, limit %d: trim the skills directory or split it across agents", len(entries), MaxSkillsPerAgent)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cached = entries
	p.bodies = bodies
	return nil
}
