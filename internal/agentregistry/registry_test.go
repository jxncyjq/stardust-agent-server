package agentregistry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stardust/legion-agent/internal/config"
)

func TestLoadLoadsAgentConfigsRelativeToConfigDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")
	if err := os.Mkdir(agentsDir, 0o700); err != nil {
		t.Fatalf("Mkdir(%q) error = %v, want nil", agentsDir, err)
	}
	researcherPath := filepath.Join(agentsDir, "researcher.json")
	researcherBody := `{
		"id": "researcher",
		"role": "research",
		"maas_profile": "deep",
		"context_files": {
			"soul_path": "configs/persona/RESEARCHER.md"
		},
		"workspace": {
			"memory_root": "memory/researcher"
		},
		"skills": {
			"install_root": "skills/researcher"
		}
	}`
	if err := os.WriteFile(researcherPath, []byte(researcherBody), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", researcherPath, err)
	}
	writerPath := filepath.Join(agentsDir, "writer.json")
	writerBody := `{
		"id": "writer",
		"role": "writing",
		"maas_profile": "fast",
		"context_files": {
			"soul_path": "configs/persona/WRITER.md"
		},
		"workspace": {
			"memory_root": "memory/writer"
		},
		"skills": {
			"install_root": "skills/writer"
		}
	}`
	if err := os.WriteFile(writerPath, []byte(writerBody), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", writerPath, err)
	}

	rootCfg := config.Config{
		Agents: map[string]string{
			"researcher": "agents/researcher.json",
			"writer":     "agents/writer.json",
		},
	}

	registry, err := Load(context.Background(), rootCfg, dir)
	if err != nil {
		t.Fatalf("Load(ctx, rootCfg, %q) error = %v, want nil", dir, err)
	}
	names := registry.Names()
	if len(names) != 2 || names[0] != "researcher" || names[1] != "writer" {
		t.Fatalf("Load(ctx, rootCfg, %q).Names() = %#v, want sorted researcher/writer", dir, names)
	}
	agent, ok := registry.Get("researcher")
	if !ok {
		t.Fatalf("Load(ctx, rootCfg, %q).Get(researcher) ok = false, want true", dir)
	}
	if agent.ID != "researcher" || agent.Role != "research" || agent.MaasProfile != "deep" {
		t.Fatalf("Load(ctx, rootCfg, %q).Get(researcher) = %#v, want researcher config", dir, agent)
	}
	if agent.ContextFiles.SoulPath != "configs/persona/RESEARCHER.md" {
		t.Fatalf("Load(ctx, rootCfg, %q).Get(researcher).ContextFiles.SoulPath = %q, want configs/persona/RESEARCHER.md", dir, agent.ContextFiles.SoulPath)
	}
	if agent.Workspace.MemoryRoot != "memory/researcher" {
		t.Fatalf("Load(ctx, rootCfg, %q).Get(researcher).Workspace.MemoryRoot = %q, want memory/researcher", dir, agent.Workspace.MemoryRoot)
	}
	if agent.Skills.InstallRoot != "skills/researcher" {
		t.Fatalf("Load(ctx, rootCfg, %q).Get(researcher).Skills.InstallRoot = %q, want skills/researcher", dir, agent.Skills.InstallRoot)
	}
}

func TestLoadMissingAgentConfigReturnsErrAgentConfigNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rootCfg := config.Config{Agents: map[string]string{"researcher": "agents/missing.json"}}

	_, err := Load(context.Background(), rootCfg, dir)
	if !errors.Is(err, ErrAgentConfigNotFound) {
		t.Fatalf("Load(ctx, rootCfg, %q) error = %v, want ErrAgentConfigNotFound", dir, err)
	}
}

func TestRegistryNewCopiesInputsAndNamesReturnsSortedCopy(t *testing.T) {
	t.Parallel()
	agents := map[string]AgentConfig{
		"writer":     {ID: "writer", Skills: config.SkillsConfig{InstallRoot: "skills/writer"}},
		"researcher": {ID: "researcher", ContextFiles: config.ContextFilesConfig{SoulPath: "AGENTS.md"}},
	}

	registry := New(agents)
	agents["writer"] = AgentConfig{ID: "mutated"}
	names := registry.Names()
	names[0] = "mutated"

	gotNames := registry.Names()
	if len(gotNames) != 2 || gotNames[0] != "researcher" || gotNames[1] != "writer" {
		t.Fatalf("New(agents).Names() = %#v, want sorted copy", gotNames)
	}
	agent, ok := registry.Get("writer")
	if !ok {
		t.Fatalf("New(agents).Get(writer) ok = false, want true")
	}
	if agent.ID != "writer" {
		t.Fatalf("New(agents).Get(writer).ID = %q, want writer", agent.ID)
	}
}

func TestAgentConfigCarriesDisabledTools(t *testing.T) {
	raw := []byte(`{"id":"a1","role":"researcher","disabled_tools":["write_file"]}`)
	var cfg AgentConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal error = %v, want nil", err)
	}
	if len(cfg.DisabledTools) != 1 || cfg.DisabledTools[0] != "write_file" {
		t.Fatalf("DisabledTools = %#v, want [write_file]", cfg.DisabledTools)
	}
}

func TestAgentConfigOmitsDisabledToolsWhenAbsent(t *testing.T) {
	var cfg AgentConfig
	if err := json.Unmarshal([]byte(`{"id":"a1","role":"r"}`), &cfg); err != nil {
		t.Fatalf("Unmarshal error = %v, want nil", err)
	}
	if cfg.DisabledTools != nil {
		t.Fatalf("DisabledTools = %#v, want nil when absent", cfg.DisabledTools)
	}
}
