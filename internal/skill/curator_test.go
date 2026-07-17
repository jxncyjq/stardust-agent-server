package skill

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeConsolidator records invocations and can be made to fail, to exercise the
// optional consolidation pass wiring.
type fakeConsolidator struct {
	calls int
	err   error
}

func (c *fakeConsolidator) Consolidate(_ context.Context, _ []Skill) error {
	c.calls++
	return c.err
}

func TestNewCuratorRejectsConsolidateWithoutConsolidator(t *testing.T) {
	repo := newFakeCuratorRepo()
	if _, err := NewCurator(CuratorConfig{Repository: repo, Usage: NewUsageStore(), Consolidate: true}); err == nil {
		t.Fatalf("NewCurator(consolidate, no consolidator) error = nil, want fail-loud")
	}
}

func TestCuratorSweepInvokesConsolidatorWhenEnabled(t *testing.T) {
	ctx := context.Background()
	repo := newFakeCuratorRepo(Skill{ID: "s1", Source: SourceWorkspace, Status: StatusEnabled})
	merger := &fakeConsolidator{}
	curator, err := NewCurator(CuratorConfig{Repository: repo, Usage: NewUsageStore(), Consolidate: true, Consolidator: merger})
	if err != nil {
		t.Fatalf("NewCurator() error = %v, want nil", err)
	}
	report, err := curator.Sweep(ctx, time.Unix(1_000_000, 0))
	if err != nil {
		t.Fatalf("Sweep() error = %v, want nil", err)
	}
	if merger.calls != 1 || !report.Consolidated {
		t.Fatalf("consolidator calls = %d, report.Consolidated = %v, want 1/true", merger.calls, report.Consolidated)
	}
}

func TestCuratorSweepSkipsConsolidatorWhenDisabled(t *testing.T) {
	ctx := context.Background()
	repo := newFakeCuratorRepo(Skill{ID: "s1", Source: SourceWorkspace, Status: StatusEnabled})
	merger := &fakeConsolidator{}
	// Consolidator present but Consolidate false → never invoked.
	curator, err := NewCurator(CuratorConfig{Repository: repo, Usage: NewUsageStore(), Consolidator: merger})
	if err != nil {
		t.Fatalf("NewCurator() error = %v, want nil", err)
	}
	if _, err := curator.Sweep(ctx, time.Unix(1_000_000, 0)); err != nil {
		t.Fatalf("Sweep() error = %v, want nil", err)
	}
	if merger.calls != 0 {
		t.Fatalf("consolidator calls = %d, want 0 when disabled", merger.calls)
	}
}

func TestCuratorSweepFailsLoudOnConsolidatorError(t *testing.T) {
	ctx := context.Background()
	repo := newFakeCuratorRepo(Skill{ID: "s1", Source: SourceWorkspace, Status: StatusEnabled})
	merger := &fakeConsolidator{err: context.DeadlineExceeded}
	curator, err := NewCurator(CuratorConfig{Repository: repo, Usage: NewUsageStore(), Consolidate: true, Consolidator: merger})
	if err != nil {
		t.Fatalf("NewCurator() error = %v, want nil", err)
	}
	if _, err := curator.Sweep(ctx, time.Unix(1_000_000, 0)); err == nil {
		t.Fatalf("Sweep() error = nil, want propagated consolidator error")
	}
}

// fakeCuratorRepo is an in-memory CuratorRepository that records how many times
// SaveSkill was called, so tests can assert idempotency and no-deletion.
type fakeCuratorRepo struct {
	mu     sync.Mutex
	skills map[string]Skill
	saves  int
}

func newFakeCuratorRepo(skills ...Skill) *fakeCuratorRepo {
	repo := &fakeCuratorRepo{skills: make(map[string]Skill)}
	for _, s := range skills {
		repo.skills[s.ID] = s
	}
	return repo
}

func (r *fakeCuratorRepo) ListSkills(_ context.Context) ([]Skill, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Skill, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, s)
	}
	return out, nil
}

func (r *fakeCuratorRepo) SaveSkill(_ context.Context, s Skill) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skills[s.ID] = s
	r.saves++
	return nil
}

func (r *fakeCuratorRepo) status(id string) Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.skills[id].Status
}

func (r *fakeCuratorRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.skills)
}

