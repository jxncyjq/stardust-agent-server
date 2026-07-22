package cognitive

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/memory"
	"github.com/stardust/legion-agent/internal/skill"
)

func TestCoreBuildContextIncludesAgentTaskAndTools(t *testing.T) {
	t.Parallel()

	core := NewCore(NoopCompressor{})
	built, err := core.BuildContext(context.Background(), Request{
		Agent: domain.Agent{
			ID:   "agent-1",
			Role: "developer",
		},
		Task: domain.Task{
			ID:    "task-1",
			Input: "ship scheduler",
		},
		Tools: []string{"read_file", "write_file"},
	})
	if err != nil {
		t.Fatalf("BuildContext() error = %v, want nil", err)
	}
	for _, want := range []string{"agent-1", "developer", "task-1", "ship scheduler", "read_file", "write_file"} {
		if !strings.Contains(built.Prompt, want) {
			t.Errorf("BuildContext() prompt missing %q:\n%s", want, built.Prompt)
		}
	}
}

func TestThresholdCompressorMarksLargeContext(t *testing.T) {
	t.Parallel()

	compressor := NewThresholdCompressor(10)
	result, err := compressor.Compress(context.Background(), strings.Repeat("x", 12))
	if err != nil {
		t.Fatalf("Compress() error = %v, want nil", err)
	}
	if !result.Compressed {
		t.Errorf("Compress() compressed = false, want true")
	}
	if result.Text == "" {
		t.Errorf("Compress() text = empty, want non-empty summary")
	}
}

func TestCoreBuildContextIncludesMemoryProviderBlocks(t *testing.T) {
	t.Parallel()

	memory := fakeMemoryProvider{
		systemBlock: "Memory:\n- prefer deterministic tests",
		prefetched:  []domain.MemoryEntry{{Content: "scheduler learned"}},
	}
	core := NewCore(NoopCompressor{}).WithMemory(memory)
	built, err := core.BuildContext(context.Background(), Request{
		Agent: domain.Agent{ID: "agent-1"},
		Task:  domain.Task{ID: "task-1", Input: "scheduler"},
	})
	if err != nil {
		t.Fatalf("BuildContext() error = %v, want nil", err)
	}
	for _, want := range []string{"prefer deterministic tests", "scheduler learned"} {
		if !strings.Contains(built.Prompt, want) {
			t.Errorf("BuildContext() prompt missing %q:\n%s", want, built.Prompt)
		}
	}
}

func TestCoreBuildContextIncludesContextFilesBlock(t *testing.T) {
	t.Parallel()

	core := NewCore(NoopCompressor{}).WithContextFiles("Agent identity:\nLegion Soul\nProject instructions:\nUse go test")
	built, err := core.BuildContext(context.Background(), Request{
		Agent: domain.Agent{ID: "agent-1", Role: "developer"},
		Task:  domain.Task{ID: "task-1", Input: "ship context files"},
	})
	if err != nil {
		t.Fatalf("BuildContext() error = %v, want nil", err)
	}
	for _, want := range []string{"Legion Soul", "Use go test", "ship context files"} {
		if !strings.Contains(built.Prompt, want) {
			t.Errorf("BuildContext() prompt missing %q:\n%s", want, built.Prompt)
		}
	}
}

func TestCoreBuildContextIncludesRecentConversationTurns(t *testing.T) {
	t.Parallel()

	core := NewCore(NoopCompressor{})
	built, err := core.BuildContext(context.Background(), Request{
		Agent: domain.Agent{ID: "agent-1", Role: "developer"},
		Task:  domain.Task{ID: "task-2", Input: "展开刚才的第三点"},
		ConversationTurns: []domain.ConversationTurn{
			{Role: domain.ConversationRoleUser, Content: "你是什么模型"},
			{Role: domain.ConversationRoleAssistant, AgentID: "agent-1", ModelProfile: "dev", Content: "我是 DeepSeek V4"},
		},
	})
	if err != nil {
		t.Fatalf("BuildContext() error = %v, want nil", err)
	}
	for _, want := range []string{"Recent conversation:", "user: 你是什么模型", "assistant(agent-1/dev): 我是 DeepSeek V4", "展开刚才的第三点"} {
		if !strings.Contains(built.Prompt, want) {
			t.Fatalf("BuildContext() prompt missing %q:\n%s", want, built.Prompt)
		}
	}
}

