package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAppliesDefaultsAndJSONFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{
		"maas": {"base_url": "https://maas.example.test", "api_key": "file-key"},
		"storage": {"driver": "sqlite", "path": "data/agent.db"},
		"server": {
			"listen_addr": ":9090",
			"admin_token": "file-token",
			"public_health_enabled": false,
			"request_id_header": "X-Correlation-ID"
		},
		"service": {"background_interval": "250ms"},
		"runtime": {"demo_response": "from config"},
		"skills": {"registry_url": "https://skills.example.test/index.json", "install_root": "data/skills"}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Maas.BaseURL != "https://maas.example.test" {
		t.Fatalf("Load(%q).Maas.BaseURL = %q, want file value", path, cfg.Maas.BaseURL)
	}
	if cfg.Maas.APIKey != "file-key" {
		t.Fatalf("Load(%q).Maas.APIKey = %q, want file value", path, cfg.Maas.APIKey)
	}
	if cfg.Storage.Driver != "sqlite" || cfg.Storage.Path != "data/agent.db" {
		t.Fatalf("Load(%q).Storage = %#v, want sqlite file storage", path, cfg.Storage)
	}
	if cfg.Server.ListenAddr != ":9090" {
		t.Fatalf("Load(%q).Server.ListenAddr = %q, want :9090", path, cfg.Server.ListenAddr)
	}
	if cfg.Server.AdminToken != "file-token" {
		t.Fatalf("Load(%q).Server.AdminToken = %q, want file-token", path, cfg.Server.AdminToken)
	}
	if cfg.Server.PublicHealthEnabled {
		t.Fatalf("Load(%q).Server.PublicHealthEnabled = %t, want false", path, cfg.Server.PublicHealthEnabled)
	}
	if cfg.Server.RequestIDHeader != "X-Correlation-ID" {
		t.Fatalf("Load(%q).Server.RequestIDHeader = %q, want X-Correlation-ID", path, cfg.Server.RequestIDHeader)
	}
	if cfg.Skills.RegistryURL != "https://skills.example.test/index.json" {
		t.Fatalf("Load(%q).Skills.RegistryURL = %q, want skills registry", path, cfg.Skills.RegistryURL)
	}
	if cfg.Skills.InstallRoot != "data/skills" {
		t.Fatalf("Load(%q).Skills.InstallRoot = %q, want data/skills", path, cfg.Skills.InstallRoot)
	}
	if cfg.Service.BackgroundInterval != "250ms" {
		t.Fatalf("Load(%q).Service.BackgroundInterval = %q, want 250ms", path, cfg.Service.BackgroundInterval)
	}
	if cfg.Runtime.DemoResponse != "from config" {
		t.Fatalf("Load(%q).Runtime.DemoResponse = %q, want file value", path, cfg.Runtime.DemoResponse)
	}
	if cfg.Runtime.MaxToolRounds != 4 {
		t.Fatalf("Load(%q).Runtime.MaxToolRounds = %d, want default 4", path, cfg.Runtime.MaxToolRounds)
	}
	if !cfg.TUI.ShowPrompt || !cfg.TUI.ShowThinking {
		t.Fatalf("Load(%q).TUI = %#v, want default prompt/thinking visible", path, cfg.TUI)
	}
}

