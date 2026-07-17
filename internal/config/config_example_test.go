package config

import "testing"

func TestFullExampleConfigLoads(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), Options{Path: "../../configs/agent.full.example.json"})
	if err != nil {
		t.Fatalf("Load(full example config) error = %v, want nil", err)
	}
	if cfg.Maas.DefaultProfile != "fast" {
		t.Fatalf("Load(full example config).Maas.DefaultProfile = %q, want fast", cfg.Maas.DefaultProfile)
	}
	if cfg.Maas.Profiles["review"].BaseURL == "" {
		t.Fatalf("Load(full example config).Maas.Profiles[review].BaseURL = %q, want non-empty", cfg.Maas.Profiles["review"].BaseURL)
	}
	if cfg.Storage.Driver != "sqlite" {
		t.Fatalf("Load(full example config).Storage.Driver = %q, want sqlite", cfg.Storage.Driver)
	}
	if !cfg.TUI.ShowPrompt || !cfg.TUI.ShowThinking {
		t.Fatalf("Load(full example config).TUI = %#v, want prompt and thinking visible", cfg.TUI)
	}
	if cfg.Runtime.MaxToolRounds != 4 {
		t.Fatalf("Load(full example config).Runtime.MaxToolRounds = %d, want 4", cfg.Runtime.MaxToolRounds)
	}
	if cfg.Skills.InstallRoot == "" {
		t.Fatalf("Load(full example config).Skills.InstallRoot = %q, want non-empty", cfg.Skills.InstallRoot)
	}
	if cfg.Tasks.IndexPath != "tasks.md" || cfg.Tasks.Root != "tasks" {
		t.Fatalf("Load(full example config).Tasks = %#v, want tasks.md/tasks defaults", cfg.Tasks)
	}
}