func TestCuratorSweepAgesStaleThenArchived(t *testing.T) {
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	repo := newFakeCuratorRepo(
		Skill{ID: "idle-stale", Source: SourceWorkspace, Status: StatusEnabled},
		Skill{ID: "idle-archive", Source: SourceWorkspace, Status: StatusEnabled},
		Skill{ID: "fresh", Source: SourceWorkspace, Status: StatusEnabled},
		Skill{ID: "pinned", Source: SourceWorkspace, Status: StatusEnabled},
		Skill{ID: "bundled", Source: SourceRegistry, Status: StatusEnabled},
	)
	usage := NewUsageStore()
	usage.Touch("idle-stale", now.Add(-40*24*time.Hour))
	usage.Touch("idle-archive", now.Add(-100*24*time.Hour))
	usage.Touch("fresh", now.Add(-1*24*time.Hour))
	usage.Touch("pinned", now.Add(-100*24*time.Hour))
	usage.Pin("pinned", true)
	usage.Touch("bundled", now.Add(-100*24*time.Hour))

	curator, err := NewCurator(CuratorConfig{Repository: repo, Usage: usage})
	if err != nil {
		t.Fatalf("NewCurator() error = %v, want nil", err)
	}

	report, err := curator.Sweep(context.Background(), now)
	if err != nil {
		t.Fatalf("Sweep() error = %v, want nil", err)
	}
	if report.MarkedStale != 1 || report.Archived != 1 {
		t.Fatalf("Sweep() report = %+v, want 1 stale + 1 archived", report)
	}
	if got := repo.status("idle-stale"); got != StatusStale {
		t.Fatalf("idle-stale status = %s, want stale", got)
	}
	if got := repo.status("idle-archive"); got != StatusArchived {
		t.Fatalf("idle-archive status = %s, want archived", got)
	}
	if got := repo.status("fresh"); got != StatusEnabled {
		t.Fatalf("fresh status = %s, want unchanged enabled", got)
	}
	if got := repo.status("pinned"); got != StatusEnabled {
		t.Fatalf("pinned status = %s, want unchanged (pinned exempt)", got)
	}
	if got := repo.status("bundled"); got != StatusEnabled {
		t.Fatalf("bundled status = %s, want unchanged (registry exempt)", got)
	}
	// Never deletes.
	if repo.count() != 5 {
		t.Fatalf("skill count = %d, want 5 (curator never deletes)", repo.count())
	}
}

func TestCuratorSweepIsIdempotent(t *testing.T) {
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	repo := newFakeCuratorRepo(Skill{ID: "s", Source: SourceWorkspace, Status: StatusEnabled})
	usage := NewUsageStore()
	usage.Touch("s", now.Add(-100*24*time.Hour))
	curator, err := NewCurator(CuratorConfig{Repository: repo, Usage: usage})
	if err != nil {
		t.Fatalf("NewCurator() error = %v, want nil", err)
	}

	if _, err := curator.Sweep(context.Background(), now); err != nil {
		t.Fatalf("first Sweep() error = %v, want nil", err)
	}
	savesAfterFirst := repo.saves
	report, err := curator.Sweep(context.Background(), now)
	if err != nil {
		t.Fatalf("second Sweep() error = %v, want nil", err)
	}
	if report.Archived != 0 || report.MarkedStale != 0 {
		t.Fatalf("second Sweep() report = %+v, want no further changes", report)
	}
	if repo.saves != savesAfterFirst {
		t.Fatalf("second Sweep() saved again (%d -> %d), want idempotent", savesAfterFirst, repo.saves)
	}
}

func TestNewCuratorValidates(t *testing.T) {
	if _, err := NewCurator(CuratorConfig{Usage: NewUsageStore()}); err == nil {
		t.Fatalf("NewCurator(no repo) error = nil, want non-nil")
	}
	if _, err := NewCurator(CuratorConfig{Repository: newFakeCuratorRepo()}); err == nil {
		t.Fatalf("NewCurator(no usage) error = nil, want non-nil")
	}
	if _, err := NewCurator(CuratorConfig{
		Repository: newFakeCuratorRepo(), Usage: NewUsageStore(),
		StaleAfter: 90 * 24 * time.Hour, ArchiveAfter: 30 * 24 * time.Hour,
	}); err == nil {
		t.Fatalf("NewCurator(archive < stale) error = nil, want non-nil")
	}
}