func TestLoadAppliesEnvironmentOverrides(t *testing.T) {
	t.Setenv("LEGION_AGENT_MAAS_URL", "https://env-maas.example.test")
	t.Setenv("LEGION_AGENT_MAAS_API_KEY", "env-key")
	t.Setenv("LEGION_AGENT_STORAGE_PATH", "env-agent.db")
	t.Setenv("LEGION_AGENT_SERVER_ADDR", ":7070")
	t.Setenv("LEGION_AGENT_ADMIN_TOKEN", "env-token")
	t.Setenv("LEGION_AGENT_PUBLIC_HEALTH", "0")
	t.Setenv("LEGION_AGENT_REQUEST_ID_HEADER", "X-Trace-ID")
	t.Setenv("LEGION_AGENT_SKILL_REGISTRY_URL", "https://env-skills.example.test/index.json")
	t.Setenv("LEGION_AGENT_SKILL_INSTALL_ROOT", "env-skills")
	t.Setenv("LEGION_AGENT_BACKGROUND_INTERVAL", "500ms")
	t.Setenv("LEGION_AGENT_MAX_TOOL_ROUNDS", "6")
	t.Setenv("LEGION_AGENT_TUI_SHOW_PROMPT", "0")
	t.Setenv("LEGION_AGENT_TUI_SHOW_THINKING", "false")

	cfg, err := Load(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Load(defaults with env) error = %v, want nil", err)
	}
	if cfg.Maas.BaseURL != "https://env-maas.example.test" {
		t.Fatalf("Load().Maas.BaseURL = %q, want env override", cfg.Maas.BaseURL)
	}
	if cfg.Maas.APIKey != "env-key" {
		t.Fatalf("Load().Maas.APIKey = %q, want env override", cfg.Maas.APIKey)
	}
	if cfg.Storage.Path != "env-agent.db" {
		t.Fatalf("Load().Storage.Path = %q, want env override", cfg.Storage.Path)
	}
	if cfg.Server.ListenAddr != ":7070" {
		t.Fatalf("Load().Server.ListenAddr = %q, want env override", cfg.Server.ListenAddr)
	}
	if cfg.Server.AdminToken != "env-token" {
		t.Fatalf("Load().Server.AdminToken = %q, want env-token", cfg.Server.AdminToken)
	}
	if cfg.Server.PublicHealthEnabled {
		t.Fatalf("Load().Server.PublicHealthEnabled = %t, want false", cfg.Server.PublicHealthEnabled)
	}
	if cfg.Server.RequestIDHeader != "X-Trace-ID" {
		t.Fatalf("Load().Server.RequestIDHeader = %q, want X-Trace-ID", cfg.Server.RequestIDHeader)
	}
	if cfg.Skills.RegistryURL != "https://env-skills.example.test/index.json" {
		t.Fatalf("Load().Skills.RegistryURL = %q, want env skill registry", cfg.Skills.RegistryURL)
	}
	if cfg.Skills.InstallRoot != "env-skills" {
		t.Fatalf("Load().Skills.InstallRoot = %q, want env-skills", cfg.Skills.InstallRoot)
	}
	if cfg.Service.BackgroundInterval != "500ms" {
		t.Fatalf("Load().Service.BackgroundInterval = %q, want env override", cfg.Service.BackgroundInterval)
	}
	if cfg.Runtime.MaxToolRounds != 6 {
		t.Fatalf("Load().Runtime.MaxToolRounds = %d, want env override 6", cfg.Runtime.MaxToolRounds)
	}
	if cfg.TUI.ShowPrompt {
		t.Fatalf("Load().TUI.ShowPrompt = true, want env override false")
	}
	if cfg.TUI.ShowThinking {
		t.Fatalf("Load().TUI.ShowThinking = true, want env override false")
	}
}

func TestLoadTUIConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"tui": {
			"show_prompt": false,
			"show_thinking": false
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.TUI.ShowPrompt {
		t.Fatalf("Load(%q).TUI.ShowPrompt = true, want false", path)
	}
	if cfg.TUI.ShowThinking {
		t.Fatalf("Load(%q).TUI.ShowThinking = true, want false", path)
	}
}

func TestLoadRuntimeMaxToolRoundsConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"runtime": {
			"demo_response": "from config",
			"max_tool_rounds": 8
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Runtime.MaxToolRounds != 8 {
		t.Fatalf("Load(%q).Runtime.MaxToolRounds = %d, want 8", path, cfg.Runtime.MaxToolRounds)
	}
}

func TestLoadSessionConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"session": {
			"enabled": true,
			"default_recent_turns": 8,
			"max_turn_chars": 3000,
			"restore_latest_on_tui_start": false,
			"cache_enabled": false,
			"cache_max_entries": 16
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if !cfg.Session.Enabled {
		t.Fatalf("Load(%q).Session.Enabled = false, want true", path)
	}
	if cfg.Session.DefaultRecentTurns != 8 {
		t.Fatalf("Load(%q).Session.DefaultRecentTurns = %d, want 8", path, cfg.Session.DefaultRecentTurns)
	}
	if cfg.Session.MaxTurnChars != 3000 {
		t.Fatalf("Load(%q).Session.MaxTurnChars = %d, want 3000", path, cfg.Session.MaxTurnChars)
	}
	if cfg.Session.RestoreLatestOnTUIStart {
		t.Fatalf("Load(%q).Session.RestoreLatestOnTUIStart = true, want false", path)
	}
	if cfg.Session.CacheEnabled {
		t.Fatalf("Load(%q).Session.CacheEnabled = true, want false", path)
	}
	if cfg.Session.CacheMaxEntries != 16 {
		t.Fatalf("Load(%q).Session.CacheMaxEntries = %d, want 16", path, cfg.Session.CacheMaxEntries)
	}
}

func TestDefaultSessionCacheConfig(t *testing.T) {
	t.Parallel()

	cfg, err := Load(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Load(default) error = %v, want nil", err)
	}
	if !cfg.Session.CacheEnabled {
		t.Fatalf("Load(default).Session.CacheEnabled = false, want true")
	}
	if cfg.Session.CacheMaxEntries != 128 {
		t.Fatalf("Load(default).Session.CacheMaxEntries = %d, want 128", cfg.Session.CacheMaxEntries)
	}
}

