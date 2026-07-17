package skill

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestSystemLoadsLocalSkills(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeSkill(t, root, "go-testing", `---
id: go-testing
name: Go Testing
version: 1.0.0
source: workspace
risk_level: safe
status: active
tags: go,test
---
# Go Testing

Use this when writing Go tests.
`)

	system := NewSystem(Config{Roots: []string{root}})
	skills, err := system.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if len(skills) != 1 {
		t.Fatalf("Load() len = %d, want 1", len(skills))
	}
	if skills[0].ID != "go-testing" {
		t.Fatalf("Load()[0].ID = %q, want %q", skills[0].ID, "go-testing")
	}
	if skills[0].Path == "" {
		t.Fatalf("Load()[0].Path = empty, want skill file path")
	}
}

func TestSystemDeduplicatesAndSortsSkills(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeSkill(t, root, "zeta", skillDoc("zeta", "Zeta", "1.0.0", "safe", "active", "zeta"))
	writeSkill(t, root, "alpha", skillDoc("alpha", "Alpha", "1.0.0", "safe", "active", "alpha"))
	writeSkill(t, root, "alpha-copy", skillDoc("alpha", "Alpha Duplicate", "1.0.0", "safe", "active", "alpha"))

	system := NewSystem(Config{Roots: []string{root}})
	skills, err := system.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if len(skills) != 2 {
		t.Fatalf("Load() len = %d, want 2", len(skills))
	}
	if skills[0].ID != "alpha" || skills[1].ID != "zeta" {
		t.Fatalf("Load() order = [%s %s], want [alpha zeta]", skills[0].ID, skills[1].ID)
	}
}

func TestSystemSelectsAtMostThreeRelevantInjectableSkills(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeSkill(t, root, "go-testing", skillDoc("go-testing", "Go Testing", "1.0.0", "safe", "active", "go,test"))
	writeSkill(t, root, "go-style", skillDoc("go-style", "Go Style", "1.0.0", "safe", "active", "go,style"))
	writeSkill(t, root, "go-errors", skillDoc("go-errors", "Go Errors", "1.0.0", "safe", "active", "go,error"))
	writeSkill(t, root, "go-security", skillDoc("go-security", "Go Security", "1.0.0", "safe", "active", "go,security"))
	writeSkill(t, root, "risky-go", skillDoc("risky-go", "Risky Go", "1.0.0", "critical", "active", "go"))
	writeSkill(t, root, "frozen-go", skillDoc("frozen-go", "Frozen Go", "1.0.0", "safe", "frozen", "go"))

	system := NewSystem(Config{Roots: []string{root}})
	injections, err := system.SelectForTask(ctx, domain.Task{
		ID:    "task-1",
		Input: "Write Go tests and improve error handling.",
	}, 3)
	if err != nil {
		t.Fatalf("SelectForTask() error = %v, want nil", err)
	}
	if len(injections) != 3 {
		t.Fatalf("SelectForTask() len = %d, want 3", len(injections))
	}
	for rank, injection := range injections {
		if injection.Rank != rank+1 {
			t.Errorf("SelectForTask()[%d].Rank = %d, want %d", rank, injection.Rank, rank+1)
		}
		if injection.Skill.RiskLevel == RiskCritical {
			t.Errorf("SelectForTask() included critical skill %q", injection.Skill.ID)
		}
		if injection.Skill.Status == StatusFrozen {
			t.Errorf("SelectForTask() included frozen skill %q", injection.Skill.ID)
		}
	}
}

func TestSystemSelectForTaskTouchesUsageOfSelectedSkills(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeSkill(t, root, "go-testing", skillDoc("go-testing", "Go Testing", "1.0.0", "safe", "active", "go,test"))
	writeSkill(t, root, "python-web", skillDoc("python-web", "Python Web", "1.0.0", "safe", "active", "python,web"))

	usage := NewUsageStore()
	stamp := time.Unix(1_700_000, 0)
	system := NewSystem(Config{Roots: []string{root}}).WithUsage(usage, func() time.Time { return stamp })

	injections, err := system.SelectForTask(ctx, domain.Task{ID: "t1", Input: "write go tests"}, 3)
	if err != nil {
		t.Fatalf("SelectForTask() error = %v, want nil", err)
	}
	if len(injections) != 1 || injections[0].Skill.ID != "go-testing" {
		t.Fatalf("SelectForTask() = %v, want only go-testing", injections)
	}
	// The selected skill is recorded as active at the injected timestamp.
	record, ok := usage.Get("go-testing")
	if !ok || record.UseCount != 1 || !record.LastActivityAt.Equal(stamp) {
		t.Fatalf("usage(go-testing) = %+v (ok=%v), want use once at %v", record, ok, stamp)
	}
	// The unselected skill is left untracked.
	if _, ok := usage.Get("python-web"); ok {
		t.Fatalf("usage(python-web) tracked, want untouched")
	}
}

func TestSystemReturnsCopyOfLoadedSkills(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeSkill(t, root, "go-testing", skillDoc("go-testing", "Go Testing", "1.0.0", "safe", "active", "go,test"))

	system := NewSystem(Config{Roots: []string{root}})
	skills, err := system.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	skills[0].Name = "changed"

	reloaded, err := system.Load(ctx)
	if err != nil {
		t.Fatalf("Load() second error = %v, want nil", err)
	}
	if reloaded[0].Name != "Go Testing" {
		t.Fatalf("Load() after caller mutation name = %q, want %q", reloaded[0].Name, "Go Testing")
	}
}

func writeSkill(t *testing.T, root string, dir string, content string) {
	t.Helper()
	path := filepath.Join(root, dir)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v, want nil", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(SKILL.md) error = %v, want nil", err)
	}
}

func skillDoc(id string, name string, version string, risk string, status string, tags string) string {
	return `---
id: ` + id + `
name: ` + name + `
version: ` + version + `
source: workspace
risk_level: ` + risk + `
status: ` + status + `
tags: ` + tags + `
---
# ` + name + `

This skill helps with ` + tags + `.
`
}