// TestCoreBuildContextDoesNotInjectSelectedSkills pins the behaviour change:
// WithSkills is retained for the /skills query paths, but SelectForTask no
// longer injects a keyword-matched top-N into the prompt. Skills reach the
// model only through the capability catalog (WithCatalog) now.
func TestCoreBuildContextDoesNotInjectSelectedSkills(t *testing.T) {
	t.Parallel()

	core := NewCore(NoopCompressor{}).WithSkills(fakeSkillProvider{
		injections: []skill.Injection{
			{
				TaskID: "task-1",
				Rank:   1,
				Reason: "matched task input",
				Skill: skill.Skill{
					ID:      "go-testing",
					Name:    "Go Testing",
					Version: "1.0.0",
					Status:  skill.StatusEnabled,
					Summary: "Use table-driven tests with useful failure messages.",
				},
			},
		},
	})
	built, err := core.BuildContext(context.Background(), Request{
		Agent: domain.Agent{ID: "agent-1"},
		Task:  domain.Task{ID: "task-1", Input: "write go tests"},
	})
	if err != nil {
		t.Fatalf("BuildContext() error = %v, want nil", err)
	}
	if strings.Contains(built.Prompt, "Mounted skills:") {
		t.Errorf("BuildContext() still injects the removed skill block:\n%s", built.Prompt)
	}
	if strings.Contains(built.Prompt, "go-testing") {
		t.Errorf("BuildContext() injected a selected skill; skills must reach the model via the catalog now:\n%s", built.Prompt)
	}
}

func TestCoreBuildContextIncludesCapabilityMemory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := memory.NewCapabilityMemoryStore()
	for _, gene := range []memory.Gene{
		{
			ID:          "gene-go-tests",
			Version:     "1.0.0",
			Status:      memory.GeneStatusActive,
			Tags:        []string{"go", "test"},
			Match:       "go test task",
			UseWhen:     "when Go tests fail",
			Plan:        "run focused tests before broad changes",
			Avoid:       "avoid changing unrelated files",
			Validation:  "go test ./...",
			SuccessRate: 0.9,
		},
		{
			ID:          "gene-draft",
			Version:     "1.0.0",
			Status:      memory.GeneStatusDraft,
			Tags:        []string{"go", "test"},
			Match:       "draft task",
			UseWhen:     "never",
			Plan:        "draft only",
			Avoid:       "draft",
			Validation:  "draft",
			SuccessRate: 0.99,
		},
	} {
		if err := store.PutGene(ctx, gene); err != nil {
			t.Fatalf("PutGene(%q) error = %v, want nil", gene.ID, err)
		}
	}
	if err := store.PromoteCapsule(ctx, memory.Capsule{
		ID:           "capsule-go-tests",
		GeneIDs:      []string{"gene-go-tests"},
		Query:        "go test task",
		Tags:         []string{"go", "test"},
		Outcome:      "success",
		SuccessCount: 3,
		Confidence:   0.8,
	}); err != nil {
		t.Fatalf("PromoteCapsule(capsule-go-tests) error = %v, want nil", err)
	}
	core := NewCore(NoopCompressor{}).WithCapabilityMemory(store)

	built, err := core.BuildContext(ctx, Request{
		Agent: domain.Agent{ID: "agent-1"},
		Task:  domain.Task{ID: "task-1", Input: "fix go test failure"},
	})
	if err != nil {
		t.Fatalf("BuildContext() error = %v, want nil", err)
	}
	for _, want := range []string{"Capability memory:", "gene-go-tests", "run focused tests", "avoid changing unrelated files", "capsule-go-tests"} {
		if !strings.Contains(built.Prompt, want) {
			t.Errorf("BuildContext() prompt missing %q:\n%s", want, built.Prompt)
		}
	}
	if strings.Contains(built.Prompt, "gene-draft") {
		t.Errorf("BuildContext() prompt includes draft gene:\n%s", built.Prompt)
	}
}

type fakeMemoryProvider struct {
	systemBlock string
	prefetched  []domain.MemoryEntry
}

func (p fakeMemoryProvider) SystemPromptBlock(context.Context, domain.Agent) (string, error) {
	return p.systemBlock, nil
}

func (p fakeMemoryProvider) Prefetch(context.Context, domain.Task) ([]domain.MemoryEntry, error) {
	return p.prefetched, nil
}

type fakeSkillProvider struct {
	injections []skill.Injection
}

func (p fakeSkillProvider) SelectForTask(context.Context, domain.Task, int) ([]skill.Injection, error) {
	return p.injections, nil
}