// TestLoadRuntimeMaxToolRoundsZeroMeansUnlimited pins the contract that an
// explicit max_tool_rounds of 0 (or negative) removes the per-task tool-round
// cap: the model may keep calling tools until it finishes the task. The value is
// normalized to UnlimitedToolRoundsCap, a large runaway hard cap that still stops
// a truly looping model. An ABSENT field keeps the safe default 4 (see
// TestLoadRuntimeMaxToolRoundsAbsentKeepsDefault).
func TestLoadRuntimeMaxToolRoundsZeroMeansUnlimited(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"runtime": {
			"max_tool_rounds": 0
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Runtime.MaxToolRounds != UnlimitedToolRoundsCap {
		t.Fatalf("Load(%q).Runtime.MaxToolRounds = %d, want unlimited cap %d", path, cfg.Runtime.MaxToolRounds, UnlimitedToolRoundsCap)
	}
}

// TestLoadRuntimeMaxToolRoundsAbsentKeepsDefault guards that omitting the field
// entirely still yields the safe default 4 — only an explicit 0 opts into
// unlimited, so existing deployments that never set it are unaffected.
func TestLoadRuntimeMaxToolRoundsAbsentKeepsDefault(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"runtime": {
			"demo_response": "no rounds field here"
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Runtime.MaxToolRounds != 4 {
		t.Fatalf("Load(%q).Runtime.MaxToolRounds = %d, want default 4 when field absent", path, cfg.Runtime.MaxToolRounds)
	}
}

func TestLoadContextFilesAndWorkspaceConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"context_files": {
			"enabled": true,
			"root": ".",
			"agents_path": "AGENTS.md",
			"soul_path": "configs/persona/SOUL.md",
			"tools_path": "configs/persona/TOOLS.md",
			"user_path": "configs/persona/USER.md",
			"memory_path": "configs/persona/MEMORY.md",
			"max_file_chars": 4096
		},
		"workspace": {
			"docs_root": "docs",
			"memory_root": "memory"
		},
		"tasks": {
			"index_path": "tasks.md",
			"root": "tasks",
			"archive_root": "tasks/archive",
			"max_index_lines": 400,
			"max_task_lines": 120,
			"max_message_chars": 280,
			"active_statuses": ["planned", "ready", "in_progress", "blocked", "review"],
			"done_statuses": ["done", "cancelled"]
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if !cfg.ContextFiles.Enabled {
		t.Fatalf("Load(%q).ContextFiles.Enabled = false, want true", path)
	}
	if cfg.ContextFiles.AgentsPath != "AGENTS.md" {
		t.Fatalf("Load(%q).ContextFiles.AgentsPath = %q, want AGENTS.md", path, cfg.ContextFiles.AgentsPath)
	}
	if cfg.ContextFiles.MemoryPath != "configs/persona/MEMORY.md" {
		t.Fatalf("Load(%q).ContextFiles.MemoryPath = %q, want configs/persona/MEMORY.md", path, cfg.ContextFiles.MemoryPath)
	}
	if cfg.ContextFiles.MaxFileChars != 4096 {
		t.Fatalf("Load(%q).ContextFiles.MaxFileChars = %d, want 4096", path, cfg.ContextFiles.MaxFileChars)
	}
	if cfg.Workspace.DocsRoot != "docs" || cfg.Workspace.MemoryRoot != "memory" {
		t.Fatalf("Load(%q).Workspace = %#v, want docs/memory roots", path, cfg.Workspace)
	}
	if cfg.Tasks.IndexPath != "tasks.md" || cfg.Tasks.Root != "tasks" || cfg.Tasks.ArchiveRoot != "tasks/archive" {
		t.Fatalf("Load(%q).Tasks paths = %#v, want tasks.md/tasks/tasks/archive", path, cfg.Tasks)
	}
	if cfg.Tasks.MaxIndexLines != 400 || cfg.Tasks.MaxTaskLines != 120 || cfg.Tasks.MaxMessageChars != 280 {
		t.Fatalf("Load(%q).Tasks limits = %#v, want configured limits", path, cfg.Tasks)
	}
	if len(cfg.Tasks.ActiveStatuses) != 5 || cfg.Tasks.ActiveStatuses[3] != "blocked" {
		t.Fatalf("Load(%q).Tasks.ActiveStatuses = %#v, want configured active statuses", path, cfg.Tasks.ActiveStatuses)
	}
	if len(cfg.Tasks.DoneStatuses) != 2 || cfg.Tasks.DoneStatuses[1] != "cancelled" {
		t.Fatalf("Load(%q).Tasks.DoneStatuses = %#v, want configured done statuses", path, cfg.Tasks.DoneStatuses)
	}
}

func TestLoadTasksEnvOverrides(t *testing.T) {
	t.Setenv("LEGION_AGENT_TASKS_INDEX_PATH", "work/tasks.md")
	t.Setenv("LEGION_AGENT_TASKS_ROOT", "work/tasks")
	t.Setenv("LEGION_AGENT_TASKS_ARCHIVE_ROOT", "work/tasks/archive")
	t.Setenv("LEGION_AGENT_TASKS_MAX_INDEX_LINES", "250")
	t.Setenv("LEGION_AGENT_TASKS_MAX_TASK_LINES", "80")
	t.Setenv("LEGION_AGENT_TASKS_MAX_MESSAGE_CHARS", "180")

	cfg, err := Load(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Load(default with task env) error = %v, want nil", err)
	}
	if cfg.Tasks.IndexPath != "work/tasks.md" {
		t.Fatalf("Load().Tasks.IndexPath = %q, want work/tasks.md", cfg.Tasks.IndexPath)
	}
	if cfg.Tasks.Root != "work/tasks" || cfg.Tasks.ArchiveRoot != "work/tasks/archive" {
		t.Fatalf("Load().Tasks roots = %#v, want env roots", cfg.Tasks)
	}
	if cfg.Tasks.MaxIndexLines != 250 || cfg.Tasks.MaxTaskLines != 80 || cfg.Tasks.MaxMessageChars != 180 {
		t.Fatalf("Load().Tasks limits = %#v, want env limits", cfg.Tasks)
	}
}

func TestLoadMaasProfiles(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{"maas":{"default_profile":"fast","profiles":{"fast":{"base_url":"https://fast.example.test","api_key":"fast-key"},"review":{"base_url":"https://review.example.test","api_key":"review-key"}}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}
	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Maas.DefaultProfile != "fast" {
		t.Fatalf("Load(%q).Maas.DefaultProfile = %q, want fast", path, cfg.Maas.DefaultProfile)
	}
	if cfg.Maas.Profiles["review"].BaseURL != "https://review.example.test" {
		t.Fatalf("Load(%q).Maas.Profiles[review].BaseURL = %q, want review URL", path, cfg.Maas.Profiles["review"].BaseURL)
	}
}

func TestLoadAgentsConfig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{"agents":{"researcher":"agents/researcher.json","writer":"agents/writer.json"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Agents["researcher"] != "agents/researcher.json" {
		t.Fatalf("Load(%q).Agents[researcher] = %q, want agents/researcher.json", path, cfg.Agents["researcher"])
	}
	if cfg.Agents["writer"] != "agents/writer.json" {
		t.Fatalf("Load(%q).Agents[writer] = %q, want agents/writer.json", path, cfg.Agents["writer"])
	}
}

func TestLoadMissingFileReturnsErrConfigNotFound(t *testing.T) {
	t.Parallel()
	_, err := Load(context.Background(), Options{Path: filepath.Join(t.TempDir(), "missing.json")})
	if !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("Load(missing) error = %v, want ErrConfigNotFound", err)
	}
}

func TestLoadMaxConcurrentTasksDefault(t *testing.T) {
	cfg, err := Load(context.Background(), Options{Path: ""})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Runtime.MaxConcurrentTasks != 4 {
		t.Fatalf("default MaxConcurrentTasks = %d, want 4", cfg.Runtime.MaxConcurrentTasks)
	}
}

func TestLoadApprovalTimeoutSecondsDefault(t *testing.T) {
	cfg, err := Load(context.Background(), Options{Path: ""})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Runtime.ApprovalTimeoutSeconds != 300 {
		t.Fatalf("default ApprovalTimeoutSeconds = %d, want 300", cfg.Runtime.ApprovalTimeoutSeconds)
	}
}

// TestNormalizeMaxToolRounds pins the config-layer normalization invariant
// directly, so the "0/negative = unlimited cap" contract is a locked regression
// line rather than only inferred through Load. This function carries a safety
// role — the cap is the only thing stopping a runaway in-flight tool loop, since
// hard-loop detection cannot break one — so changing it must break a test.
func TestNormalizeMaxToolRounds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   int
		want int
	}{
		{"explicit zero maps to the unlimited cap", 0, UnlimitedToolRoundsCap},
		{"negative maps to the unlimited cap", -1, UnlimitedToolRoundsCap},
		{"positive is used as-is", 5, 5},
		{"one is used as-is", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeMaxToolRounds(tc.in); got != tc.want {
				t.Errorf("normalizeMaxToolRounds(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestLoadDisabledToolsUnknownNameFailsLoud validates that an unknown tool name
// in disabled_tools causes Load to return an error with the bad name included.
// This is a fail-loud invariant: typos in the config must be caught eagerly.
func TestLoadDisabledToolsUnknownNameFailsLoud(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"runtime": {
			"disabled_tools": ["writ_file"]
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	_, err := Load(context.Background(), Options{Path: path})
	if err == nil {
		t.Fatal("Load with unknown disabled_tools name should return error, got nil")
	}
	if !strings.Contains(err.Error(), "writ_file") {
		t.Fatalf("Load error %q should mention unknown tool name 'writ_file'", err.Error())
	}
}

// TestLoadDisabledToolsValidNameSucceeds validates that a valid tool name in
// disabled_tools is accepted and the config loads successfully.
func TestLoadDisabledToolsValidNameSucceeds(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"runtime": {
			"disabled_tools": ["write_file"]
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load with valid disabled_tools name should succeed, got error: %v", err)
	}
	if len(cfg.Runtime.DisabledTools) != 1 || cfg.Runtime.DisabledTools[0] != "write_file" {
		t.Fatalf("Load().Runtime.DisabledTools = %v, want [write_file]", cfg.Runtime.DisabledTools)
	}
}
