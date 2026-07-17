package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/skill"
)

// Compile-time assertion that the skill system satisfies the Core provider.
var _ cognitive.SkillProvider = (*skill.System)(nil)

func TestSkillsRootAvailable(t *testing.T) {
	t.Parallel()

	if skillsRootAvailable("") {
		t.Error("skillsRootAvailable(\"\") = true, want false")
	}
	if skillsRootAvailable(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Error("skillsRootAvailable(missing) = true, want false")
	}

	dir := t.TempDir()
	if !skillsRootAvailable(dir) {
		t.Errorf("skillsRootAvailable(%q) = false, want true", dir)
	}

	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if skillsRootAvailable(file) {
		t.Errorf("skillsRootAvailable(file %q) = true, want false", file)
	}
}

// TestSkillSystemSelectsInjectableSkill verifies the L1 skill wiring used by the
// serve path: a skill present under the install root is selected for a matching
// task, which is what Core.WithSkills surfaces into the built context.
func TestSkillSystemSelectsInjectableSkill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	root := t.TempDir()
	skillDir := filepath.Join(root, "deployer")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	content := "---\nid: deployer\nname: deployer\nstatus: enabled\nrisk_level: safe\ntags: deploy, release\n---\nStep through the deployment checklist.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	system := skill.NewSystem(skill.Config{
		Roots:   []string{root},
		Scanner: skill.NewSecurityScanner(),
	})
	injections, err := system.SelectForTask(ctx, domain.Task{ID: "task-1", Input: "run the deployer release deploy"}, 3)
	if err != nil {
		t.Fatalf("select for task: %v", err)
	}
	if len(injections) == 0 {
		t.Fatal("SelectForTask for matching input = 0 injections, want the deployer skill")
	}
}
